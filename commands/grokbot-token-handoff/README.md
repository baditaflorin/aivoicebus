Fetch one configured Fleet Secrets record through a trusted SSH host using a 5-minute, one-use consumer key, then write it to a private local file. Prints only whether it wrote or reused the file, its byte length, and a SHA-256 prefix. Takes no arguments. Requires `~/.config/aivoicebus/grokbot-token-handoff.json` with mode `0600`; the consumer must already be allowlisted for that record. The command runs outside the worker sandbox because it needs SSH and writes under the user's cache directory.

Example local config (replace the example values; never put the secret value here):

```json
{
  "ssh_target": "fleet-admin-host",
  "vault_url": "https://fleet-secrets.example.com",
  "secret_name": "machine-token-record",
  "key_slug": "service-consumer",
  "consumer": "service-consumer",
  "output_file": "~/.cache/service-xfer/token"
}
```

The key slug must equal the allowlisted consumer. The script checks the key issue dry-run and key listing before reading the record. Repeated runs atomically refresh the same private file if its value changed. Anyone allowed to send `[run]` tasks to this worker can invoke the command, so keep the worker sender allowlist narrow; there is no per-command approval prompt.
