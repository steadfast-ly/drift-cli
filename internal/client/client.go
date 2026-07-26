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

	e := &cliexit.ExitError{
		Code:    cliexit.FromProblem(p.Code, status),
		Message: p.Message,
	}
	if p.Data != nil && p.Data.Detail != nil {
		e.Detail = *p.Data.Detail
	}
	if e.Message == "" {
		e.Message = fmt.Sprintf("request failed (%s)", p.Code)
	}
	if status == http.StatusUnauthorized {
		e.Hint = "run `drift auth login`, or set DRIFT_TOKEN"
	}
	return e
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
