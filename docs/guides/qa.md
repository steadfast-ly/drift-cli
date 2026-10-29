# QA

**Requires:** the `preview` role to view the queue and start testing; `qa` or
`admin` to approve, reject, unlink a pull request or carry a Ticket over.
drift server 0.32.0 or later with the QA module enabled by your admin — the
**/qa** page as described here requires drift 0.33.1 or later; the
`drift qa` commands need drift CLI 0.16.0.

The QA module turns drift into the QA team's work queue. Tickets enter the
queue when your tracker moves them to the ready-for-QA status. A **Test
Session** runs one Ticket's Linked PRs on a preview environment, and a verdict
writes the result back to the tracker and posts the `drift/qa` commit status on
the covered commits.

`drift/qa` is advisory unless your organisation makes it required in branch
protection. drift never blocks a merge by itself.

## The /qa page

Requires drift 0.33 or later: earlier versions showed one large card per
Ticket instead of the rows described here. 0.33.1 is the page described here —
0.33.0 has the same rows, but tighter spacing, a `pending` word on every chip
and no **Carry over** on a Ready row.

Open **QA** in the sidebar. The page is available only when your admin has
enabled the module; otherwise it is not found.

The page is one list of rows split into sections, spaced to read calmly rather
than to fit one screen. The section bar stays pinned under the page title and
links each section with its count; a section the server capped adds `+N more`
beside the count. An empty section keeps its heading and puts one muted phrase
beside it: "In testing — nobody right now", "Re-check — nothing to re-check",
"Ready for QA — nothing ready for QA", "Upcoming — nothing upcoming". Under
**Mine** the phrase reads "— none of yours", or "— none of yours among the
loaded tickets" when that section itself is capped, and neither the bar nor a
heading appends the cap's `+N more`, because the cap counts everyone's rows.

| Section | What is in it |
| ------- | ------------- |
| **In testing** | Tickets with an active Test Session (preparing or open). A Ticket stays here while any partition is still free. |
| **Re-check** | Tickets with an unrevoked approval whose pull request head has moved, where nothing covers the new head. |
| **Ready for QA** | Tickets in the ready-for-QA status whose open Linked PRs are all review-approved or exempt, and Tickets in the approved status with no current drift completion (for example after an approval was revoked), oldest first. |
| **Upcoming** | Tickets in the queue status still waiting on code review, and Tickets in the upcoming statuses. |
| **Decided** | Approvals and exemptions from the last 14 days, collapsed by default. The bar's **Decided** entry always opens the section, even when the address already points at it. |

The bar's **Mine** switch narrows every section to the Tickets you own a Test
Session on. While the server has capped any section, the bar notes that the
filter covers only the loaded Tickets (`Mine: among the 24 loaded tickets`).

Approvers see **Carry-over** above everything until the list is empty. It is
rows like the sections: the key and summary, then one chip per pull request
with the PR's title in the tooltip, and **Carry over** in the actions column,
which records a green on the Ticket's current heads. It lists only the Tickets
no rendered row shows: a Ticket that is also in **Ready for QA** is acted on
from that row, where an approver finds **Carry over** as its primary action, and
under **Mine** a candidate whose row the filter hid stays in the list. For
everyone else that row says **Tested in Jira: an approver can carry it over**.

If a sync phase or a Ticket evaluation is failing, a **QA sync needs attention**
strip appears above the sections; the strip names the failing phase and its
error.

The page header reads "Tickets ready for testing, their linked pull requests
and the QA verdicts posted on them."

## The Ticket row

Each Ticket is one row of two lines. The first line is a chevron and the Ticket
key (linked to the tracker, never split), then the summary, in the row's main
colour, truncated to one line with its whole text in a tooltip; clicking the
summary, like the chevron, opens the detail. The second line is the row's meta
line, in muted text, in two groups separated by a wider gap: the PR chips
first, then the warning marker and the badges, then the people and the time —
the session owners, the ages, the environment and the session count.

The actions sit in a fixed column at the right of both lines, so the primary
button and the `⋯` land at the same place on every row.

The meta line carries:

- **PR chips, at most three** — `acme/web#42`, or `web#42` when every PR on the
  page shares one organisation. A chip says only what needs attention: the
  `drift/qa` gate stays silent while it is pending (a screen reader still hears
  `<gate> pending`), passed is a green check, and `failure` or `error` keeps
  its red word; a gate drift has not reported reads `not reported` in muted
  text. An approved review shows nothing (a screen reader still hears `review
  approved`), an open PR awaiting review says `review pending`, one with changes
  asked says `changes requested`, and a closed PR shows `merged` and no review
  word. A red ring means merged before QA; an amber ring means a mixed
  approval, a partial exemption or a change since approval. A monorepo PR folds
  into one chip that reports the weakest of its registrations.
  The chips show the worst states first — failure and error, then changes
  requested, then merged before QA, then the ringed states, then an open PR
  awaiting review, then other pending gates and a gate not reported, then
  success. The remaining PRs fold into one `+N PRs` chip whose tooltip lists
  each of them with its state words and full title. That chip adds the worst of
  those states as a word (`+N PRs · review pending`, `+N PRs · changes
  requested`), except when the worst is a pending gate or a passing one, which
  it leaves silent. An open, non-exempt PR awaiting review ranks above the
  other pending states, so it takes one of the three chips first and `review
  pending` on the `+N` chip appears only when worse states hide it. The row's
  detail keeps every word — `pending`, and a closed PR's review state — so it
  is the place to read everything.
- **A warning marker** — `⚠ n` when the row has any warning: an unacknowledged
  change, a failed tracker write, a PR merged before QA, a mixed approval or a
  partial exemption. Hovering or focusing the marker lists every warning in
  full. It counts every PR and every session, not only the chips shown.
- **The tracker status** — only when it differs from the project's queue
  status, so a Ready for QA row does not repeat "Ready for QA"; it reads
  `Jira: <status>`, naming the tracker from your admin's profile, and shows the
  bare status for a tracker drift cannot name.
- **`Tested in Jira`** when someone approved in the tracker without a drift
  verdict: `Tested in Jira, not approved in drift` for an approver, whose
  tooltip names what clears it — "Carry over to clear the gate" on a row the
  Carry-over list holds, "Start testing from ⋯ to approve it in drift" when a
  partition is free, and "Approve it in drift to clear the gate" when every
  partition has a session — and `Tested in Jira: an approver can carry it over`
  for everyone else. No green is posted. Waiting on code review is the per-PR
  `review pending` chip; there is no row badge for it.
- **The session owners** — at most two: the local part of the email with an
  initials avatar, the full address in the tooltip, then `+N` for the rest (all
  their addresses in its tooltip). `N sessions` follows when there is more than
  one.
- **Times** — the oldest session's age (`8d ago`) and, while the Ticket sits in
  its project's queue status, how long it has been there (`in queue 9d`); a row
  in any other status shows no queue age.
- **The environment** — the session's environment link, or `preparing env`.
- **The session count** — on a Ticket with several partitions while one is
  still free: `needs 2 test sessions` before any has started, `1 of 2 sessions
  running` once some have.

Every tooltip on a row opens on keyboard focus as well as on hover.

### Row actions

The row's right-hand column holds one primary action and a `⋯` menu, at the
same position on every row. The first rule that applies wins:

| Primary | When |
| ------- | ---- |
| **Review** | The row has any warning, or the Ticket is In testing with more than one session, where the approver must pick one. |
| **Approve**, **Reject** | Exactly one open session, no warning, and the Ticket is In testing. Approvers only. |
| **Carry over** | An approver's Ready for QA row whose Ticket is on the Carry-over list, when the row has no warning. |
| **Start testing** | A partition has no session (see [Starting a test session](#starting-a-test-session)). |

Otherwise a row in In testing gets **Review**, which opens the detail; a row in
any other section shows no primary unless it has a warning, a free partition,
or is a Ready for QA row on the Carry-over list.

**Start testing** is filled on a Ready for QA or Re-check row and outlined in
**Upcoming**, where QA may start before code review; the behaviour is the same.
A Ready for QA row the tracker approved but drift has not has no **Start
testing** primary, because starting it would test what the tracker already
tested: an approver carrying it over gets **Carry over** instead, and otherwise
the row has no primary and **Start testing** stays in `⋯` for a deliberate
retest.

No row opens itself: a row with warnings starts collapsed too. Its primary is
**Review**, which opens the detail, and **Approve**, **Reject** and **Carry
over** on such a row exist only there — so an approver reads the warnings
before acting. **Close session** leaves a warning row's `⋯` for the same
reason; it stays in the detail's session block, and **Carry over** — which is
approval-equivalent — sits under the detail's warnings.

The `⋯` menu holds **Start testing** when it is not already the primary,
**Carry over** when it is not the primary and the row has no warning, **Close
session** (the session owner, `qa` or `admin`) on a row with a single session
and no warnings, **Comment**, **Send to QA**, **Exempt** and **Link PR**.
**Acknowledge** is never in the menu: it lives in the detail's session block,
beside the banner it acts on, and only while a change is unacknowledged.
Each dialog opens outside the menu: one Escape closes the dialog alone, and
focus returns to the `⋯`. A refused **Start testing** from the menu reports in
the row's own error line instead of vanishing with the menu.

**Send to QA** is offered only where the server accepts it: never from the
project's queue or approved status, and never for a Ticket in a Done-category
status.

## The row detail

A row with warnings opens its detail with the warnings as text — the same items
as the marker's tooltip, each one scrolling to and focusing the block it names —
then, for an approver whose Ticket is on the Carry-over list, **Carry over**
right under them. The detail then repeats the summary in full and carries:

- **Each Linked PR, one line**: its short label, the title without the `[KEY] `
  prefix (wrapped, never truncated), the gate word with its context
  (`drift/qa pending`) and its description when that says more than "Awaiting
  QA", the review and `merged` tokens, the aggregate badges — **Approved**,
  **Mixed approval**, **Changed since approval**, **Merged before QA**,
  **Carried over** — `Exempt n of m: <reason>` as text, and **Unlink** at the
  line's end for approvers.
- **Each session**: **Preparing** or **Session open**, the owner, the PRs the
  session runs as folded chips, the environment link, `revision <n>` (with
  `(started at <n>)` when the start revision differs), **Changes not
  acknowledged** while a change is outstanding, and `started <time> ago`. When
  none of the session's PR ids resolve on the row, it says `N unlisted PR(s)`
  instead of naming them. Each session carries its own change banners and its
  own **Approve**, **Reject** and **Close**, plus **Acknowledge** while a
  change is unacknowledged.
- **Partitions**, only when there are two or more, folded per pull request:
  `Needs 2 test sessions: ① acme/web#42, acme/api#7 — free · ② acme/web#9 —
  jane.doe testing`. A partition whose PRs are all unlisted shows `N unlisted
  PR(s)` in their place, and a session whose partition no longer exists gets a
  line of its own.
- **Tracker writes failed**, listing each write's attempts and last error, with
  **Retry**.
- **`Completion: none yet`**, `pending`, `done` or `done (stale)`.

A Ticket with no Linked PRs says "No linked PR yet — link one or test in the
tracker."

## Decided

**Decided** is one chronological list of the last 14 days with a type badge on
each row:

- An approval shows the Ticket key, **Approved**, the approver and its age,
  `revision <n>`, `retested` or `no retest: <reason>`, the covered PRs with
  short commit ids, and the note. Once withdrawn it names who revoked it, when
  and why; a live approval offers **Revoke**.
- A pull-request exemption shows `repo#n`, **PR exemption**, its reason and its
  grantor. Exemptions of one monorepo PR fold into a single row with a
  registration count, and a folded row takes its newest registration's place in
  the list. **Unexempt** on a folded row revokes only its active members and
  reports a partial result inline (`2 of 4 revoked; 2 failed: …`). A folded row
  keeps the audit trail: it names the newest `Revoked by <name> · <ago>`, and
  adds `n of m revoked` only while some registrations stay exempt — a fully
  revoked row says nothing more. The tooltip on that newest revocation lists
  every revoked member with its revoker and the absolute time.
- A ticket-scope exemption shows the key, **Ticket exemption** and its reason;
  it never folds.
- A bot's automatic exemptions collapse to one line per bot —
  `dependabot[bot] — 11 PRs auto-exempt (every commit by the bot)` — with the
  `auto:` audit prefix stripped from the name; its PRs are ordered by
  repository then PR number. Expand the line to see each PR.

The approver's and the grantor's timestamps each open the absolute time in a
tooltip on focus.

## How pull requests join a Ticket

drift reads the Ticket key from a pull request's **title** and **head branch**,
case-insensitively, but only for the projects your admin configured. A
sub-task's key resolves to its parent: the PR links to the parent Ticket, and
`drift qa show` on the sub-task key answers not found — use the parent key.
A pull request may belong to several Tickets; its links are sticky, so a
merged PR stays linked.

A webhook records a PR change within seconds. If a delivery is missed, the
reconcile pass catches up within about 10 minutes. A key that appears in the
title or branch after the PR opened is picked up the same way.

For a PR whose title and branch carry no key, use **Link PR**. From drift
0.32.1 or later, the dialog asks for the **Repository** and then lists the pull
requests drift knows for it — open ones first, closed and merged ones
labelled, ones already linked to this Ticket greyed out; type to filter by
number, title, author or branch. Earlier versions ask for the **Repository**
and a **Pull request number**. drift must already know the PR from a webhook
or the sync; a PR it has never seen answers not found. An approver can remove
a link with **Unlink** on the PR row.

```bash
drift qa link   PROJ-123 acme/app#42
drift qa unlink PROJ-123 acme/app#42
```

`repo#n` names the forge PR, so on a monorepo it acts on every repository
registration of that PR. Pass a row UUID to address exactly one registration.
When a reference covers several rows, every row is attempted even if one
fails, because the server commits each row on its own. `-o json` reports every
row under `rows`, each with its `pull_request_id` and `outcome`:

- `ok` — the row changed; it also carries the row's `exemption_id` or link
  `result`, and its `gate_target` where the server reports one.
- `refused` — the server answered with an HTTP error, so nothing changed for
  that row; it carries the `exit_code` and the server's `message`/`hint`.
- `unknown` — a transport error, a timeout or no usable response, so the row
  may have changed; check it before retrying, with `drift qa show <key>` or,
  for a PR exemption, the PR's `drift/qa` status (`QA exempt: …` means it
  landed).
- `not_attempted` — the run was interrupted (Ctrl-C) before this row was sent,
  so nothing changed for it. The row in flight at the interrupt stays
  `unknown`.

`partial` is true when at least one row is `ok` and at least one is not; it
says nothing about whether an `unknown` row committed. The table adds a
`FAILED ROWS` section with each row's hint, and the exit code is the first
failed row's. Retry the failed rows by UUID rather than re-running the
`repo#n` command, which would repeat the rows that already succeeded. A
partial exemption's ids are under `exemption_ids`.

## Starting a test session

Click **Start testing**. drift reuses an environment already running the
partition's branches, or creates one:

- A reused environment is woken if it is sleeping, and rebuilt if its commits
  are behind the PR heads. That can rebuild a developer's environment under
  them; the dialog says so.
- A new environment is named after the Ticket in lowercase: `<key>`, then
  `qa-<key>`, `qa-<key>-2`, and so on.
- When the Ticket has several partitions or existing environments, the
  **Start testing <key>** dialog lets you pick a **Partition** and optionally a
  **Reuse environment** ("Let drift choose" by default). Otherwise the button
  starts the first free partition directly.

With no open Linked PRs there is nothing to test, so the row has no **Start
testing** action and its detail says "No linked PR yet — link one or test in
the tracker." A Ticket whose partitions all already have a session has no
**Start testing** either; the menu drops it the same way.

```bash
drift qa start PROJ-123
drift qa start PROJ-123 --prs acme/app#42,acme/api#7
drift qa start PROJ-123 --env my-env
```

The command asks for confirmation because it may rebuild a developer's
environment; pass `--yes` in scripts.

### Preparing and open

The session's block in the detail shows **Preparing** until a verified
deployment runs every partition PR at its current head. That revision is the
**start revision**, and the row then shows **Session open**, the owner, the
environment link, `revision <n>` (with the start revision when it differs),
the PRs it runs and `started <time> ago`. A push during **Preparing** waits
for a deployment at the new head; a failed build can leave the session
preparing indefinitely, in which case the owner, `qa` or an admin can
**Close** it.

### Pushes during a session

Every new deployment appends a change to the session:

- **Redeployed** — with each service's branch and `before → after` commits, and
  a **compare** link to the forge.
- **Migration source changed** — the migrations job may have run.
- **Revision not verified** — the deployment has not been verified yet, and
  "Approve stays blocked until this revision, or a later one, is verified."

The session shows **Changes not acknowledged** while changes are outstanding.
**Acknowledge** clears that flag without approving anything. It does not clear
the retest requirement: a session that ever saw a change needs **I retested
the changes** or a no-retest reason on Approve.

`drift qa show PROJ-123` prints each session with its state, current
revision, `verified` and `saw_change`. The JSON form carries the event
timeline:

```bash
drift qa show PROJ-123 -o json | jq '.sessions[] | {id, state, current_generation}'
drift qa show PROJ-123 -o json | jq '.sessions[].events[] | {kind, generation}'
```

## Approving

**Approve** opens a dialog bound to the revision it shows: "Approving binds
revision <n> of <environment> to the Linked PRs it runs." It offers:

- **I retested the changes**.
- **No-retest reason**, marked "(required)" when the session saw a change.
- **Note (optional)**.
- **Files** — up to 10 files, 7 MiB in total.

The submit button reads **Approve revision <n>**. If the environment
redeployed while the dialog was open, the server refuses and the dialog
re-renders with the new revision and the changes since; review them and
submit again.

An approval covers the partition PRs whose deployed commit equals the PR head.
Other PRs in the partition are listed as uncovered — usually because their head
moved and has not deployed yet — and the tracker comment names what remains.
With several partitions, approve each session; the Ticket completes only when
every open Linked PR is approved, exempt or carried over.

What drift then does:

1. Uploads the evidence files and posts a tracker comment naming the approver,
   linking the environment, listing the covered commits, the retest state, the
   note and any attachments, plus any **Merged before QA** PRs.
2. Moves the Ticket to the approved status, once every Linked PR is covered.
3. Posts `drift/qa` success on the covered commits — only after those tracker
   writes land. Until then the status reads "QA approved — recording in
   tracker (<key>)".

```bash
drift qa show PROJ-123 -o json | jq '.sessions[]'   # read current_generation
drift qa approve <session> --revision 3 --retested --note "checked the flow"
drift qa approve <session> --revision 3 --no-retest "only docs changed" --yes
drift qa approve <session> --revision 3 --retested --file shot.png --file log.txt --yes
```

`--revision` is the revision shown by `qa show`. Say whether it was
retested: `--retested`, or `--no-retest <reason>`. Saying neither, or passing
`--retested=false`, is refused; when the session saw a change the refusal says
that is why a reason is required.

## Rejecting

**Reject** binds the same revision and requires a **Comment (required)**;
**Files** work as on Approve. The submit button reads **Reject revision
<n>**. The dialog warns: "Rejecting revision <n> moves the Ticket to the
rejected status and withdraws approvals on these commits."

A rejection:

- records the rejection on the covered commits and withdraws any approval of
  the same commits, including another Ticket's approval on a shared PR;
- posts the comment and files to the tracker and moves the Ticket to the
  rejected status;
- posts `drift/qa` failure: "QA rejected: <first line of the comment>".

A later approval of the same commits, or an exemption on the PR, retires the
rejection.

```bash
drift qa reject <session> --revision 3 --comment "build 42 crashes on save"
drift qa reject <session> --revision 3 --comment "..." --file crash.log --yes
```

## Exemptions

**Exempt** records "no QA needed". The dialog has a **Scope** choice:

- **Pull request** — pick one open Linked PR. The exemption survives pushes
  until it is revoked.
- **Ticket** — covers the PRs linked at that moment, and the completion comment
  reads "QA not required: <reason>".

A reason is required. A PR whose author is on your admin's exempt-authors list
is exempt automatically while every commit's author is that bot; a human commit
drops it back to pending.

**Unexempt** in **Decided** revokes an exemption; on a folded monorepo row it
revokes the active registrations one by one and reports any that failed.
Revoking a Ticket exemption also revokes every approval of that Ticket and
moves it from the approved status back to the queue, so it re-enters QA.

```bash
drift qa exempt --pr acme/app#42 --reason "docs only"
drift qa exempt --ticket PROJ-123 --reason "covered by the e2e suite"
drift qa unexempt <exemption-id>
```

## Revocation and tracker moves

**Revoke** on an approval in **Decided**, or `drift qa revoke
--approval <id>`, withdraws a drift approval. The gate goes pending again; a
Ticket still in the approved tracker status shows in **Ready for QA** with the
**Tested in Jira** badge, and you can approve it again or move it back to the
queue in the tracker. A push is different: the approval stays but no longer
covers the head, so the gate reads "changed since QA approval" and the Ticket
shows under **Re-check**.

Moving a Ticket in the tracker also matters:

- Moving it out of the approved status, or out of a Done-category status, to a
  status outside both revokes the approval.
- Approved → Done keeps the green. Queue ↔ upcoming moves and moves into the
  rejected status revoke nothing.
- drift's own moves never revoke. **Send to QA** from the approved status would
  otherwise leave a green on a Ticket back in the queue, so it is refused while
  the last-synced view still shows the Ticket in the queue, approved or Done;
  retry after the next sync.

A tracker-side approval without a drift verdict is flagged, not honoured: the
row says **Tested in Jira, not approved in drift** to an approver and **Tested
in Jira: an approver can carry it over** to everyone else, no green is posted,
and drift does not move the Ticket back. Approving in drift clears the flag;
the move is then a no-op.

If your approved status is itself a Done-category status, a manual move there
without a drift approval shows `drift/qa` pending "ticket closed without QA"
and never turns green. Approve in drift, or exempt the PR.

### Carry-over

**Carry-over** lists Tickets approved before the QA module existed and not yet
complete, and only those no rendered row shows — under **Mine** a candidate
whose row the filter hid stays listed: a Ticket that is also in **Ready for QA**
or **Upcoming** is acted on from that row. **Carry over** (or `drift qa
carryover <key>`) records a green on the Ticket's open Linked PRs at their
current heads. The Ticket leaves the list once it is done.

The list is rows, like the sections: the key and summary, one chip per pull
request — a monorepo PR's registrations fold into one — with the PR's title in
the same tooltip a row's chips use, and **Carry over** at the right. The rows
carry no gate or review state, so the chips are labels only. One line under the
heading says what the list is for: "Tested in the tracker before drift QA was
on. Carry over to turn their pull requests green."

## The drift/qa status

| State | Description | What to do |
| ----- | ----------- | ---------- |
| success | `QA passed on branch builds` | None. |
| success | `QA exempt: <reason>` | None. |
| failure | `QA rejected: <first line>` | Fix the problem and push; the new head needs a new session. |
| pending | `QA testing in progress (<name>) — pushes redeploy QA's env` | Wait for the session to end, or start one. |
| pending | `QA approved — recording in tracker (<key>)` | Wait; drift is writing the comment and status move. |
| pending | `awaiting code review` | Get the PR review-approved. |
| pending | `changed since QA approval` | The head moved; start a new session on the new revision. |
| pending | `Awaiting QA: <key> (<status name>)` | The Ticket is in the queue; test it. |
| pending | `waiting on <other ticket key>` | A shared PR also belongs to another Ticket that is not complete; finish that one too. |
| pending | `ticket closed without QA` | The Ticket is in a Done-category status without a drift approval; approve or exempt. |
| pending | `No ticket — add a key or exempt` | The PR carries no Ticket key and has no exemption; add the key or exempt the PR. |

A PR linked to several Tickets turns green only when every one of them is
approved or exempt. The full table, including how the status is posted and
reconciled, is in [PR statuses](pr-statuses.md#drift-qa).

## Permissions

| Action | Role |
| ------ | ---- |
| View the queue, start a session, comment, link a PR, Send to QA, exempt, unexempt, revoke | `preview` and above |
| Approve, reject, unlink, carry over | `qa` or `admin`, with the role confirmed at the moment of the action |
| Acknowledge or close a session | The session owner, `qa` or `admin` |

`qa` and `release` are separate roles and neither contains the other; `admin`
holds both. See [Roles](../concepts/roles.md) for the full model.

## CLI reference

The CLI covers the same queue. Every command refuses on a deployment without
the module.

```bash
drift qa list
drift qa list --json section,key,status,prs
drift qa show PROJ-123
```

`list` prints a `section` column: `ready`, `testing`, `recheck`, `upcoming`.
A capped section ends with a `+N more` row; the machine formats carry the
counts under `truncated`. The carry-over list and the last 14 days of verdicts
travel as `carry_over` and `recently_decided`.

`show` prints the Linked PRs, sessions, changes, approvals and exemptions.
Machine output also carries `rejections`, `tracker_writes`, `failed_writes` and
`partitions`; each session carries `events`, `verified` and `saw_change`.

Session and Ticket actions:

```bash
drift qa ack  <session>    # acknowledge a redeploy
drift qa end  <session>    # close without a verdict
drift qa send PROJ-123     # move a rejected/upcoming Ticket back to the queue
drift qa carryover PROJ-123
drift qa comment PROJ-123 --body "..." [--file shot.png]
drift qa health            # sync, outbox and tracker health
```

Commands that change a verdict or a grant, remove a link, or close a session
ask for confirmation on a terminal and refuse without `--yes` when the session
is not interactive: `start`, `approve`, `reject`, `exempt`, `unexempt`,
`revoke`, `unlink`, `send`, `end` and `carryover`. `link`, `comment` and `ack`
do not.

## Troubleshooting

**"Can't confirm your QA role right now"** — the server could not confirm your
`qa` role with the service that assigns roles (HTTP 502). The verdict was not
recorded, and retrying immediately usually fails the same way. Ask your admin
to check the access service.

**"revision N is now current; re-check with `drift qa show <key>` before
deciding"** — the environment redeployed between reading and deciding. The CLI
exits 5. Re-read the session and approve the revision you actually tested;
never retry the old number.

**"Revision N is not verified. Approve stays blocked until a verified revision
arrives."** — the current deployment has not been verified. Wait for the next
verified revision. The CLI refuses the same case with exit 5 and
"QA revision N is not verified yet".

**The CLI timed out** — the default per-request timeout is 30 seconds
(`--timeout`). A verdict request that timed out may still have been recorded.
Check `drift qa show <key>` before retrying; a retry of a recorded verdict
fails because the session is no longer open.

**A tracker write failed** — the row's **Tracker writes failed** block has
**Retry**. The `drift/qa` status stays pending until the write lands.

**The session never leaves Preparing** — no verified deployment has run every
partition PR at its head. Check the environment's build and deployment; the
owner, `qa` or an admin can **Close** the session.

**`this server does not support "qa"`** — the module is not enabled on that
deployment, so `/qa` is not found either. Ask your admin.

**413 on a verdict** — the request body was too large. The limits are 10 files
and 7 MiB in total; reduce `--file` sizes.

See [Errors and exit codes](../reference/errors.md) for the CLI's error
contract.
