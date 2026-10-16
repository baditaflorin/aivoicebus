#!/usr/bin/env bash
# Builds aivoicebus stamped with the release (VERSION file) and the git commit.
#
#   scripts/build.sh <goos> <goarch> <out>
#   scripts/build.sh darwin arm64 dist/aivoicebus-arm64
#
# Every shipped binary goes through here, so `aivoicebus version` always says
# what it is. Pure Go, no cgo: it cross-compiles anywhere Go runs. Heavy builds
# belong on a build host, not a laptop.
set -euo pipefail
cd "$(dirname "$0")/.."
GOOS_="${1:?usage: build.sh <goos> <goarch> <out>}"
GOARCH_="${2:?usage: build.sh <goos> <goarch> <out>}"
OUT="${3:?usage: build.sh <goos> <goarch> <out>}"

VER=$(tr -d '[:space:]' <VERSION)
# AIVOICEBUS_COMMIT names the commit when there is no .git to ask (scripts/reproduce.sh builds from a
# `git archive` of a commit, which is clean by construction).
if [ -n "${AIVOICEBUS_COMMIT:-}" ]; then
  case "$AIVOICEBUS_COMMIT" in
    *[!0-9a-f]* | "") echo "AIVOICEBUS_COMMIT must be a lowercase hex commit id" >&2; exit 2 ;;
  esac
  COMMIT=$AIVOICEBUS_COMMIT
else
  COMMIT=$(git rev-parse --short=9 HEAD 2>/dev/null || echo unknown)
  if [ "$COMMIT" != unknown ] && [ -n "$(git status --porcelain 2>/dev/null)" ]; then COMMIT="$COMMIT-dirty"; fi
fi
PKG=github.com/baditaflorin/aivoicebus/internal/version
REL=github.com/baditaflorin/aivoicebus/internal/release
# A fork or company that signs its own releases builds its public key in (base64 ed25519, never a
# private key): AIVOICEBUS_TRUST_KEY=<key> scripts/build.sh ...  The official key stays trusted too.
EXTRA=""
if [ -n "${AIVOICEBUS_TRUST_KEY:-}" ]; then
  case "$AIVOICEBUS_TRUST_KEY" in
    *[!A-Za-z0-9+/=]*) echo "AIVOICEBUS_TRUST_KEY is not a base64 public key" >&2; exit 2 ;;
  esac
  EXTRA=" -X $REL.extraKey=$AIVOICEBUS_TRUST_KEY"
fi

mkdir -p "$(dirname "$OUT")"
# -trimpath and -buildvcs=false keep the checkout path and the VCS state out of the binary, so the same
# commit built by the same Go toolchain gives the same bytes (scripts/reproduce.sh checks that).
CGO_ENABLED=0 GOOS="$GOOS_" GOARCH="$GOARCH_" go build -trimpath -buildvcs=false \
  -ldflags="-s -w -X $PKG.Release=$VER -X $PKG.Commit=$COMMIT$EXTRA" -o "$OUT" .
echo "built $OUT  release=$VER commit=$COMMIT ($GOOS_/$GOARCH_)"
