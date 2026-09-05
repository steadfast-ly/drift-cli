# Environments

Preview environments are ephemeral, branch-scoped deployments for testing and
review.

## Addressing

Environments are addressed by **slug** or **UUID**. The server resolves both.

A slug resolves only environments that are neither `destroyed` nor `canceled`,
so a slug reused over time addresses the one live environment holding it.
To address a torn-down environment, use its UUID.

## Lifecycle

An environment moves through these states:

```
requested -> building -> deploying -> running
                |                       |
                v                       v
          build_failed             deploy_failed
                                        |
                                        v
                                     running  (recovery)
```

From `running`, operator-initiated transitions:

- **sleep** -- scale to zero, keeping data and TTL. The TTL clock is frozen.
- **wake** -- scale back up. Slept time is credited back to the TTL.
- **destroy** (`drift env rm`) -- tear down the environment and its namespace.
- **cancel** -- only from `building`, stops the build.
- **relaunch** -- rebuild and redeploy every service from current branch heads.

### Failure states

`deploy_failed` is **not** terminal. The server documents
`deploying -> deploy_failed -> running` as an expected recovery path (e.g. an
ArgoCD blip). The CLI's wait logic believes a failure state only after it has
persisted for 30 seconds, to allow for recovery.

`build_failed` similarly has recovery edges. A build still in flight is
positive evidence of recovery.

## TTL

| Parameter | Value |
| --------- | ----- |
| Default TTL | 48 hours |
| Extension range | 1--120 hours per request |
| Cumulative lifetime cap | **120 hours total** (creation + all extensions) |

Extensions are **additive** and not idempotent: extending by 24 hours twice
adds 48 hours total. The cumulative cap of 120 hours covers the entire
lifetime from creation through all extensions.

Sleeping **freezes** the TTL clock. Waking credits back the slept time, so a
sleeping environment does not burn through its allowance.

## Dependencies

The server resolves transitive dependency repositories and application-group
siblings automatically at create time. When adding a service to an existing
environment, its dependencies must already be present -- if they are not, the
server returns a 409 or 400.

## Visibility

Environments are **private** by default -- reachable only from within the
network gate (VPN). `drift env share` makes an environment publicly
reachable; `drift env unshare` reverts it.

Visibility is a property of the environment, not of the caller, so it applies
to everyone at once.
