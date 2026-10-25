package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/steadfast-ly/drift-cli/internal/api"
	"github.com/steadfast-ly/drift-cli/internal/client"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
	"github.com/steadfast-ly/drift-cli/internal/output"
)

// The server's attachment caps (docs/qa-module.md §6), mirrored client-side so
// an oversized verdict fails with a sentence naming the flag instead of a 413
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
func qaWrite(app *App, cols []output.Column, row output.Row, extra map[string]any) error {
	return app.Out.Write(&output.Doc{Columns: cols, Single: true, Rows: []output.Row{row}, Extra: extra})
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
			"Ticket's Linked PRs; omitted, the server takes the first partition\n" +
			"with no open session. --env reuses an existing environment instead of\n" +
			"the server's choice.\n\n" +
			"A rebuild of a developer's environment can happen under them, so the\n" +
			"command confirms and takes --yes.\n\n" +
			cliexit.Help,
		Args: exactArgs(1, "the ticket key, e.g. DT-123"),
		RunE: func(c *cobra.Command, args []string) error {
			return runQaStart(c.Context(), app, args[0], prs, envSlug, yes)
		},
	}
	cmd.Flags().StringSliceVar(&prs, "prs", nil,
		"linked PRs to test, as repo#n (comma-separated or repeatable)")
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
		detail, err := qaTicketDetail(ctx, sess, key)
		if err != nil {
			return err
		}
		ids := make([]uuid.UUID, 0, len(prRefs))
		labels := make([]string, 0, len(prRefs))
		for _, ref := range prRefs {
			id, label, err := resolveTicketPR(detail.Card, ref)
			if err != nil {
				return err
			}
			ids = append(ids, id)
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
		{Name: "covered", Header: "Covered"},
		{Name: "uncovered", Header: "Uncovered"},
		{Name: "evaluations", Header: "Evaluations", Wide: true},
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
	retestSet := c.Flags().Changed("retested")
	noRetestSet := c.Flags().Changed("no-retest")
	switch {
	case retestSet && noRetestSet:
		return usageErrorf("--retested and --no-retest contradict each other")
	case !retestSet && !noRetestSet:
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
		"action":      "approve",
		"key":         out.TicketKey,
		"approval":    out.ApprovalId.String(),
		"covered":     len(out.Covered),
		"uncovered":   len(out.Uncovered),
		"evaluations": len(out.Evaluations),
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
		{Name: "covered", Header: "Covered"},
		{Name: "uncovered", Header: "Uncovered"},
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
		"action":    "reject",
		"key":       out.TicketKey,
		"rejection": out.RejectionId.String(),
		"covered":   len(out.Covered),
		"uncovered": len(out.Uncovered),
		"revoked":   len(out.RevokedApprovalIds),
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
			"--pr resolves a repo#n reference against the QA queue. A PR that is no\n" +
			"longer on a queued Ticket has to be named by its UUID instead.\n\n" +
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
	summary := ""
	if prRef != "" {
		id, label, err := resolveQueuedPR(ctx, sess, prRef)
		if err != nil {
			return err
		}
		scope := api.QaExemptJSONBodyScopePr
		body.Scope = scope
		body.PullRequestId = &id
		summary = fmt.Sprintf("Exempt %s from QA: %s", label, reason)
	} else {
		body.Scope = api.QaExemptJSONBodyScopeTicket
		body.TicketKey = &ticketKey
		summary = fmt.Sprintf("Exempt every PR linked to %s from QA: %s", ticketKey, reason)
	}
	if err := app.Confirm(yes, summary, "record this exemption"); err != nil {
		return err
	}

	resp, err := sess.API.QaExemptWithResponse(ctx, body)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return qaFailure(resp, resp.Headers429)
	}
	out := *resp.JSON200
	gate := ""
	if out.GateTarget != nil {
		gate = out.GateTarget.ForgeRepo + "@" + shortSHAString(out.GateTarget.Sha)
	}
	return qaWrite(app, cols, output.Row{
		"action":    "exempt",
		"exemption": out.ExemptionId.String(),
		"scope":     string(out.Scope),
		"keys":      out.TicketKeys,
		"gate":      nilIfEmpty(gate),
	}, map[string]any{
		"gate":        qaGateExtra(out.GateTarget),
		"evaluations": qaEvaluationPayload(out.Evaluations),
	})
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
		"gate":                 qaGateExtra(out.GateTarget),
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
			"the QA queue; a PR that is not on any queued Ticket has to be named by\n" +
			"its UUID instead.\n\n" +
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
	id, label, err := resolveQueuedPR(ctx, sess, prRef)
	if err != nil {
		return err
	}
	resp, err := sess.API.QaLinkWithResponse(ctx, api.QaLinkJSONRequestBody{TicketKey: key, PullRequestId: id})
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return qaFailure(resp, resp.Headers429)
	}
	out := *resp.JSON200
	return qaWrite(app, cols, output.Row{
		"action": "link",
		"key":    out.TicketKey,
		"pr":     label,
		"result": string(out.Result),
	}, map[string]any{
		"pull_request_id": out.PullRequestId.String(),
		"evaluations":     qaEvaluationPayload(out.Evaluations),
	})
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
			"linked to that Ticket; repo#n or its UUID names it.\n\n" +
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
	detail, err := qaTicketDetail(ctx, sess, key)
	if err != nil {
		return err
	}
	id, label, err := resolveTicketPR(detail.Card, prRef)
	if err != nil {
		return err
	}
	if err := app.Confirm(yes,
		fmt.Sprintf("Unlink %s from %s; its QA approval no longer covers that PR.", label, key),
		"unlink this pull request"); err != nil {
		return err
	}
	resp, err := sess.API.QaUnlinkWithResponse(ctx, api.QaUnlinkJSONRequestBody{TicketKey: key, PullRequestId: id})
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return qaFailure(resp, resp.Headers429)
	}
	out := *resp.JSON200
	return qaWrite(app, cols, output.Row{
		"action": "unlink",
		"key":    out.TicketKey,
		"repo":   out.ForgeRepo,
		"pr":     label,
		"sha":    shortSHAString(out.Sha),
	}, map[string]any{
		"pull_request_id": id.String(),
		"sha":             out.Sha,
		"gate":            qaGateExtra(&out.GateTarget),
		"evaluations":     qaEvaluationPayload(out.Evaluations),
	})
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
			return runQaSessionAction(c.Context(), app, "ack", args[0])
		},
	}
}

func newQaEndCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "end <session>",
		Aliases: []string{"close"},
		Short:   "Close a test session",
		Long: "Close a Test Session without a verdict. The server's operation is\n" +
			"`close`; `end` is the name the queue uses for it.\n\n" + cliexit.Help,
		Args: exactArgs(1, "the session id"),
		RunE: func(c *cobra.Command, args []string) error {
			return runQaSessionAction(c.Context(), app, "end", args[0])
		},
	}
}

func runQaSessionAction(ctx context.Context, app *App, action, sessionArg string) error {
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
		{Name: "carried", Header: "Carried"},
		{Name: "evaluations", Header: "Evaluations", Wide: true},
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
		"action":      "carryover",
		"key":         out.TicketKey,
		"carried":     len(out.Carried),
		"evaluations": len(out.Evaluations),
	}, map[string]any{
		"carried":     carried,
		"evaluations": qaEvaluationPayload(out.Evaluations),
	})
}

// --- resolution -------------------------------------------------------------

// qaTicketDetail reads one Ticket, for the commands that must resolve a repo#n
// against ITS linked PRs (start, unlink).
func qaTicketDetail(ctx context.Context, sess *Session, key string) (*api.QaTicketDetail, error) {
	resp, err := sess.API.QaGetWithResponse(ctx, key)
	if err != nil {
		return nil, client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return nil, client.Fail(resp, resp.Headers429)
	}
	return resp.JSON200, nil
}

// resolveTicketPR resolves a repo#n reference (or a PR UUID) against the PRs
// linked to ONE ticket. A UUID is accepted only when it names one of them: the
// server would accept any PR row, and a typo would otherwise link or unlink
// something the operator never saw.
func resolveTicketPR(card api.QaTicketCard, ref string) (uuid.UUID, string, error) {
	if id, err := uuid.Parse(strings.TrimSpace(ref)); err == nil {
		for _, p := range card.PullRequests {
			if p.PullRequestId == id {
				return id, qaPRLabel(p), nil
			}
		}
		return uuid.Nil, "", usageErrorf("%s is not linked to %s; linked PRs: %s",
			id, card.Key, qaPRList(card.PullRequests))
	}
	repo, number, err := parsePRRef(ref)
	if err != nil {
		return uuid.Nil, "", err
	}
	matches := qaMatchPRs(card.PullRequests, repo, number)
	switch len(matches) {
	case 0:
		return uuid.Nil, "", usageErrorf("no linked PR %s on %s; linked PRs: %s",
			ref, card.Key, qaPRList(card.PullRequests))
	case 1:
		return matches[0].PullRequestId, qaPRLabel(matches[0]), nil
	}
	ids := make([]string, 0, len(matches))
	for _, p := range matches {
		ids = append(ids, p.PullRequestId.String())
	}
	return uuid.Nil, "", usageErrorf("%s names %d different PRs (%s); pass the UUID",
		ref, len(matches), strings.Join(ids, ", "))
}

// resolveQueuedPR resolves repo#n against every PR on the QA queue, for the
// commands that are not scoped to one Ticket (exempt, link). A PR that is on no
// queued Ticket is not reachable through the QA reads, so its UUID is accepted
// verbatim and the help says so.
func resolveQueuedPR(ctx context.Context, sess *Session, ref string) (uuid.UUID, string, error) {
	if id, err := uuid.Parse(strings.TrimSpace(ref)); err == nil {
		return id, id.String(), nil
	}
	repo, number, err := parsePRRef(ref)
	if err != nil {
		return uuid.Nil, "", err
	}
	resp, err := sess.API.QaListWithResponse(ctx)
	if err != nil {
		return uuid.Nil, "", client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return uuid.Nil, "", client.Fail(resp, resp.Headers429)
	}
	q := *resp.JSON200
	all := make([]api.QaPullRequestCard, 0, 16)
	for _, cards := range [][]api.QaTicketCard{q.ReadyForQa, q.InTesting, q.Recheck, q.Upcoming} {
		for _, c := range cards {
			all = append(all, c.PullRequests...)
		}
	}
	matches := qaMatchPRs(all, repo, number)
	switch len(matches) {
	case 0:
		return uuid.Nil, "", usageErrorf(
			"no PR %s on the QA queue; pass its UUID instead, or use --ticket for a ticket-scoped exemption", ref)
	case 1:
		return matches[0].PullRequestId, qaPRLabel(matches[0]), nil
	}
	ids := make([]string, 0, len(matches))
	for _, p := range matches {
		ids = append(ids, p.PullRequestId.String())
	}
	return uuid.Nil, "", usageErrorf("%s names %d different PRs (%s); pass the UUID",
		ref, len(matches), strings.Join(ids, ", "))
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

// qaMatchPRs returns the cards matching a repo#n reference, deduplicated and in
// the order they were read, so an ambiguity message lists its candidates the
// way the server returned them.
func qaMatchPRs(in []api.QaPullRequestCard, repo string, number int) []api.QaPullRequestCard {
	out := make([]api.QaPullRequestCard, 0, 2)
	seen := make(map[uuid.UUID]bool, 2)
	for _, p := range in {
		if p.Number != number || !qaRepoMatches(p.ForgeRepo, repo) || seen[p.PullRequestId] {
			continue
		}
		seen[p.PullRequestId] = true
		out = append(out, p)
	}
	return out
}

func qaPRList(in []api.QaPullRequestCard) string {
	if len(in) == 0 {
		return "none"
	}
	labels := make([]string, 0, len(in))
	for _, p := range in {
		labels = append(labels, qaPRLabel(p))
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
		return nil, usageErrorf("at most %d --file attachments per verdict (got %d)",
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
			return nil, usageErrorf("attachments total more than %d MiB (limit is per verdict)",
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
// a path that is not a non-empty file of at most the whole-verdict budget.
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
		return 0, usageErrorf("--file %q is %d bytes; the limit is %d MiB per verdict",
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
