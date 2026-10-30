# Drift

Drift is the preview-environment, release and QA service your team runs at its
own address. It gives developers a deployment for each branch under review,
release managers a single place to promote images, and QA a queue tied to your
issue tracker.

## Three surfaces, one state

Everything below shows the same environments, releases and verdicts.

**The web UI** at your deployment's address (for example
`https://drift.example.com`). Sign in through your organisation's SSO. The
sidebar carries **Previews**, **Releases**, **Repositories**, **Audit Log**,
**CLI Credentials** and **Stack Profile**; **KPIs** and **QA** appear when your
admin enables those modules. The version line at the foot of the sidebar
carries a **Docs** link to this site.

**The `drift` CLI**, for the terminal and for CI. It has the same authority as
your web session and nothing more; production promotion from the CLI
additionally needs an elevated credential. One binary talks to every
deployment: a named **context** pairs an endpoint with a stored credential,
and `drift context use <name>` switches between them.

**Statuses and comments on GitHub pull requests.** Drift posts the
`drift/e2e` status for an end-to-end run and, where QA is enabled, the
`drift/qa` status for QA coverage. When your admin enables PR comments and a
preview environment starts running, Drift can comment on the linked pull
request with the environment's URLs, opening with
`Environment **<slug>** is running:` and one line per service.

## What Drift does

- **Preview environments** -- ephemeral, branch-scoped deployments for testing
  and review. Create them from a branch, extend their lifetime, swap branches,
  sleep and wake them, tear them down.
- **End-to-end runs** -- trigger a test suite against an environment and read
  the result on the pull request.
- **Release promotions** -- moving service images through the `stg`, `rc` and
  `prd` channels, with concurrency guards, elevation requirements and audit
  logging.
- **QA queue** -- on deployments that enable the module, the Tickets your
  tracker sends to QA, their linked pull requests and the verdicts that post
  `drift/qa` on the covered commits.

## 5-minute tour

```bash
# 1. Point the CLI at your deployment.
drift context add prod --endpoint https://drift.example.com

# 2. Mint a credential in the web UI at <endpoint>/credentials.
#    Paste it when prompted:
drift auth login

# 3. Verify everything works.
drift doctor

# 4. Create a preview environment from the current branch.
drift env create

# 5. Check what is deployed to stg and rc.
drift release status
```

## Where to go next

| You are | Start here |
| ------- | ---------- |
| New to Drift | [Install](getting-started/install.md), then [First steps](getting-started/first-steps.md) |
| A developer | [Web UI](getting-started/web-ui.md), [Preview environments](guides/preview-envs.md), [PR statuses](guides/pr-statuses.md), [End-to-end tests](guides/e2e.md) |
| In QA | [QA queue](guides/qa.md), [Roles](concepts/roles.md) |
| A release manager | [Promotions](guides/promotions.md), [Releases and promotions](concepts/releases.md) |
| An admin | [Administration](guides/administration.md), [Roles](concepts/roles.md) |
| Wiring up CI or an agent | [CI and automation](guides/automation.md), [LLM agents](guides/agents.md) |

Looking for a specific command? The [command reference](reference/commands/index.md)
lists every `drift` command and flag. If something is not working, start with
[Troubleshooting](troubleshooting.md).
