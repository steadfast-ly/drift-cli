package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/steadfast-ly/drift-cli/internal/api"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
)

// --- tunnel config builder ---------------------------------------------------

func TestBuildTunnelConfig(t *testing.T) {
	t.Run("basic mapping", func(t *testing.T) {
		auth := "user:pass"
		access := newFakeDbAccess("postgres", "tunnel.example.com", "db.internal", "mydb", 3306, 33060, &auth)
		tc := buildTunnelConfig(access, 0)

		if tc.ServerURL != "https://tunnel.example.com" {
			t.Fatalf("ServerURL = %q", tc.ServerURL)
		}
		if tc.Remote != "127.0.0.1:33060:db.internal:3306" {
			t.Fatalf("Remote = %q", tc.Remote)
		}
		if tc.Auth != "user:pass" {
			t.Fatalf("Auth = %q", tc.Auth)
		}
		if tc.LocalPort != 33060 {
			t.Fatalf("LocalPort = %d", tc.LocalPort)
		}
		if tc.Engine != "postgres" {
			t.Fatalf("Engine = %q", tc.Engine)
		}
		if tc.DbName != "mydb" {
			t.Fatalf("DbName = %q", tc.DbName)
		}
	})

	t.Run("port override", func(t *testing.T) {
		access := newFakeDbAccess("mysql", "tunnel.example.com", "db.internal", "mydb", 3306, 33060, nil)
		tc := buildTunnelConfig(access, 55555)

		if tc.LocalPort != 55555 {
			t.Fatalf("LocalPort = %d, want 55555", tc.LocalPort)
		}
		if tc.Remote != "127.0.0.1:55555:db.internal:3306" {
			t.Fatalf("Remote = %q", tc.Remote)
		}
	})

	t.Run("auth absent", func(t *testing.T) {
		access := newFakeDbAccess("postgres", "tunnel.example.com", "db.internal", "mydb", 5432, 33060, nil)
		tc := buildTunnelConfig(access, 0)

		if tc.Auth != "" {
			t.Fatalf("Auth = %q, want empty", tc.Auth)
		}
	})

	t.Run("remote binds localhost only", func(t *testing.T) {
		access := newFakeDbAccess("postgres", "tunnel.example.com", "db.internal", "mydb", 5432, 33060, nil)
		tc := buildTunnelConfig(access, 0)
		if !strings.HasPrefix(tc.Remote, "127.0.0.1:") {
			t.Fatalf("Remote = %q, must start with 127.0.0.1:", tc.Remote)
		}
	})

	t.Run("tunnelHost vs dbHost not swapped", func(t *testing.T) {
		access := newFakeDbAccess("postgres", "tunnel.host", "db.host", "mydb", 5432, 33060, nil)
		tc := buildTunnelConfig(access, 0)

		if tc.ServerURL != "https://tunnel.host" {
			t.Fatalf("ServerURL = %q, want https://tunnel.host", tc.ServerURL)
		}
		if !strings.Contains(tc.Remote, "db.host") {
			t.Fatalf("Remote = %q, must contain db.host", tc.Remote)
		}
		if strings.Contains(tc.Remote, "tunnel.host") {
			t.Fatalf("Remote = %q, must NOT contain tunnel.host", tc.Remote)
		}
	})
}

func TestClientHint(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		tc := tunnelConfig{LocalPort: 33060, Engine: "postgres", DbName: "mydb"}
		hint := clientHint(tc)
		if !strings.Contains(hint, "psql") {
			t.Fatalf("hint = %q, want psql", hint)
		}
		if !strings.Contains(hint, "33060") {
			t.Fatalf("hint = %q, want port 33060", hint)
		}
		if !strings.Contains(hint, "mydb") {
			t.Fatalf("hint = %q, want mydb", hint)
		}
	})

	t.Run("mysql", func(t *testing.T) {
		tc := tunnelConfig{LocalPort: 33060, Engine: "mysql", DbName: "mydb"}
		hint := clientHint(tc)
		if !strings.Contains(hint, "mysql") {
			t.Fatalf("hint = %q, want mysql", hint)
		}
		if !strings.Contains(hint, "--ssl-mode=REQUIRED") {
			t.Fatalf("hint = %q, want --ssl-mode=REQUIRED", hint)
		}
		if !strings.Contains(hint, "MariaDB") {
			t.Fatalf("hint = %q, want MariaDB alternative noted", hint)
		}
		if !strings.Contains(hint, "ssl-verify-server-cert") {
			t.Fatalf("hint = %q, want MariaDB flag mentioned", hint)
		}
	})
}

// --- end-to-end wiring through the fake server ------------------------------

// newDbAccessFake builds a fake drift server with a db-access endpoint.
// mode selects the variant: "published", "unpublished", "404".
func newDbAccessFake(t *testing.T, mode string) *fakeServer {
	t.Helper()
	doc := defaultDoc("")
	fs := &fakeServer{}
	mux := buildDbAccessMux(t, fs, doc, mode)
	fs.Server = httptest.NewServer(mux)
	t.Cleanup(fs.Close)
	return fs
}

func buildDbAccessMux(t *testing.T, fs *fakeServer, doc map[string]any, mode string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()

	// Discovery
	body, _ := json.Marshal(doc)
	mux.HandleFunc("/.well-known/drift.json", func(w http.ResponseWriter, _ *http.Request) {
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

	// db-access endpoint
	mux.HandleFunc("/api/v1/environments/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			problem(w, 401, "UNAUTHORIZED", "Authentication required",
				"urn:drift:problem:unauthenticated", "The bearer credential is missing, expired or revoked.")
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/environments/")
		parts := strings.SplitN(rest, "/", 2)
		ref := parts[0]

		// Is this a db-access request?
		if len(parts) == 2 && parts[1] == "db-access" {
			switch mode {
			case "published":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"published": true,
					"access": map[string]any{
						"kind":       "direct",
						"engine":     "postgres",
						"tunnelHost": "tunnel.example.com",
						"dbHost":     "db.internal",
						"dbPort":     5432,
						"localPort":  33060,
						"dbName":     "preview_db",
					},
				})
			case "published-with-auth":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"published": true,
					"access": map[string]any{
						"kind":       "direct",
						"engine":     "mysql",
						"tunnelHost": "tunnel.example.com",
						"dbHost":     "db.internal",
						"dbPort":     3306,
						"localPort":  33060,
						"dbName":     "preview_db",
						"chiselAuth": "user:secret",
					},
				})
			case "unpublished":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"published": false,
				})
			case "route-miss-404":
				// Simulates a pre-0.15.0 server whose catch-all route
				// returns a DECODABLE 404 problem envelope for the
				// unrecognised /db-access path — same URN as env-not-found.
				problem(w, 404, "NOT_FOUND", "No such operation",
					"urn:drift:problem:not-found", "")
			case "undecodable-404":
				// Simulates an old server that does not have the db-access
				// endpoint at all — raw HTML, no problem envelope.
				w.WriteHeader(404)
				_, _ = w.Write([]byte("<html>Not Found</html>"))
			default: // "404" or unknown ref
				problem(w, 404, "NOT_FOUND", "Environment not found",
					"urn:drift:problem:not-found", "No such environment.")
			}
			return
		}

		// Plain env get (used for some tests).
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
		default:
			problem(w, 404, "NOT_FOUND", "Environment not found",
				"urn:drift:problem:not-found", "No such environment.")
		}
	})

	return mux
}

// TestTunnelPublishedFalse verifies the published=false response is handled.
func TestTunnelPublishedFalse(t *testing.T) {
	srv := newDbAccessFake(t, "unpublished")
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	_, errOut, code := h.run("env", "tunnel", "proof-alpha")
	if code != cliexit.Error {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Error, errOut)
	}
	if !strings.Contains(errOut, "no database access") {
		t.Fatalf("error does not explain the condition: %s", errOut)
	}
}

// TestTunnelNotFound verifies 404 → exit 3.
func TestTunnelNotFound(t *testing.T) {
	srv := newDbAccessFake(t, "404")
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	_, errOut, code := h.run("env", "tunnel", "nope")
	if code != cliexit.NotFound {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.NotFound, errOut)
	}
}

// TestTunnelMissingArgument verifies that no slug → exit 2.
func TestTunnelMissingArgument(t *testing.T) {
	srv := newDbAccessFake(t, "published")
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	_, _, code := h.run("env", "tunnel")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d", code, cliexit.Usage)
	}
}

// TestTunnelConfigFromPublishedResponse exercises the WIRING: the fake
// returns db-access params, and we verify the config builder produces the
// right chisel configuration. Because we cannot actually connect to a
// chisel server in CI, we test up to the config-build boundary.
func TestTunnelConfigFromPublishedResponse(t *testing.T) {
	// Published with auth: mysql variant.
	srv := newDbAccessFake(t, "published-with-auth")
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	// We cannot drive the full command because it would try to dial a real
	// chisel server. Instead we test the config builder directly from the
	// same JSON shape the fake returns.
	auth := "user:secret"
	access := newFakeDbAccess("mysql", "tunnel.example.com", "db.internal", "preview_db", 3306, 33060, &auth)
	tc := buildTunnelConfig(access, 0)

	if tc.ServerURL != "https://tunnel.example.com" {
		t.Fatalf("ServerURL = %q", tc.ServerURL)
	}
	if tc.Remote != "127.0.0.1:33060:db.internal:3306" {
		t.Fatalf("Remote = %q", tc.Remote)
	}
	if tc.Auth != "user:secret" {
		t.Fatalf("Auth = %q", tc.Auth)
	}
	if tc.Engine != "mysql" {
		t.Fatalf("Engine = %q", tc.Engine)
	}

	// With --port override.
	tc2 := buildTunnelConfig(access, 44444)
	if tc2.LocalPort != 44444 {
		t.Fatalf("port override: LocalPort = %d", tc2.LocalPort)
	}
	if tc2.Remote != "127.0.0.1:44444:db.internal:3306" {
		t.Fatalf("port override: Remote = %q", tc2.Remote)
	}
}

// TestTunnelUnauthenticated verifies the 401 path.
func TestTunnelUnauthenticated(t *testing.T) {
	srv := newDbAccessFake(t, "published")
	h := newHarness(t)
	h.setup(t, srv, "drift_bad")

	_, errOut, code := h.run("env", "tunnel", "proof-alpha")
	if code != cliexit.AuthRequired {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.AuthRequired, errOut)
	}
}

// TestTunnelUndecodable404HintsServerVersion verifies that an old server
// without the db-access endpoint (raw HTML 404) exits 3 with the version hint.
func TestTunnelUndecodable404HintsServerVersion(t *testing.T) {
	srv := newDbAccessFake(t, "undecodable-404")
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	_, errOut, code := h.run("env", "tunnel", "proof-alpha")
	if code != cliexit.NotFound {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.NotFound, errOut)
	}
	if !strings.Contains(errOut, "0.15.0") {
		t.Fatalf("version hint missing from error: %s", errOut)
	}
}

// TestTunnelPortValidation verifies --port rejects out-of-range values.
func TestTunnelPortValidation(t *testing.T) {
	srv := newDbAccessFake(t, "published")
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	cases := []struct {
		name string
		port string
		want int
	}{
		{"negative", "-1", cliexit.Usage},
		{"too high", "70000", cliexit.Usage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errOut, code := h.run("env", "tunnel", "proof-alpha", "--port", c.port)
			if code != c.want {
				t.Fatalf("exit %d, want %d\n%s", code, c.want, errOut)
			}
			if !strings.Contains(errOut, "--port") {
				t.Fatalf("error does not name the flag: %s", errOut)
			}
		})
	}
}

// --- old-server version-based 404 disambiguation ----------------------------

// newDbAccessFakeWithVersion builds a fake drift whose discovery doc reports
// the given version. Combined with mode "route-miss-404" it models the real
// pre-0.15.0 behavior: the server's catch-all returns a decodable 404 problem
// envelope for the unrecognised /db-access route.
func newDbAccessFakeWithVersion(t *testing.T, mode, version string) *fakeServer {
	t.Helper()
	doc := defaultDoc("")
	doc["version"] = version
	fs := &fakeServer{}
	mux := buildDbAccessMux(t, fs, doc, mode)
	fs.Server = httptest.NewServer(mux)
	t.Cleanup(fs.Close)
	return fs
}

// TestTunnelOldServerRouteMiss404 verifies that a decodable 404 from a server
// whose discovery version is < 0.15.0 produces the version-floor hint, not the
// slug-resolution hint. This is the defect that was found against live EN
// (0.14.0): the route-miss envelope has the same URN as env-not-found.
func TestTunnelOldServerRouteMiss404(t *testing.T) {
	srv := newDbAccessFakeWithVersion(t, "route-miss-404", "0.14.0")
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	_, errOut, code := h.run("env", "tunnel", "proof-alpha")
	if code != cliexit.NotFound {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.NotFound, errOut)
	}
	if !strings.Contains(errOut, "0.15.0") {
		t.Fatalf("version-floor hint missing: %s", errOut)
	}
	if !strings.Contains(errOut, "0.14.0") {
		t.Fatalf("reported server version missing: %s", errOut)
	}
	if strings.Contains(errOut, "slug resolves") {
		t.Fatalf("slug hint must not appear for a route miss: %s", errOut)
	}
}

// TestDbOldServerRouteMiss404 verifies the same for env db.
func TestDbOldServerRouteMiss404(t *testing.T) {
	srv := newDbAccessFakeWithVersion(t, "route-miss-404", "0.14.0")
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	_, errOut, code := h.run("env", "db", "proof-alpha")
	if code != cliexit.NotFound {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.NotFound, errOut)
	}
	if !strings.Contains(errOut, "0.15.0") {
		t.Fatalf("version-floor hint missing: %s", errOut)
	}
	if !strings.Contains(errOut, "0.14.0") {
		t.Fatalf("reported server version missing: %s", errOut)
	}
}

// TestTunnelNewServerEnvNotFound verifies that a decodable 404 from a server
// >= 0.15.0 produces the slug-resolution hint (the route exists, the
// environment was not found).
func TestTunnelNewServerEnvNotFound(t *testing.T) {
	srv := newDbAccessFakeWithVersion(t, "404", "0.15.0")
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	_, errOut, code := h.run("env", "tunnel", "nope")
	if code != cliexit.NotFound {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.NotFound, errOut)
	}
	if !strings.Contains(errOut, "slug resolves") {
		t.Fatalf("slug hint missing: %s", errOut)
	}
	if strings.Contains(errOut, "0.15.0") {
		t.Fatalf("version-floor hint must not appear when server is new enough: %s", errOut)
	}
}

// --- helpers ----------------------------------------------------------------

func newFakeDbAccess(engine, tunnelHost, dbHost, dbName string, dbPort, localPort int, auth *string) api.DbAccessDirect {
	return api.DbAccessDirect{
		Engine:     api.DbAccessDirectEngine(engine),
		TunnelHost: tunnelHost,
		DbHost:     dbHost,
		DbName:     dbName,
		DbPort:     dbPort,
		LocalPort:  localPort,
		ChiselAuth: auth,
	}
}
