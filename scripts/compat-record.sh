#!/usr/bin/env bash
# The compat pass record: the one file that says "scripts/compat.sh passed for this code", written
# by compat.sh and checked by `release.sh sign` and `release.sh github`, so a release cannot be
# signed on the strength of a gate that never ran, ran on other code, or ran against too little.
# Plain key=value lines, no JSON, so bash 3 can read it.
#
#   compat-record.sh begin  FILE                  # compat.sh start: a stale record never outlives a run
#   compat-record.sh finish FILE FAILS CHECKS SCENARIOS NEWBIN [OLD...]
#                                                 # compat.sh end: writes the record only for a full pass of a clean tree
#   compat-record.sh check  FILE COMMIT TREE BINSHA [REQUIRED_OLD...]
#                                                 # release.sh: exit 0 only for a pass of exactly this code and set
#   compat-record.sh sha    FILE                  # sha256 of a file, whichever tool this machine has
#
# finish takes the commit and tree from git (the tree must be unmodified); without a .git (a build
# host that got the tree by rsync) COMPAT_COMMIT and COMPAT_TREE must be passed together, computed
# by whoever checked the tree out clean: git rev-parse HEAD and git rev-parse 'HEAD^{tree}'.
# check refuses a record for another commit, tree or binary (the release linux binary is the one
# compat.sh must have been run on: --new), too few scenarios or checks, and any REQUIRED_OLD release
# the run did not include (scripts/compat-releases.txt). It is an honesty check against forgetting
# or shortcutting the gate, not protection from someone who writes the file by hand: whoever signs
# can always say so with the override documented in release.sh.
set -euo pipefail

MIN_SCENARIOS=10   # compat.sh's scenario count; fewer means a truncated run
CHECKS_PER_NODE=8  # a full run does well over this per node (the old releases plus the new one)

die() { echo "compat-record: $*" >&2; exit 1; }
field() { sed -n "s/^$2=//p" "$1" | head -1; } # FILE KEY
sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
hex() { [[ "$2" =~ ^[0-9a-f]{$1}$ ]]; } # LENGTH VALUE

case "${1:-}" in
  begin)
    [ $# -eq 2 ] || die "usage: begin FILE"
    rm -f "$2"
    ;;
  sha)
    [ $# -eq 2 ] && [ -f "$2" ] || die "usage: sha FILE"
    sha "$2"
    ;;
  finish)
    [ $# -ge 6 ] || die "usage: finish FILE FAILS CHECKS SCENARIOS NEWBIN [OLD...]"
    file=$2 fails=$3 checks=$4 scen=$5 newbin=$6; shift 6
    [ "$fails" = 0 ] || die "the run had $fails failing checks: no record"
    [[ "$checks" =~ ^[0-9]+$ ]] && [ "$checks" -gt 0 ] || die "a pass record needs the number of checks that passed (got '$checks')"
    [[ "$scen" =~ ^[0-9]+$ ]] && [ "$scen" -gt 0 ] || die "a pass record needs the number of scenarios (got '$scen')"
    [ -f "$newbin" ] || die "$newbin is not a file"
    if git rev-parse --verify --quiet HEAD >/dev/null 2>&1; then
      [ -z "$(git status --porcelain 2>/dev/null)" ] || die "the tree has uncommitted changes: a pass must be for a commit, no record"
      commit=$(git rev-parse HEAD) tree=$(git rev-parse 'HEAD^{tree}')
    else
      commit=${COMPAT_COMMIT:-} tree=${COMPAT_TREE:-}
      [ -n "$commit" ] && [ -n "$tree" ] || die "no git checkout: pass COMPAT_COMMIT and COMPAT_TREE (git rev-parse HEAD and 'HEAD^{tree}' of the clean tree this was built from)"
    fi
    hex 40 "$commit" || die "'$commit' is not a full commit sha"
    hex 40 "$tree" || die "'$tree' is not a full tree hash"
    mkdir -p "$(dirname "$file")"
    {
      echo "# aivoicebus compat pass record (scripts/compat.sh); release.sh sign and github check it"
      echo "commit=$commit"
      echo "tree=$tree"
      echo "binary_sha256=$(sha "$newbin")"
      echo "result=pass"
      echo "checks=$checks"
      echo "scenarios=$scen"
      echo "old=$*"
      echo "at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    } >"$file.tmp"
    mv "$file.tmp" "$file"
    ;;
  check)
    [ $# -ge 5 ] || die "usage: check FILE COMMIT TREE BINSHA [REQUIRED_OLD...]"
    file=$2 commit=$3 tree=$4 binsha=$5; shift 5
    [ -s "$file" ] || die "no compat pass record at $file"
    [ "$(field "$file" result)" = pass ] || die "$file does not say result=pass"
    got=$(field "$file" commit)
    [ "$got" = "$commit" ] || die "$file is for commit '${got:-none}', not $commit"
    got=$(field "$file" tree)
    [ "$got" = "$tree" ] || die "$file is for tree '${got:-none}', not $tree (the code differs from what compat.sh tested)"
    got=$(field "$file" binary_sha256)
    [ "$got" = "$binsha" ] || die "$file tested a binary with sha256 '${got:-none}', not the release's $binsha: run compat.sh --new <the release's linux binary>"
    old=" $(field "$file" old) "
    for v in "$@"; do
      case "$old" in *" $v "*) ;; *) die "the compat run did not include release $v (it ran against:${old% }); scripts/compat-releases.txt lists the set the gate must cover" ;; esac
    done
    scen=$(field "$file" scenarios) checks=$(field "$file" checks)
    [[ "$scen" =~ ^[0-9]+$ ]] && [ "$scen" -ge "$MIN_SCENARIOS" ] || die "the record has '${scen:-no}' scenarios, a full run has at least $MIN_SCENARIOS"
    want=$((CHECKS_PER_NODE * ($(echo "$old" | wc -w) + 1)))
    [[ "$checks" =~ ^[0-9]+$ ]] && [ "$checks" -ge "$want" ] || die "the record has '${checks:-no}' checks, a run against ${old% } has at least $want"
    ;;
  *) echo "usage: compat-record.sh begin|finish|check|sha ..." >&2; exit 2 ;;
esac
