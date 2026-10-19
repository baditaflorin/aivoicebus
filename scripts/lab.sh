#!/usr/bin/env bash
# The mesh lab: real aivoicebus processes, real deploy scripts, fake machines.
#
# Four sandboxed "nodes" (127.0.0.11-14:7777) are installed, upgraded, rotated and broken
# by the REAL deploy/*.sh scripts, driven through stand-ins for ssh, scp and systemctl
# (scripts/lab/bin) that map each ssh alias to a directory. The nodes run the real binary
# under a supervisor that restarts it like systemd would. Then the clients (the real CLI,
# hook and MCP server) are pointed at them and every behaviour the unit tests only simulate
# is exercised on real sockets and real processes: replication, a hung node, a lost disk,
# the per-prompt hook, an MCP session, a backup restore, token rotation, canary rollouts
# with automatic rollback, and adding a node; then the release tooling: signing from the
# binaries' own stamp, the release lock between sessions, and the rollout's watcher.
#
#   scripts/lab.sh            # needs Linux (127.0.0.0/8 loopback), Go, curl, bash; run it on a build host
#
# It touches nothing outside a temp directory and kills every process it started.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
REPO=$PWD
[ "$(uname -s)" = Linux ] || { echo "the lab needs Linux (it binds 127.0.0.11-15); run it on a build host" >&2; exit 2; }

# One lab at a time on a machine: the nodes bind fixed loopback addresses.
exec 9>/tmp/aivoicebus-lab.lock
flock -n 9 || { echo "another mesh lab is running on this machine; wait for it" >&2; exit 2; }
# Nothing from the caller's environment may point the scripts at real files or nodes.
for v in $(env | sed -n 's/^\(AIVOICEBUS_[A-Z_]*\)=.*/\1/p'); do unset "$v"; done
unset CLAUDE_ENV_FILE

export LAB; LAB=$(mktemp -d)
cp -R scripts/lab/bin "$LAB/bin"
export PATH="$LAB/bin:$PATH"
export HOME="$LAB/home"; mkdir -p "$HOME"
export AIVOICEBUS_NODES_CONF="$LAB/nodes.conf"
export AIVOICEBUS_HEALTH_WAIT=12 AIVOICEBUS_VERIFY_WAIT=5 AIVOICEBUS_WATCH_INTERVAL=1 AIVOICEBUS_LOCK_HEARTBEAT=2
PORT=7777
FAILED=0
cleanup() {
  for d in "$LAB"/nodes/*/; do
    [ -f "$d/run/pid" ] && { LAB_ROOT="${d%/}" systemctl stop aivoicebus 2>/dev/null; }
  done
  pkill -f "$LAB/" 2>/dev/null
  rm -rf "$LAB"
}
trap cleanup EXIT

pass() { echo "  PASS: $*"; }
fail() { echo "  FAIL: $*"; FAILED=1; }
ok() { if "${@:2}" >/dev/null 2>&1; then pass "$1"; else fail "$1"; fi; }
# grep reads a here-string, never a pipe: with pipefail, `printf | grep -q` fails when grep quits at
# its first match and printf dies of SIGPIPE, so a check that matched was reported as failed.
has() { # has <what> <pattern> <text>
  if grep -q -- "$2" <<<"$3"; then pass "$1"; else fail "$1 (wanted /$2/ in: $(printf '%s' "$3" | head -c 600 | tr '\n' '|'))"; fi
}
lacks() { if grep -q -- "$2" <<<"$3"; then fail "$1 (found /$2/)"; else pass "$1"; fi; }
eventually() { # eventually <what> <seconds> <command...>
  local what="$1" n="$2"; shift 2
  for _ in $(seq 1 "$((n * 4))"); do "$@" >/dev/null 2>&1 && { pass "$what"; return 0; }; sleep 0.25; done
  fail "$what (not within ${n}s)"
}

section() { echo; echo "== $*"; }

# ---- build the binaries: an old release, a newer one, and two that must not be rolled out
section "build"
mkdir -p "$LAB/bins"
PKG=github.com/baditaflorin/aivoicebus/internal/version
build() { CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w -X $PKG.Release=$1 -X $PKG.Commit=lab" -o "$2" . ; }
build 6.0.0 "$LAB/bins/old" && build 6.0.1 "$LAB/bins/new" && build 6.0.3 "$LAB/bins/real603" && build 6.0.5 "$LAB/bins/real605" && build 6.0.6 "$LAB/bins/real606" &&
  build 6.0.8 "$LAB/bins/real608" && build 6.0.9 "$LAB/bins/real609" && build 6.1.0 "$LAB/bins/real610" || { echo "build failed"; exit 1; }
printf '#!/bin/sh\necho "this release is broken" >&2\nexit 1\n' >"$LAB/bins/broken"
# a release that comes up and then keeps crashing: serves for 3s, dies, is restarted by the supervisor
printf '#!/bin/sh\nif [ "$1" = version ]; then exec %s version; fi\n%s "$@" &\nP=$!\nsleep 3\nkill $P 2>/dev/null\nexit 1\n' "$LAB/bins/real603" "$LAB/bins/real603" >"$LAB/bins/flapping"
chmod +x "$LAB"/bins/*
OLD="$LAB/bins/old"; NEW="$LAB/bins/new"
echo "  built 6.0.0 and 6.0.1 and two that misbehave"

release_dir() { # release_dir <name> <version> <binary>
  local d="$LAB/rel-$1"; mkdir -p "$d"
  cp "$3" "$d/aivoicebus-linux-amd64"; chmod +x "$d/aivoicebus-linux-amd64"
  printf '{\n  "version": "%s"\n}\n' "$2" >"$d/manifest.json"; echo lab >"$d/manifest.sig"
  echo "$d"
}

# ---- the four nodes
# shellcheck disable=SC2034 # not read today; documents the ssh aliases the loop below creates
ALIASES=(lab_n1 lab_n2 lab_n3 lab_n4)
: >"$LAB/nodes.conf"
for i in 1 2 3 4; do
  mkdir -p "$LAB/nodes/lab_n$i"
  echo "n$i:lab_n$i:127.0.0.1$i" >>"$LAB/nodes.conf"
done
url() { echo "http://127.0.0.1$1:$PORT"; }
ALLURLS="$(url 1),$(url 2),$(url 3),$(url 4)"
TOKEN_FILE="$HOME/.config/aivoicebus/token"
tok() { tr -d '\n' <"$TOKEN_FILE"; }
node_pid() { cat "$LAB/nodes/lab_n$1/run/pid" 2>/dev/null; }
# anchored: the supervisor's own command line also contains the binary path and "serve"
serve_pid() { pgrep -f "^$LAB/nodes/lab_n$1/usr/local/bin/aivoicebus serve" | head -1; }
nodectl() { LAB_ROOT="$LAB/nodes/lab_n$1" systemctl "${@:2}"; }
http() { # http <token-file> <url> [curl args]: the token goes to curl on stdin, never argv
  local f="$1" u="$2"; shift 2
  printf 'header = "Authorization: Bearer %s"\n' "$(tr -d '\n' <"$f")" | curl -s --max-time 5 -K - "$@" "$u"
}
code() { http "$1" "$2" -o /dev/null -w '%{http_code}' "${@:3}"; }
health_field() { local v; v=$(http "${3:-$TOKEN_FILE}" "$(url "$1")/v1/health" | grep -o "\"$2\":[0-9]*" | head -1 | cut -d: -f2); echo "${v:-0}"; }
version_of() { http "$TOKEN_FILE" "$(url "$1")/v1/health" | grep -o '"version":"[^"]*"' | head -1 | cut -d'"' -f4; }
# the records a node holds: the sum of its version vector. Not messages plus acks: a retraction is an ack
# to a release that predates retraction and is not counted among the acks by one that applies it, so those differ between nodes
# that hold the same records.
records_of() { ( . deploy/ops.sh; log_total "$(http "$TOKEN_FILE" "$(url "$1")/v1/health")" ); }
client() { AIVOICEBUS_URL="$1" AIVOICEBUS_TOKEN="$(tok)" "${@:2}"; }
fm() { AIVOICEBUS_TOKEN="$(tok)" "$LAB/bins/new" "$@"; }

section "install on four nodes"
deploy/install.sh --gen-token >/dev/null && deploy/install.sh --gen-observer-token >/dev/null
out=$(AIVOICEBUS_BIN="$OLD" deploy/install.sh --apply 2>&1); has "all four nodes come up healthy" "healthy" "$out"
[ "$(printf '%s' "$out" | grep -c '^   healthy')" = 4 ] && pass "four 'healthy' lines" || { fail "expected four healthy nodes"; echo "$out" | tail -30; }
versions_are() { [ "$(version_of 1)$(version_of 2)$(version_of 3)$(version_of 4)" = "$1$1$1$1" ]; }
eventually "every node reports release 6.0.0" 10 versions_are 6.0.0
# right after the first start the nodes are still finding each other (and an empty node takes no
# writes until a peer has answered), so give the mesh a few seconds before judging it
eventually "aivoicebus mesh finds nothing wrong" 20 bash -c "AIVOICEBUS_URL='$ALLURLS' AIVOICEBUS_TOKEN='$(tok)' '$NEW' mesh"

section "messages replicate between real processes"
M=$(AIVOICEBUS_ME=lab/sender AIVOICEBUS_URL="$(url 1)" AIVOICEBUS_TOKEN="$(tok)" "$NEW" send --to lab/reader "hello from node 1" 2>&1)
has "send to node 1 is accepted" "^sent " "$M"
mesh_nodes() { AIVOICEBUS_URL="$ALLURLS" AIVOICEBUS_TOKEN="$(tok)" "$NEW" mesh --json 2>/dev/null; }
eventually "node 4 serves it to the reader" 20 bash -c "AIVOICEBUS_ME=lab/reader AIVOICEBUS_URL='$(url 4)' AIVOICEBUS_TOKEN='$(tok)' '$NEW' inbox | grep -q 'hello from node 1'"
same_records() { [ "$(records_of 1)" = "$(records_of 2)" ] && [ "$(records_of 2)" = "$(records_of 3)" ] && [ "$(records_of 3)" = "$(records_of 4)" ]; }
eventually "every node holds the same number of records" 20 same_records

section "a sender takes a message back (retraction)"
M=$(AIVOICEBUS_ME=lab/sender AIVOICEBUS_URL="$(url 1)" AIVOICEBUS_TOKEN="$(tok)" "$NEW" send --to lab/reader "to be taken back" 2>&1)
RID=$(printf '%s\n' "$M" | awk '/^sent /{print $2; exit}')
has "the message is sent" "^sent " "$M"
shows() { grep -q 'to be taken back' <<<"$(AIVOICEBUS_ME=lab/reader AIVOICEBUS_URL="$(url "$1")" AIVOICEBUS_TOKEN="$(tok)" "$NEW" inbox 2>&1)"; }
eventually "node 3 delivers it before the retraction" 20 shows 3
acks_before=$(health_field 1 acks)
out=$(http "$TOKEN_FILE" "$(url 1)/v1/retract" -X POST -d '{"id":"'"$RID"'","by":"lab/other"}')
has "another session on the sender's machine retracts it" '"state":"retracted"' "$out"
hidden() { ! shows "$1"; }
eventually "node 1 no longer delivers it" 5 hidden 1
eventually "node 3 no longer delivers it once the record has replicated" 20 hidden 3
eventually "node 4 no longer delivers it" 20 hidden 4
eventually "every node holds the same number of records" 20 same_records
[ "$(health_field 1 acks)" = "$acks_before" ] && pass "a retraction is not a read receipt (the ack count did not move)" || fail "the ack count moved: $acks_before -> $(health_field 1 acks)"
out=$(http "$TOKEN_FILE" "$(url 2)/v1/thread?id=$RID")
has "a thread still has it, as a tombstone naming the machine" 'withdrawn by lab at' "$out"; lacks "and without its words" "to be taken back" "$out"
has "node 2 counts it" "^aivoicebus_retracted_messages 1$" "$(http "$TOKEN_FILE" "$(url 2)/metrics")"
out=$(http "$TOKEN_FILE" "$(url 1)/v1/retract" -X POST -d '{"id":"'"$RID"'","by":"other/machine"}' -w ' %{http_code}')
has "a session on another machine is refused" ' 403$' "$out"

section "records are counted from the version vector, whatever release holds them"
log_of() { ( . deploy/ops.sh; log_total "$1" ); }
# what a node that predates retraction and one that applies it say of the same ten records when one of them is a retraction: messages plus
# acks would be 10 for the first and 9 for the second, and a new node would never have 'caught up' with an old one
old_node='{"node":"a","version":"0.6.1","log":{"n1":7,"n2":3},"messages":6,"acks":4,"sessions":2,"quarantined":0}'
new_node='{"node":"b","version":"0.7.0","log":{"n1":7,"n2":3},"messages":6,"acks":3,"sessions":2,"quarantined":0,"retracted":1}'
[ "$(log_of "$old_node") $(log_of "$new_node")" = "10 10" ] && pass "both say 10 records" || fail "log_total: $(log_of "$old_node") $(log_of "$new_node")"
[ "$(log_of '')$(log_of '{"log":{}}')$(log_of 'not json')" = "000" ] && pass "an answer without records counts 0" || fail "an empty answer"
[ "$(log_of "$(http "$TOKEN_FILE" "$(url 1)/v1/health")")" = "$(records_of 1)" ] && [ "$(records_of 1)" -ge 3 ] && pass "and node 1 really holds $(records_of 1)" || fail "node 1 count"

section "an observer token reads and cannot write"
OBS="$HOME/.config/aivoicebus/observer-token"
[ "$(code "$OBS" "$(url 2)/v1/sync?vv=%7B%7D")" = 200 ] && pass "observer reads the feed" || fail "observer feed"
[ "$(code "$OBS" "$(url 2)/v1/send" -X POST -d '{"from":"a/b","to":"all","body":"x"}')" = 403 ] && pass "observer cannot send" || fail "observer send not 403"
[ "$(code "$OBS" "$(url 2)/v1/inbox?me=a/b")" = 403 ] && pass "observer cannot read an inbox" || fail "observer inbox not 403"
printf 'wrong-token-wrong-token-wrong' >"$LAB/wrong"
[ "$(code "$LAB/wrong" "$(url 2)/v1/who")" = 401 ] && pass "a wrong token is refused" || fail "wrong token not 401"

section "a hung node does not hold a client up (hedged reads)"
kill -STOP "$(serve_pid 2)"
start=$(date +%s%N)
AIVOICEBUS_ME=lab/reader AIVOICEBUS_URL="$(url 2),$(url 1)" AIVOICEBUS_TOKEN="$(tok)" "$NEW" inbox --all >/dev/null 2>&1; rc=$?
ms=$(( ($(date +%s%N) - start) / 1000000 ))
[ $rc = 0 ] && [ $ms -lt 2000 ] && pass "inbox answered in ${ms}ms with the first node frozen" || fail "inbox took ${ms}ms (exit $rc)"
start=$(date +%s%N)
AIVOICEBUS_ME=lab/reader AIVOICEBUS_URL="$(url 2),$(url 1)" AIVOICEBUS_TOKEN="$(tok)" "$NEW" status >/dev/null 2>&1; rc=$?
ms=$(( ($(date +%s%N) - start) / 1000000 ))
[ $rc = 0 ] && [ $ms -lt 2000 ] && pass "status answered in ${ms}ms with the first node frozen" || fail "status took ${ms}ms (exit $rc)"
start=$(date +%s%N)
M=$(AIVOICEBUS_ME=lab/sender AIVOICEBUS_URL="$(url 2),$(url 1),$(url 3)" AIVOICEBUS_TOKEN="$(tok)" "$NEW" send --to lab/reader "sent while node 2 is frozen" 2>&1)
ms=$(( ($(date +%s%N) - start) / 1000000 ))
has "a send still succeeds" "^sent " "$M"
[ $ms -lt 4000 ] && pass "send took ${ms}ms" || fail "send took ${ms}ms"
kill -CONT "$(serve_pid 2)"
eventually "node 2 catches up after it thaws" 20 bash -c "AIVOICEBUS_ME=lab/reader AIVOICEBUS_URL='$(url 2)' AIVOICEBUS_TOKEN='$(tok)' '$NEW' inbox --all | grep -q 'frozen'"

section "a node that loses its disk recovers from its peers"
nodectl 4 stop
rm -rf "$LAB/nodes/lab_n4/var/lib/aivoicebus"/*
want=$(records_of 1)
nodectl 4 start
eventually "node 4 is back" 15 curl -fsS "http://127.0.0.14:$PORT/healthz"
holds_at_least() { [ "$(records_of "$1")" -ge "$2" ]; }
eventually "node 4 holds everything again ($want records)" 25 holds_at_least 4 "$want"
M=$(AIVOICEBUS_ME=lab/sender AIVOICEBUS_URL="$(url 4)" AIVOICEBUS_TOKEN="$(tok)" "$NEW" send --to lab/reader "written to the recovered node" 2>&1)
has "and takes writes after recovering" "^sent " "$M"
eventually "its new record reaches node 1" 20 bash -c "AIVOICEBUS_ME=lab/reader AIVOICEBUS_URL='$(url 1)' AIVOICEBUS_TOKEN='$(tok)' '$NEW' inbox --all | grep -q 'recovered node'"
# shellcheck disable=SC2034 # the output is not inspected; the call only has to succeed against the recovered node
s4=$(AIVOICEBUS_URL="$(url 4)" AIVOICEBUS_TOKEN="$(tok)" "$NEW" mesh --json 2>&1)

section "the per-prompt hook: urgent only, shown once"
AIVOICEBUS_ME=lab/sender AIVOICEBUS_URL="$ALLURLS" AIVOICEBUS_TOKEN="$(tok)" "$NEW" send --to lab/claude.abcd --prio ask "just an ask" >/dev/null 2>&1
AIVOICEBUS_ME=lab/sender AIVOICEBUS_URL="$ALLURLS" AIVOICEBUS_TOKEN="$(tok)" "$NEW" send --to lab/claude.abcd --prio urgent "CI is red, hold merges" >/dev/null 2>&1
hook() { (cd "$LAB" && echo '{"session_id":"abcd-0001"}' | AIVOICEBUS_HOST=lab AIVOICEBUS_URL="$ALLURLS" AIVOICEBUS_TOKEN="$(tok)" HOME="$LAB/hookhome" "$NEW" hook --tool claude --event prompt 2>&1); }
mkdir -p "$LAB/hookhome"
out=$(hook); has "urgent shows on the next prompt" "CI is red, hold merges" "$out"; lacks "the ask waits" "just an ask" "$out"; has "behind the untrusted banner" "UNTRUSTED" "$out"
rm -f "$LAB/hookhome/.config/aivoicebus/prompt-check.json"
out=$(hook); [ -z "$out" ] && pass "and is not repeated" || fail "second prompt printed: $out"

section "an MCP session over real stdio"
exchange() {
  printf '%s\n' \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}' \
    '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"aivoicebus_send","arguments":{"to":"lab/claude.abcd","body":"from an MCP agent","dry_run":true}}}' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"aivoicebus_who","arguments":{}}}' |
    AIVOICEBUS_URL="$ALLURLS" AIVOICEBUS_TOKEN="$(tok)" AIVOICEBUS_HOST=lab "$NEW" mcp 2>"$LAB/mcp.err"
}
out=$(exchange); lines=$(printf '%s\n' "$out" | wc -l | tr -d ' ')
[ "$lines" = 4 ] && pass "four responses for four requests (the notification gets none)" || fail "$lines responses"
has "initialize answers" '"serverInfo":{"name":"aivoicebus"' "$out"
has "six tools are listed (one takes a send back)" 'aivoicebus_retract' "$out"
has "a dry-run send reports its reach" 'dry run: nothing was sent' "$out"
has "who lists real sessions" 'lab/claude.abcd' "$out"
[ ! -s "$LAB/mcp.err" ] && pass "nothing but protocol on stdout, nothing on stderr" || fail "stderr: $(cat "$LAB/mcp.err")"

section "export and restore into a new node"
fm_url() { AIVOICEBUS_URL="$(url 1)" fm "$@"; }
fm_url export --out "$LAB/board.jsonl" 2>"$LAB/export.err"; has "export reports what it wrote" "exported" "$(cat "$LAB/export.err")"
mkdir -p "$LAB/restored"
imp=$("$NEW" import --data "$LAB/restored" --node restored "$LAB/board.jsonl" 2>&1); has "import reads every record" "read [0-9]* record(s): [0-9]* added, 0 already there" "$imp"
cp "$TOKEN_FILE" "$LAB/restored.token"
"$NEW" serve --node restored --addr 127.0.0.30:$PORT --data "$LAB/restored" --token-file "$LAB/restored.token" >"$LAB/restored.log" 2>&1 &
RPID=$!
eventually "a node started on the restored directory is up" 10 curl -fsS "http://127.0.0.30:$PORT/healthz"
a=$(http "$TOKEN_FILE" "http://127.0.0.30:$PORT/v1/health" | grep -o '"messages":[0-9]*' | head -1); b=$(http "$TOKEN_FILE" "$(url 1)/v1/health" | grep -o '"messages":[0-9]*' | head -1)
[ "$a" = "$b" ] && pass "it holds the same messages as the live mesh ($a)" || fail "restored $a vs live $b"
kill "$RPID" 2>/dev/null

section "metrics"
m=$(http "$TOKEN_FILE" "$(url 1)/metrics")
has "node metrics are exposed" 'aivoicebus_messages' "$m"
has "peer lag is exposed" 'aivoicebus_peer_lag_records' "$m"
has "requests are counted per token, by fingerprint" 'aivoicebus_requests_total{token="shared-' "$m"

section "rotating the shared token with no flag day"
cp "$TOKEN_FILE" "$LAB/old.token"
out=$(deploy/rotate-token.sh status 2>&1); has "status before: phase none" "phase none" "$out"
out=$(deploy/rotate-token.sh add 2>&1); has "a dry run changes nothing" "dry run only" "$out"; [ ! -e "$TOKEN_FILE.next" ] && pass "no new token was made by the dry run" || fail "dry run made a token"
out=$(deploy/rotate-token.sh add --apply 2>&1); has "add: new token made, nodes restarted" "added. Every node accepts both tokens" "$out"
NEXT="$TOKEN_FILE.next"
for i in 1 2 3 4; do [ "$(code "$LAB/old.token" "$(url $i)/v1/who")" = 200 ] && [ "$(code "$NEXT" "$(url $i)/v1/who")" = 200 ] && pass "node $i accepts both tokens" || fail "node $i does not accept both"; done
M=$(AIVOICEBUS_ME=lab/sender AIVOICEBUS_URL="$(url 1)" AIVOICEBUS_TOKEN="$(cat "$NEXT")" "$NEW" send --to lab/reader "sent with the NEW token during rotation" 2>&1); has "a send with the new token works" "^sent " "$M"
eventually "and replicates to node 3" 20 bash -c "AIVOICEBUS_ME=lab/reader AIVOICEBUS_URL='$(url 3)' AIVOICEBUS_TOKEN='$(cat "$LAB/old.token")' '$NEW' inbox --all | grep -q 'NEW token during'"
out=$(deploy/rotate-token.sh promote --apply 2>&1); has "promote: nodes now talk with the new token" "promoted" "$out"
M=$(AIVOICEBUS_ME=lab/sender AIVOICEBUS_URL="$(url 2)" AIVOICEBUS_TOKEN="$(cat "$LAB/old.token")" "$NEW" send --to lab/reader "sent with the OLD token after promote" 2>&1); has "old-token machines still work" "^sent " "$M"
eventually "and it replicates under the new inter-node token" 20 bash -c "AIVOICEBUS_ME=lab/reader AIVOICEBUS_URL='$(url 4)' AIVOICEBUS_TOKEN='$(cat "$NEXT")' '$NEW' inbox --all | grep -q 'OLD token after'"
out=$(deploy/rotate-token.sh retire --apply 2>&1); has "retire: the old token is gone" "retired. The old token is gone" "$out"
for i in 1 2 3 4; do [ "$(code "$LAB/old.token" "$(url $i)/v1/who")" = 401 ] && pass "node $i refuses the old token" || fail "node $i still accepts the old token"; done
[ ! -e "$NEXT" ] && pass "the staging file is gone" || fail "token.next left behind"
[ "$(code "$TOKEN_FILE" "$(url 1)/v1/who")" = 200 ] && pass "this machine's token file is now the new token" || fail "local token file not switched"
M=$(AIVOICEBUS_ME=lab/sender AIVOICEBUS_URL="$(url 3)" AIVOICEBUS_TOKEN="$(tok)" "$NEW" send --to lab/reader "sent after the old token was retired" 2>&1); has "sending after retirement works" "^sent " "$M"
eventually "and replication still works between nodes" 20 bash -c "AIVOICEBUS_ME=lab/reader AIVOICEBUS_URL='$(url 1)' AIVOICEBUS_TOKEN='$(tok)' '$NEW' inbox --all | grep -q 'after the old token was retired'"
out=$(AIVOICEBUS_URL="$ALLURLS" AIVOICEBUS_TOKEN="$(tok)" "$NEW" mesh 2>&1); [ $? = 0 ] && pass "the mesh is healthy after rotation" || { fail "mesh unhealthy after rotation"; echo "$out"; }
grep -q . "$HOME/.config/aivoicebus/token.nodes" 2>/dev/null && fail "rotation payload left behind" || pass "no rotation leftovers"

section "a machine's own token: bound to that machine and revocable alone"
MT="$HOME/.config/aivoicebus/machine-tokens/lab"
out=$(deploy/machine-token.sh add lab 2>&1); has "a dry run says what it would do" "dry run only" "$out"; [ ! -e "$MT" ] && pass "and makes nothing" || fail "dry run made a token"
out=$(deploy/machine-token.sh add lab --apply 2>&1); has "add: the machine gets a token" "lab has its own token" "$out"
[ "$(stat -c %a "$MT")" = 600 ] && pass "its file is mode 600" || fail "machine token mode $(stat -c %a "$MT")"
lacks "and the token is never printed" "$(cat "$MT")" "$out"
for i in 1 2 3 4; do [ "$(code "$MT" "$(url $i)/v1/who")" = 200 ] && pass "node $i accepts it" || fail "node $i rejects the machine token"; done
M=$(AIVOICEBUS_ME=lab/agent AIVOICEBUS_URL="$(url 1)" AIVOICEBUS_TOKEN="$(cat "$MT")" "$NEW" send --to lab/reader "from the machine itself" 2>&1); has "it sends as its own machine" "^sent " "$M"
before=$(cat "$TOKEN_FILE" "$MT" | sha256sum; ls -A "$HOME/.config/aivoicebus")
out=$(AIVOICEBUS_BIN="$NEW" deploy/rotate-token.sh --check 2>&1); rc=$?
[ $rc = 0 ] && has "rotate-token --check: every client machine is on its own token, so a rotation cuts nobody off" "no machine would be cut off" "$out" || fail "rotate-token --check exit $rc: $out"
has "and the report shows lab on its machine token" "^  lab .* machine  *made" "$out"
M=$(AIVOICEBUS_ME=labtwo/agent AIVOICEBUS_URL="$(url 2)" AIVOICEBUS_TOKEN="$(tok)" "$NEW" send --to lab/reader "from a machine on the shared token" 2>&1); has "a machine sends with the shared token" "^sent " "$M"
out=$(AIVOICEBUS_BIN="$NEW" deploy/rotate-token.sh --check 2>&1); rc=$?
[ $rc = 1 ] && has "rotate-token --check refuses, naming the machine on the shared token" "would cut off.*labtwo" "$out" || fail "rotate-token --check exit $rc: $out"
[ "$before" = "$(cat "$TOKEN_FILE" "$MT" | sha256sum; ls -A "$HOME/.config/aivoicebus")" ] && pass "rotate-token --check changes no file" || fail "rotate-token --check changed a file"
M=$(AIVOICEBUS_ME=other/agent AIVOICEBUS_URL="$(url 1)" AIVOICEBUS_TOKEN="$(cat "$MT")" "$NEW" send --to lab/reader "pretending to be another machine" 2>&1); has "it cannot send as another machine" "may only act as lab" "$M"
M=$(AIVOICEBUS_ME=other/agent AIVOICEBUS_URL="$(url 2)" AIVOICEBUS_TOKEN="$(cat "$MT")" "$NEW" inbox 2>&1); has "or read another machine's mail" "may only act as lab" "$M"
eventually "its message replicates (nodes talk with the shared token)" 20 bash -c "AIVOICEBUS_ME=lab/reader AIVOICEBUS_URL='$(url 4)' AIVOICEBUS_TOKEN='$(tok)' '$NEW' inbox --all | grep -q 'from the machine itself'"
deploy/install.sh --apply --tokens-only n3 >/dev/null 2>&1
[ "$(code "$MT" "$(url 3)/v1/who")" = 200 ] && pass "a re-install keeps the machine token" || fail "machine token lost by a re-install"
out=$(deploy/machine-token.sh list 2>&1); has "list shows it" "lab" "$out"
cp "$MT" "$LAB/old-machine.token"
out=$(deploy/machine-token.sh revoke lab --apply 2>&1); has "revoke: gone from every node" "revoked: no node accepts lab's token" "$out"
for i in 1 2 3 4; do [ "$(code "$LAB/old-machine.token" "$(url $i)/v1/who")" != 200 ] && pass "node $i refuses the revoked token" || fail "node $i still accepts it"; done
[ "$(code "$TOKEN_FILE" "$(url 1)/v1/who")" = 200 ] && pass "the shared token is unaffected" || fail "the shared token broke"
[ ! -e "$MT" ] && pass "the local copy is deleted" || fail "revoked token file left behind"

section "canary rollout: a good release goes to every node"
REL1=$(release_dir new 6.0.1 "$NEW")
out=$(deploy/rollout.sh "$REL1" --unsigned 2>&1); has "a dry run lists the plan" "dry run only" "$out"; [ "$(version_of 1)" = 6.0.0 ] && pass "and changes nothing" || fail "dry run changed node 1"
# shellcheck disable=SC2034 # not read today
before_pid=$(node_pid 2)
out=$(deploy/rollout.sh "$REL1" --unsigned --apply --soak 3 2>&1); rc=$?
[ $rc = 0 ] && pass "rollout exits 0" || { fail "rollout exit $rc"; echo "$out" | tail -25; }
has "the canary soaked" "canary stable" "$out"
for i in 1 2 3 4; do [ "$(version_of $i)" = 6.0.1 ] && pass "node $i runs 6.0.1" || fail "node $i runs $(version_of $i)"; done
[ -x "$LAB/nodes/lab_n1/usr/local/bin/aivoicebus.prev" ] && pass "the previous binary is kept for rollback" || fail "no aivoicebus.prev"

section "canary rollout: a release that will not start is rolled back and stops the rollout"
REL2=$(release_dir broken 6.0.2 "$LAB/bins/broken")
pids=""; for i in 2 3 4; do pids="$pids $(node_pid $i)"; done
out=$(deploy/rollout.sh "$REL2" --unsigned --apply --soak 2 2>&1); rc=$?
[ $rc != 0 ] && pass "rollout exits non-zero" || fail "a broken release 'succeeded'"
has "the canary is rolled back" "ROLLING BACK n1" "$out"; has "and healthy again" "rolled back and healthy" "$out"
has "the rollout says it stopped" "rollout stopped at n1" "$out"
[ "$(version_of 1)" = 6.0.1 ] && pass "the canary runs 6.0.1 again" || fail "the canary runs $(version_of 1)"
now=""; for i in 2 3 4; do now="$now $(node_pid $i)"; done
[ "$pids" = "$now" ] && pass "nodes 2-4 were never touched (same processes)" || fail "other nodes were restarted: $pids -> $now"

section "canary rollout: a release that crash-loops is caught by the soak"
REL3=$(release_dir flapping 6.0.3 "$LAB/bins/flapping")
out=$(deploy/rollout.sh "$REL3" --unsigned --apply --soak 6 --expect 6.0.3 2>&1); rc=$?
[ $rc != 0 ] && pass "rollout exits non-zero" || fail "a flapping release 'succeeded'"
has "the soak noticed the restarts" "restarted [0-9]* -> [0-9]* time" "$out"; has "and rolled the canary back" "rolled back and healthy" "$out"
[ "$(version_of 1)" = 6.0.1 ] && pass "the canary runs 6.0.1 again" || fail "the canary runs $(version_of 1)"

section "canary rollout: a release that starts but is not what it claims is rolled back too"
REL4=$(release_dir mislabelled 6.0.4 "$LAB/bins/real605")   # the manifest says 6.0.4; the binary says 6.0.5
pids=""; for i in 2 3 4; do pids="$pids $(node_pid $i)"; done
out=$(deploy/rollout.sh "$REL4" --unsigned --apply --soak 1 2>&1); rc=$?
[ $rc != 0 ] && pass "rollout exits non-zero" || fail "a mislabelled release 'succeeded'"
has "verification names the mismatch" "version '6.0.5' (want 6.0.4)" "$out"
has "the canary is rolled back" "rolled back and healthy" "$out"
has "the rollout says it stopped" "rollout stopped at n1" "$out"
now=""; for i in 2 3 4; do now="$now $(node_pid $i)"; done
[ "$pids" = "$now" ] && pass "nodes 2-4 were never touched (same processes)" || fail "other nodes were restarted: $pids -> $now"

section "adding a fifth node"
mkdir -p "$LAB/nodes/lab_n5"
before=$(records_of 1)
out=$(AIVOICEBUS_BIN="$NEW" deploy/add-node.sh n5 lab_n5 127.0.0.15 2>&1); has "dry run first" "dry run only" "$out"
grep -q n5 "$LAB/nodes.conf" && fail "dry run edited nodes.conf" || pass "and nodes.conf is untouched"
out=$(AIVOICEBUS_BIN="$NEW" deploy/add-node.sh n5 lab_n5 127.0.0.15 --apply 2>&1); rc=$?
[ $rc = 0 ] && pass "add-node exits 0" || { fail "add-node exit $rc"; echo "$out" | tail -20; }
has "the new node catches up (the log holds a retraction: it is counted by the version vector)" "in step with the others" "$out"
same_as_node1() { [ "$(records_of 5)" = "$(records_of 1)" ]; }
eventually "the new node holds as many records as node 1" 20 same_as_node1
has "and counts the retraction it recovered" "^aivoicebus_retracted_messages 1$" "$(http "$TOKEN_FILE" "$(url 5)/metrics")"
has "the next steps name the network rule" "Network: allow tcp:" "$out"
for i in 1 2 3 4; do grep -q "127.0.0.15" "$LAB/nodes/lab_n$i/etc/aivoicebus/node.env" && pass "node $i lists n5 as a peer" || fail "node $i does not know n5"; done
M=$(AIVOICEBUS_ME=lab/sender AIVOICEBUS_URL="http://127.0.0.15:$PORT" AIVOICEBUS_TOKEN="$(tok)" "$NEW" send --to lab/reader "written on the new node" 2>&1); has "the new node takes writes" "^sent " "$M"
eventually "and its record reaches node 2" 20 bash -c "AIVOICEBUS_ME=lab/reader AIVOICEBUS_URL='$(url 2)' AIVOICEBUS_TOKEN='$(tok)' '$NEW' inbox --all | grep -q 'written on the new node'"
out=$(deploy/add-node.sh n5 lab_n6 127.0.0.16 2>&1); [ $? != 0 ] && has "a duplicate name is refused" "already in" "$out" || fail "duplicate node accepted"

section "a mesh that cannot be reached completely is refused, not half-changed"
REL6=$(release_dir v606 6.0.6 "$LAB/bins/real606")
touch "$LAB/nodes/lab_n4/down"
out=$(deploy/rotate-token.sh add --apply 2>&1); rc=$?
[ $rc != 0 ] && has "rotate-token add refuses, naming the node" "cannot reach: n4" "$out" || fail "rotate-token add went ahead with a node down"
[ ! -e "$TOKEN_FILE.next" ] && pass "and made no new token" || fail "a new token was made"
out=$(deploy/machine-token.sh add mx --apply 2>&1); rc=$?
[ $rc != 0 ] && [ ! -e "$HOME/.config/aivoicebus/machine-tokens/mx" ] && has "machine-token add refuses and makes nothing" "cannot reach: n4" "$out" || fail "machine-token add went ahead with a node down"
out=$(deploy/rollout.sh "$REL6" --unsigned --apply 2>&1); rc=$?
[ $rc != 0 ] && has "rollout refuses before touching anything" "cannot reach: n4" "$out" || fail "rollout went ahead with a node down"
[ "$(version_of 1)" = 6.0.1 ] && pass "no node was upgraded" || fail "node 1 runs $(version_of 1)"
out=$(deploy/install.sh --apply --tokens-only n4 2>&1); rc=$?
[ $rc = 3 ] && has "install.sh says it skipped a node and ends with status 3" "NOT DONE: skipped unreachable node(s): n4" "$out" || fail "install.sh exit $rc for an unreachable node"
rm -f "$LAB/nodes/lab_n4/down"

section "the token that gets pushed must be the shared one"
deploy/machine-token.sh add mx --apply >/dev/null 2>&1
MX="$HOME/.config/aivoicebus/machine-tokens/mx"
[ -s "$MX" ] && pass "a machine token exists to misuse" || fail "no machine token"
sum_n1() { sha256sum "$LAB/nodes/lab_n1/etc/aivoicebus/token" | cut -c1-16; }
before=$(sum_n1)
out=$(AIVOICEBUS_TOKEN_FILE="$MX" deploy/install.sh --apply --tokens-only n1 2>&1); rc=$?
[ $rc != 0 ] && has "a machine token in place of the shared token is refused" "same token appears twice" "$out" || fail "a machine token was pushed as the shared token"
head -c 48 /dev/zero | tr '\0' 'z' >"$LAB/stale.token"
out=$(AIVOICEBUS_TOKEN_FILE="$LAB/stale.token" deploy/install.sh --apply --tokens-only n1 2>&1); rc=$?
[ $rc != 0 ] && has "a token the node does not accept is refused" "does not accept" "$out" || fail "a token the node rejects was pushed"
[ "$(sum_n1)" = "$before" ] && pass "node 1's token file is untouched by both" || fail "node 1's token file changed"
out=$(AIVOICEBUS_URL="$(url 1)" AIVOICEBUS_TOKEN="$(cat "$MX")" "$NEW" export --out "$LAB/feed.jsonl" 2>&1); rc=$?
[ $rc != 0 ] && has "a machine token cannot take the whole feed" "machine token" "$out" || fail "a machine token exported the board"
[ ! -e "$LAB/feed.jsonl" ] && pass "and no file was left behind" || fail "export left a file"

section "a revoke that fails half way can be finished"
cp "$LAB/nodes/lab_n3/usr/local/bin/aivoicebus" "$LAB/n3.good"
printf '#!/bin/sh\nexit 1\n' >"$LAB/n3.broken"; install -m 755 "$LAB/n3.broken" "$LAB/nodes/lab_n3/usr/local/bin/aivoicebus"
cp -p "$MX" "$LAB/mx.token"
out=$(deploy/machine-token.sh revoke mx --apply 2>&1); rc=$?
[ $rc != 0 ] && has "revoke reports that it did not finish" "revoke did not finish" "$out" || fail "revoke claimed success with a broken node"
lacks "and does not claim the token is gone" "revoked: no node accepts" "$out"
# shellcheck disable=SC2010 # the test is about a hidden marker name, which a glob would not list
grep -q '^\.revoke-pending-mx$' <<<"$(ls -A "$HOME/.config/aivoicebus/machine-tokens/")" && pass "the token is kept, hidden, until it is finished" || fail "no revoke-pending file"
[ "$(code "$LAB/mx.token" "$(url 1)/v1/who")" != 200 ] && pass "node 1 (done before the failure) refuses it" || fail "node 1 still accepts it"
out=$(deploy/machine-token.sh list 2>&1); has "list says a revoke is unfinished" "revoke unfinished" "$out"
state_before=$(ls -A "$HOME/.config/aivoicebus/machine-tokens/")
out=$(deploy/machine-token.sh revoke mx 2>&1); [ "$state_before" = "$(ls -A "$HOME/.config/aivoicebus/machine-tokens/")" ] && pass "a dry run changes no file names" || fail "a dry run renamed the token file"
install -m 755 "$LAB/n3.good" "$LAB/nodes/lab_n3/usr/local/bin/aivoicebus"   # the node is repaired
out=$(deploy/machine-token.sh revoke mx --apply 2>&1); rc=$?
[ $rc = 0 ] && has "running it again finishes the job" "revoked: no node accepts mx's token any more" "$out" || { fail "revoke did not resume (exit $rc)"; echo "$out" | tail -8; }
for i in 1 2 3 4 5; do [ "$(code "$LAB/mx.token" "$(url $i)/v1/who")" != 200 ] && pass "node $i refuses the revoked token" || fail "node $i still accepts it"; done
# shellcheck disable=SC2010 # as above: hidden marker names
grep -q revoke <<<"$(ls -A "$HOME/.config/aivoicebus/machine-tokens/")" && fail "revoke leftovers" || pass "no leftovers"

section "a canary's earlier restart does not fail a good release"
kill -KILL "$(serve_pid 1)"
restarted_once() { [ "$(cat "$LAB/nodes/lab_n1/run/nrestarts" 2>/dev/null)" = 1 ]; }
eventually "the supervisor restarted node 1 once" 10 restarted_once
out=$(deploy/rollout.sh "$REL6" --unsigned --apply --soak 3 2>&1); rc=$?
[ $rc = 0 ] && pass "the rollout succeeds (the baseline is taken after the install)" || { fail "rollout exit $rc"; echo "$out" | tail -15; }
has "the canary soaked" "canary stable" "$out"
for i in 1 2 3 4 5; do [ "$(version_of $i)" = 6.0.6 ] && pass "node $i runs 6.0.6" || fail "node $i runs $(version_of $i)"; done

section "running the same rollout again changes nothing"
pids=""; for i in 1 2 3 4 5; do pids="$pids $(node_pid $i)"; done
out=$(deploy/rollout.sh "$REL6" --unsigned --apply --soak 2 2>&1); rc=$?
[ $rc = 0 ] && pass "exit 0" || fail "exit $rc"
[ "$(printf '%s' "$out" | grep -c 'already on 6.0.6')" = 5 ] && pass "every node is skipped" || { fail "not every node was skipped"; echo "$out" | tail -12; }
now=""; for i in 1 2 3 4 5; do now="$now $(node_pid $i)"; done
[ "$pids" = "$now" ] && pass "and not one was restarted" || fail "nodes were restarted: $pids -> $now"

section "the soak is a whole number of seconds"
for bad in 30s 0 -5; do
  out=$(deploy/rollout.sh "$REL6" --unsigned --apply --soak "$bad" 2>&1); rc=$?
  [ $rc = 2 ] && has "--soak '$bad' is a usage error" "whole number of seconds" "$out" || fail "--soak '$bad' exit $rc"
done

section "release.sh signs what the binaries are, at their commit, not what the checkout says"
G="$LAB/relrepo"; mkdir -p "$G/scripts"; cp scripts/release.sh scripts/compat-record.sh "$G/scripts/"
printf '%s\n' "# lab fixture: the releases a signed release must have been checked against" 0.6.0 0.6.1 0.6.2 0.7.0 0.8.1 >"$G/scripts/compat-releases.txt"
git -C "$G" init -q && git -C "$G" config user.email lab@lab.invalid && git -C "$G" config user.name lab
echo 1.2.3 >"$G/VERSION"; git -C "$G" add -A && git -C "$G" commit -qm "release 1.2.3"
C1=$(git -C "$G" rev-parse HEAD); S1=$(git -C "$G" rev-parse --short=9 HEAD)
echo 1.3.0 >"$G/VERSION"; git -C "$G" commit -qam "another session bumps VERSION"   # main moved on after the build
stamped() { CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w -X $PKG.Release=$1 -X $PKG.Commit=$2" -o "$3" . ; }
stamped 1.2.3 "$S1" "$LAB/bins/r123" && stamped 1.3.0 "$S1" "$LAB/bins/r130" && stamped 1.2.3 "$S1-dirty" "$LAB/bins/rdirty" &&
  stamped 1.2.3 0123abcde "$LAB/bins/rforeign" || { echo "build failed"; exit 1; }
relset() { # relset <dir> <binary for linux-amd64> [binary for darwin-amd64]
  mkdir -p "$1"; cp "$2" "$1/aivoicebus-linux-amd64"; cp "$2" "$1/aivoicebus-darwin-arm64"; cp "${3:-$2}" "$1/aivoicebus-darwin-amd64"
}
rel() { "$G/scripts/release.sh" "$@" 2>&1; }
relset "$LAB/s-ok" "$LAB/bins/r123"
has "stamp reads the release and the full commit from the binaries" "^1.2.3 $C1$" "$(rel stamp "$LAB/s-ok")"
"$LAB/bins/r123" release keygen >/dev/null 2>&1   # a throwaway key in the lab's HOME
T1=$(git -C "$G" rev-parse "$C1^{tree}")
SHA1=$(scripts/compat-record.sh sha "$LAB/s-ok/aivoicebus-linux-amd64")
# mkrec <file> [key=value overrides...]: a record that passes for s-ok, with some fields changed
mkrec() {
  local f=$1; shift
  { echo "commit=$C1"; echo "tree=$T1"; echo "binary_sha256=$SHA1"; echo "result=pass"; echo "checks=120"; echo "scenarios=10"
    echo "old=0.6.0 0.6.1 0.6.2 0.7.0 0.8.1"; } >"$f"
  local kv k
  for kv in "$@"; do k=${kv%%=*}; sed "s/^$k=.*/$kv/" "$f" >"$f.n" && mv "$f.n" "$f"; done
}
refused() { # refused <what> <pattern> <dir>
  out=$(rel sign "$3"); [ $? != 0 ] && has "$1" "$2" "$out" || fail "$1: signed anyway"
  [ ! -e "$3/manifest.json" ] && pass "and nothing is signed" || fail "a manifest was written: $1"
}
echo 0.8.1 >"$G/scripts/compat-releases.txt" # a signer's edited checkout must not weaken the gate: the list is read from the signed commit
refused "sign refuses a release the mixed-version gate has not passed" "mixed-version gate has not passed" "$LAB/s-ok"
mkrec "$LAB/s-ok/compat-pass.txt" result=fail;                refused "a record that says fail does not count" "does not say result=pass" "$LAB/s-ok"
mkrec "$LAB/s-ok/compat-pass.txt" commit="$(git -C "$G" rev-parse HEAD)"; refused "a pass for another commit does not count" "is for commit" "$LAB/s-ok"
mkrec "$LAB/s-ok/compat-pass.txt" tree=0123456789012345678901234567890123456789; refused "a pass for another tree does not count" "is for tree" "$LAB/s-ok"
mkrec "$LAB/s-ok/compat-pass.txt" binary_sha256=0000000000000000000000000000000000000000000000000000000000000000; refused "a pass for another binary does not count" "tested a binary" "$LAB/s-ok"
mkrec "$LAB/s-ok/compat-pass.txt" "old=0.8.1";                refused "a run against only the newest old release does not count" "did not include release 0.6.0" "$LAB/s-ok"
mkrec "$LAB/s-ok/compat-pass.txt" checks=9;                   refused "a run with too few checks does not count" "the record has '9' checks" "$LAB/s-ok"
mkrec "$LAB/s-ok/compat-pass.txt" scenarios=3;                refused "a truncated run does not count" "has '3' scenarios" "$LAB/s-ok"
for bad in 0 false " " "short" "$(printf '\001\002\003\004\005\006\007\010\011\012\013\014')"; do
  out=$(AIVOICEBUS_SKIP_COMPAT="$bad" rel sign "$LAB/s-ok"); [ $? != 0 ] && has "override '$(printf '%s' "$bad" | tr -d '[:cntrl:]')' is not a reason" "is a reason, at least 10" "$out" || fail "override '$bad' signed"
done
[ ! -e "$LAB/s-ok/manifest.json" ] && pass "and nothing is signed" || fail "a manifest was written for a non-reason override"
relset "$LAB/s-skip" "$LAB/bins/r123"
# (the lab's throwaway key is not the compiled-in one, so `release verify` ends sign with an error
# after the manifest is written; like the other sign checks here, look at what it did, not its exit code)
out=$(AIVOICEBUS_SKIP_COMPAT="  the gate host is down; rolling back a bad release  " rel sign "$LAB/s-skip")
has "the documented override signs" "packed and signed release 1.2.3" "$out"
has "and warns loudly, with the reason" "WARNING: signing release 1.2.3 WITHOUT a compat.sh pass for commit $S1" "$out"
has "the reason is printed, trimmed" "reason given (AIVOICEBUS_SKIP_COMPAT): the gate host is down; rolling back" "$out"
has "the waiver is written into the release dir" "commit=$C1 waived compat gate .*: the gate host is down" "$(cat "$LAB/s-skip/OVERRIDES" 2>/dev/null)"
echo "lab notes" >"$LAB/notes.md"
out=$(NOTES="$LAB/notes.md" rel github "$LAB/s-skip")
has "github puts the waiver at the top of the release notes" "^\*\*Mixed-version gate WAIVED:\*\* .*the gate host is down" "$(head -1 "$LAB/gh.notes" 2>/dev/null)"
has "above the notes themselves" "lab notes" "$(cat "$LAB/gh.notes" 2>/dev/null)"
has "the waiver file is attached to the release" "/s-skip/OVERRIDES$" "$(cat "$LAB/gh.args" 2>/dev/null)"
rm -f "$LAB/gh.args" "$LAB/gh.notes"
relset "$LAB/s-nogate" "$LAB/bins/r123"; cp "$LAB/s-skip/manifest.json" "$LAB/s-skip/manifest.sig" "$LAB/s-nogate/"
out=$(NOTES="$LAB/notes.md" rel github "$LAB/s-nogate"); [ $? != 0 ] && has "github refuses a release dir with neither a record nor a waiver" "no passing compat record and no recorded waiver" "$out" || fail "github released without a gate"
[ ! -e "$LAB/gh.args" ] && pass "and gh was never called" || fail "gh was called for an ungated release"
mkrec "$LAB/s-ok/compat-pass.txt"
scripts/compat-record.sh check "$LAB/s-ok/compat-pass.txt" "$C1" "$T1" "$SHA1" 0.6.0 0.8.1 >/dev/null 2>&1 && pass "a record for the binaries' code passes the check" || fail "a good record was refused"
# the writer: compat.sh's begin and finish, in a scratch repo (a stub for a compat run)
F="$LAB/finrepo"; mkdir -p "$F"; git -C "$F" init -q && git -C "$F" config user.email lab@lab.invalid && git -C "$F" config user.name lab
echo x >"$F/f"; git -C "$F" add -A && git -C "$F" commit -qm one; printf 'bin' >"$LAB/finbin"
cr() { (cd "$F" && "$OLDPWD/scripts/compat-record.sh" "$@" 2>&1); }
echo stale >"$LAB/fin.rec"; cr begin "$LAB/fin.rec"
[ ! -e "$LAB/fin.rec" ] && pass "begin removes a stale record" || fail "a stale record survived begin"
echo dirty >>"$F/f"
out=$(cr finish "$LAB/fin.rec" 0 60 10 "$LAB/finbin" 0.8.1); [ $? != 0 ] && [ ! -e "$LAB/fin.rec" ] && has "a dirty tree never produces a record" "uncommitted changes" "$out" || fail "a record for a dirty tree: $out"
git -C "$F" checkout -q f
out=$(cr finish "$LAB/fin.rec" 2 60 10 "$LAB/finbin" 0.8.1); [ $? != 0 ] && [ ! -e "$LAB/fin.rec" ] && has "a run with failures writes none" "2 failing checks" "$out" || fail "a record for a failed run: $out"
out=$(cr finish "$LAB/fin.rec" 0 60 10 "$LAB/finbin" 0.7.0 0.8.1); rc=$?
[ $rc = 0 ] && has "a clean full pass writes the record" "^commit=$(git -C "$F" rev-parse HEAD)$" "$(cat "$LAB/fin.rec")" || fail "no record for a clean pass: $out"
has "with the tree" "^tree=$(git -C "$F" rev-parse 'HEAD^{tree}')$" "$(cat "$LAB/fin.rec")"
has "the binary's sha256" "^binary_sha256=$(scripts/compat-record.sh sha "$LAB/finbin")$" "$(cat "$LAB/fin.rec")"
has "and the releases it ran against" "^old=0.7.0 0.8.1$" "$(cat "$LAB/fin.rec")"
rm -rf "$F/.git"
out=$(cr finish "$LAB/fin2.rec" 0 60 10 "$LAB/finbin" 0.8.1); [ $? != 0 ] && has "without .git the commit and tree must be passed" "pass COMPAT_COMMIT and COMPAT_TREE" "$out" || fail "a record without git: $out"
out=$(cd "$F" && COMPAT_COMMIT="$C1" "$OLDPWD/scripts/compat-record.sh" finish "$LAB/fin2.rec" 0 60 10 "$LAB/finbin" 0.8.1 2>&1); [ $? != 0 ] && [ ! -e "$LAB/fin2.rec" ] && pass "a commit alone is not enough" || fail "COMPAT_COMMIT without a tree: $out"
out=$(cd "$F" && COMPAT_COMMIT="$C1" COMPAT_TREE="$T1" "$OLDPWD/scripts/compat-record.sh" finish "$LAB/fin2.rec" 0 60 10 "$LAB/finbin" 0.8.1 2>&1); [ $? = 0 ] && has "COMPAT_COMMIT with COMPAT_TREE records both" "^tree=$T1$" "$(cat "$LAB/fin2.rec")" || fail "no record with both: $out"
# compat.sh itself removes the stale record first (relative to where it is run) and writes nothing when it dies early
mkdir -p "$LAB/cwd"; echo stale >"$LAB/cwd/stale.rec"
out=$(cd "$LAB/cwd" && "$REPO/scripts/compat.sh" --new "$LAB/bins/r123" --record stale.rec --dir "$LAB/nodir" --old 0.9.9 2>&1); rc=$?
[ $rc != 0 ] && [ ! -e "$LAB/cwd/stale.rec" ] && [ ! -e "$REPO/stale.rec" ] && pass "compat.sh removes a stale record (relative to the caller's directory) and a failed run writes none" || fail "compat.sh left a record or failed oddly (rc $rc): $out"
out=$(rel sign "$LAB/s-ok")
has "sign says the checkout's VERSION is not what it signs" "VERSION says 1.3.0; signing what the binaries are: 1.2.3" "$out"
has "the manifest says the binaries' release" '"version": "1.2.3"' "$(cat "$LAB/s-ok/manifest.json" 2>/dev/null)"
has "and their commit" "\"commit\": \"$S1\"" "$(cat "$LAB/s-ok/manifest.json" 2>/dev/null)"
echo "lab notes" >"$LAB/notes.md"
out=$(NOTES="$LAB/notes.md" rel github "$LAB/s-ok")
has "github names the release after the binaries" "^v1.2.3$" "$(cat "$LAB/gh.args" 2>/dev/null)"
has "and attaches the pass record" "/compat-pass.txt$" "$(cat "$LAB/gh.args" 2>/dev/null)"
has "and says in the notes that the gate passed" "^Mixed-version gate: passed" "$(head -1 "$LAB/gh.notes" 2>/dev/null)"
[ ! -e "$LAB/s-ok/OVERRIDES" ] && pass "no waiver is left in a gated release dir" || fail "OVERRIDES in a gated release"
has "and creates it at their full commit" "^$C1$" "$(sed -n '/^--target$/{n;p}' "$LAB/gh.args" 2>/dev/null)"
lacks "never at main" "^main$" "$(cat "$LAB/gh.args" 2>/dev/null)"
rm -f "$LAB/gh.args" "$LAB/gh.notes"
relset "$LAB/s-mixed" "$LAB/bins/r123" "$LAB/bins/new"
out=$(rel sign "$LAB/s-mixed"); [ $? != 0 ] && has "binaries built apart are refused" "were not built together" "$out" || fail "mixed binaries signed"
[ ! -e "$LAB/s-mixed/manifest.json" ] && pass "and nothing is signed" || fail "a manifest was written for mixed binaries"
relset "$LAB/s-ver" "$LAB/bins/r130"
out=$(rel sign "$LAB/s-ver"); [ $? != 0 ] && has "a release number VERSION at the commit does not say is refused" "VERSION at their commit $S1 says 1.2.3" "$out" || fail "1.3.0 stamped on a 1.2.3 commit was signed"
relset "$LAB/s-dirty" "$LAB/bins/rdirty"
out=$(rel sign "$LAB/s-dirty"); [ $? != 0 ] && has "a dirty build is refused" "not a clean commit" "$out" || fail "a dirty build was signed"
relset "$LAB/s-dirty2" "$LAB/bins/r123" "$LAB/bins/rdirty"   # only the darwin-amd64 slot was built from a modified tree
out=$(rel sign "$LAB/s-dirty2"); [ $? != 0 ] && has "a dirty binary in a slot that cannot run here is refused too" "darwin-amd64 was built from a modified tree" "$out" || fail "a dirty non-host binary was signed"
[ ! -e "$LAB/s-dirty2/manifest.json" ] && pass "and nothing is signed" || fail "a manifest was written with a dirty binary in it"
relset "$LAB/s-foreign" "$LAB/bins/rforeign"
out=$(rel sign "$LAB/s-foreign"); [ $? != 0 ] && has "a commit this checkout lacks is refused (fetch first)" "is not in this checkout" "$out" || fail "an unknown commit went to github"
relset "$LAB/s-mis" "$LAB/bins/r123"   # 1.2.3 binaries under a manifest packed as 1.3.0: the incident
"$LAB/bins/r123" release pack --dir "$LAB/s-mis" --version 1.3.0 >/dev/null 2>&1
out=$(NOTES="$LAB/notes.md" rel github "$LAB/s-mis"); [ $? != 0 ] && has "a manifest that disagrees with the binaries is refused" "the manifest says release '1.3.0'" "$out" || fail "a mislabelled manifest went to github"
[ ! -e "$LAB/gh.args" ] && pass "and gh was never called for a refused release" || fail "gh was called: $(cat "$LAB/gh.args")"

section "release.sh: the first release has no older one to test against, and attach signs a draft, never a published release"
G2="$LAB/relrepo2"; mkdir -p "$G2/scripts"; cp scripts/release.sh scripts/compat-record.sh "$G2/scripts/"
printf '%s\n' "# no older release exists yet" >"$G2/scripts/compat-releases.txt"
git -C "$G2" init -q && git -C "$G2" config user.email lab@lab.invalid && git -C "$G2" config user.name lab
echo 1.2.3 >"$G2/VERSION"; git -C "$G2" add -A && git -C "$G2" commit -qm "the first release"
S2=$(git -C "$G2" rev-parse --short=9 HEAD)
stamped 1.2.3 "$S2" "$LAB/bins/first" || { echo "build failed"; exit 1; }
rel2() { "$G2/scripts/release.sh" "$@" 2>&1; }
relset "$LAB/s-first" "$LAB/bins/first"
out=$(rel2 sign "$LAB/s-first")
has "a first release signs with no compat record" "mixed-version gate not applicable: no release older than 1.2.3" "$out"
has "and is packed" "packed and signed release 1.2.3" "$out"
[ ! -e "$LAB/s-first/OVERRIDES" ] && pass "without any waiver on file" || fail "a waiver was written for a release that needed none"
rm -f "$LAB/gh.args" "$LAB/gh.notes" "$LAB/gh.calls"
out=$(NOTES="$LAB/notes.md" rel2 github "$LAB/s-first")
has "github says the gate is not applicable, not passed" "^Mixed-version gate: not applicable .no older release.\." "$(head -1 "$LAB/gh.notes" 2>/dev/null)"
rm -f "$LAB/gh.args" "$LAB/gh.notes" "$LAB/gh.calls"

rel sums "$LAB/s-ok" >/dev/null
has "sums writes one line per binary" "^3$" "$(wc -l <"$LAB/s-ok/SHA256SUMS" | tr -d ' ')"
has "sorted by name, in the shape sha256sum prints" "  aivoicebus-darwin-amd64$" "$(sed -n 1p "$LAB/s-ok/SHA256SUMS")"
has "and it is the same file every time" "^$(cat "$LAB/s-ok/SHA256SUMS" | sha256sum | cut -d' ' -f1)$" "$(rel sums "$LAB/s-ok" >/dev/null; sha256sum <"$LAB/s-ok/SHA256SUMS" | cut -d' ' -f1)"
printf 'aivoicebus-darwin-amd64\naivoicebus-darwin-arm64\naivoicebus-linux-amd64\nSHA256SUMS\n' >"$LAB/gh.assets"
cp "$LAB/s-ok/SHA256SUMS" "$LAB/gh.sums"
attach() { NOTES="$LAB/notes.md" rel attach "$@"; }
uploaded() { grep -c '^release upload ' "$LAB/gh.calls" 2>/dev/null || true; }
rm -f "$LAB/gh.isdraft" "$LAB/gh.calls"
out=$(attach "$LAB/s-ok"); [ $? != 0 ] && has "attach refuses when CI has built no release" "no release v1.2.3 .* push the tag" "$out" || fail "attach without a draft succeeded"
[ "$(uploaded)" = 0 ] && pass "and uploaded nothing" || fail "uploaded without a draft"
echo false >"$LAB/gh.isdraft"; rm -f "$LAB/gh.calls"
out=$(attach "$LAB/s-ok" --publish); [ $? != 0 ] && has "attach never touches a published release, even with --publish" "is not a draft" "$out" || fail "attach edited a published release"
[ "$(uploaded)" = 0 ] && ! grep -q 'draft=false' "$LAB/gh.calls" && pass "and uploaded and published nothing" || fail "gh was used on a published release: $(cat "$LAB/gh.calls")"
echo true >"$LAB/gh.isdraft"
out=$(attach "$LAB/s-ok" --bogus); [ $? != 0 ] && has "an unknown option is refused" "unknown option --bogus" "$out" || fail "attach took --bogus"
printf 'aivoicebus-darwin-amd64\naivoicebus-linux-amd64\n' >"$LAB/gh.assets"; rm -f "$LAB/gh.calls"
out=$(attach "$LAB/s-ok"); [ $? != 0 ] && has "a draft missing a binary you signed is refused" "the draft has no asset aivoicebus-darwin-arm64" "$out" || fail "attach with a missing asset succeeded"
[ "$(uploaded)" = 0 ] && pass "and uploaded nothing" || fail "uploaded with a missing asset"
printf 'aivoicebus-darwin-amd64\naivoicebus-darwin-arm64\naivoicebus-linux-amd64\n' >"$LAB/gh.assets"
echo "0000000000000000000000000000000000000000000000000000000000000000  aivoicebus-linux-amd64" >"$LAB/gh.sums"; rm -f "$LAB/gh.calls"
out=$(attach "$LAB/s-ok"); [ $? != 0 ] && has "binaries that differ from the ones CI built are refused" "you signed something other than what CI built" "$out" || fail "attach accepted different binaries"
[ "$(uploaded)" = 0 ] && pass "and uploaded nothing" || fail "uploaded different binaries"
cp "$LAB/s-ok/SHA256SUMS" "$LAB/gh.sums"; rm -f "$LAB/gh.calls"
out=$(attach "$LAB/s-ok")
has "attach to a matching draft uploads the signature" "^release upload v1.2.3 --repo .*/manifest.json .*/manifest.sig" "$(cat "$LAB/gh.calls" 2>/dev/null)"
has "and the compat record" "/compat-pass.txt" "$(grep '^release upload' "$LAB/gh.calls" 2>/dev/null)"
has "and says the release is still a draft" "still a DRAFT" "$out"
! grep -q 'draft=false' "$LAB/gh.calls" && pass "and does not publish it" || fail "attach published without --publish"
rm -f "$LAB/gh.calls"
out=$(attach "$LAB/s-ok" --publish)
has "--publish takes it out of draft" "^release edit v1.2.3 --repo .* --draft=false" "$(cat "$LAB/gh.calls" 2>/dev/null)"
rm -f "$LAB/gh.args" "$LAB/gh.notes" "$LAB/gh.calls" "$LAB/gh.isdraft" "$LAB/gh.assets" "$LAB/gh.sums"

section "the release lock: one release operation at a time"
lock() { deploy/release-lock.sh "$@" 2>&1; }
LOCKF="$LAB/nodes/lab_n1/etc/aivoicebus/release.lock"
has "status says it is free" "release lock: free" "$(lock status)"
has "alice takes it" "taken by alice" "$(lock take alice "rollout 9.9.9")"
[ -s "$LOCKF" ] && pass "the lock lives on the first node of nodes.conf" || fail "no lock file on n1"
out=$(lock status); rc=$?; [ $rc = 3 ] && has "status names the holder and what for" "held by alice .*for 'rollout 9.9.9'" "$out" || fail "status exit $rc: $out"
out=$(lock take bob); rc=$?; [ $rc = 3 ] && has "bob is refused and told who holds it" "held by alice" "$out" || fail "bob took a held lock (exit $rc)"
out=$(lock release bob); rc=$?; [ $rc = 3 ] && pass "bob cannot release alice's lock" || fail "bob released alice's lock (exit $rc)"
REL8=$(release_dir v608 6.0.8 "$LAB/bins/real608")
out=$(deploy/publish-release.sh "$REL8" --apply 2>&1); rc=$?
[ $rc != 0 ] && has "publish-release refuses while it is held" "held by alice" "$out" || fail "publish-release went ahead under alice's lock (exit $rc)"
[ ! -e "$LAB/nodes/lab_n1/srv/aivoicebus-release/manifest.json" ] && pass "and uploaded nothing" || fail "publish uploaded under someone else's lock"
out=$(deploy/rollout.sh "$REL8" --unsigned --apply --soak 1 2>&1); rc=$?
[ $rc != 0 ] && has "rollout refuses while it is held" "held by alice" "$out" || fail "rollout went ahead under alice's lock (exit $rc)"
[ "$(version_of 1)" = 6.0.6 ] && pass "and touched no node" || fail "node 1 runs $(version_of 1)"
out=$(deploy/publish-release.sh "$REL8" --apply --force-lock 2>&1); rc=$?
[ $rc = 0 ] && has "--force-lock takes it over, saying from whom" "TAKEN OVER .*alice" "$out" || fail "publish --force-lock exit $rc"
[ -s "$LAB/nodes/lab_n1/srv/aivoicebus-release/manifest.json" ] && pass "and publishes" || fail "nothing published"
has "and the lock is given back when it ends" "release lock: free" "$(lock status)"

out=$(AIVOICEBUS_LOCK_STALE=0 deploy/release-lock.sh status 2>&1); rc=$?
[ $rc = 2 ] && has "a stale limit under 60 seconds is refused (it would switch the lock off)" "at least 60" "$out" || fail "AIVOICEBUS_LOCK_STALE=0 exit $rc"
touch "$LAB/nodes/lab_n1/down"
out=$(deploy/publish-release.sh "$REL8" --apply 2>&1); rc=$?
[ $rc != 0 ] && has "with the lock node unreachable, publish refuses and says so" "cannot reach n1 (lab_n1), where the release lock lives" "$out" || fail "publish went ahead with the lock node down (exit $rc)"
has "and names the way out" "--force goes on without a lock" "$out"
out=$(deploy/publish-release.sh "$REL8" --apply --force-lock 2>&1); rc=$?
[ $rc = 0 ] && has "--force-lock goes on without a lock, loudly" "WARNING: going on WITHOUT the release lock" "$out" || fail "publish --force-lock exit $rc"
has "and skips the unreachable node" "UNREACHABLE, skipping" "$out"
rm -f "$LAB/nodes/lab_n1/down"
mkdir -p "$LAB/nodes/lab_n9"; echo "n9:lab_n9:127.0.0.19" >"$LAB/nodes9.conf"
out=$(AIVOICEBUS_NODES_CONF="$LAB/nodes9.conf" deploy/release-lock.sh take carol 2>&1); rc=$?
[ $rc = 4 ] && has "a lock node without /etc/aivoicebus says that, not 'another take in progress'" "has no /etc/aivoicebus" "$out" || fail "no-/etc/aivoicebus exit $rc: $out"
lacks "(and not the misleading message)" "another take" "$out"
has "the lock is still free on node 1" "release lock: free" "$(lock status)"

section "two rollouts at once: the second refuses; a killed one leaves its lock"
deploy/rollout.sh "$REL8" --unsigned --apply --soak 30 >"$LAB/ro-a.log" 2>&1 &
A=$!
eventually "the first rollout reaches the canary's soak" 40 grep -q soaking "$LAB/ro-a.log"
lock_when() { sed -n 's/^when=//p' "$LOCKF"; }
W0=$(lock_when)
when_moved() { [ "$(lock_when)" -gt "$W0" ]; }
eventually "the running rollout renews its lock (a long rollout must not lose it)" 15 when_moved
pids=""; for i in 2 3 4 5; do pids="$pids $(node_pid $i)"; done
out=$(deploy/rollout.sh "$REL8" --unsigned --apply --soak 1 2>&1); rc=$?
[ $rc != 0 ] && has "a second rollout refuses, naming the first" "held by .*:$A (from .*) for 'rollout 6.0.8'" "$out" || fail "a second rollout ran beside the first (exit $rc)"
kill -KILL "$A"; wait "$A" 2>/dev/null
out=$(lock status); rc=$?
[ $rc = 3 ] && has "a rollout killed with -9 leaves its lock, and status shows whose" "held by .*:$A .*for 'rollout 6.0.8'" "$out" || fail "status after a crash (exit $rc): $out"
out=$(deploy/rollout.sh "$REL8" --unsigned --apply --soak 1 2>&1); rc=$?
[ $rc != 0 ] && has "and it still holds off the next rollout" "held by" "$out" || fail "a rollout ran over a dead run's fresh lock"
now=""; for i in 2 3 4 5; do now="$now $(node_pid $i)"; done
[ "$pids" = "$now" ] && pass "nodes 2-5 were never touched" || fail "other nodes were restarted: $pids -> $now"

section "a stale lock is replaced, and the resumed rollout soaks its canary again"
[ "$(version_of 1)" = 6.0.8 ] && pass "the killed rollout had upgraded the canary" || fail "the canary runs $(version_of 1)"
sed -i 's/^when=.*/when=1/' "$LOCKF"   # the dead run last renewed it long ago
out=$(deploy/rollout.sh "$REL8" --unsigned --apply --soak 3 2>&1); rc=$?
[ $rc = 0 ] && pass "the next rollout runs" || { fail "rollout exit $rc"; echo "$out" | tail -15; }
has "it says it replaced a stale lock" "replaced a stale lock" "$out"
has "the canary is already on the release" "already on 6.0.8 with its peers connected: not installed again" "$out"
soak_line=$(printf '%s\n' "$out" | grep -n 'canary stable' | head -1 | cut -d: -f1); n2_line=$(printf '%s\n' "$out" | grep -n '^== n2' | head -1 | cut -d: -f1)
[ -n "$soak_line" ] && [ -n "$n2_line" ] && [ "$soak_line" -lt "$n2_line" ] && pass "and is soaked before any other node is touched" || fail "no soak before n2 (soak line ${soak_line:-none}, n2 line ${n2_line:-none})"
for i in 1 2 3 4 5; do [ "$(version_of $i)" = 6.0.8 ] && pass "node $i runs 6.0.8" || fail "node $i runs $(version_of $i)"; done
has "the lock is free again" "release lock: free" "$(lock status)"

section "a resumed rollout whose canary crash-loops stops before the other nodes"
AIVOICEBUS_BIN="$LAB/bins/flapping" deploy/install.sh --apply n1 >/dev/null 2>&1   # an interrupted run left n1 on it
pids=""; for i in 2 3 4 5; do pids="$pids $(node_pid $i)"; done
out=$(deploy/rollout.sh "$REL3" --unsigned --apply --soak 6 --expect 6.0.3 2>&1); rc=$?
[ $rc != 0 ] && pass "rollout exits non-zero" || fail "a crash-looping canary 'passed'"
has "the soak saw the restarts" "restarted [0-9]* -> [0-9]* time" "$out"
now=""; for i in 2 3 4 5; do now="$now $(node_pid $i)"; done
[ "$pids" = "$now" ] && pass "nodes 2-5 were never touched" || fail "other nodes were restarted: $pids -> $now"
has "the resumed canary is rolled back to its previous binary" "ROLLING BACK n1" "$out"
n1_runs() { [ "$(version_of 1)" = "$1" ]; }
eventually "and node 1 ends on the old release, not the bad one" 15 n1_runs 6.0.8

section "a resumed canary with no previous binary to go back to says so loudly"
AIVOICEBUS_BIN="$LAB/bins/flapping" deploy/install.sh --apply n1 >/dev/null 2>&1
rm -f "$LAB/nodes/lab_n1/usr/local/bin/aivoicebus.prev"
pids=""; for i in 2 3 4 5; do pids="$pids $(node_pid $i)"; done
out=$(deploy/rollout.sh "$REL3" --unsigned --apply --soak 6 --expect 6.0.3 2>&1); rc=$?
[ $rc != 0 ] && has "rollout exits non-zero and names the problem" "CANNOT ROLL BACK n1" "$out" || fail "rollout 'succeeded' or stayed quiet (exit $rc)"
has "saying the node is still on the bad release" "STILL ON release 6.0.3" "$out"
now=""; for i in 2 3 4 5; do now="$now $(node_pid $i)"; done
[ "$pids" = "$now" ] && pass "nodes 2-5 were never touched" || fail "other nodes were restarted: $pids -> $now"
AIVOICEBUS_BIN="$LAB/bins/real608" deploy/install.sh --apply n1 >/dev/null 2>&1

section "the watcher stops a rollout when a node stops accepting the observer token"
REL9=$(release_dir v609 6.0.9 "$LAB/bins/real609")
deploy/rollout.sh "$REL9" --unsigned --apply --soak 20 >"$LAB/ro-w.log" 2>&1 &
WR=$!
eventually "the rollout reaches the canary's soak" 40 grep -q soaking "$LAB/ro-w.log"
cp "$LAB/nodes/lab_n3/etc/aivoicebus/node.env" "$LAB/n3.env"
sed -i 's/^OBSERVER_ARGS=.*/OBSERVER_ARGS=/' "$LAB/nodes/lab_n3/etc/aivoicebus/node.env"; nodectl 3 restart
pids=""; for i in 2 4 5; do pids="$pids $(node_pid $i)"; done
wait "$WR"; rc=$?; out=$(cat "$LAB/ro-w.log")
[ $rc != 0 ] && pass "the rollout stops" || fail "the rollout went on (exit $rc)"
has "the watcher names the node and the token" "n3 accepted the watch token and now rejects it (HTTP 401)" "$out"
has "the canary being soaked is rolled back" "ROLLING BACK n1" "$out"; lacks "before its soak ends" "canary stable" "$out"
[ "$(version_of 1)" = 6.0.8 ] && pass "the canary runs 6.0.8 again" || fail "the canary runs $(version_of 1)"
now=""; for i in 2 4 5; do now="$now $(node_pid $i)"; done
[ "$pids" = "$now" ] && pass "nodes 2, 4 and 5 were never touched" || fail "other nodes were restarted: $pids -> $now"
has "and the lock is given back" "release lock: free" "$(lock status)"
# n3 still has no observer token. The rollout's final look shares the watcher's state: a node that had
# accepted the token (acc.<node>) and now refuses it is a trip even in a single --once look.
ST2=$(mktemp -d "$LAB/st.XXXXXX"); : >"$ST2/acc.n3"
out=$(deploy/watch.sh --once --expect 6.9.9 --state "$ST2" 2>&1); rc=$?
[ $rc = 3 ] && has "watch --once with the rollout's state catches token loss on a node" "n3 accepted the watch token" "$out" || fail "watch --once with state exit $rc: $out"
# A node that refused at first (or was down) is tracked as soon as it answers, and trips if it then refuses.
ST3=$(mktemp -d "$LAB/st.XXXXXX")
deploy/watch.sh --pid $$ --expect 6.9.9 --trip "$ST3/trip" --state "$ST3" >"$ST3/log" 2>&1 &
WP=$!
sleep 3
[ ! -s "$ST3/trip" ] && pass "a node that never accepted the token is not a trip" || fail "tripped on a node that never accepted: $(cat "$ST3/trip")"
cp "$LAB/n3.env" "$LAB/nodes/lab_n3/etc/aivoicebus/node.env"; nodectl 3 restart
obs_ok() { [ "$(code "$OBS" "$(url 3)/v1/health")" = 200 ]; }
eventually "n3 accepts the observer token again" 15 obs_ok
tracked() { [ -e "$ST3/acc.n3" ]; }
eventually "and the watcher tracks it from the pass it first answered" 10 tracked
sed -i 's/^OBSERVER_ARGS=.*/OBSERVER_ARGS=/' "$LAB/nodes/lab_n3/etc/aivoicebus/node.env"; nodectl 3 restart
tripped() { [ -s "$ST3/trip" ]; }
eventually "so losing the token afterwards trips" 20 tripped
has "naming the node" "n3 accepted the watch token" "$(cat "$ST3/trip" 2>/dev/null)"
kill "$WP" 2>/dev/null; wait "$WP" 2>/dev/null
cp "$LAB/n3.env" "$LAB/nodes/lab_n3/etc/aivoicebus/node.env"; nodectl 3 restart
eventually "n3 accepts the observer token again" 15 obs_ok

section "a watcher that dies stops the rollout instead of leaving it unwatched"
deploy/rollout.sh "$REL9" --unsigned --apply --soak 20 >"$LAB/ro-d.log" 2>&1 &
DR=$!
eventually "the rollout reaches the canary's soak" 40 grep -q soaking "$LAB/ro-d.log"
pkill -KILL -f "deploy/watch.sh --pid $DR"
pids=""; for i in 2 3 4 5; do pids="$pids $(node_pid $i)"; done
wait "$DR"; rc=$?; out=$(cat "$LAB/ro-d.log")
[ $rc != 0 ] && has "the rollout stops and says why" "the watcher process died" "$out" || fail "the rollout went on unwatched (exit $rc)"
has "the node being installed is rolled back" "ROLLING BACK n1" "$out"
eventually "node 1 is back on 6.0.8" 15 n1_runs 6.0.8
now=""; for i in 2 3 4 5; do now="$now $(node_pid $i)"; done
[ "$pids" = "$now" ] && pass "nodes 2-5 were never touched" || fail "other nodes were restarted: $pids -> $now"
has "and the lock is given back" "release lock: free" "$(lock status)"

section "the operator scripts need nothing newer than bash 3.2 (a Mac's /bin/bash)"
out=$(scripts/lint.sh bash3 2>&1); rc=$?
[ $rc = 0 ] && has "no bash 4 construct in deploy/ or release.sh (watch.sh had an associative array)" "no bash 4 constructs" "$out" || fail "lint bash3: $out"

# Last: a quarantined record replicates to every node and stays, so nothing after this can pass verify_node.
section "a quarantined record stops a rollout: the watcher and verify_node"
deploy/rollout.sh "$REL9" --unsigned --apply --soak 25 >"$LAB/ro-q.log" 2>&1 &
QR=$!
eventually "the rollout reaches the canary's soak" 40 grep -q soaking "$LAB/ro-q.log"
nodectl 4 stop   # n4's log gets a record no client writes; it is quarantined when n4 reads it back
printf '{"origin":"labbad","seq":1,"msg":{"id":"bad0000001","from":"lab/x","to":"lab/y","thread":"","body":"x","at":"2026-01-01T00:00:00Z","prio":"loud"}}\n' >>"$LAB/nodes/lab_n4/var/lib/aivoicebus/log.jsonl"
nodectl 4 start
wait "$QR"; rc=$?; out=$(cat "$LAB/ro-q.log")
[ $rc != 0 ] && pass "the rollout stops" || fail "the rollout went on (exit $rc)"
has "the watcher saw the canary's placeholder" "n1 (on 6.0.9) reports quarantined=1" "$out"
has "the canary is rolled back" "rolled back and healthy" "$out"
[ "$(version_of 1)" = 6.0.8 ] && pass "the canary runs 6.0.8 again" || fail "the canary runs $(version_of 1)"
[ "$(health_field 2 quarantined)" = 1 ] && pass "node 2 holds the placeholder too" || fail "node 2 quarantined=$(health_field 2 quarantined)"
# The placeholder is permanent. It must not block the token tools (a leak response) or the next rollout.
ok "verify_node still passes a node that holds one (rotate-token and machine-token use it)" bash -c ". deploy/ops.sh; verify_node 127.0.0.12 '$TOKEN_FILE' 6.0.8"
out=$(deploy/machine-token.sh add mq --apply 2>&1); rc=$?
[ $rc = 0 ] && has "a machine token can still be made" "mq has its own token" "$out" || { fail "machine-token add exit $rc"; echo "$out" | tail -5; }
out=$(deploy/machine-token.sh revoke mq --apply 2>&1); [ $? = 0 ] && pass "and revoked (the leak response works)" || fail "revoke exit: $out"
out=$(deploy/watch.sh --once --expect 6.0.8 2>&1); rc=$?
[ $rc = 3 ] && has "watch.sh --once with no baseline counts from 0, and trips on it" "reports quarantined=1" "$out" || fail "watch --once exit $rc: $out"
has "and passes a release no node runs" "nothing wrong" "$(deploy/watch.sh --once --expect 6.9.9 2>&1)"
REL10=$(release_dir v610 6.1.0 "$LAB/bins/real610")
out=$(deploy/rollout.sh "$REL10" --unsigned --apply --soak 3 2>&1); rc=$?
[ $rc = 0 ] && pass "a rollout after the quarantine succeeds: only an increase stops one" || { fail "rollout exit $rc"; echo "$out" | tail -12; }
for i in 1 2 3 4 5; do [ "$(version_of $i)" = 6.1.0 ] && pass "node $i runs 6.1.0" || fail "node $i runs $(version_of $i)"; done

section "signed messages only: is the mesh ready, and the switch"
unsigned() { code "$TOKEN_FILE" "$(url "$1")/v1/send" -X POST -d "{\"from\":\"labold/agent\",\"to\":\"lab/reader\",\"body\":\"unsigned, like a client older than 0.7.0 ($2)\"}"; }
[ "$(unsigned 2 one)" = 200 ] && pass "a node takes an unsigned message by default" || fail "unsigned send refused"
out=$(AIVOICEBUS_URL="$ALLURLS" AIVOICEBUS_TOKEN="$(tok)" "$NEW" mesh --signed-check 2>&1); rc=$?
[ $rc = 1 ] && has "mesh --signed-check is NOT READY, naming the sender" "labold/agent" "$out" || fail "signed-check exit $rc: $out"
has "and says why" "NOT READY: .* sender(s) sent messages without a valid signature" "$out"
out=$(AIVOICEBUS_NODE_REQUIRE_SIGNED=yes deploy/install.sh --apply --peers-only n1 2>&1); [ $? = 2 ] && pass "install.sh refuses a value other than 1 or 0" || fail "install.sh took AIVOICEBUS_NODE_REQUIRE_SIGNED=yes: $out"
out=$(AIVOICEBUS_NODE_REQUIRE_SIGNED=1 deploy/install.sh --apply --peers-only n1 2>&1); rc=$?
[ $rc = 0 ] && grep -qx 'AIVOICEBUS_REQUIRE_SIGNED=1' "$LAB/nodes/lab_n1/etc/aivoicebus/node.env" && pass "the switch: AIVOICEBUS_NODE_REQUIRE_SIGNED=1 install.sh --peers-only writes it to node.env" || fail "switch exit $rc: $out"
[ "$(unsigned 1 two)" = 403 ] && pass "the node then refuses an unsigned message" || fail "n1 still takes unsigned messages"
M=$(AIVOICEBUS_ME=lab/sender AIVOICEBUS_URL="$(url 1)" AIVOICEBUS_TOKEN="$(tok)" "$NEW" send --to lab/reader "signed, after the switch" 2>&1); has "and still takes a signed one" "^sent " "$M"
deploy/install.sh --apply --peers-only n1 >/dev/null 2>&1
[ "$(unsigned 1 three)" = 403 ] && pass "a later install without the variable keeps the switch" || fail "a re-install dropped --require-signed"
has "mesh --signed-check says which node already refuses them" "already refuses unsigned ones" "$(AIVOICEBUS_URL="$(url 1)" AIVOICEBUS_TOKEN="$(tok)" "$NEW" mesh --signed-check 2>&1)"
AIVOICEBUS_NODE_REQUIRE_SIGNED=0 deploy/install.sh --apply --peers-only n1 >/dev/null 2>&1
[ "$(unsigned 1 four)" = 200 ] && pass "AIVOICEBUS_NODE_REQUIRE_SIGNED=0 switches it off again" || fail "n1 still refuses unsigned messages"

echo
if [ "$FAILED" = 0 ]; then echo "lab passed"; else echo "LAB FAILED"; fi
exit "$FAILED"
