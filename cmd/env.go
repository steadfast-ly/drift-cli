package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/steadfast-ly/drift-cli/internal/api"
	"github.com/steadfast-ly/drift-cli/internal/client"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
	"github.com/steadfast-ly/drift-cli/internal/output"
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
			"create, relaunch, wake, redeploy, retry-build, add-service and swap-branch.\n" +
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
		newEnvRedeployCommand(app),
		newEnvExtendCommand(app),
		newEnvVisibilityCommand(app, true),
		newEnvVisibilityCommand(app, false),
		newEnvAddServiceCommand(app),
		newEnvRemoveServiceCommand(app),
		newEnvSwapBranchCommand(app),
		newEnvRetryBuildCommand(app),
		newEnvWaitCommand(app),
		newEnvE2eCommand(app),
		newEnvTunnelCommand(app),
		newEnvDbCommand(app),
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
		{Name: "owner", Header: "Owner"},
		{Name: "expires", Header: "Expires"},
		{Name: "id", Header: "Id", Wide: true},
		{Name: "namespace", Header: "Namespace", Wide: true},
		{Name: "ttl_hours", Header: "TTL(h)", Wide: true},
		{Name: "slept_at", Header: "Slept", Wide: true},
		{Name: "public", Header: "Public", Wide: true},
		{Name: "status_message", Header: "Note", Wide: true},
	}
}

func envRow(e api.Environment) output.Row {
	return output.Row{
		"slug":      e.Slug,
		"status":    string(e.Status),
		"ticket":    e.TicketId,
		"owner":     e.CreatedBy,
		"expires":   e.ExpiresAt,
		"id":        e.Id.String(),
		"namespace": e.Namespace,
		"ttl_hours": e.TtlHours,
		"slept_at":  e.SleptAt,
		"public":    e.IsPublic,
		// The server sets statusMessage when a lifecycle step needs an
		// operator's attention (a wedged teardown, a quiesce); null otherwise.
		"status_message": e.StatusMessage,
	}
}

// envGetColumns is the single-environment detail column set: envColumns minus
// the owner column. Owner is the `env list` filter aid, not shown in the
// detail view.
func envGetColumns() []output.Column {
	cols := envColumns()
	for i, c := range cols {
		if c.Name == "owner" {
			return append(cols[:i:i], cols[i+1:]...)
		}
	}
	return cols
}

func newEnvListCommand(app *App) *cobra.Command {
	var statuses []string
	var limit, offset int
	var mine bool
	var owner string
	var all bool

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List environments",
		Long: "List environments.\n\n" +
			"Paginated server-side: --limit is capped at 50 by the contract and\n" +
			"--offset walks the pages. When more results exist than were returned,\n" +
			"a note is written to stderr so a piped JSON stream stays parseable.\n" +
			"Pass --all to fetch every page and print one combined result.\n\n" +
			cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runEnvList(c.Context(), app, statuses, limit, offset, mine, owner,
				c.Flags().Changed("owner"), all, c.Flags().Changed("limit"))
		},
	}
	cmd.Flags().StringSliceVar(&statuses, "status", nil,
		"filter by status; repeatable or comma-separated (e.g. running,sleeping)")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum number of environments to return (server default 20, max 50)")
	cmd.Flags().IntVar(&offset, "offset", 0, "number of environments to skip")
	cmd.Flags().BoolVar(&mine, "mine", false, "only environments you created (resolves your email via whoami)")
	cmd.Flags().StringVar(&owner, "owner", "", "only environments created by this email (exact match)")
	cmd.Flags().BoolVar(&all, "all", false, "fetch every page and print one combined result")
	return cmd
}

// fetchEnvListPage fetches one page of environments and maps transport and HTTP
// failures exactly like the single-page path. In a walk a failure on ANY page
// is fatal: a partial result is never printed.
func fetchEnvListPage(ctx context.Context, sess *Session, params *api.EnvironmentsListParams) (*api.EnvironmentPage, error) {
	resp, err := sess.API.EnvironmentsListWithResponse(ctx, params)
	if err != nil {
		return nil, client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return nil, client.Fail(resp, resp.Headers429)
	}
	return resp.JSON200, nil
}

func runEnvList(ctx context.Context, app *App, statuses []string, limit, offset int, mine bool, owner string, ownerChanged bool, all bool, limitChanged bool) error {
	if mine && ownerChanged {
		return usageErrorf("--mine and --owner are mutually exclusive")
	}

	cols := envColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	// --all and --limit contradict, whatever the value: --all fixes its own page
	// size, so an explicit --limit 0 or --limit -5 must still be a usage error
	// rather than a silently overridden number. The conflict is the flag's
	// PRESENCE, not its numeric positivity.
	if all && limitChanged {
		return usageErrorf("--all and --limit are mutually exclusive")
	}
	// validatePage is unconditional. With --all, `limit` stays 0 unless --limit
	// was explicitly flagged (which is already rejected above), so this checks
	// exactly the offset: the walk's cursor advances from the given start, and a
	// negative value would fetch the first page at offset 0 (the parameter is
	// unset when it is not positive) while the cursor lands mid-page,
	// re-requesting rows the first page already returned.
	if err := validatePage(limit, offset); err != nil {
		return err
	}

	params := &api.EnvironmentsListParams{}
	if all {
		// The walk asks for the contract maximum so a busy server is drained in
		// the fewest round trips.
		limit = maxPageSize
		params.Limit = &limit
	} else if limit > 0 {
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

	if mine {
		// --mine resolves the caller's own email, so filtering stays a pure
		// server-side exact match on the same string the server recorded at
		// create time. A whoami failure is a normal request error, not usage.
		who, err := sess.API.AuthWhoamiWithResponse(ctx)
		if err != nil {
			return client.Transport(err, sess.Resolved.Endpoint)
		}
		if who.JSON200 == nil {
			return client.Fail(who, who.Headers429)
		}
		params.Creator = &who.JSON200.Email
	} else if owner != "" {
		params.Creator = &owner
	}

	// params is built once — creator and statuses included — and reused for
	// every page request; only the offset advances inside the walk. The whoami
	// resolution above happens exactly once, before the walk begins.
	if !all {
		return envListSinglePage(ctx, app, sess, params, cols)
	}
	return envListWalk(ctx, app, sess, params, cols, offset)
}

// envListSinglePage renders one page — the plain `env list` path. The
// pagination extra carries the page's own values and the stderr note tells the
// user how to reach the rest.
func envListSinglePage(ctx context.Context, app *App, sess *Session, params *api.EnvironmentsListParams, cols []output.Column) error {
	page, err := fetchEnvListPage(ctx, sess, params)
	if err != nil {
		return err
	}

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

// maxWalkPages caps how many pages one `--all` walk may fetch. An
// offset-ignoring upstream (a proxy or load balancer) that repeats the same
// non-empty page with hasMore=true would otherwise hang the walk forever: the
// cursor keeps advancing past one page per request, so the empty-page check
// never fires — every page IS non-empty — and the server never runs dry. Ten
// thousand pages is 500,000 environments at the 50-per-page contract ceiling,
// orders of magnitude past any real deployment; hitting it is the server fault
// it is reported as, never a sign the walk is healthy.
const maxWalkPages = 10000

// envListWalk fetches every page — starting at the chosen offset — until the
// server reports no more, and prints one combined result.
//
// The cursor advances by the number of items ACTUALLY returned, not by the
// requested limit, so a server that shortens a page mid-window neither skips
// nor repeats rows. The empty-page check is the completeness guard: --all
// promises every result, so a server that claims more while returning nothing
// would either loop or silently truncate — the walk fails loudly instead of
// spinning or printing a partial result. The page ceiling is the other
// defensive stop: an upstream that repeats a non-empty page forever is caught
// by maxWalkPages, since the empty-page check cannot see it.
func envListWalk(ctx context.Context, app *App, sess *Session, params *api.EnvironmentsListParams, cols []output.Column, start int) error {
	rows := make([]output.Row, 0, maxPageSize)
	cur := start
	for n := 0; ; n++ {
		// Checked before the fetch so an endless server costs exactly
		// maxWalkPages requests, never one more.
		if n >= maxWalkPages {
			return fmt.Errorf("--all aborted after %d pages: server keeps reporting more results", maxWalkPages)
		}
		page, err := fetchEnvListPage(ctx, sess, params)
		if err != nil {
			return err
		}
		// --all promises completeness: an empty page that claims more would
		// either loop or silently truncate the accumulated rows — fail loudly
		// instead. A terminal empty page (hasMore=false) is the legitimate
		// success path.
		if len(page.Items) == 0 && page.Pagination.HasMore {
			return fmt.Errorf("--all aborted: server returned an empty page while reporting more results")
		}
		for _, e := range page.Items {
			rows = append(rows, envRow(e))
		}
		if !page.Pagination.HasMore {
			break
		}
		cur += len(page.Items)
		params.Offset = &cur
	}

	doc := &output.Doc{
		Columns: cols,
		Rows:    rows,
		Extra: map[string]any{"pagination": map[string]any{
			// `limit` is the total rows the walk returned, not a per-page
			// ceiling, and `hasMore` is always false: nothing is left to fetch.
			"limit":   len(rows),
			"offset":  start,
			"hasMore": false,
		}},
		EmptyMessage: "No environments matched.",
	}
	return app.Out.Write(doc)
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
	cols := envGetColumns()
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
	// The status note only in the human formats, like the sub-tables below: the
	// machine formats already carry it as `status_message` (raw).
	if m := detail.Environment.StatusMessage; m != nil {
		if note := sanitizeNote(*m); note != "" {
			fmt.Fprintf(app.Stdout, "\nStatus note: %s\n", note)
		}
	}
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

// sanitizeNote makes server-side prose safe to print verbatim: the note embeds
// third-party error text, and a control sequence in it (ESC/OSC) could
// otherwise manipulate the operator's terminal. Non-printable runes are
// dropped and whitespace runs collapse to one space, so a multiline message
// stays one note line.
func sanitizeNote(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := false
	for _, r := range s {
		switch {
		case r == ' ' || r == '\n' || r == '\t':
			pendingSpace = true
		case unicode.IsPrint(r):
			if pendingSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			pendingSpace = false
			b.WriteRune(r)
		}
	}
	return b.String()
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
