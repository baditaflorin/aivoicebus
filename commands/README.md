# Named commands for `aivoicebus run`

Each directory here is one command a machine's owner can switch on. `aivoicebus run MACHINE -- NAME ARGS`
runs it on that machine with no model and no shell (see "Named commands" in docs/reference.md).

| name | what it does | arguments |
|---|---|---|
| brightness | set the display brightness (macOS, Apple Silicon) | one number, 0 to 1 |
| uptime | print uptime and load | none |
| battery | print battery state (macOS) | none |

## Switch them on

On the machine, in your own terminal (installing the worker is refused inside an agent's session):

    curl -fsSL https://raw.githubusercontent.com/baditaflorin/aivoicebus/<COMMIT>/deploy/run-setup.sh | AIVOICEBUS_RUN_REF=<COMMIT> sh

Pin `<COMMIT>` to a full commit id you have read. By default it installs `brightness uptime battery`;
choose with `AIVOICEBUS_RUN_COMMANDS="uptime"`, and who may send with `AIVOICEBUS_RUN_SENDERS="machine-a machine-b"`.
Run it again any time to pick up new commands.

## Add one

Make `commands/NAME/` (lowercase letters, digits, `-` and `_`) with:

- `command.json`: the spec from `internal/worker/runcmd.go` (`argv`, `args` patterns, `sandbox`, `timeout`).
  `argv[0]` is an absolute path. `{{DIR}}` is replaced by the machine's `run.d` directory, for files you ship.
  Give every argument the narrowest pattern that works.
- `FILES`: one file name per line for the extra files to download (optional).
- `README.md`: one or two lines: what it does and its arguments.

`go test ./internal/worker` checks that every command here parses. A command is a program that runs on
other people's machines on a peer's request: keep it read-only or tightly bounded, and say in its README
what it changes.
