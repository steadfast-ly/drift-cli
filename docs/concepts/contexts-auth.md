# Contexts and authentication

## Contexts

A **context** is a named endpoint plus the credential stored against it. Each
drift deployment you work with gets its own context.

```bash
drift context add prod --endpoint https://drift.example.com
drift context use prod
drift context current
drift context list
drift context remove prod
```

### Switching contexts

`drift context use <name>` sets the current context. The current context is
used by every command unless overridden.

### Overriding the context

For a single invocation, flags and environment variables override the current
context:

| Override | Flag | Environment variable |
| -------- | ---- | -------------------- |
| Context | `--context` | `DRIFT_CONTEXT` |
| Endpoint | `--endpoint` | `DRIFT_ENDPOINT` |

Precedence: **flag > environment variable > current context**.

### Re-pointing and removing

Re-pointing a context to a different endpoint (`drift context add prod
--endpoint https://other.example.com`) deletes the credential stored against
the old address, because it would otherwise be unreachable through both
`auth logout` and `context remove`.

`drift context remove <name>` deletes both the context and its credential.
Pass `--keep-credential` to leave the credential in place.

## Credentials

### Minting

Credentials are minted in the web UI at `<endpoint>/credentials`. You sign in
through your organisation's SSO and the page mints a scoped token.

A token can never mint another token, and pipelines cannot mint. This is by
design: every credential traces back to a human who signed in through SSO.

### Roles

Roles form a hierarchy:

| Role | Can do |
| ---- | ------ |
| `read-only` | List and inspect environments, releases, audit log |
| `preview` | Everything above, plus create/manage preview environments |
| `release` | Everything above, plus promote to rc and hotfix |
| `admin` | Everything above (admin-only operations are web-UI-only) |

You can mint a credential at or below your own role. A token's effective
authority at request time is **min(role at mint, owner's current role)** --
demotion narrows existing tokens, promotion never widens them. If the owner
is deactivated, all their tokens stop working.

### Scopes

Exactly one scope exists: `promote:prd`. Ordinary mints cannot carry it.
Production promotion requires a short-lived elevated credential minted through
a dedicated UI flow (see [Promotions](../guides/promotions.md)).

### Revocation

Credentials are revoked per-credential in the web UI. Revocation is effective
on the next request.

### Credential scoping gate

A stored credential is **only** sent to the endpoint of its own context. This
prevents a typo, a copied command line or a hostile README from sending your
credential to an unintended endpoint.

If you combine `--context prod` with `--endpoint http://elsewhere`, the CLI
refuses to send the stored credential and says so. To authenticate against an
ad-hoc endpoint, set `DRIFT_TOKEN` -- exporting a token is an explicit act.

### DRIFT_TOKEN

`DRIFT_TOKEN` overrides all stored credentials and suppresses interactive
login. This is how CI authenticates.

When `DRIFT_TOKEN` is set:

- `drift auth login` refuses to run (the credential from the environment
  always wins, so storing another would have no effect).
- `drift auth logout` warns that `DRIFT_TOKEN` is still in the shell.

### auth status

`drift auth status` shows:

- Context and endpoint
- Credential fingerprint and source (keyring, file, or environment)
- Identity (email), role, scopes
- Expiry (warns when under 24 hours)

```bash
drift auth status
```
