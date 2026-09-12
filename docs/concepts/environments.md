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

## Database access

`drift env tunnel` and `drift env db` connect to an environment's database
through an embedded [chisel](https://github.com/jpillora/chisel) tunnel. The
tunnel runs in-process -- there is no external `chisel` dependency.

Both commands are **blocking**. `tunnel` holds the connection open until
Ctrl-C; `db` launches the interactive client and exits when it does.

### Exit codes

`env tunnel` exits 0 on a clean Ctrl-C. `env db` **propagates the client's
own exit code** -- if `psql` exits 2, `drift env db` exits 2. Scripts can
branch on the outcome without parsing output.

### No password resolution

The server returns tunnel coordinates but no database credentials. The
password is left to the client's own prompt. This matches the console: the
console's chisel command also leaves the password to the client. Inventing
client-side credential resolution would create a contract the server does not
support.

### MySQL TLS flags

MySQL's `caching_sha2_password` plugin (default since 8.0) requires TLS even
over localhost. The CLI keeps TLS **on** but skips server-cert verification,
because the tunnel presents a self-signed certificate:

| Client | Flag |
| ------ | ---- |
| Oracle `mysql` | `--ssl-mode=REQUIRED` |
| MariaDB `mariadb` | `--ssl --ssl-verify-server-cert=0` |

The two flag sets are not interchangeable. The CLI detects which client is
installed by inspecting `mysql --version` output.

### Server version floor

The `db-access` endpoint requires drift server >= 0.15.0. An older server
returns exit 3 with a version hint.

## End-to-end testing

`drift env e2e <slug>` triggers an end-to-end test run against an
environment. The server dispatches the configured test workflow and
returns a run id.

Without `--wait`, the command prints the run id and returns immediately
(exit 0). With `--wait`, it polls the audit log until the run's outcome
appears:

- **passed** -- exit 0.
- **failed** -- non-zero exit, with the failure reason when the server
  provides one.
- **error** (e.g. `dispatch_failed`) -- non-zero exit, with the reason.
- **timeout** -- exit 6. The default `--wait-timeout` is 125 minutes,
  sized five minutes past the server's own 120-minute tracking ceiling.

### Exit codes

| Scenario | Exit code |
| -------- | --------- |
| Trigger accepted (no wait) | 0 |
| Run passed (with wait) | 0 |
| Run failed | 5 (conflict) |
| Run errored | 1 (error) |
| 409 (run already active) | 5 |
| 404 (env not found) | 3 |
| Wait timed out | 6 |

## Visibility

Environments are **private** by default -- reachable only from within the
network gate (VPN). `drift env share` makes an environment publicly
reachable; `drift env unshare` reverts it.

Visibility is a property of the environment, not of the caller, so it applies
to everyone at once.
