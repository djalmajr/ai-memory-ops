#!/usr/bin/env bash
# capacity-report.sh — generates a structured JSON report of PVC usage for
# ai-memory-svc per project. Read-only; safe to run in parallel with the ETL.
#
# Output: 1 JSON line per execution on stdout.
#
# Env:
#   WIKI_ROOT  — root directory of the projects (default: /data/wiki-content/wiki)
#
# Exit codes:
#   0  — success (JSON on stdout)
#   1  — WIKI_ROOT missing or IO error

set -euo pipefail

WIKI_ROOT="${WIKI_ROOT:-/data/wiki-content/wiki}"

if [[ ! -d "$WIKI_ROOT" ]]; then
  echo "{\"error\":\"WIKI_ROOT not found: $WIKI_ROOT\",\"timestamp\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}" >&2
  exit 1
fi

# PVC-level stats (df of the filesystem that mounts the PVC)
PVC_LINE=$(df -B1 "$WIKI_ROOT" | tail -1)
PVC_TOTAL=$(echo "$PVC_LINE" | awk '{print $2}')
PVC_USED=$(echo "$PVC_LINE" | awk '{print $3}')
PVC_AVAIL=$(echo "$PVC_LINE" | awk '{print $4}')
PVC_PCT=$(awk "BEGIN {printf \"%.1f\", $PVC_USED * 100 / $PVC_TOTAL}")

# Severity bucket (matches runbook thresholds)
# awk instead of bc — bc is not present in the ETL image (Debian slim)
if awk "BEGIN {exit !($PVC_PCT < 50)}"; then
  SEVERITY="ok"
elif awk "BEGIN {exit !($PVC_PCT < 70)}"; then
  SEVERITY="info"
elif awk "BEGIN {exit !($PVC_PCT < 85)}"; then
  SEVERITY="warning"
else
  SEVERITY="critical"
fi

# Per-project: list subdirs of WIKI_ROOT (each one is a project)
PROJECTS_JSON="[]"
PROJECTS_RAW=$(mktemp)
trap 'rm -f "$PROJECTS_RAW"' EXIT

first=true
for d in "$WIKI_ROOT"/*/; do
  [[ -d "$d" ]] || continue
  name=$(basename "$d")
  bytes=$(du -sb "$d" 2>/dev/null | awk '{print $1}')
  docs=$(find "$d" -name '*.md' -type f 2>/dev/null | wc -l | tr -d ' ')
  if $first; then
    first=false
  else
    printf ',\n' >> "$PROJECTS_RAW"
  fi
  printf '    {"name": "%s", "bytes": %s, "docs": %s}' "$name" "$bytes" "$docs" >> "$PROJECTS_RAW"
done

# Emit JSON
cat <<EOF
{
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "wiki_root": "$WIKI_ROOT",
  "pvc": {
    "total_bytes": $PVC_TOTAL,
    "used_bytes": $PVC_USED,
    "avail_bytes": $PVC_AVAIL,
    "pct": $PVC_PCT,
    "severity": "$SEVERITY"
  },
  "projects": [
$(cat "$PROJECTS_RAW")
  ]
}
EOF
