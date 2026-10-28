# End-to-end tests

The end-to-end suite checks a preview environment as a whole: real service
images, the environment's URLs and data, exercised by your organisation's test
code. drift triggers the suite through your CI provider, records the run, and
reports the result as the `drift/e2e` commit status on the tested commits. The
suite itself belongs to your organisation; drift owns the dispatch and the
result, not the tests.

**Requires:** the `preview` role, and an admin who has enabled e2e for your
deployment.

## Start a run from the environment page

1. Open the environment and select the **E2E** tab. The tab exists only when
   e2e is enabled.
2. Click **Run E2E**. The button is disabled unless the environment is
   `running`; wake or rebuild it first if it is not.
3. In the **Run E2E Tests** dialog, choose the **Tests branch**. The configured
   default test branch is pre-selected and marked `(default)`; use the search
   box to pick another branch.
4. Click **Trigger run**.

Triggering records the run immediately and posts the pending status. Only one
e2e run can be active per environment at a time; a second trigger is refused
with `An e2e run is already in progress for this environment` until the first
one finishes.

## Start a run from the CLI

```bash
# Fire and observe: prints the accepted run and returns.
drift env e2e checkout-fix

# Follow the run to its outcome.
drift env e2e checkout-fix --wait

# Give up waiting after an hour.
drift env e2e checkout-fix --wait --wait-timeout 60m

# Run the test code from a specific branch of the e2e repository.
drift env e2e checkout-fix --tests-branch feature/new-suite
```

| Flag | Effect |
| ---- | ------ |
| `--wait` | Follow the run until it finishes; exit 0 on pass, non-zero on failure. |
| `--no-wait` | Return as soon as the server accepts the trigger (the default). |
| `--wait-timeout <duration>` | Stop waiting after this long and exit 6; default `125m`. The server stops tracking a run after 120 minutes, so the default leaves a margin. |
| `--tests-branch <branch>` | Check out this branch of the e2e repository for the test code. Omit it to use the configured default. The branch must exist in the e2e repository; the server refuses the trigger otherwise. |

Without `--wait`, the command prints the environment slug and the new run id.
With `--wait`, it also prints the outcome and, on failure, the reason. The
`Tests Branch` column shows the branch that was requested for the run, not a
confirmation of what the provider checked out.

`--tests-branch` changes only the **test code**. The application images under
test still come from the environment's services, and the workflow definition
still comes from the configured e2e workflow. Because the server is the only
authority on whether the branch exists, an unknown branch is rejected before
anything is dispatched.

## What the run tests

A run is pinned to a snapshot of the environment's services taken at trigger
time. Each service is resolved to the latest completed build of the branch the
environment runs -- which can be newer than the build the environment currently
deploys -- and that image tag is what the suite is told to test. Services whose
environment branch is their repository's default branch are part of the same
snapshot at their default-branch build.

Your organisation's suite chooses how to use the snapshot: it can compose the
stack from those images on a runner, or exercise the live environment URLs.
Either way, drift hands it the image tags and the environment's URLs.

Your admin can exclude individual repositories from e2e. Excluded services are
left out of the snapshot passed to the suite; what the suite actually
exercises depends on your organisation's tests.

## Read the result

Open the environment's **E2E** tab. The **E2E Test Runs** card shows the latest
run and the run history, each with:

- a status badge -- **Dispatched**, **Running**, **Passed**, **Failed** or
  **Error**;
- how long ago it was triggered and, once finished, its duration;
- the tests branch when one was selected;
- **View run**, which opens the CI provider's run -- the logs and any artifacts
  live there, not in drift;
- an `attempt N` marker when the provider re-ran the same workflow.

The same outcome is posted to the pull request as
[`drift/e2e`](pr-statuses.md#drift-e2e). The status link opens the environment
page when your admin configured a drift base URL; otherwise a finished run
links to the provider's run.

| Outcome | Meaning |
| ------- | ------- |
| **Passed** | The suite ran and passed. |
| **Failed** | The suite ran and failed. If your admin has named the suite's test step and drift can see that the step never ran, the commit status says `e2e failed: tests did not run` -- a failure, but one to look for in the setup steps rather than the test report. |
| **Error** | The run concluded something other than success or failure (cancelled, skipped, timed out or errored), the dispatch failed, or the run exceeded drift's 120-minute tracking window. The reason is recorded on the run. |

Drift stops **tracking** a run after 120 minutes and records it as `error`; it
never cancels the provider's workflow, which may still be executing on the
runner. A new run can be triggered once the tracking slot is released.

## Rerun after a push

A push to a branch the environment runs creates a new commit. Commit statuses
do not travel with the branch, so the new head has no `drift/e2e` status until
a run executes against it. Once the environment runs the new commit -- rebuild
or redeploy it if needed -- trigger the suite again from the **E2E** tab or with
`drift env e2e`. The new run takes over the status from any older run covering
the same commit.

Re-running the workflow in your CI provider is different: drift treats it as
another attempt of the same run and replaces the stored verdict when the new
attempt completes. While the attempt is running, the previous verdict stands.
