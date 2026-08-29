package cmd

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/steadfast-ly/drift-cli/internal/api"
	"github.com/steadfast-ly/drift-cli/internal/client"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
	"github.com/steadfast-ly/drift-cli/internal/output"
)

func newRepoCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "repo",
		Aliases: []string{"repository", "repositories"},
		Short:   "List repositories and branches",
		Long: "List repositories and their branches.\n\n" +
			"Repositories are read-only in the CLI: CRUD is Server-Action-only\n" +
			"by design. Use the web UI to create, update or delete repositories.\n\n" +
			cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(
		newRepoListCommand(app),
		newRepoBranchesCommand(app),
	)
	return cmd
}

func repoColumns() []output.Column {
	return []output.Column{
		{Name: "id", Header: "Id"},
		{Name: "fullName", Header: "Full Name"},
		{Name: "displayName", Header: "Display Name"},
		{Name: "defaultBranch", Header: "Default Branch"},
		{Name: "helmChartKey", Header: "Chart Key"},
		{Name: "active", Header: "Active"},
		{Name: "group", Header: "Group"},
		{Name: "atomic", Header: "Atomic"},
		{Name: "stgUrl", Header: "stg URL", Wide: true},
		{Name: "rcUrl", Header: "rc URL", Wide: true},
		{Name: "prdUrl", Header: "prd URL", Wide: true},
	}
}

func repoRow(r api.Repository) output.Row {
	row := output.Row{
		"id":            r.Id.String(),
		"fullName":      r.FullName,
		"displayName":   r.DisplayName,
		"defaultBranch": r.DefaultBranch,
		"helmChartKey":  r.HelmChartKey,
		"active":        r.IsActive,
		"stgUrl":        r.StgUrl,
		"rcUrl":         r.RcUrl,
		"prdUrl":        r.PrdUrl,
	}
	if r.ApplicationGroup != nil {
		row["group"] = r.ApplicationGroup.DisplayName
		row["atomic"] = r.ApplicationGroup.Atomic
	}
	return row
}

func newRepoListCommand(app *App) *cobra.Command {
	var limit, offset int

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List repositories",
		Long: "List repositories drift knows about.\n\n" +
			"Paginated server-side: --limit is capped at 50 by the contract and\n" +
			"--offset walks the pages. When more results exist than were returned,\n" +
			"a note is written to stderr so a piped JSON stream stays parseable.\n\n" +
			cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runRepoList(c.Context(), app, limit, offset)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum number of repositories to return (server default 20, max 50)")
	cmd.Flags().IntVar(&offset, "offset", 0, "number of repositories to skip")
	return cmd
}

func runRepoList(ctx context.Context, app *App, limit, offset int) error {
	cols := repoColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	if err := validatePage(limit, offset); err != nil {
		return err
	}

	params := &api.RepositoriesListParams{}
	if limit > 0 {
		params.Limit = &limit
	}
	if offset > 0 {
		params.Offset = &offset
	}

	sess, err := app.Connect(ctx, FeatureRepositoriesRead)
	if err != nil {
		return err
	}

	resp, err := sess.API.RepositoriesListWithResponse(ctx, params)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return client.Fail(resp, resp.Headers429)
	}

	page := *resp.JSON200
	rows := make([]output.Row, 0, len(page.Items))
	for _, r := range page.Items {
		rows = append(rows, repoRow(r))
	}

	doc := &output.Doc{
		Columns: cols,
		Rows:    rows,
		Extra: map[string]any{"pagination": map[string]any{
			"limit":   page.Pagination.Limit,
			"offset":  page.Pagination.Offset,
			"hasMore": page.Pagination.HasMore,
		}},
		EmptyMessage: "No repositories found.",
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

func newRepoBranchesCommand(app *App) *cobra.Command {
	var limit, offset int
	var query string

	cmd := &cobra.Command{
		Use:   "branches <repository-id>",
		Short: "List a repository's recent branches",
		Long: "List a repository's branches with a commit in the last month.\n\n" +
			"Branches are fetched from the forge (GitHub). If the forge is\n" +
			"unreachable or the credentials are misconfigured, the server returns\n" +
			"a 502 external-service error — the CLI maps this to exit 1 with the\n" +
			"server's explanation.\n\n" +
			cliexit.Help,
		Args: exactArgs(1, "the repository UUID"),
		RunE: func(c *cobra.Command, args []string) error {
			return runRepoBranches(c.Context(), app, args[0], limit, offset, query)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum number of branches to return (server default 20, max 50)")
	cmd.Flags().IntVar(&offset, "offset", 0, "number of branches to skip")
	cmd.Flags().StringVarP(&query, "query", "q", "", "filter branches by substring")
	return cmd
}

func branchColumns() []output.Column {
	return []output.Column{
		{Name: "name", Header: "Name"},
		{Name: "protected", Header: "Protected"},
		{Name: "lastCommitDate", Header: "Last Commit"},
	}
}

func runRepoBranches(ctx context.Context, app *App, idStr string, limit, offset int, query string) error {
	cols := branchColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	if err := validatePage(limit, offset); err != nil {
		return err
	}

	id, err := uuid.Parse(idStr)
	if err != nil {
		return usageErrorf("invalid repository id %q: not a UUID", idStr)
	}

	params := &api.RepositoriesBranchesParams{}
	if limit > 0 {
		params.Limit = &limit
	}
	if offset > 0 {
		params.Offset = &offset
	}
	if query != "" {
		params.Q = &query
	}

	sess, err := app.Connect(ctx, FeatureRepositoriesRead)
	if err != nil {
		return err
	}

	resp, err := sess.API.RepositoriesBranchesWithResponse(ctx, id, params)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		e := client.Fail(resp, resp.Headers429)
		if resp.JSON404 != nil {
			e.Hint = fmt.Sprintf("no repository with id %s; run `drift repo list` to see available ids", idStr)
		}
		if resp.JSON502 != nil {
			e.Hint = "the forge (GitHub) is unreachable or its credentials are misconfigured; check with the deployment operator"
		}
		return e
	}

	page := *resp.JSON200
	rows := make([]output.Row, 0, len(page.Items))
	for _, b := range page.Items {
		rows = append(rows, output.Row{
			"name":           b.Name,
			"protected":      b.Protected,
			"lastCommitDate": b.LastCommitDate,
		})
	}

	doc := &output.Doc{
		Columns: cols,
		Rows:    rows,
		Extra: map[string]any{"pagination": map[string]any{
			"limit":   page.Pagination.Limit,
			"offset":  page.Pagination.Offset,
			"hasMore": page.Pagination.HasMore,
		}},
		EmptyMessage: "No branches found.",
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
