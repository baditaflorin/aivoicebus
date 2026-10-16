#!/usr/bin/env bash
# Cuts a release in stages, so the heavy build and the signing key stay on
# different machines. See docs/releasing.md.
#
#   scripts/release.sh build  <outdir>   # on a BUILD HOST: stamped binaries (darwin and linux, arm64 and amd64) from VERSION
#   scripts/release.sh sums   <outdir>   # write <outdir>/SHA256SUMS for the binaries in it (CI does this; so does sign)
#   scripts/release.sh sign   <outdir>   # where the signing key lives: pack, sign, verify, SHA256SUMS
#   scripts/release.sh github <outdir>   # create GitHub release v<version> from <outdir>  (NOTES=<file> for the notes)
#   scripts/release.sh attach <outdir> [--publish]   # put manifest.json and manifest.sig on the DRAFT release CI built;
#                                        # the draft is published only with --publish (see docs/releasing.md)
#   scripts/release.sh stamp  <outdir>   # print "<version> <full commit>" the binaries were built as
#
# `sign` also refuses unless scripts/compat.sh passed for this very code (commit, tree, the release
# linux binary, every release in scripts/compat-releases.txt): it wants the record compat.sh --record
# wrote (default <outdir>/compat-pass.txt, or COMPAT_RECORD=<file>). To sign without it, give a
# reason: AIVOICEBUS_SKIP_COMPAT="<at least 10 characters>". That warns loudly and is written to
# <outdir>/OVERRIDES, and `github` puts it at the top of the release notes. `github` itself refuses
# unless a passing record or a recorded waiver for the commit is in <outdir>.
#
# then:  deploy/publish-release.sh <outdir> --apply   # put it on the mesh nodes
#
# Only `build` reads VERSION from the checkout. `sign` and `github` take the version and commit
# from the binaries themselves (their build stamp), because the checkout may have moved on since
# the build: a VERSION bump merged in between once packed the binaries of one version into a manifest that
# said the next. The GitHub release is created at the stamped commit, never at main.
set -euo pipefail
START=$PWD
cd "$(dirname "$0")/.."
# COMPAT_RECORD, like compat.sh --record, means where the person running this is, not the repo root
case ${COMPAT_RECORD:-} in "" | /*) ;; *) COMPAT_RECORD=$START/$COMPAT_RECORD ;; esac
STAGE="${1:?usage: release.sh build|sums|sign|github|attach|stamp <outdir>}"
OUT="${2:?usage: release.sh build|sums|sign|github|attach|stamp <outdir>}"
PUBLISH_FLAG="${3:-}"
REPO=baditaflorin/aivoicebus

die() { echo "release.sh $STAGE: $*" >&2; exit 1; }

# host_tool: the binary of the release built for THIS machine, which runs here.
host_tool() {
  local os arch
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$(uname -m)" in arm64 | aarch64) arch=arm64 ;; x86_64) arch=amd64 ;; *) die "unsupported arch $(uname -m)" ;; esac
  echo "$OUT/aivoicebus-$os-$arch"
}

# stamp: sets VER, SHORT (the commit stamped in) and COMMIT (its full sha) from the binaries in $OUT,
# never from this checkout. The binary for this machine is run (`aivoicebus version`, which prints
# what scripts/build.sh linked in); the others cannot run here, so each must carry the same release
# and commit strings (-X values are stored as plain bytes; -trimpath keeps them out of the build
# info). Refused: a dirty or unknown commit, a binary without those strings (built at another
# commit), a commit this checkout does not have, and VERSION at that commit saying otherwise.
stamp() {
  local tool said f n=0 atv
  tool=$(host_tool)
  [ -x "$tool" ] || die "no $tool to read the release from (the binary for this machine; build it with: scripts/release.sh build $OUT)"
  said=$("$tool" version) || die "$tool does not run"
  [[ "$said" =~ ^aivoicebus\ ([^ ]+)\ \(commit\ ([^,]+), ]] || die "$tool says '$said', not 'aivoicebus <release> (commit <commit>, ...)'"
  VER=${BASH_REMATCH[1]}; SHORT=${BASH_REMATCH[2]}
  [[ "$SHORT" =~ ^[0-9a-f]{7,40}$ ]] || die "the binaries were built from '$SHORT', not a clean commit; rebuild from a clean checkout"
  [ "$VER" != dev ] || die "$tool is not a release build (release 'dev'); build with scripts/release.sh build"
  for f in "$OUT"/aivoicebus-*; do
    [ -f "$f" ] || continue
    n=$((n + 1))
    # "<sha>-dirty" contains "<sha>", so a binary built from a modified tree would pass the check
    # below: refuse it by name. (-X values sit packed in the binary with no terminator, so this is
    # a test for the dirty marker, not for "followed by a non-word byte", which neighbouring strings
    # would break.)
    ! LC_ALL=C grep -a -q -F "$SHORT-dirty" "$f" || die "$(basename "$f") was built from a modified tree ($SHORT-dirty); rebuild from a clean checkout"
    LC_ALL=C grep -a -q -F "$SHORT" "$f" && LC_ALL=C grep -a -q -F "$VER" "$f" ||
      die "$(basename "$f") does not carry release $VER commit $SHORT, which $(basename "$tool") does: the binaries were not built together. Rebuild them all (scripts/release.sh build)"
  done
  COMMIT=$(git rev-parse --verify --quiet "$SHORT^{commit}") ||
    die "commit $SHORT (stamped in the binaries) is not in this checkout: git fetch origin, then run this again"
  if atv=$(git show "$COMMIT:VERSION" 2>/dev/null | tr -d '[:space:]') && [ -n "$atv" ] && [ "$atv" != "$VER" ]; then
    die "the binaries say release $VER, but VERSION at their commit $SHORT says $atv"
  fi
  echo "release $VER, commit $SHORT, read from $n binaries" >&2
}

# compat_required: prints the releases older than $VER that scripts/compat-releases.txt lists, one per
# line (nothing when there is none). The list that counts is the one in the code being signed, not
# the signer's checkout (which may be older or edited): it is read from the signed commit, else from
# the copy compat.sh left beside the record. Fails when no list can be read. Needs COMMIT and VER.
compat_required() {
  local rec=${COMPAT_RECORD:-$OUT/compat-pass.txt} list v
  if ! list=$(git show "$COMMIT:scripts/compat-releases.txt" 2>/dev/null); then
    [ -s "$(dirname "$rec")/compat-releases.txt" ] || { echo "release.sh: no scripts/compat-releases.txt at commit $SHORT and none beside $rec" >&2; return 1; }
    list=$(cat "$(dirname "$rec")/compat-releases.txt")
  fi
  printf '%s\n' "$list" | while read -r v _; do
    case $v in ("" | "#"*) continue ;; esac # the "(" matters: bash 3.2 cannot parse a case inside $( ) without it
    if [ "$v" != "$VER" ] && [ "$(printf '%s\n%s\n' "$v" "$VER" | sort -V | head -1)" = "$v" ]; then echo "$v"; fi
  done
}

# compat_na: the mixed-version gate has nothing to test because no older release exists (the first
# release of the project, or a list that names only this one and newer). A gate that cannot run is not
# a gate that failed; it is reported as not applicable, never silently as passed.
compat_na() {
  local req
  req=$(compat_required) || return 1
  [ -z "$req" ]
}

# compat_ok: the mixed-version gate (scripts/compat.sh) passed for exactly the code in the binaries:
# the commit stamped in them, its tree, the release linux binary compat.sh was run on (--new), and
# every release in scripts/compat-releases.txt older than this one. Needs COMMIT and VER (from stamp).
compat_ok() {
  local rec=${COMPAT_RECORD:-$OUT/compat-pass.txt} tree sha req
  tree=$(git rev-parse --verify --quiet "$COMMIT^{tree}") || return 1
  sha=$(scripts/compat-record.sh sha "$OUT/aivoicebus-linux-amd64") || return 1
  req=$(compat_required) || return 1
  # shellcheck disable=SC2086 # the versions have no spaces
  scripts/compat-record.sh check "$rec" "$COMMIT" "$tree" "$sha" $req
}

# compat_override: AIVOICEBUS_SKIP_COMPAT is a reason, not a switch: at least 10 printable
# characters once control characters and edge spaces are gone, so 0, false or a space do not count.
# It prints the reason sanitised, or fails.
compat_override() {
  local r
  r=$(printf '%s' "${AIVOICEBUS_SKIP_COMPAT:-}" | LC_ALL=C tr -d '\000-\037\177' | sed 's/^ *//;s/ *$//')
  [ "${#r}" -ge 10 ] || return 1
  printf '%s' "$r" | LC_ALL=C grep -q '[A-Za-z]' || return 1
  printf '%s\n' "$r"
}

# compat_gate (sign): the gate must have passed, or the person signing must give a reason, which is
# written into $OUT/OVERRIDES (and so into the release notes by `github`). Needs COMMIT (from stamp).
compat_gate() {
  local reason
  if [ -n "${AIVOICEBUS_SKIP_COMPAT:-}" ]; then
    reason=$(compat_override) || die "AIVOICEBUS_SKIP_COMPAT is a reason, at least 10 characters with a letter in it (not 0, false, or a space): say why the mixed-version gate is skipped"
    {
      echo "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
      echo "!! WARNING: signing release $VER WITHOUT a compat.sh pass for commit $SHORT"
      echo "!! reason given (AIVOICEBUS_SKIP_COMPAT): $reason"
      echo "!! it is recorded in $OUT/OVERRIDES and in the release notes"
      echo "!! nodes will replicate with the older releases untested by the mixed-version gate"
      echo "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
    } >&2
    printf 'release=%s commit=%s waived compat gate on %s by %s: %s\n' "$VER" "$COMMIT" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$(id -un 2>/dev/null || echo unknown)" "$reason" >"$OUT/OVERRIDES"
    return 0
  fi
  rm -f "$OUT/OVERRIDES" # a waiver from an earlier signing must not excuse this one
  if compat_na; then
    echo "release.sh sign: mixed-version gate not applicable: no release older than $VER is listed in scripts/compat-releases.txt" >&2
    return 0
  fi
  compat_ok || die "the mixed-version gate has not passed for this code (see above). On the build host, after build: scripts/compat.sh --new $OUT/aivoicebus-linux-amd64 --record $OUT/compat-pass.txt (COMPAT_RECORD=<file> to keep the record elsewhere); to sign anyway: AIVOICEBUS_SKIP_COMPAT=\"<why>\""
}

# compat_note (github): refuses unless the release dir carries a passing record for this code or a
# recorded waiver for this commit, and sets COMPAT_NOTE, the line the release notes start with.
compat_note() {
  if compat_na 2>/dev/null; then
    COMPAT_NOTE="Mixed-version gate: not applicable (no older release)."
  elif compat_ok 2>/dev/null; then
    COMPAT_NOTE="Mixed-version gate: passed (scripts/compat.sh, record $(basename "${COMPAT_RECORD:-$OUT/compat-pass.txt}"))."
  elif [ -s "$OUT/OVERRIDES" ] && grep -q " commit=$COMMIT " "$OUT/OVERRIDES"; then
    COMPAT_NOTE="**Mixed-version gate WAIVED:** $(sed 's/^release=[^ ]* commit=[^ ]* //' "$OUT/OVERRIDES")"
  else
    die "no passing compat record and no recorded waiver for commit $SHORT in $OUT (sign again after scripts/compat.sh, or with AIVOICEBUS_SKIP_COMPAT)"
  fi
}

# write_sums: <outdir>/SHA256SUMS, "<sha256>  <name>" for every binary, sorted by name, so the file made in
# CI, the one made where the key lives and the one `attach` compares are byte for byte the same.
write_sums() {
  (cd "$OUT" && for f in aivoicebus-*; do
    [ -f "$f" ] || continue
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$f"; else shasum -a 256 "$f"; fi
  done | LC_ALL=C sort -k2 >SHA256SUMS)
  [ -s "$OUT/SHA256SUMS" ] || die "no aivoicebus-* binaries in $OUT to sum"
}

# check_manifest: the signed manifest names the release and commit the binaries were built as.
check_manifest() {
  [ "$(manifest_field version)" = "$VER" ] || die "the manifest says release '$(manifest_field version)', the binaries $VER: sign again"
  local mc
  mc=$(manifest_field commit)
  [ -n "$mc" ] && [ "$COMMIT" = "$(git rev-parse --verify --quiet "$mc^{commit}" || true)" ] ||
    die "the manifest names commit '$mc', the binaries $SHORT: sign again"
}

# manifest_field <name>: a top-level string field of the signed manifest.
manifest_field() { sed -n "s/^  \"$1\": \"\([^\"]*\)\".*/\1/p" "$OUT/manifest.json" | head -1; }

case "$STAGE" in
  build)
    VER=$(tr -d '[:space:]' <VERSION)
    mkdir -p "$OUT"
    for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
      scripts/build.sh "${t%/*}" "${t#*/}" "$OUT/aivoicebus-${t%/*}-${t#*/}"
    done
    echo "built release $VER in $OUT. Next, where the signing key is: scripts/release.sh sign $OUT"
    ;;
  stamp)
    stamp
    echo "$VER $COMMIT"
    ;;
  sums)
    write_sums
    echo "wrote $OUT/SHA256SUMS ($(wc -l <"$OUT/SHA256SUMS" | tr -d ' ') binaries)"
    ;;
  attach)
    # CI built the binaries and left them on a DRAFT release; the signing key never goes there. Here
    # the draft is checked against what was signed, the signature is added, and only --publish makes it public.
    case "$PUBLISH_FLAG" in "" | --publish) ;; *) die "unknown option $PUBLISH_FLAG (only --publish)" ;; esac
    [ -s "$OUT/manifest.json" ] && [ -s "$OUT/manifest.sig" ] && [ -s "$OUT/SHA256SUMS" ] || die "$OUT is not signed (run: scripts/release.sh sign $OUT)"
    stamp
    check_manifest
    compat_note
    command -v gh >/dev/null 2>&1 || die "the gh command is needed (and \`gh auth login\`): it uploads to $REPO"
    draft=$(gh release view "v$VER" --repo "$REPO" --json isDraft --jq .isDraft 2>/dev/null) ||
      die "no release v$VER on $REPO: push the tag v$VER so CI builds the draft, then run this again"
    [ "$draft" = true ] || die "release v$VER on $REPO is not a draft (already published): nothing is changed. A published release is never edited here"
    have=$(gh release view "v$VER" --repo "$REPO" --json assets --jq '.assets[].name')
    for f in "$OUT"/aivoicebus-*; do
      [ -f "$f" ] || continue
      printf '%s\n' "$have" | grep -qx "$(basename "$f")" || die "the draft has no asset $(basename "$f"): the binaries you signed are not the ones CI built"
    done
    theirs=$(gh release download "v$VER" --repo "$REPO" -p SHA256SUMS -O -) || die "the draft has no SHA256SUMS to compare with"
    [ "$theirs" = "$(cat "$OUT/SHA256SUMS")" ] ||
      die "SHA256SUMS on the draft differ from the one made from the binaries you signed: you signed something other than what CI built. Download the draft's binaries (gh release download v$VER -D $OUT) and sign those"
    extra=()
    [ ! -s "${COMPAT_RECORD:-$OUT/compat-pass.txt}" ] || extra+=("${COMPAT_RECORD:-$OUT/compat-pass.txt}")
    [ ! -s "$OUT/OVERRIDES" ] || extra+=("$OUT/OVERRIDES")
    echo "attaching the signature to the draft v$VER (commit $SHORT) on $REPO"
    gh release upload "v$VER" --repo "$REPO" "$OUT/manifest.json" "$OUT/manifest.sig" ${extra[@]+"${extra[@]}"}
    if [ -n "${NOTES:-}" ] && [ -s "$NOTES" ]; then
      notes=$(mktemp "${TMPDIR:-/tmp}/aivoicebus-notes.XXXXXX")
      { printf '%s\n\n' "$COMPAT_NOTE"; cat "$NOTES"; } >"$notes"
      gh release edit "v$VER" --repo "$REPO" --notes-file "$notes"
      rm -f "$notes"
    fi
    if [ "$PUBLISH_FLAG" = --publish ]; then
      gh release edit "v$VER" --repo "$REPO" --draft=false
      echo "published v$VER"
    else
      echo "the release is still a DRAFT. Look at it, then: scripts/release.sh attach $OUT --publish (or publish it in the web UI)"
    fi
    ;;
  sign)
    # Signing uses the binary built for THIS machine, from the same source as the others.
    stamp
    compat_gate
    TOOL=$(host_tool)
    work=$(tr -d '[:space:]' <VERSION 2>/dev/null || true)
    [ "$work" = "$VER" ] || echo "note: this checkout's VERSION says ${work:-nothing}; signing what the binaries are: $VER" >&2
    echo "signing release $VER, commit $SHORT"
    "$TOOL" release pack --dir "$OUT" --version "$VER" --commit "$SHORT" ${NOTES_LINE:+--notes "$NOTES_LINE"}
    "$TOOL" release verify --dir "$OUT"
    write_sums
    echo "signed. Next: scripts/release.sh attach $OUT (the draft CI built) or scripts/release.sh github $OUT, then deploy/publish-release.sh $OUT --apply"
    ;;
  github)
    [ -s "$OUT/manifest.json" ] && [ -s "$OUT/manifest.sig" ] || die "$OUT is not signed (run: scripts/release.sh sign $OUT)"
    [ -n "${NOTES:-}" ] && [ -s "$NOTES" ] || die "set NOTES=<release notes file>"
    stamp
    check_manifest
    compat_note
    notes=$(mktemp "${TMPDIR:-/tmp}/aivoicebus-notes.XXXXXX")
    { printf '%s\n\n' "$COMPAT_NOTE"; cat "$NOTES"; } >"$notes"
    echo "GitHub release v$VER at commit $COMMIT"
    # the gate's evidence goes with the release: the pass record, and the waiver when there is one
    extra=()
    [ ! -s "${COMPAT_RECORD:-$OUT/compat-pass.txt}" ] || extra+=("${COMPAT_RECORD:-$OUT/compat-pass.txt}")
    [ ! -s "$OUT/OVERRIDES" ] || extra+=("$OUT/OVERRIDES")
    gh release create "v$VER" --repo "$REPO" --target "$COMMIT" --title "aivoicebus $VER" --notes-file "$notes" \
      "$OUT"/aivoicebus-* "$OUT/manifest.json" "$OUT/manifest.sig" "$OUT/SHA256SUMS" ${extra[@]+"${extra[@]}"}
    rm -f "$notes"
    ;;
  *) echo "unknown stage $STAGE" >&2; exit 2 ;;
esac
