# Preview environments

**Requires:** the `preview` role for creating or changing environments, in the
CLI and in the web UI. Any signed-in user can view them.

Task-oriented walkthroughs for the full environment lifecycle. Each task lists
the CLI first, then its web UI equivalent where one exists.

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

**In the web UI:** on **Previews**, choose **New Environment**. The **Create
Environment** dialog takes the same values explicitly -- nothing is inferred:

- **Ticket ID (optional)** -- `ABC-123`-shaped; a valid one derives the slug,
  shown as **Slug:** below the field.
- **Slug** -- shown until a valid ticket ID is entered.
- **TTL (hours)** -- defaults to 24.
- **Visibility** -- **Internal (VPN)** or **Public**.
- **Repositories** -- tick each service and choose its branch.

**Export** writes the current draft to a JSON file and **Import** loads one
back, for repeatable setups. **Create Environment** submits; the button is
disabled until the form is valid.

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

Repeat `--repo` for each service. To attach a pull request number to a
specific service, use the `name:branch:pr` form:

```bash
drift env create \
  --slug multi-svc \
  --repo frontend:feature/x:42 \
  --repo backend:feature/x \
  --yes
```

**In the web UI:** tick every service under **Repositories** and choose a
branch for each. Services that others depend on are selected automatically
and marked **(auto-selected)**; application groups can be selected as one
unit. The same rule applies as in the CLI -- at least one repository must use
a non-default branch.

Repos without a `:pr` segment get their PR resolved by the server
(exactly-one-open-PR by head branch, else none).

### Database migrations

On Install profiles that co-publish database-migration images, the
environment's migration image is the profile's migration-image repository
(e.g. `db-migrations`), co-published under the selected database service's
image tag. `--migration-source` names the ECR repository of that **database
service** -- the service whose selected tag supplies the migration artifact
-- not the migration-image repository itself. Choose it explicitly:

```bash
drift env create \
  --slug my-feature \
  --repo my-service:feature/branch \
  --migration-source orders-api \
  --yes
```

**In the web UI:** when more than one eligible database service is selected,
the **Create Environment** dialog adds a **DB migrations source** select. It
lists only the services the profile considers eligible. When the install's
default source is among the selected services it is pre-selected and the
button stays enabled; if the default is not among them, the button is disabled
with **Choose a DB migrations source above to continue.** until you pick one.
The server applies the same defaults and rejections as it does for the CLI.

The flag maps directly to the optional `migrationSourceEcrRepository` request
field and is shown in the confirmation plan. The **server** is the only
authority on what is eligible:

- The value must name a database-migration service from the Install's profile
  **and** be a service included in the environment.
- A value that is not an eligible, uniquely-present source is rejected before
  the environment is created.
- A profile without a database-migration block refuses any explicit source as
  unsupported.

When the flag is omitted the server applies its own rules — the profile's
configured default when uniquely included, otherwise the sole eligible
included source, an actionable error when several eligible sources remain, or
a missing-source error when none do. The CLI does **not** infer a source from
the working directory and never prompts for one: omission means "the server
decides". Passing an explicitly empty `--migration-source` (including
whitespace-only) is a usage error rather than omission, so an empty script
variable can never silently select the default.

Explicit selection requires a server that advertises the
`environments.migration-source` capability, which the server exposes only for
Installs whose profile has a database-migration block. Against a server
without it, the create is refused **before any create write**, with a
feature-unsupported error naming the context and the server version and a
hint to upgrade the server or use a context that supports the feature — an
older server must never silently drop the field and deploy its own default.
Omitting the flag needs no special capability, so it keeps working against
every server that supports plain `env create`.

Migration-divergence warnings are the server's, not the CLI's. They compare
selected commits only among eligible, included database services — a UI
service, even in the same source repository at a different commit, never
produces one.

### Useful flags

| Flag | Purpose |
| ---- | ------- |
| `--slug` | Environment slug |
| `--repo name:branch[:pr]` | Repository, branch and optional PR number (repeatable) |
| `--ticket PROJ-1234` | Issue key |
| `--ttl 72` | Lifetime in hours (default 48, max 120) |
| `--public` | Make the environment reachable without the VPN |
| `--migration-source <repo>` | ECR repository of the database service whose selected tag supplies the migration image (server profile default when omitted) |
| `--pr` | Pull request number (single-repo plan only; use `name:branch:pr` for multi-service) |
| `--pr-title`, `--pr-url` | Pull request title and URL (companions to `--pr`) |
| `--no-infer` | Ignore working-directory inference, use only flags |
| `--yes` | Skip the confirmation prompt |

## Wait semantics

Commands that **start work** block by default. Commands that **end work**
return immediately. `--wait` and `--no-wait` override.

**In the web UI:** there are no wait flags. The environment list shows
**Auto-updating...** and the detail page shows **Auto-updating** while work is
in progress, so the status changes without a reload. An action that starts
work returns to the page immediately.

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
| `e2e` | returns | run status | 125m |

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

**In the web UI:** choose **Extend TTL** on the environment card or its detail
page. The dialog shows **Current expiry**, an **Extend by** choice of **6h**,
**12h**, **24h** or **48h**, and the resulting **New expiry**. **Extend**
applies it.

## Sleep and wake

```bash
drift env sleep my-env
drift env wake my-env
```

Sleeping freezes the TTL clock. Waking credits back the slept time.

**In the web UI:** **Sleep** asks for confirmation in a **Sleep Environment**
dialog -- **Confirm Sleep** scales all replicas to zero. **Wake** runs
directly, with no dialog.

## Swap branch

```bash
drift env swap-branch my-env backend:hotfix/urgent
```

Only the **first colon** separates the repository from the branch, so branch
names containing colons or slashes pass through intact.

**In the web UI:** open the environment, stay on the **Services** tab and
choose the pencil beside a service's branch (labelled **Swap branch for** that
service). Pick the new branch and choose **Swap**. The server re-resolves the
pull request exactly as it does for the CLI.

## Add and remove services

```bash
drift env add-service my-env another-repo:main
drift env remove-service my-env another-repo --yes
```

Adding a service requires its dependencies to already be present in the
environment. Removing a service is destructive and confirms on a terminal.

**In the web UI:** on the environment's **Services** tab, **Add Service**
opens a dialog with **Repository** and **Branch**; **Add** attaches it.
Removal is the trash icon on the service row: the **Remove Service** dialog
warns that in-flight builds are cancelled, and **Remove** confirms. A service
that another service in the environment depends on cannot be removed -- its
trash icon does nothing when clicked, and its tooltip gives the reason.

Neither `swap-branch` nor `add-service` accepts a `--pr` flag. When the
branch changes, the server re-resolves the pull request the same way it
does at creation: exactly one open PR by head branch, otherwise none.

## Retry a failed build

```bash
drift env retry-build my-env
# If the environment has multiple services, name the repository:
drift env retry-build my-env backend
```

**In the web UI:** a service row in the **Services** tab shows a retry control
(tooltip **Retry build**) when retry is allowed. It uses the same rule as the
CLI: retry is offered when there is no build yet on the row's current branch,
or when that build failed or was canceled.

## Share and unshare

```bash
drift env share my-env      # make publicly reachable
drift env unshare my-env    # revert to private
```

**In the web UI:** on the environment's detail page, while the environment is
in a state that allows it, the **Public** or **Internal** badge beside its
name is a button. Its dialog says
**Change to Public** or **Change to Internal** and explains the domain
change; **Switch to Public** or **Switch to Internal** applies it.

## Inspect

```bash
drift env list
drift env list --status running,sleeping
drift env list --mine
drift env list --owner alice@example.com
drift env list --all
drift env get my-env
drift env get my-env -o json
drift env get my-env --json slug,status,expires
```

**In the web UI:** **Previews** is the same list. Its stat cards count **Total
Environments**, **Running**, **In Progress**, **Failed** and **Sleeping**;
**Show historical** includes destroyed and canceled environments, and
**Filter by creator...** works like `--owner`. Choose a slug to open the
detail page, whose tabs are **Services**, **Build History**, **Access** and
**Activity**, plus **E2E** where the install enables end-to-end testing.

`--mine` lists only the environments you created; `--owner <email>` filters
by creator (an exact match). `--mine` and `--owner` are mutually exclusive.
Both combine with `--status`, `--limit` and
`--offset`, and the resulting Owner column shows `-` for environments created
before the server began recording a creator.

`--all` fetches every page of results and prints one combined listing; it
combines with `--status`, `--mine` and `--owner`, but not with `--limit`.

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

**In the web UI:** the environment's **Access** tab shows the connection
details for the database and the internal infrastructure services, where the
install publishes them.

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

# Run the test code from a specific branch of the e2e repository.
drift env e2e my-env --tests-branch feature/e2e-updates
```

**In the web UI:** the **Run E2E** button is on the environment's **E2E**
tab. The **Run E2E Tests** dialog pre-selects the configured default test
branch -- choosing it is the same as triggering an unselected run. **Trigger
run** dispatches and refreshes the page; the run appears on the **E2E** tab
under **E2E Test Runs**.

Without `--wait`, the command returns immediately (exit 0) after the server
accepts the trigger. With `--wait`, it follows the run until it completes:
exit 0 on pass, non-zero on failure. The wait reads the run resource
(`GET /environments/{id}/e2e/{runId}`) when the server advertises the
`environments.e2e-read` capability, and otherwise falls back to polling the
audit log for the run's completion entry — so an older server keeps working
unchanged.

`--tests-branch <branch>` selects which version of the **test code** the run
checks out; the workflow definition still runs from the profile's `e2e.ref`.
The server is the only authority on eligibility: a branch that does not
exist in the e2e repository is rejected with a validation error before any
dispatch (exit 2), and a branch equal to the profile ref (or no flag at all)
keeps the run byte-identical to a default one. Setting the flag requires a
server that advertises the `e2e-tests-branch` capability; against an older
server the trigger is refused before the POST (exit 1, feature-unsupported)
rather than silently running default tests. The `Tests Branch` column
shows the branch the server accepted and recorded at trigger time -- the
REQUESTED branch, not an attestation of what the run executed. Drift
dispatches that value to the org's adapter but cannot verify the adapter
honoured it; the adapter contract requires adapters to fail the run
rather than silently check out the default test code. The trigger
response echoes the recorded branch on a non-default run; with `--wait`,
the branch is surfaced from the run resource (or from the run's completed
audit entry on the fallback path).

## Destroy

```bash
drift env rm my-env --yes
```

**In the web UI:** choose **Destroy** on the environment card or detail page,
then **Confirm Destroy** in the **Destroy Environment** dialog. Destroyed and
canceled environments show **Relaunch**, which clones the environment's
configuration into a new environment and rebuilds it from current branch heads.

Destroy convergence is owned by a server-side cron with an escalation window
and routinely exceeds ten minutes, which is why `rm` returns immediately by
default. Pass `--wait` to follow it to `destroyed`.
