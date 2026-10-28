# Roles

**Requires:** a signed-in Drift account.

Drift has five access roles: `read-only`, `preview`, `release`, `qa` and
`admin`. They are not one ladder. `release` and `qa` are separate branches,
and only `admin` holds both.

```
read-only < preview < release < admin
read-only < preview < qa      < admin
```

- **`read-only`** -- look at what is deployed. No changes.
- **`preview`** -- everything `read-only` can do, plus create and manage
  preview environments, run end-to-end tests, and work the QA queue apart from
  its verdicts.
- **`release`** -- everything `preview` can do, plus promote images and cancel
  a promotion.
- **`qa`** -- everything `preview` can do, plus approve, reject, remove a link
  and carry over a Ticket.
- **`admin`** -- everything, including repository and application-group
  management, the Stack Profile page, and CLI credentials belonging to other
  people.

`qa` and `release` are incomparable: QA verdicts belong to QA, promotions to
release, and neither implies the other. Someone who needs both is `admin`.

## What each role can do

| Action | Minimum role | Where |
| ------ | ------------ | ----- |
| View preview environments, releases and repositories | `read-only` | Web: **Previews**, **Releases**, **Repositories**. CLI: `drift env list`, `drift release status`, `drift repo list` |
| View the audit log | `preview` | Web: **Audit Log**. CLI: `drift audit list` |
| View KPIs, when the module is enabled | `preview` | Web: **KPIs** |
| View the QA queue, when the module is enabled | `preview` | Web: **QA**. CLI: `drift qa list`, `drift qa show` |
| Create, extend, sleep, wake, destroy, relaunch, redeploy or retry a preview environment; swap its branch; add or remove a service; cancel its build; change its visibility | `preview` | Web: **Previews**. CLI: `drift env create`, `extend`, `sleep`, `wake`, `rm`, `relaunch`, `redeploy`, `retry-build`, `swap-branch`, `add-service`, `remove-service`, `cancel`, `share`, `unshare` |
| Trigger an end-to-end run | `preview` | Web: **Run E2E** on an environment. CLI: `drift env e2e` |
| Promote to `rc`; hotfix to `rc`; cancel an in-flight promotion | `release` | Web: **Releases**. CLI: `drift release promote rc`, `drift release promote hotfix`, `drift release cancel` |
| Promote to `prd`; `prd` hotfix | `release` | Web: **Releases**, **Promote to Production**. CLI: `drift release promote prd`, `drift release promote prd hotfix` -- the CLI call also needs an elevated credential scoped to `promote:prd` |
| **Start session**, **Comment**, **Send to QA**, **Exempt**, **Unexempt**, **Link PR**, **Revoke** | `preview` | Web: **QA**. CLI: `drift qa start`, `comment`, `send`, `exempt`, `unexempt`, `link`, `revoke` |
| **Acknowledge** a change, **Close** a session | `preview` | Web: **QA**. CLI: `drift qa ack`, `end` |
| **Approve**, **Reject**, **Unlink**, **Carry over** | `qa` | Web: **QA**. CLI: `drift qa approve`, `reject`, `unlink`, `carryover` |
| Add, edit or delete a repository | `admin` | Web: **Repositories** |
| Add, edit or delete an application group | `admin` | Web: **Repositories** |
| View the Stack Profile page | `admin` | Web: **Stack Profile** |
| Mint or revoke your own CLI credential | `read-only` | Web: **CLI Credentials** |
| Mint a 15-minute elevated credential for a production promotion | `release` | Web: **CLI Credentials** |
| List or revoke another person's CLI credentials | `admin` | Web: **CLI Credentials** |

A CLI credential can never be minted at a role above your own, and admin-only
work stays in the web UI: no CLI command requires `admin`.

**Acknowledge** and **Close** are limited to the session's owner, `qa` and
`admin`; the table lists the role the action itself requires.

## Who assigns roles

Roles are assigned in your organisation's identity and access console, not in
Drift. Ask your Drift admin if you need a different role.

## How a role change reaches you

**In the web UI,** Drift caches your role in a signed session cookie for up to
five minutes. A change is visible within five minutes, or immediately at your
next sign-in.

QA verdicts are the exception. Approve, reject, remove a link and carry over
are checked against your current role at the moment you submit them, so a role
you have lost is refused immediately. If Drift cannot confirm your role at that
moment, the verdict is refused rather than accepted.

**From the CLI,** a credential acts with what both its mint role and your
current role allow (see the example below). QA verdicts check your current
role with your organisation's access console at once. For everything else
Drift uses the role it last learned, which it refreshes when you use the web
UI; a higher role never goes beyond what the credential was minted with. To
cut someone off at once, an admin revokes their credentials (see
[Administration](../guides/administration.md)).

When your role changes sideways or up -- for example `release` to `qa` -- sign
in to the web UI and mint a new credential at `<endpoint>/credentials`. Roles
are not one ladder, so an old `release` credential under a `qa` account would
act as `preview`: the only role both of them include.

If your account is deactivated, all your credentials stop working.
