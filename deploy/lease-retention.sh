#!/usr/bin/env bash
# Strict, non-secret installer knob. Never source node.env or execute its values.
# validate VALUE | check-env FILE (prints at most one canonical assignment).
set -euo pipefail
valid_args() {
  local value=$1 hosts host seen=','
  [ "${#value}" -le 4096 ] || return 1
  case "$value" in
    ''|'--lease-retention off'|'--lease-retention observe') return 0 ;;
    '--lease-retention observe --lease-retention-hosts '*) hosts=${value#'--lease-retention observe --lease-retention-hosts '} ;;
    '--lease-retention on --lease-retention-hosts '*) hosts=${value#'--lease-retention on --lease-retention-hosts '} ;;
    *) return 1 ;;
  esac
  [[ "$hosts" =~ ^[a-z0-9][a-z0-9._-]{0,62}(,[a-z0-9][a-z0-9._-]{0,62})*$ ]] || return 1
  local -a list
  IFS=',' read -r -a list <<<"$hosts"
  [ "${#list[@]}" -le 64 ] || return 1
  for host in "${list[@]}"; do
    case "$seen" in *",$host,"*) return 1 ;; esac
    seen="$seen$host,"
  done
}
refuse() { echo 'lease retention: invalid or duplicate canonical configuration' >&2; exit 2; }
case ${1:-} in
  validate)
    [ $# -eq 2 ] || refuse
    valid_args "$2" || refuse
    ;;
  check-env)
    [ $# -eq 2 ] || refuse
    file=$2
    [ ! -L "$file" ] || refuse
    [ -e "$file" ] || exit 0
    [ -f "$file" ] && [ "$(wc -c <"$file")" -le 65536 ] || refuse
    count=0;kept=''
    while IFS= read -r line || [ -n "$line" ]; do
      if [[ "$line" =~ ^[[:space:]]*LEASE_RETENTION_ARGS([[:space:]]|=) ]]; then
        count=$((count + 1));[ "$count" = 1 ] || refuse
        case "$line" in LEASE_RETENTION_ARGS=*) ;; *) refuse ;; esac
        valid_args "${line#LEASE_RETENTION_ARGS=}" || refuse
        kept=$line
      fi
    done <"$file"
    [ -z "$kept" ] || printf '%s\n' "$kept"
    ;;
  *) refuse ;;
esac
