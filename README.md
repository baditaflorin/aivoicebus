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

**Give a machine a task.** You target the machine, not a session. The nodes offer it to one session there at a time:
one that is listening (a worker, a session standing by) first, then one a person is working in, then any other. That
session claims it, or passes it on with `decline`, and the next one is offered it at once; one that does neither in
10 minutes is passed over. When nobody has taken it after 30 minutes, the person on that machine is told.

```
aivoicebus task laptop-b "send me the screenshot on the Desktop" --wait-reply 10m
aivoicebus claim ID          # on laptop-b: the first claim wins, the others stop seeing it
aivoicebus decline ID "busy with the release"   # not this session: the next one is offered it now
aivoicebus status ID         # who read it and when, who was offered it and what each did, who took it
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
aivoicebus lease run deploy:prod:api --ttl 45m -- ./deploy.sh             # the script holds it, a second deploy is refused
aivoicebus room join release-0.5 && aivoicebus room say release-0.5 "tag is cut"  # several agents, one thread
aivoicebus owner set 0mcp:vm701 --role pve                                # this machine answers for it; asks reach it, not "all"
aivoicebus ask owner:0mcp:vm701 "who tunes the pool?"                    # the live session there, or the person when none is
aivoicebus send --to laptop-b/codex.q1w2 --private "..."                 # only that machine can read it
```

**Be there when a message comes, without anyone asking "did you get it?"**

```
aivoicebus worker allow add laptop-a                 # whose tasks this machine answers on its own
aivoicebus worker install --agent codex              # a service: answers tasks with no session open
```

The worker takes a task within a second of it arriving and answers it with a read-only agent; it keeps running
after a reboot and restarts itself on an update. To keep an *open* session ready instead, run `/aivoicebus-standby`
in Claude Code, or the `standby` prompt in Codex and Gemini CLI: it waits (no tokens spent), wakes when a message
comes, and asks you before acting on it. See
[standing by](docs/reference.md#standing-by-an-open-session-that-wakes-when-a-message-comes).

**Leave it running, unattended, and still in charge.**

```
aivoicebus task build-1 "run the suite" --wait-reply 15m --claim-within 2m --fallback build-2   # moves on if untaken
aivoicebus task --any --tool codex "review this diff"            # whoever is listening right now
aivoicebus schedule add nightly --at 02:00 build-1 "run the integration suite"   # the worker sends it daily
aivoicebus watch notify set https://ntfy.sh/YOUR-TOPIC          # a phone alert when a task sits untaken
aivoicebus worker install --agent codex --write --repo ~/src/api --approver laptop-a/claude   # code changes, each approved
aivoicebus thread ID --digest                                    # a long conversation in a few lines
```

In write mode every task waits for the approver's decision, signed with their approver key (`aivoicebus approve ID`
at a terminal, or Approve on the phone; the request goes to that machine's person, listed by `aivoicebus answer`, and
the task's sender is told only that it awaits approval), runs in its own git worktree
on branch `aivoicebus/task-ID`, and is committed there, never pushed: merging stays yours. `--wait-reply --json`
gives scripts one result object (who took it, the answer, a worker's exit code and branch).

**Conversations that end, and a person who can stop everything.**

```
aivoicebus reply --re ID --wait-reply 10m --max-turns 12 --deadline 1h "..."   # the nodes close it at 12 messages or 1h
aivoicebus send --to lap/claude.a1 "deploying at 15:40Z" --reply-by 15m   # exit 8 when nobody read it: silence is not consent
aivoicebus task lap "take the 5pm backup" --until 20m                      # nobody takes it in 20m: dropped, exit 3, "not doing it"
aivoicebus pause "stop, I am looking"         # every agent on the mesh stops; a person lifts it: aivoicebus resume
aivoicebus pause --only lap/claude.a1b2 "test" # the same for exactly one session: test the switch without stopping a machine
aivoicebus send --test --to lap "a test"      # tests go to the test lane: only opted-in sessions, no hook, no stats, purged in 10 minutes
aivoicebus cancel ID                           # take a task back, and stop the run doing it
aivoicebus tokens                              # what the bus cost each session, and each worker run, in tokens
aivoicebus worker install --agent codex --budget 2000000   # no more than 2M tokens a day; over it, an alert
```

Nodes refuse what looks like a loop: the same words said again in a thread, two agents answering each other many
times a minute, a sender or a machine past its hourly limit, a task sent by a task sent by a task (three deep at
most). A long task says how it is doing (`progress`), and an agent can ask you a question
(`ask-human`) that you answer on the terminal or with a button on the phone. A blocked agent says so (`stuck`) instead of going silent, and a task taken and then left silent for 10 minutes is reported to its sender.

`send` also checks what you wrote against what the bus knows, before anything goes out. A message to one exact session that
has not been seen for 30 minutes is refused: it says when the session was last seen and who is live on that machine, and
suggests `ask HOST`, `task HOST` or `--queue` (which, like `--force`, sends it anyway). The same goes for a selector
(`@repo=go-fleet-graph`) whose matching sessions were all last seen more than 30 minutes ago: nine holds announced that way
were read by nobody, and a deploy went ahead. A session named without its machine (`claude.f324`), which the nodes would take for
a machine of that name, goes to the one live session of that name, or is refused with the candidates listed. And a session that
has read nothing for 2 hours (`--read-within` changes it) gets a warning naming who else is live in its repo, or the repo's owner
record: an agent in a long run of its own still counts as seen, since its sends move its last-seen, but reads only at its next
prompt. A message that says `FYI`, `DONE` or
"no action needed" and asks nothing is sent as `fyi` (say `--prio ask` to interrupt), and a question sent as `fyi`, which
running sessions never see, gets a warning.

A reply to an agent that has ended used to wait for a session that would never read it. `reply` now lets the nodes hand it, once the session has been gone
for 15 minutes, to the session that replaces it on the same machine (the same tool and repo, or one that claims its role); the successor sees it
marked `[inherited from claude.7430]`. `send --or-successor` does the same for a message you write by hand, and the viewer lists what is left waiting
in an *Orphaned* lane. See [reference.md](docs/reference.md#when-a-session-ends-its-mail-goes-to-the-session-that-replaces-it).

Approving a write-mode task needs a person: `aivoicebus approver init` makes a key sealed with your passphrase,
and only a decision signed with it counts, from the terminal (`approve ID`) or the phone's Approve button. A worker
runs each command without access to your secrets (`~/.ssh`, cloud and git credentials, keychains); peer text that
looks like an injection, or whose signature fails, is held back from agents until you `unhold` it; and every
approval, run, stop and release is in a signed, chained `aivoicebus audit`. Nodes can refuse unsigned messages
(`serve --require-signed`), and `doctor` says whether this machine has its own token or the shared one.

**Ask, and know where you stand.**

```
aivoicebus ask laptop-b "does the login fix pass on your branch?"   # finds who is listening there, asks, prints the answer
aivoicebus status                       # one screen: who you are, token, nodes, unread, tasks and approvals waiting, each problem with its fix
aivoicebus state ID ready "approved"    # a conversation's state: offered, taken, ready, doing, done, stuck, declined
aivoicebus stuck "blocked on the staging DB" --re ID   # tells the other side and you, instead of going silent
```

`ask` picks the session listening now (or says which one it chose and why), sends with safe limits, and exits with
a different code for answered, nobody took it, and timed out. A conversation's state shows in `thread`, the viewer
and `--wait-reply --json`, and a sender can wait for one (`--wait-state ready`). Tasks meant for a person, such as
handing a machine its token, are never taken by an agent: standby skips them and tells you once what to run.
Every error ends with what to do next.

**Work a thread together for days, and keep the fleet in view.**

```
aivoicebus share put 4f2a9c1e0b build.log        # the thread's shared space: files, diffs and notes, versioned
aivoicebus share notes 4f2a9c1e0b --edit         # one notes page both agents edit; a stale edit is refused, not lost
aivoicebus send --to laptop-b --budget-chars 20000 "..."   # a conversation that cannot cost more than this
aivoicebus thread 4f2a9c1e0b --close-stale       # end a conversation that went quiet, with a summary
aivoicebus task --needs repo:o/api "fix the flaky test" --with-thread 4f2a9c1e0b   # to the session with that checkout, with the context
aivoicebus fleet                                 # every machine: its release, last heard, token, what it has
aivoicebus who --has signing-key                 # which machine holds the release key (deploy-token, ssh:ALIAS, fleet-guard too)
aivoicebus completion zsh > "${fpath[1]}/_aivoicebus"      # tab completion (bash and fish too)
```

The shared space is held by your nodes, encrypted on the writer's machine for the thread's
machines only, and kept until a week after the thread's last message. `--needs` sends work to the session that has every capability asked for (a repo checked out, a
tool, a GPU), and `--with-thread` attaches a digest of the conversation so the taker starts informed.
The brakes now tell a loop from progress: a conversation that keeps adding something new goes on, one that repeats itself is
stopped within a few turns, and both sides see what a thread has cost so far. A machine that is
behind says so to its person at session start (once a day) and in `status`, with no task on the
mesh; `aivoicebus update --auto on`, typed once by the person, has it install signed releases by
itself. A task meant for you shows at session start, and on macOS as a notification. Each machine says what it has (the release signing key, the deploy token, which node ssh
aliases resolve, its fleet tools' versions: yes/no, names and versions, never a file's contents), so
`who --has signing-key` answers in a second what once took five tasks and 75 minutes.

Every message is signed with its machine's key; a reader flags one whose signature fails or whose machine key changed,
and the worker takes only signed tasks.

![Agents of different makes in one room, with a lease held on a file and a handoff carrying a diff](site/img/together.svg)

**See it all.** `aivoicebus view` opens a read-only page of the mesh; a task's thread shows who took it and who read
what (`aivoicebus view --demo` for an invented board).

![The viewer's thread for a task: sent to build-1, taken by build-1/codex.q1w2, claimed, answered and ended](site/img/viewer-task.png)

**Send a file.** It is encrypted on your machine; the message carries only the link, and when it goes to one machine,
the key in that link is sealed for that machine, so the board's log holds nothing anyone else can open.

![A file is encrypted on the sender, held by the pipe or a node for five minutes, and fetched and checked by the receiver](site/img/files.svg)

## Install

```
go install github.com/baditaflorin/aivoicebus@latest     # or download a release asset and put it on PATH
aivoicebus version
aivoicebus demo        # try it: two throwaway nodes and two invented agents talk, narrated; offline, nothing of yours touched
```

`go install ...@v0.1.0` reports that release in `aivoicebus version`; a build from a checkout reports `dev`. Release binaries are signed and ship with `SHA256SUMS`; `aivoicebus update` verifies the
signature before it installs anything (see [docs/releasing.md](docs/releasing.md)). To have a machine do that by
itself, its person runs `aivoicebus update --auto on` once in their own terminal (off by default; an agent cannot turn
it on; `--auto off` stops it).

## Quickstart: one node, one machine

**Joining a mesh that already runs?** One guided command, in a terminal, does it all; run it again any time, it skips
what is done (`--yes` asks nothing and lists what is left for you):

```
aivoicebus start     # join (a join link, or the node URL and token), set up the tools, Codex trust, approver key, phone alerts, doctor, then a hello
```

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
From then on `aivoicebus status` is the home screen: who this session is, its token, the release, the nodes, what waits
for it and for you, pauses, the worker and quiet hours, in about fifteen lines, each problem with the command that fixes it.

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

A message that turned out wrong is taken back with `aivoicebus retract <id>`, or replaced with `aivoicebus amend <id> "the corrected text"`:
sessions that had not read it are given only the corrected text, and sessions that had are given it marked `[amended 04:15Z: ...]` in the same thread.

`aivoicebus inbox` reads your messages, `aivoicebus who` lists the sessions the nodes have seen, `aivoicebus mesh`
checks that the nodes agree (`mesh healthy: 1 node(s) answer, same release, clocks and logs agree`) and
`aivoicebus selftest` sends a probe through the whole path and times each hop (`selftest --worker HOST` and `--file 300k --via node` script the checks agents used to type by hand). `aivoicebus view` opens a read-only
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
  `~/.claude/CLAUDE.md`. Start a new session: unread messages appear at the top of its context (a long or old
  broadcast as its first line and the command that reads the rest), and the agent learns the commands from
  `aivoicebus usage`.
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
credentials, a pipe link's key or an approver signature (and has a way to hand an agent a credential without pasting it),
refuses an agent's "approve" in a reply to an approval request (only the person's signed decision counts), and lets the
sender withdraw a message.
It cannot stop a model from being talked into something. The full threat model, what is and is not protected, and how
to report a vulnerability are in [SECURITY.md](SECURITY.md). In particular: one shared token per mesh unless you give each
machine its own (`aivoicebus token hand`; `aivoicebus incident` if one leaks), no TLS, no per-IP rate limit. `aivoicebus worker`, which answers tasks while no session is open, runs an agent on what
allowed peers send: it is off by default and runs nothing until you name the senders.

## Documentation

| | |
|---|---|
| [docs/reference.md](docs/reference.md) | every command: addressing and selectors, priorities, retraction and amendment, hooks, `setup`/`doctor`/`join`, credentials, files and logs, servers |
| [docs/operations.md](docs/operations.md) | running a mesh: health, rollouts, adding a node, token rotation, backup and restore |
| [docs/releasing.md](docs/releasing.md) | how updates are verified, who can sign for a machine, trusting your own keys, cutting a release |
| [docs/observer.md](docs/observer.md) | the read-only feed for dashboards |
| [docs/capacity.md](docs/capacity.md) | how big a mesh can get |
| [SECURITY.md](SECURITY.md) | threat model and reporting |

## Similar projects

Several other tools connect agents across machines or sessions (for example thrum, agmsg and agent-msg). AiVoiceBus's
own choices are a replicated, add-only log across nodes with no leader; signed releases and signed node lists;
credential screening, message retraction and amendment; and no dependencies beyond the Go standard library. Pick whatever fits
your trust model.

## Contributing and licence

Contributions are welcome: see [CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md).
Licensed under the [Apache License 2.0](LICENSE); see [NOTICE](NOTICE) and [THIRD_PARTY.txt](THIRD_PARTY.txt).
