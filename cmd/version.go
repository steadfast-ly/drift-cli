package cmd

import (
	"context"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/steadfast/drift-cli/internal/cliexit"
	"github.com/steadfast/drift-cli/internal/output"
	"github.com/steadfast/drift-cli/spec"
)

func newVersionCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show client, server and API versions",
		Long: "Show the client version, the API contract this binary was generated\n" +
			"against, and — when a context is reachable — the server's version and\n" +
			"the minimum client it vouches for.\n\n" +
			"Client and server versions are INDEPENDENT. Compatibility is governed\n" +
			"by the discovery document, not by matching numbers, so a difference\n" +
			"between the two is expected rather than a problem.\n\n" + cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runVersion(c.Context(), app)
		},
	}
}

func runVersion(ctx context.Context, app *App) error {
	cols := []output.Column{
		{Name: "client_version", Header: "Client version"},
		{Name: "api_contract", Header: "API contract"},
		{Name: "openapi", Header: "OpenAPI", Wide: true},
		{Name: "go_version", Header: "Go", Wide: true},
		{Name: "platform", Header: "Platform", Wide: true},
		{Name: "context", Header: "Context"},
		{Name: "server_version", Header: "Server version"},
		{Name: "api_path", Header: "API path"},
		{Name: "minimum_client_version", Header: "Minimum client"},
	}
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}

	row := output.Row{
		"client_version": app.Version,
		"api_contract":   spec.APIVersion(),
		"openapi":        spec.OpenAPIVersion(),
		"go_version":     runtime.Version(),
		"platform":       runtime.GOOS + "/" + runtime.GOARCH,
	}

	// The server half is best-effort. `drift version` must work with no
	// configuration, offline, and on a machine that has never reached a
	// deployment — it is the first thing anyone runs after installing.
	cfg, err := app.Config()
	if err == nil {
		if r, rerr := cfg.Resolve(overridesFrom(app)); rerr == nil && r.Endpoint != "" {
			row["context"] = emptyToNil(r.ContextName)
			if disc, derr := app.Discover(ctx, r, false); derr == nil {
				row["server_version"] = disc.Document.Version
				row["api_path"] = disc.Document.APIV1()
				row["minimum_client_version"] = emptyToNil(disc.Document.MinimumClientVersion)
			} else {
				app.Out.Warnf("Could not reach %s for its version; showing client details only.", r.Endpoint)
			}
		}
	}

	return app.Out.Write(&output.Doc{Columns: cols, Rows: []output.Row{row}, Single: true})
}
