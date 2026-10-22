#!/usr/bin/env bash
# Takes a node out of the mesh end to end: the mirror of add-node.sh. DRY RUN by default.
#
#   deploy/remove-node.sh node-d                  # the plan, checked against the live mesh (read-only)
#   deploy/remove-node.sh node-d --apply          # out of nodes.conf and out of every other node's peer list
#   deploy/remove-node.sh node-d --stop           # later: may its service be stopped now?
#   deploy/remove-node.sh node-d --stop --apply   # disable and stop the service on it
#   deploy/remove-node.sh node-d --gone --apply   # a node that is dead already: out, without reading it
#
# What --apply does, in this order, stopping at the first thing that goes wrong:
#   1. refuses a removal that would leave fewer than 2 nodes, and the first node of nodes.conf (it
#      keeps the release lock: put another node's line first, while the lock is free)
#   2. takes the release lock and checks every remaining node answers ssh
#   3. waits (AIVOICEBUS_REMOVE_WAIT seconds, default 60) until every remaining node holds everything
#      the leaving one holds: origin by origin of the version vector in /v1/health (the numbers
#      add-node.sh sums: a sum could hide a node that is ahead on one origin and behind on another),
#      and every shared-space record it can serve (a log of its own, which /v1/health does not count)
#   4. writes deploy/nodes.conf without it; a "# removed" line in its place keeps its ssh alias and
#      address for --stop
#   5. refreshes the remaining nodes' peer lists, one at a time (install.sh --apply --peers-only)
#   6. verifies: every remaining node answers with every peer link up and none to the leaving node; a
#      probe (aivoicebus selftest: a send with the usual write quorum, read, ack, retract) goes through;
#      every remaining node then holds whatever any of them holds; `aivoicebus mesh` finds nothing wrong
# The leaving node keeps running, so a machine that still lists it keeps working, until --stop. Nothing
# pulls from it any more: a message, ack or retraction that reaches it now also reaches another node (the
# write quorum is 2), which is how the mesh survives losing any one node, and --apply and --stop count
# them; a shared file goes to one node only, so one put on it now makes --stop refuse.
#
# --stop refuses while the node is still in nodes.conf, still a peer of a remaining node, or named by the
# signed node list a remaining node serves; then it disables and stops the service. Its data stays on
# the host. --gone is for a node that no longer answers at all: its records cannot be compared, so
# anything only it held is lost; it refuses while the node answers.
# What it does NOT do, and prints as next steps: the signed node list (it needs the release key), the
# machines and dashboards whose AIVOICEBUS_URL names the node, other copies of nodes.conf, network rules.
set -euo pipefail
cd "$(dirname "$0")/.."
CONF_FILE="${AIVOICEBUS_NODES_CONF:-deploy/nodes.conf}"
export AIVOICEBUS_NODES_CONF="$CONF_FILE"
PORT="${AIVOICEBUS_PORT:-7777}"
. deploy/ops.sh
TOKEN_FILE="${AIVOICEBUS_TOKEN_FILE:-$HOME/.config/aivoicebus/token}"
WAIT="${AIVOICEBUS_REMOVE_WAIT:-60}"
FM="${AIVOICEBUS_VERIFY_BIN:-$(command -v aivoicebus || true)}" # the client that runs the probe and `mesh`

usage() { echo "usage: $0 <name> [--apply] [--stop | --gone]" >&2; exit 2; }
[ $# -ge 1 ] || usage
NAME="$1"; shift
APPLY=0; MODE=remove
for a in "$@"; do
  case "$a" in
    --apply) APPLY=1 ;;
    --stop) [ "$MODE" = remove ] || usage; MODE=stop ;;
    --gone) [ "$MODE" = remove ] || usage; MODE=gone ;;
    *) usage ;;
  esac
done
[[ "$NAME" =~ ^[a-z0-9][a-z0-9-]{0,30}$ ]] || { echo "node name '$NAME' must be lowercase letters, digits and -" >&2; exit 1; }
[[ "$WAIT" =~ ^[0-9]+$ ]] && [ "$WAIT" -ge 1 ] || { echo "AIVOICEBUS_REMOVE_WAIT is a whole number of seconds, at least 1 (got '$WAIT')" >&2; exit 2; }
[ -s "$TOKEN_FILE" ] || { echo "no token at $TOKEN_FILE" >&2; exit 1; }
. deploy/nodes.sh

LEAVING=""; REST=()
for n in "${NODES[@]}"; do
  if [ "${n%%:*}" = "$NAME" ]; then LEAVING="$n"; else REST+=("$n"); fi
done

health() { http_get "$TOKEN_FILE" "http://$1:$PORT/v1/health"; }
names() { local n; for n in "$@"; do printf '%s ' "${n%%:*}"; done; }
oneline() { tr '\n' ' ' <<<"$1" | sed 's/ *$//'; }
# summary <vector>: "3694 record(s) (database=4, docker=187, ...)"
summary() { awk 'NF == 2 { s += $2; o = o (o ? ", " : "") $1 "=" $2 } END { printf "%d record(s)%s", s, (o ? " (" o ")" : "") }' <<<"$1"; }
# removed_entry: name:alias:ip from the last "# removed" line nodes.conf has for NAME, if any.
removed_entry() { awk -v n="$NAME" '$1 == "#" && $2 == "removed" { split($4, f, ":"); if (f[1] == n) e = $4 } END { if (e != "") print e }' "$CONF_FILE"; }
rest_urls() { local n ip; for n in "${REST[@]}"; do IFS=: read -r _ _ ip <<<"$n"; printf 'http://%s:%s\n' "$ip" "$PORT"; done | paste -sd, -; }

# Shared spaces (aivoicebus share) are a log of their own, which /v1/health does not count, and a client
# puts a file on one node, not on a quorum: they are compared record by record.
# pairs_of <json>: the "origin seq" of every record in a shared-space answer.
pairs_of() { grep -o '"origin":"[^"]*","seq":[0-9]*' <<<"$1" | sed 's/^"origin":"\([^"]*\)","seq":\([0-9]*\)$/\1 \2/' || true; }
# shares_of <ip>: "thread origin seq" for each shared-space record the node can serve (a key, or a version
# whose chunks are all there): the threads from /v1/share/sync, what is complete from /v1/share/index.
# Nothing for a node without shared spaces; status 1 when it does not answer.
shares_of() {
  local vv="" enc page pairs threads="" t i=0
  while [ "$i" -lt 1000 ]; do
    enc=$(awk 'NF == 2 { printf "%s%%22%s%%22%%3A%s", (n++ ? "%2C" : ""), $1, $2 }' <<<"$vv")
    if ! page=$(http_get "$TOKEN_FILE" "http://$1:$PORT/v1/share/sync?vv=%7B$enc%7D"); then
      [ "$(http_code "$TOKEN_FILE" "http://$1:$PORT/v1/share/sync")" = 404 ] && return 0 # a release from before shared spaces
      return 1
    fi
    pairs=$(pairs_of "$page")
    [ -n "$pairs" ] || break
    vv=$(vector_max "$vv" "$pairs")
    threads="$threads $(grep -o '"thread":"[0-9a-f]*"' <<<"$page" | cut -d'"' -f4 | tr '\n' ' ')"
    i=$((i + 1))
  done
  for t in $(tr ' ' '\n' <<<"$threads" | sort -u); do
    page=$(http_get "$TOKEN_FILE" "http://$1:$PORT/v1/share/index?thread=$t") || return 1
    pairs_of "$page" | sed "s/^/$t /"
  done
}
# shares_missing <have> <want>: the lines of <want> (as shares_of prints them) that <have> lacks.
shares_missing() { printf '%s\n--\n%s\n' "$1" "$2" | awk '$0 == "--" { w = 1; next } NF == 3 { if (w) { if (!($0 in h)) print } else h[$0] = 1 }'; }
rest_shares() { local n ip; for n in "${REST[@]}"; do IFS=: read -r _ _ ip <<<"$n"; shares_of "$ip" || true; done | sort -u; }

# behind <vector> [<shares>]: one line per remaining node that does not hold all of them; nothing when every one does.
behind() {
  local n name ip j lack s miss
  for n in "${REST[@]}"; do
    IFS=: read -r name _ ip <<<"$n"
    if ! j=$(health "$ip"); then echo "$name does not answer"; continue; fi
    lack=$(awk 'NF == 2 { printf "%s%s from %s", (o++ ? ", " : ""), $2, $1 }' <<<"$(vector_missing "$(log_vector "$j")" "$1")")
    if [ -n "${2:-}" ]; then
      if s=$(shares_of "$ip"); then
        miss=$(shares_missing "$s" "$2" | grep -c . || true)
        [ "$miss" = 0 ] || lack="${lack:+$lack, }$miss shared-space record(s)"
      else
        lack="${lack:+$lack, }its shared spaces (they cannot be read)"
      fi
    fi
    [ -z "$lack" ] || echo "$name lacks $lack"
  done
  return 0
}
# wait_held <vector> [<shares>]: waits up to WAIT seconds until every remaining node holds them, and says who does not.
wait_held() {
  local left i=0
  while left=$(behind "$1" "${2:-}") && [ -n "$left" ] && [ "$i" -lt "$WAIT" ]; do sleep 1; i=$((i + 1)); done
  [ -z "$left" ] || { sed 's/^/   /' <<<"$left" >&2; return 1; }
}
# rest_top: what the remaining nodes hold between them (the per-origin maximum of their vectors).
rest_top() {
  local n ip j vs=()
  for n in "${REST[@]}"; do
    IFS=: read -r _ _ ip <<<"$n"
    j=$(health "$ip" || true); vs+=("$(log_vector "$j")")
  done
  vector_max "${vs[@]}"
}
# peers_of_leaving: the remaining nodes that still have the leaving node as a peer, or cannot say.
peers_of_leaving() {
  local n name ip j
  for n in "${REST[@]}"; do
    IFS=: read -r name _ ip <<<"$n"
    if ! j=$(health "$ip"); then echo "$name (does not answer)"; continue; fi
    ! grep -qF "\"url\":\"http://$IP:$PORT\"" <<<"$j" || echo "$name"
  done
  return 0
}
# lists_naming_leaving: the remaining nodes whose published signed node list still names the leaving
# node among its nodes, or that cannot say. A node that publishes no list (404) names nobody, and a list
# that names it under "retired" (it tells machines to stop using it) does not count.
lists_naming_leaving() {
  local n name ip code body listed
  for n in "${REST[@]}"; do
    IFS=: read -r name _ ip <<<"$n"
    code=$(http_code "$TOKEN_FILE" "http://$ip:$PORT/v1/release/nodes.json")
    case "$code" in
      404) ;;
      200)
        body=$(http_get "$TOKEN_FILE" "http://$ip:$PORT/v1/release/nodes.json" | tr -d '\n' || true)
        listed=$(grep -o '"nodes": *\[[^]]*\]' <<<"$body" || true) # its entries are flat objects
        if [ -z "$listed" ]; then echo "$name (its list could not be read)"
        elif grep -qF "//$IP:$PORT\"" <<<"$listed"; then echo "$name (serial $(grep -o '"serial": *[0-9]*' <<<"$body" | head -1 | tr -dc 0-9))"; fi ;;
      *) echo "$name (answered $code)" ;;
    esac
  done
  return 0
}
# report_extra <vector_missing output> <shares_missing output>: what the leaving node holds that no
# remaining node holds. Its own records are client writes that reached it after the others stopped
# pulling from it, each also written to another node; a record from any other origin it can only have
# pulled from a remaining node, and a shared file was put on it alone: either is a failure.
report_extra() {
  local own other
  if [ -n "$2" ]; then
    echo "$NAME holds $(grep -c . <<<"$2") shared-space record(s) that no remaining node holds, in thread(s) $(cut -d' ' -f1 <<<"$2" | sort -u | tr '\n' ' ')" >&2
    echo "A client put them on $NAME after the others stopped pulling from it, and a shared file goes to one node only. Have the writer put them again through another node (aivoicebus share put); then, or to give them up, stop it by hand: ssh $ALIAS 'systemctl disable aivoicebus; systemctl stop aivoicebus'" >&2
    return 1
  fi
  [ -n "$1" ] || { echo "   $NAME holds nothing the remaining nodes lack"; return 0; }
  own=$(awk -v n="$NAME" '$1 == n { print $2 }' <<<"$1")
  other=$(awk -v n="$NAME" '$1 != n { printf "%s%s from %s", (o++ ? ", " : ""), $2, $1 }' <<<"$1")
  [ -z "$other" ] || { echo "$NAME holds records no remaining node holds: $other. Look before going on: aivoicebus mesh" >&2; return 1; }
  echo "   note: $NAME took $own record(s) of its own after the others stopped pulling from it. They are client"
  echo "   writes, each also sent to another node (the write quorum is 2): lost only for a client that sets AIVOICEBUS_WRITE_QUORUM=1."
}

# ---- --stop: the second half, once machines no longer learn the node from the signed list
if [ "$MODE" = stop ]; then
  [ -z "$LEAVING" ] || { echo "$NAME is still in $CONF_FILE: take it out of the mesh first ($0 $NAME --apply)" >&2; exit 1; }
  entry=$(removed_entry)
  [ -n "$entry" ] || { echo "$CONF_FILE has no '# removed' line for $NAME (remove-node.sh --apply writes one): nothing to stop" >&2; exit 1; }
  IFS=: read -r _ ALIAS IP <<<"$entry"
  REST=("${NODES[@]}")
  echo "stopping aivoicebus on $NAME ($ALIAS, $IP:$PORT), out of the mesh of ${#REST[@]}: $(names "${REST[@]}")"
  bad=$(peers_of_leaving)
  [ -z "$bad" ] || { echo "$NAME is still a peer of: $(oneline "$bad"). Refresh their peer lists first: deploy/install.sh --apply --peers-only" >&2; exit 1; }
  echo "   no remaining node has it as a peer"
  bad=$(lists_naming_leaving)
  [ -z "$bad" ] || { echo "the signed node list served by $(oneline "$bad") still names $NAME. Publish one without it first: aivoicebus release nodes --conf $CONF_FILE --dir dist/nodes, then deploy/publish-nodes.sh dist/nodes --apply" >&2; exit 1; }
  echo "   no signed node list served by a remaining node names it"
  if j=$(health "$IP"); then
    held=$(log_vector "$j") # read before the others: they only grow, so what it pulled from them is in their vectors by then
    hs=$(shares_of "$IP" || true)
    report_extra "$(vector_missing "$(rest_top)" "$held")" "$(shares_missing "$(rest_shares)" "$hs")" || exit 1
  else
    echo "   $NAME does not answer on $IP:$PORT (stopped already?)"
  fi
  ssh_ok "$ALIAS" || { echo "cannot reach $ALIAS over ssh. Nothing was changed." >&2; exit 1; }
  if [ "$APPLY" = 0 ]; then
    echo "   would: systemctl disable aivoicebus, then stop it, on $ALIAS (its data stays on the host)"
    echo "dry run only (re-run with --apply)"; exit 0
  fi
  ssh -n "$ALIAS" 'systemctl disable aivoicebus >/dev/null 2>&1 || true; systemctl stop aivoicebus
    if systemctl is-active aivoicebus >/dev/null 2>&1; then echo "   the service is still active" >&2; exit 1; fi'
  if curl -fsS --max-time 3 "http://$IP:$PORT/healthz" >/dev/null 2>&1; then echo "$IP:$PORT still answers after the stop" >&2; exit 1; fi
  echo "   disabled and stopped: $IP:$PORT no longer answers"
  for n in "${REST[@]}"; do
    IFS=: read -r name _ ip <<<"$n"
    verify_node "$ip" "$TOKEN_FILE" || { echo "$name: not every peer link is up" >&2; exit 1; }
  done
  echo "   the ${#REST[@]} remaining node(s) answer with every peer link up"
  awk -v n="$NAME" -v s="stopped $(date -u +%Y-%m-%dT%H:%M:%SZ)" '$1 == "#" && $2 == "removed" && $5 == "" { split($4, f, ":"); if (f[1] == n) { print $0 " " s; next } } { print }' \
    "$CONF_FILE" >"$CONF_FILE.stop-$$" && mv "$CONF_FILE.stop-$$" "$CONF_FILE"
  echo "done. Its state directory (/var/lib/aivoicebus) is still on the host: it goes when the host is wiped."
  exit 0
fi

# ---- the removal (and --gone)
if [ -z "$LEAVING" ]; then
  if [ -n "$(removed_entry)" ]; then
    echo "$NAME is no longer in $CONF_FILE: it was removed. A run that stopped half way is finished with deploy/install.sh --apply --peers-only; its service is stopped with $0 $NAME --stop" >&2
  else
    echo "no node named $NAME in $CONF_FILE" >&2
  fi
  exit 1
fi
IFS=: read -r _ ALIAS IP <<<"$LEAVING"
[ "${#REST[@]}" -ge 2 ] || { echo "removing $NAME would leave ${#REST[@]} node(s): a mesh needs at least 2. Add one first (deploy/add-node.sh)." >&2; exit 1; }
[ "${NODES[0]%%:*}" != "$NAME" ] || { echo "$NAME is the first node of $CONF_FILE, which keeps the release lock (deploy/release-lock.sh). While the lock is free, put another node's line first, then run this again." >&2; exit 1; }

echo "removing node $NAME ($ALIAS, $IP:$PORT) from a mesh of ${#NODES[@]}: $(names "${NODES[@]}")"
echo "   leaves ${#REST[@]}: $(names "${REST[@]}")(the release lock stays on ${REST[0]%%:*})"

# precheck: what must hold before anything changes; sets WANT and SHARES, what the leaving node holds.
WANT=""; SHARES=""
precheck() {
  local n name alias bad="" code j left
  [ -n "$FM" ] && [ -x "$FM" ] || { echo "no aivoicebus client here to verify with: put one on PATH or set AIVOICEBUS_VERIFY_BIN" >&2; return 1; }
  for n in "${REST[@]}"; do
    IFS=: read -r name alias _ <<<"$n"
    ssh_ok "$alias" || bad="$bad $name"
  done
  [ -z "$bad" ] || { echo "cannot reach:$bad over ssh. Nothing was changed." >&2; return 1; }
  echo "   every remaining node answers ssh"
  code=$(http_code "$TOKEN_FILE" "http://$IP:$PORT/v1/health")
  if [ "$MODE" = gone ]; then
    [ "$code" = 000 ] || { echo "$NAME answers on $IP:$PORT (HTTP $code): it is not gone. Take it out the normal way, which first checks the others hold its records: $0 $NAME" >&2; return 1; }
    echo "   $NAME does not answer on $IP:$PORT: its records cannot be compared, and whatever only it held is lost"
    return 0
  fi
  [ "$code" = 200 ] && j=$(health "$IP") || { echo "$NAME does not answer /v1/health on $IP:$PORT (HTTP $code), so its records cannot be compared. If it is dead for good: $0 $NAME --gone" >&2; return 1; }
  WANT=$(log_vector "$j")
  SHARES=$(shares_of "$IP") || { echo "$NAME does not answer /v1/share/sync, so its shared spaces cannot be compared" >&2; return 1; }
  echo "   $NAME holds $(summary "$WANT"), and $(grep -c . <<<"$SHARES" || true) shared-space record(s)"
  [ "$APPLY" = 1 ] && return 0
  left=$(behind "$WANT" "$SHARES")
  if [ -z "$left" ]; then echo "   every remaining node holds all of it"; else sed 's/^/   not yet: /' <<<"$left"; echo "   (--apply waits up to ${WAIT}s for them)"; fi
}

if [ "$APPLY" = 0 ]; then
  echo "checks (read-only):"
  precheck || { echo "dry run: --apply would stop here" >&2; exit 1; }
  echo "   $(deploy/release-lock.sh status 2>&1 | head -1)"
  echo "plan (--apply):"
  echo "  1. take the release lock"
  if [ "$MODE" = gone ]; then echo "  2. no record check: $NAME does not answer"
  else echo "  2. wait until every remaining node holds everything $NAME holds (up to ${WAIT}s)"; fi
  echo "  3. write $CONF_FILE without $NAME; a '# removed' line keeps $ALIAS and $IP for --stop"
  echo "  4. refresh the peer lists of $(names "${REST[@]}")one at a time (each restarts once)"
  echo "  5. verify: peer links up and none to $NAME, a selftest probe, records in step, aivoicebus mesh"
  [ "$MODE" = gone ] || echo "  $NAME keeps running until: $0 $NAME --stop --apply (after the next steps it prints)"
  echo "dry run only (re-run with --apply)"; exit 0
fi

LOCK_NAME=$(printf '%s@%s:%s' "$(id -un 2>/dev/null)" "$(hostname 2>/dev/null)" "$$" | tr -cd 'A-Za-z0-9._@:-' | tail -c 80)
LOCKED=0; STAGED=""
cleanup() {
  [ -z "$STAGED" ] || rm -f "$STAGED"
  [ "$LOCKED" = 0 ] || deploy/release-lock.sh release "$LOCK_NAME" >/dev/null 2>&1 || echo "could not give the release lock back: deploy/release-lock.sh release $LOCK_NAME" >&2
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
rc=0; deploy/release-lock.sh take "$LOCK_NAME" "remove-node $NAME" || rc=$?
[ "$rc" = 0 ] || { echo "not started; nothing was changed" >&2; exit 1; }
LOCKED=1
precheck || exit 1
if [ "$MODE" = remove ]; then
  echo "waiting until every remaining node holds everything $NAME holds..."
  wait_held "$WANT" "$SHARES" || { echo "not every remaining node holds everything $NAME holds after ${WAIT}s. Nothing was changed (look: aivoicebus mesh)." >&2; exit 1; }
  echo "   they do"
fi

STAGED="$CONF_FILE.remove-$$"
mark="# removed $(date -u +%Y-%m-%dT%H:%M:%SZ) $LEAVING"
[ "$MODE" = remove ] || mark="$mark gone"
awk -v n="$NAME" -v mark="$mark" '/^#/ || NF == 0 { print; next } { split($0, f, ":") } f[1] == n { print mark; next } { print }' "$CONF_FILE" >"$STAGED"
[ "$(AIVOICEBUS_NODES_CONF="$STAGED" bash -c '. deploy/nodes.sh; echo "${#NODES[@]}"')" = "${#REST[@]}" ] || { echo "could not write the new node list. Nothing was changed." >&2; exit 1; }
mv "$STAGED" "$CONF_FILE"; STAGED=""
echo "   $CONF_FILE lists $(names "${REST[@]}")"

trap 'echo "stopped before the end: $NAME is out of $CONF_FILE, but not every remaining node may have a fresh peer list. Fix the problem, then finish with: deploy/install.sh --apply --peers-only" >&2' ERR
for n in "${REST[@]}"; do deploy/install.sh --apply --peers-only "${n%%:*}"; done
trap - ERR

echo; echo "verifying the ${#REST[@]} remaining node(s)..."
for n in "${REST[@]}"; do
  IFS=: read -r name _ ip <<<"$n"
  verify_node "$ip" "$TOKEN_FILE" || { echo "$name: not every peer link is up" >&2; exit 1; }
done
bad=$(peers_of_leaving)
[ -z "$bad" ] || { echo "still have $NAME as a peer: $(oneline "$bad")" >&2; exit 1; }
echo "   each answers with every peer link up, none of them to $NAME"
URLS=$(rest_urls)
probe=$(AIVOICEBUS_URL="$URLS" AIVOICEBUS_TOKEN_FILE="$TOKEN_FILE" AIVOICEBUS_TOKEN="" "$FM" selftest --force 2>&1) ||
  { sed 's/^/   /' <<<"$probe" >&2; echo "the probe through the remaining nodes did not go through" >&2; exit 1; }
echo "   probe: $(tail -1 <<<"$probe")"
top=$(rest_top)
wait_held "$top" || { echo "the remaining nodes do not hold the same records after ${WAIT}s: replication between them is not working" >&2; exit 1; }
echo "   every remaining node holds the $(summary "$top" | cut -d' ' -f1) record(s) any of them holds, the probe's included"
i=0
until out=$(AIVOICEBUS_URL="$URLS" AIVOICEBUS_TOKEN_FILE="$TOKEN_FILE" AIVOICEBUS_TOKEN="" "$FM" mesh 2>&1); do
  i=$((i + 1))
  [ "$i" -lt "$WAIT" ] || { sed 's/^/   /' <<<"$out" >&2; echo "$NAME is out, but aivoicebus mesh finds something wrong with the remaining nodes" >&2; exit 1; }
  sleep 1
done
echo "   aivoicebus mesh: nothing wrong"
if [ "$MODE" = remove ] && j=$(health "$IP"); then
  held=$(log_vector "$j") # read before the others, as in --stop
  hs=$(shares_of "$IP" || true)
  report_extra "$(vector_missing "$(rest_top)" "$held")" "$(shares_missing "$(rest_shares)" "$hs")" || exit 1
fi

echo; echo "$NAME is out of the mesh."
cat <<NEXT

Next, by you:
  0. $CONF_FILE is local: take $NAME's line out of every other copy (another checkout, another
     admin's machine). A copy that still lists it would put it back into the peer lists (install.sh),
     rollouts and token changes.
  1. The signed node list (it needs the release key):
       aivoicebus release nodes --conf $CONF_FILE --dir dist/nodes
       deploy/publish-nodes.sh dist/nodes --apply
  2. Machines, dashboards and exporters whose AIVOICEBUS_URL names http://$IP:$PORT: take it out (on a
     machine: ~/.config/aivoicebus/env) and restart them. Until then they keep trying $NAME, and once it
     stops their status counts it as down, unless they run a release that drops the nodes a signed list retires.
NEXT
if [ "$MODE" = remove ]; then
  echo "  3. Then stop it: $0 $NAME --stop (the checks), then $0 $NAME --stop --apply"
fi
echo "  4. Network: the rule that lets the other nodes and your clients reach $IP:$PORT can go."
