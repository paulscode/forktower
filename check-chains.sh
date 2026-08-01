#!/bin/sh
#
# Are both chains being watched?
#
# Separate from the dashboard check, and non-blocking, so that a second node
# still catching up reads as "not finished yet" rather than as a broken package.
# The distinction matters: one of these means wait, and the other means act.
set -eu

status="$(curl -fsS --max-time 10 "http://127.0.0.1:8330/api/v1/status" 2>/dev/null || true)"
if [ -z "${status}" ]; then
  echo '{"version":0,"message":"Waiting for Forktower to answer.","result":"starting"}'
  exit 0
fi

# `jq -r` rather than a grep: the field is nested, and a substring match on JSON
# is the kind of shortcut that reports success against the wrong key.
sq_state="$(printf '%s' "${status}" | jq -r '.views.sq.state // "unknown"' 2>/dev/null || echo unknown)"
sf_state="$(printf '%s' "${status}" | jq -r '.views.sf.state // "unknown"' 2>/dev/null || echo unknown)"
progress="$(printf '%s' "${status}" | jq -r '.views.sq.sync_progress // 0' 2>/dev/null || echo 0)"

# The states a view reports about itself are OK, SYNCING and DEGRADED; the rest
# are conclusions drawn by comparing views, and the sharpest of them —
# ECLIPSE_SUSPECT — must not be reported as "not answering", because a view being
# fed a fabricated quiet chain answers perfectly well. That is the case this
# whole program exists for.
case "${sf_state}" in
  OK|SYNCING) ;;
  DEGRADED)
    echo '{"version":0,"message":"Your own Bitcoin node is answering, but something is off with it.","result":"failure"}'
    exit 0
    ;;
  ECLIPSE_SUSPECT)
    echo '{"version":0,"message":"Independent sources disagree about your own chain. Open the dashboard.","result":"failure"}'
    exit 0
    ;;
  *)
    echo '{"version":0,"message":"Cannot reach your own Bitcoin node.","result":"failure"}'
    exit 0
    ;;
esac

case "${sq_state}" in
  OK)
    echo '{"version":0,"message":"Both chains are being watched.","result":"success"}'
    ;;
  SYNCING)
    pct="$(awk -v p="${progress}" 'BEGIN{printf "%.1f", p*100}' 2>/dev/null || echo 0)"
    printf '{"version":0,"message":"Catching up on the other chain — %s%% of the way.","result":"starting"}\n' "${pct}"
    ;;
  DEGRADED)
    echo '{"version":0,"message":"The other chain is being watched, but the view is unhealthy. Open the dashboard.","result":"failure"}'
    ;;
  ECLIPSE_SUSPECT)
    echo '{"version":0,"message":"Independent sources disagree about the other chain. Open the dashboard.","result":"failure"}'
    ;;
  *)
    echo '{"version":0,"message":"The second Bitcoin node is not answering.","result":"failure"}'
    ;;
esac
exit 0
