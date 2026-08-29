package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steadfast-ly/drift-cli/internal/auth"
	"github.com/steadfast-ly/drift-cli/internal/client"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
	"github.com/steadfast-ly/drift-cli/internal/config"
	"github.com/steadfast-ly/drift-cli/internal/discovery"
	"github.com/steadfast-ly/drift-cli/internal/output"
)

// checkResult is one diagnostic line.
type checkResult struct {
	name   string
	state  string // "ok", "warn", "fail", "skip"
	detail string
	// exit is the code this failure should contribute. Zero means it does not
	// affect the exit code, which is what makes a warning a warning.
	exit int
}

func newDoctorCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose connectivity, authentication and version skew",
		Long: "Diagnose the current context end to end: whether the endpoint resolves,\n" +
			"whether the network path to it is open, whether the server answers\n" +
			"discovery, whether the stored credential still authenticates, whether\n" +
			"this client is older than the server will vouch for, and what the\n" +
			"deployment says it can do.\n\n" +
			"Checks run in dependency order and later checks are SKIPPED rather than\n" +
			"failed when an earlier one made them meaningless — a credential cannot\n" +
			"be judged against a server that is unreachable.\n\n" + cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runDoctor(c.Context(), app)
		},
	}
}

func runDoctor(ctx context.Context, app *App) error {
	cols := []output.Column{
		{Name: "check", Header: "Check"},
		output.StatusColumn("state", "State"),
		{Name: "detail", Header: "Detail"},
	}
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}

	checks, extra := doctorChecks(ctx, app)

	rows := make([]output.Row, 0, len(checks))
	worst := cliexit.OK
	for _, c := range checks {
		rows = append(rows, output.Row{"check": c.name, "state": c.state, "detail": c.detail})
		if c.exit != cliexit.OK && worst == cliexit.OK {
			// First real failure wins: it is the root cause, and later codes
			// are consequences of it.
			worst = c.exit
		}
	}

	doc := &output.Doc{Columns: cols, Rows: rows, Extra: extra}
	if err := app.Out.Write(doc); err != nil {
		return err
	}

	if f := app.Out.EffectiveFormat(); f == output.FormatTable || f == output.FormatWide {
		printCapabilities(app, extra)
	}
	if worst != cliexit.OK {
		return &cliexit.ExitError{
			Code:    worst,
			Message: "one or more checks failed",
			Hint:    "the table above names the first thing to fix",
		}
	}
	return nil
}

func doctorChecks(ctx context.Context, app *App) ([]checkResult, map[string]any) {
	extra := map[string]any{}
	var checks []checkResult

	// 1. Configuration.
	cfg, err := app.Config()
	if err != nil {
		return append(checks, checkResult{"config", "fail", err.Error(), cliexit.Error}), extra
	}
	r, rerr := cfg.Resolve(overridesFrom(app))
	if rerr != nil {
		return append(checks, checkResult{"config", "fail", rerr.Error(), cliexit.CodeOf(ConfigError(rerr))}), extra
	}
	if r.Endpoint == "" {
		checks = append(checks, checkResult{
			"config", "fail",
			"no current context; run `drift context add <name> --endpoint <url>`",
			cliexit.Usage,
		})
		return checks, extra
	}
	name := r.ContextName
	if name == "" {
		name = "(none)"
	}
	checks = append(checks, checkResult{
		"config", "ok",
		fmt.Sprintf("context %s -> %s (from %s), config %s", name, r.Endpoint, r.Source, cfg.FilePath()),
		cliexit.OK,
	})
	extra["context"] = r.ContextName
	extra["endpoint"] = r.Endpoint
	extra["credential_context"] = r.CredentialContext()

	// Worth its own line: it is the difference between "my token is not working"
	// and "the CLI is deliberately not sending it", and nothing else in the
	// output would explain the 401 that follows.
	if r.ContextName != "" && r.CredentialContext() == "" {
		checks = append(checks, checkResult{
			"credential scope", "warn",
			fmt.Sprintf("context %q points at %s, so its stored credential will NOT be sent to %s; "+
				"only DRIFT_TOKEN can authenticate here",
				r.ContextName, app.contextEndpoint(r.ContextName), r.Endpoint),
			cliexit.OK,
		})
	}

	// 2. Network path. Separated from discovery on purpose: "not on the VPN"
	// and "the server is broken" are different problems with different fixes,
	// and an HTTP client conflates them into one dial error.
	netCheck := checkNetworkPath(ctx, r.Endpoint, app.Flags.timeout)
	checks = append(checks, netCheck)
	if netCheck.state == "fail" {
		checks = append(checks,
			checkResult{"discovery", "skip", "skipped: the endpoint is unreachable", cliexit.OK},
			checkResult{"auth", "skip", "skipped: the endpoint is unreachable", cliexit.OK},
			checkResult{"version skew", "skip", "skipped: the server was not reached", cliexit.OK},
		)
		return checks, extra
	}

	// 3. Discovery. Forced past the cache: `doctor` is asked precisely when the
	// operator does not trust the cached answer.
	disc, derr := app.Discover(ctx, r, true)
	if derr != nil {
		checks = append(checks,
			checkResult{"discovery", "fail", errorDetail(derr), cliexit.CodeOf(derr)},
			checkResult{"auth", "skip", "skipped: discovery failed", cliexit.OK},
			checkResult{"version skew", "skip", "skipped: discovery failed", cliexit.OK},
		)
		return checks, extra
	}
	doc := disc.Document
	cacheNote := "fetched"
	if disc.FromCache {
		cacheNote = "revalidated from cache"
	}
	checks = append(checks, checkResult{
		"discovery", "ok",
		fmt.Sprintf("org %s, drift %s, auth %s, api %s (%s)",
			doc.Org, doc.Version, doc.Auth, doc.APIV1(), cacheNote),
		cliexit.OK,
	})
	// A rejected `api.v1` is reported rather than silently defaulted: it means
	// either a broken server or an attempt to redirect credentialled requests
	// off the endpoint, and both deserve to be visible.
	if doc.APIV1Rejected() {
		checks = append(checks, checkResult{
			"discovery", "warn",
			fmt.Sprintf("the server advertised services[\"api.v1\"] = %q, which is not a plain rooted path; using %s",
				doc.Services["api.v1"], discovery.DefaultAPIV1),
			cliexit.OK,
		})
	}
	extra["org"] = doc.Org
	extra["server_version"] = doc.Version
	extra["auth_mode"] = doc.Auth
	extra["api_path"] = doc.APIV1()
	extra["features_supported"] = doc.FeaturesSupported
	extra["minimum_client_version"] = doc.MinimumClientVersion
	extra["client_version"] = app.Version
	if disc.ETag != "" {
		extra["discovery_etag"] = disc.ETag
	}

	// 4. Credential.
	checks = append(checks, checkAuth(ctx, app, r, doc))

	// 5. Version skew. WARNS — never a failing exit code, by design: a hard
	// floor would brick an operator's CLI in the middle of an incident.
	skew := discovery.CheckSkew(app.Version, doc)
	switch {
	case !skew.Comparable && doc.MinimumClientVersion != "":
		checks = append(checks, checkResult{
			"version skew", "warn",
			fmt.Sprintf("cannot compare client %s against minimum %s", app.Version, doc.MinimumClientVersion),
			cliexit.OK,
		})
	case skew.TooOld:
		checks = append(checks, checkResult{
			"version skew", "warn",
			fmt.Sprintf("client %s is older than the minimum %s this server vouches for (advisory only)",
				app.Version, skew.MinimumClient),
			cliexit.OK,
		})
	default:
		checks = append(checks, checkResult{
			"version skew", "ok",
			fmt.Sprintf("client %s meets the minimum %s", app.Version, doc.MinimumClientVersion),
			cliexit.OK,
		})
	}

	return checks, extra
}

// checkNetworkPath dials the endpoint's host and port directly.
func checkNetworkPath(ctx context.Context, endpoint string, timeout time.Duration) checkResult {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return checkResult{"network", "fail", fmt.Sprintf("endpoint %q is not a URL", endpoint), cliexit.Usage}
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	d := net.Dialer{Timeout: timeout}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := d.DialContext(dialCtx, "tcp", net.JoinHostPort(host, port))
	if err == nil {
		_ = conn.Close()
		return checkResult{"network", "ok", fmt.Sprintf("tcp %s:%s reachable", host, port), cliexit.OK}
	}

	// The three failures below need three different actions, which is the whole
	// reason this check is separate from discovery.
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr):
		return checkResult{"network", "fail",
			fmt.Sprintf("cannot resolve %s — internal DNS is usually only reachable over the VPN", host),
			cliexit.Error}
	case isNetTimeout(err):
		return checkResult{"network", "fail",
			fmt.Sprintf("connecting to %s:%s timed out — check the VPN", host, port),
			cliexit.Error}
	default:
		return checkResult{"network", "fail",
			fmt.Sprintf("cannot connect to %s:%s (%s)", host, port, summarize(err)),
			cliexit.Error}
	}
}

func checkAuth(ctx context.Context, app *App, r *config.Resolved, doc *discovery.Document) checkResult {
	if !doc.RequiresAuth() {
		return checkResult{"auth", "ok", "the server runs with authentication disabled; no credential needed", cliexit.OK}
	}
	store, err := app.Store()
	if err != nil {
		return checkResult{"auth", "fail", err.Error(), cliexit.Error}
	}
	cred, cerr := app.credentialFor(store, r)
	if cerr != nil {
		return checkResult{"auth", "fail", errorDetail(cerr), cliexit.CodeOf(cerr)}
	}
	if store.HasLegacyKeyringEntry(auth.NewKey(r.ContextName, r.Endpoint)) {
		// Not a failure — a credential IS working — but a leftover the operator
		// should clear, since it is visible to any other config directory using
		// the same context name.
		app.Out.Warnf("warning: a pre-upgrade credential is still stored under the bare context name %q; "+
			"`drift auth logout` removes it.", r.ContextName)
	}

	base, err := app.BaseURL(r, doc)
	if err != nil {
		return checkResult{"auth", "fail", errorDetail(err), cliexit.CodeOf(err)}
	}
	c, err := client.New(client.Options{
		BaseURL: base, Token: cred.Token, ClientVersion: app.Version, HTTP: app.httpClient(),
	})
	if err != nil {
		return checkResult{"auth", "fail", err.Error(), cliexit.Error}
	}
	if verr := validateCredential(ctx, c, r.Endpoint); verr != nil {
		return checkResult{"auth", "fail",
			fmt.Sprintf("credential %s from the %s was refused: %s",
				auth.Describe(cred.Token), cred.Source, errorDetail(verr)),
			cliexit.CodeOf(verr)}
	}
	return checkResult{"auth", "ok",
		fmt.Sprintf("credential %s from the %s authenticates", auth.Describe(cred.Token), cred.Source),
		cliexit.OK}
}

func printCapabilities(app *App, extra map[string]any) {
	feats, _ := extra["features_supported"].([]string)
	if len(feats) == 0 {
		return
	}
	fmt.Fprintln(app.Stdout, "\nCAPABILITIES")
	for _, f := range feats {
		fmt.Fprintf(app.Stdout, "  %s\n", f)
	}
}

func errorDetail(err error) string {
	var ee *cliexit.ExitError
	if asExitError(err, &ee) {
		if ee.Detail != "" {
			return ee.Message + " — " + ee.Detail
		}
		return ee.Message
	}
	return err.Error()
}

func isNetTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func summarize(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 && i+2 < len(s) {
		return s[i+2:]
	}
	return s
}
