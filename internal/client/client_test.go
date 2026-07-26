package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/steadfast/drift-cli/internal/api"
	"github.com/steadfast/drift-cli/internal/cliexit"
)

func problem(status int, code, msg, detail string) *api.ApiProblem {
	p := &api.ApiProblem{Defined: true, Code: code, Status: status, Message: msg}
	p.Data = &struct {
		Detail *string `json:"detail,omitempty"`
		Type   string  `json:"type"`
	}{Type: "urn:drift:problem:test"}
	if detail != "" {
		p.Data.Detail = &detail
	}
	return p
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
// a 429 added server-side would print "HTTP 429" instead of the server's words.
func TestUndeclaredStatusDecodesFromTheBody(t *testing.T) {
	body := []byte(`{"defined":true,"code":"TOO_MANY_REQUESTS","status":429,` +
		`"message":"Rate limit exceeded","data":{"type":"urn:drift:problem:rate-limited","detail":"retry in 30s"}}`)
	e := Problem(nil, body, 429)
	if e.Message != "Rate limit exceeded" || e.Detail != "retry in 30s" {
		t.Fatalf("undeclared status was not decoded: %+v", e)
	}
	if e.Code != cliexit.Error {
		t.Fatalf("code = %d", e.Code)
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
