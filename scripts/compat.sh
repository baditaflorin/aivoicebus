#!/usr/bin/env bash
# Mixed-version gate: the current tree next to REAL, SIGNED older releases, one node each, in a full
# mesh on loopback with a throwaway token. Run it before every node rollout (docs/releasing.md).
#
#   scripts/compat.sh                                  # every dist/release-X found
#   scripts/compat.sh --old 0.1.0 --old 0.2.0          # just these
#   scripts/compat.sh --dir /path/to/releases --keep   # release-X dirs elsewhere; keep the scratch dir
#   scripts/compat.sh --new dist/release/aivoicebus-linux-amd64 --record dist/release/compat-pass.txt
#       # test the RELEASE binary (not a fresh go build) and, on a full pass, write the record `release.sh sign` demands
#   (--dir, --new and --record are relative to where you run it)
#
# For each old VERSION it takes DIR/release-VERSION (a signed release dir as release.sh writes it)
# or, when that is missing and gh is installed, downloads the GitHub release vVERSION into it. The
# repository is private, so a CI runner without credentials cannot download: give it the dirs.
# Every old release is verified with this tree's `release verify` (signature against the compiled-in
# key, every file's hash) before any of its bytes run.
#
# Linux only (the old binaries are linux-amd64), and heavy enough to belong on a build host, e.g.
# inside golang:1.25 like the lab. Needs go, curl and python3. Exits non-zero on any FAIL.
#
# What each version is expected to do is derived from the release that introduced the behaviour
# (feature_since below), not from what the nodes report, so a node that silently lost a behaviour
# fails. Checks test behaviour (counts, ids, fields), never the wording of a message.
set -uo pipefail
START=$PWD
cd "$(dirname "$0")/.." || exit 2
REPO=baditaflorin/aivoicebus

die() { echo "compat: $*" >&2; exit 2; }
DIR=dist; KEEP=; OLDV=(); RECORD=; NEWBIN=
abs() { case $1 in /*) echo "$1" ;; *) echo "$START/$1" ;; esac; } # the user's paths mean where they ran it
while [ $# -gt 0 ]; do
  case $1 in
    --old) [ $# -ge 2 ] || die "--old needs a version"; OLDV+=("$2"); shift 2 ;;
    --dir) [ $# -ge 2 ] || die "--dir needs a directory"; DIR=$(abs "$2"); shift 2 ;;
    --keep) KEEP=1; shift ;;
    --record) [ $# -ge 2 ] || die "--record needs a file"; RECORD=$(abs "$2"); shift 2 ;;
    --new) [ $# -ge 2 ] || die "--new needs a binary"; NEWBIN=$(abs "$2"); shift 2 ;;
    -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
    *) die "unknown argument $1 (see --help)" ;;
  esac
done
[ -z "$RECORD" ] || scripts/compat-record.sh begin "$RECORD" || exit 2 # a stale pass must never outlive a run that did not pass
[ "$(uname -s)" = Linux ] || die "Linux only: run it on a build host (see docs/releasing.md), never on a laptop"
for t in curl python3; do command -v "$t" >/dev/null 2>&1 || die "$t is not installed"; done
[ -n "$NEWBIN" ] || command -v go >/dev/null 2>&1 || die "go is not installed (or pass --new <binary>)"
if [ ${#OLDV[@]} -eq 0 ]; then
  for d in "$DIR"/release-*; do [ -d "$d" ] && OLDV+=("${d##*/release-}"); done
  [ ${#OLDV[@]} -gt 0 ] || die "no $DIR/release-X dirs; pass --old VERSION (and --dir)"
fi
mapfile -t OLDV < <(printf '%s\n' "${OLDV[@]}" | sort -uV)
for v in "${OLDV[@]}"; do [[ $v =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "not a release number: $v"; done

# The release that first did each behaviour checked below. A new behaviour gets a line here.
feature_since() {
  case $1 in
    quarantine) echo 0.1.0 ;; # Merge and replay replace an invalid replicated record by a placeholder
    features) echo 0.1.0 ;;   # /v1/health lists the node's features
    retract) echo 0.1.0 ;;    # a retraction ack hides its message (inbox) and tombstones it (thread)
    tasks) echo 0.2.0 ;;      # holds a task (msg.TaskSince); the CLI refuses to send one to an older mesh
    progress) echo 0.10.0 ;;  # holds a --kind progress message (an older node quarantines it)
    states) echo 0.11.0 ;;    # a conversation state passes a used-up turn limit when it closes the conversation
    share) echo 0.12.0 ;;     # /v1/share/...: a thread's shared space (versions of sealed files, notes)
    loopkey) echo 0.10.0 ;;   # the loop check (a third message alike, numbers aside) and the pair brake (8, then 24 replies in 2 minutes)
    brakes) echo 0.12.0 ;;    # repeats judged by similarity: new replies pass the pair brake to 48, numbers are words, rewordings are repeats
    signing) echo 0.7.0 ;;    # keeps a message's signature; a client signs only when every node does
    *) echo "compat: unknown feature $1" >&2; return 1 ;;
  esac
}

W=$(mktemp -d "${TMPDIR:-/tmp}/aivoicebus-compat.XXXXXX")
declare -a NAME BIN VER PORT PID
cleanup() {
  for i in "${!PID[@]}"; do [ -n "${PID[$i]}" ] && kill "${PID[$i]}" 2>/dev/null; done
  wait 2>/dev/null
  if [ -n "$KEEP" ]; then echo "compat: scratch kept in $W"; else rm -rf "$W"; fi
}
trap cleanup EXIT

# ---- 1. the binaries: this tree, then each old release, verified before it runs
if [ -n "$NEWBIN" ]; then
  echo "== use the release binary $NEWBIN"
  [ -x "$NEWBIN" ] || die "$NEWBIN is not an executable file"
  cp "$NEWBIN" "$W/aivoicebus-new" && chmod 755 "$W/aivoicebus-new"
else
  echo "== build the current tree (no --new: this run can not produce a record that release.sh accepts)"
  go build -o "$W/aivoicebus-new" . || die "go build failed"
fi
NEW=$W/aivoicebus-new
NAME[0]=new; BIN[0]=$NEW; VER[0]=new
i=0
for v in "${OLDV[@]}"; do
  rd=$DIR/release-$v
  if [ ! -d "$rd" ]; then
    command -v gh >/dev/null 2>&1 || die "$rd is missing and gh is not installed (the repo is private: copy the release dir in, or use --dir)"
    echo "== download release v$v from GitHub into $rd"
    gh release download "v$v" --repo "$REPO" --dir "$rd" || die "could not download v$v (no access? the repo is private)"
  fi
  "$NEW" release verify --dir "$rd" || die "release $v in $rd does not verify: refusing to run it"
  mv=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["version"])' "$rd/manifest.json") || die "unreadable manifest in $rd"
  [ "$mv" = "$v" ] || die "$rd holds release $mv, not $v"
  i=$((i + 1))
  cp "$rd/aivoicebus-linux-amd64" "$W/aivoicebus-$v" && chmod 755 "$W/aivoicebus-$v"
  grep -qF "$v" <<<"$("$W/aivoicebus-$v" version 2>&1)" || die "the $v binary does not say it is $v"
  NAME[$i]=o${v//./}; BIN[$i]=$W/aivoicebus-$v; VER[$i]=$v
done
N=${#NAME[@]}
echo "nodes: $(for i in "${!NAME[@]}"; do printf '%s=%s ' "${NAME[$i]}" "${VER[$i]}"; done)"

has() { # INDEX FEATURE
  local since; since=$(feature_since "$2") || exit 2
  [ "${VER[$1]}" = new ] && return 0
  [ "$(printf '%s\n%s\n' "$since" "${VER[$1]}" | sort -V | head -1)" = "$since" ]
}

# ---- 2. the mesh
export HOME=$W/home; mkdir -p "$HOME"
unset AIVOICEBUS_AGENT AIVOICEBUS_HOST AIVOICEBUS_TOKEN_FILE CLAUDE_ENV_FILE AIVOICEBUS_URL AIVOICEBUS_TOKEN AIVOICEBUS_ME
head -c 48 /dev/urandom | base64 | tr -d '\n=+/' | head -c 48 >"$W/tok"
TOK=$(cat "$W/tok")
BASE=${COMPAT_BASE_PORT:-17940}
for i in "${!NAME[@]}"; do PORT[$i]=$((BASE + i)); done

up() { # PORT
  local _
  for _ in $(seq 1 120); do curl -fsS -o /dev/null "http://127.0.0.1:$1/healthz" 2>/dev/null && return 0; sleep 0.25; done
  return 1
}
start() { # INDEX [BINARY]: a node keeps its data dir whichever binary runs on it
  local i=$1 bin=${2:-${BIN[$1]}} peers="" j
  for j in "${!NAME[@]}"; do [ "$j" != "$i" ] && peers="$peers,http://127.0.0.1:${PORT[$j]}"; done
  mkdir -p "$W/d$i"
  "$bin" serve --addr "127.0.0.1:${PORT[$i]}" --data "$W/d$i" --token-file "$W/tok" --node "${NAME[$i]}" \
    --peers "${peers#,}" >>"$W/${NAME[$i]}.log" 2>&1 &
  PID[$i]=$!
  up "${PORT[$i]}" || { echo "compat: node ${NAME[$i]} did not come up; its log:"; tail -20 "$W/${NAME[$i]}.log"; exit 1; }
}
stop() { kill "${PID[$1]}" 2>/dev/null; wait "${PID[$1]}" 2>/dev/null; PID[$1]=; }

get() { curl -fsS -H "Authorization: Bearer $TOK" "http://127.0.0.1:$1$2"; }
post() { # PORT PATH BODY -> the HTTP status; the body goes to $W/last.out
  curl -sS -o "$W/last.out" -w '%{http_code}' -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
    -X POST "http://127.0.0.1:$1$2" -d "$3"
}
py() { local prog=$1; shift; python3 -I -c "$prog" "$@"; }
cli() { # INDEX-OF-NODE BINARY IDENTITY ARGS...: a CLI of any version against one node
  local p=${PORT[$1]} bin=$2 me=$3; shift 3
  AIVOICEBUS_URL=http://127.0.0.1:$p AIVOICEBUS_TOKEN=$TOK AIVOICEBUS_ME=$me "$bin" "$@"
}
vv() { get "$1" /v1/health | py 'import json,sys; print(json.dumps(json.load(sys.stdin)["log"],sort_keys=True))'; }
hfield() { get "$1" /v1/health | py 'import json,sys; d=json.load(sys.stdin); print(d.get(sys.argv[1],"absent"))' "$2"; }
feed() { get "$1" '/v1/sync?vv=%7B%7D'; } # one page; the gate's data fits in it
# A mark matches only as a whole word: node names are versions without their dots, so o010
# (0.1.0) is a prefix of o0100 (0.10.0), and a plain substring test found both.
id_of() { # PORT MARK -> the id of the message whose body holds MARK
  feed "$1" | py 'import json,re,sys
has=lambda b: re.search(re.escape(sys.argv[1])+r"(?![0-9A-Za-z_])", b)
ids=[r["msg"]["id"] for r in json.load(sys.stdin) or [] if r.get("msg") and has(r["msg"]["body"])]
print(ids[0] if len(ids)==1 else "")' "$2"
}
inbox_has() { # PORT ME ID -> yes|no
  get "$1" "/v1/inbox?me=$2" | py 'import json,sys; print("yes" if any(m["id"]==sys.argv[1] for m in json.load(sys.stdin) or []) else "no")' "$3"
}
inbox_ids() { get "$1" "/v1/inbox?me=$2" | py 'import json,sys; print(" ".join(sorted(m["id"] for m in json.load(sys.stdin) or [])))'; }
thread_of() { # PORT ID -> "retracted" (a tombstone flagged by the node), the body otherwise, or <none>
  get "$1" "/v1/thread?id=$2" | py 'import json,sys
d=[m for m in json.load(sys.stdin) or [] if m["id"]==sys.argv[1]]
print("<none>" if not d else ("retracted leak=%s" % bool(d[0]["retracted"].get("leak")) if d[0].get("retracted") else d[0]["body"]))' "$2"
}
export_from() { cli "$1" "$2" export/compat.x export --out "$3" >/dev/null 2>&1; }
converged() { # every node's version vector is the same
  local i a b
  a=$(vv "${PORT[0]}") || return 1
  for i in "${!NAME[@]}"; do b=$(vv "${PORT[$i]}") || return 1; [ "$a" = "$b" ] || return 1; done
}
converge() { local _; for _ in $(seq 1 90); do converged && return 0; sleep 0.5; done; return 1; }

FAILS=0; PASSES=0; SCEN=; declare -A SP SF; ORDER=()
scenario() { SCEN=$1; ORDER+=("$1"); SP[$1]=0; SF[$1]=0; echo "== $1"; }
check() { # GOT WANT WHAT
  if [ "$1" = "$2" ]; then echo "  PASS  $3"; PASSES=$((PASSES + 1)); SP[$SCEN]=$((SP[$SCEN] + 1))
  else echo "  FAIL  $3: got [$1] want [$2]"; FAILS=$((FAILS + 1)); SF[$SCEN]=$((SF[$SCEN] + 1)); fi
}
check_converged() { if converge; then check ok ok "$1"; else check "differ" ok "$1"; for i in "${!NAME[@]}"; do echo "    ${NAME[$i]}: $(vv "${PORT[$i]}")"; done; fi; }
check_quarantined() { # WANT-ON-ENFORCING-NODES WHAT
  local i got want
  for i in "${!NAME[@]}"; do
    got=$(hfield "${PORT[$i]}" quarantined); want=absent
    if has "$i" quarantine; then want=$1; elif [ "$got" != absent ]; then want=$1; fi # a node that predates quarantine may report 0
    check "$got" "$want" "$2 (${NAME[$i]} reports $got)"
  done
}

for i in "${!NAME[@]}"; do start "$i"; done
# A node on an empty disk takes no writes until it has heard from a peer: seed each one.
for i in "${!NAME[@]}"; do
  for _ in $(seq 1 60); do [ "$(post "${PORT[$i]}" /v1/send '{"from":"seed/compat","to":"seed/nobody","body":"seed"}')" = 200 ] && break; sleep 0.5; done
done
converge || { echo "compat: the mesh did not converge after seeding"; exit 1; }
READER=rdr/agent.q9
for i in "${!NAME[@]}"; do get "${PORT[$i]}" "/v1/inbox?me=$READER" >/dev/null; done # a session each node has seen

# ---- 3. scenarios
scenario "nodes advertise their features"
for i in "${!NAME[@]}"; do
  got=$(get "${PORT[$i]}" /v1/health | py 'import json,sys; f=json.load(sys.stdin).get("features"); print("absent" if f is None else ("retract" if "retract" in f else "other"))')
  want=absent; has "$i" features && want=retract
  check "$got" "$want" "${NAME[$i]} (${VER[$i]}) lists its features iff it has them"
done

scenario "replication both ways"
for i in "${!NAME[@]}"; do
  check "$(post "${PORT[$i]}" /v1/send '{"from":"h'"$i"'/compat.s1","to":"all","body":"replication MARK-S1-'"${NAME[$i]}"'"}')" 200 "send on ${NAME[$i]}"
done
check_converged "version vectors equal on all $N nodes"
missing=0
for i in "${!NAME[@]}"; do
  for j in "${!NAME[@]}"; do
    [ -n "$(id_of "${PORT[$j]}" "MARK-S1-${NAME[$i]}")" ] || { missing=$((missing + 1)); echo "    ${NAME[$j]} lacks the message written on ${NAME[$i]}"; }
  done
done
check "$missing" 0 "every node holds every node's message"
check_quarantined 0 "quarantined nothing"
M1=$(id_of "${PORT[0]}" MARK-S1-new)

scenario "messages from each version's CLI"
for i in "${!NAME[@]}"; do
  cli "$i" "${BIN[$i]}" "cli$i/agent.s2" send --to all --kind question --prio fyi --ttl 2h "cli MARK-S2-${NAME[$i]}" >/dev/null 2>"$W/send.err"
  check "$?" 0 "${VER[$i]} CLI sends with --kind --prio --ttl"
done
check_converged "version vectors equal"
for i in "${!NAME[@]}"; do
  for j in "${!NAME[@]}"; do
    got=$(feed "${PORT[$j]}" | py 'import json,re,sys
from datetime import datetime as D
has=lambda b: re.search(re.escape(sys.argv[1])+r"(?![0-9A-Za-z_])", b)
m=[r["msg"] for r in json.load(sys.stdin) or [] if r.get("msg") and has(r["msg"]["body"])]
if len(m)!=1: print("held %d" % len(m)); sys.exit()
m=m[0]; t=lambda s: D.fromisoformat(s.replace("Z","+00:00"))
ttl=int((t(m["exp"])-t(m["at"])).total_seconds()) if m.get("exp") else 0
print(m.get("kind"), m.get("prio"), ttl, "reach" if isinstance(m.get("reach"),int) else "no-reach")' "MARK-S2-${NAME[$i]}")
    check "$got" "question fyi 7200 reach" "${NAME[$j]} holds the ${VER[$i]} CLI's message with kind, prio, ttl and reach"
  done
done

scenario "acks with receipts"
for i in "${!NAME[@]}"; do
  cli "$i" "${BIN[$i]}" "ack$i/agent.r" ack "$M1" >/dev/null 2>&1
  check "$?" 0 "${VER[$i]} CLI acks"
  check "$(post "${PORT[$i]}" /v1/ack '{"id":"'"$M1"'","by":"raw'"$i"'/agent.r","via":"hook","chars":321}')" 200 "a receipt (via, chars) posted to ${NAME[$i]}"
done
check_converged "version vectors equal"
for j in "${!NAME[@]}"; do
  got=$(feed "${PORT[$j]}" | py 'import json,sys
a=[r["ack"] for r in json.load(sys.stdin) or [] if r.get("ack") and r["ack"]["id"]==sys.argv[1]]
print(len(a), sum(1 for x in a if x.get("via")=="hook" and x.get("chars")==321))' "$M1")
  check "$got" "$((2 * N)) $N" "${NAME[$j]} holds every ack, receipts intact"
  delivered=0
  for i in "${!NAME[@]}"; do [ "$(inbox_has "${PORT[$j]}" "ack$i/agent.r" "$M1")" = no ] || delivered=$((delivered + 1)); done
  check "$delivered" 0 "${NAME[$j]} delivers it to none of the sessions that acked it, on whatever node they did"
done

scenario "retraction"
# One message retracted on the new node with /v1/retract, and one per old node retracted through
# that node's /v1/ack with the carrier id (how a release that predates retraction writes one). The first carrier
# is a leak flag. Nodes with retraction hide all of them; older nodes keep delivering them, the
# documented degradation.
RIDS=(); RLEAK=()
check "$(post "${PORT[0]}" /v1/send '{"from":"laptop/claude.a1b2","to":"all","body":"retract MARK-R-new"}')" 200 "send on new"
RIDS+=("$(id_of "${PORT[0]}" MARK-R-new)"); RLEAK+=(False)
check "$(post "${PORT[0]}" /v1/retract '{"id":"'"${RIDS[0]}"'","by":"laptop/claude.c3d4"}')" 200 "retract on new through /v1/retract"
for ((i = 1; i < N; i++)); do
  check "$(post "${PORT[0]}" /v1/send '{"from":"laptop/claude.a1b2","to":"all","body":"retract MARK-R-'"${NAME[$i]}"'"}')" 200 "send on new"
  id=$(id_of "${PORT[0]}" "MARK-R-${NAME[$i]}"); RIDS+=("$id")
  converge
  carrier=retract; RLEAK+=(False); [ "$i" = 1 ] && { carrier=retract-leak; RLEAK[$i]=True; }
  check "$(post "${PORT[$i]}" /v1/ack '{"id":"'"$carrier:$id"'","by":"laptop/claude.c3d4"}')" 200 "retract through ${NAME[$i]}'s /v1/ack with $carrier:<id>"
done
check_converged "version vectors equal"
for j in "${!NAME[@]}"; do
  for k in "${!RIDS[@]}"; do
    if has "$j" retract; then
      check "$(inbox_has "${PORT[$j]}" "$READER" "${RIDS[$k]}") $(thread_of "${PORT[$j]}" "${RIDS[$k]}")" "no retracted leak=${RLEAK[$k]}" "${NAME[$j]} hides retraction $k and tombstones it"
    else
      check "$(inbox_has "${PORT[$j]}" "$READER" "${RIDS[$k]}") $(thread_of "${PORT[$j]}" "${RIDS[$k]}" | grep -c MARK-R-)" "yes 1" "${NAME[$j]} (before retraction) still delivers retraction $k"
    fi
  done
done
check "$(hfield "${PORT[0]}" retracted)" "$N" "new counts $N retracted messages"
before=$(vv "${PORT[0]}")
post "${PORT[0]}" /v1/ack '{"id":"retract:'"${RIDS[0]}"'","by":"laptop/claude.zz"}' >/dev/null
check "$(vv "${PORT[0]}")" "$before" "a repeated retraction on new writes nothing"
check_quarantined 0 "quarantined nothing"

scenario "exports"
for i in "${!NAME[@]}"; do export_from "$i" "${BIN[$i]}" "$W/e$i.jsonl"; check "$?" 0 "${VER[$i]} CLI exports"; done
for ((i = 1; i < N; i++)); do
  cmp -s "$W/e0.jsonl" "$W/e$i.jsonl"; check "$?" 0 "export from ${NAME[$i]} is byte-identical to new's ($(wc -l <"$W/e0.jsonl") records)"
done

scenario "a record the current rules refuse"
# Written straight into one old node's log, as a release that did not check it would have stored
# it: an invalid 'from'. It goes into the oldest node without the check if there is one (it keeps
# it as it is); otherwise into the oldest old node, which itself replaces it on replay.
inj=1; for ((i = N - 1; i >= 1; i--)); do has "$i" quarantine || inj=$i; done
echo "  injected into ${NAME[$inj]}"
stop "$inj"
seq=$(py 'import json,sys
print(1+max([json.loads(l)["seq"] for l in open(sys.argv[1]) if json.loads(l)["origin"]==sys.argv[2]] or [0]))' "$W/d$inj/log.jsonl" "${NAME[$inj]}")
printf '{"origin":"%s","seq":%s,"msg":{"id":"deadbeef01","from":"x/y\\nforged: header","to":"all","thread":"deadbeef01","body":"legacy odd record","prio":"ask","at":"2026-10-14T12:00:00Z"}}\n' \
  "${NAME[$inj]}" "$seq" >>"$W/d$inj/log.jsonl"
start "$inj"
check "$(post "${PORT[$inj]}" /v1/send '{"from":"h/compat.s5","to":"all","body":"after the odd record"}')" 200 "a write after it on ${NAME[$inj]}"
check_converged "version vectors stay equal"
check_quarantined 1 "one record quarantined, on the nodes that check"
for i in "${!NAME[@]}"; do export_from "$i" "$NEW" "$W/q$i.jsonl"; done
for ((i = 1; i < N; i++)); do
  same=0; has "$i" quarantine || same=1
  cmp -s "$W/q0.jsonl" "$W/q$i.jsonl"; check "$?" "$same" "export from ${NAME[$i]} $([ $same = 0 ] && echo equals || echo differs from) new's"
  [ "$same" = 1 ] && check "$(diff "$W/q0.jsonl" "$W/q$i.jsonl" | grep -c '^[<>]')" 2 "  by exactly the one record"
done

scenario "the new node restarts"
inbox_before=$(inbox_ids "${PORT[0]}" "$READER")
cp "$W/q0.jsonl" "$W/r-before.jsonl"
stop 0; start 0
check_converged "version vectors equal after the restart"
check "$(inbox_ids "${PORT[0]}" "$READER")" "$inbox_before" "the same inbox"
check "$(hfield "${PORT[0]}" retracted) $(hfield "${PORT[0]}" quarantined)" "$N 1" "the same retracted and quarantined counts"
export_from 0 "$NEW" "$W/r-after.jsonl"; cmp -s "$W/r-before.jsonl" "$W/r-after.jsonl"; check "$?" 0 "the same export"

scenario "upgrade in place, roll back, forward again"
for ((i = 1; i < N; i++)); do
  old_inbox=$(inbox_ids "${PORT[$i]}" "$READER"); export_from "$i" "${BIN[$i]}" "$W/u$i-0.jsonl"
  new_inbox=$(inbox_ids "${PORT[0]}" "$READER"); export_from 0 "$NEW" "$W/u-new.jsonl"
  for step in "upgraded:$NEW" "rolled back:${BIN[$i]}" "forward again:$NEW"; do
    what=${step%%:*}; bin=${step#*:}
    stop "$i"; start "$i" "$bin"
    check_converged "${NAME[$i]} $what: version vectors equal"
    if [ "$bin" = "$NEW" ]; then
      check "$(inbox_ids "${PORT[$i]}" "$READER")" "$new_inbox" "${NAME[$i]} $what: delivers what new delivers"
      check "$(hfield "${PORT[$i]}" quarantined)" 1 "${NAME[$i]} $what: quarantined 1"
      export_from "$i" "$NEW" "$W/u$i.jsonl"; cmp -s "$W/u-new.jsonl" "$W/u$i.jsonl"; check "$?" 0 "${NAME[$i]} $what: export equals new's"
    else
      check "$(inbox_ids "${PORT[$i]}" "$READER")" "$old_inbox" "${NAME[$i]} $what: delivers what it did before"
      export_from "$i" "${BIN[$i]}" "$W/u$i.jsonl"; cmp -s "$W/u$i-0.jsonl" "$W/u$i.jsonl"; check "$?" 0 "${NAME[$i]} $what: export as before the upgrade"
    fi
  done
  stop "$i"; start "$i"
done

scenario "conversation states"
# A state is a progress message whose first line starts with [state NAME] (msg/state.go): written by
# this tree's CLI through an old node, every node keeps it unchanged, an old CLI shows it as the
# ordinary message it is, and this tree's CLI folds the same state from any node. A client sends no
# progress (so no state) to a mesh with a node from before progress, which would quarantine it.
sent_id() { awk '$1=="sent"{print $2; exit}' "$1"; }
allprog=1; for k in "${!NAME[@]}"; do has "$k" progress || allprog=; done
via=$((N - 1)) # the oldest node
echo "  the taker writes through ${NAME[$via]}"
alltasks=1; for k in "${!NAME[@]}"; do has "$k" tasks || alltasks=; done
cli 0 "$NEW" cs-boss/claude.b1 task cs-lap "states MARK-CS-task" --ttl 2h >"$W/cs.out" 2>"$W/cs.err"; rc=$?
if [ -z "$alltasks" ]; then
  # A node from before tasks is in the mesh: the CLI refuses the task (as it has since 0.2.0), so there
  # is no conversation to give a state to, and nothing is written.
  check "$rc $(grep -c 'cannot hold a task' "$W/cs.err")" "1 1" "new CLI refuses a task to a mesh with a node before 0.2.0 (so no states either)"
  check_converged "version vectors equal"
else
check "$rc" 0 "new CLI sends a task on new"
T=$(sent_id "$W/cs.out")
converge
cli "$via" "$NEW" cs-lap/codex.t1 claim "$T" --settle 0s >/dev/null 2>&1; check "$?" 0 "new CLI claims it through ${NAME[$via]}"
cli "$via" "$NEW" cs-lap/codex.t1 state "$T" ready "MARK-CS-ready" >/dev/null 2>"$W/cs.err"; rc=$?
if [ -z "$allprog" ]; then
  check "$rc $(grep -c 'older than 0.10.0' "$W/cs.err")" "1 1" "new CLI refuses to send a state to a mesh with a node before 0.10.0"
  check_converged "version vectors equal"
  for k in "${!NAME[@]}"; do
    got=$(feed "${PORT[$k]}" | py 'import json,sys; print(sum(1 for r in json.load(sys.stdin) or [] if r.get("msg") and "MARK-CS-ready" in r["msg"]["body"]))')
    check "$got" 0 "${NAME[$k]} holds no state message"
  done
  check_quarantined 1 "quarantined nothing more"
else
  check "$rc" 0 "new CLI says ready through ${NAME[$via]}"
  cli "$via" "$NEW" cs-lap/codex.t1 state "$T" taken "MARK-CS-back" >/dev/null 2>&1; check "$?" 1 "new CLI refuses ready -> taken, sending nothing"
  cli "$via" "$NEW" cs-lap/codex.t1 state "$T" done "MARK-CS-done" >/dev/null 2>&1; check "$?" 0 "new CLI says done through ${NAME[$via]}"
  check_converged "version vectors equal"
  for k in "${!NAME[@]}"; do
    got=$(feed "${PORT[$k]}" | py 'import json,sys
m=[r["msg"] for r in json.load(sys.stdin) or [] if r.get("msg") and "MARK-CS-" in r["msg"]["body"] and r["msg"].get("kind")=="progress"]
print(" ".join(sorted("%s:%s" % (x["kind"], x["body"]) for x in m)) or "none")')
    check "$got" "progress:[state done] MARK-CS-done progress:[state ready] MARK-CS-ready" "${NAME[$k]} holds the two state messages as progress, bodies unchanged"
    got=$(cli "$k" "$NEW" cs-boss/claude.b1 state "$T" --json | py 'import json,sys; s=json.load(sys.stdin)["state"]; print(s["state"], s["taker"], "->".join(h["state"] for h in s["history"]), len(s.get("ignored") or []))')
    check "$got" "done cs-lap/codex.t1 offered->taken->ready->done 0" "new CLI folds the same state from ${NAME[$k]}"
    if [ "${VER[$k]}" != new ]; then
      out=$(cli "$k" "${BIN[$k]}" cs-boss/claude.b1 thread "$T" 2>&1); rc=$?
      check "$rc $(grep -c -F '[state ready] MARK-CS-ready' <<<"$out")" "0 1" "the ${VER[$k]} CLI shows the state as an ordinary message"
    fi
  done
  # a used-up turn limit: a closing state passes on a node that knows states, and counts as the
  # progress message it is (refused) on an older one. Each node is asked alone: write through it,
  # with the task made there.
  for k in "${!NAME[@]}"; do
    cli "$k" "$NEW" cs-boss/claude.b1 task cs-lap "turns MARK-CS-turns-${NAME[$k]}" --ttl 2h --max-turns 2 >"$W/cs.out" 2>/dev/null
    t=$(sent_id "$W/cs.out")
    cli "$k" "$NEW" cs-lap/codex.t1 claim "$t" --settle 0s >/dev/null 2>&1
    cli "$k" "$NEW" cs-lap/codex.t1 reply --re "$t" "the second turn MARK-CS-r-${NAME[$k]}" >/dev/null 2>&1
    cli "$k" "$NEW" cs-lap/codex.t1 state "$t" done "closing MARK-CS-close-${NAME[$k]}" >/dev/null 2>"$W/cs.err"; rc=$?
    [ "$rc" = 0 ] || rc=refused
    want=refused; has "$k" states && want=0
    check "$rc" "$want" "${NAME[$k]} (${VER[$k]}): done after the turn limit $([ $want = 0 ] && echo passes || echo is refused)"
  done
  check_converged "version vectors equal"
fi
fi # alltasks

scenario "shared spaces"
# A thread's shared space is a node's own log, apart from the message log: a node from before it
# answers 404 under /v1/share/ and its message log is untouched. Only this tree's node holds one here
# (its replication between nodes is the lab's and the unit tests'). Raw calls: the share CLI seals
# with keys a scratch HOME has none of, and what is checked is the node.
ST=$(id_of "${PORT[0]}" MARK-S1-new)
for i in "${!NAME[@]}"; do
  got=$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK" "http://127.0.0.1:${PORT[$i]}/v1/share/index?thread=$ST")
  feat=$(get "${PORT[$i]}" /v1/health | py 'import json,sys; print("yes" if "share" in (json.load(sys.stdin).get("features") or []) else "no")')
  want="404 no"; has "$i" share && want="200 yes"
  check "$got $feat" "$want" "${NAME[$i]} (${VER[$i]}) holds shared spaces iff it has them, and says so"
done
chunk=$(py 'import base64,os,json; print(json.dumps({"data": base64.b64encode(b"AVBS1"+os.urandom(4096)).decode()}))')
check "$(post "${PORT[0]}" "/v1/share/upload?thread=$ST" "$chunk")" 200 "new takes a sealed chunk"
SHA=$(py 'import json,sys; print(json.load(open(sys.argv[1]))["sha256"])' "$W/last.out")
ver=$(py 'import json,os,sys; print(json.dumps({"thread":sys.argv[1],"ver":{"id":os.urandom(16).hex(),"name":os.urandom(16).hex(),"kid":os.urandom(8).hex(),"chunks":[sys.argv[2]],"meta":"bWV0YQ==","by":"h0/compat.sh1"}}))' "$ST" "$SHA")
check "$(post "${PORT[0]}" /v1/share/put "$ver")" 200 "new takes a version of it"
check "$(py 'import json,sys; print(json.load(open(sys.argv[1]))["ver"]["num"])' "$W/last.out")" 1 "  numbered 1"
check "$(post "${PORT[0]}" "/v1/share/upload?thread=$ST" '{"data":"cGxhaW4gdGV4dCBwbGFpbiB0ZXh0IHBsYWluIHRleHQ="}')" 400 "new refuses a plaintext chunk"
# The CLI: through this tree's node it shares and reads back; through an older node it says the
# node holds no shared spaces and points at --via pipe, and stores nothing.
cli 0 "$NEW" h0/compat.sh1 send --to h9/compat.sh9 "share root MARK-SH-root" >"$W/sh.out" 2>/dev/null
SR=$(awk '$1=="sent"{print $2; exit}' "$W/sh.out")
printf 'shared text MARK-SH-text\n' >"$W/sh.txt"
# Who is in a thread is read from signed messages: with a node from before signing in the mesh no
# client signs, so the CLI refuses and says why.
allsign=1; for k in "${!NAME[@]}"; do has "$k" signing || allsign=; done
out=$(cli 0 "$NEW" h0/compat.sh1 share put "$SR" "$W/sh.txt" 2>&1); rc=$?
if [ -n "$allsign" ]; then
  check "$rc" 0 "new CLI shares a file through new"
  check "$(cli 0 "$NEW" h0/compat.sh1 share get "$SR" sh.txt 2>/dev/null)" "shared text MARK-SH-text" "new CLI reads it back through new"
else
  check "$rc $(grep -c 'first message is not signed' <<<"$out")" "1 1" "new CLI refuses to share in an unsigned thread (a node before 0.7.0 is in the mesh), saying why"
fi
converge
for ((i = 1; i < N; i++)); do
  out=$(cli "$i" "$NEW" h0/compat.sh1 share ls "$SR" 2>&1); rc=$?
  check "$rc $(grep -c 'holds no shared spaces' <<<"$out") $(grep -c -- '--via pipe' <<<"$out")" "1 1 1" "new CLI through ${NAME[$i]} (${VER[$i]}) says it holds no shared spaces, and what to do"
done
check_converged "message logs stay equal on all $N nodes"
check_quarantined 1 "quarantined nothing more"
scenario "the brakes tell a loop from progress"
# Each node is asked alone (written through it, with sessions of its own). A burst of 30 replies that
# each say something new: a node with the new brakes takes them all; one with the old pair brake
# (0.10.0: 8, 0.10.1 on: 24 in 2 minutes) refuses one before the 30th; older nodes have no brake.
# Counting up one number at a time: the old loop check refuses the third number, the new one takes
# it (numbers are words). The same thanks in other words: the new loop check refuses it within six.
words=(amber basalt cinder dahlia egret fennel garnet heron indigo juniper kestrel lichen mango
  nutmeg obsidian papaya quince russet sorrel tamarind umber vetiver wisteria xenon yarrow zinnia
  acorn bramble clover drizzle)
for k in "${!NAME[@]}"; do
  n=${NAME[$k]}; a=bk-$n/claude.a1; b=bk-$n/codex.b2
  cli "$k" "$NEW" "$a" send --to "$b" "burst root MARK-BK-$n" >"$W/bk.out" 2>/dev/null
  root=$(sent_id "$W/bk.out")
  if [ -z "$root" ]; then check "no root" sent "${NAME[$k]}: the burst starts"; continue; fi
  ok=0
  for i in $(seq 0 29); do
    from=$a; [ $((i % 2)) = 0 ] && from=$b
    cli "$k" "$NEW" "$from" reply --re "$root" "${words[$i]} ${words[$(((i * 7 + 3) % 30))]} report $i on ${words[$(((i * 11 + 5) % 30))]}" >/dev/null 2>&1 || break
    ok=$((ok + 1))
  done
  want=30; if has "$k" loopkey && ! has "$k" brakes; then want=refused; fi
  got=$ok; [ "$want" = refused ] && [ "$ok" -lt 30 ] && got=refused
  check "$got" "$want" "${NAME[$k]} (${VER[$k]}): 30 fast replies that each say something new $([ $want = 30 ] && echo all go || echo are stopped by the old pair brake)"

  a=ct-$n/claude.a1; b=ct-$n/codex.b2
  cli "$k" "$NEW" "$a" send --to "$b" "count with me MARK-CT-$n" >"$W/bk.out" 2>/dev/null
  root=$(sent_id "$W/bk.out"); ok=0
  for i in 1 2 3; do cli "$k" "$NEW" "$b" reply --re "$root" "$i" >/dev/null 2>&1 && ok=$((ok + 1)); done
  want=3; if has "$k" loopkey && ! has "$k" brakes; then want=2; fi
  check "$ok" "$want" "${NAME[$k]} (${VER[$k]}): counting 1 2 3 $([ $want = 3 ] && echo goes || echo is refused at the third number)"

  if has "$k" brakes; then
    a=th-$n/claude.a1; b=th-$n/codex.b2
    cli "$k" "$NEW" "$b" send --to "$a" "the summary MARK-TH-$n" >"$W/bk.out" 2>/dev/null
    root=$(sent_id "$W/bk.out"); at=0; i=0
    for line in "$a|Thanks! Let me know if you need anything else." "$b|You're welcome! Let me know if there is anything else you need." \
      "$a|Thanks again! Let me know if you need anything else at all." "$b|You're very welcome! Let me know if there is anything else you need." \
      "$a|Thanks! Do let me know if you need anything else." "$b|You are welcome! Let me know if there's anything else you need."; do
      i=$((i + 1))
      cli "$k" "$NEW" "${line%%|*}" reply --re "$root" "${line#*|}" >/dev/null 2>"$W/bk.err" || { at=$i; break; }
    done
    check "$at $(grep -c 'loop check' "$W/bk.err")" "5 1" "${NAME[$k]}: the same thanks in other words is refused by the loop check at the 5th reply"
  fi
done
check_converged "version vectors equal"

scenario "node logs"
for i in "${!NAME[@]}"; do check "$(grep -c -e '^panic:' -e 'fatal error:' "$W/${NAME[$i]}.log")" 0 "no panic in ${NAME[$i]}'s log"; done

echo
echo "== summary: new (this tree) + ${OLDV[*]}"
printf '  %-46s %5s %5s\n' scenario pass fail
for s in "${ORDER[@]}"; do printf '  %-46s %5s %5s  %s\n' "$s" "${SP[$s]}" "${SF[$s]}" "$([ "${SF[$s]}" = 0 ] && echo PASS || echo FAIL)"; done
echo "compat: $PASSES passed, $FAILS failed"
[ "$FAILS" = 0 ] || exit 1
if [ -n "$RECORD" ]; then
  scripts/compat-record.sh finish "$RECORD" "$FAILS" "$PASSES" "${#ORDER[@]}" "$NEW" "${OLDV[@]}" || { echo "compat: passed, but no record was written (see above)" >&2; exit 1; }
  cp scripts/compat-releases.txt "$(dirname "$RECORD")/compat-releases.txt" # the list this run was held to, for a signer with no git history
  echo "compat: record written to $RECORD"
fi
