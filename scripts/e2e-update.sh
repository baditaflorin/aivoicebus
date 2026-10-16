#!/usr/bin/env bash
# Rehearses a signed update end to end on THIS machine before a release is
# published: real signing key, the real release binaries, a real node serving
# them, an older client updating over the mesh path, then seven attacks, each
# of which must leave the installed binary untouched.
#
#   scripts/e2e-update.sh <dir with aivoicebus-<os>-<arch> for this machine> <older aivoicebus binary>
#
# It works on a COPY of the directory and a scratch HOME, and needs the signing
# key (default ~/.config/aivoicebus/release-signing.key, or $AIVOICEBUS_SIGNING_KEY).
# The older binary must report a lower release than the one under test.
# Needs bash and curl; it runs the binaries, so use the build for this OS/arch.
set -uo pipefail
REL_SRC="${1:?usage: e2e-update.sh <release dir> <older aivoicebus binary>}"
OLD="$(cd "$(dirname "${2:?usage: e2e-update.sh <release dir> <older aivoicebus binary>}")" && pwd)/$(basename "$2")"
KEY="${AIVOICEBUS_SIGNING_KEY:-$HOME/.config/aivoicebus/release-signing.key}"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in arm64 | aarch64) arch=arm64 ;; x86_64) arch=amd64 ;; *) echo "unsupported arch" >&2; exit 1 ;; esac
T=$(mktemp -d); H="$T/home"; mkdir -p "$H"
S="$T"; mkdir -p "$S/rel" && cp -R "$REL_SRC"/. "$S/rel/"
NEW="$S/rel/aivoicebus-$os-$arch"
[ -x "$NEW" ] || { echo "no $NEW" >&2; exit 1; }
[ -f "$KEY" ] || { echo "no signing key at $KEY" >&2; exit 1; }
NEWV=$("$NEW" version | awk '{print $2}'); OLDV=$("$OLD" version | awk '{print $2}')
PID=""; trap '[ -n "$PID" ] && kill "$PID" 2>/dev/null; rm -rf "$T"' EXIT
FAILED=0
pass() { echo "  PASS: $*"; }
fail() { echo "  FAIL: $*"; FAILED=1; }
check() { # check <description> <pattern> <output>
  if printf '%s' "$3" | grep -q -- "$2"; then pass "$1"; else fail "$1 (wanted /$2/ in:"; printf '%s\n' "$3" | sed 's/^/        /'; echo "      )"; fi
}
absent() { if printf '%s' "$3" | grep -q -- "$2"; then fail "$1 (found /$2/)"; else pass "$1"; fi; }

echo "== 1. sign the release with the real key; verify with the key compiled into the binary"
out=$("$NEW" release pack --dir "$S/rel" --key "$KEY"  2>&1); check "pack" "packed and signed release $NEWV" "$out"
out=$("$NEW" release verify --dir "$S/rel" 2>&1); check "verify with the compiled-in key (no --pubkey)" "release $NEWV verified" "$out"

echo "== 2. a node serving the release"
PORT=$((20000 + RANDOM % 20000)); TOKEN=e2e-token-0123456789-0123456789-xyz
printf '%s' "$TOKEN" >"$T/token"
"$NEW" serve --addr "127.0.0.1:$PORT" --data "$T/data" --token-file "$T/token" --node e2e --release-dir "$S/rel" >"$T/serve.log" 2>&1 &
PID=$!
for _ in $(seq 1 50); do curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/v1/release/manifest.json"); [ "$code" = 401 ] && pass "release files need the token (got $code)" || fail "no-token status $code"
hdr=$(curl -s -D - -o /dev/null -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/v1/who" | tr -d '\r' | grep -i '^x-aivoicebus-latest'); check "nodes announce the release" "$NEWV" "$hdr"

export AIVOICEBUS_URL="http://127.0.0.1:$PORT" AIVOICEBUS_TOKEN="$TOKEN" AIVOICEBUS_ME=e2e/agent HOME="$H"
cd "$T" || exit 1
mkdir -p "$H/.local/bin"; cp "$OLD" "$H/.local/bin/aivoicebus"; "$NEW" setup --yes >/dev/null 2>&1 || true; FM="$H/.local/bin/aivoicebus"
reset() { cp "$OLD" "$FM"; rm -f "$FM.prev"; }
installed() { "$FM" version 2>&1; }

echo "== 3. an older client notices, once"
check "old client version" "$OLDV" "$(installed)"
out=$("$FM" update --check 2>&1); check "update --check sees the release" "release $NEWV is available" "$out"
out=$("$FM" hook --tool claude 2>&1); check "hook mentions the release" "Release $NEWV is available" "$out"
out=$("$FM" hook --tool claude 2>&1); [ -z "$out" ] && pass "hook is silent the second time" || fail "hook repeated: $out"
out=$("$FM" doctor 2>&1); check "doctor warns about the release" "WARN  release" "$out"

echo "== 4. update over the mesh, then roll back"
out=$("$FM" update 2>&1); rc=$?; [ $rc = 0 ] && pass "update exit 0" || fail "update exit $rc"
check "installed the new release" "installed aivoicebus $NEWV" "$out"
check "the new binary ran setup" "claude hook" "$out"
check "now running the new release" "$NEWV" "$(installed)"
check "previous kept as .prev" "$OLDV" "$("$FM.prev" version 2>&1)"
[ -f "$H/.claude/settings.json" ] && pass "setup configured the Claude hook" || fail "no settings.json from setup"
out=$("$FM" update --check 2>&1); check "now up to date" "up to date" "$out"
out=$("$FM" update --rollback 2>&1); check "rollback" "rolled back to aivoicebus $OLDV" "$out"
check "running the old release again" "$OLDV" "$(installed)"

echo "== 5. attacks (the installed binary must not change, every time)"
mk() { rm -rf "$T/relX"; cp -R "$S/rel" "$T/relX"; reset; }
# shellcheck disable=SC2010 # counting our own staging file names, which are plain
unchanged() { check "installed binary untouched" "$OLDV" "$(installed)"; left=$(ls -A "$H/.local/bin" | grep -c '^\.aivoicebus-update-'); [ "$left" = 0 ] && pass "no staging files left" || fail "$left staging files left"; }

echo " a. binary tampered after signing"
mk; printf 'x' >>"$T/relX/aivoicebus-darwin-arm64"
out=$("$FM" update --from "dir:$T/relX" 2>&1); check "refused" "does not match the signed manifest" "$out"; unchanged

echo " b. manifest edited after signing"
mk; sed -i.bak "s/\"version\": \"$NEWV\"/\"version\": \"9.9.9\"/" "$T/relX/manifest.json"
out=$("$FM" update --from "dir:$T/relX" 2>&1); check "refused" "does not match any trusted key" "$out"; unchanged

echo " c. signed by a different (rogue) key"
mk; "$NEW" release keygen --out "$T/rogue.key" >/dev/null 2>&1
"$NEW" release pack --dir "$T/relX" --key "$T/rogue.key" >/dev/null 2>&1
out=$("$FM" update --from "dir:$T/relX" 2>&1); check "refused" "does not match any trusted key" "$out"; unchanged

echo " d. replay of an older signed release (downgrade)"
mk; "$NEW" release pack --dir "$T/relX" --key "$KEY" --version 0.0.1 >/dev/null 2>&1
out=$("$FM" update --from "dir:$T/relX" 2>&1); check "treated as up to date" "up to date" "$out"; unchanged

echo " e. correctly signed, but the binary is for another OS (a maintainer mistake)"
mk; cp "$S/rel/aivoicebus-linux-amd64" "$T/relX/aivoicebus-darwin-arm64"
"$NEW" release pack --dir "$T/relX" --key "$KEY" --version 9.8.1 >/dev/null 2>&1
out=$("$FM" update --from "dir:$T/relX" 2>&1); check "refused before the swap" "does not run here" "$out"; unchanged

echo " f. correctly signed, but the binary reports a different version than the manifest"
mk; "$NEW" release pack --dir "$T/relX" --key "$KEY" --version 9.8.5 >/dev/null 2>&1
out=$("$FM" update --from "dir:$T/relX" 2>&1); check "refused before the swap" "different version" "$out"; unchanged

echo " g. the node's token is wrong"
out=$(AIVOICEBUS_TOKEN=nope "$FM" update --check 2>&1); check "refused" "401" "$out"; check "points at the other source" "--from github" "$out"; unchanged

echo
[ "$FAILED" = 0 ] && echo "ALL CHECKS PASSED" || echo "SOME CHECKS FAILED"
exit $FAILED
