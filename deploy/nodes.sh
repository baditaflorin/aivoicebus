# shellcheck shell=bash
# Sourced by the deploy scripts (bash). Reads deploy/nodes.conf into NODES, one
# "name:alias:ip" entry per node, and offers node_urls. AIVOICEBUS_NODES_CONF points
# it at another list (the lab, a staging mesh).
NODES=()
while IFS= read -r _line; do
  case "$_line" in '' | '#'*) ;; *) NODES+=("$_line") ;; esac
done <"${AIVOICEBUS_NODES_CONF:-$(dirname "${BASH_SOURCE[0]}")/nodes.conf}"
unset _line

# node_urls prints the comma-separated client URL list, e.g. http://ip1:7777,http://ip2:7777
node_urls() {
  local n ip
  for n in "${NODES[@]}"; do
    IFS=: read -r _ _ ip <<<"$n"
    printf 'http://%s:%s\n' "$ip" "${PORT:-7777}"
  done | paste -sd, -
}
