# 0.15.1

Lease acquisition and renewal now fail closed when ownership cannot be confirmed. A successful
POST must be followed by a live exact-ID confirmation; HTTP errors, timeouts, empty responses and
stale previous claims cannot start or authorize continued work. Each renewal appends a bounded,
truthful `renewal of <previous-ID>` suffix to the original note, avoiding duplicate suppression
without self-replies tripping old-node pair brakes. The suffix never accumulates, and notes that
leave insufficient space are refused before acquisition. Key, TTL and wire format are unchanged.

`lease run` stops and reaps the command's process group when a renewal or ownership check fails,
and retracts only IDs created by that run. Renewal I/O is bounded before the current lease expiry.
Healthy command exit codes and explicit audited overrides retain their existing semantics.
Final cleanup failures are reported and the claim expires by TTL.

Interactive terminal stdin is conservatively refused before network requests or lease writes.
Pipes, files and `/dev/null` remain supported for unattended guarded work. No server storage or
history behavior changes in this release. The compatibility release matrix now includes 0.14.0
and 0.15.0, in addition to the previous entries.
