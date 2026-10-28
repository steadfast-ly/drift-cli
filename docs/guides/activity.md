# Audit log and KPIs

**Requires:** the `preview` role or above. **KPIs** also requires the KPI
module enabled by your admin.

These two pages answer different questions. **Audit Log** records who changed
what. **KPIs** tracks engineering trends reported by CI.

## Audit log

Open **Audit Log** in the sidebar, or `https://drift.example.com/audit-log`.
The page is described as "All actions performed across the system". Users
below `preview` cannot open it.

### The table

Rows are newest first. Each row has:

| Column | Contents |
| ------ | -------- |
| **Time** | Relative time, with the exact timestamp on hover. |
| **Actor** | The email address that made the change, `system` for a system-generated row, or an address with a qualifier (below). |
| **Action** | The action name. |
| **Details** | Up to three detail fields, `key: value`. |
| **Environment** | A link to the environment, when the action belongs to one. |

Click a row to expand its full details as JSON. Click an action name to filter
the table by it.

The page loads 20 rows at a time. The footer reads
`Showing 20 entries (more available)` and **Load more** fetches the next page.
An empty result reads "No audit log entries".

### Actors

`system` means the row names nobody: an ordinary lifecycle write. When there
is an address but no Drift user row behind it, a smaller italic line explains
which kind of principal it was:

- `drift` -- an automated Drift principal, such as the one that closes
  branches.
- `no user row for this address` -- the address never bound to a user, or the
  person was deleted. The address itself is still shown.

### Filters

The filter bar has:

- **Action** -- an exact action, or **All actions**. The dropdown lists the
  environment, build and promotion actions, including **Environment
  Requested**, **Environment TTL Extended**, **Environment Deploy Failed**,
  **Build Dispatched**, **Promotion Created**, **Promotion Dispatched** and
  **Promotion Already Deployed**.
- **Actor** -- a search box reading **Filter by actor...**. Type to get
  suggestions under **Search actors...**, or search for the address you typed.
  The match is a substring of the address.
- **From** and **To** -- calendar pickers bounding the timestamp. **To**
  includes the whole day.
- **Clear filters** -- appears once any filter is active.

Filters and sort order are written to the page URL, so a filtered view can be
bookmarked or shared. The CLI's `drift audit list` accepts the same filters;
see [drift audit list](../reference/commands/drift_audit_list.md).

The table can be sorted by **Time**, **Actor** or **Action**. Clicking the
active column reverses the direction.

### Actions you will see

The table renders the action value with dots and underscores as separators,
so `environment.ttl_extended` reads **Environment · Ttl extended**. Actions
group by prefix:

**Environments** -- `environment.requested`, `environment.building`,
`environment.build_failed`, `environment.build_retried`,
`environment.deploying`, `environment.deployed`, `environment.deploy_failed`,
`environment.deploying_slow`, `environment.deploying_timeout`,
`environment.canceled`, `environment.destroying`, `environment.destroyed`,
`environment.auto_destroy_triggered`, `environment.ttl_extended`,
`environment.branch_swapped`, `environment.branch_flipped_to_default`,
`environment.service_added`, `environment.service_removed`,
`environment.visibility_changed`, `environment.sleeping`,
`environment.waking`, `environment.redeploy`, `environment.redeploy_failed`,
`environment.rebuild_triggered`, `environment.relaunched`,
`environment.e2e_triggered`, `environment.e2e_completed`. The log also holds
rows Drift records about its own background steps (sleep/wake phases,
retries, clean-up); they need no action from you.

**Builds** -- `build.dispatched`, `build.recovery_dispatched`.

**Promotions** -- `promotion.created`, `promotion.dispatched`,
`promotion.canceled`, `promotion.already_deployed`,
`promotion.workflow_completed`, `promotion.workflow_failed`,
`promotion.completed_verified`, `promotion.production_healthy`,
`promotion.production_degraded`, `promotion.prd_tag_mismatch`,
`promotion.reaped`, plus rows for Drift's own health checks.

**Repositories** -- `repository.created`, `repository.updated`,
`repository.deactivated`, `repository.reactivated`, `repository.deleted`,
`repository.linked_to_group`, `repository.unlinked_from_group`.

**Application groups** -- `application_group.created`,
`application_group.updated`, `application_group.deleted`.

**Dependencies** -- `dependency.added`, `dependency.removed`.

**QA** -- `qa.approve`, `qa.reject`, `qa.comment`, `qa.send_to_qa`,
`qa.exempt`, `qa.unexempt`, `qa.link`, `qa.unlink`, `qa.carry_over`,
`qa.revoke`, `qa.tracker_revoke`.

**CLI credentials** -- `cli_credential.mint`, `cli_credential.elevate`,
`cli_credential.first_use`, `cli_credential.revoke`,
`cli_credential.admin_list`, `cli_credential.admin_revoke_all`.

**Accounts and sign-in** -- `auth.admitted`, `auth.sign_in_refused`,
`user.role_changed`, `user.banned`, `user.unbanned`.

The Action dropdown does not list every one of these. To filter by an action
that is not in the dropdown, click that action in a row.

## KPIs

**KPIs** appears in the sidebar only when the install enables the module. The
page is available to `preview` and above and is not found when the module is
off. The header reads "Weekly engineering KPIs per component — unit/API tests,
coverage, CI duration, skips and failures."

The page has one section per configured component. Each card corresponds to
one of six metrics:

| Card | Meaning | Direction |
| ---- | ------- | --------- |
| **Unit Tests** | Unit tests reported by CI. | More is better. |
| **API Tests** | API tests reported by CI. | More is better. |
| **Coverage** | Coverage as a percentage. | More is better. |
| **CI Duration** | Duration of the pipeline, shown as `Xm Ys`. | Less is better. |
| **Skipped** | Skipped tests. | Less is better. |
| **Failed** | Failed tests. | Less is better. |

A week has one point per metric. Most metrics take the week's last reported
run; **CI Duration** takes the median of the week's runs so a single slow run
does not move the line. Weeks are calendar weeks; `2025-11-03` is the Monday
that starts a week.

Each card shows the latest value and a delta against the mean of up to four
weeks before it, labelled **vs 4-wk avg**. A green delta is an improvement for
that metric; red is a regression. **no baseline** means there are no prior
weeks; **no change** means the delta is zero.

### Reading the chart

- **1M**, **3M**, **6M**, **1Y** and **All** choose the visible range. The
  default is **3M**.
- The shaded band is **backfilled history**. The dashed line marked **live CI
  start** is where live CI reporting begins.
- **CI ingested 3 days ago** shows the most recent live CI data. It turns red
  after seven days; **no live CI data yet** means no live run has reported.
- Click a point in the live-CI part of the chart to open
  **Individual CI runs recorded in this week, newest first.** Each run shows
  its date and branch, a short commit hash (linked when Drift can build a
  forge URL), the run id (or **no run**) and the value. The dialog reads
  **No CI runs found for this week.** when there are none.
- **Export CSV** downloads the visible range as
  `Component,Metric,Week Start,Value,Unit,Origin`, where `Origin` is `ci` or
  `backfill`.

Empty states are explicit: **No KPI data has been collected yet. Metrics
appear here once CI runs report them (or the historical backfill is
imported).** at page level, **No KPI data collected yet for this component.**
for a component, and **No data in selected range** in a card whose metric has
no point in the chosen timeframe.

### Admin-only panel

An admin also sees **Rejected keys (7d)** when the ingest has rejected any
component keys in the last seven days. It says: "Component keys rejected by
KPI ingest because they are not in the profile. Check the emitting workflow's
component value." The table lists **Component key**, **Total**, **First seen**
and **Last seen**.

A rejected key means a CI job reported a component name the install does not
configure; its numbers are not on the dashboard until the emitter or the
install configuration is corrected.
