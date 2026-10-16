#!/usr/bin/env bash
# Temporarily serve the client bundle from ONE node so new Macs can download it
# over the tailnet (no file copying). Off by default; auto-expires after 24h.
#
#   ./deploy/bootstrap-share.sh on   [node]   # default node: node-a; prints the URL
#   ./deploy/bootstrap-share.sh off  [node]   # stop serving + delete the bundle
set -euo pipefail
cd "$(dirname "$0")/.."
ACTION="${1:?usage: $0 on|off [node]}"; NODE="${2:-node-a}"
. deploy/nodes.sh   # NODES from deploy/nodes.conf, the one list of nodes
ALIAS=""; IP=""
for n in "${NODES[@]}"; do
  IFS=: read -r name a ip <<<"$n"
  if [ "$name" = "$NODE" ]; then ALIAS=$a; IP=$ip; fi
done
[ -n "$ALIAS" ] || { echo "unknown node $NODE (see deploy/nodes.conf)" >&2; exit 2; }
# the bundle to serve: deploy/make-client-bundle.sh writes dist/aivoicebus-client-bundle.tgz
BUNDLE="${BUNDLE:-../aivoicebus-client-bundle.tgz}"
KEYFILE="$HOME/.config/aivoicebus/bootstrap-key"

if [ "$ACTION" = on ]; then
  [ -s "$BUNDLE" ] || { echo "missing $BUNDLE" >&2; exit 1; }
  [ -s "$KEYFILE" ] || { umask 077; head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$KEYFILE"; }
  KEY=$(cat "$KEYFILE")
  ssh "$ALIAS" 'install -d -m 755 /srv/aivoicebus-share && cat > /srv/aivoicebus-share/bundle.tgz && chmod 644 /srv/aivoicebus-share/bundle.tgz' < "$BUNDLE"
  ssh -n "$ALIAS" "set -e
    sed -i '/^BOOTSTRAP_ARGS=/d' /etc/aivoicebus/node.env
    echo 'BOOTSTRAP_ARGS=--bootstrap-file /srv/aivoicebus-share/bundle.tgz --bootstrap-key $KEY' >> /etc/aivoicebus/node.env
    systemctl restart aivoicebus
    for i in \$(seq 1 40); do curl -fsS http://$IP:7777/healthz >/dev/null 2>&1 && break; sleep 1; done"
  CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://$IP:7777/bootstrap/$KEY.tgz")
  [ "$CODE" = 200 ] || { echo "download check failed: HTTP $CODE" >&2; exit 1; }
  echo "serving (24h): http://$IP:7777/bootstrap/$KEY.tgz"
else
  ssh -n "$ALIAS" "set -e
    sed -i '/^BOOTSTRAP_ARGS=/d' /etc/aivoicebus/node.env
    rm -rf /srv/aivoicebus-share
    systemctl restart aivoicebus
    for i in \$(seq 1 40); do curl -fsS http://$IP:7777/healthz >/dev/null 2>&1 && break; sleep 1; done"
  KEY=$(cat "$KEYFILE" 2>/dev/null || echo none)
  echo "after off: HTTP $(curl -s -o /dev/null -w '%{http_code}' "http://$IP:7777/bootstrap/$KEY.tgz") (expect 404)"
  rm -f "$KEYFILE"
fi
