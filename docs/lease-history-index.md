# Scoped lease history index

The server defaults to `off`. The indexed modes change local storage and
resident projections; they do not change the replication protocol or erase
history. Enablement requires the release's replay, resource, compatibility and
signed-artifact checks followed by an isolated canary.

## Modes and scope

* `off` preserves the existing in-memory replay path and creates no sidecar.
* `observe` builds a derived index while retaining the existing resident
  message and acknowledgement views. It does **not** cap memory.
* `on` keeps scoped lease payloads and acknowledgement identities on disk,
  including active and recent claims. Logical claims remain available through
  complete checked queries. Permanent managed payload and read-cache residency
  is zero; per-request lease pages contain at most 256 references and 8 MiB of
  payload. The 4096-record/8-MiB hot and 8-MiB cache budgets are ceilings, not
  limits on how many claims may exist. Valid populations above those ceilings
  remain visible and renewable.

Only explicitly allowlisted hosts' self-addressed `fleet-runner` or
`fleet-runner.<32 lowercase hex digits>` lease messages qualify. They must be
standalone root messages with a valid expiry and a nonempty `deploy:prod:`
resource suffix. Operator-wide `deploy:prod` claims, ordinary messages, tasks,
replies, test messages and other senders remain outside this scope.

The keep interval defaults to 30 days and cannot be shorter. It only removes
expired scoped identities from transient Who/Roster projections once their
expiry is at least that old. Moving the clock backwards restores the applicable
view. Exact message lookup, threads, receipt/retraction state, raw replication
and exports retain the full historical data regardless of age. An ordinary
message from the same identity also preserves its ordinary presence semantics.

## Integrity and resource behavior

`log.jsonl` remains authoritative. The private derived `lease-history.db` is
rebuilt from its raw byte offsets on startup; record IDs, origin sequence
numbers, version vectors, signatures and first-arrival collision behavior are
unchanged. Unknown acknowledgements are indexed on disk and hydrated in raw
arrival order if their target later becomes ordinary data. No permanent heap
map or slice is added per cold message, unknown target or nonce identity.

An indexed append syncs the raw file before committing the derived index. An
uncertain append or failed index update makes the store unhealthy until a
successful reopen/rebuild. Raw reference identity, generation and digest checks
also apply to normalized/quarantined records and existing test-lane overrides.
Independent withdrawal proofs and receipt relations/counts prevent a missing
single derived entry from silently freeing a hold or suppressing a notice.

Lease HTTP replies retain the legacy JSON-array format. A request captures its
version vector, raw end and index/file generation. Short read transactions end
before network writes; a slow reader does not pin a bbolt transaction or the
Store mutex. Requests have a 30-second preparation/read/write deadline. Only a
complete successful iterator may close the array. Mid-stream I/O failure or
cancellation leaves an invalid/incomplete response, so older clients also fail
closed. A changed current raw generation or failed checked legacy view returns
HTTP 503, never a valid empty lease set.

Selected caller-owned outputs can grow with the requested data, especially full
historical message/thread/receipt views. Ordinary history remains resident and
uncapped by this feature. The raw log and derived index also grow with preserved
audit history. This is a bound on permanent **scoped lease** projections, not a
claim that all process memory, disk usage or arbitrary response sizes are
bounded. Keep existing filesystem-capacity and service-health alerts enabled.

Local LastHour counters use a fixed 256-segment buffer and an indexed tape. All
non-test arrivals share one ordered tape in On mode, preserving the existing
prefix behavior during clock rollback. New arrivals may cause a batched counter
write when Stats is read. Repeated Stats without new arrivals and healthy lease
GETs without expiry-window changes perform no metadata writes. Durability is
not weakened to reduce write amplification; startup rebuild and append/renew/
query write cost must be measured before rollout.

## Files, recovery and rollback

Indexed modes require a private owned data directory, owned single-link regular
0600 raw/index/lock files and a nonblocking exclusive writer lock. The resolved
systemd StateDirectory path is supported. A second indexed writer is refused.
Only this tool's validated numeric `.lease-history-rebuild-*` scratch files are
eligible for startup cleanup; ordinary data and backups are untouched.

Stop the indexed server normally before switching binaries or modes. Keep a
verified copy of raw data and the previous binary/configuration. A missing,
corrupt or stale derived sidecar is rebuilt from raw authority; a damaged raw
log must not be papered over with an empty index. Existing torn-tail recovery
and test-lane body cleanup retain their previous semantics.

An older binary can replay the unchanged raw format. Current small stores can
therefore roll back without conversion. Once years of scoped history accumulate,
older binaries will again load that history into RAM: assess capacity before a
rollback and retain a tested indexed binary as the preferred recovery artifact.
Never delete raw audit records to make an old binary fit.

## Explicit serve configuration

The opt-in source interface is:

```sh
aivoicebus serve --lease-retention observe --lease-retention-hosts builder-name
# After the signed release and isolated resource/compatibility canary gates:
aivoicebus serve --lease-retention on --lease-retention-hosts builder-name --lease-retention-keep 720h
```

Add these to the existing reviewed service configuration while preserving its
address, data directory, tokens, peer URLs and signing requirement. The examples
show only the new flags, not a replacement service command. No environment
variable silently turns the feature on. Invalid mode, keep interval, wildcard,
empty On allowlist or malformed host is refused before token reads or data-file
creation. `server.Options.LeaseRetention` and `store.WithLeaseRetention` expose
the same validation for embedded callers. Server shutdown cancels replication
before closing the derived store and releasing its writer lock.

The existing authenticated `/metrics` export includes the fixed retention mode,
indexed record count, scoped-message count, derived-index bytes, retained read
cache bytes and fixed arrival-buffer segment count. It never labels series with
a lease resource, agent nonce or message ID. These metrics exclude caller-owned
outputs and ordinary resident history. A failed checked store returns HTTP 503;
monitor scrape failure as well as the enabled mode's resource trends.
