#!/usr/bin/env bash
# Rotates the shared token across the mesh without a flag day. DRY RUN by default.
#
#   deploy/rotate-token.sh status            # which tokens does each node accept right now?
#   deploy/rotate-token.sh add [--apply]     # 1. nodes accept the old AND a new token
#   deploy/rotate-token.sh promote [--apply] # 2. nodes use the new token to talk to each other
#   deploy/rotate-token.sh retire [--apply]  # 3. nodes drop the old token; this machine switches
#   deploy/rotate-token.sh --check           # read-only: would a rotation now cut any machine off?
#
# --check asks the nodes which token each machine heard from in the last 7 days last used
# (`aivoicebus token report`, AIVOICEBUS_BIN names the binary) and exits 1 while any machine other
# than the nodes and this one is still on the shared token, or not proven either way. Such a
# machine needs its own token first: deploy/machine-token.sh add HOST --apply, then
# `aivoicebus token hand HOST`. It changes nothing, on the nodes or here.
#
# Between `add` and `retire`, move every machine to the new token (it waits at
# ~/.config/aivoicebus/token.next; a client bundle built then carries it, see
# deploy/make-client-bundle.sh). A machine still on the old token stops working the moment
# `retire` lands, so run `status` first and retire only when you are ready for that.
#
# Why three steps: a node authenticates to its peers with the FIRST token in its file.
# `add` writes "old, new" so nothing changes yet; `promote` writes "new, old", so every node
# speaks the new token while still accepting the old one from machines not yet moved; only
# then does `retire` write "new" alone. At every step every node accepts what every other
# node sends. Nodes are restarted one at a time and each must answer with its peer links up
# before the next is touched; the script stops at the first node that does not.
#
# Every node must be reachable before any step starts (a node left behind on the old token
# would be cut off at `retire`), and every node in the mesh must be in deploy/nodes.conf.
# Tokens are never printed or put on a command line: files and stdin only.
set -euo pipefail
cd "$(dirname "$0")/.."
. deploy/nodes.sh

PORT="${AIVOICEBUS_PORT:-7777}"
. deploy/ops.sh
CONF="${AIVOICEBUS_CONFIG_DIR:-$HOME/.config/aivoicebus}"
TOKEN_FILE="${AIVOICEBUS_TOKEN_FILE:-$CONF/token}"
NEXT_FILE="$TOKEN_FILE.next"
export AIVOICEBUS_NODE_TOKENS_FILE="${AIVOICEBUS_NODE_TOKENS_FILE:-$CONF/token.nodes}"
PAYLOAD="$AIVOICEBUS_NODE_TOKENS_FILE"

usage() { echo "usage: $0 status | add | promote | retire  [--apply]   |   $0 --check" >&2; exit 2; }
[ $# -ge 1 ] || usage
CMD="$1"; shift
APPLY=0
for a in "$@"; do case "$a" in --apply) APPLY=1 ;; *) usage ;; esac; done
[ "$CMD" != --check ] || [ "$APPLY" = 0 ] || usage # read-only: there is nothing to apply
[ -s "$TOKEN_FILE" ] || { echo "no token at $TOKEN_FILE" >&2; exit 1; }

# status prints one line per node: which of the two tokens it accepts, and sets the counts.
OLD_OK=0; NEW_OK=0; REACHED=0; DOWN=0
status() {
  OLD_OK=0; NEW_OK=0; REACHED=0; DOWN=0
  local n name ip o nw
  for n in "${NODES[@]}"; do IFS=: read -r name _ ip <<<"$n"
    o="rejects"; nw="-"
    if accepts "$TOKEN_FILE" "$ip"; then o="accepts"; OLD_OK=$((OLD_OK + 1)); REACHED=$((REACHED + 1))
    elif [ "$(http_code "$TOKEN_FILE" "http://$ip:$PORT/healthz")" = 000 ]; then o="UNREACHABLE"; DOWN=$((DOWN + 1))
    else REACHED=$((REACHED + 1)); fi
    if [ -s "$NEXT_FILE" ]; then
      if accepts "$NEXT_FILE" "$ip"; then nw="accepts"; NEW_OK=$((NEW_OK + 1)); else nw="rejects"; fi
    fi
    printf '  %-10s current token: %-11s next token: %s\n' "$name" "$o" "$nw"
  done
}

phase() { # which phase is deployed, judged from the local files
  if [ ! -s "$NEXT_FILE" ]; then echo none
  elif [ ! -s "$PAYLOAD" ] || [ "$PAYLOAD" = "$TOKEN_FILE" ]; then echo none
  elif [ "$(head -n1 "$PAYLOAD")" = "$(tr -d '\n' <"$TOKEN_FILE")" ]; then echo added
  else echo promoted; fi
}

# unlisted_nodes: nodes some node peers with that are not in deploy/nodes.conf. Rotation only
# reaches the listed ones, so an unlisted node would be left on the old token.
unlisted_nodes() {
  local n ip j u listed="" out=""
  for n in "${NODES[@]}"; do IFS=: read -r _ _ ip <<<"$n"; listed="$listed http://$ip:$PORT "; done
  for n in "${NODES[@]}"; do IFS=: read -r _ _ ip <<<"$n"
    j=$(http_get "$TOKEN_FILE" "http://$ip:$PORT/v1/health" || http_get "$NEXT_FILE" "http://$ip:$PORT/v1/health" || true)
    for u in $(printf '%s' "$j" | grep -o '"url":"[^"]*"' | cut -d'"' -f4); do
      case "$listed" in *" $u "*) ;; *) case "$out" in *" $u "*) ;; *) out="$out $u " ;; esac ;; esac
    done
  done
  printf '%s' "$out"
}

# roll_out runs install.sh --tokens-only on each node in turn, then checks the node with the
# first token of the file it just got (peer links up); stops at the first failure. Dry run
# lists the plan.
roll_out() {
  local n name ip first
  if [ "$APPLY" = 0 ]; then
    for n in "${NODES[@]}"; do IFS=: read -r name _ _ <<<"$n"; echo "  would restart $name with the new token file"; done
    return 0
  fi
  all_reachable || exit 1
  local odd; odd=$(unlisted_nodes)
  [ -z "$odd" ] || { echo "nodes the mesh knows that are not in deploy/nodes.conf:$odd. Add them first (their token file would be missed). Nothing was changed." >&2; exit 1; }
  first=$(mktemp)
  (umask 077; head -n1 "$PAYLOAD" >"$first")
  for n in "${NODES[@]}"; do IFS=: read -r name _ ip <<<"$n"
    if ! deploy/install.sh --apply --tokens-only "$name" || ! verify_node "$ip" "$first"; then
      rm -f "$first"
      echo "node $name did not come back healthy with its peers connected; stopping here. Nodes already done keep the new file; re-run this step after fixing $name." >&2
      exit 1
    fi
  done
  rm -f "$first"
}

write_payload() { (umask 077; printf '%s\n%s\n' "$1" "$2" >"$PAYLOAD"); }

case "$CMD" in
--check)
  BIN="${AIVOICEBUS_BIN:-aivoicebus}"
  command -v "$BIN" >/dev/null 2>&1 || { echo "no aivoicebus binary ($BIN); set AIVOICEBUS_BIN" >&2; exit 1; }
  names=""
  for n in "${NODES[@]}"; do IFS=: read -r name alias _ <<<"$n"; names="$names,$name,$alias"; done
  rc=0
  # The token goes by file, never on a command line; the nodes are the ones in deploy/nodes.conf.
  env -u AIVOICEBUS_TOKEN AIVOICEBUS_URL="$(node_urls)" AIVOICEBUS_TOKEN_FILE="$TOKEN_FILE" \
    "$BIN" token report --check --nodes "${names#,}" || rc=$?
  case "$rc" in
    0) echo "no machine would be cut off: a rotation may start ($0 add, a dry run first)" ;;
    1) echo "do not rotate the shared token now: the machines marked above would be cut off at retire. Nothing was changed." >&2; exit 1 ;;
    *) echo "could not tell (exit $rc); do not rotate until it can. Nothing was changed." >&2; exit "$rc" ;;
  esac
  ;;
status)
  echo "token rotation: phase $(phase)"
  status
  [ "$DOWN" = 0 ] || echo "  ($DOWN node(s) unreachable: no rotation step will start until they answer)"
  ;;
add)
  [ "$(phase)" != promoted ] || { echo "already past this step (phase promoted)"; exit 0; }
  if [ "$APPLY" = 0 ]; then
    [ -s "$NEXT_FILE" ] || echo "would generate a new token at $NEXT_FILE"
    echo "each node would get a token file: the current token, then the new one"; roll_out
    echo "dry run only (re-run with --apply)"; exit 0
  fi
  all_reachable || exit 1
  if [ ! -s "$NEXT_FILE" ]; then
    gen_token "$NEXT_FILE"
    echo "new token written to $NEXT_FILE (mode 600, not printed)"
  fi
  write_payload "$(tr -d '\n' <"$TOKEN_FILE")" "$(tr -d '\n' <"$NEXT_FILE")"
  roll_out
  echo; echo "added. Every node accepts both tokens. Move the machines to the new token, then: $0 promote --apply"
  ;;
promote)
  case "$(phase)" in added | promoted) ;; *) echo "nothing to promote (phase $(phase)); run '$0 add --apply' first" >&2; exit 1 ;; esac
  status
  [ "$DOWN" = 0 ] || { echo "$DOWN node(s) unreachable: they would be left on the old token. Nothing was changed." >&2; exit 1; }
  [ "$NEW_OK" -ge "$REACHED" ] && [ "$REACHED" -gt 0 ] || { echo "not every node accepts the new token yet; finish 'add' first" >&2; exit 1; }
  if [ "$APPLY" = 0 ]; then echo "each node would get: the new token first (used between nodes), then the old one"; roll_out; echo "dry run only (re-run with --apply)"; exit 0; fi
  write_payload "$(tr -d '\n' <"$NEXT_FILE")" "$(tr -d '\n' <"$TOKEN_FILE")"
  roll_out
  echo; echo "promoted. Nodes talk to each other with the new token and still accept the old one. When every machine has the new token: $0 retire --apply"
  ;;
retire)
  [ "$(phase)" = promoted ] || { echo "retire comes after promote (phase $(phase))" >&2; exit 1; }
  status
  [ "$DOWN" = 0 ] || { echo "$DOWN node(s) unreachable: they would keep accepting the old token. Nothing was changed." >&2; exit 1; }
  [ "$REACHED" -gt 0 ] && [ "$NEW_OK" -ge "$REACHED" ] || { echo "not every node accepts the new token; run promote first" >&2; exit 1; }
  if [ "$APPLY" = 0 ]; then echo "each node would drop the old token; machines still using it would stop working"; roll_out; echo "dry run only (re-run with --apply)"; exit 0; fi
  (umask 077; tr -d '\n' <"$NEXT_FILE" >"$PAYLOAD.new" && printf '\n' >>"$PAYLOAD.new")
  mv "$PAYLOAD.new" "$PAYLOAD"
  roll_out
  # Every node is on the new token alone: check that none still takes the old one before this
  # machine lets go of it. (Capture first: a grep -q on the end of a pipe can end it early.)
  after=$(status)
  case "$after" in
    *"current token: accepts"*) echo "a node still accepts the old token; keeping it on this machine:"; echo "$after"; exit 1 ;;
  esac
  (umask 077; tr -d '\n' <"$NEXT_FILE" >"$TOKEN_FILE.new")
  mv "$TOKEN_FILE.new" "$TOKEN_FILE"
  rm -f "$NEXT_FILE" "$PAYLOAD"
  echo; echo "retired. The old token is gone from every node and from this machine; $TOKEN_FILE is the new one."
  ;;
*) usage ;;
esac
