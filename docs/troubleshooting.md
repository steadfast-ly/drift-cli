# Troubleshooting

## Start with drift doctor

`drift doctor` runs five checks in dependency order. Later checks are
**skipped** (not failed) when an earlier one fails, so the first failure is
always the root cause.

```bash
drift doctor
```

| Check | What it tests |
| ----- | ------------- |
| config | Is a context configured with an endpoint? |
| network | Can a raw TCP connection reach the host and port? |
| discovery | Does the server respond to `/.well-known/drift.json`? |
| auth | Does the stored credential authenticate? |
| version skew | Is the client older than the server's stated minimum? |

## Common problems

### Endpoint unreachable

```
network: cannot resolve drift.example.com
```

The endpoint's DNS name is only resolvable from within the organisation's
network. Connect to your VPN and try again.

```
network: connecting to drift.example.com:443 timed out
```

The VPN is connected but the route to the endpoint is missing or blocked.
Check VPN routing and firewall rules.

### 401 after long idle

```
auth: credential drift_...abc was refused
```

The token has expired. Credentials have a maximum lifetime of 30 days. Mint a
fresh one at `<endpoint>/credentials` and run `drift auth login`.

### Exit 4 on prd promote

```
Error: Elevated credential required
```

Production promotion requires a 15-minute elevated credential scoped to
`promote:prd`. Mint one at `<endpoint>/credentials` using the "Elevate for
production" card, then retry with:

```bash
DRIFT_TOKEN=drift_xxx drift release promote prd svc-a --yes
```

See [Promotions](guides/promotions.md) for the full workflow.

### Version skew warning

```
version skew: client 0.1.0 is older than minimum 0.2.0
```

This is **advisory only** -- the CLI never refuses to operate based on version
skew. Update when convenient. Compatibility is governed by the server's
discovery document, not by matching version numbers.

### Keyring vs file ambiguity

When the CLI detects that both the OS keyring and the credential file hold a
credential for the same context, it **reports the conflict** instead of
guessing which is current. Run `drift auth login` to settle which one wins,
or `drift auth logout` to remove both.

### drift version works offline

`drift version` always works, even with no configuration and no network. It
shows the client version, API contract and build details. Server details are
shown when a context is reachable, omitted otherwise.

```bash
drift version
```
