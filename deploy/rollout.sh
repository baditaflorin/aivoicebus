#!/usr/bin/env bash
# Rolls a new aivoicebus release across the nodes one at a time, canary first, and
# undoes a node that does not come up properly. DRY RUN by default.
#
#   deploy/rollout.sh dist/release                    # plan only
#   deploy/rollout.sh dist/release --apply            # canary node, soak, then the rest
#   deploy/rollout.sh dist/release --apply --canary node-b --soak 60
#
# Run it from a checkout of the RELEASE commit (git worktree add --detach <dir> <commit>), not
# from main: the deploy scripts on main may have moved on since the release was cut.
#
# The argument is a signed release directory (scripts/release.sh build+sign). Before anything
# is touched it is verified with `aivoicebus release verify` (signature against the key compiled
# into your aivoicebus, and every file against its signed hash), and the linux binary and the
# version come from it, so the nodes run exactly what was released. --unsigned skips that check,
# for a build you made yourself (then pass --expect VERSION; the lab does this).
#
# Every node must be reachable before anything starts. For each node, in order (the canary
# first, then nodes.conf order); a node already on the release is skipped:
#   1. deploy/install.sh --apply <node>   (keeps the old binary and unit as .prev)
#   2. verify: /v1/health reports the new version and every peer link is ok
#   3. the canary only: soak (default 30s), then verify again and check the service did
#      not restart since the install. A crash loop shows /healthz green for a few seconds at
#      a time, which one check misses and a soak catches. A canary already on the release (a
#      rollout that was interrupted and is run again) is soaked all the same before any
#      other node is touched.
# A node that fails any step is rolled back (the previous binary and unit, restarted, health
# checked) if its binary was replaced, and the rollout STOPS: the nodes after it are never
# touched, and the exit status is 1. Verifying includes "no quarantined records".
#
# With --apply it holds the release lock (deploy/release-lock.sh) for the whole run and refuses
# to start while another session holds it (--force-lock takes it over), and it runs
# deploy/watch.sh beside it: when a node on the release reports quarantined records, or a node
# that accepted the observer token starts rejecting it, the rollout stops at its next step and
# rolls back the node it is installing.
set -euo pipefail
cd "$(dirname "$0")/.."
. deploy/nodes.sh

PORT="${AIVOICEBUS_PORT:-7777}"   # AIVOICEBUS_PORT: a node port other than 7777 (one already taken on the hosts)
. deploy/ops.sh
TOKEN_FILE="${AIVOICEBUS_TOKEN_FILE:-$HOME/.config/aivoicebus/token}"
usage() { echo "usage: $0 <release dir> [--apply] [--canary NODE] [--soak SECONDS] [--expect VERSION] [--unsigned] [--force-lock]" >&2; exit 2; }
[ $# -ge 1 ] || usage
SRC="$1"; shift
APPLY=0; CANARY=""; SOAK=30; EXPECT=""; UNSIGNED=0; FORCE_LOCK=""
while [ $# -gt 0 ]; do
  case "$1" in
    --apply) APPLY=1 ;;
    --canary) CANARY="${2:?--canary needs a node name}"; shift ;;
    --soak) SOAK="${2:?--soak needs seconds}"; shift ;;
    --expect) EXPECT="${2:?--expect needs a version}"; shift ;;
    --unsigned) UNSIGNED=1 ;;
    --force-lock) FORCE_LOCK=1 ;;
    *) usage ;;
  esac
  shift
done
# A whole number of seconds, at least 1: "30s" or "" must not quietly skip the soak.
[[ "$SOAK" =~ ^[0-9]+$ ]] && [ "$SOAK" -ge 1 ] || { echo "--soak takes a whole number of seconds, at least 1 (got '$SOAK')" >&2; usage; }

[ -d "$SRC" ] || { echo "$SRC is not a release directory" >&2; exit 1; }
[ -s "$SRC/manifest.json" ] && [ -s "$SRC/manifest.sig" ] || { echo "$SRC has no signed manifest (run: scripts/release.sh sign $SRC)" >&2; exit 1; }
[ -x "$SRC/aivoicebus-linux-amd64" ] || { echo "no aivoicebus-linux-amd64 in $SRC" >&2; exit 1; }
if [ "$UNSIGNED" = 0 ]; then
  FM="${AIVOICEBUS_VERIFY_BIN:-$(command -v aivoicebus || true)}"
  [ -n "$FM" ] || { echo "no aivoicebus here to verify the release with (AIVOICEBUS_VERIFY_BIN=/path, or --unsigned for your own build)" >&2; exit 1; }
  "$FM" release verify --dir "$SRC" || { echo "the release in $SRC does not verify; nothing was touched" >&2; exit 1; }
fi
export AIVOICEBUS_BIN="$SRC/aivoicebus-linux-amd64"
[ -n "$EXPECT" ] || EXPECT=$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' "$SRC/manifest.json" | head -1)
[ -n "$EXPECT" ] || { echo "cannot tell which version to expect; pass --expect VERSION" >&2; exit 1; }
[ -s "$TOKEN_FILE" ] || { echo "no token at $TOKEN_FILE" >&2; exit 1; }

# Order: the canary first, the others as listed.
ORDER=()
for n in "${NODES[@]}"; do IFS=: read -r name _ _ <<<"$n"; [ "$name" = "$CANARY" ] && ORDER+=("$n"); done
[ -z "$CANARY" ] || [ ${#ORDER[@]} -gt 0 ] || { echo "no node named $CANARY in nodes.conf" >&2; exit 1; }
for n in "${NODES[@]}"; do IFS=: read -r name _ _ <<<"$n"; [ "$name" = "$CANARY" ] || ORDER+=("$n"); done

echo "rollout of release $EXPECT to ${#ORDER[@]} node(s): $(for n in "${ORDER[@]}"; do printf '%s ' "${n%%:*}"; done)"
if [ "$APPLY" = 0 ]; then
  for i in "${!ORDER[@]}"; do
    echo "  $((i + 1)). ${ORDER[$i]%%:*}: install, verify version $EXPECT and peer links$([ "$i" = 0 ] && echo ", soak ${SOAK}s and check for restarts"); roll back and stop on any failure"
  done
  echo "dry run only (re-run with --apply)"; exit 0
fi

# The release lock and the watcher, both undone however this run ends.
LOCK_NAME=$(printf '%s@%s:%s' "$(id -un 2>/dev/null)" "$(hostname 2>/dev/null)" "$$" | tr -cd 'A-Za-z0-9._@:-' | tail -c 80)   # who, where, which run
LOCKED=0; WPID=""; WSTATE=$(mktemp -d); HB_LAST=$(date +%s)
cleanup() {
  [ -z "$WPID" ] || kill "$WPID" 2>/dev/null || true   # it has ended by itself after a trip
  [ "$LOCKED" = 0 ] || deploy/release-lock.sh release "$LOCK_NAME" >/dev/null 2>&1 || echo "could not give the release lock back: deploy/release-lock.sh release $LOCK_NAME" >&2
  rm -rf "$WSTATE"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
rc=0; deploy/release-lock.sh take "$LOCK_NAME" "rollout $EXPECT" ${FORCE_LOCK:+--force} || rc=$?
case "$rc" in
  0) LOCKED=1 ;;
  6) ;;   # --force-lock with a lock node that cannot keep a lock: gone on without one, loudly
  *) echo "rollout not started; no node was touched" >&2; exit 1 ;;
esac
all_reachable || exit 1

# What each node held in quarantined records before this run: a placeholder is permanent, so what
# stops a rollout is an INCREASE (the watcher reads the same files). Taken again just before a
# node's own install.
key() { printf '%s' "$1" | tr -c 'A-Za-z0-9_.-' _; }
snap_base() { # snap_base <name> <ip>
  local h
  h=$(http_get "$TOKEN_FILE" "http://$2:$PORT/v1/health") || return 0
  quarantined_of "$h" >"$WSTATE/base.$(key "$1")"
}
for n in "${ORDER[@]}"; do IFS=: read -r name _ ip <<<"$n"; snap_base "$name" "$ip"; done
deploy/watch.sh --pid $$ --expect "$EXPECT" --trip "$WSTATE/trip" --state "$WSTATE" &
WPID=$!

remote() { ssh -n -o BatchMode=yes "$1" "$2"; }
restarts() { remote "$1" 'systemctl show aivoicebus -p NRestarts --value 2>/dev/null || echo 0' 2>/dev/null | tr -dc '0-9'; }
binsum() { remote "$1" 'sha256sum /usr/local/bin/aivoicebus 2>/dev/null | cut -d" " -f1' 2>/dev/null | tr -dc 'a-f0-9'; }
version_now() { http_get "$TOKEN_FILE" "http://$1:$PORT/v1/health" | grep -o '"version":"[^"]*"' | head -1 | cut -d'"' -f4; }

# q_ok <name> <ip>: the node holds no quarantined record it did not hold before.
q_ok() {
  [ "${AIVOICEBUS_ALLOW_QUARANTINED:-}" != 1 ] || return 0
  local h q b
  h=$(http_get "$TOKEN_FILE" "http://$2:$PORT/v1/health") || return 0
  q=$(quarantined_of "$h"); b=$(cat "$WSTATE/base.$(key "$1")" 2>/dev/null || echo 0)
  [ "$q" -le "$b" ] || { echo "   $1 now holds $q quarantined record(s), $b before: a peer offered a record no client writes (AIVOICEBUS_ALLOW_QUARANTINED=1 once it is understood)" >&2; return 1; }
}

# rollback <name> <alias> <ip> <sum-before> [force]: put the previous binary and unit back, but only
# if this rollout replaced the binary (otherwise there is nothing of ours to undo, and an older
# aivoicebus.prev must not downgrade a node that was fine). force is for a canary that was ALREADY on
# the release when this run started (an interrupted rollout run again): its binary is not replaced
# by this run, yet it is the one that just failed, so it goes back to aivoicebus.prev, and if there is
# no different previous binary the rollout says so loudly instead of leaving it quietly on the bad one.
rollback() {
  local name="$1" alias="$2" ip="$3" before="$4" force="${5:-0}" now prev
  now=$(binsum "$alias" || true)
  if [ "$force" = 1 ]; then
    prev=$(remote "$alias" 'sha256sum /usr/local/bin/aivoicebus.prev 2>/dev/null | cut -d" " -f1' 2>/dev/null | tr -dc 'a-f0-9' || true)
    if [ -z "$prev" ] || [ "$prev" = "$now" ]; then
      echo "   CANNOT ROLL BACK $name: it already ran release $EXPECT when this run began and has no different previous binary (aivoicebus.prev is $([ -z "$prev" ] && echo missing || echo identical)); it is STILL ON release $EXPECT: put a good binary on it by hand" >&2
      return 1
    fi
  elif [ -z "$now" ] || [ "$now" = "$before" ]; then
    echo "   $name still runs the binary it had before; nothing to roll back (look at why it failed)"; return 0
  fi
  echo "   ROLLING BACK $name to the previous binary"
  if remote "$alias" 'test -x /usr/local/bin/aivoicebus.prev &&
      install -m 755 /usr/local/bin/aivoicebus.prev /usr/local/bin/aivoicebus &&
      { [ ! -e /etc/systemd/system/aivoicebus.service.prev ] || { install -m 644 /etc/systemd/system/aivoicebus.service.prev /etc/systemd/system/aivoicebus.service && systemctl daemon-reload; }; } &&
      systemctl restart aivoicebus &&
      for i in $(seq 1 40); do curl -fsS http://'"$ip"':'"$PORT"'/healthz >/dev/null 2>&1 && exit 0; sleep 1; done; exit 1'; then
    echo "   rolled back and healthy"
  else
    echo "   ROLLBACK FAILED on $name: log in and look (journalctl -u aivoicebus); aivoicebus.prev may be missing" >&2; return 1
  fi
}

# The node being worked on (the previous one, until the next starts), for rollback_cur.
CUR_N=""; CUR_A=""; CUR_I=""; CUR_S=""; CUR_F=0
rollback_cur() { [ -z "$CUR_N" ] || rollback "$CUR_N" "$CUR_A" "$CUR_I" "$CUR_S" "$CUR_F" || true; }
stop_at() { # stop_at <why>: undo the node in hand and end the run
  echo "   $1" >&2; rollback_cur
  echo "rollout stopped${CUR_N:+ at $CUR_N}; later nodes were not touched" >&2; exit 1
}

# hb: tell the lock we are alive (every AIVOICEBUS_LOCK_HEARTBEAT seconds, default 60), so a rollout
# longer than the stale limit keeps its lock; if the lock was taken from us, stop.
hb() {
  local now rc=0
  [ "$LOCKED" = 1 ] || return 0
  now=$(date +%s)
  [ $((now - HB_LAST)) -ge "${AIVOICEBUS_LOCK_HEARTBEAT:-60}" ] || return 0
  HB_LAST=$now
  deploy/release-lock.sh refresh "$LOCK_NAME" >/dev/null 2>&1 || rc=$?
  [ "$rc" != 3 ] || printf 'the release lock was taken over by another session\n' >"$WSTATE/trip"
}

# watched: called between steps. Stops if the watcher tripped, or died (a rollout nobody watches is
# not one the watcher vouches for).
watched() {
  hb
  if [ -s "$WSTATE/trip" ]; then stop_at "the watcher stopped the rollout: $(cat "$WSTATE/trip")"; fi
  kill -0 "$WPID" 2>/dev/null || stop_at "the watcher process died, so the rest of the rollout would run unwatched; stopping"
}

first=1
for n in "${ORDER[@]}"; do IFS=: read -r name alias ip <<<"$n"
  watched   # a trip now is about the node before this one: it is the one rolled back
  CUR_N="$name"; CUR_A="$alias"; CUR_I="$ip"; CUR_F=0
  echo; echo "== $name"
  CUR_S=$(binsum "$alias" || true)
  snap_base "$name" "$ip"
  if [ "$(version_now "$ip" || true)" = "$EXPECT" ] && verify_node "$ip" "$TOKEN_FILE" "$EXPECT" 2>/dev/null; then
    echo "   already on $EXPECT with its peers connected: not installed again"
    # A canary already on the release is an interrupted rollout run again: it is soaked all the
    # same, or a crash-looping release would pass as "already done" and reach every other node.
    [ "$first" = 1 ] || continue
    CUR_F=1
  else
    if ! deploy/install.sh --apply "$name"; then stop_at "install failed on $name"; fi
    watched
    if ! verify_node "$ip" "$TOKEN_FILE" "$EXPECT"; then stop_at "verification failed on $name"; fi
    if ! q_ok "$name" "$ip"; then stop_at "$name is not to be trusted with the release"; fi
    echo "   verified: release $EXPECT, peer links ok"
  fi
  watched
  if [ "$first" = 1 ]; then
    baseline=$(restarts "$alias" || echo 0)   # after the install: a restart resets the counter
    echo "   soaking ${SOAK}s on the canary..."
    for _ in $(seq 1 "$SOAK"); do sleep 1; watched; done
    after=$(restarts "$alias" || echo 0)
    if ! verify_node "$ip" "$TOKEN_FILE" "$EXPECT" || ! q_ok "$name" "$ip" || [ "${after:-0}" -gt "${baseline:-0}" ]; then
      echo "   the canary restarted ${baseline:-0} -> ${after:-0} time(s) or went unhealthy during the soak" >&2
      rollback_cur; echo "rollout stopped at the canary; no other node was touched" >&2; exit 1
    fi
    echo "   canary stable"
    CUR_F=0
  fi
  first=0
done
watched
# One last look of the watcher's own, so a trip just after the last node is not missed.
if ! last=$(deploy/watch.sh --once --expect "$EXPECT" --state "$WSTATE" 2>&1); then
  stop_at "the watcher stopped the rollout after the last node: ${last##*TRIP: }"
fi
echo; echo "done: every node runs release $EXPECT. Previous binaries are kept as aivoicebus.prev (roll one back by hand if needed)."
