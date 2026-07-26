package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steadfast/drift-cli/internal/auth"
	"github.com/steadfast/drift-cli/internal/cliexit"
	"github.com/steadfast/drift-cli/internal/config"
	"github.com/steadfast/drift-cli/internal/output"
)

// overridesFrom lifts the global flags into config overrides.
func overridesFrom(app *App) config.Overrides {
	return config.Overrides{
		Context:  app.Flags.contextName,
		Endpoint: app.Flags.endpoint,
	}
}

func newContextCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Manage the named deployments this CLI talks to",
		Long: "A context is a named endpoint plus the credential stored against it.\n\n" +
			"Config lives in $XDG_CONFIG_HOME/drift/config.yaml and holds no\n" +
			"secrets, so it is safe to commit to a dotfiles repository. Credentials\n" +
			"are stored separately, in the OS keyring or a 0600 file.\n\n" +
			"Override precedence for one invocation: --endpoint / --context beat\n" +
			"DRIFT_ENDPOINT / DRIFT_CONTEXT, which beat the current context.\n\n" +
			cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(
		newContextListCommand(app),
		newContextUseCommand(app),
		newContextCurrentCommand(app),
		newContextAddCommand(app),
		newContextRemoveCommand(app),
	)
	return cmd
}

func contextColumns() []output.Column {
	return []output.Column{
		{Name: "current", Header: "Current"},
		{Name: "name", Header: "Name"},
		{Name: "endpoint", Header: "Endpoint"},
		{Name: "output", Header: "Output", Wide: true},
	}
}

func newContextListCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List configured contexts",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := app.Config()
			if err != nil {
				return err
			}
			cols := contextColumns()
			if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
				return usageErrorf("%s", err.Error())
			}
			rows := make([]output.Row, 0, len(cfg.Contexts))
			for _, c := range cfg.Contexts {
				marker := ""
				if c.Name == cfg.CurrentContext {
					marker = "*"
				}
				rows = append(rows, output.Row{
					"current":  marker,
					"name":     c.Name,
					"endpoint": c.Endpoint,
					"output":   emptyToNil(c.Output),
				})
			}
			return app.Out.Write(&output.Doc{
				Columns:      cols,
				Rows:         rows,
				EmptyMessage: "No contexts configured. Add one with `drift context add <name> --endpoint <url>`.",
			})
		},
	}
}

func newContextUseCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Switch the current context",
		Args:  exactArgs(1, "the context name"),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := app.Config()
			if err != nil {
				return err
			}
			if err := cfg.Use(args[0]); err != nil {
				return &cliexit.ExitError{
					Code:    cliexit.Usage,
					Message: err.Error(),
					Hint:    "run `drift context list` to see what is configured",
				}
			}
			if err := cfg.Save(); err != nil {
				return &cliexit.ExitError{Code: cliexit.Error, Message: err.Error()}
			}
			app.Out.Infof("Now using context %q.", args[0])
			return nil
		},
	}
}

func newContextCurrentCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "current",
		Short: "Show the current context",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := app.Config()
			if err != nil {
				return err
			}
			r, err := cfg.Resolve(overridesFrom(app))
			if err != nil {
				return ConfigError(err)
			}
			if r.ContextName == "" && r.Endpoint == "" {
				// Usage, like every other "you have not configured this" path,
				// so a script sees one code for one condition.
				return &cliexit.ExitError{
					Code:    cliexit.Usage,
					Message: "no current context",
					Hint:    "run `drift context use <name>`",
				}
			}
			cols := []output.Column{
				{Name: "name", Header: "Name"},
				{Name: "endpoint", Header: "Endpoint"},
				{Name: "source", Header: "Source"},
			}
			if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
				return usageErrorf("%s", err.Error())
			}
			return app.Out.Write(&output.Doc{
				Columns: cols,
				Single:  true,
				Rows: []output.Row{{
					"name":     emptyToNil(r.ContextName),
					"endpoint": r.Endpoint,
					"source":   r.Source,
				}},
			})
		},
	}
}

func newContextAddCommand(app *App) *cobra.Command {
	var endpoint, outputFmt string
	var use bool

	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add or update a context",
		Long: "Add or update a context.\n\n" +
			"The endpoint is the deployment's ORIGIN (https://drift.example.com),\n" +
			"not the API prefix: the API path comes from the server's discovery\n" +
			"document, so a server that moves its prefix does not require every\n" +
			"operator to re-edit their configuration.",
		Args: exactArgs(1, "the context name"),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if endpoint == "" {
				return usageErrorf("--endpoint is required")
			}
			if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
				return usageErrorf("endpoint %q must start with http:// or https://", endpoint)
			}
			if outputFmt != "" {
				if _, err := output.ParseFormat(outputFmt); err != nil {
					return usageErrorf("%s", err.Error())
				}
			}
			cfg, err := app.Config()
			if err != nil {
				return err
			}
			existed := false
			previousEndpoint := ""
			if c, err := cfg.Find(name); err == nil {
				existed = true
				previousEndpoint = c.Endpoint
			}
			cfg.Add(config.Context{
				Name:     name,
				Endpoint: strings.TrimRight(endpoint, "/"),
				Output:   outputFmt,
			})
			// First context added becomes current, so a fresh install is one
			// command away from working rather than two.
			if use || cfg.CurrentContext == "" {
				cfg.CurrentContext = name
			}
			if err := cfg.Save(); err != nil {
				return &cliexit.ExitError{Code: cliexit.Error, Message: err.Error()}
			}
			// Re-pointing a context at a different deployment orphans the
			// credential filed against the OLD address: it is keyed on that
			// endpoint, so neither `auth logout` nor `context remove` can reach
			// it afterwards. Clear it here, and report a failure to do so on the
			// same terms as logout — a credential nobody can reach is a credential
			// nobody rotates.
			var orphanErr error
			if existed && config.NormalizeEndpoint(previousEndpoint) != config.NormalizeEndpoint(endpoint) {
				store, err := app.Store()
				if err != nil {
					return err
				}
				switch err := store.Delete(auth.NewKey(name, previousEndpoint)); {
				case err == nil:
					app.Out.Warnf("Context %q now points at a different deployment, so its previous credential was removed.", name)
				case errors.Is(err, auth.ErrNoCredential):
				default:
					orphanErr = err
				}
			}

			verb := "Added"
			if existed {
				verb = "Updated"
			}
			app.Out.Infof("%s context %q -> %s (config: %s)", verb, name, endpoint, cfg.FilePath())
			if cfg.CurrentContext == name {
				app.Out.Infof("Now using context %q.", name)
			}
			if orphanErr != nil {
				return &cliexit.ExitError{
					Code:    cliexit.Error,
					Message: fmt.Sprintf("context %q was re-pointed, but the credential for its previous endpoint could not be removed", name),
					Detail:  orphanErr.Error(),
					Hint:    "revoke the credential from the previous deployment's credentials page",
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "deployment origin, e.g. https://drift.example.com (required)")
	cmd.Flags().StringVar(&outputFmt, "output", "", "default output format for this context")
	cmd.Flags().BoolVar(&use, "use", false, "switch to this context after adding it")
	return cmd
}

func newContextRemoveCommand(app *App) *cobra.Command {
	var keepCredential bool

	cmd := &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Remove a context and its stored credential",
		Args:    exactArgs(1, "the context name"),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			cfg, err := app.Config()
			if err != nil {
				return err
			}
			// Captured BEFORE the removal: the credential is keyed on the
			// endpoint, and once the context is gone there is nothing to look it
			// up from.
			endpoint := app.contextEndpoint(name)
			if err := cfg.Remove(name); err != nil {
				return ConfigError(err)
			}
			if err := cfg.Save(); err != nil {
				return &cliexit.ExitError{Code: cliexit.Error, Message: err.Error()}
			}
			// The credential goes with the context by default. Leaving a live
			// token in the keyring under a name nothing references any more is
			// how credentials outlive the people who remember them.
			if !keepCredential {
				store, err := app.Store()
				if err != nil {
					return err
				}
				switch err := store.Delete(auth.NewKey(name, endpoint)); {
				case err == nil:
					app.Out.Infof("Removed the stored credential for %q.", name)
				case errors.Is(err, auth.ErrNoCredential):
					// Nothing to remove; not worth a line.
				default:
					// Discarding this left a live token filed under a name
					// nothing references any more — unreachable through the CLI
					// and therefore never rotated. The context is already gone,
					// so this is reported rather than rolled back, and it exits
					// non-zero so a script notices.
					app.Out.Infof("Removed context %q.", name)
					return &cliexit.ExitError{
						Code:    cliexit.Error,
						Message: fmt.Sprintf("context %q was removed but its credential could not be", name),
						Detail:  err.Error(),
						Hint:    "revoke the credential from the server's credentials page",
					}
				}
			}
			app.Out.Infof("Removed context %q.", name)
			if cfg.CurrentContext == "" {
				app.Out.Warnf("No current context is set. Pick one with `drift context use <name>`.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&keepCredential, "keep-credential", false,
		"leave the stored credential in place")
	return cmd
}
