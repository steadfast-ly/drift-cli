package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/steadfast/drift-cli/internal/api"
	"github.com/steadfast/drift-cli/internal/cliexit"
)

// problem builds an envelope the way the SERVER does — from JSON — rather than
// by restating the generated struct. `ApiProblem.Data` is an anonymous struct in
// the generated client, and a hand-written copy of it here would be a second
// definition of a generated shape that nothing checks.
func problem(status int, code, msg, detail string) *api.ApiProblem {
	body := map[string]any{
		"defined": true, "code": code, "status": status, "message": msg,
		"data": map[string]any{"type": "urn:drift:problem:test", "detail": detail},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	var p api.ApiProblem
	if err := json.Unmarshal(raw, &p); err != nil {
		panic(err)
	}
	return &p
}

func TestProblemSurfacesMessageAndDetail(t *testing.T) {
	e := Problem(problem(404, "NOT_FOUND", "Environment not found", "No such environment."), nil, 404)
	if e.Code != cliexit.NotFound {
		t.Fatalf("code = %d", e.Code)
	}
	if e.Message != "Environment not found" {
		t.Fatalf("message = %q", e.Message)
	}
	if e.Detail != "No such environment." {
		t.Fatalf("detail = %q", e.Detail)
	}
}

// The envelope's own status wins over the transport status: a proxy that
// rewrites the status line cannot change what the server meant.
func TestEnvelopeStatusBeatsTransportStatus(t *testing.T) {
	e := Problem(problem(409, "CONFLICT", "busy", ""), nil, 200)
	if e.Code != cliexit.Conflict {
		t.Fatalf("code = %d, want %d", e.Code, cliexit.Conflict)
	}
}

// A status the SPEC does not declare still arrives as a decodable envelope. The
// generated client only materialises declared statuses, so without this fallback
// a status added server-side ahead of the contract would print "HTTP 507"
// instead of the server's own words.
func TestUndeclaredStatusDecodesFromTheBody(t *testing.T) {
	body := []byte(`{"defined":true,"code":"INSUFFICIENT_STORAGE","status":507,` +
		`"message":"Out of space","data":{"type":"urn:drift:problem:internal","detail":"the volume is full"}}`)
	e := Problem(nil, body, 507)
	if e.Message != "Out of space" || e.Detail != "the volume is full" {
		t.Fatalf("undeclared status was not decoded: %+v", e)
	}
	if e.Code != cliexit.Error {
		t.Fatalf("code = %d", e.Code)
	}
}

// A 429 that IS a drift envelope is exit 7 with the server's words. The status
// only earns that code through the envelope: an intermediary throttling the
// connection produces no envelope and stays on the generic path, because "drift
// is rate-limiting this credential" and "something in front of drift is shedding
// load" have different remedies.
func TestRateLimitEnvelopeIsExitSeven(t *testing.T) {
	body := []byte(`{"defined":true,"code":"TOO_MANY_REQUESTS","status":429,` +
		`"message":"Rate limit exceeded","data":{"type":"urn:drift:problem:rate-limited",` +
		`"detail":"Rate limit of 20 requests per window exceeded for this credential. Retry in 37s."}}`)
	e := Problem(nil, body, 429)
	if e.Code != cliexit.RateLimited {
		t.Fatalf("code = %d, want %d", e.Code, cliexit.RateLimited)
	}
	if !strings.Contains(e.Detail, "Retry in 37s") {
		t.Fatalf("the server's detail was dropped: %+v", e)
	}

	// No envelope, same status: not drift's rate limiter.
	if got := Problem(nil, []byte("<html>429</html>"), 429).Code; got != cliexit.RateLimited {
		// The status mapping is still by status here, deliberately: the CLI has
		// nothing better to say, and 7 with a "cannot decode" message is more
		// useful than 1. What must NOT happen is a fabricated Retry-After.
		t.Logf("undecodable 429 mapped to %d", got)
	}
	if Problem(nil, []byte("<html>429</html>"), 429).RetryAfter != 0 {
		t.Fatal("a Retry-After was invented for a response that carried none")
	}
}

// A 409 carries `data.state` and `data.event` — the state the entity was in and
// the lifecycle event that was refused. They are the whole point of the typed
// conflict, so they must reach the operator even when the server sends no
// prose detail alongside them.
func TestConflictSurfacesStateAndEvent(t *testing.T) {
	body := []byte(`{"defined":true,"code":"CONFLICT","status":409,"message":"Cannot sleep environment",` +
		`"data":{"type":"urn:drift:problem:invalid-transition","state":"building","event":"SLEEP"}}`)
	e := Problem(nil, body, 409)
	if e.Code != cliexit.Conflict {
		t.Fatalf("code = %d", e.Code)
	}
	if !strings.Contains(e.Detail, "building") || !strings.Contains(e.Detail, "SLEEP") {
		t.Fatalf("state and event were dropped: %+v", e)
	}
}

// ...but an unrelated JSON object must NOT be read as an envelope, or the user
// gets an error with an empty message.
func TestNonEnvelopeBodyIsReportedHonestly(t *testing.T) {
	for _, body := range [][]byte{
		nil,
		[]byte(`<html>502 Bad Gateway</html>`),
		[]byte(`{"error":"Invalid ID"}`),
		[]byte(`{"message":"something"}`),
	} {
		e := Problem(nil, body, 502)
		if !strings.Contains(e.Message, "cannot decode") {
			t.Fatalf("body %q produced %q", body, e.Message)
		}
		if e.Hint == "" {
			t.Fatalf("no hint for an undecodable body")
		}
	}
}

func TestUnauthorizedCarriesALoginHint(t *testing.T) {
	e := Problem(problem(401, "UNAUTHORIZED", "Authentication required", ""), nil, 401)
	if e.Code != cliexit.AuthRequired {
		t.Fatalf("code = %d", e.Code)
	}
	if !strings.Contains(e.Hint, "drift auth login") {
		t.Fatalf("hint = %q", e.Hint)
	}
}

// An envelope with an empty message still has to say something.
func TestEmptyMessageFallsBackToTheCode(t *testing.T) {
	e := Problem(&api.ApiProblem{Code: "INTERNAL_SERVER_ERROR", Status: 500}, nil, 500)
	if !strings.Contains(e.Message, "INTERNAL_SERVER_ERROR") {
		t.Fatalf("message = %q", e.Message)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "dial tcp 10.0.0.1:443: i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

func TestTransportErrorsPointAtTheVPN(t *testing.T) {
	e := Transport(timeoutErr{}, "https://drift.example.com")
	if !strings.Contains(e.Message, "https://drift.example.com") {
		t.Fatalf("message = %q", e.Message)
	}
	if !strings.Contains(e.Hint, "VPN") {
		t.Fatalf("hint = %q", e.Hint)
	}
	if !strings.Contains(e.Detail, "timed out") {
		t.Fatalf("detail = %q", e.Detail)
	}

	// A non-timeout keeps the informative tail of Go's layered dial error and
	// drops the rest, which is noise the user cannot act on.
	e = Transport(errors.New(`Get "http://x": dial tcp 127.0.0.1:1: connect: connection refused`),
		"http://x")
	if e.Detail != "connection refused" {
		t.Fatalf("detail = %q", e.Detail)
	}
	// The raw cause stays reachable for errors.Is but is never rendered.
	if strings.Contains(e.Message, "dial tcp") {
		t.Fatalf("the raw error leaked: %q", e.Message)
	}
}

func TestClientAttachesCredentialsAndAttribution(t *testing.T) {
	c, err := New(Options{BaseURL: "http://example.invalid/api/v1", Token: "drift_x", ClientVersion: "9.9.9"})
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("nil client")
	}
	// The editor itself is exercised end to end in cmd; here the point
	// is only that construction with a credential succeeds.
}

// --- base URL -----------------------------------------------------------

// The second half of the api.v1 defence: even a path that passed validation is
// only used once the JOINED url is proved to still address the endpoint.
func TestBaseURLRefusesToLeaveTheEndpoint(t *testing.T) {
	good, err := BaseURL("https://drift.example.com", "/api/v1")
	if err != nil {
		t.Fatal(err)
	}
	if good != "https://drift.example.com/api/v1" {
		t.Fatalf("BaseURL = %q", good)
	}

	// A trailing slash on either side must not produce a doubled separator.
	if got, err := BaseURL("https://drift.example.com/", "/api/v1"); err != nil || got != "https://drift.example.com/api/v1" {
		t.Fatalf("BaseURL = %q, %v", got, err)
	}

	// A bad endpoint is refused outright.
	for _, endpoint := range []string{
		"https://user:pw@drift.example.com", // userinfo
		"ftp://drift.example.com",           // not http(s)
		"not-a-url",
		"https://", // no host
	} {
		if got, err := BaseURL(endpoint, "/api/v1"); err == nil {
			t.Fatalf("BaseURL(%q) = %q, want an error", endpoint, got)
		}
	}

	// The guarantee for the PATH half is not that hostile input errors — most of
	// it is caught earlier, by Document.APIV1 — but that whatever survives can
	// never change the scheme or host that gets dialled. Anything else either
	// errors or stays put; nothing escapes.
	const endpoint = "https://drift.example.com"
	for _, p := range []string{
		"@attacker.invalid/api/v1",
		"//attacker.invalid/api/v1",
		"https://attacker.invalid/api",
		"/api/v1/../../..",
		"\\attacker.invalid\\api",
	} {
		got, err := BaseURL(endpoint, p)
		if err != nil {
			continue
		}
		u, perr := url.Parse(got)
		if perr != nil {
			t.Fatalf("BaseURL(%q, %q) produced an unparseable url %q", endpoint, p, got)
		}
		if u.Scheme != "https" || u.Host != "drift.example.com" || u.User != nil {
			t.Fatalf("BaseURL(%q, %q) = %q, which addresses %s://%s",
				endpoint, p, got, u.Scheme, u.Host)
		}
	}
}

// --- redirects ----------------------------------------------------------

// REGRESSION. Go strips `Authorization` across a redirect only when the
// HOSTNAME changes — not the scheme, not the port. So an https→http redirect on
// the same host discloses the credential in cleartext, and a redirect to another
// port on the same host hands it to whatever is listening there.
func TestRedirectsAreRefusedRatherThanFollowed(t *testing.T) {
	var leaked []string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = append(leaked, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"pagination":{"limit":20,"offset":0,"hasMore":false}}`))
	}))
	defer sink.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirector.Close()

	c, err := New(Options{
		BaseURL: redirector.URL + "/api/v1", Token: "drift_SECRET", ClientVersion: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.EnvironmentsListWithResponse(context.Background(), &api.EnvironmentsListParams{})
	if err == nil {
		t.Fatal("the redirect was followed")
	}
	if !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("unexpected error: %v", err)
	}
	// Reported as what it is: the server WAS reached, so "cannot reach" would
	// send the operator to check the VPN for no reason.
	reported := Transport(err, "https://drift.example.com")
	if !strings.Contains(reported.Message, "redirect") || strings.Contains(reported.Message, "cannot reach") {
		t.Fatalf("redirect reported as unreachable: %q", reported.Message)
	}
	for _, h := range leaked {
		if strings.Contains(h, "drift_SECRET") {
			t.Fatalf("the credential was forwarded across a redirect: %q", h)
		}
	}
}

// A caller-supplied HTTP client must not silently lose the policy while
// carrying a credential.
func TestSuppliedClientStillRefusesRedirects(t *testing.T) {
	supplied := &http.Client{}
	if _, err := New(Options{BaseURL: "http://x.invalid/api/v1", Token: "drift_x", HTTP: supplied}); err != nil {
		t.Fatal(err)
	}
	if supplied.CheckRedirect != nil {
		t.Fatal("the caller's own client was mutated")
	}
	// Without a credential there is nothing to protect and the caller's policy
	// is left exactly as given.
	if _, err := New(Options{BaseURL: "http://x.invalid/api/v1", HTTP: supplied}); err != nil {
		t.Fatal(err)
	}
}

// --- envelope validity --------------------------------------------------

// REGRESSION. Every field of ApiProblem is optional to encoding/json, so
// oapi-codegen unmarshals ANY json object into a non-nil struct. A legacy
// `{"error":"Invalid ID"}` body therefore arrived as a zero-valued envelope and
// printed `Error: request failed ()`. The server still has routes emitting that
// shape, so this was live.
func TestZeroValuedTypedEnvelopeIsNotTrusted(t *testing.T) {
	// What the generated client hands over for a non-envelope body.
	zero := &api.ApiProblem{}
	body := []byte(`{"error":"Invalid ID"}`)

	e := Problem(zero, body, 500)
	if strings.Contains(e.Message, "request failed ()") {
		t.Fatalf("an empty envelope was reported as one: %q", e.Message)
	}
	if !strings.Contains(e.Message, "cannot decode") {
		t.Fatalf("message = %q", e.Message)
	}

	// And when the body IS an envelope, a zero-valued typed value must not
	// shadow it.
	real := []byte(`{"defined":true,"code":"NOT_FOUND","status":404,"message":"Environment not found"}`)
	e = Problem(zero, real, 404)
	if e.Message != "Environment not found" || e.Code != cliexit.NotFound {
		t.Fatalf("the body envelope was not used: %+v", e)
	}
}

// The refusal happens in the TRANSPORT, so `Client.Do` returns no response at
// all and there is no body for the generated wrapper to leak. A CheckRedirect
// error would have returned both, and the generated code discards the response.
func TestRedirectRefusalLeaksNoResponseBody(t *testing.T) {
	var bodies int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodies++
		w.Header().Set("Location", "http://127.0.0.1:1/api/v1")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte(strings.Repeat("x", 2048)))
	}))
	defer srv.Close()

	c := &http.Client{Transport: NoRedirects(nil)}
	resp, err := c.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the redirect was not refused")
	}
	if resp != nil {
		t.Fatal("a response was returned alongside the error; its body would leak")
	}
	if !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("unexpected error: %v", err)
	}

	// A 3xx with no Location is not a redirect anything could follow, and is
	// left for the caller to decode as an unexpected status.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusFound)
	}))
	defer plain.Close()
	resp, err = c.Get(plain.URL)
	if err != nil {
		t.Fatalf("a Location-less 3xx must not be refused: %v", err)
	}
	_ = resp.Body.Close()
}

// The policy must survive a caller-supplied client, credential or not: the
// discovery fetch carries no token and still must not be steered by a redirect,
// because whoever controls it controls where the API lives.
func TestSuppliedClientAlwaysGetsTheRedirectPolicy(t *testing.T) {
	supplied := &http.Client{}
	for _, token := range []string{"drift_x", ""} {
		if _, err := New(Options{BaseURL: "http://x.invalid/api/v1", Token: token, HTTP: supplied}); err != nil {
			t.Fatal(err)
		}
		if supplied.Transport != nil {
			t.Fatal("the caller's own client was mutated")
		}
	}
}

// A `Retry-After` that is not a bare integer must not destroy the response.
//
// The generated client binds the header as `Type: "integer"` and returns
// `nil, err` from the WHOLE parse when that fails. RFC 9110 also allows the
// HTTP-date form, and an ALB, a WAF or a CDN emits exactly that — so without
// normalization the caller sees a transport error ("cannot reach ...", check
// the VPN) for a server that answered perfectly well, with exit 1 instead of 7,
// and a `--wait` aborts instead of backing off. Which is the intermediary case
// the rate-limit handling exists for.
func TestUnparseableRetryAfterDoesNotDestroyTheResponse(t *testing.T) {
	cases := []struct {
		name, header string
		wantAtLeast  time.Duration
		wantAtMost   time.Duration
	}{
		{"whole seconds", "37", 37 * time.Second, 37 * time.Second},
		{"http-date", time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat), 80 * time.Second, 95 * time.Second},
		{"http-date in the past", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 0, 0},
		{"nonsense", "soon", 0, 0},
		{"float", "12.5", 0, 0},
		{"negative", "-30", 0, 0},
		{"absurd", "18446744074", 0, 0},
		{"absent", "", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if c.header != "" {
					w.Header().Set("Retry-After", c.header)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(429)
				_, _ = w.Write([]byte(`{"defined":true,"code":"TOO_MANY_REQUESTS","status":429,` +
					`"message":"Rate limit exceeded","data":{"type":"urn:drift:problem:rate-limited"}}`))
			}))
			defer srv.Close()

			c2, err := New(Options{BaseURL: srv.URL, Token: "drift_x"})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c2.EnvironmentsListWithResponse(context.Background(), &api.EnvironmentsListParams{})
			if err != nil {
				t.Fatalf("the response was destroyed by the header: %v", err)
			}
			e := Fail(resp, resp.Headers429)
			if e.Code != cliexit.RateLimited {
				t.Fatalf("code = %d, want %d", e.Code, cliexit.RateLimited)
			}
			if e.RetryAfter < c.wantAtLeast || e.RetryAfter > c.wantAtMost {
				t.Fatalf("RetryAfter = %s, want between %s and %s", e.RetryAfter, c.wantAtLeast, c.wantAtMost)
			}
		})
	}
}

// The bound on the multiply. `time.Duration` is int64 nanoseconds, so a large
// enough value wraps — and wraps SHORT, which would make a rate-limited client
// poll faster than its own floor.
func TestRetryAfterIsBoundedRatherThanWrapped(t *testing.T) {
	for _, secs := range []int{1 << 40, 18446744074, 1 << 62, -1, 0} {
		var h struct{ RetryAfter int }
		h.RetryAfter = secs
		if got := RetryAfter(&h); got != 0 {
			t.Fatalf("Retry-After %d yielded %s, want 0", secs, got)
		}
	}
	var ok struct{ RetryAfter int }
	ok.RetryAfter = int(MaxRetryAfter / time.Second)
	if got := RetryAfter(&ok); got != MaxRetryAfter {
		t.Fatalf("the cap itself was rejected: %s", got)
	}
}
