package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steadfast/drift-cli/internal/auth"
	"github.com/steadfast/drift-cli/internal/cliexit"
)

// These tests drive the REAL cobra tree against a fake drift, so the exit-code
// contract is exercised end to end — flag parsing, discovery, credential
// resolution, the error envelope and the mapping — rather than unit by unit.

const goodToken = "drift_a-valid-looking-credential"

type fakeServer struct {
	*httptest.Server
	discoveryHits          int
	discoveryRevalidations int
	authHeaders            []string
	clientVers             []string
}

// newFakeDrift serves the discovery document and just enough of /api/v1 to
// exercise the CLI: a 200 for a valid bearer, a 401 envelope for anything else,
// a 404 envelope for an unknown ref.
func newFakeDrift(t *testing.T, doc map[string]any) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	mux := http.NewServeMux()

	// Served with a strong ETag and honouring If-None-Match, exactly as the
	// real route does — otherwise the caching test would prove nothing.
	body, _ := json.Marshal(doc)
	etag := fmt.Sprintf("%q", sha256.Sum256(body))
	mux.HandleFunc("/.well-known/drift.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
		if r.Header.Get("If-None-Match") == etag {
			fs.discoveryRevalidations++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fs.discoveryHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})

	problem := func(w http.ResponseWriter, status int, code, msg, ptype, detail string) {
		w.Header().Set("Content-Type", "application/json")
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="drift"`)
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"defined": true, "code": code, "status": status, "message": msg,
			"data": map[string]any{"type": ptype, "detail": detail},
		})
	}

	authed := func(r *http.Request) bool {
		fs.authHeaders = append(fs.authHeaders, r.Header.Get("Authorization"))
		fs.clientVers = append(fs.clientVers, r.Header.Get("X-Drift-Client-Version"))
		return r.Header.Get("Authorization") == "Bearer "+goodToken
	}

	mux.HandleFunc("/api/v1/environments", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			problem(w, 401, "UNAUTHORIZED", "Authentication required",
				"urn:drift:problem:unauthenticated", "The bearer credential is missing, expired or revoked.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{
				"id": "b92b68a9-877a-4f14-a92e-db1a62b803d9", "slug": "proof-alpha",
				"ticketId": "AUS-10001", "namespace": "pr-proof-alpha", "status": "running",
				"expiresAt": "2026-07-27T10:40:00Z", "ttlHours": 48, "sleptAt": nil, "isPublic": true,
			}},
			"pagination": map[string]any{"limit": 20, "offset": 0, "hasMore": false},
		})
	})

	mux.HandleFunc("/api/v1/environments/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			problem(w, 401, "UNAUTHORIZED", "Authentication required",
				"urn:drift:problem:unauthenticated", "The bearer credential is missing, expired or revoked.")
			return
		}
		ref := strings.TrimPrefix(r.URL.Path, "/api/v1/environments/")
		switch ref {
		case "proof-alpha":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"environment": map[string]any{
					"id": "b92b68a9-877a-4f14-a92e-db1a62b803d9", "slug": "proof-alpha",
					"ticketId": "AUS-10001", "namespace": "pr-proof-alpha", "status": "running",
					"expiresAt": "2026-07-27T10:40:00Z", "ttlHours": 48, "sleptAt": nil, "isPublic": true,
				},
				"services": []any{}, "builds": []any{},
			})
		case "conflicted":
			problem(w, 409, "CONFLICT", "Cannot sleep environment in building state",
				"urn:drift:problem:invalid-transition", "current state building, event SLEEP")
		case "forbidden":
			problem(w, 403, "FORBIDDEN", "Role release required",
				"urn:drift:problem:forbidden", "this credential carries read-only")
		case "broken":
			problem(w, 500, "INTERNAL_SERVER_ERROR", "Something failed",
				"urn:drift:problem:internal", "")
		case "undecodable":
			w.WriteHeader(502)
			_, _ = w.Write([]byte("<html>gateway</html>"))
		default:
			problem(w, 404, "NOT_FOUND", "Environment not found",
				"urn:drift:problem:not-found", "No such environment.")
		}
	})

	mux.HandleFunc("/api/v1/auth/whoami", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			problem(w, 401, "UNAUTHORIZED", "Authentication required",
				"urn:drift:problem:unauthenticated", "The bearer credential is missing, expired or revoked.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"email": "operator@example.com", "role": "admin", "channel": "cli",
			"credential": map[string]any{
				"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "label": "desktop",
				"scopes":    []string{},
				"expiresAt": "2026-09-24T19:19:59.694Z",
			},
		})
	})

	fs.Server = httptest.NewServer(mux)
	t.Cleanup(fs.Close)
	return fs
}

func defaultDoc(endpoint string) map[string]any {
	_ = endpoint
	return map[string]any{
		"org": "acme", "version": "1.0.0", "auth": "sso",
		"services":               map[string]string{"api.v1": "/api/v1"},
		"features_supported":     []string{"environments.read", "releases.read"},
		"minimum_client_version": "0.1.0",
	}
}

// harness wires an App to buffers, a temp config dir and an in-memory keyring.
type harness struct {
	app    *App
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	store  *auth.Store
	// key is the composite credential key for the context `setup` created.
	key auth.Key
}

// memKeyring honours the auth.Backend contract: absence is ErrNoCredential, and
// anything else means the store could not be reached. `Delete` and
// `context remove` both depend on the difference.
type memKeyring struct{ items map[string]string }

func (m *memKeyring) Get(s, u string) (string, error) {
	if v, ok := m.items[s+u]; ok {
		return v, nil
	}
	return "", fmt.Errorf("%w", auth.ErrNoCredential)
}
func (m *memKeyring) Set(s, u, p string) error { m.items[s+u] = p; return nil }
func (m *memKeyring) Delete(s, u string) error {
	if _, ok := m.items[s+u]; !ok {
		return fmt.Errorf("%w", auth.ErrNoCredential)
	}
	delete(m.items, s+u)
	return nil
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DRIFT_CONFIG_DIR", dir)
	t.Setenv("DRIFT_TOKEN", "")
	t.Setenv("DRIFT_CONTEXT", "")
	t.Setenv("DRIFT_ENDPOINT", "")
	t.Setenv("DRIFT_OUTPUT", "")
	t.Setenv("NO_COLOR", "1")

	h := &harness{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	// The real OS keyring is never touched by a test: a developer's login
	// keyring is not a test fixture, and CI has none at all.
	h.store = &auth.Store{
		Backend:  &memKeyring{items: map[string]string{}},
		FilePath: filepath.Join(dir, "credentials.yaml"),
	}
	h.app = &App{
		Stdout: h.stdout, Stderr: h.stderr, Stdin: strings.NewReader(""),
		Version: "0.1.0", HTTP: http.DefaultClient, store: h.store,
	}
	return h
}

// run executes one command line and returns (stdout, stderr, exit code).
func (h *harness) run(args ...string) (string, string, int) {
	h.stdout.Reset()
	h.stderr.Reset()
	// A fresh App per run, sharing the store and buffers: cobra flag state and
	// the memoised config must not leak between invocations, exactly as they do
	// not leak between processes.
	app := &App{
		Stdout: h.stdout, Stderr: h.stderr, Stdin: h.app.Stdin,
		Version: h.app.Version, HTTP: h.app.HTTP, store: h.store,
		// Carried over deliberately: a wait's poll period is process-level
		// configuration, not per-invocation state, and forgetting it here makes
		// every wait test sleep for real.
		waitInterval:      h.app.waitInterval,
		waitFailureWindow: h.app.waitFailureWindow,
	}
	root := NewRootCommand(app)
	root.SetArgs(args)
	root.SetOut(h.stdout)
	root.SetErr(h.stderr)

	err := root.Execute()
	if err != nil {
		renderError(app, err)
	}
	return h.stdout.String(), h.stderr.String(), cliexit.CodeOf(err)
}

func (h *harness) setup(t *testing.T, srv *fakeServer, token string) {
	t.Helper()
	if _, _, code := h.run("context", "add", "proof", "--endpoint", srv.URL); code != 0 {
		t.Fatalf("context add exited %d", code)
	}
	h.key = auth.NewKey("proof", srv.URL)
	if token != "" {
		if _, err := h.store.Set(h.key, token); err != nil {
			t.Fatal(err)
		}
	}
}

// --- exit-code mapping through the whole stack ------------------------------

func TestExitCodesEndToEnd(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"success", []string{"env", "list"}, cliexit.OK},
		{"not found", []string{"env", "get", "nope"}, cliexit.NotFound},
		{"conflict", []string{"env", "get", "conflicted"}, cliexit.Conflict},
		{"forbidden is not auth", []string{"env", "get", "forbidden"}, cliexit.Error},
		{"server error", []string{"env", "get", "broken"}, cliexit.Error},
		{"undecodable body", []string{"env", "get", "undecodable"}, cliexit.Error},
		{"bad flag", []string{"env", "list", "--nope"}, cliexit.Usage},
		{"bad output format", []string{"env", "list", "-o", "toml"}, cliexit.Usage},
		{"bad status filter", []string{"env", "list", "--status", "wat"}, cliexit.Usage},
		{"missing argument", []string{"env", "get"}, cliexit.Usage},
		{"unknown --json field", []string{"env", "list", "--json", "slugg"}, cliexit.Usage},
		{"unknown context", []string{"--context", "nope", "env", "list"}, cliexit.Usage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, code := h.run(c.args...)
			if code != c.want {
				t.Fatalf("exit %d, want %d\nstderr: %s", code, c.want, h.stderr.String())
			}
		})
	}
}

func TestUnauthenticatedExitsFour(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))

	// No credential stored at all: the CLI refuses before making a request.
	h := newHarness(t)
	h.setup(t, srv, "")
	_, errOut, code := h.run("env", "list")
	if code != cliexit.AuthRequired {
		t.Fatalf("exit %d, want %d", code, cliexit.AuthRequired)
	}
	if !strings.Contains(errOut, "drift auth login") {
		t.Fatalf("no actionable hint: %s", errOut)
	}

	// A credential the server rejects: the 401 envelope maps to the same code.
	h2 := newHarness(t)
	h2.setup(t, srv, "drift_revoked")
	_, errOut, code = h2.run("env", "list")
	if code != cliexit.AuthRequired {
		t.Fatalf("exit %d, want %d", code, cliexit.AuthRequired)
	}
	if !strings.Contains(errOut, "Authentication required") {
		t.Fatalf("the server's message was not surfaced: %s", errOut)
	}
	// The server's `data.detail` must reach the user too.
	if !strings.Contains(errOut, "missing, expired or revoked") {
		t.Fatalf("data.detail was dropped: %s", errOut)
	}
}

// A raw Go error must never be printed where the server supplied a typed one.
func TestServerMessageIsUsedNotAGoError(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	_, errOut, _ := h.run("env", "get", "conflicted")
	if !strings.Contains(errOut, "Cannot sleep environment in building state") {
		t.Fatalf("server message missing: %s", errOut)
	}
	if !strings.Contains(errOut, "current state building, event SLEEP") {
		t.Fatalf("server detail missing: %s", errOut)
	}
	for _, leak := range []string{"*api.", "json:", "&cliexit"} {
		if strings.Contains(errOut, leak) {
			t.Fatalf("a Go internal leaked into user output (%q): %s", leak, errOut)
		}
	}
}

// --- credentials ------------------------------------------------------------

// No credential value may appear in output, ever — including help text.
func TestTokenNeverAppearsInOutput(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	for _, args := range [][]string{
		{"env", "list"}, {"auth", "status"}, {"doctor"}, {"doctor", "-o", "json"},
		{"auth", "--help"}, {"--help"}, {"env", "get", "nope"},
	} {
		out, errOut, _ := h.run(args...)
		if strings.Contains(out+errOut, goodToken) {
			t.Fatalf("%v leaked the credential:\n%s%s", args, out, errOut)
		}
	}
}

func TestDriftTokenBypassesStorageAndSuppressesLogin(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, "drift_stored-and-revoked")

	// The environment override wins over the stored credential.
	h.store.EnvToken = goodToken
	_, _, code := h.run("env", "list")
	if code != cliexit.OK {
		t.Fatalf("DRIFT_TOKEN did not take precedence: exit %d\n%s", code, h.stderr.String())
	}

	// ...and interactive login is suppressed rather than silently useless.
	_, errOut, code := h.run("auth", "login")
	if code == cliexit.OK {
		t.Fatal("auth login must refuse while DRIFT_TOKEN is set")
	}
	if !strings.Contains(errOut, "DRIFT_TOKEN") {
		t.Fatalf("the refusal does not name the cause: %s", errOut)
	}
}

func TestAuthLoginValidatesBeforeStoring(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, "")

	// A rejected credential must not be written.
	h.app.Stdin = strings.NewReader("drift_bogus\n")
	_, _, code := h.run("auth", "login", "--token-stdin")
	if code != cliexit.AuthRequired {
		t.Fatalf("exit %d, want %d", code, cliexit.AuthRequired)
	}
	if _, err := h.store.Get(h.key); err == nil {
		t.Fatal("a rejected credential was stored anyway")
	}

	// A good one is stored, and the URL of the credentials page is printed.
	h.app.Stdin = strings.NewReader(goodToken + "\n")
	_, errOut, code := h.run("auth", "login", "--token-stdin")
	if code != cliexit.OK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(errOut, srv.URL+"/credentials") {
		t.Fatalf("the mint URL was not shown: %s", errOut)
	}
	got, err := h.store.Get(h.key)
	if err != nil || got.Token != goodToken {
		t.Fatalf("credential not stored: %+v %v", got, err)
	}

	// Logout removes it.
	if _, _, code := h.run("auth", "logout"); code != cliexit.OK {
		t.Fatalf("logout exit %d", code)
	}
	if _, err := h.store.Get(h.key); err == nil {
		t.Fatal("the credential survived logout")
	}
}

// Attribution: every request carries the client version so CLI traffic is
// distinguishable from browser traffic in the audit log.
func TestRequestsCarryBearerAndClientVersion(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	if _, _, code := h.run("env", "list"); code != cliexit.OK {
		t.Fatal(h.stderr.String())
	}
	if len(srv.authHeaders) == 0 {
		t.Fatal("no request reached the server")
	}
	if srv.authHeaders[0] != "Bearer "+goodToken {
		t.Fatalf("Authorization = %q", srv.authHeaders[0])
	}
	if srv.clientVers[0] != "0.1.0" {
		t.Fatalf("X-Drift-Client-Version = %q", srv.clientVers[0])
	}
}

// --- discovery, capability gating and skew ----------------------------------

// A command for an absent feature must EXIST in --help and fail on invocation,
// naming the context and the server version.
func TestAbsentFeatureFailsFastNamingTheContext(t *testing.T) {
	doc := defaultDoc("")
	doc["features_supported"] = []string{"releases.read"}
	doc["version"] = "0.9.9"
	srv := newFakeDrift(t, doc)
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	// Still discoverable in help: a help text that changes per server is
	// undiscoverable.
	out, _, _ := h.run("env", "--help")
	if !strings.Contains(out, "list") {
		t.Fatalf("env list vanished from --help:\n%s", out)
	}

	_, errOut, code := h.run("env", "list")
	if code == cliexit.OK {
		t.Fatal("an unsupported feature must fail")
	}
	for _, want := range []string{"environments.read", "proof", "0.9.9"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("the failure does not mention %q: %s", want, errOut)
		}
	}
}

// Skew WARNS and never refuses. A hard floor would brick an operator's CLI
// mid-incident.
func TestVersionSkewWarnsButSucceeds(t *testing.T) {
	doc := defaultDoc("")
	doc["minimum_client_version"] = "9.9.9"
	srv := newFakeDrift(t, doc)
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	out, errOut, code := h.run("env", "list")
	if code != cliexit.OK {
		t.Fatalf("skew refused the command: exit %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "9.9.9") || !strings.Contains(errOut, "warning") {
		t.Fatalf("no skew warning on stderr: %s", errOut)
	}
	if strings.Contains(out, "warning") {
		t.Fatalf("the warning polluted stdout: %s", out)
	}
	if !strings.Contains(out, "proof-alpha") {
		t.Fatalf("the command did not run: %s", out)
	}
}

// Discovery is cached between invocations, so a second command does not refetch
// the document from scratch.
func TestDiscoveryIsCachedAcrossInvocations(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	h.run("env", "list")
	if srv.discoveryHits != 1 {
		t.Fatalf("first run fetched the document %d times, want 1", srv.discoveryHits)
	}

	h.run("env", "list")
	h.run("env", "get", "proof-alpha")
	// Every later run revalidates with If-None-Match and gets a 304: the body is
	// never sent again, which is the whole point of the ETag.
	if srv.discoveryHits != 1 {
		t.Fatalf("the document was refetched %d times; the cache is not working", srv.discoveryHits)
	}
	if srv.discoveryRevalidations != 2 {
		t.Fatalf("expected 2 conditional revalidations, got %d", srv.discoveryRevalidations)
	}

	// `doctor` deliberately forces past the cache: it is run precisely when the
	// operator does not trust the cached answer.
	h.run("doctor")
	if srv.discoveryHits != 2 {
		t.Fatalf("doctor did not force a fresh fetch (hits=%d)", srv.discoveryHits)
	}
}

// `auth: "none"` servers need no credential at all.
func TestAuthNoneSkipsCredentials(t *testing.T) {
	doc := defaultDoc("")
	doc["auth"] = "none"
	srv := newFakeDrift(t, doc)
	h := newHarness(t)
	h.setup(t, srv, "")

	// The fake insists on a bearer, so the request still 401s — but the CLI must
	// have ATTEMPTED it rather than refusing locally with exit 4 for a missing
	// credential. The distinction is visible in the message.
	_, errOut, _ := h.run("env", "list")
	if strings.Contains(errOut, "not authenticated against context") {
		t.Fatalf("the CLI demanded a credential from an auth:none server: %s", errOut)
	}

	_, errOut, code := h.run("auth", "login")
	if code != cliexit.OK {
		t.Fatalf("login against auth:none should be a no-op success, exit %d", code)
	}
	if !strings.Contains(errOut, "authentication disabled") {
		t.Fatalf("login did not explain itself: %s", errOut)
	}
}

// --- output and precedence --------------------------------------------------

func TestOutputFormatsAndStreamSeparation(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	out, _, code := h.run("env", "list", "-o", "json")
	if code != cliexit.OK {
		t.Fatal(h.stderr.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("stdout is not parseable JSON: %v\n%s", err, out)
	}
	if _, ok := parsed["items"]; !ok {
		t.Fatalf("no items envelope: %s", out)
	}

	out, _, _ = h.run("env", "list", "--json", "slug,status")
	if strings.Contains(out, "namespace") {
		t.Fatalf("--json emitted unrequested fields: %s", out)
	}

	out, _, _ = h.run("env", "get", "proof-alpha", "-o", "yaml")
	if !strings.Contains(out, "slug: proof-alpha") {
		t.Fatalf("yaml detail: %s", out)
	}
}

// `env get` renders SERVICES and BUILDS sub-tables by hand, which must be
// suppressed for every machine format — including `--json`, which implies JSON
// without changing the -o flag. Getting this wrong emits a JSON object followed
// by a table: neither valid JSON nor a readable table.
func TestMachineFormatsEmitOnlyParseableOutput(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	for _, args := range [][]string{
		{"env", "get", "proof-alpha", "-o", "json"},
		{"env", "get", "proof-alpha", "--json", "slug,status"},
		{"doctor", "--json", "check,state"},
		{"version", "--json", "client_version"},
		{"auth", "status", "--json", "context"},
		{"env", "list", "--json", "slug"},
	} {
		out, _, _ := h.run(args...)
		var any0 any
		if err := json.Unmarshal([]byte(out), &any0); err != nil {
			t.Fatalf("%v produced unparseable stdout: %v\n%s", args, err, out)
		}
	}
}

// REGRESSION, and the most serious finding against v0.1: the stored credential
// used to be sent to any host named by --endpoint or DRIFT_ENDPOINT.
//
// The old test here exercised only the credential-LESS branch and its comment
// asserted the buggy behaviour was intended, which is why a green suite hid a
// token-exfiltration bug. This version runs with a credential present and an
// attacker-controlled endpoint that records what it is sent.
func TestStoredCredentialIsNeverSentToAForeignEndpoint(t *testing.T) {
	real := newFakeDrift(t, defaultDoc(""))
	attacker := newFakeDrift(t, defaultDoc(""))

	h := newHarness(t)
	h.setup(t, real, goodToken)

	for _, tc := range []struct {
		name string
		args []string
		env  bool
	}{
		{"--endpoint flag", []string{"--endpoint", attacker.URL, "env", "list"}, false},
		{"DRIFT_ENDPOINT", []string{"env", "list"}, true},
		{"auth status", []string{"--endpoint", attacker.URL, "auth", "status"}, false},
		{"doctor", []string{"--endpoint", attacker.URL, "doctor"}, false},
		{"env get", []string{"--endpoint", attacker.URL, "env", "get", "proof-alpha"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attacker.authHeaders = nil
			if tc.env {
				t.Setenv("DRIFT_ENDPOINT", attacker.URL)
				defer t.Setenv("DRIFT_ENDPOINT", "")
			}
			out, errOut, code := h.run(tc.args...)
			// `doctor` reports its findings on stdout, as a table; everything
			// else reports on stderr. Both are user-visible output.
			shown := out + errOut

			for _, got := range attacker.authHeaders {
				if strings.Contains(got, goodToken) {
					t.Fatalf("the credential was sent to a foreign endpoint: %q", got)
				}
			}
			if code == cliexit.OK {
				t.Fatalf("succeeded against a foreign endpoint (exit 0)\n%s", errOut)
			}
			// And the refusal has to explain itself, or the operator concludes
			// the CLI is broken.
			if !strings.Contains(shown, "refusing") && !strings.Contains(shown, "will NOT be sent") {
				t.Fatalf("the refusal was not explained: %s", shown)
			}
		})
	}
}

// DRIFT_TOKEN remains the way to authenticate against an ad-hoc endpoint:
// exporting a token is an explicit act naming a credential for what runs next.
func TestDriftTokenStillWorksAgainstAnAdHocEndpoint(t *testing.T) {
	real := newFakeDrift(t, defaultDoc(""))
	other := newFakeDrift(t, defaultDoc(""))

	h := newHarness(t)
	h.setup(t, real, "drift_stored-for-proof")
	h.store.EnvToken = goodToken

	out, errOut, code := h.run("--endpoint", other.URL, "env", "list")
	if code != cliexit.OK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "proof-alpha") {
		t.Fatalf("stdout: %s", out)
	}
	for _, got := range other.authHeaders {
		if strings.Contains(got, "drift_stored-for-proof") {
			t.Fatalf("the STORED credential leaked: %q", got)
		}
	}
}

// An --endpoint that names the context's own endpoint is a no-op and must keep
// working, or the fix would break the common `--endpoint $(same thing)` habit.
func TestEndpointEqualToTheContextKeepsTheCredential(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	_, errOut, code := h.run("--endpoint", srv.URL, "env", "list")
	if code != cliexit.OK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if len(srv.authHeaders) == 0 || !strings.Contains(srv.authHeaders[len(srv.authHeaders)-1], goodToken) {
		t.Fatalf("the credential was withheld from the context's own endpoint: %v", srv.authHeaders)
	}
}

// `auth login` must refuse to file a credential minted at one deployment under
// a context that points at another.
func TestLoginRefusesToStoreACredentialUnderTheWrongContext(t *testing.T) {
	real := newFakeDrift(t, defaultDoc(""))
	other := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, real, "")

	h.app.Stdin = strings.NewReader(goodToken + "\n")
	_, errOut, code := h.run("--endpoint", other.URL, "auth", "login", "--token-stdin")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d: %s", code, cliexit.Usage, errOut)
	}
	if _, err := h.store.Get(h.key); err == nil {
		t.Fatal("a credential for another deployment was filed under the context")
	}
}

// REGRESSION. A discovery document that names another host in services["api.v1"]
// must not redirect credentialled requests there. `@host` is the sharp form:
// Go parses `https://real@attacker/api/v1` with `attacker` as the host.
func TestPoisonedAPIPathDoesNotRedirectTheCredential(t *testing.T) {
	attacker := newFakeDrift(t, defaultDoc(""))

	doc := defaultDoc("")
	doc["services"] = map[string]string{
		"api.v1": "@" + strings.TrimPrefix(attacker.URL, "http://") + "/api/v1",
	}
	srv := newFakeDrift(t, doc)

	h := newHarness(t)
	h.setup(t, srv, goodToken)

	attacker.authHeaders = nil
	_, errOut, _ := h.run("env", "list")

	for _, got := range attacker.authHeaders {
		if strings.Contains(got, goodToken) {
			t.Fatalf("the credential was redirected by the discovery document: %q", got)
		}
	}
	// The CLI falls back to the default path and says so.
	if !strings.Contains(errOut, "unusable API path") {
		t.Fatalf("the rejection was not reported: %s", errOut)
	}
}

func TestVersionWorksWithNoConfiguration(t *testing.T) {
	h := newHarness(t)
	out, _, code := h.run("version")
	if code != cliexit.OK {
		t.Fatalf("`drift version` must work on a fresh install, exit %d: %s", code, h.stderr.String())
	}
	if !strings.Contains(out, "0.1.0") {
		t.Fatalf("client version missing: %s", out)
	}
	// The API contract comes from the vendored spec, so it is always available.
	if !strings.Contains(out, "1.0.0") {
		t.Fatalf("API contract version missing: %s", out)
	}
}

func TestVersionReportsTheServer(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	out, _, code := h.run("version", "-o", "json")
	if code != cliexit.OK {
		t.Fatal(h.stderr.String())
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	if v["server_version"] != "1.0.0" || v["api_path"] != "/api/v1" || v["context"] != "proof" {
		t.Fatalf("version payload: %#v", v)
	}
}

func TestDoctorReportsEveryCheckAndDumpsCapabilities(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	out, _, code := h.run("doctor")
	if code != cliexit.OK {
		t.Fatalf("doctor exit %d\n%s%s", code, out, h.stderr.String())
	}
	for _, want := range []string{"config", "network", "discovery", "auth", "version skew", "CAPABILITIES", "environments.read"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor omitted %q:\n%s", want, out)
		}
	}

	// Structured form carries the same facts for a machine.
	out, _, _ = h.run("doctor", "-o", "json")
	var d map[string]any
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("doctor -o json is not parseable: %v\n%s", err, out)
	}
	if d["org"] != "acme" || d["minimum_client_version"] != "0.1.0" {
		t.Fatalf("doctor json: %#v", d)
	}
}

func TestDoctorFailsWithTheRootCauseCode(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, "drift_revoked")

	out, _, code := h.run("doctor")
	if code != cliexit.AuthRequired {
		t.Fatalf("doctor exit %d, want %d\n%s", code, cliexit.AuthRequired, out)
	}
	if !strings.Contains(out, "fail") {
		t.Fatalf("the failing check is not marked:\n%s", out)
	}
}

// Unreachable endpoint: later checks are SKIPPED rather than failed, because a
// credential cannot be judged against a server that is not there.
func TestDoctorSkipsDependentChecks(t *testing.T) {
	h := newHarness(t)
	if _, _, code := h.run("context", "add", "dead", "--endpoint", "http://127.0.0.1:1"); code != 0 {
		t.Fatal("context add failed")
	}
	out, _, code := h.run("doctor")
	if code == cliexit.OK {
		t.Fatal("doctor passed against a dead endpoint")
	}
	if !strings.Contains(out, "skip") {
		t.Fatalf("dependent checks were not skipped:\n%s", out)
	}
}

func TestContextLifecycle(t *testing.T) {
	h := newHarness(t)

	if _, _, code := h.run("context", "add", "au", "--endpoint", "https://drift.au.example.com"); code != 0 {
		t.Fatal("add failed")
	}
	if _, _, code := h.run("context", "add", "en", "--endpoint", "https://drift.en.example.com"); code != 0 {
		t.Fatal("add failed")
	}

	// The first context added becomes current, so a fresh install is one
	// command from working.
	out, _, _ := h.run("context", "current", "-o", "json")
	var cur map[string]any
	if err := json.Unmarshal([]byte(out), &cur); err != nil {
		t.Fatal(err)
	}
	if cur["name"] != "au" {
		t.Fatalf("current = %#v", cur)
	}

	if _, _, code := h.run("context", "use", "en"); code != 0 {
		t.Fatal("use failed")
	}
	out, _, _ = h.run("context", "list")
	if !strings.Contains(out, "*") || !strings.Contains(out, "en") {
		t.Fatalf("list did not mark the current context:\n%s", out)
	}

	// Every "you have not configured this" failure is a usage error, so a script
	// sees one code for one condition rather than four depending on which
	// command noticed.
	if _, _, code := h.run("context", "use", "nope"); code != cliexit.Usage {
		t.Fatalf("use of an unknown context: exit %d, want %d", code, cliexit.Usage)
	}

	if _, _, code := h.run("context", "remove", "en"); code != 0 {
		t.Fatal("remove failed")
	}
	if _, _, code := h.run("context", "current"); code != cliexit.Usage {
		t.Fatal("current context was not cleared by removing it")
	}
}

func TestCompletionScripts(t *testing.T) {
	h := newHarness(t)
	for _, shell := range []string{"bash", "zsh", "fish"} {
		out, _, code := h.run("completion", shell)
		if code != cliexit.OK {
			t.Fatalf("completion %s exit %d", shell, code)
		}
		if len(out) < 100 || !strings.Contains(out, "drift") {
			t.Fatalf("completion %s produced nothing usable (%d bytes)", shell, len(out))
		}
	}
	if _, _, code := h.run("completion", "powershell"); code != cliexit.Usage {
		t.Fatalf("an unsupported shell must be a usage error, got %d", code)
	}
}

// The exit-code table must be in the help output, where a user looks for it.
func TestHelpDocumentsExitCodes(t *testing.T) {
	h := newHarness(t)
	out, _, _ := h.run("--help")
	if !strings.Contains(out, "authentication required") {
		t.Fatalf("root --help does not document the exit codes:\n%s", out)
	}
}

// REGRESSION. An unknown subcommand exited 1, which a script cannot tell apart
// from a server error. `cobra.NoArgs` fixed the original exit-0 bug but landed
// on the wrong code.
func TestUnknownSubcommandIsAUsageError(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"envv", "list"},
		{"env", "lst"},
		{"auth", "loginn"},
		{"context", "use", "a", "b"},
		{"stray-argument"},
	} {
		if _, _, code := h.run(args...); code != cliexit.Usage {
			t.Fatalf("%v exited %d, want %d", args, code, cliexit.Usage)
		}
	}
}

// REGRESSION. "no context configured" used to produce four different exit codes
// depending on which command noticed it. One condition, one code.
func TestNoContextIsAlwaysAUsageError(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"env", "list"},
		{"env", "get", "anything"},
		{"auth", "status"},
		{"auth", "login"},
		{"doctor"},
		{"context", "current"},
	} {
		_, _, code := h.run(args...)
		if code != cliexit.Usage {
			t.Fatalf("%v with no context exited %d, want %d", args, code, cliexit.Usage)
		}
	}
	// `version` is the deliberate exception: it must work on a fresh install
	// with nothing configured, and reports only what it can.
	if _, _, code := h.run("version"); code != cliexit.OK {
		t.Fatalf("version exited %d on a fresh install", code)
	}
}

// An unknown --context is the same condition and the same code, whether or not
// an endpoint override would otherwise make it moot.
func TestUnknownContextIsAlwaysAUsageError(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	for _, args := range [][]string{
		{"--context", "nope", "env", "list"},
		{"--context", "nope", "--endpoint", srv.URL, "env", "list"},
		{"--context", "nope", "doctor"},
		{"--context", "nope", "auth", "status"},
	} {
		if _, _, code := h.run(args...); code != cliexit.Usage {
			t.Fatalf("%v exited %d, want %d", args, code, cliexit.Usage)
		}
	}
}

// REGRESSION. `logout` reported success while leaving a live credential when
// the keyring could not be reached — the headless case the fallback exists for.
func TestLogoutFailsLoudlyWhenAStoreCannotConfirm(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	h.store.Backend = &brokenKeyring{}
	_, errOut, code := h.run("auth", "logout")
	if code == cliexit.OK {
		t.Fatalf("logout claimed success with an unreachable keyring: %s", errOut)
	}
	if !strings.Contains(errOut, "revoke") {
		t.Fatalf("the failure does not point at the remedy: %s", errOut)
	}
}

// ...and `context remove` must not discard the same failure, or a live token is
// left filed under a name nothing references any more.
func TestContextRemoveReportsAnUndeletableCredential(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	h.store.Backend = &brokenKeyring{}
	_, errOut, code := h.run("context", "remove", "proof")
	if code == cliexit.OK {
		t.Fatalf("context remove swallowed a credential-delete failure: %s", errOut)
	}
	if !strings.Contains(errOut, "revoke") {
		t.Fatalf("the failure does not point at the remedy: %s", errOut)
	}
}

// brokenKeyring is reachable for nothing: every call is a store-level failure,
// never an absence.
type brokenKeyring struct{}

func (brokenKeyring) Get(string, string) (string, error) {
	return "", errors.New("keyring unavailable")
}
func (brokenKeyring) Set(string, string, string) error { return errors.New("keyring unavailable") }
func (brokenKeyring) Delete(string, string) error      { return errors.New("keyring unavailable") }

// Re-pointing a context at a different deployment must not orphan the
// credential filed against the old address: keyed on that endpoint, it would be
// unreachable through both `auth logout` and `context remove`.
func TestContextAddClearsTheCredentialForAReplacedEndpoint(t *testing.T) {
	first := newFakeDrift(t, defaultDoc(""))
	second := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, first, goodToken)

	oldKey := auth.NewKey("proof", first.URL)
	if _, err := h.store.Get(oldKey); err != nil {
		t.Fatalf("precondition: %v", err)
	}

	if _, _, code := h.run("context", "add", "proof", "--endpoint", second.URL); code != cliexit.OK {
		t.Fatalf("context add exited %d", code)
	}
	if _, err := h.store.Get(oldKey); !errors.Is(err, auth.ErrNoCredential) {
		t.Fatalf("the credential for the previous endpoint was orphaned: %v", err)
	}
	// ...and the new endpoint has no credential, so the CLI asks for one rather
	// than reusing the old deployment's.
	if _, _, code := h.run("env", "list"); code != cliexit.AuthRequired {
		t.Fatalf("exit %d, want %d", code, cliexit.AuthRequired)
	}
}

// MEDIUM regression. The keyring is global to the user account, so a credential
// filed under the bare context name is shared by every config directory using
// that name. It is no longer used as a credential — but the apparent logout is
// explained rather than left as a mystery, and logout still clears it.
func TestLegacyKeyedCredentialIsRefusedButExplained(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, "")

	// Exactly what the previous keying wrote.
	if err := h.store.Backend.Set(auth.KeyringService, "proof", goodToken); err != nil {
		t.Fatal(err)
	}

	_, errOut, code := h.run("env", "list")
	if code != cliexit.AuthRequired {
		t.Fatalf("a legacy credential was used: exit %d\n%s", code, errOut)
	}
	for _, want := range []string{"earlier version", "auth login"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("the apparent logout was not explained (%q missing): %s", want, errOut)
		}
	}

	// Logging in again works and re-keys it.
	h.app.Stdin = strings.NewReader(goodToken + "\n")
	if _, _, code := h.run("auth", "login", "--token-stdin"); code != cliexit.OK {
		t.Fatalf("login exited %d: %s", code, h.stderr.String())
	}
	if _, _, code := h.run("env", "list"); code != cliexit.OK {
		t.Fatalf("exit %d after re-login", code)
	}
	// ...and the pre-upgrade entry is gone, not merely shadowed.
	if v, err := h.store.Backend.Get(auth.KeyringService, "proof"); err == nil && v != "" {
		t.Fatal("the pre-upgrade entry survived the re-login")
	}

	// Logout clears both keys.
	if _, _, code := h.run("auth", "logout"); code != cliexit.OK {
		t.Fatalf("logout exited %d", code)
	}
	if _, _, code := h.run("env", "list"); code != cliexit.AuthRequired {
		t.Fatalf("exit %d after logout", code)
	}
}

// An unresolved keyring/file conflict is reported with the two commands that
// settle it, never silently resolved.
func TestAmbiguousCredentialIsReportedWithARemedy(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	// A file copy written while the keyring was unreadable, naming a different
	// token, with no evidence of what it superseded.
	broken := &intermittentKeyring{inner: h.store.Backend}
	h.store.Backend = broken
	broken.down = true
	if _, err := h.store.Set(h.key, "drift_rotated-elsewhere"); err != nil {
		t.Fatal(err)
	}
	broken.down = false

	_, errOut, code := h.run("env", "list")
	if code != cliexit.AuthRequired {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.AuthRequired, errOut)
	}
	for _, want := range []string{"two different credentials", "auth login", "auth logout"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("the conflict report is missing %q: %s", want, errOut)
		}
	}

	// `auth logout` settles it, as advertised.
	if _, _, code := h.run("auth", "logout"); code != cliexit.OK {
		t.Fatalf("logout exited %d", code)
	}
	if _, _, code := h.run("env", "list"); code != cliexit.AuthRequired {
		t.Fatalf("exit %d after logout", code)
	}
}

// intermittentKeyring can be taken down and brought back, which is the shape of
// an SSH session against a desktop that has a keyring.
type intermittentKeyring struct {
	inner auth.Backend
	down  bool
}

func (k *intermittentKeyring) Get(s, u string) (string, error) {
	if k.down {
		return "", errors.New("keyring unavailable")
	}
	return k.inner.Get(s, u)
}

func (k *intermittentKeyring) Set(s, u, p string) error {
	if k.down {
		return errors.New("keyring unavailable")
	}
	return k.inner.Set(s, u, p)
}

func (k *intermittentKeyring) Delete(s, u string) error {
	if k.down {
		return errors.New("keyring unavailable")
	}
	return k.inner.Delete(s, u)
}

func TestAuthStatusShowsWhoamiColumns(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc(""))
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	out, errOut, code := h.run("auth", "status", "-o", "wide")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for _, want := range []string{"operator@example.com", "admin", "desktop", "cli"} {
		if !strings.Contains(out, want) {
			t.Fatalf("auth status output is missing %q:\n%s", want, out)
		}
	}
}

// When whoami returns credential: null (a cookie caller -- impossible from the
// CLI but allowed by the schema), owner and role render, credential columns
// stay empty, and no expiry warning fires.
func TestAuthStatusWhoamiCredentialNull(t *testing.T) {
	mux := http.NewServeMux()
	doc, _ := json.Marshal(defaultDoc(""))
	mux.HandleFunc("/.well-known/drift.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	})
	problem := func(w http.ResponseWriter, status int, code, msg, ptype, detail string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"defined": true, "code": code, "status": status, "message": msg,
			"data": map[string]any{"type": ptype, "detail": detail},
		})
	}
	mux.HandleFunc("/api/v1/environments", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			problem(w, 401, "UNAUTHORIZED", "Authentication required",
				"urn:drift:problem:unauthenticated", "")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":      []any{},
			"pagination": map[string]any{"limit": 20, "offset": 0, "hasMore": false},
		})
	})
	mux.HandleFunc("/api/v1/auth/whoami", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			problem(w, 401, "UNAUTHORIZED", "Authentication required",
				"urn:drift:problem:unauthenticated", "")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"email": "cookie-user@example.com", "role": "release", "channel": "api",
			"credential": nil,
		})
	})

	fsrv := httptest.NewServer(mux)
	t.Cleanup(fsrv.Close)
	srv := &fakeServer{Server: fsrv}

	h := newHarness(t)
	h.setup(t, srv, goodToken)

	// Table output: owner and role present, exit 0.
	out, errOut, code := h.run("auth", "status", "-o", "wide")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for _, want := range []string{"cookie-user@example.com", "release"} {
		if !strings.Contains(out, want) {
			t.Fatalf("auth status output is missing %q:\n%s", want, out)
		}
	}
	// No expiry warning when credential is null.
	if strings.Contains(errOut, "within 24 hours") {
		t.Fatalf("expiry warning should not fire when credential is null:\n%s", errOut)
	}

	// JSON output: label, scopes, expires should be absent keys.
	jout, jerrOut, jcode := h.run("auth", "status", "-o", "json")
	if jcode != cliexit.OK {
		t.Fatalf("exit %d\n%s", jcode, jerrOut)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(jout), &parsed); err != nil {
		t.Fatalf("cannot parse JSON output: %v\n%s", err, jout)
	}
	if parsed["owner"] != "cookie-user@example.com" {
		t.Fatalf("owner missing or wrong in JSON: %v", parsed["owner"])
	}
	if parsed["role"] != "release" {
		t.Fatalf("role missing or wrong in JSON: %v", parsed["role"])
	}
	for _, absent := range []string{"label", "scopes", "expires"} {
		if v, ok := parsed[absent]; ok && v != nil {
			t.Fatalf("JSON key %q should be absent or null when credential is null, got %v", absent, v)
		}
	}
}

func TestAuthStatusWhoamiExpiryWarning(t *testing.T) {
	// Build a custom fake that returns a credential expiring within 24 hours.
	mux := http.NewServeMux()
	doc, _ := json.Marshal(defaultDoc(""))
	mux.HandleFunc("/.well-known/drift.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	})
	problem := func(w http.ResponseWriter, status int, code, msg, ptype, detail string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"defined": true, "code": code, "status": status, "message": msg,
			"data": map[string]any{"type": ptype, "detail": detail},
		})
	}
	mux.HandleFunc("/api/v1/environments", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			problem(w, 401, "UNAUTHORIZED", "Authentication required",
				"urn:drift:problem:unauthenticated", "")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":      []any{},
			"pagination": map[string]any{"limit": 20, "offset": 0, "hasMore": false},
		})
	})
	// Whoami with an expiry 30 minutes from now.
	soon := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	mux.HandleFunc("/api/v1/auth/whoami", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			problem(w, 401, "UNAUTHORIZED", "Authentication required",
				"urn:drift:problem:unauthenticated", "")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"email": "operator@example.com", "role": "release", "channel": "cli",
			"credential": map[string]any{
				"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "label": "ephemeral",
				"scopes": []string{"promote:prd"}, "expiresAt": soon,
			},
		})
	})

	fsrv := httptest.NewServer(mux)
	t.Cleanup(fsrv.Close)
	srv := &fakeServer{Server: fsrv}

	h := newHarness(t)
	h.setup(t, srv, goodToken)

	out, errOut, code := h.run("auth", "status", "-o", "wide")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "promote:prd") {
		t.Fatalf("scopes missing from output:\n%s", out)
	}
	if !strings.Contains(errOut, "within 24 hours") {
		t.Fatalf("24-hour expiry warning missing from stderr:\n%s", errOut)
	}
}

func TestAuthStatusWhoamiExpiredCredential(t *testing.T) {
	mux := http.NewServeMux()
	doc, _ := json.Marshal(defaultDoc(""))
	mux.HandleFunc("/.well-known/drift.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	})
	problem := func(w http.ResponseWriter, status int, code, msg, ptype, detail string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"defined": true, "code": code, "status": status, "message": msg,
			"data": map[string]any{"type": ptype, "detail": detail},
		})
	}
	mux.HandleFunc("/api/v1/environments", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			problem(w, 401, "UNAUTHORIZED", "Authentication required",
				"urn:drift:problem:unauthenticated", "")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":      []any{},
			"pagination": map[string]any{"limit": 20, "offset": 0, "hasMore": false},
		})
	})
	// Whoami with an expiry in the past (already expired).
	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	mux.HandleFunc("/api/v1/auth/whoami", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			problem(w, 401, "UNAUTHORIZED", "Authentication required",
				"urn:drift:problem:unauthenticated", "")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"email": "operator@example.com", "role": "release", "channel": "cli",
			"credential": map[string]any{
				"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "label": "stale",
				"scopes": []string{}, "expiresAt": past,
			},
		})
	})

	fsrv := httptest.NewServer(mux)
	t.Cleanup(fsrv.Close)
	srv := &fakeServer{Server: fsrv}

	h := newHarness(t)
	h.setup(t, srv, goodToken)

	_, errOut, code := h.run("auth", "status")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "expired at") {
		t.Fatalf("expected 'expired at' warning, got:\n%s", errOut)
	}
	if strings.Contains(errOut, "within 24 hours") {
		t.Fatalf("'within 24 hours' should not appear for an already-expired credential:\n%s", errOut)
	}
}
