# 0.15.2

Long-running deployment automation creates a new internal lease identity for each run. Keeping every expired lease and receipt in resident message indexes eventually consumes the broker memory limit. This release adds an opt-in disk index and complete read-through views for that history.

The default is `--lease-retention off`. `observe` builds a derived index while retaining the existing in-memory views. `on` offloads only exact scoped deployment lease records from explicitly configured builder hosts; active holds, renewals and historical lookups remain visible. A 30-day retention window removes only expired internal identities from transient Who/Roster views. It does not delete the raw log, ordinary messages, tasks, receipts or audit history.

The index is derived from the canonical raw log and can be rebuilt. Raw records are synced before their index transaction is acknowledged. Checked reads fail closed on storage or integrity errors, and streamed lease responses never finish as a valid partial result. Captured generations and bounded pages let renewals and test-lane rewrites continue while a client reads slowly.

Resident managed payloads and their nonce indexes move to disk. Complete selected response bodies and ordinary message history can still grow; this is not a limit on every source of process memory or preserved disk history. Rebuilding the index on startup incurs disk writes and replay time. The release records those costs separately from quiet reads and renewals.

See [lease-history-index.md](lease-history-index.md) for scope, configuration, metrics and rollback. Keep production configuration off until the signed candidate passes the mixed-version and isolated resource gates. An older binary can replay the unchanged raw log, but sufficiently large accumulated history may exceed its memory limit. The 0.15.1 client fail-closed lease fixes remain required for unattended deployment gates.
