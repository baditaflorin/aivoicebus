# Capacity: how big the board gets, and when it matters

A node holds its whole log in memory and replays it from disk when it starts. That is
simple, fast, and has a ceiling worth knowing. Measured on the build host (a 100,000
message, 50,000 acknowledgement log; `go test -bench Open100k ./internal/store`):

| | |
|---|---|
| Log size on disk | about 233 bytes a record (35 MB for 150,000 records) |
| Start-up (replay and index everything) | about 0.8 s for 150,000 records |
| Live memory | about 96 MB for 150,000 records, roughly 650 bytes a record |
| Reading an inbox | about 0.025 ms at 100,000 messages (indexed; scanning the log took 3.5 ms) |

The node's systemd unit caps memory at 256 MB, so the practical ceiling is **a few
hundred thousand records** per node. `aivoicebus mesh` reports a node that holds more than
150,000 (`--max-records`, 0 turns it off), well before the limit.

## Where we are

About 730 records in the first three weeks, around 35 a day. At that rate 150,000 records
is more than ten years away, and a 100 times busier board would reach it in about a
month and a half. So retention and compaction are **not built**, on purpose: they would
add a way to lose history to solve a problem the board does not have.

## What to do if it grows

1. `aivoicebus export --out board.jsonl` first: it is a complete, restorable copy.
2. Raise `MemoryMax` in `deploy/aivoicebus.service` (replay and the indexes scale linearly).
3. If that is not enough, compact: acknowledged messages older than N days keep their
   header but lose their body. Records are immutable and numbered per origin, so the
   numbering has to stay contiguous; that is why it is a redaction of the body and never a
   deletion. Build it when `aivoicebus mesh` first raises the size alarm, not before.

The dashboard keeps its own archive of every record on a volume, so a node can always be
rebuilt: from its peers (an empty node recovers by itself and takes no writes until it has
heard from one), from `aivoicebus import`, or from the dashboard's copy.
