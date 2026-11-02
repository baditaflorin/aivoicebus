#!/bin/sh
# Sets up `aivoicebus run` on THIS Mac, in one go, so nothing is copied by hand:
#   1. updates aivoicebus from the signed GitHub release (the signature is checked first),
#   2. lets every machine in SENDERS send it [run] tasks (the worker allowlist),
#   3. writes the named commands of deploy/run.d/ (today: brightness) into ~/.config/aivoicebus/run.d,
#   4. installs the worker service with --run.
# Run it yourself, in your own terminal (worker install --run refuses inside an agent's session):
#   curl -fsSL https://raw.githubusercontent.com/baditaflorin/aivoicebus/<COMMIT>/deploy/run-setup.sh | sh
# Pin <COMMIT> to a full commit id you have read. Run it again any time: every step is repeatable.
set -eu

SENDERS="${AIVOICEBUS_RUN_SENDERS:-florins-macbook-air-m4-mba mac14-m1pro-mbp imac-m1-imac pentests-macbook-air-m1-mba}"
REF="${AIVOICEBUS_RUN_REF:-main}"
RAW="https://raw.githubusercontent.com/baditaflorin/aivoicebus/$REF/deploy/run.d"
DIR="$HOME/.config/aivoicebus/run.d"

command -v aivoicebus >/dev/null || { echo "aivoicebus is not installed here: run aivoicebus setup first" >&2; exit 1; }

echo "== 1. update"
aivoicebus update --from github || echo "(update did not install anything; continuing with $(aivoicebus version | head -1))"
aivoicebus worker --help 2>&1 | grep -q -- '--run' || { echo "this aivoicebus ($(aivoicebus version | head -1)) has no worker --run: it needs 0.15.0 or newer" >&2; exit 1; }

echo "== 2. who may send"
for s in $SENDERS; do
  aivoicebus worker allow add "$s" >/dev/null 2>&1 && echo "allowed $s" || echo "skipped $s (this machine, or already allowed)"
done

echo "== 3. named commands in $DIR"
mkdir -p "$DIR" && chmod 700 "$DIR"
curl -fsSL -o "$DIR/bright.py" "$RAW/bright.py"
chmod 600 "$DIR/bright.py"
printf '%s\n' '{"argv":["/usr/bin/python3","-I","'"$DIR"'/bright.py","{0}"],"args":["(0(\\.[0-9]+)?|1(\\.0+)?)"],"sandbox":false,"timeout":"30s"}' > "$DIR/brightness.json"
chmod 600 "$DIR/brightness.json"
ls -l "$DIR"

echo "== 4. worker service"
aivoicebus worker install --run
aivoicebus worker status
