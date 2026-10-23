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

echo "# a session nobody has seen is refused, --queue keeps it; a body that needs nothing goes as fyi; a question as fyi is warned"
as imac/claude
step send --to air/codex.zz99 --dry-run "the PR is ready"
step send --to air/codex.zz99 --dry-run --queue "the PR is ready"
step send --to air/codex --dry-run "FYI the build is green, no action needed"
step send --to air/codex --dry-run --prio fyi "who owns the gateway?"

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

echo "# leases: run holds a key while its command goes on and frees it after; a held key refuses a second run; globs list"
as imac/claude
step lease run deploy:prod:api --ttl 10m --note "shipping 1.2.3" --settle 0s -- sh -c 'echo "deploying under $AIVOICEBUS_LEASES"; exit 3'
step lease list
step lease take deploy:prod:api --ttl 10m --note "shipping 1.2.3" --settle 0s
step lease take load:prod/scan-a --settle 0s
as air/codex
step lease run deploy:prod:api --settle 0s -- sh -c 'echo "never runs"'
step lease run deploy:prod:api --settle 0s --override-hold short -- true
step lease run deploy:prod:api --settle 0s --override-hold "Florin agreed: the hold is stale" -- sh -c 'echo "ran anyway"'
step lease list 'deploy:*'
step lease list 'load:*' 'hub:*'
step lease list 'nothing:*'
step audit | grep -E 'lease override|^\$|^\[exit' | sed -E 's/[0-9]{4}-[0-9]{2}-[0-9]{2}T<TIME>Z/<UTC>/'   # norm took the clock; the date would go stale
as imac/claude
step inbox --ack
step lease release deploy:prod:api
step lease release load:prod/scan-a
step lease list

echo "# the hook is silent when there is nothing to say, and honours the opt-out file"
as air/codex
step hook --tool codex
( cd "$T" && touch .aivoicebus-off && as other/agent && step send --to air/codex "while opted out" >/dev/null; as air/codex; step hook --tool codex )
rm -f "$T/.aivoicebus-off"
step hook --tool codex

echo "# version and usage"
step version
echo "\$ aivoicebus usage | head -4"; "$BIN" usage >"$T/usage.txt"; head -4 "$T/usage.txt" # not a pipe into head: usage is a few pages and dies of SIGPIPE under pipefail when head leaves first

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
echo "# a long broadcast is one line at a session start, the whole text one command away; a short direct message is untouched"
as other/agent
BODY="the build VM disk emergency is over: the disk was grown to 512 GB"
for _ in 1 2 3 4 5 6 7 8 9 10; do BODY="$BODY
removed the node_modules of the idle run directories, nothing else changed."; done
echo "\$ echo '<the build VM disk emergency, 11 lines>' | aivoicebus send --force --to all -"
out=$(printf '%s\n' "$BODY" | "$BIN" send --force --to all -); echo "$out" | norm
BID=$(echo "$out" | awk '$1=="sent"{print $2}')
step send --to air/codex "can you look at the build?"
as air/codex
step hook --tool claude
step thread "$BID"
echo "# amend: a correction replaces the original; a session that had not read it gets only the new text, one that had gets it marked as replacing what it read"
as imac/claude.m1
out=$("$BIN" send --to air/amend --queue --prio urgent "deploy to prod at noon" 2>&1); echo "\$ aivoicebus send --to air/amend --queue --prio urgent deploy to prod at noon"; echo "$out" | norm
AMID=$(echo "$out" | awk '$1=="sent"{print $2}')
as air/amend.r1
step inbox --ack
as imac/claude.m1
step amend --dry-run "$AMID" "deploy to prod at 13:00, not noon"
step amend "$AMID" "deploy to prod at 13:00, not noon"
step amend "$AMID" "deploy to prod at 14:00 (the latest correction replaces the correction)"
as air/amend.r1
step inbox --ack
as air/amend.r2
step hook --tool codex
as imac/claude.m1
step thread "$AMID"
as air/amend.r1
step amend "$AMID" "I did not send this"
echo "# an approval is the person's signed decision: an agent's bare approve is refused and the person is pointed at their terminal; a pipe link's key and an approver signature stay out of a message"
as smoke-w/worker
echo "\$ aivoicebus send --to smoke-p/human '[approval needed] the worker on smoke-w would run this task: ...'"
out=$("$BIN" send --to smoke-p/human --queue --prio urgent "[approval needed] the worker on smoke-w would run this task from smoke-a/claude.a1 with write access:
write the notes file

A person approves in a terminal, with the approver passphrase: aivoicebus approve <this id>"); echo "$out" | norm
AID=$(echo "$out" | awk '$1=="sent"{print $2}')
as smoke-a/claude.a1
step reply --re "$AID" approve
step reply --re "$AID" "Approved, go ahead"
step reply --re "$AID" "which repository is this?"
as smoke-p/shell
step answer
step approve "$AID"
as smoke-a/claude.a1
KEY=$(printf 'a%.0s' $(seq 1 64))
SIG="[approver-sig v1 $(printf 'B%.0s' $(seq 1 40))]"
echo "\$ aivoicebus send --to smoke-b/worker 'LINK (5 minutes): https://pipe.example.test/#smokeid1~<64 hex characters>'"
set +e; out=$("$BIN" send --to smoke-b/worker "LINK (5 minutes): https://pipe.example.test/#smokeid1~$KEY" 2>&1 </dev/null); rc=$?; set -e
echo "$out" | norm; echo "[exit $rc]"
echo "\$ aivoicebus send --to smoke-b/worker 'done, signed: [approver-sig v1 <base64>]'"
set +e; out=$("$BIN" send --to smoke-b/worker "done, signed: $SIG" 2>&1 </dev/null); rc=$?; set -e
echo "$out" | norm; echo "[exit $rc]"
echo "# the test lane: a test message reaches only a session that opted in and asked for it, never a hook or a plain read; selftest checks the nodes hide it; pause --only stops one session"
as lap/claude.t1
step send --test --to lap/claude.t2 "calc 17*23"
as lap/claude.t2
step inbox --test
AIVOICEBUS_TEST=1 "$BIN" inbox >/dev/null 2>&1 # opts in: the session declares test=on with its reads (quietly: the broadcast above waits here too)
as lap/claude.t1
out=$("$BIN" send --test --to lap/claude.t2 "calc 17*23" 2>&1); echo "\$ aivoicebus send --test --to lap/claude.t2 \"calc 17*23\""; echo "$out" | norm
TID=$(echo "$out" | awk '$1=="sent"{print $2}')
as lap/claude.t2
step hook --tool claude
step inbox
step thread "$TID"
( export AIVOICEBUS_TEST=1; step inbox --test; step thread --test "$TID" )
as lap/claude.t1
step pause --only lap
step pause --only lap/claude.t2 --test --for 30m "too long for a test"
echo "\$ aivoicebus selftest"; "$BIN" selftest 2>&1 </dev/null | sed -E 's/ +[0-9]+ms/ <MS>/; s/^selftest passed \([0-9]+ms\)/selftest passed (<MS>)/' | norm
echo "# owner records: a machine records that it answers for a subject; a send to owner:SUBJECT reaches the session there that has the role; an unpinned record is never used for an urgent message; nobody live goes to the person"
onorm() { sed -E -e 's/([0-9]+[hms])+ ago/<AGE> ago/g' -e 's/expires in [0-9]+h/expires in <N>h/g' -e 's/ ([0-9]+[hms])+ +[0-9]+h +(signed)/ <AGE> <N>h \2/' -e 's/until .* \(lease/until <WHEN> (lease/'; }
as own-a/claude.pve1
AIVOICEBUS_LABELS="role=pve" "$BIN" inbox >/dev/null 2>&1 # a session that claims the role is live on own-a
as own-a/claude.fce3
step owner set 0mcp:vm701 --role pve --person ana | onorm
step owner set '0mcp:*' --person ana | onorm
step owner set 'bad subject' | onorm
as own-b/claude.b1
step owner get 0mcp:vm701 | onorm
step owner get 0mcp:vm999 | onorm
step owner get service:nothing | onorm
step owner list | onorm
out=$("$BIN" send --to owner:0mcp:vm701 "who tunes the pool?" 2>&1); echo "\$ aivoicebus send --to owner:0mcp:vm701 who tunes the pool?"; echo "$out" | norm | onorm
step send --to owner:0mcp:vm701 --prio urgent "stop the pool" | onorm
step owner trust 'own-a/claude.*'
step send --dry-run --to owner:0mcp:vm701 --prio urgent "stop the pool" | onorm
step task owner:0mcp:vm999 --person "restart vm999" | onorm
step lease take owner:0mcp:vm701
as own-a/claude.pve1
echo "\$ aivoicebus inbox | grep -A1 'own-b/claude.b1 ->'"; "$BIN" inbox 2>&1 | norm | grep -A1 'own-b/claude.b1 ->' # the session that has the role got it
step lease list
as own-a/claude.fce3
step owner rm 0mcp:vm701 | onorm
step owner rm 0mcp:vm701 | onorm
echo "# deadlines: --reply-by says what became of a message instead of taking silence for consent (read and no reply: exit 0; nobody read it: exit 8); --until turns an offer nobody takes into 'not doing it'"
as imac/claude.d1
stepd() { step "$@" 2>&1 | sed -E 's/\(in [0-9hms.]+\)/(in <SPAN>)/'; }
stepd send --to air/dl.r1 --queue --dry-run "deploying at 15:40Z; reply if you are mid-change" --reply-by 15m --escalate machine,person
( sleep 1; AIVOICEBUS_ME=air/dl.r1 "$BIN" inbox --ack >/dev/null 2>&1 ) &
RD=$!
stepd send --to air/dl.r1 --queue "deploying at 15:40Z; reply if you are mid-change" --reply-by 3s
wait "$RD"
stepd send --to air/dl.r2 --queue "force-pushing branch x, tell me now" --reply-by 2s
stepd send --to air/dl.r2 --queue --reply-by 25h "too far away"
echo "\$ aivoicebus task air/dl.r3 --queue \"take the 5pm backup\" --until 1m --detach"
out=$("$BIN" task air/dl.r3 --queue "take the 5pm backup" --until 1m --detach 2>&1 </dev/null); echo "$out" | sed -E 's/\(in [0-9hms.]+\)/(in <SPAN>)/' | norm
OFFER=$(echo "$out" | awk '$1=="sent"{print $2}')
stepd status "$OFFER"
echo "# a session that ends: --or-successor lets the nodes hand a message to the session that replaces it once it has been gone for 15 minutes (reply does by default); it applies to one exact session only"
as lap/claude.o2
"$BIN" inbox >/dev/null 2>&1 # declares the session, so it is live to the sends below (quietly: the broadcasts above wait here too)
as lap/claude.o1
step send --to lap/claude.o2 --or-successor --dry-run "the answer you asked for"
step send --to lap --or-successor "to a machine"
step send --to lap/claude.o2 --or-successor --file /dev/null "x"
out=$("$BIN" send --to lap/claude.o2 --or-successor "the answer you asked for" 2>&1); echo "\$ aivoicebus send --to lap/claude.o2 --or-successor the answer you asked for"; echo "$out" | norm
OID=$(echo "$out" | awk '$1=="sent"{print $2}')
as lap/claude.o2
step reply --re "$OID" --dry-run "got it"
step reply --re "$OID" --or-successor=false --dry-run "got it"
