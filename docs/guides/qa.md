# QA queue

The QA module turns drift into the QA team's work queue. Tickets enter the
queue when the issue tracker moves them to the ready-for-QA status; a **Test
Session** runs one Ticket's Linked PRs on a preview environment; a verdict
writes the result back to the tracker and posts the `drift/qa` commit status
on the covered commits.

`drift qa` drives all of it from the terminal. The module is off unless the
Install's Stack Profile enables it: a deployment without it refuses every
command here, and the CLI decides that from
`/.well-known/drift.json` (capabilities `qa` and `qa.approve`).

## Reading the queue

```bash
drift qa list
drift qa list --json section,key,status,prs
drift qa show DT-123
```

`list` prints one table with a `section` column: `ready`, `testing`,
`recheck` and `upcoming`. The server caps each section, so one it shortened
ends with a `+N more` row saying how many it held back; the machine formats
carry the same counts under `truncated` (`ready`, `testing`, `recheck`,
`upcoming`) and no pseudo-row. The carry-over list (pre-rollout Tickets an
approver still has to decide) is rendered after the table and travels as
`carry_over` in JSON; verdicts decided in the last 14 days travel as
`recently_decided`.

`show` takes a Ticket key and prints the Linked PRs, sessions, approvals and
exemptions. A sub-task's key resolves to no Ticket -- use the parent key.
Machine output carries the operational detail too: `rejections`,
`tracker_writes`, `failed_writes` and `partitions`, and each session carries
its event timeline under `events`. Each session also says `verified` -- the
generation it runs is verified -- and `saw_change`, the fact that it ever saw
a change. `saw_change` outlives the acknowledgement: acknowledging a redeploy
clears `unacknowledged_change`, not the fact that the revision moved.

```bash
drift qa show DT-123 -o json | jq '.sessions[] | {id, state, current_generation}'
drift qa show DT-123 -o json | jq '.sessions[].events[] | {kind, generation}'
```

A session's timeline records what moved under the operator: `redeploy` (with
the deployment before and after), `migration-source-changed` and `unverified`.
Everything after the last `acknowledged` entry is what nobody has looked at
yet, and the table prints those as a `CHANGES (unacknowledged)` section while
the session is open and the server flags it (`unacknowledged_change`). A
flagged open session with nothing after the last ack still raises the section,
as one `unacknowledged` row: the flag is the server's and the timeline can lag
it. An ended session's timeline is history: it never raises the section.

## Starting a session

```bash
drift qa start DT-123
drift qa start DT-123 --prs acme/widget#42,acme/api#7
drift qa start DT-123 --env my-env
```

The server reuses or creates an environment running every Linked PR's head.
`--prs` selects a partition explicitly, as `repo#n` references to the
Ticket's Linked PRs; omitted, the server takes the first partition with no
open session. `--env` reuses an existing environment instead.

A rebuild of a developer's environment can happen under them, so the command
confirms and takes `--yes`.

## Approving and rejecting

A verdict binds the **session revision** -- the deploy generation the verdict
was shown. Read it from `qa show` and pass it as `--revision`:

```bash
drift qa show DT-123 -o json | jq '.sessions[]'   # current_generation
drift qa approve <session> --revision 3 --retested --note "checked the flow"
drift qa reject  <session> --revision 3 --comment "build 42 crashes on save"
```

Say whether the revision was retested: `--retested`, or `--no-retest
<reason>` (`--retested=false` is not a statement either way and is refused).
Saying neither is refused too; when the session saw a change, the refusal
says that is why a reason is required.
If the environment redeployed between reading and deciding, the
server refuses with the new revision and the CLI prints it; re-check with
`qa show` rather than retrying the old number.

Attach evidence with `--file` (repeatable):

```bash
drift qa approve <session> --revision 3 --retested --file shot.png --file log.txt --yes
```

The CLI reads the files, checks the server's caps client-side (at most 10
files, 7 MiB of decoded bytes in total) and sends them base64-encoded. The
content type comes from the extension, else `application/octet-stream`.

`drift qa comment DT-123 --body "..."` writes a tracker comment without
changing the status; `--file` works there too.

## Exemptions, links and revocations

```bash
# No QA needed for one PR (survives pushes), or for a whole ticket.
drift qa exempt --pr acme/widget#42 --reason "docs only"
drift qa exempt --ticket DT-123 --reason "covered by the e2e suite"

drift qa unexempt <exemption-id>
drift qa revoke --approval <approval-id>
drift qa revoke --exemption <exemption-id>

drift qa link   DT-123 acme/widget#42
drift qa unlink DT-123 acme/widget#42
```

`repo#n` is resolved against the QA queue (`--pr`, `link`) or against the
Ticket's own Linked PRs (`start`, `unlink`). A pull request that is no longer
on any queued Ticket is not reachable through the QA reads; pass its UUID
instead, which the API accepts for `link`, `unlink`, `exempt --pr`.

A PR registered under several repositories (a monorepo's services are separate
Repositories) appears once per registration, and each registration is its own
link, exemption and partition on the server. `repo#n` names the forge PR, so
the command acts on every one of its rows: `start --prs` sends them all, and
`link`, `unlink` and `exempt --pr` repeat once per row. A UUID names exactly
one repository row, whether or not it is still on the queue: it is the precise
address for a single row, and the retry target for a row that failed. The
`unlink` and `exempt --pr` confirmations name the row count when there is more
than one.

Every row is attempted even when an earlier one fails, because the server
commits each row on its own. `-o json` reports every row under `rows`, each
with its `pull_request_id` and `outcome`:

- `ok` — the row changed; it also carries the row's `exemption_id` or link
  `result`, and its `gate_target` where the server reports one.
- `refused` — the server answered with an HTTP error, so nothing changed for
  that row; it carries the `exit_code` and the server's `message`/`hint`.
- `unknown` — a transport error, a timeout or no usable response, so the row
  may have changed; check it with `drift qa show <key>` before retrying.
- `not_attempted` — the run was interrupted (Ctrl-C) before this row was sent,
  so nothing changed for it. The row in flight at the interrupt stays `unknown`.

`partial` is true when at least one row is `ok` and at least one is not; it
says nothing about whether an `unknown` row committed. The table adds a
`FAILED ROWS` section with each row's hint, and the exit code is the first
failed row's. Retry the failed rows by UUID rather than re-running the `repo#n`
command, which would repeat the rows that already succeeded. A partial
exemption is revoked with the ids under `exemption_ids`.

## Session and ticket actions

```bash
drift qa ack  <session>   # acknowledge a redeploy, clearing the change banner
drift qa end  <session>   # close the session without a verdict (API: close; --yes)
drift qa send DT-123      # move the ticket back to the queue, drain the outbox
drift qa carryover DT-123 # carry pre-rollout open PRs over at their heads
drift qa health           # tracker, sync phases, outbox and evaluation health
```

`carryover` requires the `qa` role; `health` is for operators -- a verdict
command that fails is usually explained by a line there.

## Confirmation

Commands that change a verdict or a grant, remove a link, or close a session
without one, ask for confirmation on a terminal and refuse without `--yes`
when the session is not interactive:
`start`, `approve`, `reject`, `exempt`, `unexempt`, `revoke`, `unlink`,
`send`, `end` and `carryover`. `link`, `comment` and `ack` do not.

## Errors specific to QA

| Response | Exit | Meaning |
| -------- | ---- | ------- |
| 409 with `data.revision` | 5 | The session moved to a newer revision. The CLI prints the new number and the re-check hint; re-read with `qa show` before deciding. |
| 409 without `data.revision` | 5 | A state conflict (session not open, environment not running, revision not verified). Read `data.detail`; the same retry fails the same way. |
| 413 | 1 | The request body exceeded the mount's 10 MB limit. Reduce `--file` sizes; the CLI already enforces 10 files and 7 MiB in total. |
| 502 | 1 | The server could not confirm the approver's `qa` role with gatehouse. The message and detail are printed as the server wrote them; retrying immediately usually fails the same way. |
| 403 | 1 | The credential's role does not permit the action. `qa.approve` actions need the `qa` (or `admin`) role; re-authenticating will not help. |

See [Errors and exit codes](../reference/errors.md) for the full contract.
