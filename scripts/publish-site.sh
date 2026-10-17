#!/usr/bin/env bash
# Publishes site/ to the gh-pages branch, which GitHub Pages serves at
# https://baditaflorin.github.io/aivoicebus/. The site is static: nothing is built.
# Run from a clean checkout of main after the site/ change is pushed.
set -euo pipefail
cd "$(dirname "$0")/.."
[ -z "$(git status --porcelain site)" ] || { echo "publish-site.sh: site/ has uncommitted changes" >&2; exit 1; }
git push origin "$(git subtree split --prefix site HEAD)":refs/heads/gh-pages --force
echo "pushed site/ to gh-pages"
