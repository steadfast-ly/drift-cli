# drift CLI

drift is the command-line client for **drift**, the preview-environment and
release-management service. One binary talks to every deployment: a named
**context** pairs an endpoint with a stored credential, and
`drift context use <name>` switches between them.

## What drift does

drift manages two things:

- **Preview environments** -- ephemeral, branch-scoped deployments for testing
  and review. Create them from a branch, extend their lifetime, swap branches,
  sleep and wake them, tear them down.
- **Release promotions** -- moving service images through the `stg`, `rc` and
  `prd` channels, with concurrency guards, elevation requirements and
  audit logging.

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

| Goal | Page |
| ---- | ---- |
| Install the CLI | [Install](getting-started/install.md) |
| Connect to your first deployment | [First steps](getting-started/first-steps.md) |
| Understand contexts and credentials | [Contexts and authentication](concepts/contexts-auth.md) |
| Create and manage preview environments | [Preview environments](guides/preview-envs.md) |
| Promote services through stg/rc/prd | [Promotions](guides/promotions.md) |
| Use drift in CI or with an LLM agent | [CI and automation](guides/automation.md) |
| Look up a command | [Command reference](reference/commands/index.md) |
| Diagnose a problem | [Troubleshooting](troubleshooting.md) |
