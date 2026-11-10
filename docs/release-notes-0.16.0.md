# 0.16.0

Grok Bot cannot run session hooks, so it never got the automatic message delivery that Claude Code, Codex and Gemini CLI get. Its runs are started by a routine's webhook, so this release lets the bus go to it.

`aivoicebus forward --webhook` listens as `HOST/grokbot`, shows in `who` and `ping` as listening, and is offered the tasks sent to its machine. It posts each new message or task for it once to a webhook. `forward ensure`, `forward status` and `forward stop` manage it. `setup --tools grokbot` is opt-in: it writes a start-up script and one rc-file line, and `uninstall` removes both.

The webhook URL and key come from `AIVOICEBUS_WEBHOOK_URL` and `AIVOICEBUS_WEBHOOK_KEY` (or the `AIVOICEBUS_GROKBOT_WEBHOOK_*` names, or the `webhook-url` and `webhook-key` files). They are never printed and are on the secret lists: sends refuse them and the worker sandbox hides the files. The URL must be https (http only to loopback), redirects are not followed, and errors never quote the URL. The key goes in `Authorization: Bearer KEY` by default; `AIVOICEBUS_WEBHOOK_HEADER` and `AIVOICEBUS_WEBHOOK_PREFIX` change that.

The forwarder keeps the safety model of the hooks and the worker. Forwarded text carries the untrusted banner. Bodies that hold a credential or are sealed for this machine are withheld, held and person-only messages are not forwarded, and neither are tasks deeper than 3. Quiet hours and `pause` are honoured. Each message is recorded before it is posted and acknowledged only after the webhook accepted it, with retry and backoff. Two loop brakes limit wake-ups to 20 an hour overall (`--max-wakes`) and 6 an hour per thread (`--max-per-thread`).

This is not a protocol change and touches no wire-format file. Nodes and older clients need nothing; a machine only changes behaviour when its person runs `setup --tools grokbot`. See [grokbot.md](grokbot.md) for setup and the reference for the forwarder section.
