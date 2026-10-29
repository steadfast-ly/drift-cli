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

## Web UI

### I don't see the QA tab

**QA** is a module, not a standard page. It appears in the sidebar only when
your admin has enabled it for the install. There is no per-user switch.

If the tab is present but the page refuses you, your role is below `preview`.
Ask your admin for `preview` or above; QA verdicts (approve, reject, remove a
link, carry over) also need `qa` or `admin` on top of the view. See
[Roles](concepts/roles.md) and [QA queue](guides/qa.md).

### My role changed but the CLI still says the old one

```bash
drift auth status
```

The **Role** column shows the role your next request will use: what both the
credential's mint role and your current role allow. Drift refreshes your
current role when you use the web UI, and a credential never goes beyond its
mint role. This is the rule in
[Roles](concepts/roles.md#how-a-role-change-reaches-you).

If your role changed sideways or up -- for example `release` to `qa` -- sign
in to the web UI and mint a new credential at `<endpoint>/credentials`, then
run `drift auth login` with the new token. An old `release` credential would
act as `preview`.

### A button is missing

Buttons and actions follow your role. On a page your role can open, controls
above your role are hidden. Some pages also have a floor role and refuse
lower roles entirely -- **Audit Log** and **KPIs** need `preview`, **QA** needs
`preview`, and **Stack Profile** needs `admin`.

The common gates:

- **Add Repository**, **Edit**, **Deactivate**, **Delete**, **New group**,
  **Add Dependency**: `admin`.
- Promote or cancel a promotion: `release`.
- Approve, reject, remove a link, carry over: `qa` or `admin`.
- Start testing, link a PR, comment, send to QA, exempt: `preview` or above.

If a control you need is absent, ask your admin to check your role. The full
matrix is in [Roles](concepts/roles.md).

### Link PR list is empty

Drift learns about pull requests from forge webhooks. A webhook normally lands
within seconds; a missed delivery is picked up by the 10-minute reconcile.

- Confirm the PR is open and belongs to a repository registered under
  **Repositories**.
- Wait up to ten minutes and reload. If the PR is still unknown, ask your
  admin to check that the install receives forge webhooks.
- **Link PR** can only link a PR Drift already knows. From drift 0.32.1 the
  dialog lists the PRs Drift knows for the repository; an empty list means
  Drift has not seen a PR yet. On earlier servers the dialog takes a
  repository and a PR number, and answers not found for an unseen PR.
- A PR whose title or head branch carries a ticket key is linked
  automatically. **Link PR** is for a PR that carries no key.

### Start testing is missing

**Start testing** appears only when at least one partition of the Ticket's open
Linked PRs has no open session. If every partition already has one, close the
existing session first, or use it. When the row has a warning, or the Ticket
has more than one session, the primary action is **Review** instead and
**Start testing** moves into the row's `⋯` menu.

A Ticket with no open Linked PRs has nothing to run, so no session can start.
Add or link a PR, and check that the PR is still open and not merged.

A Ready for QA row the tracker approved but drift has not (the **Tested in
Jira** badge) has no **Start testing** primary, because starting it would test
what the tracker already tested: an approver carries it over instead, and
everyone else finds **Start testing** in `⋯` for a deliberate retest.

**Start testing** also needs `preview` or above. A role below `preview` cannot
open the **QA** page at all.
