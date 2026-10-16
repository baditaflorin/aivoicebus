#!/usr/bin/env bash
# Assembles the client bundle new Macs install from: both Mac binaries (stamped
# with the release and commit), the node URL list from deploy/nodes.conf, the
# install script, and the shared token. The result CONTAINS THE TOKEN: never
# commit it (dist/ and *.tgz are gitignored) and delete it after use.
#
#   ./deploy/make-client-bundle.sh      # -> dist/aivoicebus-client-bundle.tgz
#
# Needs Go (run it on a build host, not a laptop) and the token at
# $AIVOICEBUS_TOKEN_FILE or ~/.config/aivoicebus/token. Then serve it with
# deploy/bootstrap-share.sh (BUNDLE=dist/aivoicebus-client-bundle.tgz).
set -euo pipefail
cd "$(dirname "$0")/.."
. deploy/nodes.sh

TOKEN_FILE="${AIVOICEBUS_TOKEN_FILE:-$HOME/.config/aivoicebus/token}"
[ -s "$TOKEN_FILE" ] || { echo "no token at $TOKEN_FILE" >&2; exit 1; }
# During a token rotation (deploy/rotate-token.sh) a new machine must get the NEW token: the old
# one stops working at `retire`.
if [ -s "$TOKEN_FILE.next" ]; then
  TOKEN_FILE="$TOKEN_FILE.next"
  echo "a token rotation is in progress: the bundle carries the NEW token ($TOKEN_FILE)"
fi

OUT=dist
D="$OUT/aivoicebus-client-bundle"
rm -rf "$D" "$OUT/aivoicebus-client-bundle.tgz"
mkdir -p "$D"
scripts/build.sh darwin arm64 "$D/aivoicebus-arm64"
scripts/build.sh darwin amd64 "$D/aivoicebus-amd64"
install -m 755 deploy/install-client.sh "$D/install-client.sh"
node_urls >"$D/urls"
(umask 077; cp "$TOKEN_FILE" "$D/token")
tar -C "$OUT" -czf "$OUT/aivoicebus-client-bundle.tgz" aivoicebus-client-bundle
rm -rf "$D"
echo "bundle: $OUT/aivoicebus-client-bundle.tgz ($(wc -c <"$OUT/aivoicebus-client-bundle.tgz") bytes) - contains the token"
