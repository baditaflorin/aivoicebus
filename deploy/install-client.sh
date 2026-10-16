#!/usr/bin/env bash
# Installs the aivoicebus client on this Mac from an unpacked client bundle
# (built by deploy/make-client-bundle.sh). All configuration logic lives in the
# binary: this script only places the binary and token, then runs
# `aivoicebus setup` and `aivoicebus doctor`.
#
#   bash install-client.sh
set -euo pipefail
cd "$(dirname "$0")"
case "$(uname -m)" in
  arm64) BIN=aivoicebus-arm64 ;;
  x86_64) BIN=aivoicebus-amd64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

mkdir -p "$HOME/.local/bin" "$HOME/.config/aivoicebus"
chmod 700 "$HOME/.config/aivoicebus"
# a binary that arrived by download or AirDrop carries the quarantine flag
install -m 755 "$BIN" "$HOME/.local/bin/aivoicebus"
xattr -d com.apple.quarantine "$HOME/.local/bin/aivoicebus" 2>/dev/null || true
(umask 077; cp token "$HOME/.config/aivoicebus/token")
chmod 600 "$HOME/.config/aivoicebus/token"

FM="$HOME/.local/bin/aivoicebus"
"$FM" setup --yes --url "$(cat urls)"
echo
# doctor exits 1 on a FAIL, for instance a token the mesh has since retired. Under `set -e` that
# would skip the reminder below, which is about a folder that holds the token, so take its
# status and pass it on at the end.
rc=0
"$FM" doctor || rc=$?
echo
if [ "$rc" -eq 0 ]; then
  echo "Done."
else
  echo "Installed, but doctor found problems (above). If the mesh rejects the token, this bundle is out of date: ask for a new one."
fi
echo "Delete this bundle folder now (it contains the token)."
exit "$rc"
