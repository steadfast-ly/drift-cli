package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/steadfast-ly/drift-cli/internal/api"
	"github.com/steadfast-ly/drift-cli/internal/client"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
	"github.com/steadfast-ly/drift-cli/internal/output"
)

func newAuditCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "audit",
		Aliases: []string{"audit-log"},
		Short:   "Query the audit log",
		Long: "Query the audit log.\n\n" +
			"Every state-changing action drift records — environment lifecycle,\n" +
			"promotions, repository registration — appears here with actor,\n" +
			"timestamp, and associated resource ids.\n\n" +
			cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(
		newAuditListCommand(app),
		newAuditActorsCommand(app),
	)
	return cmd
}

func auditColumns() []output.Column {
	return []output.Column{
		{Name: "timestamp", Header: "Timestamp"},
		{Name: "action", Header: "Action"},
		{Name: "actor", Header: "Actor"},
		{Name: "environmentId", Header: "Environment"},
		{Name: "repositoryId", Header: "Repository"},
		{Name: "details", Header: "Details", Wide: true},
	}
}

func auditRow(e api.AuditLogEntry) output.Row {
	row := output.Row{
		"timestamp":     e.Timestamp,
		"action":        e.Action,
		"actor":         e.Actor,
		"environmentId": uuidPtr(e.EnvironmentId),
		"repositoryId":  uuidPtr(e.RepositoryId),
	}
	if e.Details != nil {
		b, err := json.Marshal(*e.Details)
		if err == nil {
			row["details"] = string(b)
		}
	}
	return row
}

// uuidPtr formats an optional UUID pointer for display.
func uuidPtr(u *uuid.UUID) any {
	if u == nil {
		return nil
	}
	return u.String()
}

func newAuditListCommand(app *App) *cobra.Command {
	var (
		action      string
		actor       string
		environment string
		since       string
		until       string
		sortBy      string
		sortDir     string
		limit       int
		offset      int
	)

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List audit-log entries",
		Long: "List audit-log entries with the same filters the web UI offers.\n\n" +
			"`--action` is an exact match (e.g. `environment.created`);\n" +
			"`--actor` is a substring match (e.g. `alice`);\n" +
			"`--since` / `--until` accept RFC 3339 or a bare date (2026-08-01).\n\n" +
			"Paginated server-side: --limit is capped at 50 by the contract and\n" +
			"--offset walks the pages.\n\n" +
			cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runAuditList(c.Context(), app, auditListFlags{
				action: action, actor: actor, environment: environment,
				since: since, until: until, sortBy: sortBy, sortDir: sortDir,
				limit: limit, offset: offset,
			})
		},
	}
	cmd.Flags().StringVar(&action, "action", "", "filter by action (exact match)")
	cmd.Flags().StringVar(&actor, "actor", "", "filter by actor (substring)")
	cmd.Flags().StringVar(&environment, "environment", "", "filter by environment UUID")
	cmd.Flags().StringVar(&since, "since", "", "entries at or after this time (RFC 3339 or date)")
	cmd.Flags().StringVar(&until, "until", "", "entries at or before this time (RFC 3339 or date)")
	cmd.Flags().StringVar(&sortBy, "sort", "", "sort field: timestamp, action, actor")
	cmd.Flags().StringVar(&sortDir, "sort-dir", "", "sort direction: asc, desc")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum number of entries to return (server default 20, max 50)")
	cmd.Flags().IntVar(&offset, "offset", 0, "number of entries to skip")
	return cmd
}

type auditListFlags struct {
	action, actor, environment string
	since, until               string
	sortBy, sortDir            string
	limit, offset              int
}

func runAuditList(ctx context.Context, app *App, f auditListFlags) error {
	cols := auditColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	if err := validatePage(f.limit, f.offset); err != nil {
		return err
	}

	params := &api.AuditListParams{}
	if f.limit > 0 {
		params.Limit = &f.limit
	}
	if f.offset > 0 {
		params.Offset = &f.offset
	}
	if f.action != "" {
		params.Action = &f.action
	}
	if f.actor != "" {
		params.Actor = &f.actor
	}
	if f.environment != "" {
		envID, err := uuid.Parse(f.environment)
		if err != nil {
			return usageErrorf("invalid environment id %q: not a UUID", f.environment)
		}
		params.EnvironmentId = &envID
	}
	if f.sortBy != "" {
		sb := api.AuditListParamsSortBy(f.sortBy)
		if !sb.Valid() {
			return usageErrorf("unknown sort field %q; valid: timestamp, action, actor", f.sortBy)
		}
		params.SortBy = &sb
	}
	if f.sortDir != "" {
		sd := api.AuditListParamsSortDir(strings.ToLower(f.sortDir))
		if !sd.Valid() {
			return usageErrorf("unknown sort direction %q; valid: asc, desc", f.sortDir)
		}
		params.SortDir = &sd
	}
	if f.since != "" {
		t, err := parseDateTime(f.since)
		if err != nil {
			return usageErrorf("invalid --since: %s", err.Error())
		}
		params.StartDate = &t
	}
	if f.until != "" {
		t, err := parseDateTime(f.until)
		if err != nil {
			return usageErrorf("invalid --until: %s", err.Error())
		}
		params.EndDate = &t
	}

	sess, err := app.Connect(ctx, FeatureAuditLogRead)
	if err != nil {
		return err
	}

	resp, err := sess.API.AuditListWithResponse(ctx, params)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return client.Fail(resp, resp.Headers429)
	}

	page := *resp.JSON200
	rows := make([]output.Row, 0, len(page.Items))
	for _, e := range page.Items {
		rows = append(rows, auditRow(e))
	}

	doc := &output.Doc{
		Columns: cols,
		Rows:    rows,
		Extra: map[string]any{"pagination": map[string]any{
			"limit":   page.Pagination.Limit,
			"offset":  page.Pagination.Offset,
			"hasMore": page.Pagination.HasMore,
		}},
		EmptyMessage: "No audit-log entries matched.",
	}
	if err := app.Out.Write(doc); err != nil {
		return err
	}
	if page.Pagination.HasMore {
		app.Out.Infof("More results available: re-run with --offset %d.",
			page.Pagination.Offset+len(page.Items))
	}
	return nil
}

// parseDateTime accepts RFC 3339 or a bare date, mapping the latter to
// midnight UTC — the same convention as the server.
func parseDateTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%q is not a date (2006-01-02) or RFC 3339 timestamp", s)
}

func newAuditActorsCommand(app *App) *cobra.Command {
	var query string

	cmd := &cobra.Command{
		Use:   "actors",
		Short: "List distinct audit-log actors",
		Long: "List distinct actors from the audit log.\n\n" +
			"Use `--query` to narrow by substring. Useful for building\n" +
			"`--actor` filters for `audit list`.\n\n" +
			cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runAuditActors(c.Context(), app, query)
		},
	}
	cmd.Flags().StringVarP(&query, "query", "q", "", "filter actors by substring")
	return cmd
}

func runAuditActors(ctx context.Context, app *App, query string) error {
	sess, err := app.Connect(ctx, FeatureAuditLogRead)
	if err != nil {
		return err
	}

	params := &api.AuditActorsParams{}
	if query != "" {
		params.Q = &query
	}

	resp, err := sess.API.AuditActorsWithResponse(ctx, params)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return client.Fail(resp, resp.Headers429)
	}

	actors := resp.JSON200.Actors
	cols := []output.Column{{Name: "actor", Header: "Actor"}}
	rows := make([]output.Row, 0, len(actors))
	for _, a := range actors {
		rows = append(rows, output.Row{"actor": a})
	}

	doc := &output.Doc{
		Columns:      cols,
		Rows:         rows,
		EmptyMessage: "No actors found.",
	}
	return app.Out.Write(doc)
}
