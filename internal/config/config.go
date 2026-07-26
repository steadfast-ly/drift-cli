// Package config owns the on-disk configuration: named contexts, the
// current-context pointer, and the precedence rules that turn a flag, an
// environment variable and a stored context into one resolved target.
//
// Config is deliberately SHAREABLE — it holds endpoints and defaults and never
// a credential. Credentials live in `internal/auth`, in the OS keyring or a
// 0600 file, so that this file can be committed to a dotfiles repo without
// leaking anything.
//
// There is no KUBECONFIG-style multi-file merge. It is a documented footgun and
// one file is enough (DESIGN.md §4).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Context is a named bundle of endpoint and defaults.
type Context struct {
	Name string `yaml:"name"`
	// Endpoint is the deployment's ORIGIN — `https://drift.example.com` — not
	// the API prefix. The API path comes from the discovery document's
	// `services["api.v1"]`, so a server that moves its prefix does not require
	// every operator to re-edit their config.
	Endpoint string `yaml:"endpoint"`
	// Output overrides the default output format for this context. Empty means
	// the global default.
	Output string `yaml:"output,omitempty"`
}

// File is the whole of `config.yaml`.
type File struct {
	CurrentContext string    `yaml:"current-context,omitempty"`
	Contexts       []Context `yaml:"contexts"`

	// path is where this was loaded from, so Save writes back to the same place.
	path string `yaml:"-"`
}

// ErrNoContext is returned when an operation needs a context and none is set.
var ErrNoContext = errors.New("no current context")

// ErrNotFound is returned when a named context does not exist.
var ErrNotFound = errors.New("context not found")

// Dir returns the configuration directory, honouring XDG_CONFIG_HOME.
func Dir() (string, error) {
	if v := os.Getenv("DRIFT_CONFIG_DIR"); v != "" {
		return v, nil
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "drift"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "drift"), nil
}

// Path returns the path to `config.yaml`.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// Load reads the config file. A missing file is not an error — it yields an
// empty config, so a first run behaves like a fresh install rather than a
// failure.
func Load() (*File, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	f := &File{path: path}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	f.path = path
	return f, nil
}

// Save writes the config back, creating the directory if needed.
//
// 0600 on the file even though it holds no secrets: it records which internal
// hostnames an operator talks to, and the cost of being conservative is zero.
func (f *File) Save() error {
	if f.path == "" {
		p, err := Path()
		if err != nil {
			return err
		}
		f.path = p
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	out, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("serialize config: %w", err)
	}
	if err := os.WriteFile(f.path, out, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	return nil
}

// FilePath reports where this config lives on disk.
func (f *File) FilePath() string { return f.path }

// Find returns the named context.
func (f *File) Find(name string) (*Context, error) {
	for i := range f.Contexts {
		if f.Contexts[i].Name == name {
			return &f.Contexts[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
}

// Add inserts or replaces a context by name.
func (f *File) Add(c Context) {
	for i := range f.Contexts {
		if f.Contexts[i].Name == c.Name {
			f.Contexts[i] = c
			return
		}
	}
	f.Contexts = append(f.Contexts, c)
}

// Remove deletes a context by name, clearing current-context if it pointed
// there. Leaving a dangling pointer would make every subsequent command fail
// with "context not found" instead of the honest "no current context".
func (f *File) Remove(name string) error {
	idx := -1
	for i := range f.Contexts {
		if f.Contexts[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	f.Contexts = append(f.Contexts[:idx], f.Contexts[idx+1:]...)
	if f.CurrentContext == name {
		f.CurrentContext = ""
	}
	return nil
}

// Use sets the current context, refusing an unknown name.
func (f *File) Use(name string) error {
	if _, err := f.Find(name); err != nil {
		return err
	}
	f.CurrentContext = name
	return nil
}

// Overrides are the per-invocation values from flags. An empty string means
// "not supplied", which is what lets the precedence chain fall through.
type Overrides struct {
	Context  string
	Endpoint string
	Output   string
}

// Resolved is a fully-resolved target: which context is in play and where it
// points.
type Resolved struct {
	// ContextName is the name of the context in use, or "" when there is no
	// current context at all.
	//
	// A non-empty ContextName does NOT license sending that context's
	// credential: see CredentialContext.
	ContextName string
	Endpoint    string
	Output      string
	// Source records where the endpoint came from, for `doctor` and error
	// messages: "flag", "env" or "context".
	Source string
	// EndpointIsContexts is true only when Endpoint is the endpoint the named
	// context actually points at — either because it came from the context, or
	// because an override named the very same URL.
	//
	// This is the credential gate. `--context` and `--endpoint` are independent
	// overrides, so a current context of `prod` combined with
	// `--endpoint http://attacker.invalid` used to resolve to (prod, attacker)
	// and the caller would then attach prod's bearer token to a host the
	// operator merely typed. The token is the whole of the user's authority on
	// that deployment, so it may only ever travel to the address its own
	// context vouches for.
	EndpointIsContexts bool
}

// CredentialContext is the context name whose stored credential may be sent to
// this endpoint, or "" when no stored credential is allowed.
//
// Callers MUST use this rather than ContextName when deciding what to attach to
// a request. `DRIFT_TOKEN` is unaffected: an explicitly exported token is the
// operator saying "use this, here", which is a different act from the CLI
// silently reusing something it found on disk.
func (r *Resolved) CredentialContext() string {
	if !r.EndpointIsContexts {
		return ""
	}
	return r.ContextName
}

// Resolve applies the precedence rule from DESIGN.md §4: flag > environment
// variable > current context.
//
// An endpoint supplied by flag or environment does NOT need a context to exist.
// That is what makes `DRIFT_ENDPOINT=... drift env list` work on a machine with
// no config at all, which is the CI case.
func (f *File) Resolve(o Overrides) (*Resolved, error) {
	r := &Resolved{}

	// Which context, if any. A named context that does not exist is always an
	// error, even when an endpoint override would otherwise make it moot —
	// silently ignoring `--context typo` would run the command against the
	// wrong server.
	name := firstNonEmpty(o.Context, os.Getenv("DRIFT_CONTEXT"), f.CurrentContext)
	var ctx *Context
	if name != "" {
		c, err := f.Find(name)
		if err != nil {
			return nil, err
		}
		ctx = c
		r.ContextName = name
	}

	switch {
	case o.Endpoint != "":
		r.Endpoint, r.Source = o.Endpoint, "flag"
	case os.Getenv("DRIFT_ENDPOINT") != "":
		r.Endpoint, r.Source = os.Getenv("DRIFT_ENDPOINT"), "env"
	case ctx != nil:
		r.Endpoint, r.Source = ctx.Endpoint, "context"
	}

	ctxOutput := ""
	if ctx != nil {
		ctxOutput = ctx.Output
	}
	r.Output = firstNonEmpty(o.Output, os.Getenv("DRIFT_OUTPUT"), ctxOutput, "table")

	r.Endpoint = NormalizeEndpoint(r.Endpoint)

	// The credential gate. An override that names the context's own endpoint is
	// still the context's endpoint — `--endpoint https://drift.example.com` when
	// that is exactly where the current context points is a no-op, and refusing
	// the credential there would be surprising with no security benefit.
	r.EndpointIsContexts = ctx != nil && r.Endpoint != "" &&
		r.Endpoint == NormalizeEndpoint(ctx.Endpoint)

	return r, nil
}

// NormalizeEndpoint canonicalises an endpoint for comparison, for joining, and
// for use in a credential storage key.
//
// Exported because `internal/auth` keys credentials on the endpoint and MUST
// canonicalise it identically. Two copies of this rule would drift, and the
// symptom would be that `https://x` and `https://x/` become two separate
// credential entries — silently un-authenticating the trailing-slash spelling
// that the credential gate deliberately accepts.
//
// Only a trailing slash is stripped. Deliberately NOT a URL round trip: two
// spellings that differ in case, in an explicit `:443`, or in percent-encoding
// are not proven to be the same host by any cheap transformation, and treating
// them as equal is precisely the kind of near-miss that would put a credential
// on the wrong wire. Anything that is not literally the configured endpoint
// falls through to "no stored credential", which is the safe answer.
func NormalizeEndpoint(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

// RequireEndpoint is Resolve plus the assertion that a target was actually
// found, with an error that names the fix.
func (f *File) RequireEndpoint(o Overrides) (*Resolved, error) {
	r, err := f.Resolve(o)
	if err != nil {
		return nil, err
	}
	if r.Endpoint == "" {
		return nil, fmt.Errorf(
			"%w: set one with `drift context add <name> --endpoint <url>` "+
				"then `drift context use <name>`, or pass --endpoint",
			ErrNoContext)
	}
	return r, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
