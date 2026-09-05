# Releases and promotions

## Channels

Drift manages three release channels:

| Channel | Purpose |
| ------- | ------- |
| `stg` | Staging -- automated deploys from the default branch |
| `rc` | Release candidate -- promoted from stg (or hotfixed) |
| `prd` | Production -- promoted from rc, requires elevation |

## Service naming

Services are named by their **helm chart key** -- the same names
`drift release status` prints. The chart key is the stable identifier across
commands; it may differ from the Kubernetes namespace your operators use.

## Promotion types

### rc

`drift release promote rc <service>...` retags each named service's current
stg image as rc and dispatches the retag workflow. Services with a shared
repository are dispatched once.

### hotfix

`drift release promote hotfix <service>... --branch <branch>` builds a branch
straight to rc, bypassing stg. This is for emergencies -- nothing has
validated the build before it reaches rc.

### prd

`drift release promote prd <service>...` promotes from rc to production.
**Requires an elevated credential** scoped to `promote:prd`. See
[Promotions guide](../guides/promotions.md) for the full runbook.

### prd hotfix

`drift release promote prd hotfix <service>... --branch <branch>` builds a
branch straight to production, bypassing both stg and rc. Requires the same
elevated credential as a normal prd promotion.

## Concurrency

The concurrency guard is **service-scoped**:

- Disjoint services (different service, different repo, different application
  group) promote in parallel.
- Overlapping services get a **409 conflict**.
- A promotion whose target already runs the requested tag healthily completes
  without dispatching a new workflow.

## Blocking

Promotions **block by default** with a 20-minute timeout. Pass `--no-wait` to
return as soon as the workflows are dispatched.

## Inspecting release state

```bash
# What is deployed to stg and rc right now?
drift release status

# Past promotions, newest first.
drift release history
```

`drift release status` shows promoted image tags and commits from gitops,
joined with live pod health from Kubernetes. A failure on either side degrades
to a warning rather than failing the request, so partial data is normal and
reported as such.
