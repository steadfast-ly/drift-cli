# Web UI

**Requires:** a Drift account on your organisation's install. What you can
see and do depends on your access role -- see [Roles](../concepts/roles.md).

The web UI and the CLI are two front ends to the same service. An environment
or promotion created in one appears in the other; there is no separate state.

## Signing in

Open your Drift URL (for example `https://drift.example.com`). The card reads
**Sign in to continue**; **Continue to sign in** sends you to your
organisation's single sign-on. Drift issues no passwords of its own -- your
identity provider decides who may sign in.

If your account is outside the install's sign-in policy, the card reads
**Your account isn't in an authorized group or domain.** Ask your admin to add
you to the right group.

Sessions eventually expire. An expired session shows the notice **Your
session has expired. Sign in again to continue.**, and the action you
attempted is not performed. Sign out from
the arrow icon beside your name at the bottom of the sidebar (tooltip **Sign
out**). On a narrow screen the sidebar is a drawer opened with **Navigation**.

## The sidebar

Every install shows the same entries, except **KPIs** and **QA**, which appear
only when your admin enables those modules. The entry is not hidden when your
role is too low -- the page itself refuses you.

| Entry | Page | Role |
| ----- | ---- | ---- |
| **Previews** | Environment home: stat cards, filters and the environment list | View: any signed-in user. **New Environment** and per-environment actions: `preview` |
| **Releases** | Release state per tier, promotion history | View: any signed-in user. Promote, hotfix, cancel: `release` |
| **Repositories** | Registered repositories, application groups and dependencies | View: any signed-in user. Create, edit, dependencies: `admin` |
| **Audit Log** | Every action taken across the install | `preview` and above |
| **CLI Credentials** | Mint and revoke CLI tokens | Your own tokens: any signed-in user, capped at your current role. **Elevate for production**: `release`. **Offboarding (admin)**: `admin` |
| **Stack Profile** | The estate description mounted on this install | `admin` |
| **KPIs** | Weekly engineering KPIs per component; listed only when enabled | `preview` and above |
| **QA** | Tickets ready for testing, linked PRs and verdicts; listed only when enabled | View: `preview` and above. Approve or reject: `qa` |

Below the navigation the sidebar shows your name, your email, a badge with
your role, the running Drift version (`v...`), and any documentation links
your admin configured.

The UI renders a single dark theme; there is no theme switch.

## Minting CLI credentials

Open **CLI Credentials** in the sidebar, or go straight to
`https://drift.example.com/credentials`.

The **New CLI credential** card takes three fields:

- **Label** -- a name you will recognise later, such as `laptop` or
  `ci-runner`.
- **Role** -- at or below your own role.
- **Expires in** -- **24 hours**, **7 days** (the default) or **30 days**. The
  hard maximum is 30 days.

Choose **Create credential**. The token is shown once under **Copy this token
now — it will not be shown again**; copy it immediately. Drift stores only a
hash, so a lost token cannot be recovered -- revoke it and mint another.

The **Your credentials** table lists what you have, with columns **Label**,
**Role**, **Scopes**, **Status**, **Created**, **Expires** and **Last used**.
Revocation takes effect on the next request.

Users with the `release` role also see **Elevate for production**, which mints
a separate 15-minute token scoped for production promotion; see
[Promotions](../guides/promotions.md). Admins see **Offboarding (admin)**,
which looks a person up by email and revokes all of their credentials. CLI
credentials are not tied to the person's identity-provider session, so
disabling their identity-provider account does not stop credentials they
already minted; **Offboarding (admin)** revokes all of them.

## Next steps

- [Create and manage preview environments](../guides/preview-envs.md)
- [Promote through the release tiers](../guides/promotions.md)
- [Work the QA queue](../guides/qa.md)
