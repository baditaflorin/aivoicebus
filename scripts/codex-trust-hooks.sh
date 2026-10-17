#!/usr/bin/env bash
# Trusts the hooks `aivoicebus setup` put in ~/.codex/hooks.json, which Codex runs only after a person
# trusts them once. It shows the hooks, starts Codex in your home directory with your keyboard attached,
# and after you quit it checks ~/.codex/config.toml to see whether the trust was saved. Run it in a terminal:
#
#   bash scripts/codex-trust-hooks.sh
#
# In Codex's "Hooks need review" screen press 2 (Trust all and continue) and Enter, then type /quit.
# "Trust all" trusts every hook Codex lists as new or changed, not only aivoicebus's, so read the list
# printed first. (Codex's keys are not typed for you: under a pty driver such as expect it draws nothing
# and takes no keys, and trusting hooks is your decision anyway.)
set -euo pipefail
codex=$(command -v codex || true)
[ -n "$codex" ] || codex=/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex
[ -x "$codex" ] || { echo "no codex found (PATH or ChatGPT.app)" >&2; exit 1; }
cfg=$HOME/.codex/config.toml
[ -s "$HOME/.codex/hooks.json" ] || { echo "no ~/.codex/hooks.json: run 'aivoicebus setup' first" >&2; exit 1; }

echo "== hooks Codex will ask you to trust (~/.codex/hooks.json):"
cat "$HOME/.codex/hooks.json"
before=$(grep -c trusted_hash "$cfg" 2>/dev/null || true)
echo
echo "== Codex starts now. In \"Hooks need review\": press 2, then Enter. Then type /quit and Enter."
echo "   (No review screen means nothing waits for review: just /quit.)"
printf "   Press Enter to start Codex... "
read -r _
cd "$HOME"
"$codex" || true   # your terminal as it is: Codex fails on redirected input or output

after=$(grep -c trusted_hash "$cfg" 2>/dev/null || true)
echo
if [ "${after:-0}" -gt "${before:-0}" ]; then
  echo "== ok: Codex saved trust for $(( after - before )) more hook(s) in ~/.codex/config.toml ($after in all)."
elif [ "${after:-0}" -gt 0 ]; then
  echo "== nothing new saved; $after hook(s) were trusted already."
else
  echo "== no trust saved (0 trusted hooks in ~/.codex/config.toml). Rerun this and pick 2 in the review screen."
  exit 1
fi
