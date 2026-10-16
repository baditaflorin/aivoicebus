#!/usr/bin/env bash
# One release operation at a time across sessions: a lock file on the FIRST node of nodes.conf.
# rollout.sh and publish-release.sh take it with --apply and give it back when they end; two
# sessions (people or agents) that start at once see each other instead of interleaving.
# install.sh, rotate-token.sh and machine-token.sh do NOT take it: they are short, and a token
# change must stay possible (a leak response) while a rollout is running.
#
#   deploy/release-lock.sh status                       # free, or who holds it, since when, for what
#   deploy/release-lock.sh take NAME [WHAT] [--force]   # exit 3 when someone else holds it
#   deploy/release-lock.sh refresh NAME                 # the holder says it is alive (rollout does, every minute)
#   deploy/release-lock.sh release NAME [--force]       # only the holder gives it back (--force: anyone)
#
# Exit status: 0 done, 3 held by someone else (or, for refresh, no longer ours), 4 the node cannot
# keep a lock (unreachable, or no /etc/aivoicebus), 5 another take was in progress, 2 usage, and 6 for
# `take --force` on a node that cannot keep a lock: the caller goes on WITHOUT a lock (the warning
# says so); without --force that is a refusal.
#
# The lock is /etc/aivoicebus/release.lock (holder, when, what, from), created with noclobber under
# flock on the node, so two takers cannot both win. `when` is renewed by `refresh`; a lock not
# renewed for AIVOICEBUS_LOCK_STALE seconds (default 7200, at least 60) is stale: a crashed run left
# it, and the next take replaces it and says so. A run killed with -9 cannot give its lock back;
# `status` shows it until then.
set -euo pipefail
cd "$(dirname "$0")/.." || exit 2
. deploy/nodes.sh

usage() { echo "usage: $0 status | take NAME [WHAT] [--force] | refresh NAME | release NAME [--force]" >&2; exit 2; }
[ $# -ge 1 ] || usage
CMD="$1"; shift
FORCE=0; ARGS=()
for a in "$@"; do case "$a" in --force) FORCE=1 ;; -*) usage ;; *) ARGS+=("$a") ;; esac; done
NAME="${ARGS[0]:-}"; WHAT="${ARGS[1]:-}"
STALE="${AIVOICEBUS_LOCK_STALE:-7200}"
case "$STALE" in '' | *[!0-9]*) echo "AIVOICEBUS_LOCK_STALE must be a number of seconds" >&2; exit 2 ;; esac
# A tiny value would make every lock stale at once, which is no lock at all.
[ "$STALE" -ge 60 ] || { echo "AIVOICEBUS_LOCK_STALE must be at least 60 seconds (got $STALE): a smaller value would switch the lock off" >&2; exit 2; }
# The values travel inside a remote shell command, so they are held to plain characters.
name_re='^[A-Za-z0-9._@:+-]{1,80}$' what_re='^[A-Za-z0-9._@:+/ -]{0,120}$'
case "$CMD" in
  take | release | refresh)
    [[ "$NAME" =~ $name_re ]] || { echo "the lock name must be 1-80 of A-Z a-z 0-9 . _ @ : + -" >&2; exit 2; } ;;
  status) ;;
  *) usage ;;
esac
[[ "$WHAT" =~ $what_re ]] || { echo "WHAT must be up to 120 of A-Z a-z 0-9 . _ @ : + / - and spaces" >&2; exit 2; }
FROM=$(hostname 2>/dev/null | tr -cd 'A-Za-z0-9._-' | head -c 60); FROM="${FROM:-unknown}"

[ ${#NODES[@]} -gt 0 ] || { echo "no nodes in nodes.conf" >&2; exit 2; }
IFS=: read -r LNODE LALIAS _ <<<"${NODES[0]}"

# The remote part: serialised by flock where the node has it (every Linux with util-linux); the
# noclobber create is what makes the lock exclusive even without it.
remote="set -u; L=/etc/aivoicebus/release.lock; umask 077
[ -d /etc/aivoicebus ] || { echo NODIR; exit 7; }
if command -v flock >/dev/null 2>&1; then exec 8>>/etc/aivoicebus/.release.lock.flock; flock -w 15 8 || { echo BUSY; exit 5; }; fi
now=\$(date +%s)
field() { sed -n \"s/^\$1=//p\" \"\$L\" 2>/dev/null | head -1; }
age() { w=\$(field when); case \"\$w\" in '' | *[!0-9]*) w=0 ;; esac; echo \$((now - w)); }
held() { echo HELD; cat \"\$L\"; echo \"age=\$(age)\"; }"
case "$CMD" in
  status) remote="$remote
[ -e \"\$L\" ] && held || echo FREE" ;;
  take) remote="$remote
if [ -e \"\$L\" ]; then
  if [ \"\$(field holder)\" = '$NAME' ]; then rm -f \"\$L\"
  elif [ $FORCE = 1 ]; then echo FORCED; held; rm -f \"\$L\"
  elif [ \"\$(age)\" -ge $STALE ]; then echo STALE; held; rm -f \"\$L\"
  else held; exit 3; fi
fi
set -C
printf 'holder=%s\nwhen=%s\nwhat=%s\nfrom=%s\n' '$NAME' \"\$now\" '$WHAT' '$FROM' >\"\$L\" || { echo LOST; exit 5; }
echo TAKEN" ;;
  refresh) remote="$remote
[ -e \"\$L\" ] || { echo FREE; exit 3; }
[ \"\$(field holder)\" = '$NAME' ] || { held; exit 3; }
sed \"s/^when=.*/when=\$now/\" \"\$L\" >\"\$L.new\" && mv \"\$L.new\" \"\$L\" && echo REFRESHED" ;;
  release) remote="$remote
[ -e \"\$L\" ] || { echo FREE; exit 0; }
if [ \"\$(field holder)\" = '$NAME' ] || [ $FORCE = 1 ]; then rm -f \"\$L\"; echo RELEASED; else held; exit 3; fi" ;;
esac

rc=0
out=$(ssh -n -o ConnectTimeout=10 -o BatchMode=yes "$LALIAS" "$remote" 2>/dev/null) || rc=$?
val() { printf '%s\n' "$out" | sed -n "s/^$1=//p" | tail -1; }
describe() { # who holds it, from the HELD block of the answer
  local age; age=$(val age)
  echo "held by $(val holder) (from $(val from)) for '$(val what)', taken or renewed $(( ${age:-0} / 60 )) min ago$([ "${age:-0}" -ge "$STALE" ] && echo ', STALE')"
}
case "$rc" in
  0 | 3) ;;
  5) echo "the release lock on $LNODE could not be taken or read (another take is in progress?); try again" >&2; exit 5 ;;
  *)
    if [ "$rc" = 7 ]; then
      echo "$LNODE ($LALIAS) has no /etc/aivoicebus, so no release lock can be kept there (is aivoicebus installed on it? the lock lives on the first node of nodes.conf)" >&2
    else
      echo "cannot reach $LNODE ($LALIAS), where the release lock lives, so no lock can be taken or read" >&2
    fi
    if [ "$CMD" = take ] && [ "$FORCE" = 1 ]; then
      echo "WARNING: going on WITHOUT the release lock (--force): another session could be releasing at the same time" >&2
      exit 6
    fi
    [ "$CMD" != take ] || echo "(--force goes on without a lock when the lock node cannot keep one; nothing was changed)" >&2
    exit 4 ;;
esac
case "$CMD" in
  status)
    if printf '%s\n' "$out" | grep -qx HELD; then echo "release lock: $(describe)"; exit 3; fi
    echo "release lock: free" ;;
  take)
    if [ "$rc" = 3 ]; then echo "release lock $(describe). Wait for it, or if that run is dead: $0 release $(val holder) --force" >&2; exit 3; fi
    printf '%s\n' "$out" | grep -qx STALE && echo "release lock: replaced a stale lock, $(describe)" >&2
    printf '%s\n' "$out" | grep -qx FORCED && echo "release lock: TAKEN OVER (--force), it was $(describe)" >&2
    echo "release lock taken by $NAME on $LNODE${WHAT:+ for \"$WHAT\"}" ;;
  refresh)
    if [ "$rc" = 3 ]; then
      if printf '%s\n' "$out" | grep -qx FREE; then echo "release lock: no lock is held any more, $NAME's is gone" >&2; else echo "release lock $(describe), no longer $NAME's" >&2; fi
      exit 3
    fi ;;
  release)
    if [ "$rc" = 3 ]; then echo "release lock $(describe), not by $NAME; not released (--force to remove it anyway)" >&2; exit 3; fi
    if printf '%s\n' "$out" | grep -qx RELEASED; then echo "release lock released by $NAME"; else echo "release lock was not held"; fi ;;
esac
