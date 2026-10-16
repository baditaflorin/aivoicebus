#!/usr/bin/env bash
# Checks that published binaries are what the source says: rebuilds a commit in the pinned Go image
# and compares every binary's sha256 with the SHA256SUMS that came with the release.
#
#   scripts/reproduce.sh <git-ref> <release-dir>     # e.g. scripts/reproduce.sh v0.1.0 dist/release
#   scripts/reproduce.sh <git-ref>                   # only builds twice and compares the two (determinism check)
#
# It needs docker and the image named in scripts/build-image, and builds four binaries, so run it on
# a build host. The commit is built from `git archive`, so the working tree cannot leak in.
# Exit 0: every binary matches. Exit 1: a mismatch (printed). Exit 2: usage or setup problem.
set -euo pipefail
cd "$(dirname "$0")/.."
REF="${1:?usage: reproduce.sh <git-ref> [<release-dir>]}"
DIR="${2:-}"
IMG=$(tr -d '[:space:]' <scripts/build-image)
command -v docker >/dev/null || { echo "reproduce.sh: docker is needed" >&2; exit 2; }
COMMIT=$(git rev-parse --verify --quiet "$REF^{commit}") || { echo "reproduce.sh: $REF is not a commit here" >&2; exit 2; }
SHORT=$(git rev-parse --short=9 "$COMMIT")

sums() { # sums <dir>: "<sha256>  <name>" for each binary, sorted by name
  (cd "$1" && for f in aivoicebus-*; do
    if command -v sha256sum >/dev/null; then sha256sum "$f"; else shasum -a 256 "$f"; fi
  done | sort -k2)
}

work=$(mktemp -d "${TMPDIR:-/tmp}/aivoicebus-reproduce.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/src"
git archive "$COMMIT" | tar -x -C "$work/src"

build() { # build <outdir>
  mkdir -p "$1"
  docker run --rm --network none --user "$(id -u):$(id -g)" -v "$work/src":/src:ro -v "$1":/out -w /src \
    -e AIVOICEBUS_COMMIT="$SHORT" -e GOFLAGS=-buildvcs=false -e HOME=/tmp -e GOCACHE=/tmp/gocache \
    "$IMG" scripts/release.sh build /out >/dev/null
}

echo "rebuilding $REF ($SHORT) in $IMG"
build "$work/a"
sums "$work/a" >"$work/a.sums"
cat "$work/a.sums"

if [ -z "$DIR" ]; then
  build "$work/b"
  sums "$work/b" >"$work/b.sums"
  if diff -u "$work/a.sums" "$work/b.sums" >/dev/null; then
    echo "two clean builds of $SHORT are byte-identical ($(wc -l <"$work/a.sums" | tr -d ' ') binaries)"
    exit 0
  fi
  diff -u "$work/a.sums" "$work/b.sums" || true
  echo "reproduce.sh: two builds of the same commit differ" >&2
  exit 1
fi

[ -s "$DIR/SHA256SUMS" ] || { echo "reproduce.sh: no $DIR/SHA256SUMS" >&2; exit 2; }
# the release's sums, in the same shape (the file may use "*name" for binary mode)
sed 's/ \*/  /' "$DIR/SHA256SUMS" | sort -k2 >"$work/released.sums"
if diff -u "$work/released.sums" "$work/a.sums" >/dev/null; then
  echo "every binary in $DIR/SHA256SUMS matches a clean rebuild of $SHORT"
  exit 0
fi
diff -u "$work/released.sums" "$work/a.sums" || true
echo "reproduce.sh: the release does not match a rebuild of $SHORT (a different toolchain, commit or tree)" >&2
exit 1
