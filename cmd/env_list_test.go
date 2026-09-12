package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/steadfast-ly/drift-cli/internal/cliexit"
)

// envListPage is one scripted page of the fake drift used by the `--all` walk
// tests. `offset` is the exact query value the page answers — "" because the
// command leaves the offset parameter unset when it is 0, so the generated
// client never emits it — and `status` overrides the HTTP status (200 when
// zero) so a walk can be interrupted mid-way. `anyOffset` makes the page
// answer EVERY offset regardless of the query, for the endless-page abort
// test: an offset-keyed script could not hold that walk, which re-requests
// the same page under ever-changing offsets and would 500 on the second one.
type envListPage struct {
	offset    string
	slugs     []string
	hasMore   bool
	limit     int
	status    int
	anyOffset bool
}

// pagedServer is a fake drift whose /api/v1/environments serves pages keyed by
// the `offset` query parameter, recording the exact query of every request so a
// test can assert the walk's request sequence rather than trusting the client.
type pagedServer struct {
	*fakeServer
	mu      sync.Mutex
	queries []url.Values
}

func (s *pagedServer) got() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.queries...)
}

// newPagedServer builds a discovery document plus a paginated environments
// list, scripted for at most len(pages) requests. A walk that stops advancing
// its offset — or never stops on an empty page — would otherwise re-serve a
// scripted page forever and hang the test, so once the script is exhausted
// every further request fails with a deterministic 500. A request whose offset
// has no scripted page is likewise a 500, so a walk that jumps to an
// unscripted offset fails loudly instead of spinning. A page marked anyOffset
// is the deliberate exception: it answers EVERY offset, exempting an
// endless-page script from both the budget and the off-script 500, so the
// client's OWN loop termination — its page ceiling — is what the test
// observes, not a mock-imposed failure.
func newPagedServer(t *testing.T, pages ...envListPage) *pagedServer {
	t.Helper()
	s := &pagedServer{}
	doc, _ := json.Marshal(defaultDoc(""))
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/drift.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	})
	mux.HandleFunc("/api/v1/auth/whoami", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			writeProblem(w, 401, "UNAUTHORIZED", "Authentication required",
				"urn:drift:problem:unauthenticated", "")
			return
		}
		s.mu.Lock()
		s.whoamiCalls++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"email": "operator@example.com", "role": "admin", "channel": "cli",
			"credential": map[string]any{
				"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "label": "desktop",
				"scopes": []string{}, "expiresAt": "2026-09-24T19:19:59.694Z",
			},
		})
	})
	mux.HandleFunc("/api/v1/environments", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			writeProblem(w, 401, "UNAUTHORIZED", "Authentication required",
				"urn:drift:problem:unauthenticated", "")
			return
		}
		q := r.URL.Query()
		// A page is matched before the budget is charged: anyOffset pages are
		// the deliberate exception that answers every offset, so they must not
		// consume the offset-keyed script's budget or trip its terminal 500.
		var pg *envListPage
		for i := range pages {
			if pages[i].anyOffset || pages[i].offset == q.Get("offset") {
				pg = &pages[i]
				break
			}
		}
		s.mu.Lock()
		// Record every request — overruns included — so a test can assert both
		// the exact request sequence and the count at which the walk failed.
		s.queries = append(s.queries, q)
		// The terminal 500 exists so a walk that never terminates fails the
		// test instead of hanging it; an anyOffset page is scripted to answer
		// forever, so the client's own loop termination must end it, and only
		// offset-keyed scripts are capped.
		overrun := !(pg != nil && pg.anyOffset) && len(s.queries) > len(pages)
		count := len(s.queries)
		s.mu.Unlock()
		if overrun {
			writeProblem(w, 500, "INTERNAL_SERVER_ERROR",
				fmt.Sprintf("mock server served %d requests for %d scripted pages; the walk is not terminating",
					count, len(pages)),
				"urn:drift:problem:internal", "")
			return
		}
		if pg == nil {
			writeProblem(w, 500, "INTERNAL_SERVER_ERROR",
				"unscripted page at offset "+q.Get("offset"),
				"urn:drift:problem:internal", "")
			return
		}
		if pg.status != 0 && pg.status != http.StatusOK {
			code, ptype := "INTERNAL_SERVER_ERROR", "urn:drift:problem:internal"
			if pg.status == http.StatusTooManyRequests {
				code, ptype = "TOO_MANY_REQUESTS", "urn:drift:problem:rate-limited"
			}
			writeProblem(w, pg.status, code, "Something failed", ptype, "")
			return
		}
		items := make([]map[string]any, 0, len(pg.slugs))
		for _, slug := range pg.slugs {
			items = append(items, envItem(slug))
		}
		off := 0
		if pg.offset != "" {
			_, _ = fmt.Sscanf(pg.offset, "%d", &off)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": items,
			"pagination": map[string]any{
				"limit": pg.limit, "offset": off, "hasMore": pg.hasMore,
			},
		})
	})
	s.fakeServer = &fakeServer{Server: httptest.NewServer(mux)}
	t.Cleanup(s.fakeServer.Close)
	return s
}

// envItem builds one environment row; the slug is the only distinguishing field
// so tests can assert each row is printed exactly once.
func envItem(slug string) map[string]any {
	return map[string]any{
		"id": "b92b68a9-877a-4f14-a92e-db1a62b803d9", "slug": slug,
		"ticketId": "PROJ-1001", "namespace": "walk-ns", "status": "running",
		"expiresAt": "2026-07-27T10:40:00Z", "ttlHours": 48, "sleptAt": nil, "isPublic": true,
		"statusMessage": nil,
	}
}

// walkSlugs builds n unique, zero-padded slugs for one scripted page. The
// padding keeps any slug from being a substring of another, so a per-slug
// occurrence count is a precise "printed exactly once" check.
func walkSlugs(prefix string, n int) []string {
	slugs := make([]string, n)
	for i := range slugs {
		slugs[i] = fmt.Sprintf("%s-%03d", prefix, i)
	}
	return slugs
}

// pageQueryString flattens one recorded request into "limit=X,offset=Y" so the
// exact request sequence is asserted in a single comparison.
func pageQueryString(q url.Values) string {
	return "limit=" + q.Get("limit") + ",offset=" + q.Get("offset")
}

func pageQueryStrings(qs []url.Values) []string {
	out := make([]string, len(qs))
	for i, q := range qs {
		out[i] = pageQueryString(q)
	}
	return out
}

// assertRowsOnce fails unless every slug appears in out exactly once.
func assertRowsOnce(t *testing.T, out string, slugs []string) {
	t.Helper()
	for _, slug := range slugs {
		if n := strings.Count(out, slug); n != 1 {
			t.Fatalf("slug %q appears %d times in output, want exactly 1:\n%s", slug, n, out)
		}
	}
}

// tableSlugs extracts the slug (first column) of every data row from a table
// render, in output order. The header row and trailing blank line are skipped.
// Parsing only the first column keeps the check independent of the other
// columns' alignment, so it catches a REORDERING of the rows rather than a
// cosmetic change.
func tableSlugs(t *testing.T, out string) []string {
	t.Helper()
	var slugs []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "SLUG" {
			continue
		}
		slugs = append(slugs, fields[0])
	}
	return slugs
}

func TestEnvListAllWalksEveryPage(t *testing.T) {
	// Page prefixes are deliberately NOT in lexicographic server order (c, a,
	// b): a mutation that sorted the combined rows by slug would reorder the
	// output to a, b, c — every row still printed exactly once, so an
	// occurrence count alone would pass while the order assertion fails.
	pageC, pageA, pageB := walkSlugs("c", 50), walkSlugs("a", 50), walkSlugs("b", 50)
	srv := newPagedServer(t,
		envListPage{offset: "", slugs: pageC, hasMore: true, limit: 50},
		envListPage{offset: "50", slugs: pageA, hasMore: true, limit: 50},
		envListPage{offset: "100", slugs: pageB, hasMore: false, limit: 50},
	)
	h := newHarness(t)
	h.setup(t, srv.fakeServer, goodToken)

	out, errOut, code := h.run("env", "list", "--all")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	want := []string{"limit=50,offset=", "limit=50,offset=50", "limit=50,offset=100"}
	if got := pageQueryStrings(srv.got()); !reflect.DeepEqual(got, want) {
		t.Fatalf("request sequence = %v, want %v", got, want)
	}
	wantRows := append(append(pageC, pageA...), pageB...)
	assertRowsOnce(t, out, wantRows)
	if got := tableSlugs(t, out); !reflect.DeepEqual(got, wantRows) {
		t.Fatalf("rows printed out of server order: got %v..., want %v...",
			got[:min(len(got), 6)], wantRows[:min(len(wantRows), 6)])
	}
	if strings.Contains(errOut, "More results") {
		t.Fatalf("--all must not print the pagination note:\n%s", errOut)
	}
}

func TestEnvListAllOffsetStart(t *testing.T) {
	a, b := walkSlugs("a", 50), walkSlugs("b", 50)
	srv := newPagedServer(t,
		envListPage{offset: "50", slugs: a, hasMore: true, limit: 50},
		envListPage{offset: "100", slugs: b, hasMore: false, limit: 50},
	)
	h := newHarness(t)
	h.setup(t, srv.fakeServer, goodToken)

	out, errOut, code := h.run("env", "list", "--all", "--offset", "50", "-o", "json")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	want := []string{"limit=50,offset=50", "limit=50,offset=100"}
	if got := pageQueryStrings(srv.got()); !reflect.DeepEqual(got, want) {
		t.Fatalf("request sequence = %v, want %v", got, want)
	}
	if errOut != "" {
		t.Fatalf("--all -o json wrote to stderr:\n%s", errOut)
	}
	var resp struct {
		Items      []map[string]any `json:"items"`
		Pagination map[string]any   `json:"pagination"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	// A walk that starts at a nonzero offset reports the START offset and the
	// total rows it returned, never the last page's cursor.
	wantPagination := map[string]any{"limit": float64(100), "offset": float64(50), "hasMore": false}
	if !reflect.DeepEqual(resp.Pagination, wantPagination) {
		t.Fatalf("pagination = %v, want %v", resp.Pagination, wantPagination)
	}
	assertRowsOnce(t, out, append(a, b...))
}

func TestEnvListAllLimitUsageError(t *testing.T) {
	srv := newPagedServer(t,
		envListPage{offset: "", slugs: walkSlugs("a", 1), hasMore: false, limit: 50},
	)
	h := newHarness(t)
	h.setup(t, srv.fakeServer, goodToken)

	// ANY explicit --limit conflicts with --all — a plain 10, but also 0 and
	// negative values — because the conflict is the flag's PRESENCE, not its
	// value: --all fixes its own page size, so a supplied --limit would
	// otherwise be silently overridden.
	for _, limit := range []string{"10", "0", "-5"} {
		t.Run("limit "+limit, func(t *testing.T) {
			_, errOut, code := h.run("env", "list", "--all", "--limit", limit)
			if code != cliexit.Usage {
				t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
			}
			if !strings.Contains(errOut, "--all and --limit are mutually exclusive") {
				t.Fatalf("error does not name the conflict:\n%s", errOut)
			}
			if n := len(srv.got()); n != 0 {
				t.Fatalf("usage error made %d requests, want 0", n)
			}
		})
	}
}

func TestEnvListAllNegativeOffsetUsageError(t *testing.T) {
	srv := newPagedServer(t,
		envListPage{offset: "", slugs: walkSlugs("a", 1), hasMore: false, limit: 50},
	)
	h := newHarness(t)
	h.setup(t, srv.fakeServer, goodToken)

	// A negative --offset with --all used to fetch the first page at 0 (the
	// offset parameter is only set when positive) while the walk cursor started
	// from the negative value, landing mid-page on the next request and
	// re-fetching rows the first page already returned. It must be rejected up
	// front, before any request, with no partial result.
	out, errOut, code := h.run("env", "list", "--all", "--offset", "-5")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if out != "" {
		t.Fatalf("usage error printed partial stdout:\n%s", out)
	}
	if !strings.Contains(errOut, "--offset") {
		t.Fatalf("error does not name the --offset flag:\n%s", errOut)
	}
	if n := len(srv.got()); n != 0 {
		t.Fatalf("usage error made %d requests, want 0", n)
	}
}

func TestEnvListAllAbortsOnEmptyPageWithMore(t *testing.T) {
	srv := newPagedServer(t,
		envListPage{offset: "", slugs: walkSlugs("a", 50), hasMore: true, limit: 50},
		// A misbehaving server claims more results but returns none. --all
		// promises completeness, so the walk must fail loudly rather than
		// silently truncate the 50 rows it already accumulated.
		envListPage{offset: "50", slugs: nil, hasMore: true, limit: 50},
	)
	h := newHarness(t)
	h.setup(t, srv.fakeServer, goodToken)

	out, errOut, code := h.run("env", "list", "--all")
	if code != cliexit.Error {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Error, errOut)
	}
	want := []string{"limit=50,offset=", "limit=50,offset=50"}
	if got := pageQueryStrings(srv.got()); !reflect.DeepEqual(got, want) {
		t.Fatalf("request sequence = %v, want %v", got, want)
	}
	if !strings.Contains(errOut, "empty page while reporting more results") {
		t.Fatalf("error does not name the empty page:\n%s", errOut)
	}
	// No partial output: the 50 accumulated rows from the first page must not
	// be printed as a complete result.
	if out != "" {
		t.Fatalf("abort printed a partial result:\n%s", out)
	}
}

func TestEnvListAllEmptyFirstPageJSON(t *testing.T) {
	// An empty FIRST page is a distinct boundary from the empty-SECOND-page
	// case: nothing is ever fetched, yet the walk must still terminate cleanly
	// when the server reports no more (hasMore=false), and the JSON result must
	// be a well-formed empty list — items decodes to a non-nil empty array,
	// never null, so `jq '.items'` sees [] rather than a null that a script may
	// not expect.
	srv := newPagedServer(t,
		// hasMore is false: the server is done, so this is the legitimate
		// empty-terminal success path — the empty page WITH more is the abort
		// case covered by TestEnvListAllAbortsOnEmptyPageWithMore.
		envListPage{offset: "", slugs: nil, hasMore: false, limit: 50},
	)
	h := newHarness(t)
	h.setup(t, srv.fakeServer, goodToken)

	out, errOut, code := h.run("env", "list", "--all", "-o", "json")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	want := []string{"limit=50,offset="}
	if got := pageQueryStrings(srv.got()); !reflect.DeepEqual(got, want) {
		t.Fatalf("request sequence = %v, want %v", got, want)
	}
	if errOut != "" {
		t.Fatalf("--all -o json wrote to stderr:\n%s", errOut)
	}
	var resp struct {
		Items      []map[string]any `json:"items"`
		Pagination map[string]any   `json:"pagination"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if resp.Items == nil {
		t.Fatalf("items decoded to null, want an empty array ([]):\n%s", out)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("items = %d, want 0", len(resp.Items))
	}
	// With nothing returned the walk's report is {limit: 0, offset: <start>,
	// hasMore: false} — the START offset, and "nothing left" to match the
	// terminal empty page.
	wantPagination := map[string]any{"limit": float64(0), "offset": float64(0), "hasMore": false}
	if !reflect.DeepEqual(resp.Pagination, wantPagination) {
		t.Fatalf("pagination = %v, want %v", resp.Pagination, wantPagination)
	}
}

func TestEnvListAllAbortsOnEndlessPages(t *testing.T) {
	// A proxy or load balancer that ignores the offset parameter replays the
	// same non-empty page with hasMore=true for every offset. The walk's own
	// cursor still advances past one page per request, so the empty-page stop
	// never fires — every page IS non-empty — and only the page ceiling can
	// end the loop. The page is scripted for EVERY offset (anyOffset), because
	// an offset-keyed script would 500 on the second request, exercising the
	// mock's overrun path instead of the client's ceiling.
	srv := newPagedServer(t,
		envListPage{anyOffset: true, slugs: walkSlugs("loop", 1), hasMore: true, limit: 50},
	)
	h := newHarness(t)
	h.setup(t, srv.fakeServer, goodToken)

	out, errOut, code := h.run("env", "list", "--all")
	if code != cliexit.Error {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Error, errOut)
	}
	if !strings.Contains(errOut, fmt.Sprintf("aborted after %d pages", maxWalkPages)) {
		t.Fatalf("error does not name the page ceiling:\n%s", errOut)
	}
	// The ceiling is checked before the next fetch, so the walk makes exactly
	// maxWalkPages requests — never one more — and the server's own cap is
	// exempted for this script, so the count is genuinely the client's.
	if got := len(srv.got()); got != maxWalkPages {
		t.Fatalf("requests = %d, want %d", got, maxWalkPages)
	}
	if out != "" {
		t.Fatalf("abort printed a partial result:\n%s", out)
	}
}

func TestEnvListAllMidWalkError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   int
	}{
		{"rate limited", http.StatusTooManyRequests, cliexit.RateLimited},
		{"server error", http.StatusInternalServerError, cliexit.Error},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newPagedServer(t,
				envListPage{offset: "", slugs: walkSlugs("a", 50), hasMore: true, limit: 50},
				envListPage{offset: "50", slugs: walkSlugs("b", 1), status: c.status},
			)
			h := newHarness(t)
			h.setup(t, srv.fakeServer, goodToken)

			out, errOut, code := h.run("env", "list", "--all")
			if code != c.want {
				t.Fatalf("exit %d, want %d\n%s", code, c.want, errOut)
			}
			// The HTTP problem diagnostic the mock served must reach the
			// operator's stderr for BOTH statuses: a mid-walk failure is only
			// as actionable as the server's reason for it. Only the mock's own
			// message is asserted — never the CLI's "Error:" prefix or layout —
			// so this pins server error propagation, not presentation.
			if !strings.Contains(errOut, "Something failed") {
				t.Fatalf("server problem message not surfaced for status %d:\n%s", c.status, errOut)
			}
			if out != "" {
				t.Fatalf("partial result printed for a mid-walk failure:\n%s", out)
			}
			if got := len(srv.got()); got != 2 {
				t.Fatalf("requests = %d, want 2 (walk aborted at the failing page)", got)
			}
		})
	}
}

func TestEnvListAllJSONPagination(t *testing.T) {
	a, b, c := walkSlugs("a", 50), walkSlugs("b", 50), walkSlugs("c", 50)
	srv := newPagedServer(t,
		envListPage{offset: "", slugs: a, hasMore: true, limit: 50},
		envListPage{offset: "50", slugs: b, hasMore: true, limit: 50},
		envListPage{offset: "100", slugs: c, hasMore: false, limit: 50},
	)
	h := newHarness(t)
	h.setup(t, srv.fakeServer, goodToken)

	out, errOut, code := h.run("env", "list", "--all", "-o", "json")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if errOut != "" {
		t.Fatalf("--all -o json wrote to stderr:\n%s", errOut)
	}
	var resp struct {
		Items      []map[string]any `json:"items"`
		Pagination map[string]any   `json:"pagination"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(resp.Items) != 150 {
		t.Fatalf("items = %d, want 150", len(resp.Items))
	}
	want := map[string]any{"limit": float64(150), "offset": float64(0), "hasMore": false}
	if !reflect.DeepEqual(resp.Pagination, want) {
		t.Fatalf("pagination = %v, want %v", resp.Pagination, want)
	}
}

func TestEnvListAllAdvancesByItemCount(t *testing.T) {
	// Pages shorter than the requested 50: the next offset must advance by the
	// page's ACTUAL item count, not the requested limit.
	a, b, c := walkSlugs("a", 3), walkSlugs("b", 2), walkSlugs("c", 1)
	srv := newPagedServer(t,
		envListPage{offset: "", slugs: a, hasMore: true, limit: 50},
		envListPage{offset: "3", slugs: b, hasMore: true, limit: 50},
		envListPage{offset: "5", slugs: c, hasMore: false, limit: 50},
	)
	h := newHarness(t)
	h.setup(t, srv.fakeServer, goodToken)

	out, errOut, code := h.run("env", "list", "--all")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	want := []string{"limit=50,offset=", "limit=50,offset=3", "limit=50,offset=5"}
	if got := pageQueryStrings(srv.got()); !reflect.DeepEqual(got, want) {
		t.Fatalf("request sequence = %v, want %v", got, want)
	}
	assertRowsOnce(t, out, append(append(a, b...), c...))
}

func TestEnvListAllRetainsStatusFilter(t *testing.T) {
	a, b := walkSlugs("a", 2), walkSlugs("b", 2)
	srv := newPagedServer(t,
		envListPage{offset: "", slugs: a, hasMore: true, limit: 50},
		envListPage{offset: "2", slugs: b, hasMore: false, limit: 50},
	)
	h := newHarness(t)
	h.setup(t, srv.fakeServer, goodToken)

	out, errOut, code := h.run("env", "list", "--all", "--status", "running")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for i, q := range srv.got() {
		if q.Get("status") != "running" {
			t.Fatalf("request %d lacks status=running: %v", i, q)
		}
	}
	assertRowsOnce(t, out, append(a, b...))
}

func TestEnvListAllMineSendsCreatorEveryPage(t *testing.T) {
	a, b, c := walkSlugs("a", 50), walkSlugs("b", 50), walkSlugs("c", 50)
	srv := newPagedServer(t,
		envListPage{offset: "", slugs: a, hasMore: true, limit: 50},
		envListPage{offset: "50", slugs: b, hasMore: true, limit: 50},
		envListPage{offset: "100", slugs: c, hasMore: false, limit: 50},
	)
	h := newHarness(t)
	h.setup(t, srv.fakeServer, goodToken)

	out, errOut, code := h.run("env", "list", "--all", "--mine")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	// --mine rides every page request: each one must carry the creator the
	// whoami call resolved, never only the first.
	qs := srv.got()
	if n := len(qs); n != 3 {
		t.Fatalf("requests = %d, want 3", n)
	}
	for i, q := range qs {
		if got := q.Get("creator"); got != "operator@example.com" {
			t.Fatalf("request %d creator = %q, want operator@example.com", i, got)
		}
	}
	// The email resolution happens exactly once, before the walk; a walk that
	// re-ran whoami per page would trip this.
	if n := srv.whoamiCalls; n != 1 {
		t.Fatalf("whoami called %d times, want 1", n)
	}
	assertRowsOnce(t, out, append(append(a, b...), c...))
}

func TestEnvListAllOwnerSendsCreatorEveryPage(t *testing.T) {
	a, b, c := walkSlugs("a", 50), walkSlugs("b", 50), walkSlugs("c", 50)
	srv := newPagedServer(t,
		envListPage{offset: "", slugs: a, hasMore: true, limit: 50},
		envListPage{offset: "50", slugs: b, hasMore: true, limit: 50},
		envListPage{offset: "100", slugs: c, hasMore: false, limit: 50},
	)
	h := newHarness(t)
	h.setup(t, srv.fakeServer, goodToken)

	out, errOut, code := h.run("env", "list", "--all", "--owner", "alice@example.com")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	// The --owner email must ride every page request exactly as given.
	qs := srv.got()
	if n := len(qs); n != 3 {
		t.Fatalf("requests = %d, want 3", n)
	}
	for i, q := range qs {
		if got := q.Get("creator"); got != "alice@example.com" {
			t.Fatalf("request %d creator = %q, want alice@example.com", i, got)
		}
	}
	// --owner is a direct value, so no whoami call is ever made.
	if n := srv.whoamiCalls; n != 0 {
		t.Fatalf("--owner triggered %d whoami calls, want 0", n)
	}
	assertRowsOnce(t, out, append(append(a, b...), c...))
}
