# Releasing and updating

Commands that start with `deploy/` or `scripts/` are files in this repository: run them from the
root of an aivoicebus checkout (`git clone https://github.com/baditaflorin/aivoicebus && cd aivoicebus`),
not from any other directory, where the shell says `no such file or directory`.

## How a machine updates

`aivoicebus update` (ask the user first; `--check` only reports):

1. Fetches `manifest.json` and `manifest.sig` from the mesh nodes when a node URL
   is configured, otherwise from the project's public GitHub release over HTTPS
   (`--from mesh`, `--from github` or `--from dir:<path>` choose one). The GitHub
   source needs no `gh` command and no login, sends no credential, and follows
   redirects only to GitHub's own hosts over https. GitHub only transports the
   files: nothing is trusted until step 2.
2. Verifies the ed25519 signature against the trusted keys: the project's public
   key **compiled into the running binary**, a key a fork built in (see below),
   and any key the user added on purpose with `aivoicebus trust-key add`. No flag
   or environment variable can add a key.
3. Refuses anything that is not strictly newer (`--force` to reinstall or
   downgrade a signed release on purpose).
4. Downloads the binary for this OS/arch and checks its sha256 and size against
   the signed manifest.
5. Stages the verified bytes next to the installed binary and runs it once
   (`version`), so a corrupt, wrong-arch or mislabelled file never gets installed.
6. Replaces the installed binary with an atomic rename, keeping the old one as
   `aivoicebus.prev` (`aivoicebus update --rollback` puts it back).
7. Runs the new binary's `setup`, so the hook, instruction snippet and config come
   from the release.

A node that holds a release announces its number in a response header on every
authenticated call. The session-start hook turns that into one line for the
person, at most once a day whatever the release, on machines that are behind, and
`status` says it too; the line stops by itself once the machine runs that release.
No message on the mesh ever carries a command or URL, and no task is sent for a
release: `fleet update` lists who is behind and sends nothing (its `--apply` sent a
task per machine and release until 0.13.x, and none was read).

Nothing installs by itself unless the person on that machine turned on automatic
update, once, in their own terminal (`aivoicebus update --auto on`, which refuses
in an agent's session and asks for a typed word; `--auto off` stops it from
anywhere). Then a session start that sees a newer release starts
`aivoicebus update --auto run` as a process of its own, at most once a day: the
steps above with nothing forced, from the release the installed binary reports.
If the new binary's setup fails it puts the previous one back and leaves that
release out from then on, as it does for a release the person rolled back with
`update --rollback`. The next session start says once what happened.

## Who can sign for a machine

| Key | Where it comes from | For |
|---|---|---|
| the project key | compiled into every official binary (`internal/release/keys.go`) | official releases |
| a fork's key | `AIVOICEBUS_TRUST_KEY=<base64 public key> scripts/build.sh ...` sets it at build time (`-X ...internal/release.extraKey`); the official key stays trusted too | a company or fork that signs its own releases |
| a user-added key | `aivoicebus trust-key add <key>` writes `~/.config/aivoicebus/trusted-keys` (mode 600) | a team running its own mesh with its own signed releases and node lists |

`trust-key add` prints the key's fingerprint (`SHA256:...`, the shape `ssh-keygen -l` prints) and
asks you to type it, or takes it with `--fingerprint` so a script can check it against a value you got
another way. **A trusted key can sign anything this machine will install and run**, so add one only for
a mesh you run, and check the fingerprint with whoever made the key by a route other than the one the
key came from. The file is read only if it is a regular file only you can read and write: a file
others can write would let them redirect updates, so a looser one is ignored (and `trust-key list`
says so). `trust-key list` shows every trusted key and where it came from; built-in keys cannot be
removed, and `trust-key rm <fingerprint>` removes one you added.

## What CI checks

The project uses no hosted CI. `.woodpecker.yml` runs `scripts/check.sh` in the pinned image on a
self-hosted Woodpecker for every pull request and push to `main`, and the same command runs by hand
on any build host. That script is the single entry point: gofmt, vet, race tests, a stamped build,
the golden smoke transcript, the mesh lab, `scripts/lint.sh` (shellcheck at warning level over
`deploy/` and `scripts/`, `systemd-analyze verify` on a copy of the unit, and every hardening
directive in `deploy/aivoicebus.service` pinned to its value; change one on purpose and change
`scripts/lint.sh` in the same commit).

Run it yourself with `scripts/check.sh` on a build host (add `AIVOICEBUS_SKIP_LAB=1` to leave out
the lab's few minutes). Without shellcheck installed it prints `lint skipped (no shellcheck)` and
carries on; CI sets `AIVOICEBUS_REQUIRE_LINT=1`, so there a missing tool fails the run. Commits are
never marked `[skip ci]`. To move the image, replace the digest in `scripts/build-image` (`.woodpecker.yml` names the
same digest, and `scripts/reproduce.sh` reads the file) with the one
`docker inspect --format '{{index .RepoDigests 0}}' golang:1.25` prints after a pull.

## The mixed-version gate

A rollout goes one node at a time, so for a while the new release replicates with the old ones.
`scripts/compat.sh` proves that works before any node is touched: it builds the current tree and
runs it in a loopback mesh next to one node per older release, using the real signed
`aivoicebus-linux-amd64` from each release dir. Every release is checked first with this tree's
`aivoicebus release verify` (signature against the compiled-in key, every hash), and nothing runs
if one fails. The token is a throwaway, and no live node is contacted.

    scripts/compat.sh --dir dist --old 0.1.0 --old 0.2.0   # or no --old: every dist/release-X

It checks: replication both ways with equal version vectors and nothing quarantined; messages
with kind, prio, ttl and reach from each version's own CLI; acks and their receipts; retractions
written through `/v1/retract` and through each old node's `/v1/ack` with the carrier id (a release
that predates retraction keeps delivering them, which is the documented degradation);
byte-identical exports from every node; a record that the current rules reject, stored on an old
node (it is quarantined on every node that checks, and the vectors stay equal); a restart of the
new node; and each old node upgraded in place, rolled back, then upgraded again, with nothing lost.
The script prints PASS/FAIL for each check plus a table, and exits non-zero on any FAIL. What each
release should do comes from `feature_since` in the script. When a release adds a behaviour that
older nodes do not have, add a line there.

**Run it before every node rollout**, on the build host (Linux only; for example inside
`golang:1.25` like the lab, with `python3` and `curl`), never on a laptop. It takes about a
minute. A missing `dist/release-X` is downloaded with `gh release download` when `gh` is
installed. A build host without network access to GitHub cannot download
releases: copy the signed release dirs to it and pass `--dir`. It is not part of
`scripts/check.sh`, because that runs without the release dirs.

Run it on the release's own linux binary, so the record is about what will be signed:
`scripts/compat.sh --new dist/release/aivoicebus-linux-amd64 --record dist/release/compat-pass.txt`
(paths are relative to where you run it). On a full pass of a clean tree it writes the record: the
commit, the tree hash, the sha256 of that binary, the checks and scenarios counted, and the old
releases it ran against; it deletes any older record first. On a build host that got the tree
without `.git`, pass `COMPAT_COMMIT=<sha> COMPAT_TREE=<tree hash>` (from `git rev-parse HEAD` and
`git rev-parse 'HEAD^{tree}'` of the clean checkout). `scripts/release.sh sign` and `github` refuse
unless the record in the release dir (or `COMPAT_RECORD=<file>`) matches the commit, tree and linux
binary being signed, covers every release in `scripts/compat-releases.txt` older than this one
(append each release there when it is cut), and has a full run's scenarios and checks. To sign
without it, give a reason: `AIVOICEBUS_SKIP_COMPAT="<at least 10 characters>" scripts/release.sh sign ...`
(`0` or `false` are not reasons). It warns loudly, writes `OVERRIDES` into the release dir, and `github`
puts the waiver at the top of the release notes.

## Cutting a release

The build and the signing key are on different machines on purpose. Other sessions (people and
agents) merge to `main` while a release is in flight, so every step after the build works from
the release itself, never from what `main` or your checkout says now.

### Build on a build host, sign on the key machine

1. Bump `VERSION`, merge to `main`. Note the merge commit: it is the release commit.
2. **Build host**, in a checkout of exactly that commit:
   `git worktree add --detach ../rel-X.Y.Z <commit>`, then from it
   `scripts/release.sh build dist/release` (stamped binaries for darwin and linux, arm64 and
   amd64; `scripts/build.sh` refuses nothing, but a dirty tree is stamped `-dirty` and will not
   sign). From a `git archive` of the commit instead of a checkout, set `AIVOICEBUS_COMMIT=<short sha>`.
   Build it a second time and compare the sha256 of every binary (`scripts/reproduce.sh <commit>`
   does both builds): a release is only signed when the two builds are byte-identical.
   Copy `dist/release` to the machine that holds the signing key.
3. **Key machine:** `scripts/release.sh sign dist/release` packs, signs, verifies and writes
   `SHA256SUMS`. It takes the release number and the commit **from the binaries**: it runs the one
   built for this machine (`aivoicebus version`) and checks every other binary carries the same
   release and commit. It never reads the checkout's `VERSION` for the manifest (it only notes a
   difference), and it refuses a dirty or unknown commit, binaries built apart, a commit your
   checkout does not have (`git fetch origin` first), and a `VERSION` at that commit that says
   something else. `scripts/release.sh stamp dist/release` prints what it read.
   It also wants the mixed-version gate's pass record for that commit and binary (see above: run
   `compat.sh --new ... --record ...` on the build host after step 2, and copy the record with the release).
   **Rehearse it** before publishing anything:
   `scripts/e2e-update.sh dist/release <an older aivoicebus binary>`. It signs a
   copy with the real key, serves it from a local node, updates the older client
   over the mesh path in a scratch HOME, and then tries seven attacks (tampered
   binary or manifest, rogue key, downgrade replay, wrong-OS binary, version
   mislabel, bad token), each of which must leave the installed binary untouched.
4. GitHub: `NOTES=notes.md scripts/release.sh github dist/release`. It creates `v<release>` with
   `--target <the full sha of the commit stamped in the binaries>`, and refuses a manifest whose
   release or commit is not the binaries'. Never `gh release create --target main` by hand: `main`
   may already hold the next release's commits (and gh wants the full 40-character sha).
5. Run the mixed-version gate on the build host against the releases the nodes run now (see
   above), before any node is touched.
6. Nodes. Run the deploy scripts from the release commit too (the worktree of step 2, or a new
   `git worktree add --detach`), not from `main`, whose scripts may have changed since.
   One session at a time: `deploy/rollout.sh` and `deploy/publish-release.sh` take the **release
   lock** (`/etc/aivoicebus/release.lock` on the first node of `deploy/nodes.conf`) with `--apply`
   and refuse to start while another session holds it, or while that node is unreachable.
   `deploy/release-lock.sh status` says who holds it, since when and for what. A running rollout
   renews the lock every minute; a lock not renewed for two hours (`AIVOICEBUS_LOCK_STALE`, at
   least 60 seconds) is stale and the next run replaces it. A run killed with -9 leaves its lock
   until then (or until `deploy/release-lock.sh release <holder> --force`, once you know that run
   is dead). `--force-lock` takes a held lock over on purpose, and goes on without any lock (with
   a warning) when the lock node cannot keep one. `install.sh`, `rotate-token.sh` and
   `machine-token.sh` do not take the lock: they are short, and a token change (a leak response)
   must stay possible while a rollout is running.
   Canary first: `deploy/rollout.sh dist/release` shows the plan,
   `deploy/rollout.sh dist/release --apply` does it. It first verifies the release (signature
   and every file's hash) with your `aivoicebus release verify`, then installs the released
   Linux binary on the canary, checks the node reports the new release, every peer link is up
   and it holds no more quarantined records than it did before the install (a placeholder is
   permanent, so only an increase stops a rollout), soaks for 30 seconds (`--soak` takes a whole number of
   seconds, at least 1; a crash loop shows /healthz green only part of the time, so it also checks
   the service did not restart), then does the other nodes the same way. A canary already on the
   release (an interrupted rollout run again) is soaked all the same before the next node, and if
   that soak fails it is put back on `aivoicebus.prev` (or, with no different previous binary, the
   rollout says loudly that it is still on the bad release). Beside it runs `deploy/watch.sh`,
   which reads every node's `/v1/health` with the observer token every few seconds and stops the
   rollout when a node on the new release holds more quarantined records than when the watch began
   or a node that accepted the observer token stops accepting it; a watcher that dies stops the
   rollout too. A node that fails any check is
   rolled back to `/usr/local/bin/aivoicebus.prev` and the rollout stops, so the nodes after it are
   never touched. By hand it is
   `AIVOICEBUS_BIN=dist/release/aivoicebus-linux-amd64 deploy/install.sh --apply build`, then
   check `aivoicebus mesh`; to roll one back by hand:
   `ssh <node> 'install -m 755 /usr/local/bin/aivoicebus.prev /usr/local/bin/aivoicebus &&
   systemctl restart aivoicebus'` (`install`, not `cp`: a running binary cannot be written
   over). Then `deploy/publish-release.sh dist/release --apply` (dry run without
   `--apply`) puts the release on the nodes.
6. Check from any machine: `aivoicebus update --check`. Remove the worktree. Nothing
   needs sending to the machines: each one's session start tells its person once a
   day while it is behind, and the ones with `update --auto on` install it by themselves
   (`aivoicebus fleet` shows who runs what).

## The signing key

`aivoicebus release keygen` writes the private key to
`~/.config/aivoicebus/release-signing.key` (mode 600, never overwritten) and
prints the public key, which is pasted into `internal/release/keys.go`. The
private key never goes in the repo, on a node, or in a message. Keep the
signing copy on a machine that does not build or serve anything, or on a hardware
key, and back it up somewhere that is not the same disk. The key for the first
public release was generated on 2026-10-15; move its private half off the machine
that generated it before the first release is signed.

- **Lost key:** nothing already installed breaks, but no further release can be
  signed. Recovery is a new key pair, its public key in the source, and a fresh
  client bundle installed by hand on each machine.
- **Rotation:** release a build whose `builtinKeys` lists the new key as well as
  the old one, signed with the old key. Once every machine runs it, sign with the
  new key and drop the old one in a later release.
- **Leaked key:** treat as lost, and rotate as above. Whoever holds the key can
  sign a release every machine will accept.

## What is and is not protected

- A compromised node or the shared token cannot make a machine install a binary:
  without the private key the signature does not verify, and a swapped binary
  fails the signed hash.
- A node can withhold updates, or replay an old signed release; the latter does
  nothing, because only a strictly newer version installs.
- A node can announce a version that does not exist; the effect is one line for
  the user, and `update` verifies before doing anything.
- Anyone with the signing key can ship anything. That is the trust anchor.
