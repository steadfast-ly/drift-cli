package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steadfast/drift-cli/internal/api"
	"github.com/steadfast/drift-cli/internal/auth"
	"github.com/steadfast/drift-cli/internal/client"
	"github.com/steadfast/drift-cli/internal/cliexit"
	"github.com/steadfast/drift-cli/internal/output"
	"golang.org/x/term"
)

// CredentialsPath is where a deployment serves its credential-minting page.
// Fixed rather than discovered: it is a web page, not an API surface, and the
// discovery document deliberately advertises only machine endpoints.
const CredentialsPath = "/credentials"

func newAuthCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Authenticate against a drift deployment",
		Long: "Authenticate against a drift deployment.\n\n" +
			"v1 login is paste-based: drift prints the URL of the server's\n" +
			"credentials page, you authenticate there through the organisation's\n" +
			"existing SSO, the page mints a scoped credential, and you paste it\n" +
			"back. The credential is stored in the OS keyring where one is\n" +
			"available and in a 0600 file otherwise.\n\n" +
			"DRIFT_TOKEN overrides stored credentials entirely and suppresses\n" +
			"interactive login, which is how CI authenticates.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(newAuthLoginCommand(app), newAuthLogoutCommand(app), newAuthStatusCommand(app))
	return cmd
}

func newAuthLoginCommand(app *App) *cobra.Command {
	var tokenStdin bool

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store a credential for the current context",
		Long: "Store a credential for the current context.\n\n" +
			"The credential is validated against the server before it is stored, so\n" +
			"a mistyped or already-revoked token fails here rather than on the next\n" +
			"command.\n\n" + cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runAuthLogin(c.Context(), app, tokenStdin)
		},
	}
	cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false,
		"read the credential from standard input instead of prompting")
	return cmd
}

func runAuthLogin(ctx context.Context, app *App, tokenStdin bool) error {
	store, err := app.Store()
	if err != nil {
		return err
	}
	// The environment override is not a stored credential, so logging in would
	// have no observable effect: the next command would still use DRIFT_TOKEN.
	// Failing loudly beats writing a credential the user will never use.
	if store.EnvToken != "" {
		return &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: "DRIFT_TOKEN is set, so interactive login is suppressed",
			Detail:  "a credential from the environment always wins over stored ones",
			Hint:    "unset DRIFT_TOKEN to log in interactively",
		}
	}

	r, err := app.Resolve()
	if err != nil {
		return err
	}
	// The credential is about to be MINTED for this endpoint and then stored
	// under this context, so the two must be the same deployment. Storing a
	// token obtained from one server under a context that points at another is
	// how a credential ends up being sent somewhere it was never issued for —
	// the same coupling `credentialFor` enforces on the way out, enforced here
	// on the way in.
	if r.CredentialContext() == "" {
		if r.ContextName == "" {
			return &cliexit.ExitError{
				Code:    cliexit.Usage,
				Message: "a credential must be stored against a named context",
				Detail:  fmt.Sprintf("the endpoint %s came from --endpoint or DRIFT_ENDPOINT, which has no context to store against", r.Endpoint),
				Hint:    "run `drift context add <name> --endpoint " + r.Endpoint + "` first",
			}
		}
		return &cliexit.ExitError{
			Code:    cliexit.Usage,
			Message: fmt.Sprintf("refusing to store a credential for %s under context %q", r.Endpoint, r.ContextName),
			Detail: fmt.Sprintf("context %q points at %s, not at the endpoint given on the command line",
				r.ContextName, app.contextEndpoint(r.ContextName)),
			Hint: "add it as its own context: `drift context add <name> --endpoint " + r.Endpoint + "`",
		}
	}

	disc, err := app.Discover(ctx, r, true)
	if err != nil {
		return err
	}
	if !disc.Document.RequiresAuth() {
		app.Out.Infof("Context %q (%s) runs with authentication disabled — no credential is needed.",
			r.ContextName, r.Endpoint)
		return nil
	}

	mintURL := strings.TrimRight(r.Endpoint, "/") + CredentialsPath
	fmt.Fprintf(app.Stderr, "Open this page, sign in, and mint a CLI credential:\n\n    %s\n\n", mintURL)

	token, err := readToken(app, tokenStdin)
	if err != nil {
		return err
	}
	if token == "" {
		return &cliexit.ExitError{Code: cliexit.Usage, Message: "no credential supplied"}
	}

	// Validate before storing. `environments.list` with limit=1 is the cheapest
	// authenticated read the contract offers; anything that returns 200 proves
	// the credential verifies and carries at least the read floor.
	base, err := app.BaseURL(r, disc.Document)
	if err != nil {
		return err
	}
	probe, err := client.New(client.Options{
		BaseURL: base, Token: token, ClientVersion: app.Version, HTTP: app.httpClient(),
	})
	if err != nil {
		return &cliexit.ExitError{Code: cliexit.Error, Message: err.Error()}
	}
	if err := validateCredential(ctx, probe, r.Endpoint); err != nil {
		return err
	}

	src, err := store.Set(auth.NewKey(r.ContextName, r.Endpoint), token)
	var stale *auth.StaleCopyError
	switch {
	case err == nil:
	case errors.As(err, &stale):
		// The new credential IS stored, but a superseded copy survived in the
		// other store. Reported as a failure rather than a warning: until it is
		// removed, which credential a later command uses is not something this
		// process can promise, and that is exactly the silent-wrong-token bug
		// this path exists to prevent.
		app.Out.Infof("Credential %s stored for context %q in the %s.",
			auth.Describe(token), r.ContextName, src)
		return &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: "the credential was stored, but a superseded copy could not be removed",
			Detail:  stale.Error(),
			Hint:    "remove the leftover copy, then run `drift auth status` to confirm which credential is in use",
		}
	default:
		return &cliexit.ExitError{Code: cliexit.Error, Message: err.Error()}
	}

	app.Out.Infof("Credential %s stored for context %q in the %s.",
		auth.Describe(token), r.ContextName, src)
	if src == auth.SourceFile {
		dir, _ := app.ConfigDir()
		app.Out.Warnf("No OS keyring was available, so the credential is in %s/credentials.yaml (mode 0600).", dir)
	}
	return nil
}

// validateCredential proves a token authenticates before it is written to
// storage.
func validateCredential(ctx context.Context, c *api.ClientWithResponses, endpoint string) error {
	one := 1
	resp, err := c.EnvironmentsListWithResponse(ctx, &api.EnvironmentsListParams{Limit: &one})
	if err != nil {
		return client.Transport(err, endpoint)
	}
	if resp.JSON200 != nil {
		return nil
	}
	e := client.Fail(resp, resp.Headers429)
	if resp.JSON401 != nil {
		e.Hint = "the credential was rejected — mint a fresh one and paste it again"
	}
	// A 403 means the credential verified but its role is below the read floor.
	// That is a real, storable credential, so the failure is reported and the
	// token is NOT written: storing something that cannot read anything would
	// only produce a confusing failure later.
	return e
}

// readToken collects the credential without echoing it.
func readToken(app *App, tokenStdin bool) (string, error) {
	if tokenStdin {
		sc := bufio.NewScanner(app.Stdin)
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return "", &cliexit.ExitError{Code: cliexit.Error, Message: "read credential from stdin: " + err.Error()}
			}
			return "", &cliexit.ExitError{Code: cliexit.Usage, Message: "no credential on stdin"}
		}
		return strings.TrimSpace(sc.Text()), nil
	}

	f, isFile := app.Stdin.(*os.File)
	if isFile && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(app.Stderr, "Paste the credential (input is hidden): ")
		raw, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(app.Stderr)
		if err != nil {
			return "", &cliexit.ExitError{Code: cliexit.Error, Message: "read credential: " + err.Error()}
		}
		return strings.TrimSpace(string(raw)), nil
	}

	return "", &cliexit.ExitError{
		Code:    cliexit.Usage,
		Message: "cannot prompt for a credential: standard input is not a terminal",
		Hint:    "pipe it with `drift auth login --token-stdin`, or set DRIFT_TOKEN",
	}
}

func newAuthLogoutCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the stored credential for the current context",
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
			if r.ContextName == "" {
				return &cliexit.ExitError{
					Code:    cliexit.Usage,
					Message: "no current context, so there is no stored credential to remove",
				}
			}
			store, err := app.Store()
			if err != nil {
				return err
			}
			// Keyed on the CONTEXT's endpoint, not on any --endpoint override:
			// the credential was filed against the address the context names.
			key := auth.NewKey(r.ContextName, app.contextEndpoint(r.ContextName))
			if err := store.Delete(key); err != nil {
				if errors.Is(err, auth.ErrNoCredential) {
					app.Out.Infof("No credential was stored for context %q.", r.ContextName)
					return nil
				}
				// A partial logout must NOT look like a logout. Revocation is the
				// control that matters most for a bearer token, so a store that
				// could not confirm removal is a failure with a server-side
				// remedy, not a warning.
				return &cliexit.ExitError{
					Code:    cliexit.Error,
					Message: fmt.Sprintf("logout did not fully remove the credential for context %q", r.ContextName),
					Detail:  err.Error(),
					Hint:    "revoke the credential from the server's credentials page",
				}
			}
			app.Out.Infof("Removed the credential for context %q.", r.ContextName)
			if store.EnvToken != "" {
				app.Out.Warnf("DRIFT_TOKEN is still set in this shell and will continue to authenticate requests.")
			}
			return nil
		},
	}
}

func newAuthStatusCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show who this CLI is, against which context",
		Long: "Show the active context, the credential in use and whether the server\n" +
			"still accepts it.\n\n" +
			"The credential's OWNER, ROLE and EXPIRY are not shown: /api/v1 has no\n" +
			"operation that describes the calling credential, so this command\n" +
			"reports what it can prove rather than guessing. They are visible on\n" +
			"the server's credentials page.\n\n" + cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runAuthStatus(c.Context(), app)
		},
	}
}

func runAuthStatus(ctx context.Context, app *App) error {
	cfg, err := app.Config()
	if err != nil {
		return err
	}
	r, err := cfg.Resolve(overridesFrom(app))
	if err != nil {
		return ConfigError(err)
	}

	row := output.Row{
		"context":  emptyToNil(r.ContextName),
		"endpoint": emptyToNil(r.Endpoint),
	}
	cols := []output.Column{
		{Name: "context", Header: "Context"},
		{Name: "endpoint", Header: "Endpoint"},
		{Name: "credential", Header: "Credential"},
		{Name: "source", Header: "Source"},
		{Name: "org", Header: "Org"},
		{Name: "server_version", Header: "Server version"},
		{Name: "auth_mode", Header: "Auth mode"},
		output.StatusColumn("status", "Status"),
	}

	store, err := app.Store()
	if err != nil {
		return err
	}
	// The same gate every request goes through, so `auth status` reports the
	// credential that WOULD be used rather than one that exists but is not
	// allowed to travel to this endpoint.
	cred, credErr := app.credentialFor(store, r)
	row["credential"] = emptyToNil(auth.Describe(cred.Token))
	row["source"] = string(cred.Source)

	doc := &output.Doc{Columns: cols, Rows: []output.Row{row}, Single: true}

	if r.Endpoint == "" {
		row["status"] = "no context"
		if err := app.Out.Write(doc); err != nil {
			return err
		}
		// Usage, not AuthRequired: nothing is wrong with the credential, the CLI
		// has simply not been pointed at a server. Every command reports this
		// one condition with this one code.
		return &cliexit.ExitError{
			Code:    cliexit.Usage,
			Message: "no current context",
			Hint:    "run `drift context add <name> --endpoint <url>` then `drift context use <name>`",
		}
	}

	disc, discErr := app.Discover(ctx, r, true)
	if discErr != nil {
		row["status"] = "unreachable"
		if err := app.Out.Write(doc); err != nil {
			return err
		}
		return discErr
	}
	row["org"] = emptyToNil(disc.Document.Org)
	row["server_version"] = emptyToNil(disc.Document.Version)
	row["auth_mode"] = emptyToNil(disc.Document.Auth)

	if !disc.Document.RequiresAuth() {
		row["status"] = "no auth required"
		return app.Out.Write(doc)
	}
	if credErr != nil {
		row["status"] = "not authenticated"
		if err := app.Out.Write(doc); err != nil {
			return err
		}
		return credErr
	}

	base, err := app.BaseURL(r, disc.Document)
	if err != nil {
		return err
	}
	c, err := client.New(client.Options{
		BaseURL: base, Token: cred.Token, ClientVersion: app.Version, HTTP: app.httpClient(),
	})
	if err != nil {
		return &cliexit.ExitError{Code: cliexit.Error, Message: err.Error()}
	}
	probeErr := validateCredential(ctx, c, r.Endpoint)
	if probeErr != nil {
		row["status"] = "rejected"
		if err := app.Out.Write(doc); err != nil {
			return err
		}
		return probeErr
	}
	row["status"] = "authenticated"
	if err := app.Out.Write(doc); err != nil {
		return err
	}
	if f := app.Out.EffectiveFormat(); f == output.FormatTable || f == output.FormatWide {
		app.Out.Infof("\nThe credential's owner, role and expiry are not exposed by /api/v1; see %s.",
			strings.TrimRight(r.Endpoint, "/")+CredentialsPath)
	}
	return nil
}

func emptyToNil(s string) any {
	if s == "" || s == "(none)" {
		return nil
	}
	return s
}
