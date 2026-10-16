#!/usr/bin/env bash
# Static checks for the shell scripts and the systemd unit. scripts/check.sh calls this.
#
#   scripts/lint.sh                        # shellcheck + the unit checks
#   AIVOICEBUS_REQUIRE_LINT=1 scripts/lint.sh   # a missing shellcheck or systemd-analyze is a failure (CI)
#
# Without AIVOICEBUS_REQUIRE_LINT, a missing systemd-analyze (macOS) skips only the parse check; the
# directive check below needs nothing but bash.
set -euo pipefail
cd "$(dirname "$0")/.."
require=${AIVOICEBUS_REQUIRE_LINT:-}
fail=0

# ---- 0. the operator scripts run on a Mac, whose /bin/bash is 3.2: nothing that needs bash 4
# (associative arrays, mapfile/readarray, case conversion, |&, &>>, namerefs, [[ -v ]]) may be in them.
# `scripts/lint.sh bash3` runs only this check (the lab calls it); it needs nothing installed.
bash4='declare -A|typeset -A|mapfile|readarray|\$\{[A-Za-z_0-9]+(\^\^|,,|\^|,)[}]|\|&|&>>|coproc|local -n|declare -n|\[\[ -v |\$\{!?[A-Za-z_0-9]+@[UuLlQEPAa]\}'
# (scripts/compat.sh is deliberately not in this list: it is Linux-only, runs on a build host and
# uses mapfile and associative arrays; it refuses to start anywhere else. compat-record.sh and
# release.sh, which a Mac runs, are in it.)
b3files=$(find deploy scripts/release.sh scripts/compat-record.sh scripts/build.sh scripts/e2e-update.sh scripts/smoke.sh -type f -name '*.sh' | sort)
# shellcheck disable=SC2086 # the names have no spaces
if hits=$(grep -nE "$bash4" $b3files | grep -vE '^[^:]+:[0-9]+:[[:space:]]*#'); then
  echo "$hits" >&2; echo "lint: bash 4 constructs in scripts that must run on bash 3.2 (see scripts/lint.sh section 0)" >&2; fail=1
else
  echo "lint: no bash 4 constructs in $(echo "$b3files" | wc -l | tr -d ' ') operator scripts"
fi
# A grep cannot see parser differences (bash 3.2 rejects `case x in a) ...;; esac` inside $( ) unless
# the pattern is written (a)), so the files are also parsed: with /bin/bash when it is a 3.x (a Mac),
# else with the bash at hand, which still catches plain syntax errors.
parser=bash
if [ -x /bin/bash ] && /bin/bash --version 2>/dev/null | grep -q 'version 3\.'; then parser=/bin/bash; fi
parsefiles=$b3files
for f in scripts/*.sh; do [ "$f" = scripts/compat.sh ] || parsefiles="$parsefiles $f"; done
# shellcheck disable=SC2046,SC2086 # file names have no spaces
for f in $(printf '%s\n' $parsefiles | sort -u); do
  if ! err=$("$parser" -n "$f" 2>&1); then echo "$err" >&2; echo "lint: $f does not parse under $parser" >&2; fail=1; fi
done
if [ "${1:-}" = bash3 ]; then [ "$fail" = 0 ] || exit 1; exit 0; fi

# ---- 1. shellcheck, warnings and above, over every script we ship or run
command -v shellcheck >/dev/null 2>&1 || { echo "lint: shellcheck is not installed" >&2; exit 1; }
files=$(find deploy scripts -type f \( -name '*.sh' -o -path 'scripts/lab/bin/*' \) | sort)
# shellcheck disable=SC2086 # the names have no spaces; word splitting builds the argument list
if shellcheck -S warning $files; then
  echo "lint: shellcheck clean ($(echo "$files" | wc -l | tr -d ' ') files)"
else
  fail=1
fi

# ---- 2. the unit still parses
# systemd-analyze exits 0 for an unknown key or a bad value (it only warns on stderr), so any
# output at all counts as a failure. It also refuses a unit whose ExecStart binary is missing,
# and the node's binary is not on a build machine: parse a copy that points at one that exists.
unit=deploy/aivoicebus.service
if command -v systemd-analyze >/dev/null 2>&1; then
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  stand_in=$(type -P true)
  sed "s#^ExecStart=/usr/local/bin/aivoicebus #ExecStart=$stand_in #" "$unit" >"$tmp/aivoicebus.service"
  grep -q "^ExecStart=$stand_in " "$tmp/aivoicebus.service" || { echo "lint: $unit no longer starts /usr/local/bin/aivoicebus; update scripts/lint.sh" >&2; fail=1; }
  if out=$(systemd-analyze verify "$tmp/aivoicebus.service" 2>&1) && [ -z "$out" ]; then
    echo "lint: systemd-analyze verify accepts $unit"
  else
    echo "$out" >&2; echo "lint: systemd-analyze verify rejects $unit" >&2; fail=1
  fi
elif [ -n "$require" ]; then
  echo "lint: systemd-analyze is not installed (AIVOICEBUS_REQUIRE_LINT is set)" >&2; fail=1
else
  echo "lint: unit parse check skipped (no systemd-analyze)"
fi

# ---- 3. the unit's hardening is still all there
# The mesh lab's systemctl stand-in reads only ExecStart, so nothing else would notice a dropped
# line. Each must appear exactly once in [Service] with exactly this value. Changing one on
# purpose means changing it here in the same commit.
service=$(awk '/^\[/ { s = $0 } s == "[Service]"' "$unit")
expect=(
  'Type=simple'
  'LoadCredential=token:/etc/aivoicebus/token'
  'Restart=always'
  'RestartSec=5'
  'DynamicUser=yes'
  'StateDirectory=aivoicebus'
  'StateDirectoryMode=0700'
  'NoNewPrivileges=yes'
  'ProtectSystem=strict'
  'ProtectHome=yes'
  'PrivateTmp=yes'
  'PrivateDevices=yes'
  'ProtectKernelTunables=yes'
  'ProtectKernelModules=yes'
  'ProtectControlGroups=yes'
  'RestrictAddressFamilies=AF_INET AF_INET6'
  'RestrictNamespaces=yes'
  'LockPersonality=yes'
  'MemoryDenyWriteExecute=yes'
  'CapabilityBoundingSet='
  'MemoryMax=256M'
)
for line in "${expect[@]}"; do
  key=${line%%=*}
  n=$(grep -c "^$key=" <<<"$service" || true)
  if [ "$n" != 1 ]; then
    echo "lint: $unit has $n '$key=' lines in [Service], want exactly 1: $line" >&2; fail=1
  elif ! grep -qxF "$line" <<<"$service"; then
    echo "lint: $unit changed: want '$line', have '$(grep "^$key=" <<<"$service")'" >&2; fail=1
  fi
done
# The token reaches the process only through the credential, never on the command line.
grep '^ExecStart=' <<<"$service" | grep -qF -- ' --token-file %d/token ' \
  || { echo "lint: $unit: ExecStart must read the token from %d/token (the credential)" >&2; fail=1; }

[ "$fail" = 0 ] || { echo "lint failed" >&2; exit 1; }
echo "lint: ${#expect[@]} unit directives in place"
echo "lint passed"
