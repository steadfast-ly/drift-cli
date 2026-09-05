---
name: drift-cli
description: >
  Drive the drift CLI for managing preview environments and promoting releases.
  Use when: creating or managing preview environments, promoting to rc or prd,
  extending an environment, checking release status, "create a preview env",
  "promote to rc", "promote to prd", "extend an environment", drift CLI usage.
---

# drift CLI -- agent skill

drift is the CLI for the drift preview-environment and release-management
service. One binary, many deployments (Installs), configured via named
contexts.

## Prerequisite check

Before any drift command, verify connectivity:

```bash
drift doctor
drift auth status
```

If `drift doctor` reports `network: fail`, the Install is behind a network
gate (VPN). Tell the human to connect to their VPN -- do not retry blindly.

## Command map

```
drift auth login             Store a credential (paste-based or --token-stdin)
drift auth logout            Remove the stored credential
drift auth status            Show identity, role, scopes, expiry

drift context list           List configured contexts
drift context use <name>     Switch the current context
drift context current        Show the current context
drift context add <name>     Add or update a context (--endpoint required)
drift context remove <name>  Remove a context and its credential

drift env list               List environments (--status repeatable)
drift env get <ref>          Show one environment with services and builds
drift env create             Create a preview environment
drift env rm <ref>           Tear down an environment
drift env cancel <ref>       Cancel a building environment
drift env relaunch <ref>     Rebuild and redeploy every service
drift env sleep <ref>        Scale to zero (TTL clock freezes)
drift env wake <ref>         Bring a sleeping environment back
drift env extend <ref>       Add hours to TTL (--hours, additive, 120h cap)
drift env share <ref>        Make publicly reachable
drift env unshare <ref>      Revert to private
drift env add-service <ref> <repo>:<branch>
drift env remove-service <ref> <repo>
drift env swap-branch <ref> <repo>:<new-branch>
drift env retry-build <ref> [repo]
drift env wait <ref>         Wait for a state (--for <state>)

drift release status         What is deployed to stg and rc
drift release history        Past promotions
drift release promote rc <service>...
drift release promote hotfix <service>... --branch <branch>
drift release promote prd <service>...
drift release promote prd hotfix <service>... --branch <branch>

drift repo list              List repositories
drift repo branches <id>     List a repository's recent branches

drift audit list             Query the audit log
drift audit actors           List distinct audit-log actors

drift api <method> <path>    Raw API passthrough (escape hatch)
drift doctor                 Diagnose connectivity, auth, version skew
drift version                Client and server versions
drift completion <shell>     Shell completion (bash, zsh, fish)
```

## Machine-interface rules

- Always prefer `--json <fields>` or `-o json` for parsing. Never scrape
  table output.
- Data goes to stdout, diagnostics to stderr.
- Field names in `--json` are validated against a CLI-owned stable contract.

## Exit codes

| Code | Meaning | Agent action |
| ---- | ------- | ------------ |
| 0 | Success | Proceed. |
| 1 | Error | Read stderr for details. |
| 2 | Usage | Fix the command invocation. |
| 3 | Not found | The resource does not exist. |
| 4 | Auth required | If the error is `urn:drift:problem:elevation-required` (from `release promote prd*`), do NOT suggest `drift auth login` -- follow the production-promotion procedure above. For all other auth failures, ask the human to re-authenticate (`drift auth login`). |
| 5 | Conflict/failed | Read `data.type`: `invalid-transition` = do not retry unchanged; `conflict` = may retry. |
| 6 | Wait timeout | The operation may still complete server-side. Check with `drift env get` or `drift env wait`. Do not blindly re-run mutations. |
| 7 | Rate limited | Wait the `Retry-After` seconds, then retry. |

## Wait semantics

Commands that start work **block by default** (create, relaunch, wake,
retry-build, add-service, swap-branch, promote rc/hotfix/prd). Commands that
end work **return immediately** (rm, sleep, cancel).

Override with `--wait` or `--no-wait`. Follow up with:

```bash
drift env wait <ref> --for <state>
```

## GUARDRAILS

### Destructive and irreversible commands

The following commands are destructive, irreversible, or security-sensitive:

- `drift env rm`
- `drift env relaunch`
- `drift env remove-service`
- `drift env share` (exposes the environment outside the network gate)
- `drift env unshare`
- `drift context remove` (deletes the stored credential)
- `drift release promote rc`
- `drift release promote hotfix`
- `drift release promote prd`
- `drift release promote prd hotfix`
- Any non-GET `drift api` call -- require the human to approve the exact
  method, path, and body before executing.

**Confirm with the human before running any of these.** Never pass `--yes`
unless the human has already approved that exact action in this session.

### Production promotion

`drift release promote prd` is a production deployment. The **default** is
that the human runs the promotion command themselves. The agent must
**NEVER**:

- Mint or handle the elevated credential itself.
- Store, write to a file, echo, or log an elevated token.
- Run `drift auth login` with an elevated token (it overwrites the stored
  credential).

The human mints a 15-minute token in the web UI and runs the command
themselves. Only if the human **explicitly asks** the agent to run it may the
agent execute a single command with `DRIFT_TOKEN` injected by the human.
When doing so:

- Pin `--context <name>` to the exact deployment the human named, so the
  elevated token cannot fire at the wrong Install.
- Never echo, log, or write the token value.
- Execute exactly one command, then discard the variable.

```bash
DRIFT_TOKEN=drift_xxx drift release promote prd svc-a --context prod --yes
```

To keep the token out of shell history, use `read -rsp` to capture it
instead of pasting it inline.

### Rate limits

Max 20 mutations per minute per credential. Batch operations and pace
accordingly. Do not loop-extend TTLs -- extensions are additive with a
**120-hour lifetime cap**.

## Common recipes

### Create an environment for a branch

```bash
drift env create --slug my-feature --repo my-service:feature/branch --yes
drift env get my-feature --json slug,status,expires
```

### Extend an environment

```bash
drift env extend my-feature --hours 24
```

### Swap a branch

```bash
drift env swap-branch my-feature my-service:other-branch
```

### Check what is on rc

```bash
drift release status --json service,rc_tag,rc_health
```

### Promote to rc

```bash
drift release promote rc svc-a svc-b --yes
```

### Audit who did something

```bash
drift audit list --action environment.created --actor alice
drift audit list --environment <uuid> --sort timestamp --sort-dir desc
```

## Troubleshooting

Run `drift doctor` for end-to-end diagnostics.

Full documentation: <https://steadfast-ly.github.io/drift-cli/>
