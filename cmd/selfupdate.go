package cmd

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
	"github.com/steadfast-ly/drift-cli/internal/output"
	"github.com/steadfast-ly/drift-cli/internal/selfupdate"
)

func newSelfUpdateCommand(app *App) *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Update drift to the latest release",
		Long: "Download and install the latest drift release from GitHub.\n\n" +
			"--check reports whether an update is available without downloading.\n" +
			"It works on any build (dev or release): it is a read-only query.\n" +
			"The update itself is restricted to release builds.\n\n" +
			"Managed installs (mise, Homebrew) are detected and refused with a\n" +
			"message naming the package manager's own upgrade command.\n\n" +
			"Dev builds (version not a release semver) cannot self-update.\n\n" + cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if check {
				return runSelfUpdateCheck(c.Context(), app)
			}
			return runSelfUpdate(c.Context(), app)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "report whether an update is available (exit 0 either way)")
	return cmd
}

// selfupdateHTTPClient returns the HTTP client for GitHub release operations.
// A separate fixed 30s client: app.httpClient() is for drift server requests
// and honours --timeout, which does not belong to a GitHub download.
func selfupdateHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// runSelfUpdateCheck queries the latest release and reports whether an update
// is available. Both outcomes are successful queries (exit 0); only a real
// failure exits non-zero.
func runSelfUpdateCheck(ctx context.Context, app *App) error {
	cols := []output.Column{
		{Name: "current_version", Header: "Current version"},
		{Name: "latest_version", Header: "Latest version"},
		{Name: "update_available", Header: "Update available"},
		{Name: "release_url", Header: "Release URL", Wide: true},
	}
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}

	res, _, err := selfupdate.Check(ctx, selfupdateHTTPClient(), app.Version)
	if err != nil {
		return &cliexit.ExitError{Code: cliexit.Error, Message: "checking for updates: " + err.Error()}
	}

	row := output.Row{
		"current_version":  res.CurrentVersion,
		"latest_version":   res.LatestVersion,
		"update_available": res.UpdateAvailable,
	}
	if res.ReleaseURL != "" {
		row["release_url"] = res.ReleaseURL
	}
	return app.Out.Write(&output.Doc{Columns: cols, Rows: []output.Row{row}, Single: true})
}

// runSelfUpdate applies the update: prose to stderr, no structured output.
// Use `--check` for machine-readable output.
func runSelfUpdate(ctx context.Context, app *App) error {
	if !selfupdate.IsReleaseBuild(app.Version) {
		return &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: fmt.Sprintf("self-update is not available for dev builds (version %s); build from source or install a release", app.Version),
		}
	}

	client := selfupdateHTTPClient()
	res, release, err := selfupdate.Check(ctx, client, app.Version)
	if err != nil {
		return &cliexit.ExitError{Code: cliexit.Error, Message: "checking for updates: " + err.Error()}
	}
	if !res.UpdateAvailable {
		fmt.Fprintf(app.Stderr, "Already up to date (%s)\n", res.CurrentVersion)
		return nil
	}

	if err := selfupdate.Apply(ctx, client, release, app.Version); err != nil {
		return &cliexit.ExitError{Code: cliexit.Error, Message: err.Error()}
	}
	fmt.Fprintf(app.Stderr, "Updated drift from %s to %s\n", res.CurrentVersion, res.LatestVersion)
	return nil
}
