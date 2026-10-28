# Administration

**Requires:** the `admin` role.

An admin holds every other role as well, so the admin account can do
everything a `release` or `qa` user can. This page covers the work that is
admin-only: the **Repositories** page, **Stack Profile**, and the offboarding
half of **CLI Credentials**. The **Audit Log** and **KPIs** pages are covered
in [Audit log and KPIs](activity.md).

## Roles

Roles are granted in your organisation's identity and access console, not in
Drift. Drift has no screen that edits a person's role.

- Each person holds exactly one role.
- `qa` and `release` are separate roles, not steps on one ladder. A `qa`
  grant replaces a `release` grant.
- Someone who needs both QA verdicts and release promotions must be `admin`.
- Drift refreshes a role from your access console; a change is visible in the
  web UI within about five minutes, or at the next sign-in. See
  [Roles](../concepts/roles.md) for the five roles and what each can do.

## CLI credentials

Open **CLI Credentials**, or go to `https://drift.example.com/credentials`.
The page header is **CLI credentials**, described as "Bearer tokens for the
drift CLI and /api/v1".

An admin sees the standard credential cards:

- **New CLI credential** -- mint a token at or below your own role, with a
  lifetime of 24 hours, 7 days or 30 days. The token is shown once.
- **Your credentials** -- your own tokens, with label, role, scopes, status,
  created, expiry and last-use times. Revocation takes effect on the next
  request.
- **Elevate for production** -- admins hold `release`, so this card is
  present. It mints a separate 15-minute credential carrying `promote:prd`;
  the label is the only field.

### Offboarding (admin)

The card **Offboarding (admin)** is what an admin sees beyond their own
credentials. CLI credentials work until they expire or are revoked; they are
not tied to the person's identity-provider session, so disabling their
identity-provider account does not stop credentials they already minted.
**Offboarding (admin)** revokes all of them.

1. Enter the person's **User email**.
2. Choose **Look up**. Their credentials appear with **Label**, **Role**,
   **Status**, **Expires** and **Last used**.
3. Choose **Revoke all** to revoke every live credential they hold.

Nothing is revoked until a lookup has succeeded for the address currently in
the box, so a mistyped address produces "No credentials for that user." rather
than a silent no-op. Outcomes read:

- `Nothing to revoke: person@example.com had no live credentials.`
- `Revoked 3 credential(s) for person@example.com.`

Both the lookup and the bulk revoke are recorded in the audit log as
`cli_credential.admin_list` and `cli_credential.admin_revoke_all`.

Admins cannot browse everyone's credentials. The only cross-person view is
this lookup, and it is by exact email.

## Repositories

**Repositories** lists every repository Drift can deploy and the dependencies
between them. Every signed-in user can read the page; only admins see **Add
Repository**, **New group**, **Edit**, **Deactivate**, **Reactivate**,
**Delete**, **Add Dependency** and the group actions.

The **Registered Repositories** table has columns **Name**, **Helm Chart
Key**, **ECR Repository**, **Default Branch** and **Status**. The name cell
shows the display name over the repository's `owner/name` full name. Badges
mark a repository as **Internal** or **Not in e2e**. Status is **Active** or
**Inactive**.

Repositories that share an application group collapse into one row; the row
lists the group and its members.

### Add a repository

**Add Repository** opens a dialog of the same name with three groups of
fields.

| Field | What it controls |
| ----- | ---------------- |
| **Repository Name** | The `owner/name` the build reads. The owner is editable so a repository can live under another organisation your forge credentials reach. |
| **Display Name** | The name users see in Drift. |
| **ECR Repository** | The image repository this service is published to. One active repository may use a given value. |
| **Helm Chart Key** | The chart this repository is deployed from. |
| **Workflow File** | The CI workflow Drift triggers for a build. Defaults to `deploy-staging.yml`. |
| **Default Branch** | The branch used when a build or environment does not name one. Defaults to `develop`. |
| **Staging URL**, **RC URL**, **Production URL** | The service's address in each tier. They appear as shortcuts on the environment's service details and on **Releases**. Each must be an `https://` URL. |
| **URL path suffix (e.g. /ops/)** | Appended to the tier host. Must start with `/`. |
| **Live version source** | **Cluster** reads the live version from the deployment; **Edge (build marker)** reads a marker served by the service over HTTP. |
| **Marker path** | The marker's path on the service. Must start with `/`; leave empty to use the install default. |
| **Internal only** | The service always uses the VPN-only domain, regardless of environment visibility. Adds the **Internal** badge. |
| **Included in e2e** | Unticked, the service still deploys but is left out of the services sent to the e2e suite. Adds the **Not in e2e** badge. |

Repository name, display name, ECR repository, chart key, workflow file and
default branch are required. The description is not on the create form; add
it later with **Edit**.

### Edit

**Edit** opens **Edit Repository**. It exposes every field except the
repository name, plus **Description** and an **Application group** select. **Save Changes** sends
only the fields you changed. A group change is applied after the field
updates; if the fields save but the group change fails, the dialog says so and
keeps your field changes.

Linking a repository to a group is refused when its **Workflow File** or
**Default Branch** differs from an existing member's, because every member of
a group must build from the same workflow and branch.

### Deactivate, reactivate, delete

**Deactivate** asks for confirmation and explains that the repository "will no
longer be available for new environments". Existing environments still holding
it count as active use, and deactivation is refused with:

```
Cannot deactivate repository: currently used in an active environment
```

An inactive repository shows **Reactivate** in place of **Deactivate**.

**Delete** is offered only for an inactive repository. **Delete permanently**
cannot be undone: "All associated builds and environment references will be
removed." Drift refuses to delete an active repository (`must be deactivated
first`) or one still used by an active environment.

## Application groups

Application groups live on **Repositories**, in the **Application Groups**
card below the repository table. They describe a monorepo that ships several
services from one repository.

Each group row shows its display name, full name, an **Atomic** or
**Independent** badge, the member count and a chip per member, with **Edit**
and **Delete**.

- **Atomic** -- every member deploys as one unit, and an environment that
  includes one member includes them all.
- **Independent** -- members can be selected separately.

**New group** opens **New Application Group**:

| Field | Meaning |
| ----- | ------- |
| **Full Name** | The stable identifier, such as `acme/platform`. It cannot be changed after creation. |
| **Display Name** | The label shown in Drift. |
| **Atomic** | "All members deploy as one unit when enabled." On by default. |

**Create Group** adds it. To attach a repository, use **Edit** on that
repository and pick the group under **Application group**; **None** detaches
it. The group's **Edit** dialog changes only display name and atomicity.
**Delete** is disabled while the group has members, with the tooltip "Detach
N members first".

## Dependencies

The **Dependency Graph** card, below the groups, shows edges as
`repository → depends on`. Selecting a repository for an environment also
pulls in everything it depends on, transitively.

**Add Dependency** appears when there are at least two repositories. It takes
a **Repository** and a **Depends On** repository. **Add** saves the edge;
**Remove** deletes it. A self-dependency or a cycle is refused with:

```
Adding this dependency would create a cycle
```

A repository with no edges is listed as "(no dependencies)"; when the whole
graph is empty the card reads "No dependencies defined."

## Stack Profile

**Stack Profile** in the sidebar, or `https://drift.example.com/profile`, is
admin-only and read-only. There are no form controls: Drift never edits this
document.

The **Document** card shows the **Path** Drift reads and a **Checksum** badge.
The badge says **Verified at boot** with the checksum, **No checksum offered**
when none was supplied, or **Not verified at boot (development)** outside a
production install.

Below it, one card per section shows the loaded configuration in a
field/value list. **Raw document (JSON)** expands the whole document. Changes
are made outside Drift and take effect on the next deployment; this page
confirms what the running Drift loaded.

Application groups are not part of this page. Manage them under
[Repositories](#application-groups).
