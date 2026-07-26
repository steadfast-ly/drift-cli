// Package cmd is the cobra command tree and the per-invocation runtime it hangs
// off.
//
// `App` is deliberately built LAZILY. Constructing it eagerly in a
// PersistentPreRun would make `drift version`, `drift context list` and
// `drift completion` fail on a machine with no configuration, which is exactly
// the machine on which someone runs them first.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/steadfast/drift-cli/internal/api"
	"github.com/steadfast/drift-cli/internal/auth"
	"github.com/steadfast/drift-cli/internal/client"
	"github.com/steadfast/drift-cli/internal/cliexit"
	"github.com/steadfast/drift-cli/internal/config"
	"github.com/steadfast/drift-cli/internal/discovery"
	"github.com/steadfast/drift-cli/internal/output"
)

func init() {
	// `internal/client` depends on `internal/discovery`, so the discovery
	// package cannot import the redirect policy it should use. Wired here, where
	// both are already in scope, so a Fetcher built without an explicit client
	// still refuses redirects.
	discovery.RedirectPolicy = client.NoRedirects
}

// Version is the client version, overridden at build time by goreleaser via
// `-ldflags "-X ...cli.Version=<tag>"`.
var Version = "0.1.0-dev"

// globalFlags are the flags every command shares.
type globalFlags struct {
	contextName string
	endpoint    string
	outputFmt   string
	jsonFields  string
	timeout     time.Duration
	noColor     bool
}

// App is the per-invocation runtime.
type App struct {
	Flags   globalFlags
	Stdout  io.Writer
	Stderr  io.Writer
	Stdin   io.Reader
	Out     *output.Writer
	Version string

	// HTTP is overridable so tests can drive the whole stack against a
	// httptest server without a network.
	HTTP *http.Client

	// configDir is resolved once; the credential store and the discovery cache
	// both hang off it.
	configDir string
	cfg       *config.File
	store     *auth.Store
}

// NewApp builds a runtime bound to the real process.
func NewApp() *App {
	return &App{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Stdin:   os.Stdin,
		Version: Version,
	}
}

// initOutput builds the writer. Called from the root command's
// PersistentPreRunE, so an invalid `-o` is a usage error before any request is
// made.
func (a *App) initOutput() error {
	format, err := output.ParseFormat(a.Flags.outputFmt)
	if err != nil {
		return &cliexit.ExitError{Code: cliexit.Usage, Message: err.Error()}
	}
	// Decided per stream: stdout and stderr are redirected independently, so
	// asking about one of them tells you nothing about the other.
	color := output.ColorEnabled(a.Stdout)
	errColor := output.ColorEnabled(a.Stderr)
	if a.Flags.noColor {
		color, errColor = false, false
	}
	a.Out = &output.Writer{
		Out:        a.Stdout,
		Err:        a.Stderr,
		Format:     format,
		JSONFields: output.SplitFields(a.Flags.jsonFields),
		Color:      color,
		ErrColor:   errColor,
	}
	return nil
}

// ConfigDir resolves and memoises the configuration directory.
func (a *App) ConfigDir() (string, error) {
	if a.configDir != "" {
		return a.configDir, nil
	}
	d, err := config.Dir()
	if err != nil {
		return "", err
	}
	a.configDir = d
	return d, nil
}

// Config loads and memoises the config file.
func (a *App) Config() (*config.File, error) {
	if a.cfg != nil {
		return a.cfg, nil
	}
	c, err := config.Load()
	if err != nil {
		return nil, &cliexit.ExitError{Code: cliexit.Error, Message: err.Error()}
	}
	a.cfg = c
	return c, nil
}

// Store returns the credential store.
func (a *App) Store() (*auth.Store, error) {
	if a.store != nil {
		return a.store, nil
	}
	dir, err := a.ConfigDir()
	if err != nil {
		return nil, err
	}
	a.store = auth.NewStore(dir)
	return a.store, nil
}

// Resolve applies the flag > env > context precedence and requires a target.
func (a *App) Resolve() (*config.Resolved, error) {
	cfg, err := a.Config()
	if err != nil {
		return nil, err
	}
	r, err := cfg.RequireEndpoint(config.Overrides{
		Context:  a.Flags.contextName,
		Endpoint: a.Flags.endpoint,
		Output:   "",
	})
	if err != nil {
		return nil, ConfigError(err)
	}
	return r, nil
}

// ConfigError maps a configuration failure onto ONE exit code.
//
// Every "you have not pointed this CLI at a server" failure — no current
// context, an unknown `--context`, an endpoint that is not a URL — is a usage
// error. They used to produce four different codes depending on which command
// noticed (0 from `version`, 1 from `env list`, 3 from `context current`, 4 from
// `auth status`), which is unusable from a script: the caller cannot tell "you
// are not configured" from "the server is down" from "your token expired".
//
// `errors.Is` against the package sentinels rather than matching on message
// text, which silently stops working the moment the wording changes.
func ConfigError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, config.ErrNotFound), errors.Is(err, config.ErrNoContext):
		return &cliexit.ExitError{Code: cliexit.Usage, Message: err.Error()}
	default:
		return &cliexit.ExitError{Code: cliexit.Error, Message: err.Error()}
	}
}

// httpClient returns the HTTP client to use, honouring --timeout.
func (a *App) httpClient() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	timeout := a.Flags.timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// The discovery fetch shares this client, so it gets the same refusal.
	// Nothing credentialled rides that request, but whoever controls one
	// redirect controls the whole document — `features_supported`, `auth`, and
	// where the API lives — and leaving it on a laxer policy than the requests
	// it configures is an asymmetry with no justification.
	return &http.Client{Timeout: timeout, Transport: client.NoRedirects(nil)}
}

// Discover fetches the discovery document for the resolved endpoint.
func (a *App) Discover(ctx context.Context, r *config.Resolved, force bool) (*discovery.Result, error) {
	dir, err := a.ConfigDir()
	if err != nil {
		return nil, err
	}
	f := &discovery.Fetcher{HTTP: a.httpClient(), Cache: discovery.NewCache(dir)}
	res, err := f.Fetch(ctx, r.Endpoint, force)
	if err != nil {
		return nil, client.Transport(err, r.Endpoint)
	}
	return res, nil
}

// Session is a connected, authenticated client plus everything the command
// needs to explain itself.
type Session struct {
	API        *api.ClientWithResponses
	Resolved   *config.Resolved
	Discovery  *discovery.Result
	Credential auth.Credential
	BaseURL    string
}

// Connect resolves the target, discovers its capabilities, checks skew, gates on
// a required feature and attaches a credential.
//
// `feature` may be empty for commands that need no particular capability.
func (a *App) Connect(ctx context.Context, feature string) (*Session, error) {
	r, err := a.Resolve()
	if err != nil {
		return nil, err
	}

	disc, err := a.Discover(ctx, r, false)
	if err != nil {
		return nil, err
	}
	doc := disc.Document

	// Skew warns, never refuses (DESIGN.md §4). A hard floor would brick an
	// operator's CLI mid-incident, which is exactly when it matters most.
	if warning := discovery.CheckSkew(a.Version, doc).Warning(r.ContextName, doc.Version); warning != "" {
		a.Out.Warnf("%s", warning)
	}

	// Capability gating: the command EXISTS in `--help` regardless, because a
	// help text that changes per server is undiscoverable. It fails here
	// instead, naming the context and the server version so the operator knows
	// which deployment said no.
	if feature != "" && !doc.HasFeature(feature) {
		where := r.Endpoint
		if r.ContextName != "" {
			where = fmt.Sprintf("%s (%s)", r.ContextName, r.Endpoint)
		}
		return nil, &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: fmt.Sprintf("this server does not support %q", feature),
			Detail: fmt.Sprintf("context %s is running drift %s, which advertises: %s",
				where, doc.Version, strings.Join(doc.FeaturesSupported, ", ")),
			Hint: "upgrade the server, or pick a context that has the feature",
		}
	}

	store, err := a.Store()
	if err != nil {
		return nil, err
	}
	cred, credErr := a.credentialFor(store, r)
	if credErr != nil && doc.RequiresAuth() {
		return nil, credErr
	}

	base, err := a.BaseURL(r, doc)
	if err != nil {
		return nil, err
	}
	c, err := client.New(client.Options{
		BaseURL:       base,
		Token:         cred.Token,
		ClientVersion: a.Version,
		HTTP:          a.httpClient(),
	})
	if err != nil {
		return nil, &cliexit.ExitError{Code: cliexit.Error, Message: err.Error()}
	}

	return &Session{
		API: c, Resolved: r, Discovery: disc, Credential: cred, BaseURL: base,
	}, nil
}

// BaseURL builds the API base and proves it still addresses the endpoint the
// operator configured.
//
// A separate step from string concatenation because `services["api.v1"]` is
// attacker-controlled in the threat model that matters here: the CLI is pointed
// at a URL, and the document served from it decides where the credential goes.
func (a *App) BaseURL(r *config.Resolved, doc *discovery.Document) (string, error) {
	if doc.APIV1Rejected() {
		a.Out.Warnf("warning: %s advertised an unusable API path in its discovery document; using %s.",
			r.Endpoint, discovery.DefaultAPIV1)
	}
	base, err := client.BaseURL(r.Endpoint, doc.APIV1())
	if err != nil {
		return "", &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: "refusing to send requests to a host the endpoint does not name",
			Detail:  err.Error(),
			Hint:    "check the endpoint, and run `drift doctor` to see what the server advertised",
		}
	}
	return base, nil
}

// credentialFor resolves the credential that may be sent to this endpoint.
//
// The gate is `CredentialContext`, NOT `ContextName`. `--context` and
// `--endpoint` are independent overrides, so a current context of `prod` plus
// `--endpoint http://somewhere-else` resolves to a context name AND a foreign
// address; attaching the context's token there hands the operator's entire
// authority on prod to whatever they just typed, and a typo, a copied
// command line or a hostile README is enough. A stored credential travels only
// to the address its own context vouches for.
//
// `DRIFT_TOKEN` is deliberately exempt. Exporting a token is an explicit act
// naming a credential for whatever runs next, which is a different thing from
// the CLI silently reusing something it found on disk.
func (a *App) credentialFor(store *auth.Store, r *config.Resolved) (auth.Credential, error) {
	credCtx := r.CredentialContext()
	// The key carries the endpoint as well as the name: a credential is filed
	// against the address it was minted for, so two config files that both call
	// a context `prod` cannot share one entry.
	cred, err := store.Get(auth.NewKey(credCtx, r.Endpoint))
	if err == nil {
		return cred, nil
	}

	// An unresolved conflict is its own condition: there IS a credential, in
	// fact two, and drift declines to guess which is current rather than run the
	// operator's commands under the wrong one.
	if errors.Is(err, auth.ErrAmbiguousCredential) {
		return cred, &cliexit.ExitError{
			Code:    cliexit.AuthRequired,
			Message: fmt.Sprintf("two different credentials are stored for context %q", credCtx),
			Detail: "one is in the OS keyring and one in the credential file. The file copy was " +
				"written while the keyring was unreachable, so it could not record which credential " +
				"it superseded, and drift will not guess which is current.",
			Hint: "run `drift auth login` to settle it, or `drift auth logout` to clear both",
		}
	}

	switch {
	case r.ContextName != "" && credCtx == "":
		return cred, &cliexit.ExitError{
			Code: cliexit.AuthRequired,
			Message: fmt.Sprintf("refusing to send the credential for context %q to %s",
				r.ContextName, r.Endpoint),
			Detail: fmt.Sprintf(
				"context %q points at %s; the endpoint came from --endpoint or DRIFT_ENDPOINT, "+
					"and a stored credential is only ever sent to the address its own context names",
				r.ContextName, a.contextEndpoint(r.ContextName)),
			Hint: "set DRIFT_TOKEN to authenticate against this endpoint, " +
				"or add it as a context of its own with `drift context add`",
		}
	case r.ContextName == "":
		return cred, &cliexit.ExitError{
			Code:    cliexit.AuthRequired,
			Message: fmt.Sprintf("no credential for %s", r.Endpoint),
			Detail:  "there is no current context, so there is no stored credential to use",
			Hint:    "set DRIFT_TOKEN, or run `drift context add <name> --endpoint " + r.Endpoint + "` and `drift auth login`",
		}
	case !errors.Is(err, auth.ErrNoCredential):
		// A store that could not be READ is not the same as an empty one, and
		// telling the user to log in when the real problem is an unparseable
		// credentials file sends them to a command that will fail the same way.
		return cred, &cliexit.ExitError{
			Code:    cliexit.AuthRequired,
			Message: fmt.Sprintf("the stored credential for context %q could not be read", r.ContextName),
			Detail:  err.Error(),
			Hint:    "fix or remove the credential store, then run `drift auth login`",
		}
	default:
		e := &cliexit.ExitError{
			Code:    cliexit.AuthRequired,
			Message: fmt.Sprintf("not authenticated against context %q", r.ContextName),
			Hint:    "run `drift auth login`, or set DRIFT_TOKEN",
		}
		// An upgrade re-keyed credentials on context name PLUS endpoint. A
		// credential filed by an older build under the bare name cannot be
		// attributed to a deployment, so it is not used — but saying nothing
		// makes a working install look like it logged itself out.
		if store.HasLegacyKeyringEntry(auth.NewKey(r.ContextName, r.Endpoint)) {
			e.Detail = "a credential from an earlier version of drift is stored under this context's " +
				"name, but not against an endpoint, so it cannot be shown to belong to " + r.Endpoint
			e.Hint = "run `drift auth login` once; `drift auth logout` removes the old entry"
		}
		return cred, e
	}
}

// contextEndpoint is the endpoint a named context points at, for error messages
// that need to contrast it with the override in play.
func (a *App) contextEndpoint(name string) string {
	cfg, err := a.Config()
	if err != nil {
		return "(unknown)"
	}
	c, err := cfg.Find(name)
	if err != nil {
		return "(unknown)"
	}
	return c.Endpoint
}
