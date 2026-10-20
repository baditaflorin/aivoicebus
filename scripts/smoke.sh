#!/usr/bin/env bash
# End-to-end smoke test of a BUILT aivoicebus binary. Starts a throwaway node on
# loopback, drives the CLI through every command, and prints a transcript with
# ids and times normalised, so two builds can be compared with diff:
#
#   scripts/smoke.sh ./aivoicebus-old > old.txt
#   scripts/smoke.sh ./aivoicebus-new > new.txt && diff -u old.txt new.txt
#
# Touches nothing outside a temp dir (HOME is redirected). Needs bash and curl.
set -euo pipefail
BIN=$(cd "$(dirname "${1:?usage: smoke.sh <aivoicebus binary>}")" && pwd)/$(basename "$1")
T=$(mktemp -d)
PID=""
trap '[ -n "$PID" ] && kill "$PID" 2>/dev/null; rm -rf "$T"' EXIT
export HOME="$T/home"; mkdir -p "$HOME"
unset AIVOICEBUS_AGENT AIVOICEBUS_ME AIVOICEBUS_HOST AIVOICEBUS_TOKEN_FILE CLAUDE_ENV_FILE AIVOICEBUS_LABELS # a session's pinned repo would turn on the collision radar

PORT=$((20000 + RANDOM % 20000))
TOKEN=smoke-token-0123456789-0123456789
printf '%s' "$TOKEN" >"$T/token"
"$BIN" serve --addr "127.0.0.1:$PORT" --data "$T/data" --token-file "$T/token" --node smoke >"$T/serve.log" 2>&1 &
PID=$!
for _ in $(seq 1 50); do curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
export AIVOICEBUS_URL="http://127.0.0.1:$PORT" AIVOICEBUS_TOKEN="$TOKEN"
cd "$T" # a non-git directory: the labels in the transcript must not depend on where this runs

norm() {
  sed -E -e "s#$BIN#<BIN>#g" -e "s#$T#<TMP>#g" -e 's/127\.0\.0\.1:[0-9]+/127.0.0.1:<PORT>/g' \
    -e 's/[0-9a-f]{10}/<ID>/g' \
    -e 's/[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}/<TIME>/g' \
    -e 's/ at [0-9]{2}:[0-9]{2} UTC/ at <TIME> UTC/g' \
    -e 's/(^|[^0-9:<])[0-9]{2}:[0-9]{2}(:[0-9]{2})?([^0-9]|$)/\1<TIME>\3/g' \
    -e 's/([A-Z][a-z]{2} [0-9]{1,2} |[0-9]{4} [A-Z][a-z]{2} [0-9]{1,2} )<TIME> [A-Z]{2,5}/<TIME>/g' \
    -e 's/\(commit [^,]+, [a-z0-9]+\/[a-z0-9]+\)/(commit <C>, <PLATFORM>)/' \
    -e 's/aivoicebus [0-9]+\.[0-9]+\.[0-9]+ \(commit/aivoicebus <VER> (commit/' \
    -e 's/release [0-9]+\.[0-9]+\.[0-9]+/release <VER>/g'
}
# step <args...>: run the binary, print command, normalised output and exit code
step() {
  echo "\$ aivoicebus $*" | norm
  local out rc
  set +e; out=$("$BIN" "$@" 2>&1 </dev/null); rc=$?; set -e
  [ -z "$out" ] || printf '%s\n' "$out" | norm
  echo "[exit $rc]"
}
as() { export AIVOICEBUS_ME=$1; }

echo "# no args / unknown command / bad flag"
step
step frobnicate
step send --bogus
step send -h

echo "# identity"
as imac/claude
step whoami

echo "# send, reply, thread"
out=$("$BIN" send --to air/codex "hello there"); echo "\$ aivoicebus send --to air/codex hello there"; echo "$out" | norm
ID=$(echo "$out" | awk '{print $2}')
echo "\$ echo 'from stdin' | aivoicebus send --to air/codex -"
echo 'from stdin' | "$BIN" send --to air/codex - | norm
step send --to air/codex
step send "no target"

echo "# inbox as the recipient"
as air/codex
step hook --tool codex
step inbox
step inbox --ack
step inbox
step inbox --all
step send --to imac/claude --re "$ID" "answer"
step thread "$ID"
step thread

echo "# acks, who, whole-host and whole-tool targets"
as imac/claude
step inbox --ack
as other/agent
step send --to imac "to the whole host"
as imac/claude
step inbox
step ack "$ID"
step who | sed -E 's/ +/ /g'

echo "# labels and selectors"
declare_as() { export AIVOICEBUS_LABELS=$2; as "$1"; "$BIN" inbox >/dev/null 2>&1; }
declare_as imac/claude "repo=alpha,tool=claude"
step whoami --labels
declare_as air/codex "repo=beta,tool=codex"
as other/agent; export AIVOICEBUS_LABELS=""
step send --to @repo=alpha "for the alpha repo"
step send --queue --to "@repo=alpha,tool=codex" "alpha codex only" # nobody matches: --queue, or send refuses
step send --to "@bad key=x" "oops"
as imac/claude; export AIVOICEBUS_LABELS="repo=alpha,tool=claude"; step inbox --ack
as air/codex; export AIVOICEBUS_LABELS="repo=beta,tool=codex"; step inbox
step who @repo=alpha
step who @repo=nobody
unset AIVOICEBUS_LABELS

echo "# priority and ttl"
as other/agent
step send --to air/codex --prio urgent "build is broken"
step send --to air/codex --prio fyi --ttl 2h "lunch at noon"
step send --to air/codex "plain question"
step send --to air/codex --prio loud "nope"
step send --to air/codex --ttl 10s "too short"
as air/codex
step hook --tool codex
echo "\$ aivoicebus inbox --all | tail -9"; "$BIN" inbox --all | norm | tail -9

echo "# retract: undo a send (the text is hidden, a tombstone stays; the last message sent is the default)"
as imac/claude
out=$("$BIN" send --to air/codex "sent to the wrong session" 2>&1); echo "\$ aivoicebus send --to air/codex sent to the wrong session"; echo "$out" | norm
RID=$(echo "$out" | awk '$1=="sent"{print $2}')
# The built node runs the release in VERSION, which a test keeps at or after the one that
# introduced retractions, so no node is named as still delivering the message.
retract_step() { step "$@" | sed -E 's/[0-9]{4}-<TIME>/<DATE>/'; }
retract_step retract --dry-run
retract_step retract
retract_step retract "$RID"
retract_step retract --leak "$RID"
as imac/mcp.zzzz # another session of the same machine: with no id it takes nothing back, and names what the machine sent last
retract_step retract
as air/codex
step inbox
echo "\$ aivoicebus inbox --all | tail -2"; "$BIN" inbox --all | norm | tail -2
step thread "$RID"

echo "# the hook is silent when there is nothing to say, and honours the opt-out file"
as air/codex
step hook --tool codex
( cd "$T" && touch .aivoicebus-off && as other/agent && step send --to air/codex "while opted out" >/dev/null; as air/codex; step hook --tool codex )
rm -f "$T/.aivoicebus-off"
step hook --tool codex

echo "# version and usage"
step version
echo "\$ aivoicebus usage | head -4"; "$BIN" usage | head -4

echo "# setup and doctor, in a scratch HOME"
step setup --dry-run --url "$AIVOICEBUS_URL"
step setup --yes --url "$AIVOICEBUS_URL"
step doctor --offline
mkdir -p "$HOME/.local/bin"; cp "$BIN" "$HOME/.local/bin/aivoicebus"
cp "$T/token" "$HOME/.config/aivoicebus/token"; chmod 600 "$HOME/.config/aivoicebus/token"
step doctor --offline
step setup
step doctor
step uninstall --dry-run
step trust-key list

echo "# hand a credential to the agents (piped here; at a terminal it asks with nothing shown)"
echo "\$ printf '<token>' | aivoicebus secret save demo --no-check --host api.example.test"
printf '%s' "$TOKEN" | "$BIN" secret save demo --no-check --host api.example.test 2>&1 | norm
step secret list | sed -E 's/[0-9]{4}-<TIME>/<DATE>/'   # norm turned the month-day and time into <TIME>; the year would go stale
step secret curl demo -- https://evil.example.com/x
step secret curl demo -- -v https://api.example.test/x
step secret rm demo
step secret list

echo "# worker: runs nothing until a sender is allowed; a dry run; one pass; the answer"
as smoke-a/claude.a1
step task smoke-b "what is 6*7"
as smoke-b/worker
step worker --once --exec cat
step worker allow add all
step worker allow add smoke-a
step worker allow list
step worker --dry-run --exec cat
step worker --once --settle 0s --exec cat
step worker --once --settle 0s --exec cat
as smoke-a/claude.a1
step inbox --ack

echo "# not configured"
( unset AIVOICEBUS_URL; export HOME="$T/empty"; mkdir -p "$HOME"; step who )
echo "# wrong token"
( export AIVOICEBUS_TOKEN=wrong-token-0123456789-0123456789; step who )
echo "# hook with the mesh down"
( export AIVOICEBUS_URL=http://127.0.0.1:1; step hook --tool claude )
