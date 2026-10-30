package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steadfast-ly/drift-cli/internal/api"
	"github.com/steadfast-ly/drift-cli/internal/client"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
	"github.com/steadfast-ly/drift-cli/internal/output"
)

func newQaCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "qa",
		Short: "Work the QA queue: tickets, test sessions and verdicts",
		Long: "Work the QA queue: the Tickets the tracker places in the ready-for-QA\n" +
			"status, their Linked PRs, and the Test Sessions QA runs on them.\n\n" +
			"`list` and `show` read the queue; `start` opens a session on a Ticket's\n" +
			"environment; `approve` and `reject` record a verdict on the session's\n" +
			"revision and write it back to the tracker. A verdict binds the deploy\n" +
			"generation it was shown (`--revision`), so a redeploy between reading\n" +
			"and deciding is refused with the new revision rather than approved\n" +
			"blindly.\n\n" +
			"Commands that change a verdict or a grant, and removing a link, ask\n" +
			"for confirmation and take --yes.\n\n" +
			"The whole group is gated on the server advertising `qa`, and the\n" +
			"approver actions additionally on `qa.approve`. A deployment without the\n" +
			"module refuses every command here.\n\n" +
			cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(
		newQaListCommand(app),
		newQaShowCommand(app),
		newQaHealthCommand(app),
		newQaStartCommand(app),
		newQaApproveCommand(app),
		newQaRejectCommand(app),
		newQaExemptCommand(app),
		newQaUnexemptCommand(app),
		newQaRevokeCommand(app),
		newQaLinkCommand(app),
		newQaUnlinkCommand(app),
		newQaSendCommand(app),
		newQaCommentCommand(app),
		newQaAckCommand(app),
		newQaEndCommand(app),
		newQaCarryOverCommand(app),
	)
	return cmd
}

// --- list -------------------------------------------------------------------

// qaListColumns is the CLI's stable field contract for a queue entry. The
// section is a column rather than a nested shape so a script can select one
// kind of work with `--json section,key`.
func qaListColumns() []output.Column {
	return []output.Column{
		{Name: "section", Header: "Section"},
		{Name: "key", Header: "Key"},
		{Name: "summary", Header: "Summary"},
		output.StatusColumn("status", "Status"),
		{Name: "prs", Header: "PRs"},
		{Name: "sessions", Header: "Sessions"},
		{Name: "entered", Header: "Entered"},
		{Name: "project", Header: "Project", Wide: true},
		{Name: "completion", Header: "Completion", Wide: true},
		{Name: "url", Header: "URL", Wide: true},
	}
}

func newQaListCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls", "queue"},
		Short:   "List the QA queue",
		Long: "List the QA queue: ready for QA (oldest entry first), in testing,\n" +
			"re-check and upcoming, with the linked PR count and any active\n" +
			"session per Ticket.\n\n" +
			"A section the server capped says how many it held back: a `+N more`\n" +
			"row in the table, `truncated` in the machine formats.\n\n" +
			"Under the sections, `No ticket` lists open pull requests with no live\n" +
			"ticket link and no exemption; `drift qa exempt --pr` and\n" +
			"`drift qa link` resolve them.\n\n" +
			"The carry-over list (pre-rollout Tickets an approver has to decide)\n" +
			"and the recently decided verdicts travel under `carry_over` and\n" +
			"`recently_decided` in the machine formats, the No ticket section\n" +
			"under `no_ticket`.\n\n" +
			cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return runQaList(c.Context(), app) },
	}
}

func runQaList(ctx context.Context, app *App) error {
	cols := qaListColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	sess, err := app.Connect(ctx, FeatureQa)
	if err != nil {
		return err
	}
	resp, err := sess.API.QaListWithResponse(ctx)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return client.Fail(resp, resp.Headers429)
	}
	queue := *resp.JSON200

	sections := []struct {
		name    string
		cards   []api.QaTicketCard
		omitted int
	}{
		{"ready", queue.ReadyForQa, queue.Truncated.ReadyForQa},
		{"testing", queue.InTesting, queue.Truncated.InTesting},
		{"recheck", queue.Recheck, queue.Truncated.Recheck},
		{"upcoming", queue.Upcoming, queue.Truncated.Upcoming},
	}
	format := app.Out.EffectiveFormat()
	rows := make([]output.Row, 0, len(queue.ReadyForQa)+len(queue.InTesting)+len(queue.Recheck)+len(queue.Upcoming))
	for _, s := range sections {
		for _, c := range s.cards {
			rows = append(rows, qaTicketRow(s.name, c))
		}
		// The server caps each section, so what it held back has to be said
		// where the section is: a row under its own entries. The machine
		// formats carry `truncated` instead, so the pseudo-row never reaches
		// them.
		if (format == output.FormatTable || format == output.FormatWide) && s.omitted > 0 {
			rows = append(rows, output.Row{
				"section": s.name,
				"key":     fmt.Sprintf("+%d more", s.omitted),
			})
		}
	}

	noTicket := qaNoTicketItems(queue.NoTicket)
	omittedNoTicket := 0
	if queue.Truncated.NoTicket != nil {
		omittedNoTicket = *queue.Truncated.NoTicket
	}
	noTicketRows := make([]map[string]any, 0, len(noTicket))
	for _, p := range noTicket {
		noTicketRows = append(noTicketRows, qaNoTicketPayload(p))
	}

	var carryOver []api.QaCarryOverCandidate
	if queue.CarryOver != nil {
		carryOver = *queue.CarryOver
	}
	carryRows := make([]map[string]any, 0, len(carryOver))
	for _, c := range carryOver {
		carryRows = append(carryRows, qaCarryOverPayload(c))
	}
	approvals := make([]map[string]any, 0, len(queue.RecentlyDecided.Approvals))
	for _, a := range queue.RecentlyDecided.Approvals {
		approvals = append(approvals, qaApprovalPayload(a))
	}
	exemptions := make([]map[string]any, 0, len(queue.RecentlyDecided.Exemptions))
	for _, e := range queue.RecentlyDecided.Exemptions {
		exemptions = append(exemptions, qaExemptionPayload(e))
	}

	empty := "The QA queue is empty."
	if len(noTicket) > 0 {
		empty = "No Tickets are in the QA queue."
	}
	doc := &output.Doc{
		Columns:      cols,
		Rows:         rows,
		EmptyMessage: empty,
		Extra: map[string]any{
			"truncated": map[string]any{
				"ready":     queue.Truncated.ReadyForQa,
				"testing":   queue.Truncated.InTesting,
				"recheck":   queue.Truncated.Recheck,
				"upcoming":  queue.Truncated.Upcoming,
				"no_ticket": omittedNoTicket,
			},
			"no_ticket":  noTicketRows,
			"carry_over": carryRows,
			"recently_decided": map[string]any{
				"approvals":  approvals,
				"exemptions": exemptions,
			},
		},
	}
	if err := app.Out.Write(doc); err != nil {
		return err
	}

	if format != output.FormatTable && format != output.FormatWide {
		return nil
	}
	if err := writeQaNoTicket(app, noTicket, omittedNoTicket); err != nil {
		return err
	}
	if len(carryOver) == 0 {
		return nil
	}
	fmt.Fprintln(app.Stdout, "\nCARRY-OVER (approvers, until empty)")
	carryCols := []output.Column{
		{Name: "key", Header: "Key"},
		{Name: "summary", Header: "Summary"},
		{Name: "prs", Header: "PRs"},
		{Name: "status", Header: "Status"},
	}
	carryOut := make([]output.Row, 0, len(carryOver))
	for _, c := range carryOver {
		carryOut = append(carryOut, qaCarryOverRow(c))
	}
	return app.Out.Write(&output.Doc{Columns: carryCols, Rows: carryOut})
}

func qaTicketRow(section string, c api.QaTicketCard) output.Row {
	status := string(c.StatusCategory)
	if c.StatusName != nil && *c.StatusName != "" {
		status = *c.StatusName
	}
	states := make([]string, 0, len(c.ActiveSessions))
	for _, s := range c.ActiveSessions {
		states = append(states, string(s.State))
	}
	completion := ""
	if c.Completion != nil {
		completion = string(c.Completion.State)
		if !c.Completion.Current {
			completion += " (stale)"
		}
	}
	return output.Row{
		"section":       section,
		"key":           c.Key,
		"summary":       c.Summary,
		"status":        status,
		"category":      string(c.StatusCategory),
		"prs":           len(c.PullRequests),
		"sessions":      strings.Join(states, ","),
		"entered":       c.EnteredQueueAt,
		"project":       c.ProjectKey,
		"completion":    nilIfEmpty(completion),
		"verdict_epoch": c.VerdictEpoch,
		"url":           c.Url,
	}
}

func qaCarryOverRow(c api.QaCarryOverCandidate) output.Row {
	return output.Row{
		"key":     c.TicketKey,
		"summary": c.Summary,
		"prs":     len(c.PullRequests),
		"status":  c.StatusId,
	}
}

func qaCarryOverPayload(c api.QaCarryOverCandidate) map[string]any {
	prs := make([]map[string]any, 0, len(c.PullRequests))
	for _, pr := range c.PullRequests {
		prs = append(prs, map[string]any{
			"repo":            pr.ForgeRepo,
			"number":          pr.Number,
			"title":           pr.Title,
			"head_sha":        pr.HeadSha,
			"pull_request_id": pr.PullRequestId.String(),
		})
	}
	return map[string]any{
		"key":           c.TicketKey,
		"summary":       c.Summary,
		"status":        c.StatusId,
		"pull_requests": prs,
	}
}

// qaNoTicketItems dereferences the section: a server older than 0.34 does not
// send it, and an absent list is an empty one.
func qaNoTicketItems(in *[]api.QaUnlinkedPullRequest) []api.QaUnlinkedPullRequest {
	if in == nil {
		return nil
	}
	return *in
}

// writeQaNoTicket prints the No ticket section under the Ticket sections. Its
// rows are pull requests, not Tickets, so they carry no key, status or session
// and are printed as their own table.
func writeQaNoTicket(app *App, items []api.QaUnlinkedPullRequest, omitted int) error {
	if len(items) == 0 {
		return nil
	}
	fmt.Fprintln(app.Stdout, "\nNO TICKET (open pull requests with no live ticket link and no exemption)")
	rows := make([]output.Row, 0, len(items)+1)
	for _, p := range items {
		rows = append(rows, qaNoTicketRow(p))
	}
	if omitted > 0 {
		rows = append(rows, output.Row{"pr": fmt.Sprintf("+%d more", omitted)})
	}
	return app.Out.Write(&output.Doc{Columns: qaNoTicketColumns(), Rows: rows})
}

func qaNoTicketColumns() []output.Column {
	return []output.Column{
		{Name: "pr", Header: "PR"},
		{Name: "title", Header: "Title"},
		{Name: "author", Header: "Author"},
		{Name: "updated", Header: "Updated"},
		{Name: "registrations", Header: "Registrations"},
	}
}

func qaNoTicketRow(p api.QaUnlinkedPullRequest) output.Row {
	return output.Row{
		"pr":            fmt.Sprintf("%s#%d", p.ForgeRepo, p.Number),
		"title":         p.Title,
		"author":        p.AuthorLogin,
		"updated":       p.UpdatedAt,
		"registrations": qaRegistrationsLabel(p),
	}
}

// qaRegistrationsLabel says how much of a monorepo PR the section holds: a
// partly linked PR has bare rows among linked ones. A single registration is
// the ordinary case and needs no note.
func qaRegistrationsLabel(p api.QaUnlinkedPullRequest) string {
	switch {
	case p.BlockedRegistrations < p.Registrations:
		return fmt.Sprintf("%d of %d registrations", p.BlockedRegistrations, p.Registrations)
	case p.Registrations > 1:
		return fmt.Sprintf("%d registrations", p.Registrations)
	default:
		return ""
	}
}

// qaBlockedPullRequestIDs names every bare registration, as the server
// reported them, so a script can retry one by UUID.
func qaBlockedPullRequestIDs(p api.QaUnlinkedPullRequest) []string {
	return uuidStrings(p.BlockedPullRequestIds)
}

func qaNoTicketPayload(p api.QaUnlinkedPullRequest) map[string]any {
	return map[string]any{
		"pull_request_id":          p.PullRequestId.String(),
		"blocked_pull_request_ids": qaBlockedPullRequestIDs(p),
		"repository_id":            p.RepositoryId.String(),
		"repo":                     p.ForgeRepo,
		"number":                   p.Number,
		"title":                    p.Title,
		"author":                   p.AuthorLogin,
		"head_branch":              p.HeadBranch,
		"updated":                  p.UpdatedAt,
		"registrations":            p.Registrations,
		"blocked_registrations":    p.BlockedRegistrations,
		"url":                      p.Url,
	}
}

// --- show -------------------------------------------------------------------

func qaShowColumns() []output.Column {
	return []output.Column{
		{Name: "key", Header: "Key"},
		{Name: "summary", Header: "Summary"},
		output.StatusColumn("status", "Status"),
		{Name: "category", Header: "Category"},
		{Name: "prs", Header: "PRs"},
		{Name: "session_count", Header: "Sessions"},
		{Name: "entered", Header: "Entered"},
		{Name: "project", Header: "Project", Wide: true},
		{Name: "completion", Header: "Completion", Wide: true},
		{Name: "verdict_epoch", Header: "Verdict epoch", Wide: true},
		{Name: "url", Header: "URL", Wide: true},
	}
}

func newQaShowCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "show <key>",
		Aliases: []string{"get"},
		Short:   "Show one QA ticket, its linked PRs, sessions and verdicts",
		Long: "Show one QA ticket by tracker key, with its Linked PRs, Test Sessions,\n" +
			"approvals, exemptions, rejections and tracker writes. Each session\n" +
			"carries `verified` (its generation is verified) and `saw_change`.\n\n" +
			"A sub-task's key resolves to no Ticket; use the parent key.\n\n" +
			cliexit.Help,
		Args: exactArgs(1, "the ticket key, e.g. DT-123"),
		RunE: func(c *cobra.Command, args []string) error {
			return runQaShow(c.Context(), app, args[0])
		},
	}
}

func runQaShow(ctx context.Context, app *App, key string) error {
	cols := qaShowColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	sess, err := app.Connect(ctx, FeatureQa)
	if err != nil {
		return err
	}
	resp, err := sess.API.QaGetWithResponse(ctx, key)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return client.Fail(resp, resp.Headers429)
	}
	d := *resp.JSON200
	c := d.Card

	prs := make([]map[string]any, 0, len(c.PullRequests))
	for _, p := range c.PullRequests {
		prs = append(prs, qaPRPayload(p))
	}
	sessions := make([]map[string]any, 0, len(d.Sessions))
	for _, s := range d.Sessions {
		sessions = append(sessions, qaSessionPayload(s))
	}
	approvals := make([]map[string]any, 0, len(d.Approvals))
	for _, a := range d.Approvals {
		approvals = append(approvals, qaApprovalPayload(a))
	}
	exemptions := make([]map[string]any, 0, len(d.Exemptions))
	for _, e := range d.Exemptions {
		exemptions = append(exemptions, qaExemptionPayload(e))
	}
	rejections := make([]map[string]any, 0, len(d.Rejections))
	for _, r := range d.Rejections {
		rejections = append(rejections, qaRejectionPayload(r))
	}
	writes := make([]map[string]any, 0, len(d.TrackerWrites))
	for _, w := range d.TrackerWrites {
		writes = append(writes, qaTrackerWritePayload(w))
	}
	failed := make([]map[string]any, 0, len(c.FailedWrites))
	for _, w := range c.FailedWrites {
		failed = append(failed, map[string]any{
			"id":           w.Id.String(),
			"kind":         string(w.Kind),
			"attempts":     w.Attempts,
			"last_error":   w.LastError,
			"last_outcome": w.LastOutcome,
		})
	}
	partitions := make([]map[string]any, 0, len(c.Partitions))
	for _, p := range c.Partitions {
		prs := make([]string, 0, len(p.PullRequestIds))
		for _, id := range p.PullRequestIds {
			prs = append(prs, id.String())
		}
		var session any
		if p.SessionId != nil {
			session = p.SessionId.String()
		}
		partitions = append(partitions, map[string]any{
			"session":          session,
			"pull_request_ids": prs,
		})
	}

	row := qaTicketRow("", c)
	row["session_count"] = len(d.Sessions)
	doc := &output.Doc{
		Columns: cols,
		Single:  true,
		Rows:    []output.Row{row},
		Extra: map[string]any{
			"pull_requests":  prs,
			"sessions":       sessions,
			"approvals":      approvals,
			"exemptions":     exemptions,
			"rejections":     rejections,
			"tracker_writes": writes,
			"failed_writes":  failed,
			"partitions":     partitions,
		},
	}
	if err := app.Out.Write(doc); err != nil {
		return err
	}

	format := app.Out.EffectiveFormat()
	if format != output.FormatTable && format != output.FormatWide {
		return nil
	}
	if len(c.PullRequests) > 0 {
		fmt.Fprintln(app.Stdout, "\nLINKED PRS")
		if err := app.Out.Write(&output.Doc{Columns: qaPRColumns(), Rows: qaPRRows(c.PullRequests)}); err != nil {
			return err
		}
	}
	if len(d.Sessions) > 0 {
		fmt.Fprintln(app.Stdout, "\nSESSIONS")
		if err := app.Out.Write(&output.Doc{Columns: qaSessionColumns(), Rows: qaSessionRows(d.Sessions)}); err != nil {
			return err
		}
	}
	if changes := qaChangeRows(d.Sessions); len(changes) > 0 {
		fmt.Fprintln(app.Stdout, "\nCHANGES (unacknowledged)")
		if err := app.Out.Write(&output.Doc{Columns: qaChangeColumns(), Rows: changes}); err != nil {
			return err
		}
	}
	if len(d.Approvals) > 0 {
		fmt.Fprintln(app.Stdout, "\nAPPROVALS")
		if err := app.Out.Write(&output.Doc{Columns: qaApprovalColumns(), Rows: qaApprovalRows(d.Approvals)}); err != nil {
			return err
		}
	}
	if len(d.Exemptions) > 0 {
		fmt.Fprintln(app.Stdout, "\nEXEMPTIONS")
		if err := app.Out.Write(&output.Doc{Columns: qaExemptionColumns(), Rows: qaExemptionRows(d.Exemptions)}); err != nil {
			return err
		}
	}
	if len(c.FailedWrites) > 0 {
		app.Out.Warnf("warning: %d tracker write(s) failed; see `-o json` under failed_writes.", len(c.FailedWrites))
	}
	return nil
}

func qaPRColumns() []output.Column {
	return []output.Column{
		{Name: "repo", Header: "Repo"},
		{Name: "pr", Header: "PR"},
		{Name: "title", Header: "Title"},
		{Name: "review", Header: "Review"},
		{Name: "gate", Header: "Gate"},
		{Name: "head", Header: "Head", Wide: true},
		{Name: "approved", Header: "Approved", Wide: true},
		{Name: "exempt", Header: "Exempt", Wide: true},
	}
}

func qaPRRows(in []api.QaPullRequestCard) []output.Row {
	out := make([]output.Row, 0, len(in))
	for _, p := range in {
		var gate any
		if p.Gate != nil {
			gate = string(p.Gate.State)
		}
		approved := "no"
		switch {
		case p.ApprovedAtHead:
			approved = "head"
		case p.ApprovedAtOtherHead:
			approved = "other head"
		}
		if p.CarriedAtHead {
			approved = "carried"
		}
		out = append(out, output.Row{
			"repo":     p.ForgeRepo,
			"pr":       p.Number,
			"title":    p.Title,
			"review":   string(p.ReviewState),
			"gate":     gate,
			"head":     shortSHA(&p.HeadSha),
			"approved": approved,
			"exempt":   p.ExemptReason,
		})
	}
	return out
}

func qaPRPayload(p api.QaPullRequestCard) map[string]any {
	m := map[string]any{
		"repo":                   p.ForgeRepo,
		"number":                 p.Number,
		"title":                  p.Title,
		"head_branch":            p.HeadBranch,
		"head_sha":               p.HeadSha,
		"merge_head_sha":         p.MergeHeadSha,
		"review_state":           string(p.ReviewState),
		"open":                   p.IsOpen,
		"merged_at":              p.MergedAt,
		"merged_before_qa":       p.MergedBeforeQa,
		"approved_at_head":       p.ApprovedAtHead,
		"approved_at_other_head": p.ApprovedAtOtherHead,
		"carried_at_head":        p.CarriedAtHead,
		"exempt_reason":          p.ExemptReason,
		"url":                    p.Url,
		"pull_request_id":        p.PullRequestId.String(),
		"repository_id":          p.RepositoryId.String(),
	}
	if p.Gate != nil {
		m["gate"] = map[string]any{
			"state":       string(p.Gate.State),
			"description": p.Gate.Description,
			"target_url":  p.Gate.TargetUrl,
		}
	}
	return m
}

func qaSessionColumns() []output.Column {
	return []output.Column{
		{Name: "id", Header: "Session"},
		output.StatusColumn("state", "State"),
		{Name: "owner", Header: "Owner"},
		{Name: "environment", Header: "Environment"},
		{Name: "generation", Header: "Generation"},
		{Name: "verified", Header: "Verified"},
		{Name: "saw_change", Header: "Saw change"},
		{Name: "started", Header: "Started"},
		{Name: "ended", Header: "Ended", Wide: true},
		{Name: "end_reason", Header: "End reason", Wide: true},
		{Name: "change", Header: "Change", Wide: true},
	}
}

func qaSessionRows(in []api.QaSessionDetail) []output.Row {
	out := make([]output.Row, 0, len(in))
	for _, s := range in {
		var endReason any
		if s.EndReason != nil {
			endReason = string(*s.EndReason)
		}
		out = append(out, output.Row{
			"id":          s.Id.String(),
			"state":       string(s.State),
			"owner":       s.Owner,
			"environment": s.EnvironmentSlug,
			"generation":  s.CurrentGeneration,
			"verified":    s.CurrentGenerationVerified,
			"saw_change":  s.SawChange,
			"started":     s.StartedAt,
			"ended":       s.EndedAt,
			"end_reason":  endReason,
			"change":      s.UnacknowledgedChange,
		})
	}
	return out
}

func qaSessionPayload(s api.QaSessionDetail) map[string]any {
	ids := make([]string, 0, len(s.PullRequestIds))
	for _, id := range s.PullRequestIds {
		ids = append(ids, id.String())
	}
	timeline := make([]map[string]any, 0, len(s.Events))
	for _, e := range s.Events {
		timeline = append(timeline, qaEventPayload(e))
	}
	m := map[string]any{
		"id":                    s.Id.String(),
		"key":                   s.TicketKey,
		"state":                 string(s.State),
		"owner":                 s.Owner,
		"environment":           s.EnvironmentSlug,
		"environment_id":        s.EnvironmentId,
		"start_generation":      s.StartGeneration,
		"current_generation":    s.CurrentGeneration,
		"verified":              s.CurrentGenerationVerified,
		"saw_change":            s.SawChange,
		"started_at":            s.StartedAt,
		"ended_at":              s.EndedAt,
		"end_reason":            s.EndReason,
		"unacknowledged_change": s.UnacknowledgedChange,
		"pull_request_ids":      ids,
		"events":                timeline,
	}
	return m
}

func qaEventPayload(e api.QaSessionEvent) map[string]any {
	return map[string]any{
		"id":         e.Id.String(),
		"at":         e.At,
		"generation": e.Generation,
		"kind":       string(e.Kind),
		"before":     qaServicesPayload(e.Before),
		"after":      qaServicesPayload(e.After),
	}
}

// qaServicesPayload mirrors the wire's null-versus-list distinction: a
// migration-source or redeploy event the server could not resolve carries no
// service list at all.
func qaServicesPayload(in *[]api.QaDeploymentService) any {
	if in == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(*in))
	for _, s := range *in {
		out = append(out, map[string]any{
			"repository_id": s.RepositoryId.String(),
			"branch":        s.Branch,
			"commit_sha":    s.CommitSha,
			"image_tag":     s.ImageTag,
		})
	}
	return out
}

// qaChangeColumns is the timeline's human view: what changed since the last
// acknowledgement, one row per event.
func qaChangeColumns() []output.Column {
	return []output.Column{
		{Name: "session", Header: "Session"},
		{Name: "kind", Header: "Change"},
		{Name: "generation", Header: "Generation"},
		{Name: "detail", Header: "Detail", Wide: true},
	}
}

// qaChangeRows is what still needs a look: the events after a session's last
// acknowledgement, and only for a session the server flags and that is still
// open. An ended session's timeline is history — there is no revision left to
// decide against — so it never raises the banner.
func qaChangeRows(in []api.QaSessionDetail) []output.Row {
	out := make([]output.Row, 0)
	for _, s := range in {
		if !s.UnacknowledgedChange || s.State != api.QaSessionDetailStateOpen {
			continue
		}
		events := qaUnacknowledgedEvents(s.Events)
		if len(events) == 0 {
			// The flag is the server's and the timeline can lag it, so a flagged
			// session with nothing after the last ack still needs a look.
			out = append(out, output.Row{
				"session":    s.Id.String(),
				"kind":       "unacknowledged",
				"generation": s.CurrentGeneration,
				"detail":     "no event after the last ack; re-read the environment",
			})
			continue
		}
		for _, e := range events {
			out = append(out, output.Row{
				"session":    s.Id.String(),
				"kind":       string(e.Kind),
				"generation": e.Generation,
				"detail":     nilIfEmpty(qaEventDetail(e)),
			})
		}
	}
	return out
}

// qaUnacknowledgedEvents is the session's timeline after its last
// acknowledgement: `ack` appends an `acknowledged` event and clears the flag,
// so everything after that marker is what the operator still has to look at.
func qaUnacknowledgedEvents(in []api.QaSessionEvent) []api.QaSessionEvent {
	last := -1
	for i, e := range in {
		if e.Kind == api.QaSessionEventKindAcknowledged {
			last = i
		}
	}
	return in[last+1:]
}

// qaEventDetail renders a change for the table: a redeploy shows the
// deployment it moved from and to, since that is what a verdict binds.
func qaEventDetail(e api.QaSessionEvent) string {
	switch e.Kind {
	case api.QaSessionEventKindRedeploy:
		return qaServicesLabel(e.Before) + " -> " + qaServicesLabel(e.After)
	case api.QaSessionEventKindMigrationSourceChanged:
		return "migration source moved"
	case api.QaSessionEventKindUnverified:
		return "deployment not verified"
	default:
		return ""
	}
}

// qaServicesLabel is one side of a redeploy, as `branch@sha` per service.
func qaServicesLabel(in *[]api.QaDeploymentService) string {
	if in == nil || len(*in) == 0 {
		return "-"
	}
	labels := make([]string, 0, len(*in))
	for _, s := range *in {
		label := s.Branch
		if sha := derefString(s.CommitSha); sha != "" {
			label += "@" + shortSHAString(sha)
		}
		if s.ImageTag != "" {
			label += " (" + s.ImageTag + ")"
		}
		labels = append(labels, label)
	}
	return strings.Join(labels, ", ")
}

func qaApprovalColumns() []output.Column {
	return []output.Column{
		{Name: "id", Header: "Approval"},
		{Name: "approver", Header: "Approver"},
		{Name: "at", Header: "At"},
		{Name: "generation", Header: "Generation"},
		{Name: "retested", Header: "Retested"},
		{Name: "revoked", Header: "Revoked"},
	}
}

func qaApprovalRows(in []api.QaApprovalDetail) []output.Row {
	out := make([]output.Row, 0, len(in))
	for _, a := range in {
		out = append(out, output.Row{
			"id":         a.Id.String(),
			"approver":   a.Approver,
			"at":         a.At,
			"generation": a.Generation,
			"retested":   a.Retested,
			"revoked":    a.RevokedAt,
		})
	}
	return out
}

func qaApprovalPayload(a api.QaApprovalDetail) map[string]any {
	prs := make([]map[string]any, 0, len(a.PullRequests))
	for _, p := range a.PullRequests {
		prs = append(prs, map[string]any{
			"repo":            p.ForgeRepo,
			"number":          p.Number,
			"sha":             p.Sha,
			"pull_request_id": p.PullRequestId.String(),
		})
	}
	return map[string]any{
		"id":               a.Id.String(),
		"key":              a.TicketKey,
		"approver":         a.Approver,
		"at":               a.At,
		"generation":       a.Generation,
		"retested":         a.Retested,
		"no_retest_reason": a.NoRetestReason,
		"note":             a.Note,
		"pull_requests":    prs,
		"revoked_at":       a.RevokedAt,
		"revoked_by":       a.RevokedBy,
		"revoke_reason":    a.RevokeReason,
	}
}

func qaExemptionColumns() []output.Column {
	return []output.Column{
		{Name: "id", Header: "Exemption"},
		{Name: "scope", Header: "Scope"},
		{Name: "reason", Header: "Reason"},
		{Name: "granted_by", Header: "Granted by"},
		{Name: "auto", Header: "Auto"},
		{Name: "revoked", Header: "Revoked"},
	}
}

func qaExemptionRows(in []api.QaExemptionDetail) []output.Row {
	out := make([]output.Row, 0, len(in))
	for _, e := range in {
		out = append(out, output.Row{
			"id":         e.Id.String(),
			"scope":      string(e.Scope),
			"reason":     e.Reason,
			"granted_by": e.GrantedBy,
			"auto":       e.GrantedAuto,
			"revoked":    e.RevokedAt,
		})
	}
	return out
}

func qaExemptionPayload(e api.QaExemptionDetail) map[string]any {
	m := map[string]any{
		"id":           e.Id.String(),
		"scope":        string(e.Scope),
		"reason":       e.Reason,
		"granted_by":   e.GrantedBy,
		"granted_auto": e.GrantedAuto,
		"at":           e.At,
		"ticket_key":   e.TicketKey,
		"forge_repo":   e.ForgeRepo,
		"number":       e.Number,
		"revoked_at":   e.RevokedAt,
		"revoked_by":   e.RevokedBy,
	}
	if e.PullRequestId != nil {
		m["pull_request_id"] = e.PullRequestId.String()
	}
	return m
}

func qaRejectionPayload(r api.QaRejectionDetail) map[string]any {
	prs := make([]map[string]any, 0, len(r.PullRequests))
	for _, p := range r.PullRequests {
		prs = append(prs, map[string]any{
			"repo":            p.ForgeRepo,
			"number":          p.Number,
			"sha":             p.Sha,
			"pull_request_id": p.PullRequestId.String(),
			"retired_at":      p.RetiredAt,
			"retired_reason":  p.RetiredReason,
		})
	}
	return map[string]any{
		"id":            r.Id.String(),
		"at":            r.At,
		"rejected_by":   r.RejectedBy,
		"comment":       r.Comment,
		"pull_requests": prs,
	}
}

func qaTrackerWritePayload(w api.QaTrackerWriteDetail) map[string]any {
	return map[string]any{
		"id":              w.Id.String(),
		"kind":            string(w.Kind),
		"state":           string(w.State),
		"seq":             w.Seq,
		"epoch":           w.Epoch,
		"attempts":        w.Attempts,
		"last_outcome":    w.LastOutcome,
		"last_error":      w.LastError,
		"next_attempt_at": w.NextAttemptAt,
		"sent_at":         w.SentAt,
		"completed_at":    w.CompletedAt,
	}
}

// --- health -----------------------------------------------------------------

func qaHealthColumns() []output.Column {
	return []output.Column{
		{Name: "component", Header: "Component"},
		output.StatusColumn("state", "State"),
		{Name: "last_success", Header: "Last success"},
		{Name: "last_error", Header: "Last error"},
		{Name: "detail", Header: "Detail"},
	}
}

func newQaHealthCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Show QA sync, outbox and tracker health",
		Long: "Show the QA module's own health: the tracker, each sync phase, the\n" +
			"tracker outbox and any Ticket whose completion evaluation keeps\n" +
			"failing. Intended for operators; a verdict command failing is usually\n" +
			"explained by a line here.\n\n" + cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return runQaHealth(c.Context(), app) },
	}
}

func runQaHealth(ctx context.Context, app *App) error {
	cols := qaHealthColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	sess, err := app.Connect(ctx, FeatureQa)
	if err != nil {
		return err
	}
	resp, err := sess.API.QaHealthWithResponse(ctx)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return client.Fail(resp, resp.Headers429)
	}
	h := *resp.JSON200

	rows := make([]output.Row, 0, len(h.Phases)+2)
	if h.Tracker != nil {
		rows = append(rows, output.Row{
			"component":    "tracker",
			"state":        qaHealthState(h.Tracker.LastError),
			"last_success": h.Tracker.LastSuccessAt,
			"last_error":   h.Tracker.LastError,
			"detail":       nil,
		})
	}
	for _, p := range h.Phases {
		detail := ""
		if p.Cursor != nil && *p.Cursor != "" {
			detail = "cursor " + *p.Cursor
		}
		rows = append(rows, output.Row{
			"component":    "phase " + p.Phase,
			"state":        qaHealthState(p.LastError),
			"last_success": p.LastSuccessAt,
			"last_error":   p.LastError,
			"detail":       nilIfEmpty(detail),
		})
	}
	outboxState := "ok"
	if h.Outbox.Failed > 0 {
		outboxState = "fail"
	}
	rows = append(rows, output.Row{
		"component":    "outbox",
		"state":        outboxState,
		"last_success": nil,
		"last_error":   nil,
		"detail": fmt.Sprintf("pending %d, claimed %d, failed %d",
			h.Outbox.Pending, h.Outbox.Claimed, h.Outbox.Failed),
	})
	if len(h.EvaluationAlerts) > 0 {
		keys := make([]string, 0, len(h.EvaluationAlerts))
		for _, a := range h.EvaluationAlerts {
			keys = append(keys, fmt.Sprintf("%s (%d)", a.TicketKey, a.Attempts))
		}
		rows = append(rows, output.Row{
			"component": "evaluation",
			"state":     "warn",
			"detail":    strings.Join(keys, ", "),
		})
	}

	phases := make([]map[string]any, 0, len(h.Phases))
	for _, p := range h.Phases {
		phases = append(phases, map[string]any{
			"phase":           p.Phase,
			"last_success_at": p.LastSuccessAt,
			"last_error":      p.LastError,
			"cursor":          p.Cursor,
			"claimed_until":   p.ClaimedUntil,
		})
	}
	alerts := make([]map[string]any, 0, len(h.EvaluationAlerts))
	for _, a := range h.EvaluationAlerts {
		alerts = append(alerts, map[string]any{"key": a.TicketKey, "attempts": a.Attempts})
	}
	extra := map[string]any{
		"outbox": map[string]any{
			"pending": h.Outbox.Pending,
			"claimed": h.Outbox.Claimed,
			"failed":  h.Outbox.Failed,
		},
		"phases":            phases,
		"evaluation_alerts": alerts,
	}
	if h.Tracker != nil {
		extra["tracker"] = map[string]any{
			"last_success_at": h.Tracker.LastSuccessAt,
			"last_error":      h.Tracker.LastError,
		}
	}
	return app.Out.Write(&output.Doc{
		Columns:      cols,
		Rows:         rows,
		Extra:        extra,
		EmptyMessage: "No QA health data.",
	})
}

func qaHealthState(lastErr *string) string {
	if lastErr != nil && *lastErr != "" {
		return "fail"
	}
	return "ok"
}
