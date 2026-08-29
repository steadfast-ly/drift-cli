package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

const body = `{"org":"acme","version":"1.4.2","auth":"sso","services":{"api.v1":"/api/v1"},` +
	`"features_supported":["environments.read","promotions.rc"],"minimum_client_version":"0.3.0"}`

func etagOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// discoveryServer serves the document with a strong ETag and honours
// If-None-Match, exactly as the real route does.
func discoveryServer(t *testing.T, payload string) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var full, notModified atomic.Int32
	tag := etagOf(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != WellKnownPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", tag)
		w.Header().Set("Cache-Control", "no-cache")
		if r.Header.Get("If-None-Match") == tag {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		full.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv, &full, &notModified
}

func newFetcher(t *testing.T) *Fetcher {
	t.Helper()
	return &Fetcher{HTTP: http.DefaultClient, Cache: NewCache(t.TempDir())}
}

func TestFetchThenRevalidateWith304(t *testing.T) {
	srv, full, notModified := discoveryServer(t, body)
	f := newFetcher(t)
	ctx := context.Background()

	first, err := f.Fetch(ctx, srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.FromCache {
		t.Fatal("the first fetch cannot come from a cache")
	}
	if first.Document.Org != "acme" || first.Document.Version != "1.4.2" {
		t.Fatalf("document: %+v", first.Document)
	}

	second, err := f.Fetch(ctx, srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	if !second.FromCache || !second.Revalidated {
		t.Fatalf("second fetch should have been a revalidated cache hit: %+v", second)
	}
	if second.Document.Org != "acme" {
		t.Fatalf("the cached body was lost: %+v", second.Document)
	}
	if full.Load() != 1 {
		t.Fatalf("server sent the body %d times, want 1", full.Load())
	}
	if notModified.Load() != 1 {
		t.Fatalf("server answered 304 %d times, want 1", notModified.Load())
	}
}

// A changed document must be picked up: the ETag no longer matches, so the
// server sends a body and the cache is replaced.
func TestCacheIsReplacedWhenTheDocumentChanges(t *testing.T) {
	srv, _, _ := discoveryServer(t, body)
	f := newFetcher(t)
	ctx := context.Background()

	if _, err := f.Fetch(ctx, srv.URL, false); err != nil {
		t.Fatal(err)
	}

	changed := strings.Replace(body, `"1.4.2"`, `"1.5.0"`, 1)
	srv2, _, _ := discoveryServer(t, changed)
	// A different endpoint would use a different cache file, so the cached
	// entry is rewritten under the FIRST endpoint's key to simulate an upgrade
	// in place.
	entry := f.Cache.Read(srv.URL)
	if entry == nil {
		t.Fatal("nothing was cached")
	}
	entry.Endpoint = srv2.URL
	if err := f.Cache.Write(entry); err != nil {
		t.Fatal(err)
	}

	res, err := f.Fetch(ctx, srv2.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.FromCache {
		t.Fatal("a stale ETag must not produce a cache hit")
	}
	if res.Document.Version != "1.5.0" {
		t.Fatalf("stale document served: %+v", res.Document)
	}
}

// `doctor` forces past the cache: it is run precisely when the operator does
// not trust the cached answer.
func TestForceSkipsTheCachedRead(t *testing.T) {
	srv, full, notModified := discoveryServer(t, body)
	f := newFetcher(t)
	ctx := context.Background()

	if _, err := f.Fetch(ctx, srv.URL, false); err != nil {
		t.Fatal(err)
	}
	res, err := f.Fetch(ctx, srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.FromCache {
		t.Fatal("force must not read the cache")
	}
	if full.Load() != 2 || notModified.Load() != 0 {
		t.Fatalf("force sent a conditional request: full=%d, 304=%d", full.Load(), notModified.Load())
	}
}

// Offline with a warm cache is a legitimate degraded mode. Refusing to run
// because the VPN dropped mid-session helps nobody.
func TestOfflineFallsBackToTheCache(t *testing.T) {
	srv, _, _ := discoveryServer(t, body)
	f := newFetcher(t)
	ctx := context.Background()

	if _, err := f.Fetch(ctx, srv.URL, false); err != nil {
		t.Fatal(err)
	}
	dead := srv.URL
	srv.Close()

	res, err := f.Fetch(ctx, dead, false)
	if err != nil {
		t.Fatalf("a warm cache must survive the server going away: %v", err)
	}
	if !res.FromCache || res.Document.Org != "acme" {
		t.Fatalf("offline fallback: %+v", res)
	}
}

func TestOfflineWithNoCacheIsAnError(t *testing.T) {
	srv, _, _ := discoveryServer(t, body)
	url := srv.URL
	srv.Close()

	f := newFetcher(t)
	if _, err := f.Fetch(context.Background(), url, false); err == nil {
		t.Fatal("expected a transport error with no cache to fall back on")
	}
}

// A corrupt cache file is a miss, never a hard failure: a cache that can brick
// the CLI is worse than no cache.
func TestCorruptCacheEntryIsAMiss(t *testing.T) {
	srv, full, _ := discoveryServer(t, body)
	f := newFetcher(t)
	ctx := context.Background()

	if _, err := f.Fetch(ctx, srv.URL, false); err != nil {
		t.Fatal(err)
	}
	if err := writeGarbage(f.Cache, srv.URL); err != nil {
		t.Fatal(err)
	}
	res, err := f.Fetch(ctx, srv.URL, false)
	if err != nil {
		t.Fatalf("a corrupt cache entry must not fail the fetch: %v", err)
	}
	if res.Document.Org != "acme" {
		t.Fatalf("document: %+v", res.Document)
	}
	if full.Load() != 2 {
		t.Fatalf("expected a full refetch, server sent the body %d times", full.Load())
	}
}

func writeGarbage(c *Cache, endpoint string) error {
	return os.WriteFile(c.pathFor(endpoint), []byte("{not json"), 0o600)
}

func TestClearRemovesTheEntryAndIsIdempotent(t *testing.T) {
	srv, _, _ := discoveryServer(t, body)
	f := newFetcher(t)
	if _, err := f.Fetch(context.Background(), srv.URL, false); err != nil {
		t.Fatal(err)
	}
	if err := f.Cache.Clear(srv.URL); err != nil {
		t.Fatal(err)
	}
	if f.Cache.Read(srv.URL) != nil {
		t.Fatal("the entry survived Clear")
	}
	if err := f.Cache.Clear(srv.URL); err != nil {
		t.Fatalf("Clear must be idempotent: %v", err)
	}
}

// --- document semantics -----------------------------------------------------

func TestDocumentAccessors(t *testing.T) {
	var d Document
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	if d.APIV1() != "/api/v1" {
		t.Fatalf("APIV1 = %q", d.APIV1())
	}
	if !d.HasFeature("environments.read") || d.HasFeature("environments.write") {
		t.Fatal("HasFeature is wrong")
	}
	if !d.RequiresAuth() {
		t.Fatal("auth: sso must require authentication")
	}

	// `auth: "none"` covers AUTH_ENABLED=false servers; the CLI skips login.
	none := Document{Auth: "none"}
	if none.RequiresAuth() {
		t.Fatal("auth: none must not require authentication")
	}

	// A document with no `services` still has to yield the only prefix v1 ever
	// shipped with, rather than an empty path that would produce bad URLs.
	empty := Document{}
	if empty.APIV1() != "/api/v1" {
		t.Fatalf("default APIV1 = %q", empty.APIV1())
	}
	var nilDoc *Document
	if nilDoc.APIV1() != "/api/v1" || nilDoc.RequiresAuth() || nilDoc.HasFeature("x") {
		t.Fatal("a nil document must be inert rather than panic")
	}
}

// --- version skew -----------------------------------------------------------

func TestSkewWarnsButNeverRefuses(t *testing.T) {
	doc := &Document{Version: "1.4.2", MinimumClientVersion: "0.3.0"}

	old := CheckSkew("0.2.9", doc)
	if !old.TooOld || !old.Comparable {
		t.Fatalf("0.2.9 vs 0.3.0: %+v", old)
	}
	w := old.Warning("au", doc.Version)
	if w == "" || !strings.Contains(w, "0.2.9") || !strings.Contains(w, "0.3.0") ||
		!strings.Contains(w, `context "au"`) || !strings.Contains(w, "1.4.2") {
		// The warning must name the client version, the floor, the CONTEXT and
		// the server version: an operator with several contexts otherwise has
		// no way to tell which deployment complained.
		t.Fatalf("warning text is missing something: %q", w)
	}

	exact := CheckSkew("0.3.0", doc)
	if exact.TooOld {
		t.Fatal("the floor itself must not warn")
	}
	if exact.Warning("au", doc.Version) != "" {
		t.Fatal("a satisfied floor must produce no warning")
	}

	newer := CheckSkew("1.0.0", doc)
	if newer.TooOld {
		t.Fatal("a newer client must not warn")
	}
}

// A development build must compare as its release version. Sorting `0.1.0-dev`
// below `0.1.0` would warn on every single invocation during development.
func TestPrereleaseComparesAsItsReleaseVersion(t *testing.T) {
	doc := &Document{Version: "1.0.0", MinimumClientVersion: "0.1.0"}
	s := CheckSkew("0.1.0-dev", doc)
	if s.TooOld {
		t.Fatalf("0.1.0-dev must satisfy a 0.1.0 floor: %+v", s)
	}
	if s := CheckSkew("v0.1.0+build.7", doc); s.TooOld {
		t.Fatalf("a v-prefixed build metadata version must parse: %+v", s)
	}
}

// git describe outputs `v0.2.0` when exactly on a tag. The skew check must
// strip the `v` prefix, otherwise a tagged release always warns.
func TestVPrefixedDescribeOutputParsesCleanly(t *testing.T) {
	doc := &Document{Version: "1.0.0", MinimumClientVersion: "0.2.0"}
	s := CheckSkew("v0.2.0", doc)
	if !s.Comparable {
		t.Fatalf("v0.2.0 must be comparable: %+v", s)
	}
	if s.TooOld {
		t.Fatalf("v0.2.0 must not be below the 0.2.0 floor: %+v", s)
	}
}

func TestUnparseableVersionsMakeNoClaim(t *testing.T) {
	doc := &Document{Version: "1.0.0", MinimumClientVersion: "0.1.0"}
	s := CheckSkew("not-a-version", doc)
	if s.Comparable || s.TooOld {
		t.Fatalf("an unparseable client version must make no claim: %+v", s)
	}
	if s.Warning("au", doc.Version) != "" {
		t.Fatal("no claim means no warning")
	}

	// A server that advertises no floor is not a skew problem either.
	if s := CheckSkew("0.0.1", &Document{Version: "1.0.0"}); s.TooOld || s.Comparable {
		t.Fatalf("absent floor: %+v", s)
	}
}

func TestCompareSemverPartialVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1", "1.0.0", 0},
		{"1.2", "1.2.0", 0},
		{"1.2.3", "1.2.4", -1},
		{"1.10.0", "1.9.0", 1},
		{"2.0.0", "10.0.0", -1},
	}
	for _, c := range cases {
		got, ok := compareSemver(c.a, c.b)
		if !ok {
			t.Fatalf("compareSemver(%q,%q) failed to parse", c.a, c.b)
		}
		if got != c.want {
			t.Fatalf("compareSemver(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if _, ok := compareSemver("1.2.3.4", "1.0.0"); ok {
		t.Fatal("four components must not parse")
	}
}

// --- api.v1 validation ------------------------------------------------------

// REGRESSION. `services["api.v1"]` was concatenated onto the endpoint with no
// validation, so a document could move credentialled requests to another host
// while the CLI kept displaying the configured endpoint. `"@attacker/api/v1"`
// is the sharp one: Go parses `https://drift.example.com@attacker/api/v1` with
// `attacker` as the HOST and the real endpoint demoted to userinfo.
func TestMaliciousAPIPathsAreRefused(t *testing.T) {
	hostile := []string{
		"@127.0.0.1:18082/api/v1",
		"@attacker.invalid/api/v1",
		"//attacker.invalid/api/v1",
		"/\\attacker.invalid/api/v1",
		"https://attacker.invalid/api/v1",
		"http://attacker.invalid",
		"api/v1",
		"",
		"/api/v1?x=1",
		"/api/v1#frag",
		"/api/v1 ",
		"/api\r\nHost: attacker.invalid",
		"/api/v1:8080",
	}
	for _, p := range hostile {
		d := &Document{Services: map[string]string{"api.v1": p}}
		if got := d.APIV1(); got != DefaultAPIV1 {
			t.Fatalf("APIV1 accepted %q and returned %q", p, got)
		}
		if !d.APIV1Rejected() {
			t.Fatalf("%q was refused silently; doctor could not report it", p)
		}
	}
}

func TestLegitimateAPIPathsAreAccepted(t *testing.T) {
	for _, p := range []string{"/api/v1", "/api/v2", "/drift/api/v1", "/v1"} {
		d := &Document{Services: map[string]string{"api.v1": p}}
		if got := d.APIV1(); got != p {
			t.Fatalf("APIV1 rejected %q and returned %q", p, got)
		}
		if d.APIV1Rejected() {
			t.Fatalf("%q was marked rejected", p)
		}
	}
	// An absent key is not a rejection, just the default.
	d := &Document{}
	if d.APIV1() != DefaultAPIV1 || d.APIV1Rejected() {
		t.Fatal("an absent api.v1 must be the plain default")
	}
}

// REGRESSION, second half. A poisoned document that reached the cache used to
// outlive the fix: the server answers 304 from then on, the cached path is
// reused on every invocation, and nothing ever revalidates its CONTENT.
// Validating on read is what makes the repair take effect without anyone
// knowing to clear a cache directory.
func TestPoisonedCacheEntryIsNotServed(t *testing.T) {
	srv, full, _ := discoveryServer(t, body)
	f := newFetcher(t)
	ctx := context.Background()

	if _, err := f.Fetch(ctx, srv.URL, false); err != nil {
		t.Fatal(err)
	}

	// Poison the cache directly, as a compromised server would have via a
	// document served before the validation landed.
	entry := f.Cache.Read(srv.URL)
	if entry == nil {
		t.Fatal("nothing cached")
	}
	entry.Document.Services["api.v1"] = "@127.0.0.1:18082/api/v1"
	if err := f.Cache.Write(entry); err != nil {
		t.Fatal(err)
	}

	// Read must refuse it outright...
	if got := f.Cache.Read(srv.URL); got != nil {
		t.Fatalf("a poisoned entry was served from the cache: %+v", got.Document.Services)
	}
	// ...and the next fetch must go to the network rather than 304 against it,
	// which is what would otherwise make the poisoning permanent.
	res, err := f.Fetch(ctx, srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Document.APIV1() != "/api/v1" {
		t.Fatalf("APIV1 = %q", res.Document.APIV1())
	}
	if full.Load() < 2 {
		t.Fatalf("the poisoned entry was revalidated rather than refetched (bodies sent: %d)", full.Load())
	}
}

// A cache file whose stored endpoint does not match the one being asked for is
// not this endpoint's document, whatever the file name hashes to.
func TestCacheEntryForAnotherEndpointIsIgnored(t *testing.T) {
	srv, _, _ := discoveryServer(t, body)
	f := newFetcher(t)
	if _, err := f.Fetch(context.Background(), srv.URL, false); err != nil {
		t.Fatal(err)
	}
	entry := f.Cache.Read(srv.URL)
	if entry == nil {
		t.Fatal("nothing cached")
	}
	// Rewrite the stored endpoint, leaving the file where it is.
	entry.Endpoint = "https://somewhere-else.example.com"
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Cache.pathFor(srv.URL), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := f.Cache.Read(srv.URL); got != nil {
		t.Fatal("a document filed under another endpoint was served")
	}
}

// A 5xx or a 404 mid-deploy is as transient as a dropped connection, and the
// transport path already falls back. Not doing so here bricked every command
// for the duration of a restart despite a perfectly good cached document.
func TestNon200FallsBackToTheCache(t *testing.T) {
	var fail atomic.Bool
	tag := etagOf(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("ETag", tag)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	f := newFetcher(t)
	ctx := context.Background()
	if _, err := f.Fetch(ctx, srv.URL, false); err != nil {
		t.Fatal(err)
	}

	fail.Store(true)
	res, err := f.Fetch(ctx, srv.URL, false)
	if err != nil {
		t.Fatalf("a warm cache must survive a 502: %v", err)
	}
	if !res.FromCache || res.Document.Org != "acme" {
		t.Fatalf("%+v", res)
	}

	// With no cache there is nothing to fall back to and it must still fail.
	f2 := newFetcher(t)
	if _, err := f2.Fetch(ctx, srv.URL, false); err == nil {
		t.Fatal("a 502 with no cache must be an error")
	}
}

func TestFetcherHasADefaultTimeout(t *testing.T) {
	f := &Fetcher{Cache: NewCache(t.TempDir())}
	if c := f.httpClient(); c.Timeout != DefaultTimeout {
		t.Fatalf("default client timeout = %v, want %v", c.Timeout, DefaultTimeout)
	}
}

// A `..` segment is not an origin escape — client.BaseURL's scheme/host
// assertion still holds — but against a path-scoped endpoint it silently
// retargets the prefix, and reaching another tenant's data is not something a
// discovery document should be able to ask for.
func TestDotDotSegmentsAreRefused(t *testing.T) {
	for _, p := range []string{
		"/../tenant-b/api/v1",
		"/api/../../v1",
		"/api/v1/..",
		"/..",
	} {
		d := &Document{Services: map[string]string{"api.v1": p}}
		if got := d.APIV1(); got != DefaultAPIV1 {
			t.Fatalf("APIV1 accepted %q and returned %q", p, got)
		}
		if !d.APIV1Rejected() {
			t.Fatalf("%q was refused silently", p)
		}
	}
	// A path merely CONTAINING dots in a segment name is fine.
	for _, p := range []string{"/api/v1.1", "/a..b/v1", "/api/v1/."} {
		d := &Document{Services: map[string]string{"api.v1": p}}
		if d.APIV1Rejected() {
			t.Fatalf("%q was refused", p)
		}
	}
}
