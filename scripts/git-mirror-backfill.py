#!/usr/bin/env python3
"""git-mirror-backfill — seed the backup repo with every existing page in
ONE commit.

The git-mirror webhook is forward-only: it mirrors writes that happen
AFTER it's wired into the admission chain. Anything older sits in the
engine's PVC but never reaches the backup. This script walks a live
snapshot, decodes the workspace+project UUIDs into human names, and
writes every page into a fresh clone of the backup repo — committing
the whole batch as a single "backfill" snapshot rather than 1 commit
per page (which would produce hundreds of fake timestamps for a
one-shot seed).

Workflow:

  1. `ai-memory backup` inside the main pod → tarball with wiki/ + db/
  2. `kubectl cp` the tarball out
  3. Read sqlite to translate ws_id/proj_id → (workspace_name, project_name)
  4. `git clone` (or init) the backup repo in a tempdir, using REPO_URL
     from either the env or the cluster Secret used by the live webhook
  5. Write every .md into wiki/<ws>/<proj>/<path> in the working tree
  6. `git add -A && git commit && git push` — ONE commit, ONE push

Bypasses the engine's admission chain (no webhook call), so re-running
this never re-triggers contributors / other hooks. Idempotent — if no
content changed since the last backfill the commit is skipped (no
`--allow-empty`).

Requirements (local): python3, kubectl, git, tar.

Usage:

    ./git-mirror-backfill.py                            # ns=memory, release=memory-v2
    NS=memory RELEASE=memory-v2 ./git-mirror-backfill.py
    ./git-mirror-backfill.py --dry-run                  # enumerate without writing
    ./git-mirror-backfill.py --workspace default --project ai-memory-ops
    ./git-mirror-backfill.py --snapshot /tmp/snap.tar.gz
    REPO_URL='https://user:token@github.com/owner/repo.git' ./git-mirror-backfill.py
"""

from __future__ import annotations

import argparse
import os
import shutil
import sqlite3
import subprocess
import sys
import tarfile
import tempfile
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Iterator

NS = os.environ.get("NS", "memory")
RELEASE = os.environ.get("RELEASE", "memory-v2")
SECRET_NAME = os.environ.get("SECRET_NAME", f"{RELEASE}-git-mirror-url")
SECRET_KEY = os.environ.get("SECRET_KEY", "REPO_URL")
REPO_BRANCH = os.environ.get("REPO_BRANCH", "main")
GIT_USER = os.environ.get("GIT_USER", "git-mirror-backfill")
GIT_EMAIL = os.environ.get("GIT_EMAIL", "backfill@ai-memory.local")
MAIN_LABEL = "app.kubernetes.io/component=main"


def sh(*args: str, check: bool = True, capture: bool = True, cwd: str | None = None) -> str:
    res = subprocess.run(args, check=check, capture_output=capture, text=True, cwd=cwd)
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
    kubectl("exec", "-c", "ai-memory", pod, "--", "rm", "-f", remote)


def resolve_repo_url() -> str:
    """Resolve REPO_URL: explicit env var wins; otherwise read from the
    same Secret the live git-mirror Deployment consumes."""
    explicit = os.environ.get("REPO_URL")
    if explicit:
        return explicit
    try:
        b64 = kubectl(
            "get", "secret", SECRET_NAME,
            "-o", f"jsonpath={{.data.{SECRET_KEY}}}",
        )
    except subprocess.CalledProcessError:
        sys.exit(
            f"REPO_URL not set and Secret {NS}/{SECRET_NAME} key {SECRET_KEY} not found. "
            "Either export REPO_URL=https://user:token@host/owner/repo.git "
            "or create the Secret first."
        )
    if not b64:
        sys.exit(f"Secret {NS}/{SECRET_NAME} key {SECRET_KEY} is empty")
    import base64
    return base64.b64decode(b64).decode().strip()


def _uuid_str(raw: bytes | str) -> str:
    """Engine stores workspace/project ids as 16-byte BLOB but uses the
    hex-with-dashes form for on-disk paths. Coerce both to the path form."""
    if isinstance(raw, bytes):
        return str(uuid.UUID(bytes=raw))
    return raw


def load_name_lookup(db_path: Path) -> dict[str, tuple[str, str]]:
    """Return project_id -> (workspace_name, project_name)."""
    conn = sqlite3.connect(db_path)
    try:
        cur = conn.execute(
            "SELECT w.name, p.id, p.name "
            "FROM workspaces w JOIN projects p ON p.workspace_id = w.id"
        )
        return {_uuid_str(pid): (ws, name) for ws, pid, name in cur.fetchall()}
    finally:
        conn.close()


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
        _ws_id, proj_id, *page_parts = parts
        names = proj_lookup.get(proj_id)
        if names is None:
            continue
        ws_name, proj_name = names
        if only_ws and ws_name != only_ws:
            continue
        if only_proj and proj_name != only_proj:
            continue
        page_path = "/".join(page_parts)
        # `log-YYYY-MM.md` is an audit ledger — the engine writes it
        # straight to disk bypassing write_page, so we skip it in the
        # mirror too (high-churn append log, no backup value).
        name = Path(page_path).name
        if name.startswith("log-") and name.endswith(".md") and len(name) == len("log-YYYY-MM.md"):
            continue
        yield ws_name, proj_name, page_path, md


def git(*args: str, cwd: Path) -> str:
    """Wrapper to run git inside the working tree (or anywhere via cwd)."""
    return sh("git", *args, cwd=str(cwd))


def setup_workdir(repo_url: str, workdir: Path) -> None:
    """Clone the backup repo into `workdir`. Falls back to init+remote-add
    if the remote is empty (fresh repo, no branch yet)."""
    # Try a shallow clone of REPO_BRANCH; fall back to plain clone; finally
    # init locally and add origin (for a brand-new empty remote).
    try:
        sh("git", "clone", "--branch", REPO_BRANCH, "--depth", "1", repo_url, str(workdir))
    except subprocess.CalledProcessError:
        try:
            shutil.rmtree(workdir, ignore_errors=True)
            workdir.mkdir(parents=True, exist_ok=True)
            sh("git", "clone", repo_url, str(workdir))
            git("checkout", "-B", REPO_BRANCH, cwd=workdir)
        except subprocess.CalledProcessError:
            shutil.rmtree(workdir, ignore_errors=True)
            workdir.mkdir(parents=True, exist_ok=True)
            git("init", "-b", REPO_BRANCH, cwd=workdir)
            git("remote", "add", "origin", repo_url, cwd=workdir)
    git("config", "user.name", GIT_USER, cwd=workdir)
    git("config", "user.email", GIT_EMAIL, cwd=workdir)


def clear_wiki_tree(workdir: Path) -> None:
    """Remove the existing wiki/ contents so the backfill reflects the
    snapshot's truth (deletions in the engine become deletions in the
    backup). The .git dir is untouched."""
    wiki = workdir / "wiki"
    if wiki.exists():
        shutil.rmtree(wiki)
    wiki.mkdir()


def write_pages(workdir: Path, pages: list[tuple[str, str, str, Path]]) -> int:
    """Copy each source .md into `workdir/wiki/<ws>/<proj>/<path>`.
    Returns the number of files written."""
    count = 0
    for ws, proj, page_path, src in pages:
        dest = workdir / "wiki" / ws / proj / page_path
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(src, dest)
        count += 1
    return count


def main() -> int:
    ap = argparse.ArgumentParser(description="One-shot backfill of the git-mirror backup repo.")
    ap.add_argument("--dry-run", action="store_true", help="enumerate without writing anything")
    ap.add_argument("--workspace", help="only this workspace name")
    ap.add_argument("--project", help="only this project name")
    ap.add_argument("--snapshot", help="reuse an existing snapshot tarball (skip the in-pod backup)")
    ap.add_argument("--keep-snapshot", action="store_true", help="don't delete the local tarball on exit")
    ap.add_argument("--message", help="override the commit message")
    ap.add_argument("--no-push", action="store_true", help="commit locally but don't push")
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
        snap_dir = work / "snap"
        snap_dir.mkdir()
        print(f">> extracting to {snap_dir}")
        with tarfile.open(tarball) as t:
            t.extractall(snap_dir, filter="data")  # type: ignore[arg-type]
        wiki_root = snap_dir / "wiki"
        db_path = snap_dir / "db" / "memory.sqlite"
        if not wiki_root.is_dir() or not db_path.is_file():
            sys.exit("tarball missing expected layout (wiki/ + db/memory.sqlite)")
        # 3. Resolve names + enumerate pages
        proj_lookup = load_name_lookup(db_path)
        print(f">> {len(proj_lookup)} projects in store")
        pages = list(iter_pages(wiki_root, proj_lookup, args.workspace, args.project))
        print(f">> {len(pages)} pages selected")
        if args.dry_run:
            for ws, proj, page_path, _ in pages:
                print(f"  dry  {ws}/{proj}/{page_path}")
            return 0
        if not pages:
            print(">> nothing to backfill")
            return 0
        # 4. Clone backup repo
        repo_url = resolve_repo_url()
        # Mask the token in the log line.
        safe = repo_url
        if "@" in safe:
            scheme, rest = safe.split("://", 1) if "://" in safe else ("", safe)
            creds, host = rest.split("@", 1)
            if ":" in creds:
                user, _ = creds.split(":", 1)
                safe = f"{scheme}://{user}:<TOKEN>@{host}" if scheme else f"{user}:<TOKEN>@{host}"
        repo_dir = work / "repo"
        print(f">> cloning {safe} (branch {REPO_BRANCH}) into {repo_dir}")
        setup_workdir(repo_url, repo_dir)
        # 5. Replace wiki/ with the snapshot contents
        clear_wiki_tree(repo_dir)
        written = write_pages(repo_dir, pages)
        print(f">> wrote {written} files into {repo_dir}/wiki")
        # 6. One commit + one push
        git("add", "-A", cwd=repo_dir)
        status = git("status", "--porcelain", cwd=repo_dir)
        if not status:
            print(">> nothing changed since last backfill; skipping commit")
            return 0
        msg = args.message or (
            f"backfill {datetime.now(timezone.utc).strftime('%Y-%m-%d %H:%M:%SZ')}: "
            f"{written} pages from snapshot"
        )
        git("commit", "-m", msg, cwd=repo_dir)
        if args.no_push:
            print(">> commit done; --no-push set, skipping git push")
        else:
            print(">> pushing")
            git("push", "-u", "origin", REPO_BRANCH, cwd=repo_dir)
        print(f"\ndone: {written} pages in one commit")
        return 0
    finally:
        for p in cleanup_paths:
            shutil.rmtree(p, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
