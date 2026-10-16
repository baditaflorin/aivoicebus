#!/usr/bin/env bash
# Publishes a SIGNED release directory to the mesh nodes, so `aivoicebus update`
# can fetch it from them. DRY RUN by default.
#
#   ./deploy/publish-release.sh dist/release            # print the plan, touch nothing
#   ./deploy/publish-release.sh dist/release --apply    # upload to every reachable node
#   ./deploy/publish-release.sh dist/release --apply node-a
#
# With --apply it holds the release lock (deploy/release-lock.sh) while it uploads, and refuses
# to start while another session holds it, or while the lock node (the first in nodes.conf) is
# unreachable. --force-lock takes a held lock over, and goes on WITHOUT a lock, with a warning,
# when the lock node cannot keep one.
#
# Needs the nodes to run a aivoicebus with --release-dir (deploy/install.sh does
# that). Order matters: binaries first, then the signature, the manifest LAST, so
# a client never sees a manifest that points at files that are not there yet.
# Each file goes up under a dot-name (never served) and is renamed into place.
# The nodes do not verify anything: clients check the signature themselves.
set -euo pipefail
cd "$(dirname "$0")/.."
. deploy/nodes.sh

usage() { echo "usage: $0 <release dir> [--apply] [--force-lock] [node]" >&2; exit 2; }
[ $# -ge 1 ] || usage
DIR="$1"; shift
APPLY=0; ONLY=""; FORCE_LOCK=""
for a in "$@"; do
  case "$a" in --apply) APPLY=1 ;; --force-lock) FORCE_LOCK=1 ;; -*) usage ;; *) ONLY="$a" ;; esac
done
[ -s "$DIR/manifest.json" ] && [ -s "$DIR/manifest.sig" ] || { echo "$DIR has no signed manifest (run: scripts/release.sh sign $DIR)" >&2; exit 1; }
VER=$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' "$DIR/manifest.json" | head -1)
FILES=$(cd "$DIR" && ls aivoicebus-* 2>/dev/null | tr '\n' ' ')
[ -n "$FILES" ] || { echo "no aivoicebus-* binaries in $DIR" >&2; exit 1; }
echo "release $VER: $FILES+ manifest.sig manifest.json"
if [ "$APPLY" = 1 ]; then
  LOCK_NAME=$(printf '%s@%s:%s' "$(id -un 2>/dev/null)" "$(hostname 2>/dev/null)" "$$" | tr -cd 'A-Za-z0-9._@:-' | tail -c 80)   # who, where, which run
  LOCKED=0
  trap '[ "$LOCKED" = 0 ] || deploy/release-lock.sh release "$LOCK_NAME" >/dev/null 2>&1 || echo "could not give the release lock back: deploy/release-lock.sh release $LOCK_NAME" >&2' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  rc=0; deploy/release-lock.sh take "$LOCK_NAME" "publish $VER" ${FORCE_LOCK:+--force} || rc=$?
  case "$rc" in
    0) LOCKED=1 ;;
    6) ;;   # no lock could be kept and --force-lock said go on
    *) echo "not published; nothing was uploaded" >&2; exit 1 ;;
  esac
fi

for n in "${NODES[@]}"; do IFS=: read -r name alias ip <<<"$n"
  [ -z "$ONLY" ] || [ "$ONLY" = "$name" ] || continue
  echo "== $name ($alias)"
  [ "${LOCKED:-0}" = 0 ] || deploy/release-lock.sh refresh "$LOCK_NAME" >/dev/null 2>&1 || { echo "   the release lock is no longer ours; stopping" >&2; exit 1; }
  if [ "$APPLY" = 0 ]; then echo "   would upload to /srv/aivoicebus-release/ (manifest last)"; continue; fi
  ssh -n -o ConnectTimeout=10 -o BatchMode=yes "$alias" true 2>/dev/null || { echo "   UNREACHABLE, skipping (re-run: $0 $DIR --apply $name)"; continue; }
  ssh -n "$alias" 'install -d -m 755 /srv/aivoicebus-release'
  for f in $FILES manifest.sig manifest.json; do
    scp -q "$DIR/$f" "$alias:/srv/aivoicebus-release/.$f.new"
    ssh -n "$alias" "chmod 644 /srv/aivoicebus-release/.$f.new && mv /srv/aivoicebus-release/.$f.new /srv/aivoicebus-release/$f"
  done
  echo "   uploaded"
done
echo
[ "$APPLY" = 1 ] && echo "done. From any machine: aivoicebus update --check" || echo "dry run only."
