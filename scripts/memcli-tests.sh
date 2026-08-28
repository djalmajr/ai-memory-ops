#!/usr/bin/env bash
# Live regression tests for bin/memcli against the real ai-memory server.
# Requires AI_MEMORY_AUTH_TOKEN in the environment. Read-only: only GET/POST
# search endpoints are exercised; no mutation.
set -u

MEMCLI="${MEMCLI:-$(dirname "$0")/../bin/memcli}"
pass=0 fail=0

ok()   { pass=$((pass+1)); echo "ok   - $1"; }
bad()  { fail=$((fail+1)); echo "FAIL - $1"; }
# expect <desc> <want_exit> <cmd…>  (want_exit: 0 or nonzero as "ne0")
expect() {
  local desc="$1" want="$2"; shift 2
  local out; out=$("$@" 2>&1); local got=$?
  case "$want" in
    0)   [ "$got" -eq 0 ] && ok "$desc" || { bad "$desc (exit=$got: $(echo "$out" | head -c 120))"; } ;;
    ne0) [ "$got" -ne 0 ] && ok "$desc" || { bad "$desc (expected failure, got success)"; } ;;
  esac
}

# --- JSON encoding: server returning 200 proves the body parsed as JSON ---
expect "quote in query"          0   "$MEMCLI" msearch 'say "hello"' limit=2 ai-memory
expect "backslash in query"      0   "$MEMCLI" msearch 'path\like\this' limit=2 ai-memory
expect "newline in query"        0   "$MEMCLI" msearch "$(printf 'line1\nline2')" limit=2 ai-memory
expect "tab in query"            0   "$MEMCLI" msearch "$(printf 'a\tb')" limit=2 ai-memory

# --- limit validation (client-side, no request sent) ---
expect "limit injection blocked" ne0 "$MEMCLI" msearch q 'limit=1,"x":2' ai-memory
expect "non-numeric limit"       ne0 "$MEMCLI" msearch q limit=abc ai-memory

# --- scopes ---
expect "cross-workspace scope"   0   "$MEMCLI" msearch fact limit=2 _perftmp/smoke djalmajr/ai-memory
expect "default-workspace scope" 0   "$MEMCLI" msearch tencent limit=2 ai-memory
expect "no scopes rejected"      ne0 "$MEMCLI" msearch q limit=2
over26=$(seq 1 26 | sed 's/^/p/')
# shellcheck disable=SC2086
expect ">25 scopes rejected"     ne0 "$MEMCLI" msearch q $over26

# --- passthrough / encoding regressions ---
expect "bad key=value rejected"  ne0 "$MEMCLI" briefing ai-memory bogus
expect "utf8 page path"          0   "$MEMCLI" read ai-memory follow-ups/avaliacao-tencent-pausada.md
expect "observations params"     0   "$MEMCLI" sessions ai-memory include_open=true limit=1

echo "----"
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
