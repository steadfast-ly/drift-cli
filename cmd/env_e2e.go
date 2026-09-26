package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/steadfast-ly/drift-cli/internal/api"
	"github.com/steadfast-ly/drift-cli/internal/client"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
	"github.com/steadfast-ly/drift-cli/internal/output"
	"github.com/steadfast-ly/drift-cli/internal/wait"
)

// e2eDefaultTimeout is sized to the server's tracking ceiling (120 min) plus
// margin. The server times out an e2e run after 120 minutes, so a CLI timeout
// shorter than that would declare failure while the run might still be
// converging; five minutes of margin keeps the CLI from racing the server.
const e2eDefaultTimeout = 125 * time.Minute

func newEnvE2eCommand(app *App) *cobra.Command {
	flags := &waitFlags{}
	// The e2e verb's wait policy differs from lifecycle mutations: the default is
	// NO wait (fire-and-observe), and the "goal" is not an environment status but
	// a terminal e2e run -- the run resource when the server advertises
	// environments.e2e-read, otherwise the run's completion audit entry -- so the
	// policy struct's Goal field is unused.
	policy := waitPolicy{Timeout: e2eDefaultTimeout, Blocks: false}
	testsBranch := ""

	cmd := &cobra.Command{
		Use:   "e2e <slug-or-id>",
		Short: "Trigger an end-to-end test run",
		Long: "Trigger an end-to-end test run against an environment.\n\n" +
			"Without --wait, prints the accepted run (slug + e2eRunId) and\n" +
			"returns immediately. With --wait, follows the run to completion --\n" +
			"reading the run resource when the server advertises the\n" +
			"environments.e2e-read capability, otherwise polling the audit log --\n" +
			"then exits 0 on pass and non-zero on failure.\n\n" +
			"--tests-branch selects the branch of the profile's e2e repository\n" +
			"whose test code this run checks out; the workflow definition still\n" +
			"runs from the profile ref. The server is the only authority on\n" +
			"eligibility: a branch that does not exist in the e2e repository is\n" +
			"rejected with a validation error before any dispatch, and the CLI\n" +
			"does not guess or validate the value. Setting the flag requires a\n" +
			"server that advertises the e2e-tests-branch capability; an older\n" +
			"server that would silently run default tests is refused before the\n" +
			"trigger. Omitting the flag keeps the exact pre-feature request.\n\n" +
			"The server's tracking ceiling is 120 minutes; the default\n" +
			"--wait-timeout is 125 minutes to stay clear of it.\n\n" +
			cliexit.Help,
		Args: exactArgs(1, "the environment slug or id"),
		RunE: func(c *cobra.Command, args []string) error {
			// An explicitly empty (or whitespace-only) --tests-branch is a
			// usage error, NOT omission. `Changed` is what distinguishes the
			// operator saying "" from not saying anything at all: an empty CI
			// variable must not silently select the server's default, which is
			// exactly the explicit-choice-vs-omission boundary. Checked here,
			// before Connect and any trigger write. True omission still
			// delegates to the server default unchanged.
			if c.Flags().Changed("tests-branch") && strings.TrimSpace(testsBranch) == "" {
				return usageErrorf(
					"--tests-branch must not be empty when given; omit the flag to use the server's profile default")
			}
			return runEnvE2e(c.Context(), app, args[0], policy, flags, testsBranch)
		},
	}
	// Manual flag registration: the e2e verb's wait is not "wait for a state"
	// but "wait for the run to finish", so the generic description from
	// waitFlags.register does not apply.
	flags.cmd = cmd
	cmd.Flags().BoolVar(&flags.wait, "wait", false, "wait for the e2e run to complete")
	cmd.Flags().BoolVar(&flags.noWait, "no-wait", true, "return as soon as the server accepts the request")
	cmd.Flags().DurationVar(&flags.timeout, "wait-timeout", e2eDefaultTimeout, "how long to wait before giving up (exit 6)")
	cmd.Flags().StringVar(&testsBranch, "tests-branch", "",
		"branch of the profile's e2e repository whose test code this run checks out (profile ref when omitted)")
	return cmd
}

// e2eColumns is the output shape for the e2e verb.
func e2eColumns() []output.Column {
	return []output.Column{
		{Name: "slug", Header: "Slug"},
		{Name: "e2eRunId", Header: "Run"},
		{Name: "testsBranch", Header: "Tests Branch"},
		{Name: "outcome", Header: "Outcome"},
		{Name: "reason", Header: "Reason", Wide: true},
		{Name: "environmentId", Header: "Id", Wide: true},
	}
}

func runEnvE2e(ctx context.Context, app *App, ref string, policy waitPolicy, flags *waitFlags, testsBranch string) error {
	if err := flags.validate(); err != nil {
		return err
	}

	cols := e2eColumns()

	// An explicit tests branch is refused against a server that cannot honour
	// it. The server advertises `e2e-tests-branch` only when its profile's e2e
	// block is enabled; an older server that drops the unknown request field
	// and silently runs default tests would be a wrong-answer bug, so we gate
	// BEFORE the trigger. Omission needs no capability and keeps the exact
	// pre-feature request.
	features := []string{FeatureEnvironmentsWrite}
	if testsBranch != "" {
		features = append(features, FeatureE2eTestsBranch)
	}
	sess, err := app.Connect(ctx, features...)
	if err != nil {
		return err
	}

	e, err := resolveEnv(ctx, sess, ref)
	if err != nil {
		return err
	}

	// The body carries the chosen branch ONLY when the flag was set; omission
	// sends the empty body, which the server treats exactly as an absent one.
	// There is no client-side branch validation — a bad value comes back as a
	// server ValidationError through the standard exit-code path.
	body := api.EnvironmentsTriggerE2eJSONRequestBody{}
	if testsBranch != "" {
		branch := testsBranch
		body.TestsBranch = &branch
	}
	resp, err := sess.API.EnvironmentsTriggerE2eWithResponse(ctx, e.ID, body)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return client.Fail(resp, resp.Headers429)
	}

	runID := resp.JSON200.E2eRunId
	// The server echoes the requested tests branch on a non-default run
	// (omit-when-default). This is the value the server accepted and
	// recorded; drift dispatches it to the org's adapter but cannot verify
	// the adapter honoured it.
	echo := ""
	if resp.JSON200.TestsBranch != nil {
		echo = *resp.JSON200.TestsBranch
	}

	if !flags.shouldWait(policy) {
		return writeE2eResult(app, cols, e.Slug, runID, e.ID, "", "", echo)
	}

	// --wait: read the run resource when the server advertises the capability,
	// otherwise fall back to the audit log. The run row is the authoritative
	// resource; the audit fallback keeps an older server working unchanged.
	var outcome, reason, waitBranch string
	var waitErr error
	if sess.Discovery.Document.HasFeature(FeatureE2eRead) {
		outcome, reason, waitBranch, waitErr = waitForE2eRun(ctx, app, sess, e, runID, flags.deadline(policy))
	} else {
		outcome, reason, waitBranch, waitErr = waitForE2e(ctx, app, sess, e, runID, flags.deadline(policy))
	}
	// The wait path surfaces the branch the server recorded for the run — from
	// the run row on the read path, from the completed audit entry on the
	// fallback. The echo below is a defensive fallback only: on a conformant
	// server the recorded value and the trigger echo agree, because the
	// server's single normalization point nulls an explicit branch equal to the
	// profile default in BOTH the response and the record. The echo stands in
	// only for the off-spec case where the wait's source omits the field.
	if waitBranch == "" {
		waitBranch = echo
	}
	// Print the result even on failure — the operator needs to know which run
	// and what happened, and a bare error line does not say.
	if werr := writeE2eResult(app, cols, e.Slug, runID, e.ID, outcome, reason, waitBranch); werr != nil && waitErr == nil {
		return werr
	}
	return waitErr
}

func writeE2eResult(app *App, cols []output.Column, slug string, runID, envID uuid.UUID, outcome, reason, testsBranch string) error {
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	row := output.Row{
		"slug":          slug,
		"e2eRunId":      runID.String(),
		"testsBranch":   nilIfEmpty(testsBranch),
		"outcome":       nilIfEmpty(outcome),
		"reason":        nilIfEmpty(reason),
		"environmentId": envID.String(),
	}
	return app.Out.Write(&output.Doc{
		Columns: cols,
		Single:  true,
		Rows:    []output.Row{row},
	})
}

// nilIfEmpty returns nil for an empty string so the output layer renders "-"
// in tables and null in JSON, matching the convention for absent values.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// waitForE2e polls the audit log until an entry with
// action=environment.e2e_completed and a matching e2eRunId appears.
//
// The third return is the testsBranch the server recorded for the run, read
// from the completed entry's details map (omitted when the run used the
// profile default).
func waitForE2e(
	ctx context.Context, app *App, sess *Session, e *envRef,
	runID uuid.UUID, timeout time.Duration,
) (outcome string, reason string, testsBranch string, err error) {
	progress := wait.NewProgress(app.Stderr, output.IsTerminal(app.Stderr) && app.Out.ErrColor)
	defer progress.Stop()

	interval := app.waitInterval
	if interval == 0 {
		interval = wait.DefaultInterval
	}

	start := time.Now()
	deadline := start.Add(timeout)

	action := "environment.e2e_completed"
	limit := 5
	sortBy := api.AuditListParamsSortByTimestamp
	sortDir := api.AuditListParamsSortDirDesc

	for {
		params := &api.AuditListParams{
			Action:        &action,
			EnvironmentId: &e.ID,
			Limit:         &limit,
			SortBy:        &sortBy,
			SortDir:       &sortDir,
		}

		// Bound this poll by the wait deadline: the command's context carries no
		// deadline of its own, so without this a slow response could run past
		// --wait-timeout by up to the per-request --timeout, and a terminal
		// answer arriving after the deadline would be reported as the verdict.
		pctx, cancel := context.WithDeadline(ctx, deadline)
		resp, pollErr := sess.API.AuditListWithResponse(pctx, params)
		// Captured before cancel(), so a deadline that fired while the poll was
		// in flight is still visible afterwards.
		pollExpired := pctx.Err() == context.DeadlineExceeded
		// Released before the next iteration rather than deferred: a defer
		// inside the loop would hold every poll's timer until the wait ended.
		cancel()
		if pollErr != nil {
			// The wait deadline cut the poll short, so the request failed only
			// because it was cancelled at the deadline. That is the same "not
			// known yet" outcome as the between-polls check below, and takes the
			// same exit 6 — rather than a transport failure. A cancelled parent
			// context is excluded: that is the operator interrupting.
			if pollExpired && ctx.Err() == nil {
				return "", "", "", e2eTimeoutError(e.Slug, runID, time.Since(start), e2eAuditTimeoutHint)
			}
			return "", "", "", client.Transport(pollErr, sess.Resolved.Endpoint)
		}
		if resp.JSON200 == nil {
			// A rate limit during a poll is not fatal — back off and continue,
			// exactly as the environment wait does.
			fail := client.Fail(resp, resp.Headers429)
			if fail.Code == cliexit.RateLimited {
				backoff := fail.RetryAfter
				if backoff < interval {
					backoff = interval
				}
				progress.Throttled(backoff)
				if time.Now().Add(backoff).After(deadline) {
					return "", "", "", e2eTimeoutError(e.Slug, runID, time.Since(start), e2eAuditTimeoutHint)
				}
				if err := sleepCtx(ctx, backoff); err != nil {
					return "", "", "", &cliexit.ExitError{Code: cliexit.Error, Message: "the wait was interrupted", Err: err}
				}
				continue
			}
			return "", "", "", fail
		}

		for _, entry := range resp.JSON200.Items {
			if entry.Details == nil {
				continue
			}
			details := *entry.Details
			entryRunID, ok := details["e2eRunId"]
			if !ok {
				continue
			}
			// The details map comes from JSON, so the run id is a string.
			entryRunIDStr, ok := entryRunID.(string)
			if !ok {
				continue
			}
			if entryRunIDStr != runID.String() {
				continue
			}
			// Found the matching entry.
			out, _ := details["outcome"].(string)
			rsn, _ := details["reason"].(string)
			tests, _ := details["testsBranch"].(string)
			switch out {
			case "passed":
				return out, rsn, tests, nil
			case "failed":
				return out, rsn, tests, &cliexit.ExitError{
					Code:    cliexit.Conflict,
					Message: fmt.Sprintf("e2e run %s failed", runID),
					Detail:  reasonDetail(rsn),
				}
			default:
				// "error" or any other non-passed outcome.
				return out, rsn, tests, &cliexit.ExitError{
					Code:    cliexit.Error,
					Message: fmt.Sprintf("e2e run %s finished with outcome %q", runID, out),
					Detail:  reasonDetail(rsn),
				}
			}
		}

		progress.Observed(api.EnvironmentStatus("waiting"), time.Since(start))

		now := time.Now()
		if !now.Add(interval).Before(deadline) {
			return "", "", "", e2eTimeoutError(e.Slug, runID, now.Sub(start), e2eAuditTimeoutHint)
		}
		if err := sleepCtx(ctx, interval); err != nil {
			return "", "", "", &cliexit.ExitError{Code: cliexit.Error, Message: "the wait was interrupted", Err: err}
		}
	}
}

func reasonDetail(reason string) string {
	if reason == "" {
		return ""
	}
	return "reason: " + reason
}

// waitForE2eRun polls the run resource until it reaches a terminal status.
//
// This is the read path used when the server advertises
// `environments.e2e-read`; the audit-log fallback (waitForE2e) stays for older
// servers. The two share their poll cadence, deadline and terminal mapping --
// only the thing being read differs. The run row carries NO failure reason (the
// audit entry's `reason` has no equivalent there), so the second return is
// always empty rather than an invented one; the third is the branch the server
// recorded for the run (empty when the run used the profile default).
func waitForE2eRun(
	ctx context.Context, app *App, sess *Session, e *envRef,
	runID uuid.UUID, timeout time.Duration,
) (outcome string, reason string, testsBranch string, err error) {
	progress := wait.NewProgress(app.Stderr, output.IsTerminal(app.Stderr) && app.Out.ErrColor)
	defer progress.Stop()

	interval := app.waitInterval
	if interval == 0 {
		interval = wait.DefaultInterval
	}

	start := time.Now()
	deadline := start.Add(timeout)

	for {
		// Bound this poll by the wait deadline, exactly as the audit loop does:
		// the command's context carries no deadline of its own, so without this
		// a slow response could run past --wait-timeout by up to the
		// per-request --timeout.
		pctx, cancel := context.WithDeadline(ctx, deadline)
		resp, pollErr := sess.API.EnvironmentsGetE2eRunWithResponse(pctx, e.ID, runID)
		// Captured before cancel(), so a deadline that fired while the poll was
		// in flight is still visible afterwards.
		pollExpired := pctx.Err() == context.DeadlineExceeded
		// Released before the next iteration rather than deferred: a defer
		// inside the loop would hold every poll's timer until the wait ended.
		cancel()
		if pollErr != nil {
			// The wait deadline cut the poll short — the same "not known yet"
			// outcome as the between-polls check below, and the same exit 6,
			// rather than a transport failure. A cancelled parent context is
			// excluded: that is the operator interrupting.
			if pollExpired && ctx.Err() == nil {
				return "", "", "", e2eTimeoutError(e.Slug, runID, time.Since(start), e2eRunTimeoutHint)
			}
			return "", "", "", client.Transport(pollErr, sess.Resolved.Endpoint)
		}
		if resp.JSON200 == nil {
			// A 429 during a poll is not fatal -- back off and continue,
			// exactly as the audit loop does. A typed 404 is: the run is gone
			// or belongs to another environment, and retrying would only spin
			// until the deadline.
			fail := client.Fail(resp, resp.Headers429)
			if fail.Code == cliexit.RateLimited {
				backoff := fail.RetryAfter
				if backoff < interval {
					backoff = interval
				}
				progress.Throttled(backoff)
				if time.Now().Add(backoff).After(deadline) {
					return "", "", "", e2eTimeoutError(e.Slug, runID, time.Since(start), e2eRunTimeoutHint)
				}
				if err := sleepCtx(ctx, backoff); err != nil {
					return "", "", "", &cliexit.ExitError{Code: cliexit.Error, Message: "the wait was interrupted", Err: err}
				}
				continue
			}
			return "", "", "", fail
		}

		run := resp.JSON200
		tests := ""
		if run.TestsBranch != nil {
			tests = *run.TestsBranch
		}
		switch run.Status {
		case api.E2eRunStatusPassed:
			return "passed", "", tests, nil
		case api.E2eRunStatusFailed:
			return "failed", "", tests, &cliexit.ExitError{
				Code:    cliexit.Conflict,
				Message: fmt.Sprintf("e2e run %s failed", runID),
				Detail:  forgeRunDetail(run.ForgeRunUrl),
			}
		case api.E2eRunStatusDispatched, api.E2eRunStatusRunning:
			// Still in flight; keep polling.
		default:
			// "error" or any status a future server adds.
			return string(run.Status), "", tests, &cliexit.ExitError{
				Code:    cliexit.Error,
				Message: fmt.Sprintf("e2e run %s finished with outcome %q", runID, run.Status),
				Detail:  forgeRunDetail(run.ForgeRunUrl),
			}
		}

		progress.Observed(api.EnvironmentStatus("waiting"), time.Since(start))

		now := time.Now()
		if !now.Add(interval).Before(deadline) {
			return "", "", "", e2eTimeoutError(e.Slug, runID, now.Sub(start), e2eRunTimeoutHint)
		}
		if err := sleepCtx(ctx, interval); err != nil {
			return "", "", "", &cliexit.ExitError{Code: cliexit.Error, Message: "the wait was interrupted", Err: err}
		}
	}
}

// forgeRunDetail is the Detail for a failed/error exit on the run-read path.
// The run row has no failure reason, so the forge run URL -- when the server
// recorded one -- is the only actionable detail. It is the server's own value,
// not a reason the CLI invented, and it is labelled so a bare URL is not
// mistaken for one.
func forgeRunDetail(url *string) string {
	if url == nil || *url == "" {
		return ""
	}
	return "forge run: " + *url
}

// e2eTimeoutHints are the source-specific Hints for an e2e wait timeout. They
// differ because the two sources need different capabilities: following the
// audit log needs `audit-log.read`, while the run read needs only
// `environments.e2e-read`. Pointing a read-path operator at the audit log would
// send them after a dependency that path deliberately does not have.
const (
	e2eAuditTimeoutHint = "the run may still be in progress; check the audit log with `drift audit list --action environment.e2e_completed`"
	e2eRunTimeoutHint   = "the run may still be in progress; the run resource is authoritative — re-read it with `drift api GET /environments/{id}/e2e/{runId}`, or check the forge run once it records one"
)

// e2eTimeoutError builds the exit-6 failure for an e2e wait. The caller passes
// the Hint for the source it was following: the message is the same either way,
// but the advice is not.
func e2eTimeoutError(slug string, runID uuid.UUID, elapsed time.Duration, hint string) error {
	return &cliexit.ExitError{
		Code: cliexit.WaitTimeout,
		Message: fmt.Sprintf("timed out after %s waiting for e2e run %s on %s",
			elapsed.Round(time.Second), runID, slug),
		Hint: hint,
	}
}

// sleepCtx sleeps for d, returning ctx.Err() on cancellation. Mirrors
// internal/wait.realSleep so a cancelled context propagates immediately
// rather than burning a doomed HTTP attempt.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
