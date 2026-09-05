# Network access

## The network gate

Drift Installs are typically served on an internal network -- behind a VPN,
a private link, or a similar network gate managed by your organisation. The
CLI must run from a machine that can reach the endpoint.

If you cannot reach the endpoint, connect to your organisation's VPN first.

Environments created with `--public` (or toggled with `drift env share`) are
reachable from the public internet without the VPN. The CLI itself still needs
to reach the drift server to manage them.

## drift doctor

`drift doctor` diagnoses connectivity in dependency order:

1. **config** -- is a context configured with an endpoint?
2. **network** -- can a raw TCP connection reach the endpoint's host and port?
3. **discovery** -- does the server respond to `/.well-known/drift.json`?
4. **auth** -- does the stored credential authenticate?
5. **version skew** -- is the client older than the server's stated minimum?

Later checks are **skipped**, not failed, when an earlier one fails. A
credential cannot be judged against a server that is unreachable, so the
doctor does not try.

```bash
drift doctor
```

The output names the first thing to fix. Common patterns:

| Doctor says | Likely cause | Fix |
| ----------- | ------------ | --- |
| network: cannot resolve `host` | Not on the VPN | Connect to your VPN |
| network: timed out | VPN connected but route missing | Check VPN routing |
| discovery: fail | Server down or wrong endpoint | Verify the endpoint URL |
| auth: fail | Token expired or revoked | `drift auth login` with a fresh token |
| version skew: warn | CLI is older than recommended | Update the CLI (advisory only) |
