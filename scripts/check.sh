#!/usr/bin/env bash
# The full pre-merge check: shell and unit lint, format, vet, race tests, a stamped
# build, the end-to-end smoke transcript compared with its golden file, and the mesh
# lab. It compiles and runs the race detector, so run it on a build host, not a
# laptop. CI runs exactly this and nothing else.
#
#   scripts/check.sh            # check
#   scripts/check.sh --update   # accept the current transcript as the new golden
#   AIVOICEBUS_SKIP_LAB=1 scripts/check.sh   # without the mesh lab (about two minutes of it)
#   AIVOICEBUS_REQUIRE_LINT=1 scripts/check.sh   # fail, not skip, when shellcheck/systemd-analyze are missing (CI)
set -euo pipefail
cd "$(dirname "$0")/.."

# The cheap checks first, so a mistake in a script or the unit fails in seconds, not after the
# race tests. Lint needs shellcheck (and systemd-analyze for the unit parse): a developer without
# them skips it, CI sets AIVOICEBUS_REQUIRE_LINT so a missing tool cannot pass silently.
if command -v shellcheck >/dev/null 2>&1 || [ -n "${AIVOICEBUS_REQUIRE_LINT:-}" ]; then
  scripts/lint.sh
else
  echo "lint skipped (no shellcheck)"
fi

bad=$(gofmt -l .)
[ -z "$bad" ] || { echo "gofmt needed on:"; echo "$bad"; exit 1; }
go vet ./...
go test -race -count=1 ./...

D=$(mktemp -d); trap 'rm -rf "$D"' EXIT
scripts/build.sh "$(go env GOOS)" "$(go env GOARCH)" "$D/aivoicebus"
scripts/smoke.sh "$D/aivoicebus" 2>/dev/null >"$D/smoke.txt"

# The mesh lab (scripts/lab.sh): real processes and the real deploy scripts. Linux only.
if [ "${1:-}" != "--update" ] && [ -z "${AIVOICEBUS_SKIP_LAB:-}" ]; then
  if [ "$(uname -s)" = Linux ]; then
    if scripts/lab.sh >"$D/lab.txt" 2>&1; then
      echo "mesh lab: $(grep -c '  PASS' "$D/lab.txt") checks passed"
    else
      cat "$D/lab.txt"; echo "the mesh lab failed"; exit 1
    fi
  else
    echo "mesh lab skipped (needs Linux; it runs on the build host)"
  fi
fi

if [ "${1:-}" = "--update" ]; then
  cp "$D/smoke.txt" scripts/smoke.golden.txt
  echo "golden updated: scripts/smoke.golden.txt"
  exit 0
fi
diff -u scripts/smoke.golden.txt "$D/smoke.txt" || { echo "smoke transcript differs from scripts/smoke.golden.txt (scripts/check.sh --update to accept)"; exit 1; }
echo "all checks passed"
