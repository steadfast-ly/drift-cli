// Package discovery fetches, caches and interprets `/.well-known/drift.json`.
//
// The discovery document is what makes one binary talk to several drift
// deployments (DESIGN.md §4). It answers, per context: which organisation this
// is, whether the deployment uses SSO at all, where the versioned API lives,
// which features exist there, and whether this client is older than the server
// will vouch for.
//
// Two properties are load-bearing:
//
//   - **Caching is ETag-revalidated, not time-boxed.** The server serves
//     `Cache-Control: no-cache` with a strong ETag, which means "cache this,
//     but revalidate". A 304 costs one round trip and no parsing, and it can
//     never serve a stale answer, which a TTL can.
//   - **Version skew WARNS, it never refuses.** A hard floor means a server
//     upgrade bricks every operator's CLI mid-incident, which is precisely when
//     the CLI matters most.
package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// WellKnownPath is where every drift deployment serves the document.
const WellKnownPath = "/.well-known/drift.json"

// Document is the discovery document. Unknown fields are ignored, and unknown
// feature strings are simply not matched — that is what keeps the list additive
// across server versions.
type Document struct {
	Org                  string            `json:"org"`
	Version              string            `json:"version"`
	Auth                 string            `json:"auth"`
	Services             map[string]string `json:"services"`
	FeaturesSupported    []string          `json:"features_supported"`
	MinimumClientVersion string            `json:"minimum_client_version"`
}

// DefaultAPIV1 is the prefix used when a document omits `services["api.v1"]` or
// advertises one that is not a plain rooted path. It is the only prefix v1 ever
// shipped with, so it is a compatibility fallback rather than a guess.
const DefaultAPIV1 = "/api/v1"

// APIV1 returns the versioned API path this deployment advertises.
//
// The value is VALIDATED, because it is concatenated onto the endpoint to form
// the URL a bearer credential is sent to. A document serving
// `"@evil.example.com/api/v1"` turns `https://drift.example.com` +
// that into a URL whose host is the attacker and whose real endpoint has been
// demoted to userinfo — Go parses it exactly that way — and the token goes to
// the attacker while the CLI still prints the endpoint the operator configured.
// Anything that is not a single rooted path is refused and the default used.
func (d *Document) APIV1() string {
	if d == nil {
		return DefaultAPIV1
	}
	p, ok := d.Services["api.v1"]
	if !ok || !validAPIPath(p) {
		return DefaultAPIV1
	}
	return p
}

// APIV1Rejected reports whether the document advertised an `api.v1` that was
// refused, so `doctor` can say so instead of silently using the default.
func (d *Document) APIV1Rejected() bool {
	if d == nil {
		return false
	}
	p, ok := d.Services["api.v1"]
	return ok && !validAPIPath(p)
}

// validAPIPath accepts only an absolute, single-segment-rooted path.
//
// The rejections are each a way to escape the endpoint:
//
//   - not starting with `/` — `evil.example.com/api` joined onto an origin is
//     ambiguous, and a bare `//` or `\\` is a protocol-relative URL naming a
//     different host outright.
//   - `@` — everything before it becomes userinfo, so the real endpoint is
//     demoted to a username and the text after `@` becomes the host.
//   - `:` — a scheme, or a port on an injected authority.
//   - `?` and `#` — a query or fragment cannot be part of a base path, and
//     smuggling one changes what the joined request actually addresses.
//   - control characters and whitespace — request-splitting material.
//   - a `..` segment. Not an origin escape — the scheme/host assertion in
//     `client.BaseURL` still holds and the credential stays on the configured
//     host — but against a path-scoped endpoint it silently retargets the
//     prefix, and `/../tenant-b/api/v1` reaching another tenant's data is not a
//     thing a discovery document should be able to ask for.
func validAPIPath(p string) bool {
	if p == "" || p[0] != '/' {
		return false
	}
	if strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return false
	}
	if strings.ContainsAny(p, "@:?#\\ \t\r\n") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// RequiresAuth reports whether this deployment enforces SSO. `auth: "none"`
// covers AUTH_ENABLED=false servers, where the CLI skips login entirely.
func (d *Document) RequiresAuth() bool {
	return d != nil && d.Auth != "none"
}

// HasFeature reports whether a feature string is advertised.
func (d *Document) HasFeature(name string) bool {
	if d == nil {
		return false
	}
	for _, f := range d.FeaturesSupported {
		if f == name {
			return true
		}
	}
	return false
}

// Entry is a cached document plus the ETag needed to revalidate it.
type Entry struct {
	ETag      string    `json:"etag"`
	FetchedAt time.Time `json:"fetchedAt"`
	Document  Document  `json:"document"`
	// Endpoint is stored so a cache file can be read back and attributed even
	// though its name is a hash.
	Endpoint string `json:"endpoint"`
}

// Cache is an on-disk store of discovery documents, one file per endpoint.
type Cache struct {
	Dir string
}

// NewCache places the cache under the config directory. Not XDG_CACHE_HOME: the
// entry is bound to a configured context and a user who copies their config
// directory to a new machine expects the CLI to keep working.
func NewCache(configDir string) *Cache {
	return &Cache{Dir: filepath.Join(configDir, "cache", "discovery")}
}

func (c *Cache) pathFor(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return filepath.Join(c.Dir, hex.EncodeToString(sum[:16])+".json")
}

// Read returns the cached entry for an endpoint, or nil when there is none.
// A corrupt cache file is treated as a miss rather than an error — a cache that
// can brick the CLI is worse than no cache.
//
// Two checks make a cache hit safe to USE, not merely safe to parse:
//
//   - the stored endpoint must match the one being asked for. The file name is
//     a hash, so a collision or a hand-edited cache directory would otherwise
//     serve one deployment's document for another's address.
//   - the stored `api.v1` must still validate. A poisoned document written once
//     would otherwise outlive the fix: the server answers 304 forever after, the
//     cached path is reused on every invocation, and the CLI keeps displaying
//     the endpoint the operator configured while the credential goes elsewhere.
//     Validating on READ as well as on fetch is what makes the repair take
//     effect without anyone knowing to clear a cache.
func (c *Cache) Read(endpoint string) *Entry {
	raw, err := os.ReadFile(c.pathFor(endpoint))
	if err != nil {
		return nil
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil
	}
	if e.Endpoint != endpoint {
		return nil
	}
	if e.Document.APIV1Rejected() {
		// Drop it rather than serve it: a refetch is one request, and leaving
		// the file in place would re-warn on every command.
		_ = c.Clear(endpoint)
		return nil
	}
	return &e
}

// Write stores an entry. Failures are returned but callers may ignore them:
// an unwritable cache degrades performance, not correctness.
func (c *Cache) Write(e *Entry) error {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	out, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.pathFor(e.Endpoint), out, 0o600)
}

// Clear removes the cached entry for an endpoint.
func (c *Cache) Clear(endpoint string) error {
	err := os.Remove(c.pathFor(endpoint))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Result is the outcome of a fetch, including whether the cache was used.
type Result struct {
	Document *Document
	// FromCache is true when the server answered 304 and the cached body was
	// reused, or when the network was skipped entirely.
	FromCache bool
	// Revalidated is true when a conditional request was actually made.
	Revalidated bool
	ETag        string
	FetchedAt   time.Time
}

// DefaultTimeout bounds a discovery fetch when the caller supplies no client of
// its own. Discovery precedes every command, so an unbounded one would hang the
// CLI against a black-holed VPN route rather than failing with advice.
const DefaultTimeout = 15 * time.Second

// Fetcher retrieves discovery documents, revalidating against a cache.
type Fetcher struct {
	HTTP  *http.Client
	Cache *Cache
}

// RedirectPolicy, when set, wraps the default client's transport. Injected by
// the caller rather than imported, because `internal/client` depends on this
// package and the dependency cannot run the other way.
var RedirectPolicy func(http.RoundTripper) http.RoundTripper

func (f *Fetcher) httpClient() *http.Client {
	if f.HTTP != nil {
		return f.HTTP
	}
	c := &http.Client{Timeout: DefaultTimeout}
	if RedirectPolicy != nil {
		c.Transport = RedirectPolicy(nil)
	}
	return c
}

// cacheResult serves a cached entry, copying the document so a caller cannot
// mutate what the next read will return.
func cacheResult(e *Entry) *Result {
	doc := e.Document
	return &Result{Document: &doc, FromCache: true, ETag: e.ETag, FetchedAt: e.FetchedAt}
}

// Fetch retrieves the document for an endpoint.
//
// `force` skips the cached copy for the READ but still writes the result back;
// `drift doctor` uses it so that "is this server reachable" is answered by the
// network rather than by a file.
func (f *Fetcher) Fetch(ctx context.Context, endpoint string, force bool) (*Result, error) {
	var cached *Entry
	if f.Cache != nil && !force {
		cached = f.Cache.Read(endpoint)
	}

	url := strings.TrimRight(endpoint, "/") + WellKnownPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build discovery request: %w", err)
	}
	if cached != nil && cached.ETag != "" {
		req.Header.Set("If-None-Match", cached.ETag)
	}

	resp, err := f.httpClient().Do(req)
	if err != nil {
		// Offline with a warm cache is a legitimate degraded mode: the endpoint
		// list and feature set change rarely, and refusing to run because the
		// VPN dropped mid-session helps nobody.
		if cached != nil {
			return cacheResult(cached), nil
		}
		return nil, fmt.Errorf("reach %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified && cached != nil {
		r := cacheResult(cached)
		r.Revalidated = true
		return r, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		if cached != nil {
			return cacheResult(cached), nil
		}
		return nil, fmt.Errorf("read discovery document: %w", err)
	}
	// A 5xx from a load balancer, or a 404 from a server mid-deploy, is the same
	// kind of transient as a dropped connection — and the transport-error branch
	// above already falls back. Not doing so here meant a deployment restart
	// bricked every command for its duration even with a perfectly good cached
	// document.
	if resp.StatusCode != http.StatusOK {
		if cached != nil {
			return cacheResult(cached), nil
		}
		return nil, fmt.Errorf("discovery at %s returned %s", url, http.StatusText(resp.StatusCode))
	}

	var doc Document
	if err := json.Unmarshal(body, &doc); err != nil {
		if cached != nil {
			return cacheResult(cached), nil
		}
		return nil, fmt.Errorf("parse discovery document from %s: %w", url, err)
	}

	entry := &Entry{
		ETag:      resp.Header.Get("ETag"),
		FetchedAt: time.Now().UTC(),
		Document:  doc,
		Endpoint:  endpoint,
	}
	if f.Cache != nil {
		_ = f.Cache.Write(entry)
	}
	return &Result{
		Document: &doc, Revalidated: cached != nil,
		ETag: entry.ETag, FetchedAt: entry.FetchedAt,
	}, nil
}

// --- version skew -----------------------------------------------------------

// Skew describes how a client version compares to a server's floor.
type Skew struct {
	ClientVersion string
	MinimumClient string
	// TooOld is true when the client is below the server's advertised floor.
	// It is ADVISORY: callers warn, they do not refuse.
	TooOld bool
	// Comparable is false when either version could not be parsed, in which
	// case no claim is made either way.
	Comparable bool
}

// Warning renders the skew warning, or "" when there is nothing to say.
func (s Skew) Warning(contextName, serverVersion string) string {
	if !s.TooOld {
		return ""
	}
	where := "the server"
	if contextName != "" {
		where = fmt.Sprintf("context %q", contextName)
	}
	return fmt.Sprintf(
		"warning: client %s is older than the minimum %s advertised by %s (drift %s).\n"+
			"         Commands may fail in ways this version cannot explain. Upgrade with `mise up drift` or reinstall.",
		s.ClientVersion, s.MinimumClient, where, serverVersion)
}

// CheckSkew compares a client version against a document's floor.
func CheckSkew(clientVersion string, doc *Document) Skew {
	s := Skew{ClientVersion: clientVersion}
	if doc == nil || doc.MinimumClientVersion == "" {
		return s
	}
	s.MinimumClient = doc.MinimumClientVersion
	cmp, ok := compareSemver(clientVersion, doc.MinimumClientVersion)
	if !ok {
		return s
	}
	s.Comparable = true
	s.TooOld = cmp < 0
	return s
}

// compareSemver compares two dotted versions, ignoring any pre-release or build
// suffix. Returns (-1, 0, 1) and whether the comparison was possible.
//
// Deliberately tiny rather than a dependency: the only question ever asked is
// "is this build older than that floor", and a development build like
// `0.1.0-dev` must compare as its release version rather than sorting below it
// and warning on every single invocation.
func compareSemver(a, b string) (int, bool) {
	pa, ok := parseSemver(a)
	if !ok {
		return 0, false
	}
	pb, ok := parseSemver(b)
	if !ok {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
