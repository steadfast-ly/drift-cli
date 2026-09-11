# Preview environments

Task-oriented walkthroughs for the full environment lifecycle.

## Create

From inside a git checkout, `drift env create` infers everything it can:

| Field | Inferred from |
| ----- | ------------- |
| repository | the `origin` remote |
| branch | `HEAD` |
| slug | the branch name, lowercased and hyphenated |
| ticket | an issue key in the branch name, uppercased |
| PR number, title, URL | `gh pr list --head <branch>` |

Every field has a flag that overrides inference. Outside a git repository or
in a non-interactive session, nothing is inferred and `--slug` and `--repo`
are required.

```bash
# From a checkout (interactive) -- everything is inferred.
drift env create

# Explicit, suitable for scripts.
drift env create \
  --slug my-feature \
  --repo my-service:feature/branch \
  --ticket PROJ-1234 \
  --ttl 72 \
  --yes
```

### Multi-service environments

Repeat `--repo` for each service:

```bash
drift env create \
  --slug multi-svc \
  --repo frontend:feature/x \
  --repo backend:feature/x \
  --yes
```

### Useful flags

| Flag | Purpose |
| ---- | ------- |
| `--slug` | Environment slug |
| `--repo name:branch` | Repository and branch (repeatable) |
| `--ticket PROJ-1234` | Issue key |
| `--ttl 72` | Lifetime in hours (default 48, max 120) |
| `--public` | Make the environment reachable without the VPN |
| `--pr`, `--pr-title`, `--pr-url` | Pull request metadata |
| `--no-infer` | Ignore working-directory inference, use only flags |
| `--yes` | Skip the confirmation prompt |

## Wait semantics

Commands that **start work** block by default. Commands that **end work**
return immediately. `--wait` and `--no-wait` override.

| Command | Default | Goal state | Timeout |
| ------- | ------- | ---------- | ------- |
| `create` | blocks | `running` | 30m |
| `relaunch` | blocks | `running` | 30m |
| `retry-build` | blocks | `running` | 30m |
| `add-service` | blocks | `running` | 30m |
| `swap-branch` | blocks | `running` | 30m |
| `wake` | blocks | `running` | 10m |
| `rm` | returns | `destroyed` | 20m |
| `sleep` | returns | `sleeping` | 10m |
| `cancel` | returns | `canceled` | 5m |
| `e2e` | returns | audit entry | 125m |

`drift env wait <slug-or-id> --for <state>` follows any of them afterwards:

```bash
drift env rm my-env --wait
# or:
drift env rm my-env
drift env wait my-env --for destroyed --timeout 20m
```

Timing out is exit 6 and rolls nothing back.

## Extend

```bash
drift env extend my-env --hours 24
```

Extensions are **additive**: extending by 24 hours twice adds 48 hours total.
The cumulative lifetime cap is **120 hours** (creation + all extensions).

## Sleep and wake

```bash
drift env sleep my-env
drift env wake my-env
```

Sleeping freezes the TTL clock. Waking credits back the slept time.

## Swap branch

```bash
drift env swap-branch my-env backend:hotfix/urgent
```

Only the **first colon** separates the repository from the branch, so branch
names containing colons or slashes pass through intact.

## Add and remove services

```bash
drift env add-service my-env another-repo:main
drift env remove-service my-env another-repo --yes
```

Adding a service requires its dependencies to already be present in the
environment. Removing a service is destructive and confirms on a terminal.

## Retry a failed build

```bash
drift env retry-build my-env
# If the environment has multiple services, name the repository:
drift env retry-build my-env backend
```

## Share and unshare

```bash
drift env share my-env      # make publicly reachable
drift env unshare my-env    # revert to private
```

## Inspect

```bash
drift env list
drift env list --status running,sleeping
drift env get my-env
drift env get my-env -o json
drift env get my-env --json slug,status,expires
```

## Database access

Open a tunnel to the environment's database and launch an interactive
client:

```bash
drift env db my-env
drift env db my-env --user dbadmin
```

`--user` sets the database username; without it, the client uses its own
default (typically your OS login name).

This opens a chisel tunnel, then exec's `psql` (Postgres) or
`mysql`/`mariadb` (MySQL) against the forwarded local port. The password
is left to the client's own prompt. When the client exits, the tunnel
closes and the client's exit code is propagated.

To open the tunnel without launching a client (e.g. for a GUI tool):

```bash
drift env tunnel my-env
# Tunnel ready on 127.0.0.1:33060
#   psql -h 127.0.0.1 -p 33060 preview_db
```

The tunnel blocks until Ctrl-C. `--port` overrides the local bind port.

Both commands require drift server >= 0.15.0.

## End-to-end testing

Trigger an e2e test run against an environment:

```bash
# Fire and observe -- prints the run id and returns.
drift env e2e my-env

# Wait for the result.
drift env e2e my-env --wait
drift env e2e my-env --wait --wait-timeout 60m
```

Without `--wait`, the command returns immediately (exit 0) after the server
accepts the trigger. With `--wait`, it polls the audit log until the run
completes: exit 0 on pass, non-zero on failure.

## Destroy

```bash
drift env rm my-env --yes
```

Destroy convergence is owned by a server-side cron with an escalation window
and routinely exceeds ten minutes, which is why `rm` returns immediately by
default. Pass `--wait` to follow it to `destroyed`.
