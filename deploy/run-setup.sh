#!/bin/sh
# Sets up `aivoicebus run` on THIS Mac, in one go, so nothing is copied by hand:
#   1. updates aivoicebus from the signed GitHub release (the signature is checked first),
#   2. lets every machine in SENDERS send it [run] tasks (the worker allowlist),
#   3. writes the named commands of commands/ (default: brightness uptime battery) into ~/.config/aivoicebus/run.d,
#   4. installs the worker service with --run.
# Run it yourself, in your own terminal (worker install --run refuses inside an agent's session):
#   curl -fsSL https://raw.githubusercontent.com/baditaflorin/aivoicebus/<COMMIT>/deploy/run-setup.sh | sh
# Pin <COMMIT> to a full commit id you have read. Run it again any time: every step is repeatable.
set -eu

SENDERS="${AIVOICEBUS_RUN_SENDERS:-florins-macbook-air-m4-mba mac14-m1pro-mbp imac-m1-imac pentests-macbook-air-m1-mba}"
REF="${AIVOICEBUS_RUN_REF:-main}"
COMMANDS="${AIVOICEBUS_RUN_COMMANDS:-brightness uptime battery}"
RAW="https://raw.githubusercontent.com/baditaflorin/aivoicebus/$REF/commands"
TMP="$(mktemp)"; trap 'rm -f "$TMP"' EXIT
DIR="$HOME/.config/aivoicebus/run.d"

command -v aivoicebus >/dev/null || { echo "aivoicebus is not installed here: run aivoicebus setup first" >&2; exit 1; }

echo "== 1. update"
aivoicebus update --from github || echo "(update did not install anything; continuing with $(aivoicebus version | head -1))"
aivoicebus worker --help 2>&1 | grep -q '^  -run' || { echo "this aivoicebus ($(aivoicebus version | head -1)) has no worker --run: it needs 0.15.0 or newer" >&2; exit 1; }

echo "== 2. who may send"
for s in $SENDERS; do
  aivoicebus worker allow add "$s" >/dev/null 2>&1 && echo "allowed $s" || echo "skipped $s (this machine, or already allowed)"
done

echo "== 3. named commands in $DIR"
mkdir -p "$DIR" && chmod 700 "$DIR"
for c in $COMMANDS; do
  case "$c" in *[!a-z0-9_-]* | "") echo "skipped $c: not a command name" >&2; continue ;; esac
  curl -fsSL "$RAW/$c/command.json" | sed "s|{{DIR}}|$DIR|g" > "$DIR/$c.json" || { echo "skipped $c: no such command in the repo" >&2; rm -f "$DIR/$c.json"; continue; }
  chmod 600 "$DIR/$c.json"
  if curl -fsSL -o "$TMP" "$RAW/$c/FILES" 2>/dev/null; then
    while IFS= read -r f; do
      case "$f" in "" | */* | .*) continue ;; esac
      curl -fsSL -o "$DIR/$f" "$RAW/$c/$f" && chmod 600 "$DIR/$f"
    done < "$TMP"
  fi
  echo "installed $c"
done
ls -l "$DIR"

echo "== 4. worker service"
aivoicebus worker install --run
aivoicebus worker status
