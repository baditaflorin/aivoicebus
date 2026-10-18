#!/usr/bin/env bash
# Gives one machine its own token, bound to that machine, and takes it away again.
# DRY RUN by default.
#
#   deploy/machine-token.sh list
#   deploy/machine-token.sh add <host> [--apply]      # new token for <host>; nodes restarted one at a time
#   deploy/machine-token.sh revoke <host> [--apply]   # that machine's token stops working everywhere
#
# <host> is the machine part of its identity (`aivoicebus whoami` shows host/agent; the host is
# what comes before the slash, e.g. laptop-a). The token may only send, acknowledge and
# read mail AS that host: with it nobody can speak as another machine, and losing the machine
# means revoking one token, not re-keying everyone. It can still read the replication feed and
# the roster, like any agent token; the observer token is the read-only one.
#
# The token is written to ~/.config/aivoicebus/machine-tokens/<host> (mode 600) and never printed.
# Hand it over with `aivoicebus token hand <host>`: it sends <host> a task, a session there runs
# `aivoicebus token install ID`, the person there approves, and the token goes over as a one-time,
# end-to-end-encrypted file; install checks it is <host>'s own before it replaces that machine's
# ~/.config/aivoicebus/token, and hand reports success only when the nodes saw <host> answer with
# it (docs/reference.md, "Machine tokens"). NEVER put it on the machine you deploy from, whose
# ~/.config/aivoicebus/token must stay the shared token. deploy/install.sh adds every file in that directory to each node's token file, so the
# tokens survive a re-install. Nodes use the shared token between themselves; a machine token
# cannot replicate, so keep at least one shared token (deploy/rotate-token.sh rotates it).
#
# Every node must be reachable before add or revoke starts: a revoke that skipped a node would
# say "revoked" while that node still accepts the token. Both are safe to re-run after a failure.
set -euo pipefail
cd "$(dirname "$0")/.."
. deploy/nodes.sh

PORT="${AIVOICEBUS_PORT:-7777}"
. deploy/ops.sh
DIR="${AIVOICEBUS_MACHINE_TOKENS_DIR:-$HOME/.config/aivoicebus/machine-tokens}"
export AIVOICEBUS_MACHINE_TOKENS_DIR="$DIR"
usage() { echo "usage: $0 list | add <host> [--apply] | revoke <host> [--apply]" >&2; exit 2; }
[ $# -ge 1 ] || usage
CMD="$1"; shift
HOST=""; APPLY=0
if [ "$CMD" != list ]; then
  [ $# -ge 1 ] || usage
  HOST="$1"; shift
  for a in "$@"; do case "$a" in --apply) APPLY=1 ;; *) usage ;; esac; done
  [[ "$HOST" =~ ^[a-z0-9][a-z0-9._-]{0,62}$ ]] || { echo "host '$HOST' must be lowercase letters, digits, . _ - (the part of an identity before the slash)" >&2; exit 1; }
fi

# roll_out gives every node the current token set, one at a time, and checks each answers
# with its peers connected. Returns 1 at the first node that does not (never exits, so a
# caller can record the failure).
roll_out() {
  local n name ip tf rc=0
  tf=$(mktemp); (umask 077; cat "${AIVOICEBUS_TOKEN_FILE:-$HOME/.config/aivoicebus/token}" >"$tf")
  for n in "${NODES[@]}"; do IFS=: read -r name _ ip <<<"$n"
    if ! deploy/install.sh --apply --tokens-only "$name" || ! verify_node "$ip" "$tf"; then
      echo "node $name did not come back healthy; stopping here. Run this command again once it is fixed." >&2
      rc=1; break
    fi
  done
  rm -f "$tf"
  return $rc
}

case "$CMD" in
list)
  if ls "$DIR"/* >/dev/null 2>&1; then ls "$DIR" | sed 's/^/  /'; else echo "  (no machine tokens)"; fi
  # shellcheck disable=SC2010 # only our own hidden marker files are matched, never arbitrary names
  ls -A "$DIR" 2>/dev/null | grep '^\.revoke-pending-' | sed 's/^\.revoke-pending-/  (revoke unfinished, run it again: /; s/$/)/' || true
  ;;
add)
  if [ "$APPLY" = 0 ]; then
    if [ -e "$DIR/$HOST" ]; then echo "$HOST already has a token; --apply would give it to every node again (resuming an interrupted add)"
    else echo "would create a token for $HOST at $DIR/$HOST and give it to every node (one restart each, one at a time)"; fi
    echo "dry run only (re-run with --apply)"; exit 0
  fi
  all_reachable || exit 1
  if [ -e "$DIR/$HOST" ]; then
    echo "$HOST already has a token; giving it to every node again"
  else
    mkdir -p "$DIR"; chmod 700 "$DIR"
    gen_token "$DIR/$HOST"
  fi
  roll_out || exit 1
  echo; echo "$HOST has its own token, at $DIR/$HOST (mode 600, not printed)."
  echo "Hand it to $HOST with: aivoicebus token hand $HOST   (a session there installs it; a person approves). It works only for identities $HOST/..."
  ;;
revoke)
  if [ "$APPLY" = 0 ]; then
    if [ -e "$DIR/$HOST" ] || [ -e "$DIR/.revoke-pending-$HOST" ]; then
      echo "would remove the token of $HOST from every node (one restart each, one at a time) and delete $DIR/$HOST"
    else echo "$HOST has no machine token" >&2; exit 1; fi
    echo "dry run only (re-run with --apply)"; exit 0
  fi
  all_reachable || exit 1
  if [ -e "$DIR/.revoke-pending-$HOST" ] && [ ! -e "$DIR/$HOST" ]; then mv "$DIR/.revoke-pending-$HOST" "$DIR/$HOST"; fi # finish an interrupted revoke
  [ -e "$DIR/$HOST" ] || { echo "$HOST has no machine token" >&2; exit 1; }
  mv "$DIR/$HOST" "$DIR/.revoke-pending-$HOST"   # hidden from install.sh's glob while the nodes are updated
  if roll_out; then
    rm -f "$DIR/.revoke-pending-$HOST"
    echo; echo "revoked: no node accepts $HOST's token any more."
  else
    echo "revoke did not finish: nodes after the failing one may still accept the token. Fix the failing node, then run this command again (the token is kept as $DIR/.revoke-pending-$HOST until then)." >&2
    exit 1
  fi
  ;;
*) usage ;;
esac
