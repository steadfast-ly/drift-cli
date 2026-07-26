package cmd

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/steadfast/drift-cli/internal/cliexit"
)

// Feature strings, as advertised by `/.well-known/drift.json`. Named constants
// rather than literals at the call sites, because a typo in one of these is a
// command that fails against every server with a confusing message.
const (
	FeatureEnvironmentsRead  = "environments.read"
	FeatureEnvironmentsWrite = "environments.write"
	FeatureRepositoriesRead  = "repositories.read"
	FeatureReleasesRead      = "releases.read"
	FeaturePromotionsRc      = "promotions.rc"
	FeaturePromotionsHotfix  = "promotions.hotfix"
)

// NewRootCommand builds the command tree.
func NewRootCommand(app *App) *cobra.Command {
	root := &cobra.Command{
		Use:   "drift",
		Short: "Manage drift preview environments and releases",
		Long: "drift is the command-line client for drift, the preview-environment\n" +
			"and release-management service.\n\n" +
			"One binary talks to every deployment. A CONTEXT names an endpoint and\n" +
			"a stored credential; `drift context use <name>` switches between them,\n" +
			"and --context or --endpoint override the choice for one invocation.\n\n" +
			cliexit.Help,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Cobra's default is to print help and exit 0 for an unknown
		// subcommand, which makes a typo in a script look like success. NoArgs
		// alone fixed the 0 but landed on 1, which a script cannot tell apart
		// from a server error — `usageArgs` carries the usage code with it.
		Args: usageArgs(cobra.NoArgs),
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			return app.initOutput()
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	f := &app.Flags
	pf := root.PersistentFlags()
	pf.StringVar(&f.contextName, "context", "", "context to use (overrides the current context)")
	pf.StringVar(&f.endpoint, "endpoint", "", "server endpoint to use (overrides the context's)")
	pf.StringVarP(&f.outputFmt, "output", "o", "table", "output format: table, wide, json or yaml")
	pf.StringVar(&f.jsonFields, "json", "", "emit JSON with exactly these comma-separated fields")
	pf.DurationVar(&f.timeout, "timeout", 30*time.Second, "per-request timeout")
	pf.BoolVar(&f.noColor, "no-color", false, "disable colour (NO_COLOR and TERM=dumb are honoured too)")

	// A malformed flag is a usage error (exit 2), not a generic failure.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &cliexit.ExitError{Code: cliexit.Usage, Message: err.Error()}
	})

	root.AddCommand(
		newAuthCommand(app),
		newContextCommand(app),
		newEnvCommand(app),
		newReleaseCommand(app),
		newDoctorCommand(app),
		newVersionCommand(app),
		newCompletionCommand(app),
	)

	// Every subcommand's argument validation gets the same treatment, including
	// the `cobra.NoArgs` the group commands carry — that is what turns
	// `drift env lst` from exit 1 into exit 2. Applied by walking the tree AFTER
	// it is assembled, so a command added later cannot forget; doing it before
	// AddCommand walks an empty tree and silently does nothing.
	applyUsageArgs(root)
	return root
}

// Execute runs the tree and maps the resulting error onto the exit-code
// contract. It is the single place a process exit code is decided.
func Execute() int {
	app := NewApp()
	root := NewRootCommand(app)
	root.SetOut(app.Stdout)
	root.SetErr(app.Stderr)

	err := root.Execute()
	if err == nil {
		return cliexit.OK
	}
	renderError(app, err)
	return cliexit.CodeOf(err)
}

// renderError prints a failure to stderr.
//
// Errors are DATA-free: they never carry a token, and they never print a raw Go
// error where the server supplied a typed one. `ExitError.Err` exists for
// `errors.Is` and is deliberately not rendered.
func renderError(app *App, err error) {
	w := app.Stderr
	if w == nil {
		w = os.Stderr
	}
	var ee *cliexit.ExitError
	if !asExitError(err, &ee) {
		fmt.Fprintf(w, "Error: %s\n", err.Error())
		return
	}
	fmt.Fprintf(w, "Error: %s\n", ee.Message)
	if ee.Detail != "" {
		fmt.Fprintf(w, "  %s\n", ee.Detail)
	}
	if ee.Hint != "" {
		fmt.Fprintf(w, "\nHint: %s\n", ee.Hint)
	}
}

// asExitError is errors.As, named for readability at the call sites.
//
// Hand-rolled unwrapping was a mistake: it followed only `Unwrap() error` and
// so walked past a `errors.Join` or any multi-error, silently downgrading a
// wrapped ExitError to exit 1.
func asExitError(err error, target **cliexit.ExitError) bool {
	return errors.As(err, target)
}

// usageErrorf builds a usage failure (exit 2).
func usageErrorf(format string, args ...any) error {
	return &cliexit.ExitError{Code: cliexit.Usage, Message: fmt.Sprintf(format, args...)}
}

// exactArgs is cobra.ExactArgs with the CLI's usage exit code attached.
func exactArgs(n int, usage string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return usageErrorf("expected %d argument(s): %s", n, usage)
		}
		return nil
	}
}

// usageArgs wraps a cobra argument validator so its failures carry exit 2.
//
// Cobra reports "unknown command" and "accepts 0 arg(s)" as plain errors, which
// `CodeOf` maps to 1 — indistinguishable, from a script, from the server having
// failed. A mistyped subcommand is a usage error and must say so.
func usageArgs(inner cobra.PositionalArgs) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if inner == nil {
			return nil
		}
		err := inner(c, args)
		if err == nil {
			return nil
		}
		var ee *cliexit.ExitError
		if errors.As(err, &ee) {
			return err
		}
		return &cliexit.ExitError{Code: cliexit.Usage, Message: err.Error()}
	}
}

// applyUsageArgs walks the assembled tree and wraps every argument validator.
func applyUsageArgs(c *cobra.Command) {
	for _, sub := range c.Commands() {
		if sub.Args != nil {
			sub.Args = usageArgs(sub.Args)
		}
		applyUsageArgs(sub)
	}
}
