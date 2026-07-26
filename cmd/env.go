package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steadfast/drift-cli/internal/api"
	"github.com/steadfast/drift-cli/internal/client"
	"github.com/steadfast/drift-cli/internal/cliexit"
	"github.com/steadfast/drift-cli/internal/output"
)

func newEnvCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "env",
		Aliases: []string{"environment", "environments"},
		Short:   "Create, inspect and manage preview environments",
		Long: "Create, inspect and manage preview environments.\n\n" +
			"Environments are addressed by SLUG or by UUID and the server resolves\n" +
			"both. A slug resolves only environments that are neither destroyed nor\n" +
			"canceled, so a slug reused over time addresses the one live\n" +
			"environment holding it; address a torn-down environment by id.\n\n" +
			"Commands that start work BLOCK by default and take --no-wait:\n" +
			"create, relaunch, wake, retry-build, add-service and swap-branch.\n" +
			"Commands that end it return immediately and take --wait: rm, sleep\n" +
			"and cancel. `drift env wait` follows either afterwards.\n\n" +
			cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(
		newEnvListCommand(app),
		newEnvGetCommand(app),
		newEnvCreateCommand(app),
		newEnvRmCommand(app),
		newEnvCancelCommand(app),
		newEnvRelaunchCommand(app),
		newEnvSleepCommand(app),
		newEnvWakeCommand(app),
		newEnvExtendCommand(app),
		newEnvVisibilityCommand(app, true),
		newEnvVisibilityCommand(app, false),
		newEnvAddServiceCommand(app),
		newEnvRemoveServiceCommand(app),
		newEnvSwapBranchCommand(app),
		newEnvRetryBuildCommand(app),
		newEnvWaitCommand(app),
	)
	return cmd
}

// envColumns is the CLI's stable field contract for an environment. Names are
// chosen here rather than inherited from the wire, so a server-side rename does
// not silently break a script built on `--json`.
func envColumns() []output.Column {
	return []output.Column{
		{Name: "slug", Header: "Slug"},
		output.StatusColumn("status", "Status"),
		{Name: "ticket", Header: "Ticket"},
		{Name: "expires", Header: "Expires"},
		{Name: "id", Header: "Id", Wide: true},
		{Name: "namespace", Header: "Namespace", Wide: true},
		{Name: "ttl_hours", Header: "TTL(h)", Wide: true},
		{Name: "slept_at", Header: "Slept", Wide: true},
		{Name: "public", Header: "Public", Wide: true},
	}
}

func envRow(e api.Environment) output.Row {
	return output.Row{
		"slug":      e.Slug,
		"status":    string(e.Status),
		"ticket":    e.TicketId,
		"expires":   e.ExpiresAt,
		"id":        e.Id.String(),
		"namespace": e.Namespace,
		"ttl_hours": e.TtlHours,
		"slept_at":  e.SleptAt,
		"public":    e.IsPublic,
	}
}

func newEnvListCommand(app *App) *cobra.Command {
	var statuses []string
	var limit, offset int

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List environments",
		Long: "List environments.\n\n" +
			"Paginated server-side: --limit is capped at 50 by the contract and\n" +
			"--offset walks the pages. When more results exist than were returned,\n" +
			"a note is written to stderr so a piped JSON stream stays parseable.\n\n" +
			cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runEnvList(c.Context(), app, statuses, limit, offset)
		},
	}
	cmd.Flags().StringSliceVar(&statuses, "status", nil,
		"filter by status; repeatable or comma-separated (e.g. running,sleeping)")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum number of environments to return (server default 20, max 50)")
	cmd.Flags().IntVar(&offset, "offset", 0, "number of environments to skip")
	return cmd
}

func runEnvList(ctx context.Context, app *App, statuses []string, limit, offset int) error {
	cols := envColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}

	params := &api.EnvironmentsListParams{}
	if limit > 0 {
		params.Limit = &limit
	}
	if offset > 0 {
		params.Offset = &offset
	}
	if len(statuses) > 0 {
		parsed, err := parseStatuses(statuses)
		if err != nil {
			return err
		}
		params.Status = &parsed
	}

	sess, err := app.Connect(ctx, FeatureEnvironmentsRead)
	if err != nil {
		return err
	}

	resp, err := sess.API.EnvironmentsListWithResponse(ctx, params)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return client.Fail(resp, resp.Headers429)
	}

	page := *resp.JSON200
	rows := make([]output.Row, 0, len(page.Items))
	for _, e := range page.Items {
		rows = append(rows, envRow(e))
	}

	doc := &output.Doc{
		Columns: cols,
		Rows:    rows,
		Extra: map[string]any{"pagination": map[string]any{
			"limit":   page.Pagination.Limit,
			"offset":  page.Pagination.Offset,
			"hasMore": page.Pagination.HasMore,
		}},
		EmptyMessage: "No environments matched.",
	}
	if err := app.Out.Write(doc); err != nil {
		return err
	}
	// Pagination advice on stderr, so it never corrupts `-o json`.
	if page.Pagination.HasMore {
		app.Out.Infof("More results available: re-run with --offset %d.",
			page.Pagination.Offset+len(page.Items))
	}
	return nil
}

// parseStatuses validates the filter client-side.
//
// The generated enum knows the legal set, so a typo is a usage error naming the
// alternatives rather than a round trip that comes back as a 400 the user has
// to interpret.
func parseStatuses(in []string) ([]api.EnvironmentsListParamsStatus, error) {
	out := make([]api.EnvironmentsListParamsStatus, 0, len(in))
	for _, raw := range in {
		s := api.EnvironmentsListParamsStatus(strings.TrimSpace(raw))
		if !s.Valid() {
			return nil, usageErrorf("unknown status %q; valid statuses: %s",
				raw, strings.Join(validStatuses(), ", "))
		}
		out = append(out, s)
	}
	return out, nil
}

func validStatuses() []string {
	all := []api.EnvironmentsListParamsStatus{
		api.EnvironmentsListParamsStatusRequested,
		api.EnvironmentsListParamsStatusBuilding,
		api.EnvironmentsListParamsStatusBuildFailed,
		api.EnvironmentsListParamsStatusDeploying,
		api.EnvironmentsListParamsStatusRunning,
		api.EnvironmentsListParamsStatusSleeping,
		api.EnvironmentsListParamsStatusWaking,
		api.EnvironmentsListParamsStatusDeployFailed,
		api.EnvironmentsListParamsStatusDestroying,
		api.EnvironmentsListParamsStatusDestroyed,
		api.EnvironmentsListParamsStatusCanceled,
	}
	out := make([]string, 0, len(all))
	for _, s := range all {
		out = append(out, string(s))
	}
	sort.Strings(out)
	return out
}

func newEnvGetCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "get <slug-or-id>",
		Short: "Show one environment, with its services and builds",
		Long: "Show one environment, with its services and builds.\n\n" +
			"The reference is a slug or a UUID; the SERVER resolves both, so there\n" +
			"is no client-side list-and-filter that could disagree with it about\n" +
			"which environment a reused slug names.\n\n" + cliexit.Help,
		Args: exactArgs(1, "the environment slug or id"),
		RunE: func(c *cobra.Command, args []string) error {
			return runEnvGet(c.Context(), app, args[0])
		},
	}
}

func runEnvGet(ctx context.Context, app *App, ref string) error {
	cols := envColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}

	sess, err := app.Connect(ctx, FeatureEnvironmentsRead)
	if err != nil {
		return err
	}

	resp, err := sess.API.EnvironmentsGetWithResponse(ctx, ref)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		e := client.Fail(resp, resp.Headers429)
		if resp.JSON404 != nil {
			e.Hint = fmt.Sprintf(
				"a slug resolves only live environments; if %q was destroyed or canceled, address it by id", ref)
		}
		return e
	}

	detail := *resp.JSON200
	doc := &output.Doc{
		Columns: cols,
		Single:  true,
		Rows:    []output.Row{envRow(detail.Environment)},
		Extra: map[string]any{
			"services": servicePayload(detail.Services),
			"builds":   buildPayload(detail.Builds),
		},
	}
	if err := app.Out.Write(doc); err != nil {
		return err
	}

	// Sub-tables only in the human formats: the machine formats already carry
	// the same data under `services` and `builds`.
	format := app.Out.EffectiveFormat()
	if format != output.FormatTable && format != output.FormatWide {
		return nil
	}
	wide := format == output.FormatWide
	if len(detail.Services) > 0 {
		fmt.Fprintln(app.Stdout, "\nSERVICES")
		if err := app.Out.Write(&output.Doc{Columns: serviceColumns(), Rows: serviceRows(detail.Services, wide)}); err != nil {
			return err
		}
	}
	if len(detail.Builds) > 0 {
		fmt.Fprintln(app.Stdout, "\nBUILDS")
		if err := app.Out.Write(&output.Doc{Columns: buildColumns(), Rows: buildRows(detail.Builds, wide)}); err != nil {
			return err
		}
	}
	return nil
}

func serviceColumns() []output.Column {
	return []output.Column{
		{Name: "branch", Header: "Branch"},
		{Name: "pr", Header: "PR"},
		{Name: "image_tag", Header: "Image tag"},
		{Name: "repository_id", Header: "Repository", Wide: true},
		{Name: "id", Header: "Id", Wide: true},
	}
}

func serviceRows(in []api.EnvironmentService, _ bool) []output.Row {
	out := make([]output.Row, 0, len(in))
	for _, s := range in {
		out = append(out, output.Row{
			"branch":        s.Branch,
			"pr":            s.PrNumber,
			"image_tag":     s.ImageTag,
			"repository_id": s.RepositoryId.String(),
			"id":            s.Id.String(),
		})
	}
	return out
}

func servicePayload(in []api.EnvironmentService) []map[string]any {
	rows := serviceRows(in, true)
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapRow(r))
	}
	return out
}

func buildColumns() []output.Column {
	return []output.Column{
		{Name: "branch", Header: "Branch"},
		output.StatusColumn("status", "Status"),
		{Name: "commit", Header: "Commit"},
		{Name: "image_tag", Header: "Image tag"},
		{Name: "created", Header: "Created"},
		{Name: "pr", Header: "PR", Wide: true},
		{Name: "started", Header: "Started", Wide: true},
		{Name: "repository_id", Header: "Repository", Wide: true},
		{Name: "id", Header: "Id", Wide: true},
	}
}

func buildRows(in []api.EnvironmentBuild, _ bool) []output.Row {
	out := make([]output.Row, 0, len(in))
	for _, b := range in {
		out = append(out, output.Row{
			"branch":        b.Branch,
			"status":        string(b.Status),
			"commit":        shortSHA(b.CommitSha),
			"image_tag":     b.ImageTag,
			"created":       b.CreatedAt,
			"pr":            b.PrNumber,
			"started":       b.StartedAt,
			"repository_id": b.RepositoryId.String(),
			"id":            b.Id.String(),
		})
	}
	return out
}

func buildPayload(in []api.EnvironmentBuild) []map[string]any {
	rows := buildRows(in, true)
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapRow(r))
	}
	return out
}

// shortSHA trims a commit to the conventional 7 characters, preserving nil.
func shortSHA(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	if len(*s) <= 7 {
		return *s
	}
	return (*s)[:7]
}

// mapRow converts a Row into a plain map with JSON-safe values.
func mapRow(r output.Row) map[string]any {
	out := make(map[string]any, len(r))
	for k, v := range r {
		out[k] = output.Normalize(v)
	}
	return out
}
