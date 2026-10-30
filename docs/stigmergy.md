# Marks and messages (stigmergy)

Two ways for agents on many machines to coordinate. A **message** is pushed into a reader's context: it costs
tokens whether or not it still matters, and it lasts until it expires. A **mark** is left in the environment and read
only by whoever looks: it costs the writer one record, the reader nothing until it is relevant, and it ends when it
stops being true. The mesh started as messages; the cheap and durable parts of it are marks. The owner's rule is the
least tokens, so every rule below is bookkeeping a program does, and no model reads it: a settled item costs zero
tokens, an open one at most one short line.

## What exists

| mark | where it lives | written by | read by | how it ends | what a reader pays |
|---|---|---|---|---|---|
| claim | a `claim` message in the task's thread | the session that takes a task | the other sessions, as "taken" | `end`, a `done` state, expiry | nothing: the task is no longer offered |
| lease | a `lease` record (`lease`, `owner:SUBJECT`) | a session or machine that answers for a thing | `owner:` routing, `lease list` | its own expiry, `owner rm` | a lookup when routing |
| fact | the `has` and `ssh` labels (`who --has`) | each machine, about itself, every few minutes | `who --has`, `fleet`, and `has:` conditions | the next probe says otherwise | nothing until asked |
| presence | the roster the nodes keep from reads | every inbox read | `who`, `ping`, offers | not heard for 30 minutes | a lookup |
| shared notes | a thread's shared space (`share notes`) | the sessions of a thread | the same | the thread's expiry | what they open |
| settle | an ack `settle:ID` by `<machine>/human` | the person (`done`) or a hook (a met `--done-when`) | the nodes, when delivering | never (it is a record) | **0**: the message is not delivered |
| key, supersedes | the last lines of a message body | the sender | the nodes and the hooks | the later message | **0** for the replaced one |
| done-when | the last line of a message body | the sender | the receiving machine's hooks and `status` | the condition becomes true | **0** once true |
| message | an inbox entry (`send`, `task`, `ask`) | anyone | every session it reaches | read, expiry, retraction | its text, at every session start until read |

## What a want adds

A **want** is an ordinary task with three more words: a key, a done-when and a TTL.

    aivoicebus task 0own "update aivoicebus to 0.15.0" \
        --key update-0own --done-when 'release>=0.15.0' --ttl 3d

It is open while a session or the person could still do it, and it **evaporates** by itself when a fact satisfies it:
`done-when` is checked on the receiving machine, locally, from a fixed list (`release>=`, `has:`, `file:~/`, `service:`),
never from message text. Whoever settles it first wins, and none of them needs a model: the person (`done`), the machine
(`--done-when`), the sender (a later message with the same `--key`, or `--supersedes`), an agent (`claim`, then `end`), or
time (`--ttl`). A settled want stays in `thread` for audit and is never shown again, to a session that already started or to
one that starts later. The want's state is the node's log plus the machine's own facts; nothing keeps a copy that can go stale.

Measured (tests in `internal/hook`): 20 items waiting for the person inject 537 characters at a session start, three lines of
at most 160 and one count; the same 20 settled inject 0. Before 0.15.0 one line of about 300 characters named the first item and
counted the rest (cheaper at 20 items, but it hid what the other 19 were, and carried a sentence of instructions each time). Observed on 2026-10-25 (PR #117's review): one task to a Mac was shown to 31 sessions and claimed after
19 hours, so a line that is repeated at every start is paid once per session for as long as the item lives.

## The one piece still missing: salience decay and top-N sensing

An open want costs the same line at every session start for its whole life (3 days by default), however old it is and
however many sessions already told the person. **Proposal, no code yet.** Give each open item a salience
`s = w(prio) * 0.5^(age / h(prio))`, with weights urgent 4, ask 2, fyi 1 and half-lives of 24 h, 6 h and 1 h (a task the
person has not touched for two half-lives is a quarter as loud as when it arrived). A session start senses only the items
with `s >= 1` ranked by `s`, at most N of them (N = 1 by default, 3 for the person's own terminal), plus the existing count,
and an item a session was already told (`person-told`, which exists) is not repeated until its salience rises again, for
instance when its done-when is still false and its deadline gets near. The inputs are all in the record (priority, time,
deadline), so it is one pure function in `internal/view`, testable like the budget above.

Token numbers (estimates, at 4 characters per token, for a Mac with 40 session starts a day and 20 open items of mixed age;
they are PREDICTED from today's measured 537 characters, not measured with decay): today 40 x 537 = 21,480 characters, about
5,400 tokens a day. With top-1 and told-once: the first start of a session shows one line (at most 160 characters), later
starts show nothing unless something new or louder arrived, so about 15 sessions x 160 = 2,400 characters, about 600 tokens a
day, and 0 once every item is settled or has decayed below 1. The risk is a quiet item that matters and fades: the count
line (`+N more`) and `aivoicebus status` keep it one command away, and a done-when or a deadline never decays.
