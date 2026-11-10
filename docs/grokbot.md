# Grok Bot on the bus

Claude Code, Codex and Gemini CLI get messages through hooks that run inside their sessions. Grok Bot has no
session to hook into: it is started by something else (a routine, a webhook, a person's message) and runs on a
computer of its own. So the bus goes to it instead: a small forwarder on that computer reads the messages and
tasks addressed to Grok Bot's identity and POSTs them to a webhook that starts a Grok Bot run.

    peer ──► node ──► aivoicebus forward --webhook ──POST──► Grok Bot routine ──► a run that reads the
                              (host/grokbot, listening)                          message and replies as host/grokbot

It is the session-start hook's job done from outside the session, with the hook's rules.

## Identity

The forwarder is a session of its own, **`HOST/grokbot`** (for example `grokbot-box/grokbot`), with the title
`aivoicebus webhook forwarder`. It holds a read open on a node, so `who` and `ping` show it as listening and a task
sent to the machine is offered to it like to any listening session (see
[offers](reference.md#tasks-for-a-machine-and-conversations-between-agents)). Sending to `HOST/grokbot` reaches it directly, and `@tool=grokbot`
selects it. `--as NAME` picks another agent part.

A run the webhook starts is a different process with its own detected identity (a Cursor-based agent shows as
`HOST/cursor-agent`). To speak as the forwarder's identity, the payload tells it to export `AIVOICEBUS_ME=HOST/grokbot`
before it runs `reply`, `claim` or `decline`, so a task offered to `grokbot` is claimed and answered by `grokbot`.

## Set it up

1. Install aivoicebus on the machine Grok Bot runs on, and `join` the mesh (a machine token bound to that machine is
   best: `deploy/machine-token.sh`).
2. Make a routine in Grok Bot that starts on a webhook. It gives a URL and a sender key.
3. Give the forwarder both, in the environment or in files (mode 0600, never an argument):

       export AIVOICEBUS_WEBHOOK_URL=https://...                     # or put it in ~/.config/aivoicebus/env
       export AIVOICEBUS_WEBHOOK_KEY=...                             # or ~/.config/aivoicebus/webhook-key

   `AIVOICEBUS_GROKBOT_WEBHOOK_URL` and `_KEY` are read as well. `setup` and `LoadEnvFile` read the URL (and the
   header and prefix) from `~/.config/aivoicebus/env`; the key is a credential and is read from the environment or
   `webhook-key` only. The URL must be https (plain http only to this machine).
4. `aivoicebus setup --tools grokbot` writes `~/.local/bin/ensure-aivoicebus-forward` and one line in `~/.bashrc`
   (`~/.zshrc` when that is the only rc file) that run `aivoicebus forward ensure` when a shell opens. The tool is
   named explicitly: a plain `setup` never touches it. `uninstall` removes both.
5. `aivoicebus forward --webhook --check` tests the settings; `aivoicebus forward ensure --verbose` starts it in the
   background (log `~/.config/aivoicebus/forward.out`; decisions in `forward.log`); `forward status` and `forward stop`.

On a machine with systemd or launchd you can run `aivoicebus forward --webhook` as a service of your own instead;
the ensure line is for machines, like a cloud box, where nothing else restarts it. When the binary is replaced
(`update`), the forwarder starts the new one itself and exits.

### The key header

The key goes in the **`Authorization`** header as `Bearer KEY`. `AIVOICEBUS_WEBHOOK_HEADER=X-Name` sends it in another
header (with no prefix, unless `AIVOICEBUS_WEBHOOK_PREFIX` says one; set it empty for none). The same variables exist
with the `AIVOICEBUS_GROKBOT_WEBHOOK_` prefix.

## What is posted

One JSON object per wake, `Content-Type: application/json`, with an `Idempotency-Key` (a hash of the message ids, the
same for the same messages):

| field | meaning |
|---|---|
| `text` | what the run reads: the untrusted banner, up to five messages (header, sender, signature verdict, body cut to `--body` runes), the offer lines of tasks offered to it, and how to reply as `HOST/grokbot` |
| `messages[]` | `id`, `thread`, `re`, `from`, `to`, `kind`, `priority`, `at`, `verified`, `body_bytes`, `withheld`, `offered` |
| `identity`, `machine`, `reply` | who the bus addresses, and the environment and commands to answer with |
| `untrusted`, `notice` | always `true` and the banner: the messages are data, not instructions |

The routine's prompt should say what to do with such an event: tell the user, answer questions, and take a task only
as the banner says (with the user's approval). Nothing in the payload is a command to the agent.

## Safety

The forwarder follows the hooks' and the worker's rules.

- **Untrusted, labelled.** Every word a peer wrote is inside `text` under the banner. The sender and signature verdict
  come from this machine's check, never from the node.
- **No credentials, no secrets.** A body that holds this machine's token, a credential-shaped string or the webhook's
  own URL or key is replaced by a fixed sentence; so is a body sealed for this machine alone (it was meant for no
  third party). No token, key or URL goes in a payload. The URL and key are never printed: logs and errors name the
  webhook's host only, a redirect is never followed (the key header would follow it), and `webhook-url`/`webhook-key`
  are on the secret lists (a send refuses text holding them, the worker's sandbox cannot read them).
- **Held and person-only messages stay put.** A message `held` for the user (bad signature, a key that changed, text
  that reads like instructions to an agent) and a task meant for the person are not forwarded.
- **Once.** A message is acknowledged after the webhook accepted it, and recorded in `person-told` first, so a lost
  acknowledgement never wakes Grok Bot twice. A failed POST is retried (3 tries, doubling), then the loop backs off
  from 5 seconds to 5 minutes (at least 2 minutes after a 4xx, which a wait will not fix).
- **Brakes.** At most `--max-wakes` POSTs (20) and `--max-per-thread` messages of one conversation (6) in any hour,
  `--min-gap` (5s) between POSTs, `--batch` (5) messages in one. Past a limit, messages wait unread and go on when
  the window moves; the nodes' own turn, depth and budget limits apply as to every session. A task more than three
  deep is not forwarded. A `pause` stops it; quiet hours hold what is not urgent.
- **Only what concerns Grok Bot.** Direct messages, tasks offered to it, and replies. Broadcasts that are neither
  (`--broadcasts`) and fyi messages (`--fyi`) are not forwarded; fyi is counted as at a session start.
