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
`recheck` and `upcoming`. The carry-over list (pre-rollout Tickets an
approver still has to decide) is rendered after the table and travels as
`carry_over` in JSON; verdicts decided in the last 14 days travel as
`recently_decided`.

`show` takes a Ticket key and prints the Linked PRs, sessions, approvals and
exemptions. A sub-task's key resolves to no Ticket -- use the parent key.
Machine output carries the operational detail too: `rejections`,
`tracker_writes`, `failed_writes` and `partitions`.

```bash
drift qa show DT-123 -o json | jq '.sessions[] | {id, state, current_generation}'
```

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
<reason>`. If the environment redeployed between reading and deciding, the
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

## Session and ticket actions

```bash
drift qa ack  <session>   # acknowledge a redeploy, clearing the change banner
drift qa end  <session>   # close the session without a verdict (API: close)
drift qa send DT-123      # move the ticket back to the queue, drain the outbox
drift qa carryover DT-123 # carry pre-rollout open PRs over at their heads
drift qa health           # tracker, sync phases, outbox and evaluation health
```

`carryover` requires the `qa` role; `health` is for operators -- a verdict
command that fails is usually explained by a line there.

## Confirmation

Commands that change a verdict or a grant, and removing a link, ask for
confirmation on a terminal and refuse without `--yes` when the session is not
interactive:
`start`, `approve`, `reject`, `exempt`, `unexempt`, `revoke`, `unlink`,
`send` and `carryover`. `link`, `comment`, `ack` and `end` do not.

## Errors specific to QA

| Response | Exit | Meaning |
| -------- | ---- | ------- |
| 409 with `data.revision` | 5 | The session moved to a newer revision. The CLI prints the new number and the re-check hint; re-read with `qa show` before deciding. |
| 409 without `data.revision` | 5 | A state conflict (session not open, environment not running, revision not verified). Read `data.detail`; the same retry fails the same way. |
| 413 | 1 | The request body exceeded the mount's 10 MB limit. Reduce `--file` sizes; the CLI already enforces 10 files and 7 MiB in total. |
| 502 | 1 | The server could not confirm the approver's `qa` role with gatehouse. The message and detail are printed as the server wrote them; retrying immediately usually fails the same way. |
| 403 | 1 | The credential's role does not permit the action. `qa.approve` actions need the `qa` (or `admin`) role; re-authenticating will not help. |

See [Errors and exit codes](../reference/errors.md) for the full contract.
