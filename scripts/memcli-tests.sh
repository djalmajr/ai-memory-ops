#!/usr/bin/env bash
# Live regression tests for bin/memcli against a real ai-memory server.
#
# Requirements: AI_MEMORY_AUTH_TOKEN in the environment, jq on PATH.
# Read-only by default. The non-ASCII page-path roundtrip mutates a scratch
# scope and only runs when BOTH MEMCLI_ALLOW_WRITE=1 and MEMCLI_ALLOW_DELETE=1
# are set (it needs the ai-memory binary); otherwise it is reported as skipped.
#
# Configurable scopes (defaults suit the personal deployment):
#   T_PROJECT        project with real pages/sessions   (default: ai-memory)
#   T_SCRATCH_SCOPE  <workspace/project> for the write roundtrip
#                    (default: _perftmp/memcli-tests)
set -u

MEMCLI="${MEMCLI:-$(dirname "$0")/../bin/memcli}"
T_PROJECT="${T_PROJECT:-ai-memory}"
T_SCRATCH_SCOPE="${T_SCRATCH_SCOPE:-_perftmp/memcli-tests}"
WS="${MEMCLI_WORKSPACE:-djalmajr}"

command -v jq >/dev/null || { echo "memcli-tests: jq is required" >&2; exit 2; }

pass=0 fail=0 skip=0
ok()   { pass=$((pass+1)); echo "ok   - $1"; }
bad()  { fail=$((fail+1)); echo "FAIL - $1"; }
skipt(){ skip=$((skip+1)); echo "skip - $1"; }

# expect_fail <desc> <cmd…> — command must fail (client-side rejection).
expect_fail() {
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then bad "$desc (expected failure, got success)"; else ok "$desc"; fi
}

# expect_json <desc> <jq-assert-expr> <cmd…> — command must succeed AND the
# output must satisfy the jq boolean expression (-e).
expect_json() {
  local desc="$1" expr="$2"; shift 2
  local out; out=$("$@" 2>&1) || { bad "$desc (exit: $(echo "$out" | head -c 120))"; return; }
  if echo "$out" | jq -e "$expr" >/dev/null 2>&1; then ok "$desc"
  else bad "$desc (jq assert '$expr' failed on: $(echo "$out" | head -c 120))"; fi
}

# --- JSON encoding: 200 + parseable array proves the POST body was valid JSON ---
expect_json "quote in query"     'type=="array"' "$MEMCLI" msearch 'say "hello"' limit=2 "$T_PROJECT"
expect_json "backslash in query" 'type=="array"' "$MEMCLI" msearch 'path\like\this' limit=2 "$T_PROJECT"
expect_json "newline in query"   'type=="array"' "$MEMCLI" msearch "$(printf 'line1\nline2')" limit=2 "$T_PROJECT"
expect_json "tab in query"       'type=="array"' "$MEMCLI" msearch "$(printf 'a\tb')" limit=2 "$T_PROJECT"

# --- limit validation (client-side, no request sent) ---
expect_fail "limit injection blocked" "$MEMCLI" msearch q 'limit=1,"x":2' "$T_PROJECT"
expect_fail "non-numeric limit"       "$MEMCLI" msearch q limit=abc "$T_PROJECT"

# --- scopes ---
expect_json "default-workspace scope" 'type=="array"' "$MEMCLI" msearch tencent limit=2 "$T_PROJECT"
expect_fail "no scopes rejected"  "$MEMCLI" msearch q limit=2
# shellcheck disable=SC2046
expect_fail ">25 scopes rejected" "$MEMCLI" msearch q $(seq 1 26 | sed 's/^/p/')

# Cross-workspace scope: discovered dynamically — first non-$WS workspace
# with at least one project; skipped when the deployment has none.
xws=$("$MEMCLI" workspaces 2>/dev/null | jq -r --arg ws "$WS" \
  '[.[] | select(.workspace_name != $ws and .project_count > 0)][0].workspace_name // empty')
xproj=''
if [ -n "$xws" ]; then
  xproj=$(MEMCLI_WORKSPACE="$xws" "$MEMCLI" projects 2>/dev/null | jq -r '.[0].project_name // empty')
fi
if [ -n "$xws" ] && [ -n "$xproj" ]; then
  expect_json "cross-workspace scope ($xws/$xproj)" 'type=="array"' \
    "$MEMCLI" msearch fact limit=2 "$xws/$xproj" "$T_PROJECT"
else
  skipt "cross-workspace scope (no second workspace with projects)"
fi

# --- passthrough validation ---
expect_fail "bad key=value rejected" "$MEMCLI" briefing "$T_PROJECT" bogus
expect_json "briefing limit= passthrough" '.counts.pages_latest >= 0' "$MEMCLI" briefing "$T_PROJECT" limit=1
expect_json "sessions params" '.sessions | type=="array"' \
  "$MEMCLI" sessions "$T_PROJECT" include_open=true limit=1

# --- observations: real session id, real filters, field-level asserts ---
sid=$("$MEMCLI" sessions "$T_PROJECT" include_open=true limit=1 2>/dev/null | jq -r '.sessions[0].session_id // empty')
if [ -n "$sid" ]; then
  expect_json "observations filters (order/total/limit honored)" \
    '(.order=="desc") and (.total|type=="number") and ((.observations|length) <= 2) and (.body_max_chars==500)' \
    "$MEMCLI" observations "$T_PROJECT" "$sid" limit=2 order=desc kinds=user-prompt,stop body_max_chars=500
else
  skipt "observations filters (no visible session in $T_PROJECT)"
fi

# --- scratch-write safety machinery -----------------------------------------
SERVER="${MEMCLI_SERVER_URL:-https://memory.djalmajr.dev}"

# Percent-encode a page path per segment (preserves '/').
enc_uri_path() { jq -rn --arg s "$1" '$s | split("/") | map(@uri) | join("/")'; }

# HTTP status of a page probe: 404 = safely absent, 200 = exists,
# anything else (401/403/5xx) or 000 (network) = NOT safe to write.
probe_status() { # <ws> <proj> <path>
  local code
  code=$(curl -sS -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer ${AI_MEMORY_AUTH_TOKEN:-}" \
    "$SERVER/api/v1/workspaces/$1/projects/$2/pages/$(enc_uri_path "$3")" 2>/dev/null) || code=000
  printf %s "$code"
}

# Write-gate decision. The write call exists ONLY inside the `proceed` branch,
# so proving the gate proves a non-404 preflight can never write.
gate() { case "$1" in 404) echo proceed ;; 200) echo exists ;; *) echo "blocked:$1" ;; esac; }

# --- fault injection: non-404 preflights must block (read-only, always run) ---
fi_path="notes/memcli faultprobe $(date +%s)-$$-${RANDOM}.md"
st=$(AI_MEMORY_AUTH_TOKEN="invalid-token" probe_status "$WS" "$T_PROJECT" "$fi_path")
[ "$(gate "$st")" = "blocked:401" ] \
  && ok "fault: bad-token preflight blocks write (status $st)" \
  || bad "fault: bad-token preflight not blocked (status $st → $(gate "$st"))"
st=$(SERVER="http://127.0.0.1:9" probe_status "$WS" "$T_PROJECT" "$fi_path")
[ "$(gate "$st")" = "blocked:000" ] \
  && ok "fault: network-failure preflight blocks write (status $st)" \
  || bad "fault: network-failure preflight not blocked (status $st → $(gate "$st"))"
# 200 test: discover a real page from T_PROJECT instead of hardcoding one.
epage=$("$MEMCLI" pages "$T_PROJECT" 2>/dev/null | jq -r '.[0].path // empty')
if [ -n "$epage" ]; then
  st=$(probe_status "$WS" "$T_PROJECT" "$epage")
  [ "$(gate "$st")" = "exists" ] \
    && ok "fault: existing page refused (status $st: $epage)" \
    || bad "fault: existing page not refused (status $st → $(gate "$st"): $epage)"
else
  skipt "fault: existing page refused (no pages in $T_PROJECT)"
fi

# --- enc_path: non-ASCII + space + parentheses in a REAL page path ----------
# Full roundtrip (scope preflight → 404 preflight → write → read-back assert
# → verified teardown) in a scratch scope, plus an interrupted-write fault
# test. The suite NEVER creates scopes: write-page auto-creates missing
# workspaces/projects, so writing into a nonexistent scope would leave scope
# rows behind even after page deletion. The scratch scope must be provisioned
# once by the operator (fail-closed otherwise), e.g.:
#   printf '# memcli-tests scratch scope\n\nLive-test scratch; safe to purge.\n' | \
#     MEMCLI_ALLOW_WRITE=1 MEMCLI_WORKSPACE=<ws> memcli write <proj> readme.md -
if [ "${MEMCLI_ALLOW_WRITE:-0}" = "1" ] && [ "${MEMCLI_ALLOW_DELETE:-0}" = "1" ] && command -v ai-memory >/dev/null; then
  sws="${T_SCRATCH_SCOPE%%/*}"; sproj="${T_SCRATCH_SCOPE#*/}"
  # Fail-closed scope preflight: workspace AND project must already exist.
  scope_ok=$(MEMCLI_WORKSPACE="$sws" "$MEMCLI" projects 2>/dev/null | \
    jq -r --arg p "$sproj" '[.[] | select(.project_name == $p)] | length')
  if [ "${scope_ok:-0}" != "1" ]; then
    bad "scratch scope $T_SCRATCH_SCOPE does not exist — refusing to write (provision it once; see header)"
  else
  upath="notes/memcli enc-path ação çê $(date +%s)-$$-${RANDOM} (test).md"
  cleanup_scratch() {
    MEMCLI_WORKSPACE="$sws" "$MEMCLI" delete "$sproj" "$upath" >/dev/null 2>&1 || true
  }
  # Teardown: delete (idempotent) + probe. Traps are cleared ONLY here, and
  # only after the probe verifies 404 — covers write-failure-after-commit and
  # delete-failure orphans alike.
  scratch_teardown() { cleanup_scratch; [ "$(probe_status "$sws" "$sproj" "$upath")" = "404" ]; }
  st=$(probe_status "$sws" "$sproj" "$upath")
  case "$(gate "$st")" in
    proceed)
      # Signal handlers route through the VERIFIED teardown: EXIT is cleared
      # only on a probe-confirmed 404; otherwise the EXIT trap stays armed as
      # a last-chance retry. Armed BEFORE the write: covers the
      # commit-before-return window.
      on_scratch_signal() {
        if scratch_teardown; then trap - EXIT INT TERM; fi
        exit "$1"
      }
      trap 'on_scratch_signal 130' INT
      trap 'on_scratch_signal 143' TERM
      trap 'scratch_teardown || true' EXIT
      if printf '# Título ação çê\n\nenc_path roundtrip marker\n' | \
           MEMCLI_WORKSPACE="$sws" "$MEMCLI" write "$sproj" "$upath" - >/dev/null 2>&1; then
        expect_json "non-ascii page path read-back" \
          '.title=="Título ação çê"' \
          env MEMCLI_WORKSPACE="$sws" "$MEMCLI" read "$sproj" "$upath"
      else
        # The server may still have committed before the client failed;
        # teardown below settles it either way.
        bad "non-ascii page path write returned failure (scratch scope $T_SCRATCH_SCOPE)"
      fi
      if scratch_teardown; then
        ok "scratch teardown verified (probe 404)"
        trap - EXIT INT TERM
      else
        # EXIT trap stays armed for a last-chance cleanup on process exit.
        bad "scratch teardown NOT verified — page may remain: $T_SCRATCH_SCOPE/$upath"
      fi ;;
    exists)    bad "scratch path unexpectedly exists — refusing to touch it: $T_SCRATCH_SCOPE/$upath" ;;
    blocked:*) bad "scratch preflight status $st — refusing to write" ;;
  esac

  # --- fault injection: signals after the write commits ----------------------
  # Matrix: INT/130 happy path, TERM/143 happy path, and a simulated cleanup
  # failure inside the signal handler (auth/network-style) that must be
  # rescued by the still-armed EXIT retry.
  run_sig_fault() { # <desc> <SIGNAL> <expected-rc> <fail-first-cleanup 0|1>
    local desc="$1" sig="$2" want="$3" ff="$4"
    local p="notes/memcli sig-fault $sig-$ff $(date +%s)-$$-${RANDOM}.md"
    local pst; pst=$(probe_status "$sws" "$sproj" "$p")
    if [ "$(gate "$pst")" != "proceed" ]; then bad "$desc (preflight status $pst — refusing to write)"; return; fi
    sh -c '
      MEMCLI="$1"; sws="$2"; sproj="$3"; upath="$4"; sig="$5"; ff="$6"
      tried=0
      teardown() {
        if [ "$ff" = 1 ] && [ "$tried" = 0 ]; then tried=1; return 1; fi  # simulated delete failure
        MEMCLI_WORKSPACE="$sws" "$MEMCLI" delete "$sproj" "$upath" >/dev/null 2>&1
        ! MEMCLI_WORKSPACE="$sws" "$MEMCLI" read "$sproj" "$upath" >/dev/null 2>&1
      }
      code=130; [ "$sig" = TERM ] && code=143
      on_sig() { if teardown; then trap - EXIT; fi; exit "$code"; }
      trap on_sig INT TERM
      trap "teardown || true" EXIT
      printf "# sig fault\n\nmarker\n" | MEMCLI_WORKSPACE="$sws" "$MEMCLI" write "$sproj" "$upath" - >/dev/null 2>&1
      kill -"$sig" $$
      exit 99  # unreachable when the trap fires
    ' _ "$MEMCLI" "$sws" "$sproj" "$p" "$sig" "$ff" >/dev/null 2>&1
    local rc=$? fst
    fst=$(probe_status "$sws" "$sproj" "$p")
    if [ "$rc" -eq "$want" ] && [ "$fst" = "404" ]; then
      ok "$desc"
    else
      # Best-effort recovery, then report the FINAL state.
      MEMCLI_WORKSPACE="$sws" "$MEMCLI" delete "$sproj" "$p" >/dev/null 2>&1 || true
      fst=$(probe_status "$sws" "$sproj" "$p")
      bad "$desc (rc=$rc want=$want, final probe=$fst)$([ "$fst" != "404" ] && echo " — page remains: $T_SCRATCH_SCOPE/$p")"
    fi
  }
  run_sig_fault "fault: interrupted write cleaned up and exited 130" INT 130 0
  run_sig_fault "fault: terminated write cleaned up and exited 143" TERM 143 0
  run_sig_fault "fault: failed in-handler cleanup rescued by EXIT retry (rc 130)" INT 130 1
  fi  # scope preflight
else
  skipt "non-ascii page path roundtrip + interrupt fault (needs MEMCLI_ALLOW_WRITE=1, MEMCLI_ALLOW_DELETE=1, ai-memory binary)"
fi

echo "----"
echo "pass=$pass fail=$fail skip=$skip"
[ "$fail" -eq 0 ]
