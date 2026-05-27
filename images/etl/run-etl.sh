#!/usr/bin/env bash
#
# run-etl.sh — orchestrator of the ai-memory-svc ETL (ai-memory stack).
#
# Flow (per execution):
#   1. Optionally update skills (SKILLS_UPDATE_ON_RUN=true).
#   2. For each source in $WIKI_SOURCES_JSON:
#      a. shallow git clone of the source into /data/sources/<name>.
#      b. opencode run → markdown into /data/etl-out/<name>/.
#      c. opencode run wiki-lint → if FAIL, skip publishing the source.
#      d. For each generated .md: POST /admin/write-page to ai-memory
#         (workspace=$AI_MEMORY_WORKSPACE, project=<name>, path=rel, body=content).
#   3. Optional (ETL_RUN_INDEXER=true): POST /admin/embed to recompute vectors.
#
# Unlike the older stack, there is no clone/push of a wiki repo nor a separate index/embed step.
# ai-memory is the source of truth (single writer actor) — we write via HTTP,
# eliminating the ETL×MCP race (push_with_retry/merge=union are no longer needed).
#
# Consumed env vars (required ones marked with *):
#   OPENCODE_API_KEY*    — OpenCode plan key
#   WIKI_SOURCES_JSON*   — JSON array [{name,repoUrl,branch}]
#   AI_MEMORY_SERVER_URL — default "http://localhost:49374"
#   AI_MEMORY_WORKSPACE  — default "my-org"
#   AI_MEMORY_AUTH_TOKEN — optional bearer (2nd layer; empty = no header)
#   GITLAB_API_TOKEN     — clone private sources (fallback for SOURCE_GITLAB_TOKEN)
#   SOURCE_GITLAB_TOKEN  — alternative token only to clone the sources
#   WIKI_LANGUAGE        — default "en"
#   OPENCODE_MODEL       — default "opencode-go/kimi-k2.6"
#   ETL_RUN_INDEXER      — "true" triggers /admin/embed at the end (default true)
#   SKILLS_UPDATE_ON_RUN — "true" updates skills before the run

set -euo pipefail

# ---------------------------------------------------------------------------
# Validation of required env vars
# ---------------------------------------------------------------------------
: "${OPENCODE_API_KEY:?OPENCODE_API_KEY not set}"
: "${WIKI_SOURCES_JSON:?WIKI_SOURCES_JSON not set}"

: "${AI_MEMORY_SERVER_URL:=http://localhost:49374}"
: "${AI_MEMORY_WORKSPACE:=my-org}"
: "${AI_MEMORY_AUTH_TOKEN:=}"
: "${WIKI_LANGUAGE:=en}"
: "${OPENCODE_MODEL:=opencode-go/kimi-k2.6}"
: "${GITLAB_API_TOKEN:=}"
# SOURCE_GITLAB_TOKEN: alternative token only to clone the sources. Fallback to GITLAB_API_TOKEN.
: "${SOURCE_GITLAB_TOKEN:=$GITLAB_API_TOKEN}"

ETL_OUT_BASE="${ETL_OUT_BASE:-/data/etl-out}"
# Base for the source clones. Default under /data (PVC, same prefix as the opencode
# cwd). Overridable for local tests outside the container.
ETL_SOURCES_BASE="${ETL_SOURCES_BASE:-/data/sources}"
INGEST_INSTRUCTIONS="${INGEST_INSTRUCTIONS:-/usr/local/share/wiki-ingest/ingest-instructions.md}"
[[ -f "$INGEST_INSTRUCTIONS" ]] || INGEST_INSTRUCTIONS=""

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
ts()   { date -u +%Y-%m-%dT%H:%M:%SZ; }
log()  { printf '[run-etl %s] %s\n' "$(ts)" "$*"; }
warn() { printf '[run-etl %s] WARN: %s\n' "$(ts)" "$*" >&2; }
die()  { printf '[run-etl %s] ERROR: %s\n' "$(ts)" "$*" >&2; exit 1; }

# Inserts oauth2:TOKEN@ into HTTP(S) URLs for authenticated clone.
url_with_token() {
  local url="$1" tok="${2:-$GITLAB_API_TOKEN}"
  [[ -z "$tok" ]] && { echo "$url"; return; }
  echo "$url" | sed -E "s|(https?://)|\1oauth2:${tok}@|"
}

# curl against ai-memory with optional bearer. Args passed through to curl.
am_curl() {
  if [[ -n "$AI_MEMORY_AUTH_TOKEN" ]]; then
    curl -fsS -H "Authorization: Bearer $AI_MEMORY_AUTH_TOKEN" "$@"
  else
    curl -fsS "$@"
  fi
}

# Writes a page to ai-memory via /admin/write-page.
# Args: project relative_path file_path
write_page() {
  local project="$1" rel="$2" file="$3" payload
  payload=$(jq -n \
    --arg ws "$AI_MEMORY_WORKSPACE" --arg proj "$project" \
    --arg path "$rel" --rawfile body "$file" --arg tag "$project" \
    '{workspace:$ws, project:$proj, path:$path, body:$body, tier:"semantic", tags:[$tag]}')
  am_curl -X POST "$AI_MEMORY_SERVER_URL/admin/write-page" \
    -H 'Content-Type: application/json' -d "$payload" >/dev/null
}

# ---------------------------------------------------------------------------
# Precondition: ai-memory reachable
# ---------------------------------------------------------------------------
log "ai-memory at $AI_MEMORY_SERVER_URL (workspace=$AI_MEMORY_WORKSPACE)"
am_curl "$AI_MEMORY_SERVER_URL/admin/status" >/dev/null \
  || die "ai-memory did not respond at $AI_MEMORY_SERVER_URL/admin/status"

# ---------------------------------------------------------------------------
# Optional skills update
# ---------------------------------------------------------------------------
if [[ "${SKILLS_UPDATE_ON_RUN:-false}" == "true" ]]; then
  log "updating skills (npx -y skills update -y)"
  npx -y skills update -y 2>&1 | tail -5 || warn "skills update failed; using bundled"
else
  log "using skills bundled in the image (SKILLS_UPDATE_ON_RUN=false)"
fi

# ---------------------------------------------------------------------------
# Iterate sources
# ---------------------------------------------------------------------------
sources_count=$(echo "$WIKI_SOURCES_JSON" | jq 'length')
log "ingest of $sources_count source(s)"

any_published=0

for i in $(seq 0 $((sources_count - 1))); do
  name=$(echo "$WIKI_SOURCES_JSON" | jq -r ".[$i].name")
  repo=$(echo "$WIKI_SOURCES_JSON" | jq -r ".[$i].repoUrl")
  branch=$(echo "$WIKI_SOURCES_JSON" | jq -r ".[$i].branch // \"main\"")
  # Per-source token: each source may declare `tokenEnv` (name of the env var with
  # its GitLab PAT). Sources on distinct GitLabs (e.g.: gitlab.example.com vs
  # gitlab.example.com) use different tokens. Default: SOURCE_GITLAB_TOKEN.
  token_env=$(echo "$WIKI_SOURCES_JSON" | jq -r ".[$i].tokenEnv // \"SOURCE_GITLAB_TOKEN\"")
  src_token="${!token_env:-$SOURCE_GITLAB_TOKEN}"

  log "==[$name]== clone $repo (branch=$branch)"
  # OpenCode blocks reads outside the working dir. We clone under /data/ (same
  # prefix as the opencode cwd) and generate the output under /data/ too.
  src_dir="$ETL_SOURCES_BASE/$name"
  out_dir="$ETL_OUT_BASE/$name"
  rm -rf "$src_dir" "$out_dir"
  mkdir -p "$(dirname "$src_dir")" "$out_dir"
  src_url=$(url_with_token "$repo" "$src_token")
  if ! git clone --branch "$branch" --depth 1 "$src_url" "$src_dir" 2>&1 | tail -3; then
    warn "[$name] source clone failed — skipping"
    continue
  fi

  # ---- opencode run — generates markdown straight from the source ----
  log "[$name] running ingest via opencode ($OPENCODE_MODEL)"
  ingest_prompt="Analyze the project source code in $src_dir and generate technical
documentation in Markdown under $out_dir/.

Minimum structure to create:
- $out_dir/index.md (project overview)
- $out_dir/architecture.md (layers, runtime, integrations)
- $out_dir/modules/<name>.md (one per relevant top-level module/folder)
- $out_dir/dependencies.md (external libs + versions)

Content language: $WIKI_LANGUAGE."

  if [[ -n "$INGEST_INSTRUCTIONS" && -f "$INGEST_INSTRUCTIONS" ]]; then
    # OpenCode blocks Read outside the working dir — we inline the directives.
    ingest_prompt+="

============ GUIDELINES (follow strictly) ============
$(cat "$INGEST_INSTRUCTIONS")
============ END OF GUIDELINES ============"
  fi
  ingest_prompt+="

Do NOT use any skill — generate the files directly.
Do NOT modify files outside $out_dir.
Do NOT commit or git push.
After generating, list the created files."

  if ! opencode run --model "$OPENCODE_MODEL" "$ingest_prompt" 2>&1 | tee /tmp/ingest-"$name".log; then
    warn "[$name] opencode ingest failed — not publishing"
    rm -rf "$src_dir"
    continue
  fi

  # ---- validate that .md files were generated ----
  md_files=$(find "$out_dir" -maxdepth 4 -name "*.md" -type f | wc -l | tr -d ' ')
  if [[ "$md_files" -lt 1 ]]; then
    warn "[$name] opencode ran but generated no .md — aborting publication"
    rm -rf "$src_dir"
    continue
  fi
  log "[$name] $md_files .md file(s) generated"

  # ---- opencode run wiki-lint (BLOCKING via parsing) ----
  log "[$name] running wiki-lint (blocking via parsing)"
  lint_prompt="Use the wiki-lint skill to validate all files in $out_dir.

CRITICAL — output protocol (parsed by the orchestrator):
- If there is ANY blocking error, include as the LAST line: LINT_RESULT: FAIL
- If it passes (warnings accepted), include: LINT_RESULT: PASS
- Do not use other variations of these strings."

  if ! opencode run --model "$OPENCODE_MODEL" "$lint_prompt" 2>&1 | tee /tmp/lint-"$name".log; then
    warn "[$name] opencode/wiki-lint exec failed — aborting publication"
    rm -rf "$src_dir"
    continue
  fi
  lint_marker=$(grep -oE 'LINT_RESULT:\s*(PASS|FAIL)' /tmp/lint-"$name".log | tail -1 | awk '{print $NF}')
  if [[ "$lint_marker" != "PASS" ]]; then
    warn "[$name] wiki-lint = '${lint_marker:-MISSING}' — aborting publication (expected PASS)"
    rm -rf "$src_dir"
    continue
  fi
  log "[$name] wiki-lint: PASS"

  # ---- publish each .md to ai-memory (/admin/write-page) ----
  published=0
  while IFS= read -r file; do
    rel="${file#"$out_dir"/}"
    if write_page "$name" "$rel" "$file"; then
      published=$((published + 1))
    else
      warn "[$name] failed to write $rel to ai-memory"
    fi
  done < <(find "$out_dir" -maxdepth 4 -name "*.md" -type f)

  log "[$name] $published/$md_files page(s) published to $AI_MEMORY_WORKSPACE/$name"
  if [[ "$published" -gt 0 ]]; then
    any_published=1
    # Per-project embed (/admin/embed is scoped by workspace+project; calling only
    # with workspace falls back to the default project and embeds nothing). Tolerable no-op when
    # embeddings are disabled on the server (FTS5-only).
    if [[ "${ETL_RUN_INDEXER:-true}" == "true" ]]; then
      log "[$name] /admin/embed (my-org/$name)"
      am_curl -X POST "$AI_MEMORY_SERVER_URL/admin/embed" \
        -H 'Content-Type: application/json' \
        -d "$(jq -n --arg ws "$AI_MEMORY_WORKSPACE" --arg p "$name" '{workspace:$ws, project:$p}')" \
        2>/dev/null | head -c 200 || warn "[$name] embed failed/inapplicable — continuing"
      echo
    fi
  fi

  rm -rf "$src_dir"
done

if [[ $any_published -eq 1 ]]; then
  log "ETL run complete — pages published to ai-memory"
else
  log "ETL run complete — nothing published"
fi
