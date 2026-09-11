package cmd

import (
	"context"
	"fmt"
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
	// an audit-log entry, so the policy struct's Goal field is unused.
	policy := waitPolicy{Timeout: e2eDefaultTimeout, Blocks: false}

	cmd := &cobra.Command{
		Use:   "e2e <slug-or-id>",
		Short: "Trigger an end-to-end test run",
		Long: "Trigger an end-to-end test run against an environment.\n\n" +
			"Without --wait, prints the accepted run (slug + e2eRunId) and\n" +
			"returns immediately. With --wait, polls the audit log until the\n" +
			"run's outcome appears, then exits 0 on pass and non-zero on\n" +
			"failure.\n\n" +
			"The server's tracking ceiling is 120 minutes; the default\n" +
			"--wait-timeout is 125 minutes to stay clear of it.\n\n" +
			cliexit.Help,
		Args: exactArgs(1, "the environment slug or id"),
		RunE: func(c *cobra.Command, args []string) error {
			return runEnvE2e(c.Context(), app, args[0], policy, flags)
		},
	}
	// Manual flag registration: the e2e verb's wait is not "wait for a state"
	// but "wait for the run's audit entry", so the generic description from
	// waitFlags.register does not apply.
	flags.cmd = cmd
	cmd.Flags().BoolVar(&flags.wait, "wait", false, "wait for the e2e run to complete")
	cmd.Flags().BoolVar(&flags.noWait, "no-wait", true, "return as soon as the server accepts the request")
	cmd.Flags().DurationVar(&flags.timeout, "wait-timeout", e2eDefaultTimeout, "how long to wait before giving up (exit 6)")
	return cmd
}

// e2eColumns is the output shape for the e2e verb.
func e2eColumns() []output.Column {
	return []output.Column{
		{Name: "slug", Header: "Slug"},
		{Name: "e2eRunId", Header: "Run"},
		{Name: "outcome", Header: "Outcome"},
		{Name: "reason", Header: "Reason", Wide: true},
		{Name: "environmentId", Header: "Id", Wide: true},
	}
}

func runEnvE2e(ctx context.Context, app *App, ref string, policy waitPolicy, flags *waitFlags) error {
	if err := flags.validate(); err != nil {
		return err
	}

	cols := e2eColumns()

	sess, err := app.Connect(ctx, FeatureEnvironmentsWrite)
	if err != nil {
		return err
	}

	e, err := resolveEnv(ctx, sess, ref)
	if err != nil {
		return err
	}

	resp, err := sess.API.EnvironmentsTriggerE2eWithResponse(ctx, e.ID)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return client.Fail(resp, resp.Headers429)
	}

	runID := resp.JSON200.E2eRunId

	if !flags.shouldWait(policy) {
		return writeE2eResult(app, cols, e.Slug, runID, e.ID, "", "")
	}

	// --wait: poll the audit log for the run's completion entry.
	outcome, reason, waitErr := waitForE2e(ctx, app, sess, e, runID, flags.deadline(policy))
	// Print the result even on failure — the operator needs to know which run
	// and what happened, and a bare error line does not say.
	if werr := writeE2eResult(app, cols, e.Slug, runID, e.ID, outcome, reason); werr != nil && waitErr == nil {
		return werr
	}
	return waitErr
}

func writeE2eResult(app *App, cols []output.Column, slug string, runID, envID uuid.UUID, outcome, reason string) error {
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	row := output.Row{
		"slug":          slug,
		"e2eRunId":      runID.String(),
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
func waitForE2e(
	ctx context.Context, app *App, sess *Session, e *envRef,
	runID uuid.UUID, timeout time.Duration,
) (outcome string, reason string, err error) {
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

		resp, pollErr := sess.API.AuditListWithResponse(ctx, params)
		if pollErr != nil {
			return "", "", client.Transport(pollErr, sess.Resolved.Endpoint)
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
					return "", "", e2eTimeoutError(e.Slug, runID, time.Since(start))
				}
				if err := sleepCtx(ctx, backoff); err != nil {
					return "", "", &cliexit.ExitError{Code: cliexit.Error, Message: "the wait was interrupted", Err: err}
				}
				continue
			}
			return "", "", fail
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
			switch out {
			case "passed":
				return out, rsn, nil
			case "failed":
				return out, rsn, &cliexit.ExitError{
					Code:    cliexit.Conflict,
					Message: fmt.Sprintf("e2e run %s failed", runID),
					Detail:  reasonDetail(rsn),
				}
			default:
				// "error" or any other non-passed outcome.
				return out, rsn, &cliexit.ExitError{
					Code:    cliexit.Error,
					Message: fmt.Sprintf("e2e run %s finished with outcome %q", runID, out),
					Detail:  reasonDetail(rsn),
				}
			}
		}

		progress.Observed(api.EnvironmentStatus("waiting"), time.Since(start))

		now := time.Now()
		if !now.Add(interval).Before(deadline) {
			return "", "", e2eTimeoutError(e.Slug, runID, now.Sub(start))
		}
		if err := sleepCtx(ctx, interval); err != nil {
			return "", "", &cliexit.ExitError{Code: cliexit.Error, Message: "the wait was interrupted", Err: err}
		}
	}
}

func reasonDetail(reason string) string {
	if reason == "" {
		return ""
	}
	return "reason: " + reason
}

func e2eTimeoutError(slug string, runID uuid.UUID, elapsed time.Duration) error {
	return &cliexit.ExitError{
		Code: cliexit.WaitTimeout,
		Message: fmt.Sprintf("timed out after %s waiting for e2e run %s on %s",
			elapsed.Round(time.Second), runID, slug),
		Hint: "the run may still be in progress; check the audit log with `drift audit list --action environment.e2e_completed`",
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
