// Package client assembles the generated API client from a resolved context and
// a credential, and translates the server's error envelope into the CLI's
// exit-code contract.
//
// The generated package (`internal/api`) is regenerated from the vendored spec
// and must never be hand-edited, so everything that is a POLICY decision — how
// to attach a credential, what an envelope means, which exit code a failure
// deserves — lives here instead.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/steadfast/drift-cli/internal/api"
	"github.com/steadfast/drift-cli/internal/cliexit"
)

// ClientVersionHeader is the attribution header the server records in the audit
// row for a credential's first use. Sanitised server-side; sent unconditionally
// so CLI traffic is distinguishable from browser traffic in the audit log.
const ClientVersionHeader = "X-Drift-Client-Version"

// BaseURL joins a deployment endpoint and the discovery document's API path,
// and PROVES the result still addresses the endpoint.
//
// String concatenation is not safe here even with a validated path, because the
// endpoint itself is operator-supplied and the joined string is where a bearer
// credential is sent. Parsing both and comparing scheme and host afterwards is
// the only formulation that cannot be talked out of the origin the operator
// configured: whatever cleverness is in either half, the assertion is on the
// URL that will actually be dialled.
func BaseURL(endpoint, apiPath string) (string, error) {
	base, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("endpoint %q is not a URL: %w", endpoint, err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return "", fmt.Errorf("endpoint %q must be http or https", endpoint)
	}
	if base.Host == "" {
		return "", fmt.Errorf("endpoint %q has no host", endpoint)
	}
	if base.User != nil {
		// Credentials in the endpoint are never needed and would be sent to
		// whatever the rest of the URL resolves to.
		return "", fmt.Errorf("endpoint %q must not contain userinfo", endpoint)
	}

	joined := base.JoinPath(apiPath)
	if joined.Scheme != base.Scheme || joined.Host != base.Host || joined.User != nil {
		return "", fmt.Errorf(
			"the server's advertised API path would move requests from %s://%s to %s://%s",
			base.Scheme, base.Host, joined.Scheme, joined.Host)
	}
	return strings.TrimRight(joined.String(), "/"), nil
}

// ErrRedirectRefused marks a refused redirect so it can be reported as what it
// is rather than as "cannot reach the server" — which it is not: the server was
// reached, and answered with something the client will not follow.
var ErrRedirectRefused = errors.New("refusing to follow a redirect")

// NoRedirects wraps a RoundTripper so that a redirect response is an error.
//
// Go strips `Authorization` across a redirect only when the HOSTNAME changes;
// it consults neither the scheme nor the port. So a 302 from `https://drift` to
// `http://drift` discloses the credential in cleartext, and one to a different
// port on the same host forwards it to whatever is listening there. For an API
// client there is no legitimate redirect to follow — every operation is a direct
// call on a versioned path — so the whole class is refused.
//
// Done at the TRANSPORT rather than through `CheckRedirect`, for two reasons.
// A CheckRedirect error makes `Client.Do` return BOTH a response and an error,
// and the generated client discards the response — leaking the connection,
// since nothing closes that body. And the transport sees every response,
// including the discovery fetch, so one wrapper covers both clients rather than
// leaving the document that decides where credentials go on a laxer policy than
// the requests that carry them.
func NoRedirects(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &noRedirectTransport{base: base}
}

type noRedirectTransport struct{ base http.RoundTripper }

func (t *noRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	// Every request the CLI makes goes through this transport, including the
	// discovery fetch, so one call here covers all twenty-five operations that
	// declare a 429.
	normalizeRetryAfter(resp)
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return resp, nil
	}
	location := resp.Header.Get("Location")
	if location == "" {
		// A 3xx with nowhere to go is not a redirect the client could follow;
		// let the caller decode it as an unexpected status.
		return resp, nil
	}
	// Drained and closed here, so the connection is returned to the pool and
	// nothing is left for a caller that only looks at the error.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()

	target := location
	if u, perr := url.Parse(location); perr == nil {
		target = u.Redacted()
	}
	return nil, fmt.Errorf("%w to %s", ErrRedirectRefused, target)
}

func hasNoRedirects(rt http.RoundTripper) bool {
	_, ok := rt.(*noRedirectTransport)
	return ok
}

// MaxRetryAfter caps how long a `Retry-After` may ask the CLI to wait.
//
// Twenty-four hours is far beyond anything a per-credential window can
// legitimately need and far below the point at which the arithmetic below
// stops being meaningful. Anything larger is a broken or hostile intermediary,
// not a rate limiter with a plan.
const MaxRetryAfter = 24 * time.Hour

// normalizeRetryAfter rewrites a 429's `Retry-After` into the whole-seconds
// form the generated parser can read, and drops it when it cannot.
//
// This exists because of how the generated client fails. It binds the header as
// `Type: "integer"` and returns `nil, err` from the WHOLE response parse when
// that fails — so an RFC 9110 HTTP-date, which is legal and is what an ALB, a
// WAF or a CDN emits, does not merely lose the hint: it destroys the response.
// The caller then sees a transport error ("cannot reach ...", check the VPN) for
// a server that answered perfectly well, with the wrong exit code, and inside a
// `--wait` the loop aborts instead of backing off — defeating the backoff in
// exactly the intermediary case it was designed for.
//
// Done at the transport rather than by editing the generated code, which is
// never hand-edited. The rewrite is meaning-preserving: an HTTP-date becomes
// the delta in seconds it denotes. Anything else — a negative, a float, a word
// — is REMOVED rather than guessed at, so the response still parses and
// `RetryAfter` reports zero, which callers already floor to their own interval.
func normalizeRetryAfter(resp *http.Response) {
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return
	}
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if raw == "" {
		return
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs < 0 || secs > int(MaxRetryAfter/time.Second) {
			resp.Header.Del("Retry-After")
		}
		return
	}
	if when, err := http.ParseTime(raw); err == nil {
		secs := int(time.Until(when).Round(time.Second) / time.Second)
		if secs < 0 {
			secs = 0
		}
		if secs > int(MaxRetryAfter/time.Second) {
			resp.Header.Del("Retry-After")
			return
		}
		resp.Header.Set("Retry-After", strconv.Itoa(secs))
		return
	}
	resp.Header.Del("Retry-After")
}

// Options configures a client.
type Options struct {
	// BaseURL is the full API base, endpoint + the discovery document's
	// `services["api.v1"]`.
	BaseURL string
	// Token is the bearer credential. Empty is legitimate against an
	// `auth: "none"` deployment.
	Token         string
	ClientVersion string
	Timeout       time.Duration
	HTTP          *http.Client
}

// New builds a generated client with credentials and attribution attached.
func New(o Options) (*api.ClientWithResponses, error) {
	httpClient := o.HTTP
	if httpClient == nil {
		timeout := o.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		httpClient = &http.Client{Timeout: timeout, Transport: NoRedirects(nil)}
	} else if !hasNoRedirects(httpClient.Transport) {
		// A caller-supplied client (tests, and anything that needs a custom
		// transport) must not silently lose the policy. Copied rather than
		// mutated: the caller's client may be shared, and reaching into it would
		// change behaviour it did not ask to change.
		clone := *httpClient
		clone.Transport = NoRedirects(clone.Transport)
		httpClient = &clone
	}

	editor := func(_ context.Context, req *http.Request) error {
		if o.Token != "" {
			req.Header.Set("Authorization", "Bearer "+o.Token)
		}
		if o.ClientVersion != "" {
			req.Header.Set(ClientVersionHeader, o.ClientVersion)
		}
		return nil
	}

	return api.NewClientWithResponses(o.BaseURL,
		api.WithHTTPClient(httpClient),
		api.WithRequestEditorFn(editor))
}

// Problem turns a decoded error envelope into an ExitError.
//
// `message` and `data.detail` are the two human-facing strings the server
// produces, and both are surfaced: the message states what went wrong, the
// detail usually states why. A raw Go error is never substituted for either —
// the whole point of the shared envelope is that the CLI has something typed to
// say.
//
// `body` is the raw response, used only when `p` is nil. The generated client
// only materialises the statuses an operation DECLARES, so a status the spec
// omits — a 409 raised by a code path the contract has not caught up with, a
// 429 from a rate limiter added server-side — arrives with every typed field
// nil even though the body is a perfectly good envelope. Decoding it here is
// what keeps that case a typed, actionable message instead of "HTTP 409".
func Problem(p *api.ApiProblem, body []byte, transportStatus int) *cliexit.ExitError {
	// The validity check has to gate the TYPED value too, not only the decoded
	// fallback. Every field of `ApiProblem` is optional as far as encoding/json
	// is concerned, so oapi-codegen unmarshals any JSON object at all into a
	// non-nil struct — a legacy `{"error":"Invalid ID"}` body produces a zero
	// value that is not nil, and printing it yields `Error: request failed ()`.
	// The server still has routes emitting exactly that shape (DESIGN.md §2,
	// deferred items), so this is reachable today.
	if !valid(p) {
		p = decodeProblem(body)
	}
	if p == nil {
		// The server answered with something that is not an envelope at all.
		// Say exactly that rather than inventing a cause: a mismatched spec and
		// a broken proxy look identical from here, and guessing between them
		// sends the operator down the wrong path.
		return &cliexit.ExitError{
			Code: cliexit.FromHTTPStatus(transportStatus),
			Message: fmt.Sprintf("server returned HTTP %d with a body this client cannot decode",
				transportStatus),
			Hint: "the server may be running a newer API than this CLI; run `drift doctor`",
		}
	}

	// The envelope's own status is authoritative over the transport status: a
	// proxy that rewrites the status line cannot change what the server meant.
	status := p.Status
	if status == 0 {
		status = transportStatus
	}

	// The problem type from `data.type` is what discriminates two 403s that
	// need different exit codes: a role-floor refusal vs. an elevation-required
	// scope failure.
	var problemType string
	if p.Data != nil {
		problemType = p.Data.Type
	}

	e := &cliexit.ExitError{
		Code:    cliexit.FromProblem(problemType, status),
		Message: p.Message,
	}
	if p.Data != nil && p.Data.Detail != nil {
		e.Detail = *p.Data.Detail
	}
	// A 409 from the lifecycle machine carries the state the entity was in and
	// the event that was refused. Rendered only when the server sent no prose
	// of its own, so the two are never printed twice — but rendered, because
	// "the environment is building, which does not accept SLEEP" is the entire
	// content of a typed invalid-transition and losing it leaves the operator
	// with a bare "Cannot sleep environment".
	if e.Detail == "" && p.Data != nil && p.Data.State != nil && p.Data.Event != nil {
		e.Detail = fmt.Sprintf("the environment is %s, which does not accept %s",
			*p.Data.State, *p.Data.Event)
	}
	if e.Message == "" {
		e.Message = fmt.Sprintf("request failed (%s)", p.Code)
	}
	switch {
	case status == http.StatusForbidden && problemType == cliexit.ElevationRequiredType:
		e.Hint = "mint a 15-minute elevated credential at /credentials, then run `drift auth login --token-stdin` with it and retry"
	case status == http.StatusUnauthorized:
		e.Hint = "run `drift auth login`, or set DRIFT_TOKEN"
	}
	return e
}

// Response is the part of every generated `*Response` type that exists on all
// of them, regardless of which statuses an operation declares.
//
// It exists so that error handling is written ONCE rather than once per
// operation. The generated code emits these accessors for every operation; a
// contract change that removed one would fail this build, which is the point.
//
// 404, 409 and 502 are deliberately NOT here: `environments.list` declares no
// 404, so requiring `GetJSON404` would exclude it. They are picked up through
// the optional interfaces below, which is the same shape the contract has.
type Response interface {
	GetBody() []byte
	StatusCode() int
	GetJSON400() *api.ApiProblem
	GetJSON401() *api.ApiProblem
	GetJSON403() *api.ApiProblem
	GetJSON429() *api.ApiProblem
	GetJSON500() *api.ApiProblem
	GetJSON503() *api.ApiProblem
}

type withNotFound interface{ GetJSON404() *api.ApiProblem }
type withConflict interface{ GetJSON409() *api.ApiProblem }
type withBadGateway interface{ GetJSON502() *api.ApiProblem }

// RetryAfterHeader constrains the per-operation 429 header structs the
// generator emits.
//
// oapi-codegen names one type per operation — `EnvironmentsCreateResponse429Headers`,
// `EnvironmentsSleepResponse429Headers`, and so on — with identical contents.
// The underlying-type constraint accepts all of them without naming any, so
// adding an operation to the contract needs no change here, while a change to
// the header's SHAPE (a rename, a second header, a different type) stops
// compiling rather than silently returning zero.
type RetryAfterHeader interface {
	~struct{ RetryAfter int }
}

// RetryAfter reads the server's `Retry-After` off a typed 429 header struct.
//
// Read from the parsed header rather than from `HTTPResponse.Header`, so the
// contract's declaration ("whole seconds, always present on a 429") is what the
// CLI depends on, not a string lookup that would silently yield zero if the
// server ever moved to the HTTP-date form.
// The multiply is GUARDED, and the guard is not theoretical. `time.Duration` is
// an int64 of nanoseconds, so `time.Duration(secs) * time.Second` wraps for any
// value above about 292 years — and a wrapped result is not merely wrong, it is
// wrong in the dangerous direction: `Retry-After: 18446744074` came back as
// 0.29s, which would make a rate-limited client poll FASTER than its own floor.
// Values past the cap are treated as "the server did not say".
func RetryAfter[H RetryAfterHeader](h *H) time.Duration {
	if h == nil {
		return 0
	}
	secs := (struct{ RetryAfter int })(*h).RetryAfter
	if secs <= 0 || secs > int(MaxRetryAfter/time.Second) {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// Fail turns any non-2xx generated response into an ExitError.
//
// One function for every operation, so a new command cannot forget a status:
// the envelope is picked from whichever typed field the generator populated,
// and `Problem` still decodes the raw body when the server answered with a
// status the contract does not declare.
func Fail[H RetryAfterHeader](r Response, retry *H) *cliexit.ExitError {
	problems := []*api.ApiProblem{
		r.GetJSON400(), r.GetJSON401(), r.GetJSON403(), r.GetJSON429(),
		r.GetJSON500(), r.GetJSON503(),
	}
	if v, ok := r.(withNotFound); ok {
		problems = append(problems, v.GetJSON404())
	}
	if v, ok := r.(withConflict); ok {
		problems = append(problems, v.GetJSON409())
	}
	if v, ok := r.(withBadGateway); ok {
		problems = append(problems, v.GetJSON502())
	}

	e := Problem(first(problems...), r.GetBody(), r.StatusCode())
	if e.Code == cliexit.RateLimited {
		e.RetryAfter = RetryAfter(retry)
		e.Hint = rateLimitHint(e.RetryAfter)
	}
	return e
}

// rateLimitHint states what to do about a 429 in the caller's own terms.
//
// No header changes the answer — the limit is per credential, not per request
// shape — so the advice is to wait, and the CLI says exactly how long rather
// than leaving the operator to guess an interval and get throttled again.
func rateLimitHint(d time.Duration) string {
	if d <= 0 {
		return "the limit is per credential; wait and retry"
	}
	return fmt.Sprintf("the limit is per credential; retry in %s", d.Round(time.Second))
}

func first(ps ...*api.ApiProblem) *api.ApiProblem {
	for _, p := range ps {
		if p != nil {
			return p
		}
	}
	return nil
}

// valid reports whether a decoded value is actually the drift error envelope.
//
// `code` and `status` are the two fields the server always sets and that no
// unrelated JSON object — an ALB error page, a `{"error":"..."}` from a legacy
// route — happens to carry. Requiring both is what stops such a body being
// reported as an envelope with an empty message.
func valid(p *api.ApiProblem) bool {
	return p != nil && p.Code != "" && p.Status != 0
}

// decodeProblem parses a body as the shared envelope, returning nil when it is
// not one.
func decodeProblem(body []byte) *api.ApiProblem {
	if len(body) == 0 {
		return nil
	}
	var p api.ApiProblem
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	if !valid(&p) {
		return nil
	}
	return &p
}

// Transport wraps a network-level failure with advice worth acting on.
//
// A connection failure against a VPN-gated server is overwhelmingly "not on the
// VPN", and saying so beats reprinting a dial error the operator has to decode.
func Transport(err error, endpoint string) *cliexit.ExitError {
	if errors.Is(err, ErrRedirectRefused) {
		return &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: fmt.Sprintf("%s answered with a redirect, which this client will not follow", endpoint),
			Detail: "the drift API does not redirect; following one can disclose the credential, " +
				"because Go strips Authorization only when the HOSTNAME changes and not when the scheme or port does",
			Hint: "check that the endpoint is the deployment's real origin, not a proxy or a vanity URL",
			Err:  err,
		}
	}

	msg := fmt.Sprintf("cannot reach %s", endpoint)
	e := &cliexit.ExitError{Code: cliexit.Error, Message: msg, Err: err}
	switch {
	case isTimeout(err):
		e.Detail = "the request timed out"
		e.Hint = "check the VPN connection, then run `drift doctor`"
	default:
		e.Detail = summarizeNetError(err)
		e.Hint = "check the endpoint and the VPN connection, then run `drift doctor`"
	}
	return e
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// summarizeNetError trims Go's layered dial errors to the part a human acts on.
func summarizeNetError(err error) string {
	s := err.Error()
	// `Get "http://…": dial tcp …: connect: connection refused` — the tail is
	// the only informative part and the URL is already in the message.
	if i := strings.LastIndex(s, ": "); i >= 0 && i+2 < len(s) {
		return s[i+2:]
	}
	return s
}
