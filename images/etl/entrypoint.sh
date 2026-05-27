#!/usr/bin/env bash
#
# entrypoint.sh — PID 1 of the container. Replaced via `exec` by the chosen mode,
# so the one that responds to signals (k8s SIGTERM) is supercronic or run-etl.sh
# directly — both handle signals well.
#
# Modes:
#   cron (default)  — populates /etc/supercronic.crontab with ETL_SCHEDULE,
#                      then exec supercronic in foreground (becomes PID 1
#                      and does native reaping of zombies).
#   run             — runs run-etl.sh once and exits.
#
# Relevant env vars:
#   ETL_SCHEDULE  — cron 5-field (e.g.: "0 0 * * *"). Default "0 0 * * *".

set -euo pipefail

mode="${1:-cron}"

case "$mode" in
  run)
    exec /usr/local/bin/run-etl.sh
    ;;
  cron)
    schedule="${ETL_SCHEDULE:-0 0 * * *}"
    # Replace placeholder with the effective schedule
    sed "s|@@SCHEDULE@@|${schedule}|" /etc/supercronic.tmpl > /etc/supercronic.crontab
    echo "[entrypoint] supercronic schedule = '${schedule}'"
    exec /usr/local/bin/supercronic /etc/supercronic.crontab
    ;;
  *)
    echo "usage: entrypoint.sh [cron|run]" >&2
    echo "  cron (default) — run supercronic with ETL_SCHEDULE" >&2
    echo "  run            — run run-etl.sh once and exit" >&2
    exit 64
    ;;
esac
