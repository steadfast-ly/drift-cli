package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// isolate points the config machinery at a scratch directory. DRIFT_CONFIG_DIR
// is the only override the package honours ahead of XDG, so a test can never
// touch a developer's real configuration.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DRIFT_CONFIG_DIR", dir)
	// Cleared so a developer's real environment cannot leak into a precedence
	// assertion — the bug this suite exists to catch would be invisible if the
	// ambient value happened to match.
	t.Setenv("DRIFT_CONTEXT", "")
	t.Setenv("DRIFT_ENDPOINT", "")
	t.Setenv("DRIFT_OUTPUT", "")
	return dir
}

func seeded(t *testing.T) *File {
	t.Helper()
	f := &File{}
	f.Add(Context{Name: "au", Endpoint: "https://drift.au.example.com"})
	f.Add(Context{Name: "en", Endpoint: "https://drift.en.example.com", Output: "json"})
	f.CurrentContext = "au"
	return f
}

func TestDirHonoursXDG(t *testing.T) {
	t.Setenv("DRIFT_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/xdg", "drift"); got != want {
		t.Fatalf("Dir() = %q, want %q", got, want)
	}
}

func TestPrecedenceFlagBeatsEnvBeatsContext(t *testing.T) {
	isolate(t)
	f := seeded(t)

	// 1. Context only.
	r, err := f.Resolve(Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Endpoint != "https://drift.au.example.com" || r.Source != "context" {
		t.Fatalf("context tier: got %+v", r)
	}

	// 2. Environment beats the context.
	t.Setenv("DRIFT_ENDPOINT", "https://from-env.example.com")
	r, err = f.Resolve(Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Endpoint != "https://from-env.example.com" || r.Source != "env" {
		t.Fatalf("env tier: got %+v", r)
	}

	// 3. Flag beats both.
	r, err = f.Resolve(Overrides{Endpoint: "https://from-flag.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Endpoint != "https://from-flag.example.com" || r.Source != "flag" {
		t.Fatalf("flag tier: got %+v", r)
	}
}

func TestContextSelectionPrecedence(t *testing.T) {
	isolate(t)
	f := seeded(t)

	t.Setenv("DRIFT_CONTEXT", "en")
	r, err := f.Resolve(Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if r.ContextName != "en" || r.Endpoint != "https://drift.en.example.com" {
		t.Fatalf("DRIFT_CONTEXT ignored: %+v", r)
	}

	r, err = f.Resolve(Overrides{Context: "au"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ContextName != "au" {
		t.Fatalf("--context did not beat DRIFT_CONTEXT: %+v", r)
	}
}

// A named context that does not exist must fail even when an endpoint override
// would make it moot. Silently ignoring `--context typo` runs the command
// against the wrong server, which is the worst possible outcome.
func TestUnknownContextIsAnErrorEvenWithEndpointOverride(t *testing.T) {
	isolate(t)
	f := seeded(t)

	_, err := f.Resolve(Overrides{Context: "nope", Endpoint: "https://elsewhere.example.com"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestOutputPrecedence(t *testing.T) {
	isolate(t)
	f := seeded(t)

	// Context default.
	f.CurrentContext = "en"
	r, _ := f.Resolve(Overrides{})
	if r.Output != "json" {
		t.Fatalf("context output default ignored: %q", r.Output)
	}

	// Environment beats the context default.
	t.Setenv("DRIFT_OUTPUT", "yaml")
	r, _ = f.Resolve(Overrides{})
	if r.Output != "yaml" {
		t.Fatalf("DRIFT_OUTPUT ignored: %q", r.Output)
	}

	// Flag beats everything.
	r, _ = f.Resolve(Overrides{Output: "wide"})
	if r.Output != "wide" {
		t.Fatalf("--output ignored: %q", r.Output)
	}

	// With nothing configured anywhere, the default is a table.
	f.CurrentContext = "au"
	t.Setenv("DRIFT_OUTPUT", "")
	r, _ = f.Resolve(Overrides{})
	if r.Output != "table" {
		t.Fatalf("default output: %q", r.Output)
	}
}

// An endpoint from a flag or the environment must work with NO context at all.
// That is the CI case: a runner with an empty config directory.
func TestEndpointOverrideNeedsNoContext(t *testing.T) {
	isolate(t)
	f := &File{}

	r, err := f.RequireEndpoint(Overrides{Endpoint: "https://ci.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ContextName != "" {
		t.Fatalf("invented a context: %q", r.ContextName)
	}
	// A trailing slash is stripped so joining the API path cannot produce `//`.
	if r.Endpoint != "https://ci.example.com" {
		t.Fatalf("endpoint not normalised: %q", r.Endpoint)
	}
}

func TestRequireEndpointWithNothingConfigured(t *testing.T) {
	isolate(t)
	f := &File{}
	_, err := f.RequireEndpoint(Overrides{})
	if !errors.Is(err, ErrNoContext) {
		t.Fatalf("want ErrNoContext, got %v", err)
	}
}

func TestSaveLoadRoundTripAnd0600(t *testing.T) {
	dir := isolate(t)
	f := seeded(t)
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config mode = %o, want 600", perm)
	}

	back, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if back.CurrentContext != "au" || len(back.Contexts) != 2 {
		t.Fatalf("round trip lost data: %+v", back)
	}
	if back.Contexts[1].Output != "json" {
		t.Fatalf("per-context output lost: %+v", back.Contexts[1])
	}
}

func TestLoadMissingFileIsEmptyNotAnError(t *testing.T) {
	isolate(t)
	f, err := Load()
	if err != nil {
		t.Fatalf("a first run must not fail: %v", err)
	}
	if len(f.Contexts) != 0 || f.CurrentContext != "" {
		t.Fatalf("expected an empty config, got %+v", f)
	}
}

// Removing the current context must clear the pointer. A dangling pointer makes
// every later command fail with "context not found" instead of the honest
// "no current context".
func TestRemoveClearsCurrentPointer(t *testing.T) {
	isolate(t)
	f := seeded(t)
	if err := f.Remove("au"); err != nil {
		t.Fatal(err)
	}
	if f.CurrentContext != "" {
		t.Fatalf("current context left dangling: %q", f.CurrentContext)
	}
	if err := f.Remove("au"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: want ErrNotFound, got %v", err)
	}
}

func TestUseRefusesUnknownContext(t *testing.T) {
	isolate(t)
	f := seeded(t)
	if err := f.Use("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if f.CurrentContext != "au" {
		t.Fatalf("a failed Use changed the pointer to %q", f.CurrentContext)
	}
}

func TestAddReplacesByName(t *testing.T) {
	isolate(t)
	f := seeded(t)
	f.Add(Context{Name: "au", Endpoint: "https://moved.example.com"})
	if len(f.Contexts) != 2 {
		t.Fatalf("Add duplicated a context: %+v", f.Contexts)
	}
	c, err := f.Find("au")
	if err != nil {
		t.Fatal(err)
	}
	if c.Endpoint != "https://moved.example.com" {
		t.Fatalf("Add did not replace: %+v", c)
	}
}

// --- the credential gate ----------------------------------------------------

// REGRESSION. `Resolve` used to set ContextName from the current context
// regardless of where the endpoint came from, and the caller then attached that
// context's stored bearer token to whatever `--endpoint` named. Reproduced
// against a listener that logged the token.
//
// `CredentialContext` is the gate: it is empty unless the endpoint in play IS
// the context's own endpoint.
func TestCredentialContextRefusesAForeignEndpoint(t *testing.T) {
	isolate(t)
	f := seeded(t)

	// Baseline: no override, so the context's credential is in scope.
	r, err := f.Resolve(Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.EndpointIsContexts || r.CredentialContext() != "au" {
		t.Fatalf("the context's own endpoint must be in scope: %+v", r)
	}

	// A flag pointing somewhere else: the context is still named — commands
	// report it — but its credential is out of scope.
	r, err = f.Resolve(Overrides{Endpoint: "http://attacker.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ContextName != "au" {
		t.Fatalf("the context should still be identified: %+v", r)
	}
	if r.EndpointIsContexts || r.CredentialContext() != "" {
		t.Fatalf("a foreign endpoint must not receive the context credential: %+v", r)
	}

	// The environment variable is the same override with the same consequence.
	t.Setenv("DRIFT_ENDPOINT", "http://attacker.invalid")
	r, err = f.Resolve(Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if r.CredentialContext() != "" {
		t.Fatalf("DRIFT_ENDPOINT must not receive the context credential: %+v", r)
	}
}

// An override that names the context's OWN endpoint is a no-op, and refusing
// the credential there would be surprising with no security benefit.
func TestCredentialContextAllowsAnEquivalentOverride(t *testing.T) {
	isolate(t)
	f := seeded(t)

	for _, spelling := range []string{
		"https://drift.au.example.com",
		"https://drift.au.example.com/",
		"  https://drift.au.example.com  ",
	} {
		r, err := f.Resolve(Overrides{Endpoint: spelling})
		if err != nil {
			t.Fatal(err)
		}
		if r.CredentialContext() != "au" {
			t.Fatalf("%q is the context's own endpoint but was treated as foreign: %+v", spelling, r)
		}
	}
}

// Near misses are refused rather than guessed at. A cheap normalisation that
// declared these equal would be the exact failure the gate exists to prevent.
func TestCredentialContextRefusesNearMisses(t *testing.T) {
	isolate(t)
	f := seeded(t)

	for _, spelling := range []string{
		"http://drift.au.example.com",           // scheme downgrade
		"https://drift.au.example.com:8443",     // different port
		"https://drift.au.example.com.evil.tld", // suffix
		"https://user@drift.au.example.com",     // userinfo
		"https://drift.au.example.com/sub",      // different path
	} {
		r, err := f.Resolve(Overrides{Endpoint: spelling})
		if err != nil {
			t.Fatal(err)
		}
		if r.CredentialContext() != "" {
			t.Fatalf("%q was accepted as the context's endpoint: %+v", spelling, r)
		}
	}
}

// With no context at all there is nothing to leak, and nothing to send.
func TestCredentialContextWithNoContext(t *testing.T) {
	isolate(t)
	f := &File{}
	r, err := f.Resolve(Overrides{Endpoint: "https://ci.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if r.CredentialContext() != "" || r.EndpointIsContexts {
		t.Fatalf("%+v", r)
	}
}
