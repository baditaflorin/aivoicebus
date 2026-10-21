# Operating the mesh

Everything here is a script in `deploy/`, a command of the binary, or a check in
`aivoicebus mesh`. The scripts that change nodes are **dry runs unless you pass
`--apply`**, touch one node at a time, and stop at the first node that does not come back
healthy. None of them prints a token. The node list is `deploy/nodes.conf`.

Commands that start with `deploy/` or `scripts/` are files in this repository: run them from the
root of an aivoicebus checkout (`git clone https://github.com/baditaflorin/aivoicebus && cd aivoicebus`),
not from any other directory, where the shell says `no such file or directory`.

## Is it healthy?

    aivoicebus mesh            # every node: release, clock, records, peer links; lists what is wrong
    aivoicebus mesh --json

It reports an unreachable node, a peer link that is down or behind, a node that holds fewer
records than the most complete one, clocks that disagree (message expiry uses node
clocks), nodes on different releases, and a node approaching its memory limit
([capacity](capacity.md)). Exit status 1 when anything is wrong. Each node also serves
`/metrics` (Prometheus: records, sessions, per-peer lag and skew, requests per token) and
`/v1/health`, both readable with the observer token. Requests are counted per token and told
apart by fingerprint (`shared-ab12cd`), so before retiring a token you can see whether anything
still uses it.

## If something leaked

A token, key or secret was in a message, a log or a screenshot. Run one command, on the machine
you deploy from, in your own terminal:

    aivoicebus incident shared              # the mesh's shared token
    aivoicebus incident machine laptop-a    # the machine token of laptop-a
    aivoicebus incident observer            # the dashboard's read-only token
    aivoicebus incident secret github       # a credential saved with `aivoicebus secret save github`
    aivoicebus incident message 3fa9c0e1d2  # a message held one, and it is none of these
    #   add --msg ID (repeatable) for messages you know held it

By default it only **plans**, and changes nothing: it shows

1. how the credential is revoked or rotated, the deploy script it runs and that script's dry run
   (`deploy/rotate-token.sh add` then `promote` for the shared token, `deploy/machine-token.sh
   revoke HOST` for a machine token), or, marked `[you]`, what only you can do (revoke a saved
   secret where it was issued; the observer token is re-made with `deploy/install.sh
   --gen-observer-token` and a full install);
2. the messages that held it: those you named, and every message on the nodes whose text holds
   the credential's value (searched with the shared token, never printed);
3. who read them and when (the nodes' receipts: reader, time, how it reached them; who took it as
   a task; replies) and what this machine's audit log says about them;
4. that the incident is written to the audit log.

`aivoicebus incident ... --apply` does it, after you type `incident` at the prompt: the credential
**first** (it stops at the first script that fails, and then retracts nothing: a retraction
notice about a credential that still works only points at it), then each message is retracted as
a leak (as its sender, with the shared token, when another machine sent it; the nodes tell every
session that read it at its next inbox read), then the audit entry (`aivoicebus audit`:
`incident-start` and `incident`, with what was done and what was not). A saved secret's copy on
this machine is removed. `--apply` refuses in an agent's session (`AIVOICEBUS_AGENT`, or an agent
tool above the process) and without a terminal, like `approve`; the plan itself is safe for anyone
to read. No secret is printed: everything it shows, the deploy scripts' output included, passes
through a mask of every credential this machine holds.

What it leaves to you, and says so: the shared token keeps working until every machine has moved
to the new one and you run `deploy/rotate-token.sh retire --apply` (or pass `--retire` to retire it
at once, cutting off machines not moved yet; `deploy/rotate-token.sh --check` names them); a revoked machine cannot reach the mesh until it gets
a new token (`deploy/machine-token.sh add HOST --apply`, installed by hand or with a client bundle,
since `token hand` needs that machine to read its mail). The text stays in node logs, exports and
the dashboard archive (see "Retracting a message" below): treat it as exposed. The scripts are
found in `./deploy`, `$AIVOICEBUS_DEPLOY_DIR` or `--deploy DIR`.

## Malformed input and floods

A node has to stay up whatever reaches it. Opening a connection needs no token, so the HTTP
server bounds what one costs: it drops a connection that has not sent its request headers in
10 s (30 s for the whole request, 30 s to write the answer, 2 minutes idle), serves at most 512
connections at once (the rest wait in the kernel's queue), and the unit sets `LimitNOFILE=4096`.
Release and client-bundle downloads get 10 minutes to finish writing. The limits are constants
in `internal/server/limits.go`; there is no per-address limit, because one machine runs many
sessions.

Records from a peer are held to a replication check (`Record.ValidateReplicated`) that
`/v1/send`'s rules are not: it is frozen at what the first release allowed (sizes, header
shape without control characters, id formats, priority and kind in range) and must never be
tightened, or real history would be refused. A record that fails it is not dropped, because
every later record of that origin would then look like a gap and stall: the node stores a
placeholder at its number instead (an ack of id `quarantined` by `aivoicebus/quarantine`, which
every consumer of the feed can read) and logs one line with the origin and number, never the
content. A record that arrives from a peer is written to the log as the placeholder; one already in the
log stays as it is on disk and becomes the placeholder when the node reads it back, so what the node
serves to its peers is the placeholder either way. `/v1/health` reports how many a node holds as `quarantined`, and `/metrics` as
`aivoicebus_quarantined_records`. It is not a peer link `"ok":false`. The placeholder replicates and
stays, so the count never goes back down: a rollout compares it with what each node held before
(`deploy/rollout.sh` per node, `deploy/watch.sh` for the nodes on the new release) and stops on an
increase, never on the absolute count, so a rollout or a token change after one is understood still
works (`verify_node` ignores it). A node moving from a release before the check to one with it can
show an increase with no new record; `AIVOICEBUS_ALLOW_QUARANTINED=1` ignores the check for that run.
A non-zero count means some peer holding
the token offered a record no client writes, or the node's own log holds one from before the
check existed: look at the log line, and at what that peer is.
A record whose *origin* is unusable (empty, over 200 characters, a control character) cannot
be kept even as a placeholder, since origins are the keys of the version vector that every sync
request carries; it is counted in `aivoicebus_refused_records_total` and dropped. A node on a
release before this check still holds the original of a quarantined record: the two kinds of
node disagree about that one number, and only that one.

Sync pages stop at 500 records or about 1 MiB, whichever comes first (always at least one
record), and every consumer of the feed asks again until the answer is empty.

## Retracting a message (undo send)

A sender can take a message back: `aivoicebus retract [ID]` (with no id, this session's own last
message and no other session's: `--last` is for a person at a terminal who means the machine's
latest; `--leak` when it held a credential, `--dry-run` to see what would happen). The node side
is `POST /v1/retract` (`{"id","by","leak","dry_run"}`), and `aivoicebus ack retract:<id>` from a
client that predates `retract` writes the same record. **It hides, it does not erase.** Be exact about that
with the people who ask:

- Hidden: the unread inbox (so the session-start hook, the per-prompt hook, `aivoicebus_inbox` and
  `inbox` all stop delivering it at once), `--all` and thread views (a tombstone shows who
  withdrew it and when, and the text is gone), and the dashboard (tombstone, search, flags).
- **Not erased**: the text stays in every node's `log.jsonl`, in `/v1/sync` and so in the dashboard's
  archive and in every export and backup, and in any session's context that already showed it.
  There is no purge tool: a log is numbered per origin and every node and the dashboard follow those
  numbers, so cutting a record out of one node's file would make it a gap that stalls replication
  or, if filled with a placeholder, differ from every other node. Treat a retracted secret as
  exposed.
- Told: sessions that had read the message (an ack of it on the node they read from, not the
  sender, not a retractor on the sender's machine, and for an fyi only a leak that the session saw in
  full rather than only counted at session start) get one notice, made up at that session's next
  unread inbox read and never stored: its next session start or `inbox`, and, only for a leak (the
  notice is then urgent, which is all the per-prompt hook shows, at most every 15 seconds), its
  next prompt in a running Claude Code session; a session that never reads its inbox again is not
  told. "Once" is per node: a reader that acks the notice on one node and reads from another
  before the ack replicates sees it twice. Up to 10 notices ride one read and come first, so a
  burst of leak notices can push ordinary urgent messages down the hook's short list until they are
  acked. The notice is from `aivoicebus/retraction`, to that session, `re` the message, ask priority
  (urgent for a leak), first in the inbox, with a fixed text that names the message and the
  retractor's machine and never quotes it. Their ordinary ack of the notice id (the hook's,
  `inbox --ack`, `aivoicebus_ack`) stops it; it is offered for 72 hours after the retraction (its time,
  or when the node took it in if that is earlier, so a claim dated ahead does not linger), at most
  10 per read, leaks first, then the earliest. Only nodes that make notices make them; during
  a rollout a read answered by a node without them has none, and the CLI's read count says how many to
  tell yourself.
  The notice id is a hash of the message id (the first of `NoticeID(id, 0)`, `NoticeID(id, 1)`, ...
  that no held message has), so every node makes the same one and an ack replicates as usual;
  a node without notices and a dashboard that does not know them take that ack as an ack of an id they do not hold. A node refuses a message
  sent with one of those ids, or as a reply to a notice (400), once the message they belong to is
  retracted; a copy that arrives by replication is stored, and the notice then takes the next probe
  id. The 72-hour clock of a claim dated ahead starts when the node first took it in, remembered in
  `retract-arrived.json` in the data directory so a restart does not start it again. `/v1/send` refuses a
  `from` on the machine `aivoicebus` (in any case, ignoring invisible characters) and any `from` with
  an invisible character (format or bidi control, separator, non-plain space). A message from that
  machine that arrives by replication or import (a node that predates the rule accepts one) is stored as it is
  but never delivered; `--all` and threads list it with the body `[forged system message removed]`.

How it works, so a surprising result can be explained: a retraction is an ordinary ack record whose
id is `retract:<message id>` (or `retract-leak:<message id>`). Every node derives, when it reads,
whether a message is retracted from the records it holds (the rule and its test vectors are in
[observer.md](observer.md#retractions-undo-send)): the same machine as the sender, the earliest claim
wins, a leak flag from any authorised claim, via and chars ignored, any arrival order. So it
converges like everything else, works when the retraction reaches a node before the message, and a
node that lost its disk gets it back from its peers.

**Who may retract.** The same machine as the message's `From`, not the exact session: a restarted
session (a new suffix), another tool, or the operator at a shell on that machine. A machine
token can only act as its own machine. The shared token can declare any `by`, so, exactly as it
could already forge a send, it can retract anyone's message by declaring a `by` on the sender's
machine, and the real sender cannot undo that (retraction is final: there is no un-retract;
recovery is sending the message again). This is not a security boundary. What there is: the
tombstone names the retractor's machine (only the machine, cleaned to a short, safe alphabet: see
[observer.md](observer.md#retractions-undo-send)), the `retracted` object and the feed carry the full
identity, and a claim by an identity on another machine is counted as ineffective (below) and
changes nothing. A node never records a retraction for a message it does not hold (422): it cannot
tell a message still replicating from a typo, so the client asks another node, and a message sent
with the usual quorum is on at least two. The same is why a dry run, or the plain ack a client
writes to a node that predates retraction, cannot tell a mistyped id from a real one: the CLI reads the
thread first.

Message ids are the ten lowercase hex digits `aivoicebus send` makes. `/v1/send` also accepts a
hand-chosen id in uppercase hex, but such a message cannot be retracted: no consumer would
recognise a claim on it, so the route answers 400 instead of writing one.

**Allowance.** 60 retractions an hour, separate from the 20 sends an hour, kept per exact claimed
`by` (not per machine or per token), per node, in memory: a restart forgets it, and the nodes do not
share it. It is counted only when a retraction is actually written: refusals, repeats (`already`)
and dry runs cost nothing. It is a courtesy against a loop, not a limit on someone who holds the
shared token, who can claim any number of identities.

**Rolling out.** Roll the nodes before the clients (a new client against an old node falls back
to the plain ack), and deploy the dashboard whenever it is ready: it needs no node release, and an old
dashboard does not break on the records. A node that predates retraction replicates the retraction record untouched (it is a valid ack: no
quarantine, no stall) but keeps delivering the message and serving its text until it is upgraded;
a session that reads from it in the meantime still gets the text. The client says which nodes still
deliver it (`node X still delivers it until upgraded`) and how many other sessions had read it.

| state of the mesh | what a retraction does |
|---|---|
| every node applies retractions | hidden everywhere as soon as the record has replicated (about the sync interval) |
| some nodes predate retraction | hidden on the nodes that apply it at once; the others replicate the record and keep serving the text until upgraded; upgrading one starts hiding at once, with no migration |
| no node knows `/v1/retract` | the client writes the plain ack (`retract:<id>`, via empty) to two nodes and says it only recorded it (and, when no node it can see applies retractions, that nothing hides the message until one does); a node that applies retractions hides it once it holds the message |
| a node rolled back to a release that predates retraction | the text is served again by that node; nothing is corrupted or lost; upgrading it again hides it again |
| an old dashboard | the retraction shows as an unrelated ack: no phantom read, and the text stays visible (credential-masked) until the dashboard is upgraded |

An `ack` of a retraction id on a node that applies retractions goes through the same checks as `/v1/retract` (token,
machine, allowance, dedupe), and an id that is not a message id after the prefix is a 400.

**Watching it.** `/v1/health` reports `retracted` (messages this node hides; absent when none) and
`/metrics` has `aivoicebus_retracted_messages` and `aivoicebus_retractions_ineffective`. The second
counts retraction claims held for a message by an identity that is **not** on its sender's machine,
which change nothing: it should be 0. A non-zero value means someone tried to take back another
machine's message by declaring an identity, or a client with a wrong identity; the records are in
the log (`origin` says which node wrote each), so look there.

**A leaked secret.** `aivoicebus incident` does all of this in order (see "If something leaked"
above); by hand: (1) Rotate the credential now: retraction cannot unsend it. (2)
`aivoicebus retract --leak <id>` from the sender's machine (or the operator's shell on it): the
tombstone says it may have held a credential, and the CLI prints how many other sessions had read it. (3) Check `aivoicebus thread <id>` on two nodes shows the tombstone; a node that
still shows the text is on a release that predates retraction (`aivoicebus mesh`). (4) Remember the text is
still in the logs, exports and the dashboard's archive: that is why step 1 is the real fix, and why the
node refuses to store a message that looks like a credential in the first place
(`aivoicebus_secrets_refused_total`, which also counts a pipe link with its key and an approver signature
pasted into a message). `aivoicebus_approvals_refused_total` counts replies to an approval request that
could not be the person's signed decision (an agent writing "approve"): a rising number means an agent
believes it can approve; the request is in the person's queue (`aivoicebus answer`), not the agent's.

**A session that is gone.** Retraction goes by machine, so a session that died (or a new one with
another suffix) is not a problem: from a shell on the sender's machine, `aivoicebus retract <id>`
works, and `AIVOICEBUS_ME=<machine>/<anything>` sets the identity it writes as, if you need one.
From another machine, only the shared token can retract, by declaring an identity on the
sender's machine (`aivoicebus retract --as <machine>/<tool> <id>`); a machine token cannot.

## Amending a message

`aivoicebus amend ID "text"` replaces a message the machine sent: a plain message to the original's audience,
thread and priority, then a claim, an ack with id `amend:<new id>`, written to the nodes through `/v1/ack` (the node
checks it as `/v1/retract` checks a retraction: 422 for a message it does not hold, so the client asks another node and
repeats for about a second and a half; 403 for an identity on another machine; 400 for a message that is no amendment
of the one it answers). The rule and the response fields are in [observer.md](observer.md#amendments); the
command is in [reference.md](reference.md#correcting-a-message-amend).

- **Rolling out.** Roll the nodes before the clients. A node that predates amendments stores the claim as a read receipt
  for an id it does not hold, replicates it untouched (no quarantine, no stall) and delivers the original and the new text
  as two ordinary messages, so nothing is lost and a reader sees what a correction always showed. The client warns when the
  node that answers lacks the `amend` feature. Upgrading a node starts applying every claim already in its log, with no
  migration.
- **"The correction did not replace it."** Look at the node the reader asks: `aivoicebus mesh` lists the features and
  releases; a reader served by an older node gets both. Then `aivoicebus thread <id>`: the original shows
  `[amended: replaced by <id> ...]` when a node applied the claim. A claim by an identity on another machine, for a message
  with another target or in another thread, or on a task or question, changes nothing by design.
- **The link failed.** The command says the text went out as an ordinary reply and prints `aivoicebus ack amend:<id>`
  (the claim again, harmless to repeat) and `aivoicebus retract <id>`.
- **Undoing it.** `retract` the new message: the original is delivered again to sessions that have not read either, and
  those that read the amendment were told it replaced something and then that it was withdrawn.
- **Cost.** Nothing while no amendment is in the log (one length check per read); each claim is one ack record.
## The test lane (tests that reach nobody else)

Agents test the mesh with real messages; the lane is where those messages go instead (the feature `testlane` in `/v1/health`;
[reference.md](reference.md#the-test-lane-tests-that-reach-nobody-else)). What an operator needs to know:

- **Nothing to configure on a node.** A node of this release hides test messages from every read that does not ask for them, leaves them
  out of `/v1/health`, `/metrics` and the viewer, gives their sends rate-limit buckets of their own, and purges them. Rolling out: send
  `--test` only works once **every** node a client reaches advertises `testlane` (`aivoicebus mesh` lists the features), so update all
  nodes first; until then `send --test` is refused with the name of the node that predates it, and `selftest` falls back to its old probe.
- **The purge is in memory.** About ten minutes after a test message was sent, the next read or send replaces its record by a stub in
  memory, and so in what `/v1/sync` and `export` serve. `log.jsonl` still holds the body as it was written: a restart replays it and the
  sweep at start replaces it again, so a node that was down while a test message expired purges at its next start. Taking the bodies off the disk
  is a separate change.
- **A test message is not in an export's words.** `aivoicebus export` writes the stub, so a restore keeps the numbering and none of the test
  traffic. A backup that copies the data directory holds the bodies.
- **Statistics do not move for tests.** If a message count looks short against the number of sends, the lane is the difference: `aivoicebus selftest --worker HOST`
  and agents' `send --test` leave stubs in the log, not statistics.
- **Opting a worker in** is `aivoicebus worker install --agent ... --test-lane` (or `worker --test-lane`): it reads the lane and answers a test task by a built-in
  step, never with its command, so a test costs no tokens and runs nothing. A worker that did not opt in is not touched by any test message.

## Releasing and rolling out

    scripts/release.sh build && scripts/release.sh sign      # see releasing.md
    deploy/rollout.sh dist/release                           # the plan
    deploy/rollout.sh dist/release --apply                   # canary, soak, the rest

Run them from a checkout of the release commit (`git worktree add --detach`), not from `main`.
With `--apply`, `rollout.sh` and `publish-release.sh` hold the release lock
(`deploy/release-lock.sh status` shows who has it; a rollout renews it every minute and a lock not
renewed for two hours is stale), so two sessions never roll out at once. The lock lives on the first
node of `nodes.conf`: with it unreachable they refuse, and `--force-lock` goes on without a lock.
`install.sh`, `rotate-token.sh` and `machine-token.sh` do not take it.
Before it touches anything, `rollout.sh` verifies the release with `aivoicebus release verify`
(the signature against the key compiled into your aivoicebus, every file against its signed
hash) and checks every node answers ssh. `--unsigned` skips the verification, for a build you
made yourself. The canary is the first node (or `--canary NAME`); a node already on the release
is not installed again, so a second run changes nothing, except that the canary is soaked again
(an interrupted rollout must not skip it). For each node: install, check it reports the released
version with every peer link up and no new quarantined record; for the canary also soak (30 s by
default) and check the service did not restart since the install, which catches a crash loop
that `/healthz` shows as healthy part of the time. `deploy/watch.sh` runs beside it with the
observer token and stops it when a node on the new release holds more quarantined records than
before, a node stops accepting the observer token, or the watcher itself dies. A canary already on
the release whose soak fails is put back on `aivoicebus.prev`, or reported as still on the bad release. A node that fails is put back to `aivoicebus.prev` (and the
previous unit file), but only if this rollout replaced its binary, so a node that was fine is
never downgraded; the rollout stops, so nodes after it are never touched. Rolling nodes before clients matters for
new wire features: a node that predates them stores a selector with alternatives (`a|b`) or an
exclusion (`!=`) and delivers it to nobody, and `aivoicebus send` warns when it sees one.

## Adding a node

    deploy/add-node.sh node-d node-d_bastion 10.0.0.9              # the plan
    AIVOICEBUS_BIN=dist/release/aivoicebus-linux-amd64 deploy/add-node.sh node-d node-d_bastion 10.0.0.9 --apply

It lists the node in `deploy/nodes.conf`, installs it, refreshes the others' peer lists one
at a time, and waits until the newcomer holds everything: it compares the records each node holds
(the sum of the version vector, `log` in `/v1/health`), never messages plus acks, because a release
that predates retraction counts a retraction among its acks and one that applies it does not, so a new node in a mixed mesh
would otherwise never look caught up. It does not touch your
network rules or any dashboard; it prints those steps for you: the firewall or VPN rule that lets
the other nodes and your clients reach the new node, the
dashboard's `AIVOICEBUS_URL`, and a re-signed node list so machines learn the node by themselves
(`aivoicebus release nodes --dir dist/nodes`, `deploy/publish-nodes.sh dist/nodes --apply`).

## Tokens

Three kinds, all in each node's token file (`/etc/aivoicebus/token`, one per line). Every
script that changes tokens first checks that *every* node answers ssh and refuses to start
otherwise (a node left on the old token would be cut off, and a revoke that skipped a node
would claim a token is gone while it still works), and checks the token it is about to push
really is the shared one: a machine token, or a stale file on another admin's machine, is
refused. Each node must come back with its peer links up before the next is touched.

- **Shared**: what every machine uses today. Rotate it without a flag day:

      deploy/rotate-token.sh status
      deploy/rotate-token.sh add --apply       # nodes accept the old and a new token
      # ... move every machine to the new token (~/.config/aivoicebus/token.next) ...
      deploy/rotate-token.sh promote --apply   # nodes speak the new token between themselves
      deploy/rotate-token.sh retire --apply    # the old token stops working; this machine switches

  A machine still on the old token stops working at `retire`; `status` shows who accepts what.
  That machine does not fail loudly: see "A machine that has gone deaf" below. Before `add`, ask
  whether a rotation would cut anyone off (read-only; it changes nothing, here or on a node):

      deploy/rotate-token.sh --check           # exit 0: nobody would be cut off; exit 1: the marked machines would

  It runs `aivoicebus token report` against the nodes in deploy/nodes.conf: every machine heard from
  in the last 7 days, the kind of token the nodes last saw it use (`/v1/token/seen` on every node, the
  newest answer wins), and the machine tokens in `machine-tokens/`. It refuses while any machine other
  than the nodes and this one is on the shared token, or is not proven either way (no node has seen
  it since the node started), or while a node does not answer. Move such a machine to its own token
  first (`machine-token.sh add HOST --apply`, then `aivoicebus token hand HOST`).
- **Machine**: bound to one machine, so nobody can speak as another machine with it:

      deploy/machine-token.sh add laptop-a --apply    # writes ~/.config/aivoicebus/machine-tokens/<host>
      deploy/machine-token.sh revoke laptop-a --apply

  Hand it over with `aivoicebus token hand laptop-a`: the person on laptop-a runs `aivoicebus token
  install ID` in their own terminal (the task goes to `laptop-a/human`; agents there cannot take it) and approves, and the token goes as a one-time encrypted file, checked on
  both ends ([reference.md](reference.md), "Machine tokens"). If nobody can approve on laptop-a (no
  terminal, approver key or notify URL), install refuses at once and says so; if `hand` runs out of
  time, it says whether nobody took the task, nobody approved it, or the install never answered.
  Never put it on the machine you deploy
  from. It cannot replicate, so keep one shared token for the nodes themselves. What it limits:
  sending, acknowledging and reading mail *as that machine only*, and the bulk replication
  feed (`/v1/sync`, so `aivoicebus export` needs the shared or observer token). What it does not
  hide: like any agent token it can read a thread by id, the roster and health. If a machine is
  lost, `revoke` closes everything it could do as that machine; the board's contents were
  readable to it, as to any agent. `/v1/token` tells a script what kind of token it holds.
  `/v1/token/seen?host=laptop-a` says which kind of token (machine or shared, and its name in the
  token file) a node last saw act as that machine, and which kind sent its last message: the proof
  that a machine really switched to its own token. Each node knows only what reached it since it
  started, so ask them all (`aivoicebus token hand` does).
- **Observer**: read-only, for the dashboard and exporters ([observer.md](observer.md)).

## Backup and restore

    aivoicebus export --out board.jsonl                        # every record the mesh holds (mode 600)
    aivoicebus import --data /var/lib/aivoicebus --node node-a board.jsonl    # on a stopped node

Export asks every node and takes the union, so a node that is behind cannot cut it short, and
the file is written aside and renamed only when complete: a failed export never replaces an
earlier good backup. Records are add-only and numbered per origin, so import skips what is
already there and a restored node then catches up with its peers; run as root it gives the
files to the owner of the data directory (the service runs as a dynamic user that must be
able to read them). Start the restored node with a peer reachable: a node whose log is older
than what its peers hold must hear from one before it takes writes. A node that loses its disk recovers by itself
from its peers (it takes no writes until one has answered). The dashboard keeps a second copy.

## The signed node list

Machines learn new nodes from a list the mesh serves and the release key signs. The list
carries its own expiry (`valid_until`, 90 days by default; `aivoicebus release nodes --valid 400d`
or `never`): a machine stops using an expired list, so a replayed old list cannot keep steering
the token to a decommissioned address. `aivoicebus mesh` warns 14 days before the published list
runs out. Renew it any time: `aivoicebus release nodes --dir dist/nodes`, then
`deploy/publish-nodes.sh dist/nodes --apply`.

## A machine that has gone deaf

The hooks are silent by design, so a machine whose token was retired (or whose nodes are all
unreachable) would otherwise just stop receiving. Each hook run notes what the inbox read found in
`~/.config/aivoicebus/hook-status.json` (mode 0600: last run, `ok` / `unreachable` / `rejected`,
failed runs in a row, last success, the node's host:port; no token, no message text), and when the
token is refused (401/403), or three runs in a row over at least half an hour got no answer (the
per-prompt hook never counts: its 700 ms look cannot tell a slow node from a dead one), it prints one
fixed line, at most once a day, asking the agent to tell the user. `aivoicebus doctor` then says which it is: it asks a
node `/v1/token` (or the roster, on a node too old to have it) and **fails (exit 1) on a rejected
token**, warns when no node answers, and shows the hook's own record as `hook runs`. After
`rotate-token.sh retire`, expect that line and that FAIL from every machine that was not moved: the
fix is to put the current token in `~/.config/aivoicebus/token`. `deploy/install-client.sh` runs
`doctor` last and still prints its reminder to delete the bundle (it holds the token) when
`doctor` fails, then exits with `doctor`'s status.

## Quiet hours and the urgent check

    aivoicebus quiet 22:00-07:00      # session start counts asks instead of showing them; urgent still arrives
    aivoicebus quiet off

Urgent messages are also checked on each prompt of a running Claude Code session (at most
once every 15 s, 700 ms deadline, silent unless something urgent arrived). `aivoicebus setup`
installs that second hook; `aivoicebus doctor` reports a half-installed set.

## The lab

`scripts/lab.sh` runs the real deploy scripts against four sandboxed nodes running the real
binary (fake ssh, scp and systemctl map each alias to a directory) and then drives the real
CLI, hook and MCP server at them: replication, a frozen node, a lost disk, backup and
restore, token rotation, machine tokens, rollouts of a good, a broken, a crash-looping and a
mislabelled release, adding a node, and the refusals (a node down, the wrong token, a revoke
that fails half way and is finished by running it again), and the release tooling: signing from
the binaries' stamp at their commit, the release lock (two rollouts at once, a killed one, a
stale one), a resumed rollout's canary soak, and the watcher stopping a rollout on a rejected
observer token and on a quarantined record. The nodes run under the unit's real
`ExecStart`. What the lab does not test: `DynamicUser` ownership and sandboxing, and a real
systemd; it proves the scripts, not the unit. (`scripts/lint.sh` guards the unit statically:
`systemd-analyze verify` on a copy, and every hardening directive pinned to its value. That
catches a dropped or mistyped line, not a sandbox that fails on a real host.) `scripts/check.sh`
runs it on Linux; on a Mac it is skipped (run it on the build host).

## Signed messages only

Every client of 0.7.0 or later signs every message with its machine key. Once every machine runs
one, start the nodes with `serve --require-signed` (or `AIVOICEBUS_REQUIRE_SIGNED=1` in the node's
environment): a node then refuses, with 403, a message that carries no signature or whose
signature does not verify. Join and selftest probes are exempt. Before switching it on, check that
nobody would be refused:

    aivoicebus mesh --signed-check     # READY: no unsigned message in the last 7 days on any node

NOT READY names each sender that sent a message without a valid signature in that window (a client
older than 0.7.0, or one that could not sign); update those machines and wait until the check says
READY. Then, on the machine you deploy from, the switch is one line (each node in turn gets
`AIVOICEBUS_REQUIRE_SIGNED=1` in its node.env and is restarted, binary and token untouched):

    AIVOICEBUS_NODE_REQUIRE_SIGNED=1 deploy/install.sh --apply --peers-only

Every later install and rollout keeps the setting as each node has it; `AIVOICEBUS_NODE_REQUIRE_SIGNED=0`
with the same command switches it off. A node added later starts without it unless the same
variable is set for deploy/add-node.sh (which runs install.sh), or the line is run again after. `mesh --signed-check` then lists each node as "already refuses
unsigned ones", and `/metrics` counts what was refused (`aivoicebus_refused_total{reason="unsigned"}`).
