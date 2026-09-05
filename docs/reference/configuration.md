# Configuration

## Config directory

The config directory is resolved in this order:

1. `DRIFT_CONFIG_DIR` environment variable
2. `$XDG_CONFIG_HOME/drift`
3. `~/.config/drift`

## config.yaml

The main config file is `config.yaml` inside the config directory. It stores
contexts and defaults and **no secrets**, so it is safe to commit to a
dotfiles repository. Note that config.yaml does contain context names and
endpoint URLs, which may reveal internal hostnames; keep your dotfiles
repository's visibility consistent with your organisation's policy.

```yaml
current-context: prod
contexts:
  - name: prod
    endpoint: https://drift.example.com
```

The file is created with mode 0600.

## Credential storage

Credentials are resolved in this order:

1. `DRIFT_TOKEN` environment variable (always wins)
2. OS keyring (macOS Keychain, GNOME Keyring, Windows Credential Manager)
3. `credentials.yaml` in the config directory (mode 0600, headless fallback)

`drift auth login` reports which store was used. The CLI never stores
`DRIFT_TOKEN` -- it is an in-memory override.

## Discovery cache

The server's discovery document (`/.well-known/drift.json`) is cached in the
config directory with an ETag. The CLI revalidates rather than expires it.
`drift doctor` forces a fresh fetch past the cache.

## Environment variables

| Variable | Purpose |
| -------- | ------- |
| `DRIFT_TOKEN` | Credential override; suppresses interactive login |
| `DRIFT_CONTEXT` | Context override (same as `--context`) |
| `DRIFT_ENDPOINT` | Endpoint override (same as `--endpoint`) |
| `DRIFT_OUTPUT` | Output format override (same as `-o`) |
| `DRIFT_CONFIG_DIR` | Config directory override |
| `NO_COLOR` | Disable colour output |

## Global flags

| Flag | Default | Purpose |
| ---- | ------- | ------- |
| `--context` | current context | Context to use |
| `--endpoint` | context's endpoint | Server endpoint to use |
| `-o`, `--output` | `table` | Output format: `table`, `wide`, `json`, `yaml` |
| `--json` | | Emit JSON with exactly these comma-separated fields |
| `--timeout` | `30s` | Per-request timeout |
| `--no-color` | `false` | Disable colour output |

Override precedence: **flag > environment variable > per-context default > built-in default**.

For example, the output format is resolved as: `--output` flag, then
`DRIFT_OUTPUT`, then the context's `output` field in config.yaml, then
`table`.
