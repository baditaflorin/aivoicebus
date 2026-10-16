#!/usr/bin/env bash
# Adds a node to the mesh end to end. DRY RUN by default.
#
#   deploy/add-node.sh node-d node-d_bastion 100.64.0.9             # the plan
#   AIVOICEBUS_BIN=dist/release/aivoicebus-linux-amd64 deploy/add-node.sh node-d node-d_bastion 100.64.0.9 --apply
#
# What it does, in this order, stopping at the first thing that goes wrong:
#   1. checks the arguments and that the node is not already listed (deploy/nodes.conf)
#   2. adds the node to deploy/nodes.conf (the one list every script reads)
#   3. installs aivoicebus on it (deploy/install.sh --apply <node>); it starts as a peer of the others
#      and, being empty, recovers the board from them before it takes writes
#   4. refreshes the other nodes' peer lists, one at a time (install.sh --apply --peers-only)
#   5. waits until the new node holds everything the others hold
# What it does NOT do, because each is a decision or a security setting, and prints as next steps:
#   - the network rules for the node (firewall or VPN policy: the other nodes and your clients must reach its port)
#   - the dashboard's AIVOICEBUS_URL, and a re-signed node list for clients (aivoicebus release nodes + deploy/publish-nodes.sh)
set -euo pipefail
cd "$(dirname "$0")/.."
CONF_FILE="${AIVOICEBUS_NODES_CONF:-deploy/nodes.conf}"
export AIVOICEBUS_NODES_CONF="$CONF_FILE"
PORT=7777
. deploy/ops.sh
TOKEN_FILE="${AIVOICEBUS_TOKEN_FILE:-$HOME/.config/aivoicebus/token}"

usage() { echo "usage: $0 <name> <ssh-alias> <ip> [--apply]" >&2; exit 2; }
[ $# -ge 3 ] || usage
NAME="$1"; ALIAS="$2"; IP="$3"; shift 3
APPLY=0
for a in "$@"; do case "$a" in --apply) APPLY=1 ;; *) usage ;; esac; done

[[ "$NAME" =~ ^[a-z0-9][a-z0-9-]{0,30}$ ]] || { echo "node name '$NAME' must be lowercase letters, digits and -" >&2; exit 1; }
[[ "$ALIAS" =~ ^[A-Za-z0-9_.-]+$ ]] || { echo "ssh alias '$ALIAS' looks wrong" >&2; exit 1; }
[[ "$IP" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || { echo "ip '$IP' is not an IPv4 address" >&2; exit 1; }
. deploy/nodes.sh
for n in "${NODES[@]}"; do
  IFS=: read -r name alias ip <<<"$n"
  [ "$name" != "$NAME" ] || { echo "a node named $NAME is already in $CONF_FILE" >&2; exit 1; }
  [ "$ip" != "$IP" ] || { echo "$IP already belongs to node $name" >&2; exit 1; }
done

echo "adding node $NAME ($ALIAS, $IP:$PORT) to a mesh of ${#NODES[@]}: $(for n in "${NODES[@]}"; do printf '%s ' "${n%%:*}"; done)"
if [ "$APPLY" = 0 ]; then
  echo "  1. add '$NAME:$ALIAS:$IP' to $CONF_FILE"
  echo "  2. install aivoicebus on $NAME (needs AIVOICEBUS_BIN, the signed release's linux binary)"
  echo "  3. refresh the peer list of each existing node, one at a time"
  echo "  4. wait until $NAME holds everything the others hold"
  echo "dry run only (re-run with --apply)"; exit 0
fi

[ -n "${AIVOICEBUS_BIN:-}" ] && [ -x "$AIVOICEBUS_BIN" ] || { echo "set AIVOICEBUS_BIN to the signed release's linux binary (dist/release/aivoicebus-linux-amd64); this script does not build one on the machine you are on" >&2; exit 1; }
all_reachable || exit 1
ssh_ok "$ALIAS" || { echo "cannot reach the new node ($ALIAS) over ssh. Nothing was changed." >&2; exit 1; }
printf '%s:%s:%s\n' "$NAME" "$ALIAS" "$IP" >>"$CONF_FILE"
trap 'echo "stopped before the end; $NAME is listed in $CONF_FILE. Fix the problem and re-run: deploy/install.sh --apply $NAME" >&2' ERR
deploy/install.sh --apply "$NAME"
for n in "${NODES[@]}"; do IFS=: read -r name _ _ <<<"$n"
  deploy/install.sh --apply --peers-only "$name"
done

# Wait for the new node to catch up: its log must hold every origin's records the others hold.
health() { http_get "$TOKEN_FILE" "http://$1:$PORT/v1/health"; }
want=0
for n in "${NODES[@]}"; do IFS=: read -r _ _ ip <<<"$n"; j=$(health "$ip" || true); t=$(log_total "$j"); [ "$t" -le "$want" ] || want=$t; done
echo; echo "waiting for $NAME to hold $want record(s)..."
got=0
for _ in $(seq 1 60); do
  j=$(health "$IP" || true); got=$(log_total "$j")
  [ "$got" -ge "$want" ] && break
  sleep 1
done
[ "$got" -ge "$want" ] || { echo "$NAME holds $got of $want records after 60s; it is still catching up (check: aivoicebus mesh)" >&2; exit 1; }
trap - ERR
echo "$NAME holds $got record(s): in step with the others."
cat <<NEXT

Next, by you:
  0. Commit the new line in deploy/nodes.conf ($NAME:$ALIAS:$IP): every other script reads that file, and a
     checkout without it would leave $NAME out of token rotations and rollouts.
  1. Network: allow tcp:$PORT to $IP from the other nodes and from your clients (firewall or VPN policy).
  2. Dashboards and exporters: add http://$IP:$PORT to their AIVOICEBUS_URL and restart them.
  3. Clients: aivoicebus release nodes --dir dist/nodes, then deploy/publish-nodes.sh dist/nodes --apply.
NEXT
