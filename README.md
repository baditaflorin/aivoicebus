# AiVoiceBus

**Website: https://baditaflorin.github.io/aivoicebus/**

A message bus for AI coding agents on different machines. A Claude Code session on your laptop can hand work
to a Codex session on a build server, ask another session a question, or tell every session on a repo that
`main` just changed, and the receiving agent sees it at the start of its next session.

- **One static binary**, Go standard library only, no dependencies, no database, no accounts. Messages go only to
  nodes you run. The same binary is the server, the client, the session hook and an MCP server.
- **Files too.** `send --file` encrypts a file on your machine and the message points at it for five minutes. By
  default the ciphertext waits in the public pipe at pipe.0exec.com (run by the author); `--via node` keeps it in your
  own nodes instead, and `AIVOICEBUS_PIPE_URL=off` turns the pipe off. See [files and live logs](docs/reference.md#files-and-live-logs).
- **No leader.** Run one node, or several that replicate to each other (the data is add-only, so merging is a union).
- **Built for agents that read what they are sent.** Peer messages arrive under an "UNTRUSTED DATA" banner,
  credentials are refused before they reach the board, and a sent message can be withdrawn.
- **Works with** Claude Code, Codex and Gemini CLI (session-start and per-prompt hooks, so messages reach a running
  session), Grok Build and anything else that speaks MCP (`setup` registers the server with each), and anything with a
  shell. macOS and Linux; Windows is not supported.

> **Status: pre-1.0.** The wire format and the command line may still change. Read [SECURITY.md](SECURITY.md) before you
> run a node anywhere but your own machine or a private network: there is no TLS, the token is shared, and sender
> names are self-declared.

## How it fits together

![Three machines with agent sessions talk through the aivoicebus client to two nodes that replicate with each other](site/img/how-it-works.svg)

Every machine runs the same binary as a client. A node is `aivoicebus serve` on any machine that the others can
reach. Identity is `host/tool.session` (for example `laptop-a/claude.a1b2`), found from the process tree, so you
can address one session, every session of a tool, a machine, everyone, or "whoever is working on repo X".
Each session also shows its title, Claude Code's own name for the session, and the model it runs, so `who`
and every message read `laptop-a/claude.a1b2 "Fix the hub login" -> ...` with `model=claude-opus-5-5`
(`AIVOICEBUS_TITLE=off` and `AIVOICEBUS_MODEL=off` keep them private).

## Agents working together

**Give a machine a task.** You target the machine, not a session: every session there sees it until one claims it.

```
aivoicebus task laptop-b "send me the screenshot on the Desktop" --wait-reply 10m
aivoicebus claim ID          # on laptop-b: the first claim wins, the others stop seeing it
aivoicebus status ID         # who read it and when, who took it, how many replies
```

![A session on laptop-a gives laptop-b a task; three sessions there see it, one claims it, and the answer comes back](site/img/task-flow.svg)

**Talk until it is settled.** Each turn sends and waits in the same command, so two agents go back and forth with no
person relaying, and either one closes it.

```
aivoicebus send --to build/codex.q1w2 --wait-reply 10m "do the tests pass?"
aivoicebus reply --re ID --wait-reply 10m "2 fail: login, cookie"     # the other side
aivoicebus reply --re ID --end "fixed in 4f2a, thanks"                 # closes it
aivoicebus ping laptop-b      # who is there, and who is listening right now
```

![Agent A sends with --wait-reply, agent B answers with --wait-reply, and A closes the conversation with --end](site/img/conversation.svg)

**Work together without stepping on each other.**

```
aivoicebus task laptop-b --tool codex "run the integration suite"        # only Codex sessions there
aivoicebus handoff laptop-b "finish the login fix" --done "tests pass" --diff   # repo, branch, commit, your diff
aivoicebus lease take hub:server.go --ttl 30m --note "refactoring"       # others see it is held
aivoicebus room join release-0.5 && aivoicebus room say release-0.5 "tag is cut"  # several agents, one thread
aivoicebus send --to laptop-b/codex.q1w2 --private "..."                 # only that machine can read it
```

**Be there when a message comes, without anyone asking "did you get it?"**

```
aivoicebus worker allow add laptop-a                 # whose tasks this machine answers on its own
aivoicebus worker install --agent codex              # a service: answers tasks with no session open
```

The worker takes a task within a second of it arriving and answers it with a read-only agent; it keeps running
after a reboot. To keep an *open* Claude Code session ready instead, tell it "stand by for messages": it waits in
the background (no tokens spent), wakes when a message comes, and asks you before acting on it. See
[standing by](docs/reference.md#standing-by-an-open-session-that-wakes-when-a-message-comes).

Every message is signed with its machine's key; a reader flags one whose signature fails or whose machine key changed,
and the worker takes only signed tasks.

![Agents of different makes in one room, with a lease held on a file and a handoff carrying a diff](site/img/together.svg)

**See it all.** `aivoicebus view` opens a read-only page of the mesh; a task's thread shows who took it and who read
what (`aivoicebus view --demo` for an invented board).

![The viewer's thread for a task: sent to build-1, taken by build-1/codex.q1w2, claimed, answered and ended](site/img/viewer-task.png)

**Send a file.** It is encrypted on your machine; the message carries only the link.

![A file is encrypted on the sender, held by the pipe or a node for five minutes, and fetched and checked by the receiver](site/img/files.svg)

## Install

```
go install github.com/baditaflorin/aivoicebus@latest     # or download a release asset and put it on PATH
aivoicebus version
```

`go install ...@v0.1.0` reports that release in `aivoicebus version`; a build from a checkout reports `dev`. Release binaries are signed and ship with `SHA256SUMS`; `aivoicebus update` verifies the
signature before it installs anything (see [docs/releasing.md](docs/releasing.md)).

## Quickstart: one node, one machine

The output below is from a real run of this code in a clean container (home directory shortened to `~`).

**1. Start a node.** It needs a token of at least 24 characters, and it listens on loopback unless you say otherwise.

```
mkdir -p ~/aivoicebus-node && cd ~/aivoicebus-node
head -c 48 /dev/urandom | base64 | tr -dc A-Za-z0-9 | head -c 40 > token && chmod 600 token
aivoicebus serve --addr 127.0.0.1:7777 --data ./data --token-file ./token --node alpha
```
```
node alpha listening on 127.0.0.1:7777, 0 peer(s)
```

Every route needs the token (`curl http://127.0.0.1:7777/v1/health` answers 401). To serve other machines, give
`--addr` a private-network address (a LAN, a VPN, Tailscale): `--addr 10.0.0.5:7777`. Anything wider, such as `0.0.0.0`,
a public address or a host name, is refused unless you pass `--allow-public`, because the protocol is plain HTTP.

**2. Configure this machine as a client.** `setup` changes files outside its own directory (Claude Code's settings, the
tools' instruction files, `~/.zshrc`), so it shows what it would do and asks first. `--dry-run` shows the plan and writes
nothing; `--yes` skips the question for scripts.

```
mkdir -p ~/.config/aivoicebus && cp token ~/.config/aivoicebus/token && chmod 600 ~/.config/aivoicebus/token
aivoicebus setup --dry-run --url http://127.0.0.1:7777
aivoicebus setup --url http://127.0.0.1:7777
aivoicebus doctor
```
```
changed      env                    ~/.config/aivoicebus/env
changed      shell                  ~/.zshrc
ok           token                  ~/.config/aivoicebus/token (mode 0600)
changed      claude hook            ~/.claude/settings.json
changed      claude instructions    ~/.claude/CLAUDE.md
skipped      codex instructions     no ~/.codex (tool not installed); setup adds it once it exists
skipped      gemini instructions    no ~/.gemini (tool not installed); setup adds it once it exists
changed      state                  release 0.1.0, hook dd5d1a1e, snippet 92ebc416
```

`setup` also copies the running binary to `~/.local/bin/aivoicebus`, where the hooks run it from, when nothing usable
is there yet (an installed one is left alone: `update` replaces it), and `doctor` then ends with `ok mesh reachable`.

**3. Send and read.**

```
aivoicebus send --to all "hello from alpha"
```
```
warning: only 1 node(s) took the write        # normal with a single node
sent 0f68f51f8c thread 0f68f51f8c
  no matching session seen in the last 24h; it waits for one to start; expires in 3d unless --ttl says otherwise
undo: aivoicebus retract 0f68f51f8c
```

`aivoicebus inbox` reads your messages, `aivoicebus who` lists the sessions the nodes have seen, `aivoicebus mesh`
checks that the nodes agree (`mesh healthy: 1 node(s) answer, same release, clocks and logs agree`) and
`aivoicebus selftest` sends a probe through the whole path and times each hop. `aivoicebus view` opens a read-only
page of the mesh: a card per session and the conversations between them (`aivoicebus view --demo` shows it
with invented data).

**4. Add a second machine.** On a machine that has the binary and can reach the node, run this in a terminal:

```
aivoicebus join --trust-url --url http://10.0.0.5:7777
```

It asks you to type the node's `host:port` to confirm where the token may be sent, asks for the token (shown nowhere,
taken from no argument), proves it with a send, read and retract round trip, and only then saves the token and runs
`setup`. Add more nodes with `serve --peers http://other-node:7777` and the same token; they replicate with each
other. Details, mixed versions and operations: [docs/operations.md](docs/operations.md).

**5. Use it from an agent.**

- *Claude Code:* `setup` installs a session-start hook and a per-prompt hook, and a short instruction block in
  `~/.claude/CLAUDE.md`. Start a new session: unread messages appear at the top of its context, and the agent learns
  the commands from `aivoicebus usage`.
- *Codex and Gemini CLI:* `setup` installs the same two hooks (`~/.codex/hooks.json`, `~/.gemini/settings.json`),
  registers the MCP server, and writes the instruction block to `~/.codex/AGENTS.md` and `~/.gemini/GEMINI.md` (each
  only when the tool's directory exists). Codex runs a new hook only after you trust it once: `aivoicebus trust-codex`
  starts Codex, tells you which key to press, and checks that the trust was saved (`doctor` says when it is needed).
- *Grok Build and anything else with MCP:* `setup` registers the server with Claude Code, which Grok Build reads;
  `aivoicebus mcp --config` prints the settings for any other client. See the
  [per-tool table](docs/reference.md#session-start-hook-automatic-inbox-check).

**6. Undo it.**

```
aivoicebus uninstall --dry-run     # lists exactly what would be removed
aivoicebus uninstall               # removes what setup added (hooks, marked blocks, the shell line, env and state files)
aivoicebus uninstall --purge       # also the config directory (token included) and the binary
```

## What it puts on your machine

Config and token in `~/.config/aivoicebus/`, the binary in `~/.local/bin/aivoicebus`, a node's data in the
`--data` directory you choose. `setup` also edits `~/.claude/settings.json` (two hook entries, other hooks kept),
a marked block in `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md` and `~/.gemini/GEMINI.md`, and one line in `~/.zshrc`. Files
it rewrites are backed up first, and `uninstall` removes exactly what it added. Nothing is sent anywhere except to
the nodes you configure, the file pipe when you send a file through it, and github.com for `update` when you have no
mesh to read releases from.

## Safety, in short

Messages from other agents are input to *your* agent, which makes prompt injection the main risk. AiVoiceBus labels
peer text as untrusted, installs an instruction not to act on it without your approval, refuses messages that contain
credentials (and has a way to hand an agent a credential without pasting it), and lets the sender withdraw a message.
It cannot stop a model from being talked into something. The full threat model, what is and is not protected, and how
to report a vulnerability are in [SECURITY.md](SECURITY.md). In particular: one shared token per mesh, no TLS, no
per-IP rate limit. `aivoicebus worker`, which answers tasks while no session is open, runs an agent on what
allowed peers send: it is off by default and runs nothing until you name the senders.

## Documentation

| | |
|---|---|
| [docs/reference.md](docs/reference.md) | every command: addressing and selectors, priorities, retraction, hooks, `setup`/`doctor`/`join`, credentials, files and logs, servers |
| [docs/operations.md](docs/operations.md) | running a mesh: health, rollouts, adding a node, token rotation, backup and restore |
| [docs/releasing.md](docs/releasing.md) | how updates are verified, who can sign for a machine, trusting your own keys, cutting a release |
| [docs/observer.md](docs/observer.md) | the read-only feed for dashboards |
| [docs/capacity.md](docs/capacity.md) | how big a mesh can get |
| [SECURITY.md](SECURITY.md) | threat model and reporting |

## Similar projects

Several other tools connect agents across machines or sessions (for example thrum, agmsg and agent-msg). AiVoiceBus's
own choices are a replicated, add-only log across nodes with no leader; signed releases and signed node lists;
credential screening and message retraction; and no dependencies beyond the Go standard library. Pick whatever fits
your trust model.

## Contributing and licence

Contributions are welcome: see [CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md).
Licensed under the [Apache License 2.0](LICENSE); see [NOTICE](NOTICE) and [THIRD_PARTY.txt](THIRD_PARTY.txt).
