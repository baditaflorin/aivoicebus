#!/usr/bin/env bash
# Publishes the signed node list (nodes.json + nodes.sig) to every node, so machines learn
# about a new node without anyone touching them (clients refresh it once a day at session
# start, or with `aivoicebus nodes --refresh`). DRY RUN by default.
#
#   aivoicebus release nodes --dir dist/nodes     # on the machine with the signing key; reads deploy/nodes.conf
#   deploy/publish-nodes.sh dist/nodes           # show what would be uploaded
#   deploy/publish-nodes.sh dist/nodes --apply   # upload to every node
#
# The list is signed with the release key: nodes only store and serve it, and a client
# trusts it only if it verifies against the key compiled into its binary and its serial
# is newer than the one it already has. Uploads the signature last, like publish-release.sh,
# so a client never sees a list whose signature is the old one for long.
set -euo pipefail
cd "$(dirname "$0")/.."
DIR="${1:?usage: $0 <dir with nodes.json and nodes.sig> [--apply]}"; APPLY=0; [ "${2:-}" = "--apply" ] && APPLY=1
for f in nodes.json nodes.sig; do [ -s "$DIR/$f" ] || { echo "missing $DIR/$f (run: aivoicebus release nodes --dir $DIR)" >&2; exit 1; }; done
. deploy/nodes.sh
echo "list: $(python3 -c "import json,sys; d=json.load(open('$DIR/nodes.json')); print('serial', d['serial'], '-', ', '.join(n['name'] for n in d['nodes']))")"
for n in "${NODES[@]}"; do IFS=: read -r name alias ip <<<"$n"
  echo "== $name ($alias)"
  [ "$APPLY" = 1 ] || { echo "   would upload nodes.json, then nodes.sig to /srv/aivoicebus-release/"; continue; }
  ssh -n -o ConnectTimeout=10 -o BatchMode=yes "$alias" true 2>/dev/null || { echo "   UNREACHABLE, skipping (re-run later)"; continue; }
  ssh "$alias" 'install -d -m 755 /srv/aivoicebus-release && cat > /srv/aivoicebus-release/nodes.json.new && chmod 644 /srv/aivoicebus-release/nodes.json.new' < "$DIR/nodes.json"
  ssh "$alias" 'cat > /srv/aivoicebus-release/nodes.sig.new && chmod 644 /srv/aivoicebus-release/nodes.sig.new && mv /srv/aivoicebus-release/nodes.json.new /srv/aivoicebus-release/nodes.json && mv /srv/aivoicebus-release/nodes.sig.new /srv/aivoicebus-release/nodes.sig' < "$DIR/nodes.sig"
  echo "   uploaded"
done
[ "$APPLY" = 1 ] && echo && echo "done. Check from any machine: aivoicebus nodes --refresh" || echo "dry run only."
