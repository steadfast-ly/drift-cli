# Pull request statuses

drift reports back to GitHub on the pull request itself. Two commit statuses
track automated verification -- `drift/e2e` and `drift/qa` -- and, when your
admin enables it, one comment lists the URLs of a running preview environment.
Statuses attach to commits, so GitHub shows them on the pull request's latest
commit and on any commit they were posted to.

**Requires:** access to the pull request on GitHub. Rerunning e2e needs the
`preview` role; approving or rejecting QA needs `qa` or `admin`.

!!! note
    The context names below are the defaults. Your admin can change either
    context and can turn `drift/e2e` posting off entirely. `drift/qa` has a
    configurable context too, but it disappears only when the QA module is
    disabled.

Whether a status blocks merging is decided by your repository's branch
protection, not by drift. A `drift/e2e` or `drift/qa` check can be required or
advisory; drift always posts the same statuses either way.

## drift/e2e {#drift-e2e}

drift posts `drift/e2e` when someone runs the end-to-end suite against a
preview environment -- from the environment page's **Run E2E** button or with
`drift env e2e`. A pending status is posted as soon as the run is accepted, and
a terminal status when the run finishes. See [End-to-end tests](e2e.md) for
starting a run.

The status goes on the commit of every service in the environment that has a
recorded commit and whose branch is not its repository's default branch. If two
services share a repository and commit, they share one status. Services still
on their repository's default branch are tested but get no status, and no pull
request is required: a tested commit gets the status even if no PR has been
opened for it.

Every description is the base line followed by ` · env: <slug>`, plus
` · tests: <branch>` when the run checked out a non-default tests branch. For
example:

```text
e2e failed: tests did not run · env: checkout-fix · tests: feature/new-suite
```

GitHub caps a commit-status description at 140 characters. When the whole line
would be longer, drift shortens the base line first, then the tests branch, and
only then the environment slug, ending each shortened part with `…`. The
` · env: ` and ` · tests: ` markers are never dropped.

| Description | Meaning | What you do |
| ----------- | ------- | ----------- |
| `e2e run dispatched · env: <slug>` | The run was accepted and the suite is queued or running. | Wait; follow it on the environment's **E2E** tab. |
| `e2e passed · env: <slug>` | The suite ran against this commit and passed. | Nothing. |
| `e2e failed · env: <slug>` | The suite ran and failed. | Open **View run** on the environment's **E2E** tab and read the provider's logs, then fix and push or open a new run. |
| `e2e failed: tests did not run · env: <slug>` | The run failed before the suite's test step started -- for example the stack did not come up. The status is still a failure. | Open **View run** and inspect the earlier steps (setup, image pulls, stack startup), not the test report. |
| `e2e errored · env: <slug>` | The run concluded something other than success or failure (cancelled, skipped, timed out or errored), the dispatch failed, or the run exceeded drift's tracking window. The reason is recorded on the run, not in the status. | Check the run on the **E2E** tab, fix the cause, and trigger a new run. |

A new run for the same commit replaces the older run's status; the newest run
owns the check. A push creates a new commit, and the old status does not follow
it, so the new head shows no `drift/e2e` until another run completes on it.

## drift/qa {#drift-qa}

drift posts `drift/qa` on the head commit of pull requests covered by the QA
queue. One status is computed per commit, across every open pull request whose
head is that commit in the repository -- linked to a ticket or not. A failure
wins over any pending text, a pending text wins over success, and success needs
every one of those pull requests to be green.

The status is recomputed when pull request activity changes it (a push, a
review, opening or closing the PR) and whenever a QA decision or tracker change
affects the PR. A push makes a new head commit, which starts without an
approval; an old commit's green status is history.

Pending descriptions appear in this order of precedence -- the first matching
row is the one posted:

| Description | Meaning | What you do |
| ----------- | ------- | ----------- |
| `waiting on <ticket key>` | Another ticket's pull request at this commit is still going through QA. | Wait for that ticket, or remove the shared dependency. |
| `QA approved — recording in tracker (<ticket key>)` | QA approved, and drift is writing the approval to your tracker. | Wait. If it is stuck, open the QA page and use **Retry** on the failed tracker write. |
| `QA testing in progress (<name>) — pushes redeploy QA's env` | A QA session is open and the named person owns it. Pushing to a linked PR rebuilds that session's environment. | Wait for the verdict, or coordinate with the session owner before pushing. |
| `awaiting code review` | The pull request has not been approved by a reviewer yet. | Get a code review and approval. |
| `changed since QA approval` | A push after the approval moved the head; the approval does not cover the new code. | Get it re-approved in QA, or the approver can re-run the decision. |
| `ticket closed without QA` | The ticket was moved to Done without a drift QA approval. | Have QA approve it in drift (move the ticket back to the QA queue first if it still needs testing), or exempt the PR if QA is not required. Reopening the ticket in the tracker alone does not clear it. |
| `Awaiting QA: <ticket key> (<status name>)` | The ticket is queued for QA and waiting for a test session. | Wait for QA to start a session, or start one yourself if you have the role. |
| `No ticket — add a key or exempt` | The pull request has no linked ticket and no exemption. | Put a ticket key in the PR title or head branch, link a ticket, or exempt the PR. |

Terminal descriptions:

| Description | Meaning | What you do |
| ----------- | ------- | ----------- |
| `QA rejected: <first line of the rejection comment>` | QA rejected the code at this commit; the rest of the rejection comment is on the QA page. | Read the full comment on the QA page, fix, push, and send it back to QA. |
| `QA passed on branch builds` | Every covered pull request at this commit is green. | Nothing. |
| `QA exempt: <reason>` | The pull request's only coverage is an exemption, and this is its reason. | Nothing, unless the exemption no longer applies. |

Like `drift/e2e`, the QA description is clipped to GitHub's 140-character limit
with a trailing `…`.

## Environment comment

When a preview environment first reaches `running`, drift can post one comment
on each pull request linked to that environment. Your admin enables this
feature; without it, no comment is posted.

The comment is posted once per environment, at that first running transition,
and is not updated by later rebuilds. Its body is:

```text
Environment **<slug>** is running:

- **<service>**: https://<service>-<slug>.<domain>
- **<service>**: https://<service>-<slug>.<domain>
```

Each bullet is one service in the environment, labelled with its display name
and linked to its preview URL. Use it to open the running environment without
going back to the Drift UI; when the environment is rebuilt or woken again, the
URLs may change and the old comment will not follow them.
