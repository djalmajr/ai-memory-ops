#!/usr/bin/env python3
"""git-mirror-backfill — seed the backup repo with every existing page.

The git-mirror webhook is forward-only: it mirrors writes that happen
AFTER it's wired into the admission chain. Anything older sits in the
engine's PVC but never reaches the backup. This script walks the live
data dir, decodes the workspace+project UUIDs into human names, and
POSTs each page directly to the webhook's `/sync` endpoint — bypassing
the engine's admission chain so we don't re-trigger contributors /
other hooks or churn frontmatter.

Workflow:

  1. `ai-memory backup` inside the main pod → tarball with wiki/ + db/
  2. `kubectl cp` the tarball out
  3. Read sqlite to translate ws_id/proj_id → (workspace_name, project_name)
  4. `kubectl port-forward` the git-mirror Service to localhost
  5. For each .md, split frontmatter/body and POST to /sync

Idempotent: re-runs produce empty commits (`--allow-empty` in the
webhook) but otherwise don't disrupt the repo state.

Requirements (local): python3, kubectl, sqlite3 (mac ships it), tar.

Usage:

    ./git-mirror-backfill.py                    # defaults: ns=memory, release=memory-v2
    NS=memory RELEASE=memory-v2 ./git-mirror-backfill.py
    ./git-mirror-backfill.py --dry-run          # list pages without POSTing
    ./git-mirror-backfill.py --workspace default --project ai-memory-ops
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import signal
import sqlite3
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path
from typing import Iterator

NS = os.environ.get("NS", "memory")
RELEASE = os.environ.get("RELEASE", "memory-v2")
GITMIRROR_SVC = f"{RELEASE}-wiki-service-git-mirror"
GITMIRROR_PORT = 8080
LOCAL_PORT = int(os.environ.get("LOCAL_PORT", "18080"))
MAIN_LABEL = "app.kubernetes.io/component=main"


def sh(*args: str, check: bool = True, capture: bool = True) -> str:
    """Run a subprocess, return stdout. Raises on non-zero by default."""
    res = subprocess.run(
        args,
        check=check,
        capture_output=capture,
        text=True,
    )
    return (res.stdout or "").strip()


def kubectl(*args: str, **kw) -> str:
    return sh("kubectl", "-n", NS, *args, **kw)


def fetch_main_pod() -> str:
    pod = kubectl(
        "get", "pod", "-l", MAIN_LABEL, "-o", "jsonpath={.items[0].metadata.name}"
    )
    if not pod:
        sys.exit(f"no pod with label {MAIN_LABEL} in ns {NS}")
    return pod


def snapshot_pvc(pod: str, dest: Path) -> None:
    """Take an `ai-memory backup` tarball from the pod and copy it out."""
    remote = "/tmp/git-mirror-backfill.tar.gz"
    print(f">> snapshotting pod={pod} (this takes a few seconds for large wikis)")
    kubectl(
        "exec", "-c", "ai-memory", pod, "--",
        "/usr/local/bin/ai-memory", "backup", "--to", remote,
    )
    kubectl("cp", "-c", "ai-memory", f"{pod}:{remote}", str(dest))
    # Clean up the in-pod tarball
    kubectl("exec", "-c", "ai-memory", pod, "--", "rm", "-f", remote)


def _uuid_str(raw: bytes | str) -> str:
    """Engine stores workspace/project ids as 16-byte BLOB but uses the
    hex-with-dashes form for on-disk paths. Coerce both to the path form."""
    if isinstance(raw, bytes):
        return str(uuid.UUID(bytes=raw))
    return raw


def load_name_lookup(db_path: Path) -> tuple[dict[str, str], dict[str, tuple[str, str]]]:
    """Return (workspace_id -> workspace_name, project_id -> (workspace_name, project_name))."""
    conn = sqlite3.connect(db_path)
    try:
        cur = conn.execute(
            "SELECT w.id, w.name, p.id, p.name "
            "FROM workspaces w JOIN projects p ON p.workspace_id = w.id"
        )
        ws_names: dict[str, str] = {}
        proj_lookup: dict[str, tuple[str, str]] = {}
        for ws_id, ws_name, proj_id, proj_name in cur.fetchall():
            ws_names[_uuid_str(ws_id)] = ws_name
            proj_lookup[_uuid_str(proj_id)] = (ws_name, proj_name)
        return ws_names, proj_lookup
    finally:
        conn.close()


def split_frontmatter(text: str) -> tuple[dict, str]:
    """Parse the first ---...--- block as JSON when possible; fall back to
    raw passthrough. The webhook re-serialises frontmatter as JSON anyway
    so anything not parsing yields an empty frontmatter object."""
    if not text.startswith("---\n"):
        return {}, text
    end = text.find("\n---\n", 4)
    if end < 0:
        return {}, text
    raw = text[4:end].strip()
    body = text[end + 5 :]
    if not raw:
        return {}, body
    # Engine writes frontmatter as YAML; rather than pull PyYAML, we
    # leave it as a string blob inside a single key so the backfilled
    # markdown still contains it verbatim.
    return {"_raw_frontmatter": raw}, body


def iter_pages(
    wiki_root: Path,
    proj_lookup: dict[str, tuple[str, str]],
    only_ws: str | None,
    only_proj: str | None,
) -> Iterator[tuple[str, str, str, Path]]:
    """Yield (workspace, project, page_path, file_path) for every .md."""
    for md in sorted(wiki_root.glob("*/*/**/*.md")):
        rel = md.relative_to(wiki_root)
        parts = rel.parts
        if len(parts) < 3:
            continue
        ws_id, proj_id, *page_parts = parts
        names = proj_lookup.get(proj_id)
        if names is None:
            # orphan page (project deleted but file remained); skip
            continue
        ws_name, proj_name = names
        if only_ws and ws_name != only_ws:
            continue
        if only_proj and proj_name != only_proj:
            continue
        page_path = "/".join(page_parts)
        # `log-YYYY-MM.md` is an audit ledger — the engine writes it
        # straight to disk bypassing write_page, so we skip it in the
        # mirror too (it churns on every event and isn't a wiki page).
        if page_path.startswith("log-") and page_path.endswith(".md") and len(page_path) == len("log-YYYY-MM.md"):
            continue
        yield ws_name, proj_name, page_path, md


def post_one(ws: str, proj: str, page_path: str, file: Path, timeout: float) -> int:
    """POST one page to the webhook. Returns the HTTP status code."""
    text = file.read_text(encoding="utf-8", errors="replace")
    fm, body = split_frontmatter(text)
    payload = json.dumps({
        "page": {
            "path": page_path,
            "frontmatter": fm,
            "body": body,
        },
        "ctx": {
            "workspace": ws,
            "project": proj,
            "actor": {"agent": "backfill", "user": "backfill-script"},
        },
    }).encode()
    req = urllib.request.Request(
        f"http://127.0.0.1:{LOCAL_PORT}/sync",
        data=payload,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status
    except urllib.error.HTTPError as e:
        return e.code


def start_port_forward() -> subprocess.Popen:
    """Start kubectl port-forward and wait for it to become reachable."""
    print(f">> kubectl port-forward svc/{GITMIRROR_SVC} {LOCAL_PORT}:{GITMIRROR_PORT}")
    pf = subprocess.Popen(
        ["kubectl", "-n", NS, "port-forward", f"svc/{GITMIRROR_SVC}", f"{LOCAL_PORT}:{GITMIRROR_PORT}"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        # Put pf in its own process group so we can clean up reliably.
        preexec_fn=os.setsid,
    )
    # Poll /healthz briefly to make sure the tunnel is up.
    for _ in range(40):
        time.sleep(0.25)
        try:
            urllib.request.urlopen(f"http://127.0.0.1:{LOCAL_PORT}/healthz", timeout=1).read()
            return pf
        except (urllib.error.URLError, ConnectionResetError):
            continue
    os.killpg(pf.pid, signal.SIGTERM)
    sys.exit("port-forward never became reachable")


def main() -> int:
    ap = argparse.ArgumentParser(description="Backfill git-mirror with existing pages.")
    ap.add_argument("--dry-run", action="store_true", help="enumerate without POSTing")
    ap.add_argument("--workspace", help="only this workspace name")
    ap.add_argument("--project", help="only this project name")
    ap.add_argument("--snapshot", help="reuse an existing snapshot tarball (skip the in-pod backup)")
    ap.add_argument("--keep-snapshot", action="store_true", help="don't delete the local tarball on exit")
    ap.add_argument("--timeout", type=float, default=15.0, help="per-page POST timeout seconds")
    args = ap.parse_args()

    work = Path(tempfile.mkdtemp(prefix="git-mirror-backfill-"))
    cleanup_paths: list[Path] = [work] if not args.keep_snapshot else []
    try:
        # 1. Snapshot
        tarball = Path(args.snapshot) if args.snapshot else work / "snap.tar.gz"
        if not args.snapshot:
            pod = fetch_main_pod()
            snapshot_pvc(pod, tarball)
        else:
            print(f">> reusing snapshot at {tarball}")
        # 2. Extract
        print(f">> extracting to {work}")
        with tarfile.open(tarball) as t:
            t.extractall(work, filter="data")  # type: ignore[arg-type]
        wiki_root = work / "wiki"
        db_path = work / "db" / "memory.sqlite"
        if not wiki_root.is_dir() or not db_path.is_file():
            sys.exit(f"tarball missing expected layout (wiki/ + db/memory.sqlite)")
        # 3. Resolve names
        ws_names, proj_lookup = load_name_lookup(db_path)
        print(f">> {len(ws_names)} workspaces, {len(proj_lookup)} projects in store")
        # 4. (optional) port-forward
        if args.dry_run:
            pf = None
        else:
            pf = start_port_forward()
        # 5. Iterate
        ok = 0
        skip = 0
        fail = 0
        try:
            for ws, proj, page_path, md in iter_pages(wiki_root, proj_lookup, args.workspace, args.project):
                if args.dry_run:
                    print(f"  dry  {ws}/{proj}/{page_path}")
                    skip += 1
                    continue
                code = post_one(ws, proj, page_path, md, args.timeout)
                if 200 <= code < 300:
                    print(f"  ok   {ws}/{proj}/{page_path}")
                    ok += 1
                else:
                    print(f"  FAIL {ws}/{proj}/{page_path} (HTTP {code})")
                    fail += 1
        finally:
            if pf:
                os.killpg(pf.pid, signal.SIGTERM)
        print(f"\ndone: ok={ok} fail={fail} dry={skip}")
        return 1 if fail else 0
    finally:
        for p in cleanup_paths:
            shutil.rmtree(p, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
