# CI and automation

## Authentication

Pipelines authenticate with `DRIFT_TOKEN`. A human mints a role-appropriate
token in the web UI at `<endpoint>/credentials` and stores it as a CI secret.

- Pipelines **cannot mint tokens** -- a token can never mint another token.
- Pick the TTL accordingly (max 30 days).
- Rotation is re-mint in the UI + replace the CI secret.
- `DRIFT_TOKEN` overrides all stored credentials and suppresses interactive
  login.

## Output contract

### Formats

| Flag | Description |
| ---- | ----------- |
| `-o table` | Human-readable table (default) |
| `-o wide` | Table with additional columns |
| `-o json` | Full JSON output |
| `-o yaml` | Full YAML output |
| `--json field1,field2` | JSON with exactly those fields |

Data goes to **stdout**, diagnostics to **stderr**, so
`drift env list -o json > envs.json` produces a file that parses cleanly.

### Scripting with --json

`--json field1,field2` emits exactly those fields. The field names are
validated against a CLI-owned stable contract -- the recommended scripting
surface:

```bash
drift env list --json slug,status,expires
drift env get my-env --json slug,status,id
drift release status --json service,stg_tag,rc_tag
```

### Non-interactive confirmations

Pass `--yes` to skip interactive confirmation prompts. Without `--yes`,
destructive commands refuse to run in a non-interactive session.

## Exit codes

| Code | Meaning |
| ---- | ------- |
| 0 | Success |
| 1 | Error |
| 2 | Usage -- bad flags or arguments |
| 3 | Not found |
| 4 | Authentication required |
| 5 | State conflict or operation failed |
| 6 | Wait timed out |
| 7 | Rate limited -- retry after the interval the server names |

Exit codes are a public contract. Script against them:

```bash
drift env get my-env --json status 2>/dev/null
case $? in
  0) echo "found" ;;
  3) echo "not found" ;;
  4) echo "need to re-authenticate" ;;
  *) echo "unexpected error" ;;
esac
```

## Rate limits

Limits are per-credential, with two buckets:

| Bucket | Default |
| ------ | ------- |
| Reads | 120 requests per 60 seconds |
| Mutations | 20 requests per 60 seconds |

A 429 response carries `Retry-After` in whole seconds and produces exit
code **7**. Poll reads freely; back off on writes.

The CLI never retries a mutation automatically -- a write is a deliberate act,
and silently repeating one is worse than reporting it.

## The api escape hatch

`drift api <method> <path>` sends a raw request to the drift API. The path is
relative to the discovered API base:

```bash
# GET a resource.
drift api GET /repositories

# POST with a body from a file.
drift api POST /environments --body @payload.json

# POST with a body from stdin.
echo '{"slug":"test"}' | drift api POST /environments --body -

# Include response headers.
drift api GET /environments --include
```

Flags:

| Flag | Purpose |
| ---- | ------- |
| `--body @file` or `--body -` | Request body |
| `-H key=value` | Request header (repeatable; Authorization is refused) |
| `--include` | Print status line and headers to stderr |

The response body is written to stdout byte-exact. Non-2xx responses are
decoded through the standard problem envelope, so exit codes and hints match
the typed commands.

## Example: CI environment lifecycle

```bash
#!/usr/bin/env bash
set -euo pipefail

# Create an environment for this PR branch.
drift env create \
  --slug "pr-${PR_NUMBER}" \
  --repo "my-service:${BRANCH}" \
  --ticket "${TICKET}" \
  --ttl 24 \
  --yes

# ... run tests against the environment ...

# Tear it down.
drift env rm "pr-${PR_NUMBER}" --yes --wait
```
