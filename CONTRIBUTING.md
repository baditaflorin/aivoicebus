# Contributing

Thanks for helping. AiVoiceBus is deliberately small: Go standard library only, no third-party modules. A pull
request that adds a dependency will not be merged without a very good reason.

## Before you start

- Open an issue for anything bigger than a bug fix, so we agree on the shape first.
- Security issues: see [SECURITY.md](SECURITY.md), not the issue tracker.

## Build and test

    scripts/build.sh linux amd64 dist/aivoicebus   # stamped build (VERSION file + git commit)
    go vet ./... && go test ./...                  # unit tests
    scripts/check.sh                               # the whole gate

`scripts/check.sh` runs shell and unit-file lint, gofmt, vet, race tests, a stamped build, the end-to-end smoke
transcript compared with `scripts/smoke.golden.txt`, and the mesh lab (`scripts/lab.sh`: real nodes and the real deploy
scripts on loopback, Linux only because it binds 127.0.0.11-16). The race detector and the lab are heavy and the lab
starts real processes, so run the gate on a Linux machine or in a container, not on a laptop you care about, for
example in the image CI uses (its digest is in `scripts/build-image`):

    docker run --rm -v "$PWD":/src -w /src "$(cat scripts/build-image)" \
      bash -c 'apt-get update -qq && apt-get install -y -qq shellcheck systemd && scripts/check.sh'

Set `AIVOICEBUS_SKIP_LAB=1` to leave the lab out while you iterate.

## Rules

- Write tests for every behaviour you add or change, and make sure a test can fail (run it once against the old code).
- Never put credential-shaped literals in tests: build them from fragments at run time, so scanners and GitHub push
  protection stay quiet (see `internal/secrets/corpus_test.go`).
- A new package under `internal/` must be added to the dependency table in `arch_test.go`, with the smallest set of
  imports it needs; dependencies point one way.
- If user-visible behaviour changes, update `README.md`, `docs/` and `internal/assets/usage.md` in the same commit, and
  the golden smoke transcript (`scripts/check.sh --update`, then read the diff).
- The wire format is pinned by `internal/msg/testdata/*-vectors.json`. Changing those files is a protocol change: say
  so in the pull request.
- Keep comments short and about why, match the surrounding code, run `gofmt`.

## Sign-off

Add `Signed-off-by: Your Name <you@example.com>` to each commit (`git commit -s`), certifying the Developer Certificate
of Origin (https://developercertificate.org). Contributions are licensed under the project's licence, Apache-2.0.

## Pull requests

One logical change per pull request, with a description of why and the test output. CI must be green. Be kind: see
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
