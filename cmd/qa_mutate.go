package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/steadfast-ly/drift-cli/internal/api"
	"github.com/steadfast-ly/drift-cli/internal/client"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
	"github.com/steadfast-ly/drift-cli/internal/output"
)

// The server's attachment caps (docs/qa-module.md §6), mirrored client-side so
// an oversized request fails with a sentence naming the flag instead of a 413
// after the whole body has been uploaded: at most ten files, 7 MiB of DECODED
// bytes in total (the base64 JSON then stays inside the mount's 10 MB limit).
const (
	qaMaxAttachmentBytes = 7 << 20
	qaMaxAttachments     = 10
)

// qaAttachmentJSON aliases the generated request bodies' attachment element, so
// the three bodies that carry `files` accept the same value without repeating
// the anonymous struct at every call site.
type qaAttachmentJSON = struct {
	ContentType string `json:"contentType"`
	Data        string `json:"data"`
	Filename    string `json:"filename"`
}

// qaResponse is every QA operation that declares a 409. The reads (list, get,
// health) declare none, so they use client.Fail directly.
type qaResponse interface {
	client.Response
	GetJSON409() *api.ApiProblem
}

// qaFailure is client.Fail plus the two QA-specific readings of a failure:
// a stale revision is a 409 whose `data.revision` names the one to re-check,
// and a 413 means the attachments were too large to send at all.
//
// A 502 (the approver role could not be confirmed) is deliberately left as the
// server wrote it: the message names the authority and the detail the reason,
// and rephrasing it here would only add a second copy to keep in step.
func qaFailure[H client.RetryAfterHeader](r qaResponse, retry *H) *cliexit.ExitError {
	e := client.Fail(r, retry)
	switch r.StatusCode() {
	case http.StatusRequestEntityTooLarge:
		e.Hint = fmt.Sprintf(
			"attachments must be at most %d files and %d MiB in total; reduce --file sizes",
			qaMaxAttachments, qaMaxAttachmentBytes>>20)
	case http.StatusConflict:
		if p := r.GetJSON409(); p != nil && p.Data != nil && p.Data.Revision != nil {
			e.Hint = fmt.Sprintf(
				"revision %d is now current; re-check with `drift qa show <key>` before deciding",
				*p.Data.Revision)
		}
	}
	return e
}

// qaWrite renders a single-object mutation result. Field validation happens in
// each command before it connects, so a typo in --json never costs a request.
//
// A nil row renders no object: a per-row mutation whose every row failed has no
// summary to show, and its per-row outcomes live in Extra.
func qaWrite(app *App, cols []output.Column, row output.Row, extra map[string]any) error {
	doc := &output.Doc{Columns: cols, Single: true, Extra: extra}
	if row != nil {
		doc.Rows = []output.Row{row}
	}
	return app.Out.Write(doc)
}

// --- session commands -------------------------------------------------------

func qaSessionMutationColumns() []output.Column {
	return []output.Column{
		{Name: "action", Header: "Action"},
		{Name: "key", Header: "Key"},
		{Name: "session", Header: "Session"},
		output.StatusColumn("state", "State"),
		{Name: "environment", Header: "Environment"},
		{Name: "generation", Header: "Generation"},
		{Name: "change", Header: "Change", Wide: true},
	}
}

func qaSessionMutationRow(ctx context.Context, sess *Session, action string, v *api.QaSessionView) output.Row {
	s := v.Session
	return output.Row{
		"action":      action,
		"key":         s.TicketKey,
		"session":     s.Id.String(),
		"state":       string(s.State),
		"environment": qaEnvironmentLabel(ctx, sess, s.EnvironmentId),
		"generation":  s.CurrentGeneration,
		"change":      s.UnacknowledgedChange,
	}
}

// qaEnvironmentLabel names a session's environment the way the rest of the
// CLI does. The session view carries only the id, so this costs one
// environments read; a deployment without that capability, or an id that no
// longer resolves, falls back to the id rather than adding a second failure to
// a mutation that already succeeded.
func qaEnvironmentLabel(ctx context.Context, sess *Session, id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	if !sess.Discovery.Document.HasFeature(FeatureEnvironmentsRead) {
		return id.String()
	}
	resp, err := sess.API.EnvironmentsGetWithResponse(ctx, id.String())
	if err != nil || resp.JSON200 == nil {
		return id.String()
	}
	return resp.JSON200.Environment.Slug
}

func qaSessionViewExtra(v *api.QaSessionView) map[string]any {
	ids := make([]string, 0, len(v.Session.PullRequestIds))
	for _, id := range v.Session.PullRequestIds {
		ids = append(ids, id.String())
	}
	return map[string]any{"pull_request_ids": ids, "events": len(v.Events)}
}

func newQaStartCommand(app *App) *cobra.Command {
	var prs []string
	var envSlug string
	var yes bool

	cmd := &cobra.Command{
		Use:   "start <key>",
		Short: "Start a QA session on a ticket",
		Long: "Start a Test Session on a Ticket: the server reuses or creates a\n" +
			"preview environment running every Linked PR's head.\n\n" +
			"--prs picks a partition explicitly, as repo#n references to the\n" +
			"Ticket's Linked PRs; repo#n names the forge PR and covers every one of\n" +
			"its repository rows, while a UUID names exactly one row. Omitted, the\n" +
			"server takes the first partition with no open session. --env reuses an\n" +
			"existing environment instead of the server's choice.\n\n" +
			"A rebuild of a developer's environment can happen under them, so the\n" +
			"command confirms and takes --yes.\n\n" +
			cliexit.Help,
		Args: exactArgs(1, "the ticket key, e.g. DT-123"),
		RunE: func(c *cobra.Command, args []string) error {
			return runQaStart(c.Context(), app, args[0], prs, envSlug, yes)
		},
	}
	cmd.Flags().StringSliceVar(&prs, "prs", nil,
		"linked PRs to test, as repo#n or a row UUID (comma-separated or repeatable)")
	cmd.Flags().StringVar(&envSlug, "env", "", "environment slug or id to reuse")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm without prompting")
	return cmd
}

func runQaStart(ctx context.Context, app *App, key string, prRefs []string, envRef string, yes bool) error {
	cols := qaSessionMutationColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}

	features := []string{FeatureQa}
	if envRef != "" {
		// Resolving a slug is a read on the environments surface; gating on it
		// keeps the refusal honest on a deployment that dropped that capability.
		features = append(features, FeatureEnvironmentsRead)
	}
	sess, err := app.Connect(ctx, features...)
	if err != nil {
		return err
	}

	body := api.QaStartJSONRequestBody{}
	summary := fmt.Sprintf("Start a QA session on %s.", key)
	if len(prRefs) > 0 {
		card, err := qaTicketCard(ctx, sess, key)
		if err != nil {
			return err
		}
		ids := make([]uuid.UUID, 0, len(prRefs))
		labels := make([]string, 0, len(prRefs))
		for _, ref := range prRefs {
			prIDs, label, err := resolveTicketPR(card, ref)
			if err != nil {
				return err
			}
			ids = append(ids, prIDs...)
			labels = append(labels, label)
		}
		body.Prs = &ids
		summary += fmt.Sprintf(" PRs: %s.", strings.Join(labels, ", "))
	}
	if envRef != "" {
		e, err := resolveEnv(ctx, sess, envRef)
		if err != nil {
			return err
		}
		id := e.ID
		body.EnvironmentId = &id
		summary += fmt.Sprintf(" Environment: %s.", e.Slug)
	}
	if err := app.Confirm(yes, summary, "start this QA session"); err != nil {
		return err
	}

	resp, err := sess.API.QaStartWithResponse(ctx, key, body)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return qaFailure(resp, resp.Headers429)
	}
	return qaWrite(app, cols, qaSessionMutationRow(ctx, sess, "start", resp.JSON200), qaSessionViewExtra(resp.JSON200))
}

// --- approve / reject -------------------------------------------------------

func qaVerdictPRs(in []api.QaVerdictPullRequest) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, p := range in {
		out = append(out, map[string]any{
			"repo":            p.ForgeRepo,
			"number":          p.Number,
			"title":           p.Title,
			"head_branch":     p.HeadBranch,
			"head_sha":        p.HeadSha,
			"pull_request_id": p.PullRequestId.String(),
			"repository_id":   p.RepositoryId.String(),
		})
	}
	return out
}

func qaEvaluationPayload(in []api.QaCompletionEvaluation) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, e := range in {
		m := map[string]any{
			"key":         e.TicketKey,
			"epoch":       e.Epoch,
			"evaluated":   e.Evaluated,
			"invalidated": e.Invalidated,
		}
		if e.Created != nil {
			m["completion"] = e.Created.Id.String()
			m["completion_state"] = string(e.Created.State)
		}
		out = append(out, m)
	}
	return out
}

func qaApprovalOutcomeColumns() []output.Column {
	return []output.Column{
		{Name: "action", Header: "Action"},
		{Name: "key", Header: "Key"},
		{Name: "approval", Header: "Approval"},
		{Name: "covered_count", Header: "Covered"},
		{Name: "uncovered_count", Header: "Uncovered"},
		{Name: "evaluations_count", Header: "Evaluations", Wide: true},
	}
}

func newQaApproveCommand(app *App) *cobra.Command {
	var revision int
	var retested bool
	var noRetest, note string
	var files []string
	var yes bool

	cmd := &cobra.Command{
		Use:   "approve <session>",
		Short: "Approve a QA session at the revision you tested",
		Long: "Approve a QA session. --revision is the deploy generation `drift qa\n" +
			"show` reported for the session; a redeploy since then is refused with\n" +
			"the new revision instead of approving code nobody tested.\n\n" +
			"Say whether the revision was retested: --retested, or --no-retest\n" +
			"with the reason it was not. --note is optional prose, --file attaches\n" +
			"evidence (repeatable; at most 10 files, 7 MiB in total).\n\n" +
			"Requires the `qa` role and the server's `qa.approve` capability.\n\n" +
			cliexit.Help,
		Args: exactArgs(1, "the session id"),
		RunE: func(c *cobra.Command, args []string) error {
			body := api.QaApproveJSONRequestBody{ShownGeneration: revision, Retested: retested}
			if c.Flags().Changed("no-retest") {
				body.NoRetestReason = new(noRetest)
			}
			if c.Flags().Changed("note") {
				body.Note = new(note)
			}
			return runQaApprove(c.Context(), app, args[0], body, files, yes, c)
		},
	}
	cmd.Flags().IntVar(&revision, "revision", 0, "the deploy generation shown for the session (required)")
	cmd.Flags().BoolVar(&retested, "retested", false, "the revision was retested")
	cmd.Flags().StringVar(&noRetest, "no-retest", "", "reason the revision was not retested")
	cmd.Flags().StringVar(&note, "note", "", "note to write with the verdict")
	cmd.Flags().StringArrayVar(&files, "file", nil, "attach a file (repeatable)")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm without prompting")
	return cmd
}

// qaSawChange reports whether the queue's card for this session records a
// change it saw. The queue is the only read that carries the flag for a
// session id, and a session it does not list is unknown rather than unchanged:
// the caller keeps its generic refusal instead of claiming a change it cannot
// see.
func qaSawChange(ctx context.Context, app *App, id uuid.UUID) bool {
	sess, err := app.Connect(ctx, FeatureQa)
	if err != nil {
		return false
	}
	resp, err := sess.API.QaListWithResponse(ctx)
	if err != nil || resp.JSON200 == nil {
		return false
	}
	queue := *resp.JSON200
	for _, cards := range [][]api.QaTicketCard{queue.ReadyForQa, queue.InTesting, queue.Recheck, queue.Upcoming} {
		for _, c := range cards {
			for _, s := range c.ActiveSessions {
				if s.Id == id {
					return s.SawChange
				}
			}
		}
	}
	return false
}

func runQaApprove(ctx context.Context, app *App, sessionArg string, body api.QaApproveJSONRequestBody, files []string, yes bool, c *cobra.Command) error {
	cols := qaApprovalOutcomeColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	id, err := uuid.Parse(sessionArg)
	if err != nil {
		return usageErrorf("invalid session id %q: not a UUID", sessionArg)
	}
	if !c.Flags().Changed("revision") || body.ShownGeneration < 0 {
		return usageErrorf("--revision is required (the generation `drift qa show` reported)")
	}
	// --retested=false states nothing about the revision, so it counts as unset:
	// the operator still has to say --no-retest <reason>.
	retestSet := c.Flags().Changed("retested") && body.Retested
	noRetestSet := c.Flags().Changed("no-retest")
	switch {
	case retestSet && noRetestSet:
		return usageErrorf("--retested and --no-retest contradict each other")
	case !retestSet && !noRetestSet:
		if qaSawChange(ctx, app, id) {
			return usageErrorf("the session saw a change: say --retested or --no-retest <reason>")
		}
		return usageErrorf("say whether the revision was retested: --retested or --no-retest <reason>")
	case noRetestSet && strings.TrimSpace(derefString(body.NoRetestReason)) == "":
		return usageErrorf("--no-retest needs a reason")
	}

	atts, err := qaAttachments(files)
	if err != nil {
		return err
	}
	if len(atts) > 0 {
		body.Files = &atts
	}

	sess, err := app.Connect(ctx, FeatureQa, FeatureQaApprove)
	if err != nil {
		return err
	}

	summary := fmt.Sprintf("Approve QA session %s at revision %d", id, body.ShownGeneration)
	if body.Retested {
		summary += " (retested)."
	} else {
		summary += fmt.Sprintf(" (not retested: %s).", strings.TrimSpace(derefString(body.NoRetestReason)))
	}
	if len(atts) > 0 {
		summary += fmt.Sprintf(" Attaching %d file(s).", len(atts))
	}
	if err := app.Confirm(yes, summary, "record this approval"); err != nil {
		return err
	}

	resp, err := sess.API.QaApproveWithResponse(ctx, id, body)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return qaFailure(resp, resp.Headers429)
	}
	out := *resp.JSON200
	return qaWrite(app, cols, output.Row{
		"action":            "approve",
		"key":               out.TicketKey,
		"approval":          out.ApprovalId.String(),
		"covered_count":     len(out.Covered),
		"uncovered_count":   len(out.Uncovered),
		"evaluations_count": len(out.Evaluations),
	}, map[string]any{
		"covered":     qaVerdictPRs(out.Covered),
		"uncovered":   qaVerdictPRs(out.Uncovered),
		"evaluations": qaEvaluationPayload(out.Evaluations),
	})
}

func qaRejectionColumns() []output.Column {
	return []output.Column{
		{Name: "action", Header: "Action"},
		{Name: "key", Header: "Key"},
		{Name: "rejection", Header: "Rejection"},
		{Name: "covered_count", Header: "Covered"},
		{Name: "uncovered_count", Header: "Uncovered"},
		{Name: "revoked", Header: "Approvals revoked", Wide: true},
	}
}

func newQaRejectCommand(app *App) *cobra.Command {
	var revision int
	var comment string
	var files []string
	var yes bool

	cmd := &cobra.Command{
		Use:   "reject <session>",
		Short: "Reject a QA session with a required comment",
		Long: "Reject a QA session at the revision you tested. The comment is written\n" +
			"to the tracker and is required; --file attaches evidence.\n\n" +
			"Requires the `qa` role and the server's `qa.approve` capability.\n\n" +
			cliexit.Help,
		Args: exactArgs(1, "the session id"),
		RunE: func(c *cobra.Command, args []string) error {
			return runQaReject(c.Context(), app, args[0], revision, comment, files, yes, c)
		},
	}
	cmd.Flags().IntVar(&revision, "revision", 0, "the deploy generation shown for the session (required)")
	cmd.Flags().StringVar(&comment, "comment", "", "why the revision was rejected (required)")
	cmd.Flags().StringArrayVar(&files, "file", nil, "attach a file (repeatable)")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm without prompting")
	return cmd
}

func runQaReject(ctx context.Context, app *App, sessionArg string, revision int, comment string, files []string, yes bool, c *cobra.Command) error {
	cols := qaRejectionColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	id, err := uuid.Parse(sessionArg)
	if err != nil {
		return usageErrorf("invalid session id %q: not a UUID", sessionArg)
	}
	if !c.Flags().Changed("revision") || revision < 0 {
		return usageErrorf("--revision is required (the generation `drift qa show` reported)")
	}
	if strings.TrimSpace(comment) == "" {
		return usageErrorf("--comment is required")
	}

	atts, err := qaAttachments(files)
	if err != nil {
		return err
	}
	body := api.QaRejectJSONRequestBody{ShownGeneration: revision, Comment: comment}
	if len(atts) > 0 {
		body.Files = &atts
	}

	sess, err := app.Connect(ctx, FeatureQa, FeatureQaApprove)
	if err != nil {
		return err
	}

	summary := fmt.Sprintf("Reject QA session %s at revision %d: %s", id, revision, comment)
	if len(atts) > 0 {
		summary += fmt.Sprintf(" Attaching %d file(s).", len(atts))
	}
	if err := app.Confirm(yes, summary, "record this rejection"); err != nil {
		return err
	}

	resp, err := sess.API.QaRejectWithResponse(ctx, id, body)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return qaFailure(resp, resp.Headers429)
	}
	out := *resp.JSON200
	return qaWrite(app, cols, output.Row{
		"action":          "reject",
		"key":             out.TicketKey,
		"rejection":       out.RejectionId.String(),
		"covered_count":   len(out.Covered),
		"uncovered_count": len(out.Uncovered),
		"revoked":         len(out.RevokedApprovalIds),
	}, map[string]any{
		"covered":              qaVerdictPRs(out.Covered),
		"uncovered":            qaVerdictPRs(out.Uncovered),
		"revoked_approval_ids": uuidStrings(out.RevokedApprovalIds),
		"evaluations":          qaEvaluationPayload(out.Evaluations),
	})
}

// --- exemptions and revocations --------------------------------------------

func qaGateExtra(g *api.QaGateTarget) map[string]any {
	if g == nil {
		return nil
	}
	return map[string]any{
		"repo":            g.ForgeRepo,
		"sha":             g.Sha,
		"pull_request_id": g.PullRequestId.String(),
	}
}

// --- per-row mutations ------------------------------------------------------

// qaRowRef names one row of a per-row mutation the way the command's payload
// does: a pull request row by id, or a ticket for a ticket-scoped exemption.
type qaRowRef struct {
	Field string
	Value string
}

func (r qaRowRef) payload(fields map[string]any) map[string]any {
	m := make(map[string]any, len(fields)+1)
	m[r.Field] = r.Value
	for k, v := range fields {
		m[k] = v
	}
	return m
}

// Row outcomes of a per-row mutation: what the CLI knows about each row after
// it was attempted.
const (
	// qaRowOK means the server committed the row.
	qaRowOK = "ok"
	// qaRowRefused means the server answered with an HTTP error, so nothing
	// changed for that row.
	qaRowRefused = "refused"
	// qaRowUnknown means no usable answer arrived -- a transport error, a
	// timeout, or a 2xx without the row's result -- so the row may have changed.
	qaRowUnknown = "unknown"
	// qaRowNotAttempted means the run stopped before the row was dispatched, so
	// no request was sent for it and nothing can have changed.
	qaRowNotAttempted = "not_attempted"
)

// qaRowResult is one row of a per-row mutation. A monorepo PR is several rows
// and the server commits each independently, so every row is reported on its
// own rather than folded into one verdict.
type qaRowResult struct {
	Row     qaRowRef
	Outcome string
	Fields  map[string]any     // the row's own result, on ok
	Err     *cliexit.ExitError // nil on ok
}

// qaUnknownHint says what a transport failure leaves uncertain and how to
// check it. keys are the Tickets the command knows can show the row; there is
// none when it knows of none.
func qaUnknownHint(keys ...string) string {
	switch len(keys) {
	case 0:
		return "the row may have changed; check with `drift qa show <key>` before retrying it"
	case 1:
		return fmt.Sprintf("the row may have changed; check with `drift qa show %s` before retrying it", keys[0])
	default:
		return fmt.Sprintf("the row may have changed; check with `drift qa show` on %s before retrying it",
			strings.Join(keys, " or "))
	}
}

// qaUnknownRow classifies a request that got no answer at all: the row may have
// been committed before the connection failed.
func qaUnknownRow(ref qaRowRef, err error, endpoint string, keys ...string) qaRowResult {
	e := client.Transport(err, endpoint)
	e.Hint = qaUnknownHint(keys...)
	return qaRowResult{Row: ref, Outcome: qaRowUnknown, Err: e}
}

// qaFailedRow classifies a response that carried no outcome: an HTTP error is a
// refusal the server made, while a 2xx the client could not decode leaves the
// row's state unknown.
func qaFailedRow[H client.RetryAfterHeader](ref qaRowRef, resp qaResponse, retry *H, keys ...string) qaRowResult {
	if code := resp.StatusCode(); code >= 200 && code < 300 {
		return qaRowResult{Row: ref, Outcome: qaRowUnknown, Err: &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: fmt.Sprintf("the server answered HTTP %d without the row's result", code),
			Hint:    qaUnknownHint(keys...),
		}}
	}
	return qaRowResult{Row: ref, Outcome: qaRowRefused, Err: qaFailure(resp, retry)}
}

// qaNotAttemptedRow is a row the run never dispatched: the operator interrupted
// it, so nothing was sent and nothing changed. No exit code is dedicated to an
// interrupt (the e2e wait reports one as a plain error), so it is the general
// failure code the transport error in flight also carries.
func qaNotAttemptedRow(ref qaRowRef) qaRowResult {
	return qaRowResult{Row: ref, Outcome: qaRowNotAttempted, Err: &cliexit.ExitError{
		Code:    cliexit.Error,
		Message: "the run was interrupted before this row was sent",
	}}
}

// qaInterruptible scopes Ctrl-C/SIGTERM to a per-row loop, as env db/tunnel
// do for theirs: the signal cancels the in-flight request instead of killing
// the process, so the loop can still report which rows it sent. Installed
// after the confirmation, so an interrupt at the prompt still exits at once.
func qaInterruptible(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
}

// qaInterruptedRows appends the rows from `from` on as not_attempted when the
// context is cancelled, and reports whether the run must stop. An interrupted
// run dispatches nothing more, so those rows must not claim a request was made.
func qaInterruptedRows(ctx context.Context, refs []qaRowRef, from int) ([]qaRowResult, bool) {
	if ctx.Err() == nil {
		return nil, false
	}
	out := make([]qaRowResult, 0, len(refs)-from)
	for _, ref := range refs[from:] {
		out = append(out, qaNotAttemptedRow(ref))
	}
	return out, true
}

// qaRowsPayload is the machine contract: one entry per row, with the row's own
// result on success and the failure's code, message and hint otherwise.
func qaRowsPayload(in []qaRowResult) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, r := range in {
		m := r.Row.payload(r.Fields)
		m["outcome"] = r.Outcome
		if r.Err != nil {
			m["exit_code"] = r.Err.Code
			m["message"] = r.Err.Message
			if r.Err.Hint != "" {
				m["hint"] = r.Err.Hint
			}
		}
		out = append(out, m)
	}
	return out
}

// qaPartial reports whether at least one row reported ok and at least one
// reported something else. It says nothing about whether an unknown row
// committed: that row may be in either state.
func qaPartial(in []qaRowResult) bool {
	ok, failed := 0, 0
	for _, r := range in {
		if r.Outcome == qaRowOK {
			ok++
			continue
		}
		failed++
	}
	return ok > 0 && failed > 0
}

func qaFailedRowColumns() []output.Column {
	return []output.Column{
		{Name: "row", Header: "Row"},
		{Name: "outcome", Header: "Outcome"},
		{Name: "message", Header: "Message"},
		{Name: "hint", Header: "Hint"},
	}
}

// qaReportRows lists the rows that did not change after the ones that did, and
// returns the first failure with its own exit code: a partial run must never
// exit 0, and a script must see the code the server's first refusal named. A
// single-row run has no partial state, so it returns the failure unwrapped.
func qaReportRows(app *App, total int, rows []qaRowResult) error {
	failed := make([]qaRowResult, 0, len(rows))
	for _, r := range rows {
		if r.Outcome != qaRowOK {
			failed = append(failed, r)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	// One row and it failed: nothing changed, so the server's own error is the
	// whole report and there is no partial state to list.
	if total == 1 {
		return failed[0].Err
	}
	if f := app.Out.EffectiveFormat(); f == output.FormatTable || f == output.FormatWide {
		table := make([]output.Row, 0, len(failed))
		for _, r := range failed {
			// Every row carries its own hint: the returned error below only
			// renders the first one, and each unknown row needs its own "check
			// it with" advice.
			table = append(table, output.Row{
				"row": r.Row.Value, "outcome": r.Outcome, "message": r.Err.Message,
				"hint": r.Err.Hint,
			})
		}
		fmt.Fprintln(app.Stdout, "\nFAILED ROWS")
		if err := app.Out.Write(&output.Doc{Columns: qaFailedRowColumns(), Rows: table}); err != nil {
			return err
		}
	}
	first := *failed[0].Err
	first.Message = fmt.Sprintf("%d of %d repository row(s) failed: %s",
		len(failed), total, first.Message)
	return &first
}

// qaAppendUnique is a union in the order the rows reported, so several rows
// naming the same ticket do not repeat it.
func qaAppendUnique(dst []string, values ...string) []string {
	seen := make(map[string]bool, len(dst)+len(values))
	for _, v := range dst {
		seen[v] = true
	}
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			dst = append(dst, v)
		}
	}
	return dst
}

// qaUnionEvaluations merges the rows' completion evaluations, one entry per
// ticket and epoch. The rows of one forge PR report the same pair, and a later
// row reports the newer state, so the last reading wins its flags; the first
// appearance keeps its position and its `created` completion, which a later
// reading of the same epoch may omit.
func qaUnionEvaluations(in [][]api.QaCompletionEvaluation) []api.QaCompletionEvaluation {
	out := make([]api.QaCompletionEvaluation, 0)
	index := make(map[string]int, len(in))
	for _, evals := range in {
		for _, e := range evals {
			k := e.TicketKey + "/" + strconv.Itoa(e.Epoch)
			if i, ok := index[k]; ok {
				if e.Created == nil {
					e.Created = out[i].Created
				}
				out[i] = e
				continue
			}
			index[k] = len(out)
			out = append(out, e)
		}
	}
	return out
}

// qaRowsNote is the confirmation's row count, so an operator sees how many
// registrations a repo#n reference is about to change before confirming. One
// row is the ordinary case and needs no note.
func qaRowsNote(n int) string {
	if n <= 1 {
		return ""
	}
	return fmt.Sprintf(" (%d repository rows)", n)
}

// qaJoinIDs renders every id a per-row run produced, so each exemption stays
// revocable from the one table cell.
func qaJoinIDs(in []uuid.UUID) string {
	out := make([]string, 0, len(in))
	for _, id := range in {
		out = append(out, id.String())
	}
	return strings.Join(out, ", ")
}

func qaExemptionOutcomeColumns() []output.Column {
	return []output.Column{
		{Name: "action", Header: "Action"},
		{Name: "exemption", Header: "Exemption"},
		{Name: "scope", Header: "Scope"},
		{Name: "keys", Header: "Tickets"},
		{Name: "gate", Header: "Gate", Wide: true},
	}
}

func newQaExemptCommand(app *App) *cobra.Command {
	var prRef, ticketKey, reason string
	var yes bool

	cmd := &cobra.Command{
		Use:   "exempt (--pr repo#n | --ticket KEY) --reason ...",
		Short: "Record a QA exemption on a PR or a ticket",
		Long: "Record a \"no QA needed\" on a Linked PR or a whole Ticket, with a\n" +
			"reason. A PR-scoped exemption survives pushes; a Ticket-scoped one\n" +
			"covers the PRs linked at that moment.\n\n" +
			"--pr resolves a repo#n reference against the QA queue; repo#n names the\n" +
			"forge PR and covers every one of its repository rows. A PR that is no\n" +
			"longer on a queued Ticket has to be named by its UUID instead. A UUID\n" +
			"names exactly one repository row: it is the precise address, and the way\n" +
			"to retry a single failed row.\n\n" +
			cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runQaExempt(c.Context(), app, prRef, ticketKey, reason, yes)
		},
	}
	cmd.Flags().StringVar(&prRef, "pr", "", "linked PR to exempt, as repo#n (or its UUID)")
	cmd.Flags().StringVar(&ticketKey, "ticket", "", "ticket key to exempt")
	cmd.Flags().StringVar(&reason, "reason", "", "why QA is not needed (required)")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm without prompting")
	return cmd
}

func runQaExempt(ctx context.Context, app *App, prRef, ticketKey, reason string, yes bool) error {
	cols := qaExemptionOutcomeColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	switch {
	case prRef != "" && ticketKey != "":
		return usageErrorf("--pr and --ticket contradict each other")
	case prRef == "" && ticketKey == "":
		return usageErrorf("name what to exempt: --pr repo#n or --ticket KEY")
	case strings.TrimSpace(reason) == "":
		return usageErrorf("--reason is required")
	}

	sess, err := app.Connect(ctx, FeatureQa)
	if err != nil {
		return err
	}

	body := api.QaExemptJSONRequestBody{Reason: reason}
	bodies := []api.QaExemptJSONRequestBody{}
	// cardKeys are the Tickets the row can be seen on, for the unknown-row
	// hint. A ticket-scoped exemption names its Ticket; a PR one takes the
	// Tickets whose queue cards carried the PR.
	var cardKeys []string
	summary := ""
	if prRef != "" {
		ids, label, keys, err := resolveQueuedPR(ctx, sess, prRef,
			", or use --ticket for a ticket-scoped exemption")
		if err != nil {
			return err
		}
		cardKeys = keys
		body.Scope = api.QaExemptJSONBodyScopePr
		for _, id := range ids {
			b := body
			b.PullRequestId = &id
			bodies = append(bodies, b)
		}
		summary = fmt.Sprintf("Exempt %s from QA: %s", label, reason) + qaRowsNote(len(ids))
	} else {
		body.Scope = api.QaExemptJSONBodyScopeTicket
		body.TicketKey = &ticketKey
		bodies = append(bodies, body)
		cardKeys = []string{ticketKey}
		summary = fmt.Sprintf("Exempt every PR linked to %s from QA: %s", ticketKey, reason)
	}
	if err := app.Confirm(yes, summary, "record this exemption"); err != nil {
		return err
	}

	// A PR exemption is per row, so a monorepo PR needs one per registration.
	// Every row is attempted: the server commits each on its own, so stopping at
	// the first refusal would hide the rows that already hold an exemption. An
	// interrupt is the one exception: nothing after it may be dispatched.
	refs := make([]qaRowRef, len(bodies))
	for i, b := range bodies {
		refs[i] = qaExemptRowRef(b)
	}
	ctx, stopSignals := qaInterruptible(ctx)
	defer stopSignals()
	rows := make([]qaRowResult, 0, len(bodies))
	exemptionIDs := make([]uuid.UUID, 0, len(bodies))
	keys := make([]string, 0, len(bodies))
	evals := make([][]api.QaCompletionEvaluation, 0, len(bodies))
	var last *api.QaExemptionOutcome
	for i, b := range bodies {
		ref := refs[i]
		resp, err := sess.API.QaExemptWithResponse(ctx, b)
		if err != nil {
			rows = append(rows, qaUnknownRow(ref, err, sess.Resolved.Endpoint, cardKeys...))
			if rest, stop := qaInterruptedRows(ctx, refs, i+1); stop {
				rows = append(rows, rest...)
				break
			}
			continue
		}
		if resp.JSON200 == nil {
			rows = append(rows, qaFailedRow(ref, resp, resp.Headers429, cardKeys...))
			continue
		}
		out := *resp.JSON200
		last = &out
		exemptionIDs = append(exemptionIDs, out.ExemptionId)
		keys = qaAppendUnique(keys, out.TicketKeys...)
		evals = append(evals, out.Evaluations)
		rows = append(rows, qaRowResult{Row: ref, Outcome: qaRowOK, Fields: map[string]any{
			"exemption_id": out.ExemptionId.String(),
			"gate_target":  qaGateExtra(out.GateTarget),
		}})
	}

	extra := map[string]any{
		"exemption_ids": uuidStrings(exemptionIDs),
		"ticket_keys":   keys,
		"rows":          qaRowsPayload(rows),
		"partial":       qaPartial(rows),
		"evaluations":   qaEvaluationPayload(qaUnionEvaluations(evals)),
	}
	var row output.Row
	if last != nil {
		gate := ""
		if last.GateTarget != nil {
			gate = last.GateTarget.ForgeRepo + "@" + shortSHAString(last.GateTarget.Sha)
		}
		row = output.Row{
			"action":    "exempt",
			"exemption": qaJoinIDs(exemptionIDs),
			"scope":     string(last.Scope),
			"keys":      keys,
			"gate":      nilIfEmpty(gate),
		}
		extra["gate_target"] = qaGateExtra(last.GateTarget)
	}
	if err := qaWrite(app, cols, row, extra); err != nil {
		return err
	}
	return qaReportRows(app, len(bodies), rows)
}

// qaExemptRowRef names the row an exemption request is for: a PR row by id, or
// the ticket for a ticket-scoped exemption, which is one row by construction.
func qaExemptRowRef(b api.QaExemptJSONRequestBody) qaRowRef {
	if b.PullRequestId != nil {
		return qaRowRef{Field: "pull_request_id", Value: b.PullRequestId.String()}
	}
	return qaRowRef{Field: "ticket_key", Value: derefString(b.TicketKey)}
}

func qaRevocationColumns() []output.Column {
	return []output.Column{
		{Name: "action", Header: "Action"},
		{Name: "keys", Header: "Tickets"},
		{Name: "approvals_revoked", Header: "Approvals revoked"},
		{Name: "moved", Header: "Moved to queue"},
		{Name: "gate", Header: "Gate", Wide: true},
	}
}

func newQaUnexemptCommand(app *App) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "unexempt <exemption-id>",
		Short: "Revoke a QA exemption",
		Long: "Revoke an exemption by id, restoring the gate's normal verdict for the\n" +
			"PR or Ticket it covered.\n\n" + cliexit.Help,
		Args: exactArgs(1, "the exemption id"),
		RunE: func(c *cobra.Command, args []string) error {
			return runQaUnexempt(c.Context(), app, args[0], yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm without prompting")
	return cmd
}

func runQaUnexempt(ctx context.Context, app *App, idArg string, yes bool) error {
	cols := qaRevocationColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	id, err := uuid.Parse(idArg)
	if err != nil {
		return usageErrorf("invalid exemption id %q: not a UUID", idArg)
	}
	sess, err := app.Connect(ctx, FeatureQa)
	if err != nil {
		return err
	}
	if err := app.Confirm(yes,
		fmt.Sprintf("Revoke QA exemption %s; the gate returns to the normal verdict.", id),
		"revoke this exemption"); err != nil {
		return err
	}
	resp, err := sess.API.QaUnexemptWithResponse(ctx, id)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return qaFailure(resp, resp.Headers429)
	}
	return qaWrite(app, cols, qaRevocationRow("unexempt", resp.JSON200), qaRevocationExtra(resp.JSON200))
}

func newQaRevokeCommand(app *App) *cobra.Command {
	var approvalID, exemptionID string
	var yes bool
	cmd := &cobra.Command{
		Use:   "revoke (--approval <id> | --exemption <id>)",
		Short: "Revoke a QA approval or exemption",
		Long: "Revoke an approval or an exemption by id. Revoking an exemption is the\n" +
			"same operation as `unexempt`.\n\n" + cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runQaRevoke(c.Context(), app, approvalID, exemptionID, yes)
		},
	}
	cmd.Flags().StringVar(&approvalID, "approval", "", "approval id to revoke")
	cmd.Flags().StringVar(&exemptionID, "exemption", "", "exemption id to revoke")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm without prompting")
	return cmd
}

func runQaRevoke(ctx context.Context, app *App, approvalArg, exemptionArg string, yes bool) error {
	cols := qaRevocationColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	switch {
	case approvalArg != "" && exemptionArg != "":
		return usageErrorf("--approval and --exemption contradict each other")
	case approvalArg == "" && exemptionArg == "":
		return usageErrorf("name what to revoke: --approval <id> or --exemption <id>")
	}
	body := api.QaRevokeJSONRequestBody{}
	target := ""
	if approvalArg != "" {
		id, err := uuid.Parse(approvalArg)
		if err != nil {
			return usageErrorf("invalid approval id %q: not a UUID", approvalArg)
		}
		body.ApprovalId = &id
		target = "approval " + id.String()
	} else {
		id, err := uuid.Parse(exemptionArg)
		if err != nil {
			return usageErrorf("invalid exemption id %q: not a UUID", exemptionArg)
		}
		body.ExemptionId = &id
		target = "exemption " + id.String()
	}

	sess, err := app.Connect(ctx, FeatureQa)
	if err != nil {
		return err
	}
	if err := app.Confirm(yes,
		fmt.Sprintf("Revoke QA %s; the gate returns to the normal verdict.", target),
		"revoke this grant"); err != nil {
		return err
	}
	resp, err := sess.API.QaRevokeWithResponse(ctx, body)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return qaFailure(resp, resp.Headers429)
	}
	return qaWrite(app, cols, qaRevocationRow("revoke", resp.JSON200), qaRevocationExtra(resp.JSON200))
}

func qaRevocationRow(action string, out *api.QaRevocationOutcome) output.Row {
	return output.Row{
		"action":            action,
		"keys":              out.TicketKeys,
		"approvals_revoked": len(out.RevokedApprovalIds),
		"moved":             out.MovedToQueue,
		"gate":              qaGateLabel(out.GateTarget),
	}
}

func qaRevocationExtra(out *api.QaRevocationOutcome) map[string]any {
	return map[string]any{
		"revoked_approval_ids": uuidStrings(out.RevokedApprovalIds),
		"gate_target":          qaGateExtra(out.GateTarget),
		"evaluations":          qaEvaluationPayload(out.Evaluations),
	}
}

// --- links, send, comment, ack, end, carry-over -----------------------------

func qaLinkColumns() []output.Column {
	return []output.Column{
		{Name: "action", Header: "Action"},
		{Name: "key", Header: "Key"},
		{Name: "pr", Header: "PR"},
		{Name: "result", Header: "Result"},
	}
}

func newQaLinkCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "link <key> <repo#n>",
		Short: "Link a pull request to a ticket by hand",
		Long: "Link a pull request to a Ticket by hand. repo#n is resolved against\n" +
			"the QA queue and names the forge PR, covering every one of its repository\n" +
			"rows; a PR that is not on any queued Ticket has to be named by its UUID\n" +
			"instead. A UUID names exactly one repository row: it is the precise\n" +
			"address, and the way to retry a single failed row.\n\n" +
			"Links are sticky: a merged PR stays linked.\n\n" + cliexit.Help,
		Args: exactArgs(2, "the ticket key and repo#n (or the PR's UUID)"),
		RunE: func(c *cobra.Command, args []string) error {
			return runQaLink(c.Context(), app, args[0], args[1])
		},
	}
}

func runQaLink(ctx context.Context, app *App, key, prRef string) error {
	cols := qaLinkColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	sess, err := app.Connect(ctx, FeatureQa)
	if err != nil {
		return err
	}
	ids, label, _, err := resolveQueuedPR(ctx, sess, prRef, "")
	if err != nil {
		return err
	}
	// A link is per row, so a monorepo PR is linked once per registration, and
	// every row is attempted: a refusal halfway through must not hide the rows
	// that already linked. An interrupt is the one exception.
	refs := make([]qaRowRef, len(ids))
	for i, id := range ids {
		refs[i] = qaRowRef{Field: "pull_request_id", Value: id.String()}
	}
	ctx, stopSignals := qaInterruptible(ctx)
	defer stopSignals()
	rows := make([]qaRowResult, 0, len(ids))
	linked := make([]uuid.UUID, 0, len(ids))
	results := make([]string, 0, len(ids))
	evals := make([][]api.QaCompletionEvaluation, 0, len(ids))
	ticketKey := ""
	for i, id := range ids {
		ref := refs[i]
		resp, err := sess.API.QaLinkWithResponse(ctx, api.QaLinkJSONRequestBody{TicketKey: key, PullRequestId: id})
		if err != nil {
			rows = append(rows, qaUnknownRow(ref, err, sess.Resolved.Endpoint, key))
			if rest, stop := qaInterruptedRows(ctx, refs, i+1); stop {
				rows = append(rows, rest...)
				break
			}
			continue
		}
		if resp.JSON200 == nil {
			rows = append(rows, qaFailedRow(ref, resp, resp.Headers429, key))
			continue
		}
		out := *resp.JSON200
		ticketKey = out.TicketKey
		linked = append(linked, out.PullRequestId)
		results = qaAppendUnique(results, string(out.Result))
		evals = append(evals, out.Evaluations)
		rows = append(rows, qaRowResult{Row: ref, Outcome: qaRowOK, Fields: map[string]any{
			"result": string(out.Result),
		}})
	}
	extra := map[string]any{
		"pull_request_ids": uuidStrings(linked),
		"rows":             qaRowsPayload(rows),
		"partial":          qaPartial(rows),
		"evaluations":      qaEvaluationPayload(qaUnionEvaluations(evals)),
	}
	var row output.Row
	if len(linked) > 0 {
		row = output.Row{
			"action": "link",
			"key":    ticketKey,
			"pr":     label,
			"result": strings.Join(results, ", "),
		}
	}
	if err := qaWrite(app, cols, row, extra); err != nil {
		return err
	}
	return qaReportRows(app, len(ids), rows)
}

func qaUnlinkColumns() []output.Column {
	return []output.Column{
		{Name: "action", Header: "Action"},
		{Name: "key", Header: "Key"},
		{Name: "repo", Header: "Repo"},
		{Name: "pr", Header: "PR"},
		{Name: "sha", Header: "Sha", Wide: true},
	}
}

func newQaUnlinkCommand(app *App) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "unlink <key> <repo#n>",
		Short: "Remove a hand-made or discovered link",
		Long: "Remove the link between a pull request and a Ticket. The PR must be\n" +
			"linked to that Ticket. repo#n names the forge PR and covers every one of\n" +
			"its repository rows; a UUID names exactly one repository row, the precise\n" +
			"address and the way to retry a single failed row.\n\n" +
			"Requires the `qa` role and the server's `qa.approve` capability.\n\n" +
			cliexit.Help,
		Args: exactArgs(2, "the ticket key and repo#n (or the PR's UUID)"),
		RunE: func(c *cobra.Command, args []string) error {
			return runQaUnlink(c.Context(), app, args[0], args[1], yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm without prompting")
	return cmd
}

func runQaUnlink(ctx context.Context, app *App, key, prRef string, yes bool) error {
	cols := qaUnlinkColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	sess, err := app.Connect(ctx, FeatureQa, FeatureQaApprove)
	if err != nil {
		return err
	}
	card, err := qaTicketCard(ctx, sess, key)
	if err != nil {
		return err
	}
	ids, label, err := resolveTicketPR(card, prRef)
	if err != nil {
		return err
	}
	summary := fmt.Sprintf("Unlink %s from %s; its QA approval no longer covers that PR.", label, key) +
		qaRowsNote(len(ids))
	if err := app.Confirm(yes, summary, "unlink this pull request"); err != nil {
		return err
	}
	// A link is per row, so a monorepo PR is unlinked once per registration, and
	// every row is attempted: a refusal halfway through must not hide the rows
	// that are already gone. An interrupt is the one exception.
	refs := make([]qaRowRef, len(ids))
	for i, id := range ids {
		refs[i] = qaRowRef{Field: "pull_request_id", Value: id.String()}
	}
	ctx, stopSignals := qaInterruptible(ctx)
	defer stopSignals()
	rows := make([]qaRowResult, 0, len(ids))
	unlinked := make([]uuid.UUID, 0, len(ids))
	evals := make([][]api.QaCompletionEvaluation, 0, len(ids))
	var last *api.QaUnlinkOutcome
	for i, id := range ids {
		ref := refs[i]
		resp, err := sess.API.QaUnlinkWithResponse(ctx, api.QaUnlinkJSONRequestBody{TicketKey: key, PullRequestId: id})
		if err != nil {
			rows = append(rows, qaUnknownRow(ref, err, sess.Resolved.Endpoint, key))
			if rest, stop := qaInterruptedRows(ctx, refs, i+1); stop {
				rows = append(rows, rest...)
				break
			}
			continue
		}
		if resp.JSON200 == nil {
			rows = append(rows, qaFailedRow(ref, resp, resp.Headers429, key))
			continue
		}
		out := *resp.JSON200
		last = &out
		unlinked = append(unlinked, id)
		evals = append(evals, out.Evaluations)
		rows = append(rows, qaRowResult{Row: ref, Outcome: qaRowOK, Fields: map[string]any{
			"result":      "unlinked",
			"gate_target": qaGateExtra(&out.GateTarget),
		}})
	}
	extra := map[string]any{
		"pull_request_ids": uuidStrings(unlinked),
		"rows":             qaRowsPayload(rows),
		"partial":          qaPartial(rows),
		"evaluations":      qaEvaluationPayload(qaUnionEvaluations(evals)),
	}
	var row output.Row
	if last != nil {
		row = output.Row{
			"action": "unlink",
			"key":    last.TicketKey,
			"repo":   last.ForgeRepo,
			"pr":     label,
			"sha":    shortSHAString(last.Sha),
		}
		extra["head_sha"] = last.Sha
		extra["gate_target"] = qaGateExtra(&last.GateTarget)
	}
	if err := qaWrite(app, cols, row, extra); err != nil {
		return err
	}
	return qaReportRows(app, len(ids), rows)
}

func qaSendColumns() []output.Column {
	return []output.Column{
		{Name: "action", Header: "Action"},
		{Name: "key", Header: "Key"},
		{Name: "done", Header: "Outbox done"},
		{Name: "failed", Header: "Outbox failed"},
		{Name: "retried", Header: "Retried"},
		{Name: "paused", Header: "Paused", Wide: true},
	}
}

func newQaSendCommand(app *App) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "send <key>",
		Short: "Send a ticket back to the QA queue",
		Long: "Move a Ticket back to the queue status and drain its tracker outbox.\n\n" +
			cliexit.Help,
		Args: exactArgs(1, "the ticket key"),
		RunE: func(c *cobra.Command, args []string) error {
			return runQaSend(c.Context(), app, args[0], yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm without prompting")
	return cmd
}

func runQaSend(ctx context.Context, app *App, key string, yes bool) error {
	cols := qaSendColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	sess, err := app.Connect(ctx, FeatureQa)
	if err != nil {
		return err
	}
	if err := app.Confirm(yes, fmt.Sprintf("Send %s back to the QA queue.", key), "send this ticket to QA"); err != nil {
		return err
	}
	resp, err := sess.API.QaSendWithResponse(ctx, key)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return qaFailure(resp, resp.Headers429)
	}
	out := *resp.JSON200
	row := output.Row{"action": "send", "key": out.TicketKey}
	extra := map[string]any{}
	if d := out.Drain; d != nil {
		row["done"] = d.Done
		row["failed"] = d.Failed
		row["retried"] = d.Retried
		row["paused"] = d.Paused
		extra["drain"] = map[string]any{
			"done": d.Done, "failed": d.Failed, "retried": d.Retried,
			"cancelled": d.Cancelled, "conflicts": d.Conflicts, "paused": d.Paused,
		}
	}
	return qaWrite(app, cols, row, extra)
}

func qaCommentColumns() []output.Column {
	return []output.Column{
		{Name: "action", Header: "Action"},
		{Name: "key", Header: "Key"},
	}
}

func newQaCommentCommand(app *App) *cobra.Command {
	var body string
	var files []string
	cmd := &cobra.Command{
		Use:   "comment <key> --body ...",
		Short: "Comment on a ticket without changing its status",
		Long: "Write a comment on a Ticket in the tracker, without changing its QA\n" +
			"status. --file attaches evidence (repeatable).\n\n" + cliexit.Help,
		Args: exactArgs(1, "the ticket key"),
		RunE: func(c *cobra.Command, args []string) error {
			return runQaComment(c.Context(), app, args[0], body, files)
		},
	}
	cmd.Flags().StringVar(&body, "body", "", "the comment text (required)")
	cmd.Flags().StringArrayVar(&files, "file", nil, "attach a file (repeatable)")
	return cmd
}

func runQaComment(ctx context.Context, app *App, key, body string, files []string) error {
	cols := qaCommentColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	if strings.TrimSpace(body) == "" {
		return usageErrorf("--body is required")
	}
	atts, err := qaAttachments(files)
	if err != nil {
		return err
	}
	req := api.QaCommentJSONRequestBody{Body: body}
	if len(atts) > 0 {
		req.Files = &atts
	}
	sess, err := app.Connect(ctx, FeatureQa)
	if err != nil {
		return err
	}
	resp, err := sess.API.QaCommentWithResponse(ctx, key, req)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return qaFailure(resp, resp.Headers429)
	}
	return qaWrite(app, cols, output.Row{"action": "comment", "key": resp.JSON200.TicketKey}, nil)
}

func newQaAckCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "ack <session>",
		Short: "Acknowledge a redeploy on a session",
		Long: "Acknowledge that the session's environment moved to a new generation,\n" +
			"clearing the card's change banner.\n\n" + cliexit.Help,
		Args: exactArgs(1, "the session id"),
		RunE: func(c *cobra.Command, args []string) error {
			return runQaSessionAction(c.Context(), app, "ack", args[0], false)
		},
	}
}

func newQaEndCommand(app *App) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "end <session>",
		Aliases: []string{"close"},
		Short:   "Close a test session",
		Long: "Close a Test Session without a verdict. The server's operation is\n" +
			"`close`; `end` is the name the queue uses for it.\n\n" +
			"A closed session takes no verdict, so the command confirms and takes\n" +
			"--yes.\n\n" + cliexit.Help,
		Args: exactArgs(1, "the session id"),
		RunE: func(c *cobra.Command, args []string) error {
			return runQaSessionAction(c.Context(), app, "end", args[0], yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm without prompting")
	return cmd
}

func runQaSessionAction(ctx context.Context, app *App, action, sessionArg string, yes bool) error {
	cols := qaSessionMutationColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	id, err := uuid.Parse(sessionArg)
	if err != nil {
		return usageErrorf("invalid session id %q: not a UUID", sessionArg)
	}
	sess, err := app.Connect(ctx, FeatureQa)
	if err != nil {
		return err
	}
	if action == "end" {
		if err := app.Confirm(yes,
			fmt.Sprintf("Close QA session %s without a verdict.", id),
			"close this QA session"); err != nil {
			return err
		}
	}
	var view *api.QaSessionView
	if action == "ack" {
		resp, err := sess.API.QaAckWithResponse(ctx, id)
		if err != nil {
			return client.Transport(err, sess.Resolved.Endpoint)
		}
		if resp.JSON200 == nil {
			return qaFailure(resp, resp.Headers429)
		}
		view = resp.JSON200
	} else {
		resp, err := sess.API.QaCloseWithResponse(ctx, id)
		if err != nil {
			return client.Transport(err, sess.Resolved.Endpoint)
		}
		if resp.JSON200 == nil {
			return qaFailure(resp, resp.Headers429)
		}
		view = resp.JSON200
	}
	return qaWrite(app, cols, qaSessionMutationRow(ctx, sess, action, view), qaSessionViewExtra(view))
}

func qaCarryOverColumns() []output.Column {
	return []output.Column{
		{Name: "action", Header: "Action"},
		{Name: "key", Header: "Key"},
		{Name: "carried_count", Header: "Carried"},
		{Name: "evaluations_count", Header: "Evaluations", Wide: true},
	}
}

func newQaCarryOverCommand(app *App) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "carryover <key>",
		Aliases: []string{"carry-over"},
		Short:   "Carry a pre-rollout ticket's open PRs over at their heads",
		Long: "Carry a pre-rollout Ticket's open Linked PRs over at their heads, so\n" +
			"the gate can go green for commits tested before the module existed.\n\n" +
			"Requires the `qa` role and the server's `qa.approve` capability.\n\n" +
			cliexit.Help,
		Args: exactArgs(1, "the ticket key"),
		RunE: func(c *cobra.Command, args []string) error {
			return runQaCarryOver(c.Context(), app, args[0], yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm without prompting")
	return cmd
}

func runQaCarryOver(ctx context.Context, app *App, key string, yes bool) error {
	cols := qaCarryOverColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	sess, err := app.Connect(ctx, FeatureQa, FeatureQaApprove)
	if err != nil {
		return err
	}
	if err := app.Confirm(yes,
		fmt.Sprintf("Carry over every open PR linked to %s at its current head.", key),
		"carry this ticket over"); err != nil {
		return err
	}
	resp, err := sess.API.QaCarryOverWithResponse(ctx, key)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return qaFailure(resp, resp.Headers429)
	}
	out := *resp.JSON200
	carried := make([]map[string]any, 0, len(out.Carried))
	for _, c := range out.Carried {
		carried = append(carried, map[string]any{
			"pull_request_id": c.PullRequestId.String(),
			"sha":             c.Sha,
		})
	}
	return qaWrite(app, cols, output.Row{
		"action":            "carryover",
		"key":               out.TicketKey,
		"carried_count":     len(out.Carried),
		"evaluations_count": len(out.Evaluations),
	}, map[string]any{
		"carried":     carried,
		"evaluations": qaEvaluationPayload(out.Evaluations),
	})
}

// --- resolution -------------------------------------------------------------

// qaTicketCard reads one Ticket, for the commands that must resolve a repo#n
// against ITS linked PRs (start, unlink).
func qaTicketCard(ctx context.Context, sess *Session, key string) (api.QaTicketCard, error) {
	resp, err := sess.API.QaGetWithResponse(ctx, key)
	if err != nil {
		return api.QaTicketCard{}, client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return api.QaTicketCard{}, client.Fail(resp, resp.Headers429)
	}
	return resp.JSON200.Card, nil
}

// resolveTicketPR resolves a repo#n reference (or a PR UUID) against the PRs
// linked to ONE ticket. repo#n names the forge PR and covers every repository
// row of it; a UUID names exactly one row and is accepted only when that row is
// linked here, because the server would accept any PR row and a typo would
// otherwise link or unlink something the operator never saw.
func resolveTicketPR(card api.QaTicketCard, ref string) ([]uuid.UUID, string, error) {
	if id, err := uuid.Parse(strings.TrimSpace(ref)); err == nil {
		for _, p := range card.PullRequests {
			if p.PullRequestId == id {
				return []uuid.UUID{id}, qaPRLabel(p), nil
			}
		}
		return nil, "", usageErrorf("%s is not linked to %s; linked PRs: %s",
			id, card.Key, qaPRList(card.PullRequests))
	}
	repo, number, err := parsePRRef(ref)
	if err != nil {
		return nil, "", err
	}
	matches := qaMatchPRs(qaCardRows(card.Key, card.PullRequests), repo, number)
	switch len(matches) {
	case 0:
		return nil, "", usageErrorf("no linked PR %s on %s; linked PRs: %s",
			ref, card.Key, qaPRList(card.PullRequests))
	case 1:
		return matches[0].Ids, matches[0].Label, nil
	}
	return nil, "", qaAmbiguousPR(ref, matches)
}

// qaCardRows pairs each PR row on a Ticket's card with that Ticket's key.
func qaCardRows(key string, in []api.QaPullRequestCard) []qaQueuePR {
	out := make([]qaQueuePR, 0, len(in))
	for _, p := range in {
		out = append(out, qaQueuePR{Key: key, PR: p})
	}
	return out
}

// resolveQueuedPR resolves repo#n against every PR on the QA queue, for the
// commands that are not scoped to one Ticket (exempt, link). repo#n names the
// forge PR and covers every repository row of it. A UUID names exactly one row
// whether or not that row is still queued, so it is the precise address and the
// retry target for a single row; no queue read is spent on it. `hint` is the
// caller's advice for a repo#n that matched nothing, which differs between the
// two commands. keys are the Tickets whose cards carried the matched rows.
func resolveQueuedPR(ctx context.Context, sess *Session, ref, hint string) ([]uuid.UUID, string, []string, error) {
	if id, err := uuid.Parse(strings.TrimSpace(ref)); err == nil {
		return []uuid.UUID{id}, id.String(), nil, nil
	}
	repo, number, err := parsePRRef(ref)
	if err != nil {
		return nil, "", nil, err
	}
	resp, err := sess.API.QaListWithResponse(ctx)
	if err != nil {
		return nil, "", nil, client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return nil, "", nil, client.Fail(resp, resp.Headers429)
	}
	q := *resp.JSON200
	all := make([]qaQueuePR, 0, 16)
	for _, cards := range [][]api.QaTicketCard{q.ReadyForQa, q.InTesting, q.Recheck, q.Upcoming} {
		for _, c := range cards {
			all = append(all, qaCardRows(c.Key, c.PullRequests)...)
		}
	}
	matches := qaMatchPRs(all, repo, number)
	switch len(matches) {
	case 0:
		return nil, "", nil, usageErrorf(
			"no PR %s on the QA queue; pass its UUID instead%s", ref, hint)
	case 1:
		return matches[0].Ids, matches[0].Label, matches[0].Keys, nil
	}
	return nil, "", nil, qaAmbiguousPR(ref, matches)
}

// qaQueuePR is one PR row read from a Ticket's card, with that Ticket's key.
type qaQueuePR struct {
	Key string
	PR  api.QaPullRequestCard
}

// qaPRMatch is one forge pull request. A monorepo registers each service as its
// own Repository, so the same PR is discovered once per registration and the
// card carries one row per registration: Ids are that one PR's rows, and every
// row has to be acted on because the server's links, exemptions and partitions
// are per row. Keys are the Tickets whose cards carried those rows.
type qaPRMatch struct {
	Label string
	Ids   []uuid.UUID
	Keys  []string
}

// qaAmbiguousPR names the candidates a reference matched. A monorepo PR is ONE
// candidate however many rows it has, so the count is of forge PRs and each
// candidate is named by repository.
func qaAmbiguousPR(ref string, matches []qaPRMatch) error {
	labels := make([]string, 0, len(matches))
	for _, m := range matches {
		labels = append(labels, m.Label)
	}
	return usageErrorf("%s names %d different PRs (%s); name the repository",
		ref, len(matches), strings.Join(labels, ", "))
}

// parsePRRef splits `repo#n`, where repo is `owner/name` or just `name`.
func parsePRRef(ref string) (string, int, error) {
	trimmed := strings.TrimSpace(ref)
	i := strings.LastIndex(trimmed, "#")
	if i <= 0 || i == len(trimmed)-1 {
		return "", 0, usageErrorf("invalid PR reference %q: want repo#n, e.g. acme/widget#42", ref)
	}
	number, err := strconv.Atoi(trimmed[i+1:])
	if err != nil || number <= 0 {
		return "", 0, usageErrorf("invalid PR number in %q: want a positive integer after #", ref)
	}
	return trimmed[:i], number, nil
}

// qaRepoMatches accepts the forge's owner/name or the bare repository name,
// case-insensitively.
func qaRepoMatches(forgeRepo, want string) bool {
	if strings.EqualFold(forgeRepo, want) {
		return true
	}
	if i := strings.LastIndex(forgeRepo, "/"); i >= 0 {
		return strings.EqualFold(forgeRepo[i+1:], want)
	}
	return false
}

func qaPRLabel(p api.QaPullRequestCard) string {
	return fmt.Sprintf("%s#%d", p.ForgeRepo, p.Number)
}

// qaMatchPRs returns the cards matching a repo#n reference, grouped by forge PR
// (its repo and number) and in the order they were read, so an ambiguity
// message lists its candidates the way the server returned them.
func qaMatchPRs(in []qaQueuePR, repo string, number int) []qaPRMatch {
	out := make([]qaPRMatch, 0, 2)
	index := make(map[string]int, 2)
	for _, row := range in {
		p := row.PR
		if p.Number != number || !qaRepoMatches(p.ForgeRepo, repo) {
			continue
		}
		key := strings.ToLower(p.ForgeRepo)
		i, ok := index[key]
		if !ok {
			index[key] = len(out)
			out = append(out, qaPRMatch{
				Label: qaPRLabel(p),
				Ids:   []uuid.UUID{p.PullRequestId},
				Keys:  qaAppendUnique(nil, row.Key),
			})
			continue
		}
		duplicate := false
		for _, id := range out[i].Ids {
			duplicate = duplicate || id == p.PullRequestId
		}
		if !duplicate {
			out[i].Ids = append(out[i].Ids, p.PullRequestId)
		}
		out[i].Keys = qaAppendUnique(out[i].Keys, row.Key)
	}
	return out
}

// qaPRList names a card's linked PRs. A monorepo PR is listed once however
// many rows it has: the label names the forge PR, not a registration.
func qaPRList(in []api.QaPullRequestCard) string {
	if len(in) == 0 {
		return "none"
	}
	labels := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, p := range in {
		label := qaPRLabel(p)
		if seen[label] {
			continue
		}
		seen[label] = true
		labels = append(labels, label)
	}
	return strings.Join(labels, ", ")
}

func qaGateLabel(g *api.QaGateTarget) any {
	if g == nil {
		return nil
	}
	return g.ForgeRepo + "@" + shortSHAString(g.Sha)
}

// --- attachments ------------------------------------------------------------

// qaAttachments reads and encodes --file paths for a verdict or a comment.
//
// The checks mirror the server's: at most ten files and 7 MiB of decoded bytes
// in total, so the base64 body stays inside the mount's 10 MB limit. Doing it
// here names the offending file; the server could only answer 413.
func qaAttachments(paths []string) ([]qaAttachmentJSON, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if len(paths) > qaMaxAttachments {
		return nil, usageErrorf("at most %d --file attachments per request (got %d)",
			qaMaxAttachments, len(paths))
	}
	opened := make([]*os.File, 0, len(paths))
	defer func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}()
	for _, path := range paths {
		// Opening a FIFO or device blocks until a writer appears, so only a
		// regular file is opened; the descriptor is re-checked below in case
		// the path was swapped in between.
		info, err := os.Stat(path)
		if err != nil {
			return nil, usageErrorf("cannot read --file %q: %s", path, err)
		}
		if err := qaRegularFile(info, path); err != nil {
			return nil, err
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, usageErrorf("cannot read --file %q: %s", path, err)
		}
		opened = append(opened, f)
	}

	// The budget comes from the descriptors just opened, and the reads below
	// use those same descriptors: no path is reopened, so a file swapped in
	// between cannot be sized as one thing and sent as another.
	sizes := make([]int64, 0, len(paths))
	total := int64(0)
	for i, f := range opened {
		size, err := qaAttachmentSize(f, paths[i])
		if err != nil {
			return nil, err
		}
		total += size
		if total > qaMaxAttachmentBytes {
			return nil, usageErrorf("attachments total more than %d MiB (the limit is per request)",
				qaMaxAttachmentBytes>>20)
		}
		sizes = append(sizes, size)
	}

	out := make([]qaAttachmentJSON, 0, len(paths))
	for i, path := range paths {
		data, err := qaReadAttachment(opened[i], path, sizes[i])
		if err != nil {
			return nil, err
		}
		contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		out = append(out, qaAttachmentJSON{
			Filename:    filepath.Base(path),
			ContentType: contentType,
			Data:        base64.StdEncoding.EncodeToString(data),
		})
	}
	return out, nil
}

// qaAttachmentSize reports the size qaAttachments will read from f, rejecting
// a path that is not a non-empty file of at most the whole-request budget.
func qaAttachmentSize(f *os.File, path string) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, usageErrorf("cannot read --file %q: %s", path, err)
	}
	if err := qaRegularFile(info, path); err != nil {
		return 0, err
	}
	switch {
	case info.Size() == 0:
		return 0, usageErrorf("--file %q is empty", path)
	case info.Size() > qaMaxAttachmentBytes:
		return 0, usageErrorf("--file %q is %d bytes; the limit is %d MiB per request",
			path, info.Size(), qaMaxAttachmentBytes>>20)
	}
	return info.Size(), nil
}

// qaRegularFile refuses anything but a regular file, naming a directory as one.
func qaRegularFile(info os.FileInfo, path string) error {
	switch {
	case info.IsDir():
		return usageErrorf("--file %q is a directory", path)
	case !info.Mode().IsRegular():
		return usageErrorf("--file %q is not a regular file", path)
	}
	return nil
}

// qaReadAttachment reads at most one byte past the size qaAttachmentSize took
// from this descriptor, and refuses a file that grew under it.
func qaReadAttachment(f *os.File, path string, size int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(f, size+1))
	if err != nil {
		return nil, usageErrorf("cannot read --file %q: %s", path, err)
	}
	if int64(len(data)) > size {
		return nil, usageErrorf("--file %q changed while it was being read", path)
	}
	return data, nil
}

// --- small helpers ----------------------------------------------------------

func uuidStrings(in []uuid.UUID) []string {
	out := make([]string, 0, len(in))
	for _, id := range in {
		out = append(out, id.String())
	}
	return out
}

func shortSHAString(s string) string {
	if len(s) <= 7 {
		return s
	}
	return s[:7]
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
