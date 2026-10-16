#!/usr/bin/env bash
# The rollout's safety watcher. rollout.sh starts it in the background and stops it; it can also
# be run once by hand. Every few seconds it reads /v1/health of every node, read-only, with the
# OBSERVER token (~/.config/aivoicebus/observer-token; the shared token when there is none), and
# trips when
#   - a node on the release being rolled out holds MORE quarantined records than it did when the
#     watch began (a peer offered a record no client writes: the new release may be refusing real
#     history; a placeholder from before the rollout is permanent and is not a trip), or
#   - a node that has answered with the token now rejects it (401/403: the install lost the
#     observer drop-in or the token, which takes the dashboard's view of that node away).
# A node that does not answer at all is not a trip: the node being installed restarts.
#
#   deploy/watch.sh --once --expect VERSION              # two looks; exit 3 and the reason on a trip
#   deploy/watch.sh --pid PID --expect VERSION --trip FILE [--state DIR]
#                                                        # until PID ends; a trip writes FILE
#
# --state DIR holds what the watch knows (the baseline count per node in base.<node>, the nodes
# that accepted the token in acc.<node>); rollout.sh shares it so its own baselines are the
# watcher's. Without it a loop records the baseline at its first look and --once uses 0.
# AIVOICEBUS_WATCH_INTERVAL sets the seconds between looks (default 3). AIVOICEBUS_ALLOW_QUARANTINED=1
# ignores quarantined records. Written for bash 3.2 (the macOS /bin/bash): no associative arrays;
# scripts/lint.sh checks every deploy script for the constructs that need bash 4.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2
. deploy/nodes.sh
PORT=7777
. deploy/ops.sh

usage() { echo "usage: $0 --once --expect VERSION | --pid PID --expect VERSION --trip FILE [--state DIR]" >&2; exit 2; }
ONCE=0; RPID=""; EXPECT=""; TRIP=""; STATE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --once) ONCE=1 ;;
    --pid) RPID="${2:?}"; shift ;;
    --expect) EXPECT="${2:?}"; shift ;;
    --trip) TRIP="${2:?}"; shift ;;
    --state) STATE="${2:?}"; shift ;;
    *) usage ;;
  esac
  shift
done
[ -n "$EXPECT" ] || usage
[ "$ONCE" = 1 ] || { [ -n "$RPID" ] && [ -n "$TRIP" ]; } || usage
OBS="$HOME/.config/aivoicebus/observer-token"
[ -s "$OBS" ] || OBS="${AIVOICEBUS_TOKEN_FILE:-$HOME/.config/aivoicebus/token}"
[ -s "$OBS" ] || { echo "watch: no token to read the nodes with" >&2; exit 2; }
if [ -z "$STATE" ]; then STATE=$(mktemp -d); trap 'rm -rf "$STATE"' EXIT; fi

say() { echo "   [watch] $*"; }
trip() {
  say "TRIP: $1"
  [ -z "$TRIP" ] || printf '%s\n' "$1" >"$TRIP"
  exit 3
}
key() { printf '%s' "$1" | tr -c 'A-Za-z0-9_.-' _; }
# look: one pass over every node.
look() {
  local n name ip h code v q b k
  for n in "${NODES[@]}"; do IFS=: read -r name _ ip <<<"$n"
    k=$(key "$name")
    code=$(http_code "$OBS" "http://$ip:$PORT/v1/health")
    case "$code" in
      200)
        : >"$STATE/acc.$k"   # every pass: a node that was down, or refused at first, is tracked once it answers
        h=$(http_get "$OBS" "http://$ip:$PORT/v1/health") || continue
        v=$(printf '%s' "$h" | grep -o '"version":"[^"]*"' | head -1 | cut -d'"' -f4)
        q=$(quarantined_of "$h")
        if [ ! -e "$STATE/base.$k" ] && [ "$ONCE" = 0 ]; then printf '%s\n' "$q" >"$STATE/base.$k"; fi
        b=$(cat "$STATE/base.$k" 2>/dev/null || echo 0)
        [ "$(cat "$STATE/ver.$k" 2>/dev/null)" = "$v" ] || { [ -e "$STATE/ver.$k" ] && say "$name now reports release ${v:-?}"; printf '%s\n' "$v" >"$STATE/ver.$k"; }
        if [ "$v" = "$EXPECT" ] && [ "$q" -gt "$b" ] && [ "${AIVOICEBUS_ALLOW_QUARANTINED:-}" != 1 ]; then
          trip "$name (on $EXPECT) reports quarantined=$q, $b when the watch began: look at its log before going on"
        fi ;;
      401 | 403)
        [ ! -e "$STATE/acc.$k" ] || trip "$name accepted the watch token and now rejects it (HTTP $code): the observer setup or the token may have been lost" ;;
    esac
  done
}

look
if [ "$ONCE" = 1 ]; then look; say "nothing wrong"; exit 0; fi
while kill -0 "$RPID" 2>/dev/null; do
  sleep "${AIVOICEBUS_WATCH_INTERVAL:-3}"
  look
done
