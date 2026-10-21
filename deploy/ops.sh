# shellcheck shell=bash
# Helpers shared by the operator scripts (install.sh, rotate-token.sh, machine-token.sh,
# rollout.sh, add-node.sh, remove-node.sh). Sourced, not run. Needs bash, curl and ssh, and the caller to
# have sourced deploy/nodes.sh (NODES, node_urls) and set PORT.
#
# Tokens only ever travel on stdin or in a file: curl gets its Authorization header from a
# config on stdin, so no token is ever in a process list.

# ssh_ok <alias>: the node answers ssh at all.
ssh_ok() { ssh -n -o ConnectTimeout=10 -o BatchMode=yes "$1" true 2>/dev/null; }

# all_reachable: every node of nodes.conf answers ssh. A step that must reach every node
# (a token change, a revoke) checks this BEFORE changing anything, so a node that is down is
# a refusal to start, not a node silently left behind.
all_reachable() {
  local n name alias bad=""
  for n in "${NODES[@]}"; do
    IFS=: read -r name alias _ <<<"$n"
    ssh_ok "$alias" || bad="$bad $name"
  done
  [ -z "$bad" ] || { echo "cannot reach:$bad. Nothing was changed. Bring them back (or take them out of deploy/nodes.conf on purpose) and run this again." >&2; return 1; }
}

# http_code <token-file> <url> [curl args]: the HTTP status, "000" when nothing answered.
http_code() {
  local f="$1" u="$2"; shift 2
  printf 'header = "Authorization: Bearer %s"\n' "$(tr -d '\n' <"$f" | head -c 200)" |
    curl -s -o /dev/null -w '%{http_code}' --max-time 5 -K - "$@" "$u" || true
}

# http_get <token-file> <url>: the body on success, nothing and status 1 otherwise.
http_get() {
  printf 'header = "Authorization: Bearer %s"\n' "$(tr -d '\n' <"$1" | head -c 200)" |
    curl -fsS --max-time 5 -K - "$2" 2>/dev/null
}

# log_total <health json>: how many records a node holds: the sum of the version vector it reports
# under "log" (records per origin, 0 for an answer with none). Compare nodes by this and not by
# "messages" plus "acks": a retraction is an ack to a release that predates retraction and is not counted among
# the acks by one that applies it (it is not a read receipt), so those two numbers differ between nodes that hold
# exactly the same records. Origins are node names, so no ':' or '}' is expected inside the object.
log_total() {
  local s=0 x
  for x in $(printf '%s' "$1" | grep -o '"log":{[^}]*}' | head -1 | grep -o ':[0-9][0-9]*' | tr -d :); do s=$((s + x)); done
  echo "$s"
}

# log_vector <health json>: the same version vector, one "origin count" line per origin, sorted.
# remove-node.sh compares nodes by it origin by origin: a sum can hide a node that is ahead on one
# origin and behind on another.
log_vector() {
  printf '%s' "$1" | grep -o '"log":{[^}]*}' | head -1 | sed 's/^"log":{//; s/}$//' | tr ',' '\n' |
    sed -n 's/^"\([^"]*\)":\([0-9][0-9]*\)$/\1 \2/p' | sort
}

# vector_missing <have> <want>: what a node holding the vector <have> lacks of <want> (both as
# log_vector prints them), one "origin missing-count" line per origin; nothing when it holds all of it.
vector_missing() {
  printf '%s\n%s\n' "$1" "--" "$2" | awk '$0 == "--" { w = 1; next } NF == 2 { if (w) want[$1] = $2; else have[$1] = $2 }
    END { for (o in want) if (want[o] > have[o] + 0) print o, want[o] - have[o] }' | sort
}

# vector_max <vector>...: the per-origin maximum of the vectors given: what the nodes hold between them.
vector_max() {
  printf '%s\n' "$@" | awk 'NF == 2 && (!($1 in m) || $2 > m[$1]) { m[$1] = $2 } END { for (o in m) print o, m[o] }' | sort
}

# accepts <token-file> <ip>: the node accepts this token (/v1/who exists in every release).
accepts() { [ "$(http_code "$1" "http://$2:${PORT:-7777}/v1/who")" = 200 ]; }

# quarantined_of <health json>: the node's "quarantined" count, 0 when the field is absent (a
# release before the replication check, which cannot quarantine anything). The count is permanent
# (the placeholder replicates and stays), so it is never a pass/fail test on its own: rollout.sh and
# watch.sh compare it with what the node held before, and verify_node ignores it.
quarantined_of() {
  local q
  q=$(printf '%s' "$1" | grep -o '"quarantined":[0-9]*' | head -1 | cut -d: -f2)
  echo "${q:-0}"
}

# verify_node <ip> <token-file> [expect-version]: the node answers with this token, runs the
# expected release when one is given, and every peer link of it is up. Retried for
# AIVOICEBUS_VERIFY_WAIT seconds (default 20): peer links need a replication tick after a
# restart. A node too old to have /v1/health is judged by an authenticated /v1/who only
# (and cannot be checked for a version).
verify_node() {
  local ip="$1" tf="$2" want="${3:-}" j v bad code
  for _ in $(seq 1 "${AIVOICEBUS_VERIFY_WAIT:-20}"); do
    code=$(http_code "$tf" "http://$ip:${PORT:-7777}/v1/health")
    case "$code" in
      200)
        j=$(http_get "$tf" "http://$ip:${PORT:-7777}/v1/health") || j=""
        v=$(printf '%s' "$j" | grep -o '"version":"[^"]*"' | head -1 | cut -d'"' -f4) # the node's own, before its peers'
        bad=$(printf '%s' "$j" | grep -o '"ok":false' | wc -l | tr -d ' ')
        if { [ -z "$want" ] || [ "$v" = "$want" ]; } && [ "$bad" = 0 ]; then return 0; fi ;;
      404)
        if [ -z "$want" ] && accepts "$tf" "$ip"; then return 0; fi ;;
    esac
    sleep 1
  done
  echo "   verify failed on $ip: answer ${code:-?}, version '${v:-?}'${want:+ (want $want)}, ${bad:-?} peer link(s) not ok" >&2
  return 1
}

# is_shared_token <token-file> <ip>: the node says this token is an unrestricted shared token
# (not a machine token, which may act only as one machine, and not the read-only observer).
# Returns 0 yes, 1 no, 2 the node cannot say (older than the release that added /v1/token).
is_shared_token() {
  local ip="$2" code body
  code=$(http_code "$1" "http://$ip:${PORT:-7777}/v1/token")
  case "$code" in
    200) body=$(http_get "$1" "http://$ip:${PORT:-7777}/v1/token") || return 2
         printf '%s' "$body" | grep -q '"host":""' && printf '%s' "$body" | grep -q '"observer":false' ;;
    404) return 2 ;;
    *) return 1 ;;
  esac
}

# gen_token <file>: 48 random characters, mode 600, never printed.
gen_token() {
  (umask 077
   until [ "$( { wc -c <"$1"; } 2>/dev/null || echo 0)" -ge 40 ]; do
     head -c 36 /dev/urandom | base64 | tr -d '\n=+/' | head -c 48 >"$1"
   done)
}
