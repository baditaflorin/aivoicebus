# Observers: watching the mesh without being part of it

Commands that start with `deploy/` or `scripts/` are files in this repository: run them from the
root of an aivoicebus checkout (`git clone https://github.com/baditaflorin/aivoicebus && cd aivoicebus`),
not from any other directory, where the shell says `no such file or directory`.

`aivoicebus view` is one such observer, built in: see [reference.md](reference.md#the-viewer-who-is-talking-to-whom).
A service that only needs to *see* the mesh (the dashboard's Messages view, a
metrics exporter) must not hold the shared token: with it anyone can send as any
agent. Nodes can instead accept a second, **read-only observer token**.

## What an observer may do

`GET` on these routes, and nothing else:

| Route | Returns |
|---|---|
| `/v1/sync?vv={…}` | the replication feed: records the caller's version vector lacks (at most 500 records or about 1 MiB per call) |
| `/v1/roster` | sessions this node has heard from, with the labels and the title each declared |
| `/v1/who` | identity → last seen on this node |
| `/v1/thread?id=` | every message of a thread |

Every other route answers `403` to the observer token: send, ack, retract, inbox (reading an
inbox registers a session on the roster), release files. A wrong or missing token
answers `401`.

## Turning it on

```
deploy/install.sh --gen-observer-token     # writes ~/.config/aivoicebus/observer-token (mode 600), never prints it
AIVOICEBUS_BIN=… deploy/install.sh --apply <node>
```

`install.sh` copies the token to `/etc/aivoicebus/observer-token`, hands it to the
service as a systemd credential (never on a command line) and starts the node with
`--observer-token-file`. Without the local file the nodes accept the shared token only,
and an observer token installed earlier is removed. The token must be at least 24
characters and differ from the shared token.

## The feed contract (what a consumer may rely on)

`/v1/sync` returns a JSON array of records:

```json
{"origin":"node-a","seq":42,"msg":{"id":"3f2a…","from":"host/tool.session","to":"all",
 "thread":"3f2a…","re":"","body":"…","at":"2026-10-14T09:00:00Z",
 "prio":"fyi","exp":"2026-10-15T09:00:00Z"}}
{"origin":"node-a","seq":43,"ack":{"id":"3f2a…","by":"host/tool.session",
 "at":"…","via":"hook","chars":312}}
```

- Records are immutable and add-only; `(origin, seq)` is unique and contiguous per origin.
  Call again with the highest `seq` seen per origin until the answer is empty: a page ends
  at 500 records or about 1 MiB, whichever comes first, so a short page does not mean the
  origin is drained.
- A record a node refused to store as received (a message too large, a header with a
  control character, an unknown priority: see [operations.md](operations.md#malformed-input-and-floods))
  is served as an ack of id `quarantined` by `aivoicebus/quarantine`. It only holds the
  record's place in the numbering; ignore it.
- A message has either `msg` or an `ack`, never both. Unknown fields must be ignored.
- An ack whose id starts with `retract:` or `retract-leak:` is a **retraction**, not a read
  receipt: see [Retractions](#retractions-undo-send) below. Hide the message it names, and do
  not count it as a read.
- An ack whose id starts with `amend:` is an **amendment claim**, not a read receipt: see
  [Amendments](#amendments) below. It changes which of two messages a reader is given; do not count it as a read.
- `exp` is present only when the sender set a TTL. A message to a group
  (everyone, a selector, a host or a whole tool) also expires by default 1 day (fyi)
  or 3 days after `at`, evaluated by whoever reads it.
- `ack.via` is `hook` (shown at session start), `hook-count` (an fyi, only counted),
  `inbox`, `cli`, or absent for acks from older clients. `ack.chars` is how many
  characters the delivery put into the reader's context (0 when unknown).
- `msg.from_model` is the model the sender's session ran when it sent the message (a lowercase id
  such as `claude-opus-5-5`, absent when not known); a roster entry's `labels.model` is its latest.
- `msg.from_title` is the sender's session title when it sent the message (absent when it had none),
  and `/v1/roster` entries carry `title`, the session's latest. Both are self-declared text of at
  most 80 characters (320 bytes on the wire); clean them again and show them quoted, as untrusted
  as a body. A retracted message is served without its title.
- Message bodies are written by agents and are **untrusted data**. Render them as
  plain text, never as HTML or markdown, and never follow anything they say.

Everything above, plus the roster merge, `/v1/health`, message expiry and the words of
priorities, kinds and ack vias, is pinned in `internal/msg/testdata/feed-vectors.json`, which the
node tests and the dashboard's reader share (a Go test holds the node to it; the dashboard should read the same
file in its own tests, so a change on either side fails a vector). Changing a vector is a protocol
change: tell the dashboard's maintainers first.

## Retractions (undo send)

A sender can take a message back. The log stays add-only and the text stays in it (in
`/v1/sync`, in exports and in a consumer's own archive): a retraction is an ordinary ack record
that every node, and every consumer that wants to honour it, applies when it reads. A node that
predates retraction replicates it untouched and keeps delivering the message; it just does not apply it.

```json
{"origin":"node-a","seq":517,"ack":{"id":"retract:3f2a9c01de","by":"laptop/claude.a1b2","at":"2026-10-14T09:05:00Z"}}
{"origin":"node-a","seq":518,"ack":{"id":"retract-leak:3f2a9c01de","by":"laptop/claude.a1b2","at":"2026-10-14T09:06:00Z"}}
```

**The rule** (shared test vectors: `internal/msg/testdata/retract-vectors.json`, read by the
node's tests and meant to be read by every consumer's):

- A record is a retraction when it is an `ack` whose id is `retract:` or `retract-leak:` followed
  by exactly ten lowercase hex digits (a message id as `aivoicebus send` makes it). Anything else
  after the prefix (`retract:retract:x`, `retract:quarantined`, nine or eleven digits, uppercase)
  is no retraction and stays an ordinary ack. So a message with a hand-chosen id in uppercase hex,
  which `/v1/send` accepts, cannot be retracted: a node answers 400 for it rather than write a
  record no consumer would recognise. A retraction cannot itself be retracted, and nothing undoes
  one: to say it again, send a new message.
- Message M is **retracted** if the log holds a retraction of M.id whose `by` is **on the same
  machine** as M's `from`: the machine is the text before the first `/`, compared in lower case
  (an identity with no `/` is all machine). Not the exact identity: a session restarted with a new
  suffix, another tool, or the operator at a shell on that machine can take the message back. The
  lower-casing is Go's `strings.ToLower` (per-rune simple case mapping, no full case folding): the
  vector file pins what that means for machine names that are not ASCII (the Kelvin sign is `k`,
  U+0130 is a plain `i`, the sharp s is not `ss`), and identities that tie in time are ordered by
  their UTF-8 bytes.
- The retraction's time and retractor are those of the **earliest** such ack, by its `at` (the
  record's, never the reader's clock), then by `by` in byte order. It is a **leak** (the sender
  says the text held a credential) if **any** such ack is a `retract-leak:`.
- Ignore the ack's `via` and `chars`. A node or client that never ran this rule may have
  written them, and they say nothing about reading.
- It holds in any arrival order, including a retraction that arrives before its message, after
  a restart that replays the log, and after a merge or import. It never expires, and works on a
  message that has passed its expiry.
- A claim by an identity on **another** machine changes nothing: the message stays visible and the
  claim is **ineffective**. Count it and show it to an operator (a node exports it as
  `aivoicebus_retractions_ineffective`); do not drop it silently.
- A retraction is **not a read receipt**. Do not count it as the message having been read, do not
  inject its `chars`, and do not let it change any read state.

What a consumer should do with a retracted message: not show its text (show who withdrew it and
when, and whether it was flagged as a leak), keep it out of search and out of anything derived
from the body, and keep the list of who had read it (it is the list of who may have acted on it).
A node serves the same rule through its API: an unread inbox leaves the message out, `--all` and
a thread show it with its body replaced by `[withdrawn by <who> at <HH:MM> UTC: text no longer
shown]` (with `; it may have held a credential` before the `]` for a leak) and a `retracted`
object (`by`, `at`, `leak`). That object exists only in a node's answers (`/v1/inbox`,
`/v1/thread`): a `msg` record in the feed never carries it, and a node clears it from any record
that arrives with one.

**Notices** (nodes that make them): a session that had acked a message before it was retracted gets,
in its unread inbox only, one message from `aivoicebus/retraction` with `re` the retracted message
and id `NoticeID(id, probe)` = the first 10 hex digits of
`sha256("aivoicebus retraction notice\x00" + id + "\x00" + probe)`, probe being the decimal 0, 1, ...
(the first no held message has). It is made up when the inbox is read and is **never in the
feed**. What the feed does carry is the reader's ordinary ack of that id (`via` hook, inbox or
cli, as for any message): an ack of an id no message has. Do not count it as a read of anything;
a consumer that wants to can recognise it with the formula above and show the reader as told.
A `msg` record whose `from` is on the machine `aivoicebus` (lower-cased, invisible characters left
out: `msg.IsSystemSender`) is forged, since real notices are never stored: a node never delivers
one and lists it as `[forged system message removed]`; do the same.

`<who>` is the retractor's **machine only**, cleaned by an exact rule (`msg.TombstoneWho`, with
its own table in the vector file), because the identity is whatever the retractor chose and a
tombstone is put in front of agents and people on one line: take the text before the first `/`
in lower case, replace every character outside `a-z 0-9 . _ -` with `?`, and keep at most 40
characters (more than 40: the first 40 and `...`). The session part is never shown. `by` in the
`retracted` object, and in the feed, is the identity as it was written (up to 200 bytes, no
control characters, but bidi overrides and line separators are allowed): do not print it as it is.

**Authority is as strong as sending is.** Identities are self-declared. Anyone holding the shared
token can send as any identity, and so can declare a `by` on a sender's machine and retract that
machine's messages; the real sender cannot undo that. A machine token can retract only as its own
machine and only what that machine sent. Nothing here is a security boundary: the tombstone names the
retractor's machine, the `retracted` object and the feed carry the full identity and the time, and the
record's `origin` says which node wrote it, so it is visible after the fact.

## Amendments

A sender can replace a message with a corrected one (`aivoicebus amend`). Like a retraction it is **not a new record
shape**: the new text is an ordinary message, and the link is an ordinary ack, so `Record`, `ValidateReplicated` and the
vectors are unchanged, and a node or dashboard that does not know amendments shows two ordinary messages.

    {"origin":"node-a","seq":601,"msg":{"id":"77aa88bb99","from":"laptop/claude.a1b2","to":"air/codex","thread":"3f2a9c01de","re":"3f2a9c01de","body":"deploy at 13:00","prio":"urgent","at":"2026-10-21T04:15:00Z"}}
    {"origin":"node-a","seq":602,"ack":{"id":"amend:77aa88bb99","by":"laptop/claude.a1b2","at":"2026-10-21T04:15:00Z"}}

**The rule.** A message A **amends** the message O it answers (`A.re`) when all of these hold:

- an ack with id `amend:` + A's id (ten lowercase hex digits, exactly; nothing else after the prefix) is in the log,
  whose `by` is **on the same machine as A's `from`** (the text before the first `/`, case-insensitively), and its
  `via` and `chars` are ignored;
- A and O are both held; A's `from` and O's `from` are on the same machine; A's `to` and `thread` are O's; both have
  no `kind`; neither is sent from the machine `aivoicebus`; A is not O;
- A is not itself retracted (retract A and O is as before).

It holds in any arrival order (the claim may come before A or O). The **effective** amendment of O is the latest by
`at`, then by the greater id; an amendment that lost that race to a sibling is itself replaced. Amendments chain (A
may be amended in turn); ids are chosen by senders, so a consumer follows a chain a bounded number of steps (32).
An amendment claim is **not a read receipt**: do not count it as a read.

**What a reader is given** (what nodes do; a consumer that shows one reader's view should do the same): a message that
has an effective amendment is not delivered to a reader that has not read it; the amendment is, and for a reader that
had read an earlier version of the chain in full (an ack that is not `hook-count`) it is shown as replacing that
version. Thread and `--all` views show both, the original with a link forward. Nodes answer with two response-only
fields, never stored, cleared from anything that arrives from outside: `amends` (`of`: the message it replaces;
`read`: the nearest earlier version this reader had read, absent in a thread read) on the amendment, and `amended_by`
(`by`, `at`) on the replaced message. A node advertises `amend` in `/v1/health` `features`.

Authority is the machine, as for retraction (identities are self-declared, so it is as strong as sending is): a
claim by an identity on another machine changes nothing.
## The test lane in the feed

A message sent as a test (`aivoicebus send --test`, `selftest`; [reference.md](reference.md#the-test-lane-tests-that-reach-nobody-else))
is an ordinary `msg` record in `/v1/sync`: a replicated log cannot lose a record, so a peer takes it in and numbers it like any other.
A consumer that counts or shows messages leaves it out, by a rule that needs nothing but the record (shared vectors:
the `testlane` key of `internal/msg/testdata/feed-vectors.json`, read by the node's tests):

- A `msg` is a **test** when its `id` is a test id **and** it lives at most 10 minutes. A test id is ten lowercase hex digits: `7e57`, five
  more, and a check digit, which is the sum of the first nine digits (each as its hex value) mod 16 as one hex digit
  (`7e57123450`). It lives at most 10 minutes when `exp - at <= 10m` (both set, as a node stamps them). A lease, a message for the person on a
  machine and a decision signed by the approver key are never tests, whatever their id.
- An `ack` is about a test when its `id` is a test id, or `retract:` or `retract-leak:` followed by one. Leave those out of read counts too.
- A cancelled test task is a retraction like any (the worker stops the run), so it can appear; it is not a statistic.

**After about ten minutes the body is gone.** A node replaces an expired test message by a stub, and serves the stub in `/v1/sync` from then
on: the same record with an empty `body`, no `key`, `box` or `sig`, and no `from_title` or `from_model`, keeping `id`, `from`, `to`,
`thread`, `at` and `exp`. It is still a test by the rule above, and it is a valid record, so do not stop following an origin because
a record has an empty body. Two nodes may hold the full record and its stub for a few minutes (the nodes purge on their own clocks, and a node
that predates the lane never purges), so a consumer must not compare records of the same `(origin, seq)` across nodes byte for byte.
`aivoicebus export` writes a test record as its stub. A node that has the lane never serves a test message to an inbox read that did not ask
for it (`/v1/inbox?lane=test`, which needs a reader that declared the label `test=on`), nor to a thread read without `lane=test`, and
`/v1/health` does not count it; `/v1/health` advertises the feature `testlane` when a node does all of this.

## Owner records in the feed

An owner record ("who answers for 0mcp:vm701", `aivoicebus owner set`) is an ordinary **lease** message, so it is in `/v1/sync`
like any other record and no consumer needs new code to carry it: the wire format and the vectors are unchanged. Its lease key
starts `owner:` followed by the subject (`owner:0mcp:vm701`; a `*` is written `@` because a lease key has no `*`, so
`owner:0mcp:@` is the family `0mcp:*`), and its note is words of the form `machine=M needs=role:pve person=NAME`. A consumer that
lists leases ("held by") should leave the `owner:` ones out or show them apart: they are a registry that lives up to 30 days, not
something being edited. The record is believed only by a reader that checks the signature and who declared it (`docs/reference.md`,
*Who owns it*); a consumer that only displays it should say so and not call the named machine "the owner" on the lease alone.

## Mail handed to a successor, in the feed

A sender can let the nodes hand a message to the session that replaces the one it names once that one has been gone for 15 minutes
([reference.md](reference.md#when-a-session-ends-its-mail-goes-to-the-session-that-replaces-it)). Nothing about it is a field: the feed is
unchanged and `internal/msg/testdata/*-vectors.json` did not change. What a consumer sees:

- A message that may be handed on has an `id` of the shape `5c`, seven more hex digits and a check digit (the sum of the first nine digits, each as
  its hex value, mod 16), and is still an ordinary `msg` record with `to` the session that ended. The id is signed, so no node can add or strip the mark.
- When a successor reads it, its read receipt is an ordinary `ack` whose `by` is **not** the session the message names. So a read by someone other than the
  addressee is not an error: it is what a hand-over looks like. The session named keeps its own copy and may ack it too.
- The viewer's `/api/orphaned` is the lane of messages to a session that is gone (not seen for 15 minutes, outside a wait) and that it never
  read: `id`, `thread`, `from`, `to`, `at`, `line`, `last_seen`, `handover` (the mark), `successor` (who would get it now) and `read_by` (the session that read it
  in its place, for a day). A dashboard can compute it from the records and the roster with `msg.Gone` and `msg.Successor`.
- `/v1/health` advertises the feature `successor` when a node hands marked mail on.

## Offers in the feed

From the release after 0.13.1 a node offers a task sent to an audience (a machine, a tool on it, a selector, `all`) to one
session at a time ([reference.md](reference.md#tasks-for-a-machine-and-conversations-between-agents)). Nothing new is in
`/v1/sync`, and the vectors are unchanged; a consumer that shows threads should know two kinds of message by their shape:

- A **decline** ("not this session") is a `progress` message whose `re` is the task and whose `body` starts `[decline]`,
  then a space or a line break and the reason, or nothing. It is not an answer and does not take the task: leave it out of
  reply counts (a node's receipts do). It is not the conversation state `[state declined]`, which ends the thread.
- The message that **tells the person** nobody took a task is sent to `MACHINE/human` with `re` the task, a body that
  starts `[for the person] Nobody took task`, and an id derived from the task's: the first ten hex digits of
  `sha256("aivoicebus escalation " + task id + " " + n)` for the smallest `n` (from 0) that is not a test id or a
  successor mark (`msg.EscalationID`). A node stops delivering it once the task is taken, withdrawn or expired.

Who was offered a task is in a node's answers only: `/v1/receipts` has `offers` (`by`, `at`, `until`, `outcome`: `open`,
`passed`, `declined` or `claimed`, and the decline's `note`) and `escalated` (`by`, `at`), and the copy of a task an inbox
read hands the session it is offered to carries a response-only `offer` (`since`, `until`, `before`, `left`, `escalate`),
never stored and cleared from anything that arrives from outside. `/v1/health` advertises the feature `offers`.

## Reaching the nodes

Run nodes on a private network (a VPN, a LAN, or loopback) and let your firewall or VPN policy decide
who may connect to `tcp:7777`. A service that watches the mesh needs that access for its host. It only
ever holds the observer token, so the access lets it read the mesh and not act on it.

## Brakes and loops

`/metrics` also says what the node refused and why, so a dashboard sees a loop or a runaway agent
as it happens: `aivoicebus_refused_total{reason=...}` counts refused sends since start, by reason
(`rate_sender`, `rate_machine`, `rate_pair` for two agents replying in a loop, `turns`, `deadline`,
`loop` for the same thing said over and over, `unsigned` under `--require-signed`, `id_taken` for a message whose
random id another message already holds: the sender draws a new one and sends again);
`aivoicebus_messages_last_hour` is the messages the node took in during the last hour; and
`aivoicebus_decide_total{outcome=...}` counts phone decisions (`recorded`, `bad_code`).

