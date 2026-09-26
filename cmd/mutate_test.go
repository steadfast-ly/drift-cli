package cmd

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steadfast-ly/drift-cli/internal/auth"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
	"github.com/steadfast-ly/drift-cli/internal/infer"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files in cmd/testdata")

// Table rendering prints timestamps in LOCAL time, so the golden files would
// otherwise encode whichever zone happened to generate them and fail everywhere
// else. Pinned here rather than avoided by dropping the timestamps, because the
// column is part of the output being tested.
func TestMain(m *testing.M) {
	_ = os.Setenv("TZ", "UTC")
	time.Local = time.UTC
	os.Exit(m.Run())
}

const (
	envID  = "11111111-1111-4111-8111-111111111111"
	svcID  = "22222222-2222-4222-8222-222222222222"
	repoID = "33333333-3333-4333-8333-333333333333"
	promID = "44444444-4444-4444-8444-444444444444"

	// Monorepo fixture: two services from the same repo (acme/forge).
	forgeRepoID  = "55555555-5555-4555-8555-555555555555"
	binderRepoID = "66666666-6666-4666-8666-666666666666"
)

// mutServer is a drift with the whole write surface, scripted.
//
// The environment's status comes from a QUEUE rather than from a variable, so a
// test can script the exact sequence a wait will observe — including the
// `deploying -> deploy_failed -> running` blip, which is the behaviour most
// likely to be got wrong and impossible to reproduce against a static fake.
type mutServer struct {
	*httptest.Server

	mu       sync.Mutex
	statuses []string
	builds   []string
	promotes []string
	calls    []string
	// cancelBody is the decoded body of the last cancel request, so a test can
	// tell a reason that was sent from one that was not sent at all.
	cancelBody map[string]any
	// rateLimit fires a 429 on the Nth matching mutation, once.
	rateLimitAfter int
	retryAfter     int
	mutations      int
	// extraRepos are appended to the repository list, for the ambiguity case.
	extraRepos []map[string]any
	// prd403Elevation causes the prd promote endpoint to return a 403 with the
	// elevation-required problem type.
	prd403Elevation bool
	// e2eTestsBranch adds the profile-conditional `e2e-tests-branch`
	// capability to the served discovery document when true (refreshDiscoveryDoc
	// must be called after setting it). Off by default, so an e2e trigger
	// without --tests-branch — and a --tests-branch refusal — work against the
	// plain mutServer.
	e2eTestsBranch bool
	// e2eRead adds the profile-conditional `environments.e2e-read` capability
	// to the served discovery document when true (refreshDiscoveryDoc must be
	// called after setting it). Off by default, so the --wait tests exercise
	// the audit-log fallback unless they opt in.
	e2eRead bool
	// noCancelFeature REMOVES `promotions.cancel` from the served discovery
	// document, for the capability-gate test (refreshDiscoveryDoc must be called
	// after setting it). Inverted from e2eTestsBranch because every other cancel
	// test needs the capability present, and one of them forgetting to add it
	// would fail for the wrong reason.
	noCancelFeature bool
	// discoveryDoc is the served `/.well-known/drift.json` body, rebuilt by
	// refreshDiscoveryDoc from the capability flags above.
	discoveryDoc []byte
}

func (s *mutServer) nextStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.statuses) == 0 {
		return "running"
	}
	v := s.statuses[0]
	if len(s.statuses) > 1 {
		s.statuses = s.statuses[1:]
	}
	return v
}

func (s *mutServer) nextPromotion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.promotes) == 0 {
		return "completed"
	}
	v := s.promotes[0]
	if len(s.promotes) > 1 {
		s.promotes = s.promotes[1:]
	}
	return v
}

func (s *mutServer) record(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, what)
}

func (s *mutServer) seen(what string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c == what {
			n++
		}
	}
	return n
}

// cancelReason returns the reason the last cancel request carried, and whether
// the field was present at all. The two are different: an omitted reason is
// legitimate, and a reason sent as the empty string is not.
func (s *mutServer) cancelReason() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.cancelBody["reason"]
	if !ok {
		return "", false
	}
	reason, _ := v.(string)
	return reason, true
}

// refreshDiscoveryDoc rebuilds the served discovery document from the
// server's capability flags. Capabilities like `e2e-tests-branch` are
// profile-conditional on a real server, so a test that exercises them flips
// the flag and refreshes. Called with the fields already set; safe before the
// server starts serving.
func (s *mutServer) refreshDiscoveryDoc() {
	s.mu.Lock()
	defer s.mu.Unlock()
	features := []string{
		"environments.read", "environments.write", "repositories.read",
		"releases.read", "promotions.rc", "promotions.hotfix", "promotions.prd",
	}
	if !s.noCancelFeature {
		features = append(features, "promotions.cancel")
	}
	if s.e2eTestsBranch {
		features = append(features, "e2e-tests-branch")
	}
	if s.e2eRead {
		features = append(features, "environments.e2e-read")
	}
	doc, _ := json.Marshal(map[string]any{
		"org": "acme", "version": "1.0.0", "auth": "sso",
		"services":               map[string]string{"api.v1": "/api/v1"},
		"features_supported":     features,
		"minimum_client_version": "0.1.0",
	})
	s.discoveryDoc = doc
}

func writeProblem(w http.ResponseWriter, status int, code, msg, ptype, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"defined": true, "code": code, "status": status, "message": msg,
		"data": map[string]any{"type": ptype, "detail": detail},
	})
}

func newMutServer(t *testing.T) *mutServer {
	t.Helper()
	s := &mutServer{rateLimitAfter: -1, retryAfter: 1}
	s.refreshDiscoveryDoc()
	mux := http.NewServeMux()

	mux.HandleFunc("/.well-known/drift.json", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		doc := s.discoveryDoc
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	})

	// One gate for every mutation, so the rate-limit script does not have to be
	// repeated per endpoint. `Retry-After` in whole seconds, exactly as the
	// contract declares it.
	limited := func(w http.ResponseWriter) bool {
		s.mu.Lock()
		s.mutations++
		fire := s.rateLimitAfter >= 0 && s.mutations > s.rateLimitAfter
		retry := s.retryAfter
		s.mu.Unlock()
		if !fire {
			return false
		}
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
		writeProblem(w, 429, "TOO_MANY_REQUESTS", "Rate limit exceeded",
			"urn:drift:problem:rate-limited",
			fmt.Sprintf("Rate limit of 20 requests per window exceeded for this credential. Retry in %ds.", retry))
		return true
	}

	mux.HandleFunc("/api/v1/repositories", func(w http.ResponseWriter, _ *http.Request) {
		items := []map[string]any{{
			"id": repoID, "owner": "acme", "name": "widget", "fullName": "acme/widget",
			"displayName": "Widget", "description": nil, "defaultBranch": "main",
			"helmChartKey": "widget", "isActive": true,
			"stgUrl": nil, "rcUrl": nil, "prdUrl": nil,
			"applicationGroupId": nil, "applicationGroup": nil,
		}}
		items = append(items, s.extraRepos...)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":      items,
			"pagination": map[string]any{"limit": 50, "offset": 0, "hasMore": false},
		})
	})

	mux.HandleFunc("/api/v1/environments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":      []any{},
				"pagination": map[string]any{"limit": 20, "offset": 0, "hasMore": false},
			})
			return
		}
		if limited(w) {
			return
		}
		body, _ := readJSON(r)
		s.record("create:" + fmt.Sprint(body["slug"]))
		if repos, ok := body["repos"].([]any); ok {
			for _, r := range repos {
				if repo, ok := r.(map[string]any); ok {
					s.record("repo:" + fmt.Sprint(repo["repositoryId"]))
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"environmentId": envID})
	})

	mux.HandleFunc("/api/v1/environments/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/environments/")
		parts := strings.Split(rest, "/")
		ref := parts[0]

		if len(parts) == 1 && r.Method == http.MethodGet {
			if ref != "proof-alpha" && ref != envID {
				writeProblem(w, 404, "NOT_FOUND", "Environment not found",
					"urn:drift:problem:not-found", "No such environment.")
				return
			}
			status := s.nextStatus()
			s.mu.Lock()
			builds := append([]string(nil), s.builds...)
			s.mu.Unlock()
			buildRows := make([]map[string]any, 0, len(builds))
			for _, b := range builds {
				buildRows = append(buildRows, map[string]any{
					"id": svcID, "repositoryId": repoID, "branch": "topic", "prNumber": nil,
					"commitSha": "abcdef1234567890", "status": b, "imageTag": nil,
					"startedAt": nil, "createdAt": "2026-07-26T10:00:00Z",
				})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"environment": map[string]any{
					"id": envID, "slug": "proof-alpha", "ticketId": "PROJ-1001",
					"namespace": "pr-proof-alpha", "status": status,
					"expiresAt": "2026-07-28T10:00:00Z", "ttlHours": 48,
					"sleptAt": nil, "isPublic": false,
				},
				"services": []map[string]any{{
					"id": svcID, "repositoryId": repoID, "branch": "topic",
					"prNumber": nil, "imageTag": nil,
				}},
				"builds": buildRows,
			})
			return
		}

		if limited(w) {
			return
		}
		action := r.Method
		if len(parts) > 1 {
			action = strings.Join(parts[1:], "/")
		}
		s.record(action)
		if action == "conflict" {
			writeProblem(w, 409, "CONFLICT", "Cannot sleep environment in building state",
				"urn:drift:problem:invalid-transition", "")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"environmentId": envID})
	})

	mux.HandleFunc("/api/v1/releases/state", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"stg": map[string]any{
				"services": []map[string]any{{
					"helmChartKey": "widget", "imageTag": "stg-abc1234",
					"commitSha": "abc1234def5678", "deploymentTimestamp": nil, "ecrRepository": "widget",
				}},
				"health": []map[string]any{{
					"helmChartKey": "widget", "readyReplicas": 2, "totalReplicas": 2,
					"status": "healthy", "pods": []any{},
				}},
				"fetchedAt": "2026-07-26T10:00:00Z",
			},
			"rc": map[string]any{
				"services": []map[string]any{{
					"helmChartKey": "widget", "imageTag": "rc-9990000",
					"commitSha": "9990000aaa", "deploymentTimestamp": nil, "ecrRepository": "widget",
				}},
				"health": []map[string]any{{
					"helmChartKey": "widget", "readyReplicas": 1, "totalReplicas": 2,
					"status": "progressing", "pods": []any{},
				}},
				"fetchedAt": "2026-07-26T10:00:00Z",
			},
		})
	})

	promotion := func(status string) map[string]any {
		return map[string]any{
			"id": promID, "promotionType": "rc", "services": []string{"widget"},
			"status": status, "statusMessage": nil, "workflowDispatches": []any{},
			"versionSnapshot": []any{}, "serviceHealthStatuses": map[string]any{},
			"createdBy": "operator@example.com", "hotfixBranch": nil,
			"createdAt": "2026-07-26T10:00:00Z", "completedAt": nil,
		}
	}
	mux.HandleFunc("/api/v1/releases/promotions/active", func(w http.ResponseWriter, _ *http.Request) {
		status := s.nextPromotion()
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{"active": nil, "recent": []map[string]any{promotion(status)}}
		if status != "completed" && status != "failed" && status != "deploy_failed" {
			body["active"] = promotion(status)
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/api/v1/releases/promotions/history", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":      []map[string]any{promotion("completed")},
			"pagination": map[string]any{"limit": 20, "offset": 0, "hasMore": false},
		})
	})
	mux.HandleFunc("/api/v1/releases/promotions/rc", func(w http.ResponseWriter, _ *http.Request) {
		if limited(w) {
			return
		}
		s.record("promote:rc")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"promotionId": promID, "dispatchCount": 1})
	})
	mux.HandleFunc("/api/v1/releases/promotions/rc/hotfix", func(w http.ResponseWriter, _ *http.Request) {
		if limited(w) {
			return
		}
		s.record("promote:hotfix")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"promotionId": promID, "dispatchCount": 1})
	})
	mux.HandleFunc("/api/v1/releases/promotions/prd", func(w http.ResponseWriter, r *http.Request) {
		// Only match POST, not /prd/hotfix (which has its own handler).
		if r.URL.Path != "/api/v1/releases/promotions/prd" {
			http.NotFound(w, r)
			return
		}
		if limited(w) {
			return
		}
		s.mu.Lock()
		elev := s.prd403Elevation
		s.mu.Unlock()
		if elev {
			w.Header().Set("WWW-Authenticate", `Bearer realm="drift", error="insufficient_scope", scope="promote:prd"`)
			writeProblem(w, 403, "FORBIDDEN", "Elevated credential required",
				"urn:drift:problem:elevation-required", "This operation requires a credential scoped to promote:prd.")
			return
		}
		s.record("promote:prd")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"promotionId": promID, "dispatchCount": 1})
	})
	mux.HandleFunc("/api/v1/releases/promotions/prd/hotfix", func(w http.ResponseWriter, _ *http.Request) {
		if limited(w) {
			return
		}
		s.record("promote:prd:hotfix")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"promotionId": promID, "dispatchCount": 1})
	})
	// Registered on the one id the fixtures use, so a request for any other id
	// is a genuine 404 from the mux rather than a silently accepted cancel.
	mux.HandleFunc("/api/v1/releases/promotions/"+promID+"/cancel", func(w http.ResponseWriter, r *http.Request) {
		if limited(w) {
			return
		}
		body, _ := readJSON(r)
		s.mu.Lock()
		s.cancelBody = body
		s.mu.Unlock()
		s.record("cancel")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"promotionId": promID, "previousStatus": "promoting", "status": "failed",
		})
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func readJSON(r *http.Request) (map[string]any, error) {
	var m map[string]any
	err := json.NewDecoder(r.Body).Decode(&m)
	return m, err
}

// mutHarness is `harness` pointed at a write-capable drift, with the poll
// interval collapsed so a real wait runs at test speed.
func newMutHarness(t *testing.T, s *mutServer) *harness {
	t.Helper()
	h := newHarness(t)
	h.app.waitInterval = time.Millisecond
	// Scaled with the interval: the production window is thirty seconds against
	// a three-second poll, ten polls; five milliseconds against a one-millisecond
	// poll keeps the same shape without the wall-clock cost.
	h.app.waitFailureWindow = 5 * time.Millisecond
	if _, _, code := h.run("context", "add", "proof", "--endpoint", s.URL); code != 0 {
		t.Fatalf("context add exited %d", code)
	}
	h.key = auth.NewKey("proof", s.URL)
	if _, err := h.store.Set(h.key, goodToken); err != nil {
		t.Fatal(err)
	}
	return h
}

// --- destructive operations and the non-TTY refusal -------------------------

// The rule that matters most: a destructive command in a script REFUSES rather
// than prompting into a void or, worse, proceeding. The harness's streams are
// buffers, which is exactly what a redirect looks like.
func TestDestructiveCommandsRefuseWithoutYesOffATerminal(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)

	for _, args := range [][]string{
		{"env", "rm", "proof-alpha"},
		{"env", "relaunch", "proof-alpha"},
		{"env", "remove-service", "proof-alpha", "widget"},
		{"release", "promote", "rc", "widget"},
		{"release", "promote", "hotfix", "widget", "--branch", "hotfix/x"},
		{"release", "cancel", promID},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, errOut, code := h.run(args...)
			if code != cliexit.Usage {
				t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
			}
			if !strings.Contains(errOut, "--yes") {
				t.Fatalf("the refusal does not name the remedy: %s", errOut)
			}
			if !strings.Contains(errOut, "not interactive") {
				t.Fatalf("the refusal does not say why: %s", errOut)
			}
		})
	}
	// No mutation reached the server. That is the claim: a refusal must not
	// depend on a READ, so `release cancel` skips even the promotion lookup it
	// would make for its summary (pinned in its own test), while the env
	// commands still resolve the environment they were about to change.
	if n := s.seen("DELETE") + s.seen("relaunch") + s.seen("promote:rc") + s.seen("cancel"); n != 0 {
		t.Fatalf("%d destructive calls were made despite the refusal", n)
	}
}

func TestYesProceedsWithoutATerminal(t *testing.T) {
	s := newMutServer(t)
	s.statuses = []string{"destroying"}
	h := newMutHarness(t, s)

	out, errOut, code := h.run("env", "rm", "proof-alpha", "--yes")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if s.seen("DELETE") != 1 {
		t.Fatalf("destroy was not called: %v", s.calls)
	}
	if !strings.Contains(out, "destroying") {
		t.Fatalf("the resulting state was not reported: %s", out)
	}
}

// --- wait semantics through the real command tree ---------------------------

// The blip, end to end. A single `deploy_failed` between two healthy states
// must not fail the command.
func TestWaitToleratesATransientDeployFailed(t *testing.T) {
	s := newMutServer(t)
	s.statuses = []string{"building", "deploying", "deploy_failed", "deploying", "running"}
	h := newMutHarness(t, s)

	out, errOut, code := h.run("env", "wait", "proof-alpha", "--for", "running")
	if code != cliexit.OK {
		t.Fatalf("exit %d, want 0 — a transient deploy_failed failed the wait\n%s", code, errOut)
	}
	if !strings.Contains(out, "running") {
		t.Fatalf("final state not reported: %s", out)
	}
	// The transition history reaches stderr, and the blip is IN it: the operator
	// should be able to see what happened afterwards.
	if !strings.Contains(errOut, "deploy_failed") {
		t.Fatalf("the blip was hidden: %s", errOut)
	}
}

func TestWaitFailsOnASustainedFailure(t *testing.T) {
	s := newMutServer(t)
	s.statuses = []string{"deploy_failed"}
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "wait", "proof-alpha", "--for", "running")
	if code != cliexit.Conflict {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Conflict, errOut)
	}
	if !strings.Contains(errOut, "held that state for") {
		t.Fatalf("the rule was not explained: %s", errOut)
	}
	// The message must not claim more than the observation supports.
	if strings.Contains(errOut, "is not a transient") {
		t.Fatalf("the message asserts a conclusion the evidence cannot carry: %s", errOut)
	}
}

// A build still running is recovery in progress, so the same sustained
// `build_failed` that would fail above does not fail here.
func TestWaitDoesNotFailWhileABuildIsInFlight(t *testing.T) {
	s := newMutServer(t)
	s.statuses = []string{"build_failed", "build_failed", "build_failed", "build_failed", "running"}
	s.builds = []string{"in_progress"}
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "wait", "proof-alpha", "--for", "running")
	if code != cliexit.OK {
		t.Fatalf("exit %d, want 0 — a retry in flight was read as a failure\n%s", code, errOut)
	}
}

func TestWaitTimeoutIsExitSix(t *testing.T) {
	s := newMutServer(t)
	s.statuses = []string{"building"}
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "wait", "proof-alpha", "--for", "running", "--timeout", "20ms")
	if code != cliexit.WaitTimeout {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.WaitTimeout, errOut)
	}
	if !strings.Contains(errOut, "timed out") || !strings.Contains(errOut, "building") {
		t.Fatalf("the timeout does not say what it saw: %s", errOut)
	}
}

func TestWaitRefusesAnUnreachableGoal(t *testing.T) {
	s := newMutServer(t)
	s.statuses = []string{"sleeping"}
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "wait", "proof-alpha", "--for", "running", "--timeout", "10m")
	if code != cliexit.Conflict {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Conflict, errOut)
	}
	if !strings.Contains(errOut, "will not reach") {
		t.Fatalf("the refusal is not explained: %s", errOut)
	}
}

func TestWaitRejectsAnUnknownState(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)
	_, errOut, code := h.run("env", "wait", "proof-alpha", "--for", "wat")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d", code, cliexit.Usage)
	}
	if !strings.Contains(errOut, "deploy_failed") {
		t.Fatalf("the alternatives were not listed: %s", errOut)
	}
}

// Which commands block by default is a contract of its own.
func TestDefaultWaitPolicyPerCommand(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		blocks bool
	}{
		{"create blocks", []string{"env", "create", "--slug", "proof-alpha", "--repo", "widget:topic", "--yes"}, true},
		{"wake blocks", []string{"env", "wake", "proof-alpha"}, true},
		{"relaunch blocks", []string{"env", "relaunch", "proof-alpha", "--yes"}, true},
		{"retry-build blocks", []string{"env", "retry-build", "proof-alpha"}, true},
		{"redeploy blocks", []string{"env", "redeploy", "proof-alpha"}, true},
		{"rm returns", []string{"env", "rm", "proof-alpha", "--yes"}, false},
		{"sleep returns", []string{"env", "sleep", "proof-alpha"}, false},
		{"cancel returns", []string{"env", "cancel", "proof-alpha"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newMutServer(t)
			// A state that is neither the goal nor a failure: a command that
			// blocks polls it repeatedly and times out, one that does not
			// returns 0 immediately.
			s.statuses = []string{"deploying"}
			h := newMutHarness(t, s)
			args := append(append([]string{}, c.args...), "--wait-timeout", "30ms")
			_, errOut, code := h.run(args...)
			if c.blocks && code != cliexit.WaitTimeout {
				t.Fatalf("exit %d, want %d — this command should block by default\n%s",
					code, cliexit.WaitTimeout, errOut)
			}
			if !c.blocks && code != cliexit.OK {
				t.Fatalf("exit %d, want 0 — this command should return immediately\n%s", code, errOut)
			}
		})
	}
}

func TestNoWaitAndWaitOverrideTheDefault(t *testing.T) {
	s := newMutServer(t)
	s.statuses = []string{"deploying"}
	h := newMutHarness(t, s)

	// create blocks by default; --no-wait returns at once.
	if _, errOut, code := h.run("env", "create", "--slug", "proof-alpha",
		"--repo", "widget:topic", "--yes", "--no-wait"); code != cliexit.OK {
		t.Fatalf("--no-wait still blocked: exit %d\n%s", code, errOut)
	}

	// sleep returns by default; --wait follows it to `sleeping`. The two are
	// told apart by WHAT they report: without waiting the state is whatever the
	// follow-up read saw, with waiting it is the goal.
	s2 := newMutServer(t)
	s2.statuses = []string{"running", "running", "sleeping"}
	h2 := newMutHarness(t, s2)
	out, errOut, code := h2.run("env", "sleep", "proof-alpha", "--wait", "--wait-timeout", "5s")
	if code != cliexit.OK {
		t.Fatalf("--wait failed: exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "sleeping") || !strings.Contains(out, "true") {
		t.Fatalf("--wait did not block: %s", out)
	}
}

// `cancel --wait` is the other command-only goal, and must also be able to
// reach exit 0.
func TestWaitOnCancelSucceedsOnceTheWriteIsVisible(t *testing.T) {
	s := newMutServer(t)
	s.statuses = []string{"building", "canceled"}
	h := newMutHarness(t, s)

	if _, errOut, code := h.run("env", "cancel", "proof-alpha", "--wait",
		"--wait-timeout", "5s"); code != cliexit.OK {
		t.Fatalf("exit %d, want 0\n%s", code, errOut)
	}
}

// When a commanded transition never becomes visible, the honest answer is a
// TIMEOUT, not a conflict.
//
// No system edge anywhere targets `sleeping` — the only way in is the SLEEP
// command — so a reachability argument declares every `sleep --wait` futile,
// which is exactly backwards when this process just issued the command. The CLI
// cannot prove anything here, and saying the operation "cannot succeed" on a
// sleep the server accepted would be a fabrication.
func TestWaitOnSleepThatNeverLandsIsATimeoutNotAConflict(t *testing.T) {
	s := newMutServer(t)
	s.statuses = []string{"running"} // the sleep never takes effect
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "sleep", "proof-alpha", "--wait", "--wait-timeout", "40ms")
	if code != cliexit.WaitTimeout {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.WaitTimeout, errOut)
	}
	if strings.Contains(errOut, "cannot succeed") {
		t.Fatalf("a commanded transition was declared futile: %s", errOut)
	}
}

// The fail-fast refusal survives where it is SOUND: a bare `drift env wait`
// asks about work nobody here started, and `running` does have server-raised
// edges into it that `sleeping` cannot reach.
func TestBareWaitStillRefusesAProvablyUnreachableGoal(t *testing.T) {
	s := newMutServer(t)
	s.statuses = []string{"sleeping"}
	h := newMutHarness(t, s)

	start := time.Now()
	_, errOut, code := h.run("env", "wait", "proof-alpha", "--for", "running", "--timeout", "10m")
	if code != cliexit.Conflict {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Conflict, errOut)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("it waited out the timeout instead of reasoning about the machine")
	}
	// The hint names the command that walks the edge.
	if !strings.Contains(errOut, "drift env wake") {
		t.Fatalf("the hint does not name the command that would help: %s", errOut)
	}
}

// A state this build has never heard of must not be reasoned about. Every rule
// would otherwise answer from an empty edge list and invent both claims.
func TestUnknownServerStateIsReportedAsSkewNotAsAConflict(t *testing.T) {
	s := newMutServer(t)
	s.statuses = []string{"hibernating"}
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "wait", "proof-alpha", "--for", "running", "--timeout", "10m")
	if code != cliexit.Error {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Error, errOut)
	}
	if !strings.Contains(errOut, "does not know") || !strings.Contains(errOut, "upgrade drift") {
		t.Fatalf("skew was not reported as skew: %s", errOut)
	}
	if strings.Contains(errOut, "will not reach") {
		t.Fatalf("an unknown state produced a fabricated reachability claim: %s", errOut)
	}
}

func TestWaitAndNoWaitTogetherIsAUsageError(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)
	if _, _, code := h.run("env", "sleep", "proof-alpha", "--wait", "--no-wait"); code != cliexit.Usage {
		t.Fatalf("exit %d, want %d", code, cliexit.Usage)
	}
}

// --- redeploy ---------------------------------------------------------------

func TestRedeployCallsTheServerAndWaitsForRunning(t *testing.T) {
	s := newMutServer(t)
	s.statuses = []string{"deploy_failed", "deploying", "running"}
	h := newMutHarness(t, s)

	out, errOut, code := h.run("env", "redeploy", "proof-alpha")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if s.seen("redeploy") != 1 {
		t.Fatalf("expected one redeploy call, got %d; calls: %v", s.seen("redeploy"), s.calls)
	}
	if !strings.Contains(out, "running") {
		t.Fatalf("final state not reported: %s", out)
	}
	if !strings.Contains(out, "true") {
		t.Fatalf("waited column not true: %s", out)
	}
}

func TestRedeployNoWaitReturnsImmediately(t *testing.T) {
	s := newMutServer(t)
	s.statuses = []string{"deploying"}
	h := newMutHarness(t, s)

	out, errOut, code := h.run("env", "redeploy", "proof-alpha", "--no-wait")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if s.seen("redeploy") != 1 {
		t.Fatalf("expected one redeploy call; calls: %v", s.calls)
	}
	if !strings.Contains(out, "deploying") {
		t.Fatalf("status after mutation not reported: %s", out)
	}
	if !strings.Contains(out, "false") {
		t.Fatalf("waited column not false: %s", out)
	}
}

func TestRedeployNotFoundExitsNonZero(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)

	_, _, code := h.run("env", "redeploy", "nonexistent")
	if code == cliexit.OK {
		t.Fatal("expected a non-zero exit for a missing env")
	}
	if s.seen("redeploy") != 0 {
		t.Fatal("the redeploy endpoint was called despite a 404 on resolve")
	}
}

func TestRedeployConflictSurfacesTheRefusal(t *testing.T) {
	s := newMutServer(t)
	s.statuses = []string{"running"}
	h := newMutHarness(t, s)

	// Intercept the redeploy POST and return a 409, mirroring the server's
	// invalid-transition problem shape.
	base := s.Config.Handler
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/environments/")
		if r.Method == http.MethodPost && strings.HasSuffix(rest, "/redeploy") {
			s.record("redeploy")
			state := "running"
			event := "REDEPLOY"
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"defined": true, "code": "CONFLICT", "status": 409,
				"message": "Cannot redeploy environment in running state",
				"data": map[string]any{
					"type":  "urn:drift:problem:invalid-transition",
					"state": state, "event": event,
				},
			})
			return
		}
		base.ServeHTTP(w, r)
	})

	_, errOut, code := h.run("env", "redeploy", "proof-alpha")
	if code != cliexit.Conflict {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Conflict, errOut)
	}
	if s.seen("redeploy") != 1 {
		t.Fatalf("expected exactly one redeploy call; calls: %v", s.calls)
	}
	if !strings.Contains(errOut, "Cannot redeploy") {
		t.Fatalf("the refusal message was not surfaced: %s", errOut)
	}
}

// --- rate limiting ----------------------------------------------------------

func TestRateLimitedMutationIsExitSevenWithTheRetryInterval(t *testing.T) {
	s := newMutServer(t)
	s.rateLimitAfter = 0 // the very first mutation is limited
	s.retryAfter = 37
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "sleep", "proof-alpha")
	if code != cliexit.RateLimited {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.RateLimited, errOut)
	}
	if !strings.Contains(errOut, "37s") {
		t.Fatalf("the Retry-After was not surfaced: %s", errOut)
	}
	if !strings.Contains(errOut, "per credential") {
		t.Fatalf("the limit's scope was not explained: %s", errOut)
	}
}

// A 429 DURING a wait is the server asking for time, not a failure of the
// environment: the wait backs off and carries on.
func TestRateLimitDuringAWaitBacksOffRatherThanFailing(t *testing.T) {
	s := newMutServer(t)
	s.statuses = []string{"deploying", "running"}
	h := newMutHarness(t, s)

	// The READS are limited here, not the mutation: `rateLimitAfter` counts
	// mutations, so the limit is scripted onto the poll path by hand. It fires on
	// the second GET rather than the first, because the first is the reference
	// resolution that happens BEFORE the wait starts — a 429 there is a failed
	// command, not a throttled poll, and the two are deliberately different.
	reads := 0
	base := s.Config.Handler
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/environments/") {
			reads++
		}
		if reads == 2 && r.Method == http.MethodGet &&
			strings.HasPrefix(r.URL.Path, "/api/v1/environments/") {
			w.Header().Set("Retry-After", "1")
			writeProblem(w, 429, "TOO_MANY_REQUESTS", "Rate limit exceeded",
				"urn:drift:problem:rate-limited", "Retry in 1s.")
			return
		}
		base.ServeHTTP(w, r)
	})

	start := time.Now()
	_, errOut, code := h.run("env", "wait", "proof-alpha", "--for", "running", "--timeout", "30s")
	if code != cliexit.OK {
		t.Fatalf("exit %d, want 0 — a 429 aborted the wait\n%s", code, errOut)
	}
	// It honoured the server's number rather than its own: a full second, not
	// the millisecond poll interval this harness uses.
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("backed off for %s, want at least the 1s the server asked for", elapsed)
	}
	if !strings.Contains(errOut, "rate limited") {
		t.Fatalf("the backoff was not reported: %s", errOut)
	}
}

// --- create -----------------------------------------------------------------

// Off a terminal nothing is inferred, so the required fields must be named.
func TestCreateRequiresExplicitFlagsWhenNotInteractive(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "create")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "--slug") {
		t.Fatalf("the missing field was not named: %s", errOut)
	}
	if !strings.Contains(errOut, "interactive") {
		t.Fatalf("the reason inference did not run was not given: %s", errOut)
	}
}

func TestCreateRejectsAnUnusableSlugBeforeCallingTheServer(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)
	_, errOut, code := h.run("env", "create", "--slug", "Not_A_Slug", "--repo", "widget:topic", "--yes")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if s.seen("create:Not_A_Slug") != 0 {
		t.Fatal("an invalid slug still reached the server")
	}
}

func TestCreateResolvesRepositoryNamesToIds(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)
	for _, name := range []string{"widget", "acme/widget", "Widget"} {
		if _, errOut, code := h.run("env", "create", "--slug", "proof-alpha",
			"--repo", name+":topic", "--yes", "--no-wait"); code != cliexit.OK {
			t.Fatalf("%q did not resolve: exit %d\n%s", name, code, errOut)
		}
	}
	_, errOut, code := h.run("env", "create", "--slug", "proof-alpha",
		"--repo", "nope:topic", "--yes", "--no-wait")
	if code != cliexit.NotFound {
		t.Fatalf("exit %d, want %d", code, cliexit.NotFound)
	}
	// Listed by helm chart key: it is the only one of the four accepted
	// spellings that is unique, since a monorepo has several services under one
	// `owner/name`.
	if !strings.Contains(errOut, "known services: widget") {
		t.Fatalf("the known services were not listed: %s", errOut)
	}
}

// A name that several services share is refused rather than resolved by an
// arbitrary rule, and the alternatives are given as the names that WOULD
// disambiguate.
func TestAmbiguousRepositoryNameIsRefused(t *testing.T) {
	s := newMutServer(t)
	s.extraRepos = []map[string]any{{
		"id": svcID, "owner": "acme", "name": "widget", "fullName": "acme/widget",
		"displayName": "Widget API", "description": nil, "defaultBranch": "main",
		"helmChartKey": "widget-api", "isActive": true,
		"stgUrl": nil, "rcUrl": nil, "prdUrl": nil,
		"applicationGroupId": nil, "applicationGroup": nil,
	}}
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "create", "--slug", "proof-alpha",
		"--repo", "acme/widget:topic", "--yes", "--no-wait")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "widget, widget-api") {
		t.Fatalf("the disambiguating names were not offered: %s", errOut)
	}
}

// A monorepo hosts multiple services that share the same FullName, Name and
// Owner. When one service's HelmChartKey happens to equal the shared Name,
// the old four-way resolver declares it ambiguous and the service becomes
// unnameable. The two-pass resolver fixes this: HelmChartKey is matched first
// and, being unique, wins.
func TestMonorepoResolvesChartKeyFirst(t *testing.T) {
	s := newMutServer(t)
	// Mimic a monorepo: two services from the same repo, one with
	// chart key "binder" and one with chart key "forge" (== the repo Name).
	s.extraRepos = []map[string]any{
		{
			"id": forgeRepoID, "owner": "acme", "name": "forge",
			"fullName": "acme/forge", "displayName": "Forge",
			"description": nil, "defaultBranch": "main",
			"helmChartKey": "forge", "isActive": true,
			"stgUrl": nil, "rcUrl": nil, "prdUrl": nil,
			"applicationGroupId": nil, "applicationGroup": nil,
		},
		{
			"id": binderRepoID, "owner": "acme", "name": "forge",
			"fullName": "acme/forge", "displayName": "Binder",
			"description": nil, "defaultBranch": "main",
			"helmChartKey": "binder", "isActive": true,
			"stgUrl": nil, "rcUrl": nil, "prdUrl": nil,
			"applicationGroupId": nil, "applicationGroup": nil,
		},
	}
	h := newMutHarness(t, s)

	// "forge" is ambiguous in the four-way resolver (matches Forge via
	// HelmChartKey AND Binder via Name) but unambiguous via chart key.
	_, errOut, code := h.run("env", "create", "--slug", "proof-alpha",
		"--repo", "forge:topic", "--yes", "--no-wait")
	if code != cliexit.OK {
		t.Fatalf("forge: exit %d, want %d\n%s", code, cliexit.OK, errOut)
	}
	if s.seen("repo:"+forgeRepoID) != 1 {
		t.Fatalf("forge resolved to the wrong repository; calls: %v", s.calls)
	}

	// "binder" matches only the Binder service's chart key — no ambiguity in
	// either pass.
	_, errOut, code = h.run("env", "create", "--slug", "proof-beta",
		"--repo", "binder:topic", "--yes", "--no-wait")
	if code != cliexit.OK {
		t.Fatalf("binder: exit %d, want %d\n%s", code, cliexit.OK, errOut)
	}
	if s.seen("repo:"+binderRepoID) != 1 {
		t.Fatalf("binder resolved to the wrong repository; calls: %v", s.calls)
	}

	// A spelling that matches multiple services and is NOT a chart key is
	// still refused. "acme/forge" is the FullName for both rows.
	_, errOut, code = h.run("env", "create", "--slug", "proof-gamma",
		"--repo", "acme/forge:topic", "--yes", "--no-wait")
	if code != cliexit.Usage {
		t.Fatalf("acme/forge: exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "binder") || !strings.Contains(errOut, "forge") {
		t.Fatalf("the disambiguating names were not offered: %s", errOut)
	}
}

// --- per-repo PR parsing and guards -----------------------------------------

func TestSplitRepoBranchPR(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		wantRepo   string
		wantBranch string
		wantPR     int
		wantErr    bool
	}{
		{"plain", "widget:topic", "widget", "topic", 0, false},
		{"with pr", "widget:topic:42", "widget", "topic", 42, false},
		{"branch with slash", "widget:feature/foo:99", "widget", "feature/foo", 99, false},
		{"no pr segment", "nodus:sr/migration-check", "nodus", "sr/migration-check", 0, false},
		{"trailing colon", "widget:topic:", "", "", 0, true},
		{"non-numeric pr", "widget:topic:abc", "", "", 0, true},
		{"zero pr", "widget:topic:0", "", "", 0, true},
		{"negative pr", "widget:topic:-1", "", "", 0, true},
		{"empty branch with pr", "widget::42", "", "", 0, true},
		{"no colon at all", "widget", "", "", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, branch, pr, err := splitRepoBranchPR(c.input)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", c.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", c.input, err)
			}
			if repo != c.wantRepo || branch != c.wantBranch || pr != c.wantPR {
				t.Fatalf("splitRepoBranchPR(%q) = (%q, %q, %d), want (%q, %q, %d)",
					c.input, repo, branch, pr, c.wantRepo, c.wantBranch, c.wantPR)
			}
		})
	}
}

// --pr with 2+ repos is a usage error.
func TestPRFlagWithMultipleReposIsUsageError(t *testing.T) {
	s := newMutServer(t)
	s.extraRepos = []map[string]any{{
		"id": svcID, "owner": "acme", "name": "gadget", "fullName": "acme/gadget",
		"displayName": "Gadget", "description": nil, "defaultBranch": "main",
		"helmChartKey": "gadget", "isActive": true,
		"stgUrl": nil, "rcUrl": nil, "prdUrl": nil,
		"applicationGroupId": nil, "applicationGroup": nil,
	}}
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "create", "--slug", "proof-alpha",
		"--repo", "widget:topic", "--repo", "gadget:topic",
		"--pr", "42", "--yes", "--no-wait")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "name:branch:pr") {
		t.Fatalf("the usage error does not suggest the alternative syntax: %s", errOut)
	}
}

// --pr conflicts with a :pr segment on the same single repo.
func TestPRFlagConflictsWithSegment(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "create", "--slug", "proof-alpha",
		"--repo", "widget:topic:99", "--pr", "42", "--yes", "--no-wait")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "conflicts") {
		t.Fatalf("the error does not say 'conflicts': %s", errOut)
	}
}

// Per-repo PR stamping: each repo gets only its own PR; absent repos get nil.
func TestPerRepoPRStamping(t *testing.T) {
	s := newMutServer(t)
	s.extraRepos = []map[string]any{{
		"id": svcID, "owner": "acme", "name": "gadget", "fullName": "acme/gadget",
		"displayName": "Gadget", "description": nil, "defaultBranch": "main",
		"helmChartKey": "gadget", "isActive": true,
		"stgUrl": nil, "rcUrl": nil, "prdUrl": nil,
		"applicationGroupId": nil, "applicationGroup": nil,
	}}

	// Record the full body the CLI sends.
	var sentBody map[string]any
	base := s.Config.Handler
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/environments" {
			body, _ := readJSON(r)
			sentBody = body
			// Recreate the body reader for the underlying handler.
			encoded, _ := json.Marshal(body)
			r.Body = io.NopCloser(bytes.NewReader(encoded))
		}
		base.ServeHTTP(w, r)
	})

	h := newMutHarness(t, s)
	_, errOut, code := h.run("env", "create", "--slug", "proof-alpha",
		"--repo", "widget:topic:1088",
		"--repo", "gadget:feature/x",
		"--yes", "--no-wait")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}

	repos, ok := sentBody["repos"].([]any)
	if !ok || len(repos) != 2 {
		t.Fatalf("expected 2 repos in body, got: %v", sentBody["repos"])
	}

	// First repo (widget) should have prNumber = 1088.
	r0 := repos[0].(map[string]any)
	if r0["prNumber"] == nil {
		t.Fatal("widget's prNumber should be 1088, got nil")
	}
	if int(r0["prNumber"].(float64)) != 1088 {
		t.Fatalf("widget prNumber = %v, want 1088", r0["prNumber"])
	}

	// Second repo (gadget) should have NO prNumber.
	r1 := repos[1].(map[string]any)
	if r1["prNumber"] != nil {
		t.Fatalf("gadget should have no prNumber, got %v", r1["prNumber"])
	}
}

// An explicit --migration-source against a server that advertises
// environments.write but not environments.migration-source must be refused
// BEFORE any create write, with a feature-unsupported failure. This guards the
// safety boundary where an older server would silently drop the unknown field
// and deploy its own default migration source.
func TestMigrationSourceRefusedOnServerWithoutCapability(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "create", "--slug", "proof-alpha",
		"--repo", "widget:topic", "--migration-source", "orders-api",
		"--yes", "--no-wait")
	if code != cliexit.Error {
		t.Fatalf("exit %d, want %d (feature-unsupported)\n%s", code, cliexit.Error, errOut)
	}
	if s.seen("create:proof-alpha") != 0 {
		t.Fatal("an explicit source reached the server even though it does not advertise the capability")
	}
	if !strings.Contains(errOut, "does not support") {
		t.Fatalf("the feature-unsupported failure was not reported:\n%s", errOut)
	}
}

// An EXPLICIT empty --migration-source is a usage error before any inference,
// Connect or create write. Treating it as omission would let an empty CI
// variable silently select the server's default behind the operator's back,
// which is exactly the explicit-choice-vs-omission boundary.
func TestMigrationSourceExplicitEmptyIsUsageError(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "create", "--slug", "proof-alpha",
		"--repo", "widget:topic", "--migration-source", "",
		"--yes", "--no-wait")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d (usage)\n%s", code, cliexit.Usage, errOut)
	}
	if s.seen("create:proof-alpha") != 0 {
		t.Fatal("an explicitly empty --migration-source reached the server")
	}
	if !strings.Contains(errOut, "--migration-source") {
		t.Fatalf("the usage error does not name the flag:\n%s", errOut)
	}
}

// --pr with a single repo still works.
func TestPRFlagSingleRepoStillWorks(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "create", "--slug", "proof-alpha",
		"--repo", "widget:topic", "--pr", "42",
		"--pr-title", "Test PR", "--pr-url", "https://github.com/acme/widget/pull/42",
		"--yes", "--no-wait")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
}

// --pr-title and --pr-url without --pr are silently ignored today. This test
// pins that behavior so a future change to error there is a deliberate one.
func TestPRTitleAndURLWithoutPRNumberAreIgnored(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "create", "--slug", "proof-alpha",
		"--repo", "widget:topic",
		"--pr-title", "Orphan title", "--pr-url", "https://github.com/acme/widget/pull/99",
		"--yes", "--no-wait")
	if code != cliexit.OK {
		t.Fatalf("exit %d, want 0 — companion flags without --pr should be ignored\n%s", code, errOut)
	}
	// The summary must NOT mention a PR, since --pr was not given.
	if strings.Contains(errOut, "pr #") {
		t.Fatalf("a PR appeared in the summary without --pr: %s", errOut)
	}
}

// Per-repo confirm output: PR is shown on the service line, not as a separate line.
func TestPerRepoPRInSummary(t *testing.T) {
	s := newMutServer(t)
	s.extraRepos = []map[string]any{{
		"id": svcID, "owner": "acme", "name": "gadget", "fullName": "acme/gadget",
		"displayName": "Gadget", "description": nil, "defaultBranch": "main",
		"helmChartKey": "gadget", "isActive": true,
		"stgUrl": nil, "rcUrl": nil, "prdUrl": nil,
		"applicationGroupId": nil, "applicationGroup": nil,
	}}
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "create", "--slug", "proof-alpha",
		"--repo", "widget:topic:1088",
		"--repo", "gadget:feature/x",
		"--yes", "--no-wait")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	// The confirmation summary goes to stderr.
	if !strings.Contains(errOut, "widget @ topic  pr #1088") {
		t.Fatalf("widget's PR not shown on service line: %s", errOut)
	}
	if strings.Contains(errOut, "gadget") && strings.Contains(errOut, "pr #") {
		// gadget should not have a PR on its line.
		for _, line := range strings.Split(errOut, "\n") {
			if strings.Contains(line, "gadget") && strings.Contains(line, "pr #") {
				t.Fatalf("gadget should not have a PR: %s", line)
			}
		}
	}
}

// The inferred marks must sit next to the field they annotate: the repo mark
// after the branch, the PR mark after the PR text. With the marks at the wrong
// positions, "pr #42   (inferred)" looks like the PR was inferred (it was not),
// and two bare "(inferred)" at line end are indistinguishable.
func TestSummaryInferredMarksPlacement(t *testing.T) {
	t.Run("inferred repo + explicit pr", func(t *testing.T) {
		p := &plan{
			Slug: "test",
			Repos: []planRepo{{
				Name:   "widget",
				Branch: "topic",
				PR:     &infer.PullRequest{Number: 42, Title: "Fix it"},
			}},
			Inferred: map[string]bool{"repo": true},
		}
		s := p.summary()
		// The repo mark must appear between the branch and the PR section.
		if !strings.Contains(s, "widget @ topic   (inferred)  pr #42 Fix it") {
			t.Fatalf("mark not adjacent to branch:\n%s", s)
		}
		// There must be exactly ONE (inferred) on the service line.
		for _, line := range strings.Split(s, "\n") {
			if strings.Contains(line, "service") {
				count := strings.Count(line, "(inferred)")
				if count != 1 {
					t.Fatalf("expected 1 (inferred) mark, got %d: %q", count, line)
				}
			}
		}
	})

	t.Run("inferred repo + inferred pr", func(t *testing.T) {
		p := &plan{
			Slug: "test",
			Repos: []planRepo{{
				Name:   "widget",
				Branch: "topic",
				PR:     &infer.PullRequest{Number: 99, Title: "Draft"},
			}},
			Inferred: map[string]bool{"repo": true, "pr": true},
		}
		s := p.summary()
		// Both marks must appear, each adjacent to what it annotates.
		if !strings.Contains(s, "widget @ topic   (inferred)  pr #99 Draft   (inferred)") {
			t.Fatalf("marks not in the right places:\n%s", s)
		}
		// There must be exactly TWO (inferred) on the service line.
		for _, line := range strings.Split(s, "\n") {
			if strings.Contains(line, "service") {
				count := strings.Count(line, "(inferred)")
				if count != 2 {
					t.Fatalf("expected 2 (inferred) marks, got %d: %q", count, line)
				}
			}
		}
	})

	t.Run("explicit repo + explicit pr", func(t *testing.T) {
		p := &plan{
			Slug: "test",
			Repos: []planRepo{{
				Name:   "widget",
				Branch: "topic",
				PR:     &infer.PullRequest{Number: 42},
			}},
			Inferred: map[string]bool{},
		}
		s := p.summary()
		if strings.Contains(s, "(inferred)") {
			t.Fatalf("no marks expected for all-explicit:\n%s", s)
		}
	})
}

// Mutate verbs' syntax is unchanged (splitRepoBranch, not splitRepoBranchPR).
func TestMutateVerbsSyntaxUnchanged(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)

	// add-service with a :pr segment should be treated as branch, not PR.
	// splitRepoBranch splits on first colon only, so "widget:topic:42" gives
	// branch "topic:42" (the whole remainder), which is a valid branch name
	// as far as the CLI is concerned.
	_, _, code := h.run("env", "add-service", "proof-alpha", "widget:topic:42")
	// It should reach the server (branch = "topic:42"). The server might reject
	// it but the CLI does not parse the :42 as a PR.
	if code == cliexit.Usage {
		t.Fatal("add-service should not parse :42 as a PR segment")
	}
}

// --- promotions -------------------------------------------------------------

func TestPromoteRcWaitsForTheMachineToFinish(t *testing.T) {
	s := newMutServer(t)
	s.promotes = []string{"dispatched", "promoting", "deploying", "completed"}
	h := newMutHarness(t, s)

	out, errOut, code := h.run("release", "promote", "rc", "widget", "--yes")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "completed") {
		t.Fatalf("final status not reported: %s", out)
	}
}

// A promotion's failure states are FINAL in its machine, so one observation is
// enough — unlike an environment, where the same word is a recoverable blip.
func TestPromoteFailsImmediatelyOnDeployFailed(t *testing.T) {
	s := newMutServer(t)
	s.promotes = []string{"deploy_failed"}
	h := newMutHarness(t, s)

	_, errOut, code := h.run("release", "promote", "rc", "widget", "--yes")
	if code != cliexit.Conflict {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Conflict, errOut)
	}
}

func TestPromotePrdWaitsForTheMachineToFinish(t *testing.T) {
	s := newMutServer(t)
	s.promotes = []string{"dispatched", "promoting", "deploying", "completed"}
	h := newMutHarness(t, s)

	out, errOut, code := h.run("release", "promote", "prd", "widget", "--yes")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "completed") {
		t.Fatalf("final status not reported: %s", out)
	}
	if s.seen("promote:prd") != 1 {
		t.Fatalf("expected exactly one promote:prd call, got %d", s.seen("promote:prd"))
	}
}

func TestPromotePrdHotfixSuccess(t *testing.T) {
	s := newMutServer(t)
	s.promotes = []string{"dispatched", "completed"}
	h := newMutHarness(t, s)

	out, errOut, code := h.run("release", "promote", "prd", "hotfix", "widget", "--branch", "fix/urgent", "--yes")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "completed") {
		t.Fatalf("final status not reported: %s", out)
	}
	if s.seen("promote:prd:hotfix") != 1 {
		t.Fatalf("expected exactly one promote:prd:hotfix call, got %d", s.seen("promote:prd:hotfix"))
	}
}

func TestPromotePrd403ElevationRequiredExits4(t *testing.T) {
	s := newMutServer(t)
	s.prd403Elevation = true
	h := newMutHarness(t, s)

	_, errOut, code := h.run("release", "promote", "prd", "widget", "--yes")
	if code != cliexit.AuthRequired {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.AuthRequired, errOut)
	}
	for _, want := range []string{"/credentials", "DRIFT_TOKEN"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("the hint is missing %q: %s", want, errOut)
		}
	}
}

func TestPromotePrdPlain403StaysExit1(t *testing.T) {
	// A 403 without the elevation-required type stays a plain Error (exit 1),
	// not AuthRequired. Re-authenticating will not help when the ROLE is wrong.
	srv := newFakeDrift(t, map[string]any{
		"org": "acme", "version": "1.0.0", "auth": "sso",
		"services":               map[string]string{"api.v1": "/api/v1"},
		"features_supported":     []string{"environments.read", "releases.read", "promotions.prd"},
		"minimum_client_version": "0.1.0",
	})
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	// The fake server's generic 403 (for /environments/forbidden) carries
	// type urn:drift:problem:forbidden, not elevation-required.
	_, errOut, code := h.run("env", "get", "forbidden")
	if code != cliexit.Error {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Error, errOut)
	}
}

func TestPromotePrdFeatureGateAbsent(t *testing.T) {
	// When the discovery document does NOT include promotions.prd, the command
	// fails with the standard gate message.
	srv := newFakeDrift(t, map[string]any{
		"org": "acme", "version": "1.0.0", "auth": "sso",
		"services":               map[string]string{"api.v1": "/api/v1"},
		"features_supported":     []string{"environments.read", "releases.read"},
		"minimum_client_version": "0.1.0",
	})
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	_, errOut, code := h.run("release", "promote", "prd", "widget", "--yes")
	if code != cliexit.Error {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Error, errOut)
	}
	if !strings.Contains(errOut, "this server does not support") {
		t.Fatalf("feature gate message missing: %s", errOut)
	}
}

// --- cancel -----------------------------------------------------------------

// cancelProblem makes the cancel endpoint answer with a problem envelope, the
// way the server does for a promotion it will not cancel. Everything else in
// the mux is untouched.
func cancelProblem(t *testing.T, s *mutServer, status int, code, msg, ptype, detail string) {
	t.Helper()
	base := s.Config.Handler
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/releases/promotions/"+promID+"/cancel" {
			s.record("cancel")
			writeProblem(w, status, code, msg, ptype, detail)
			return
		}
		base.ServeHTTP(w, r)
	})
}

// countRequests wraps the mux with a request counter over the paths pred
// selects, so a test can prove what did NOT reach the server — including a
// request the command was supposed to skip rather than one it never had reason
// to make.
func countRequests(s *mutServer, pred func(*http.Request) bool) func() int {
	var mu sync.Mutex
	n := 0
	base := s.Config.Handler
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pred(r) {
			mu.Lock()
			n++
			mu.Unlock()
		}
		base.ServeHTTP(w, r)
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// anyRequest counts everything, for the checks that must precede the first
// request of any kind.
func anyRequest(*http.Request) bool { return true }

// promotionLookups counts reads of the listing the cancel summary is built
// from.
func promotionLookups(r *http.Request) bool {
	return r.URL.Path == "/api/v1/releases/promotions/active"
}

// The happy path: the reason travels in the body, and both statuses the server
// reports — what the promotion was and what it became — reach the operator.
func TestReleaseCancelSendsTheReasonAndReportsBothStatuses(t *testing.T) {
	s := newMutServer(t)
	s.promotes = []string{"promoting"}
	h := newMutHarness(t, s)

	out, errOut, code := h.run("release", "cancel", promID,
		"--reason", "retag workflow was cancelled", "--yes")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if s.seen("cancel") != 1 {
		t.Fatalf("expected exactly one cancel call; calls: %v", s.calls)
	}
	if reason, ok := s.cancelReason(); !ok || reason != "retag workflow was cancelled" {
		t.Fatalf("reason = %q (present: %v), want %q", reason, ok, "retag workflow was cancelled")
	}
	if !strings.Contains(out, "promoting") || !strings.Contains(out, "failed") {
		t.Fatalf("the previous and resulting statuses were not both reported: %s", out)
	}
	// --yes waives the QUESTION, not the disclosure: the summary is printed
	// either way, and names what is about to happen.
	want := "This cancels the rc promotion " + promID +
		" (services: widget; status: promoting). It will be marked failed and cannot be resumed."
	if !strings.Contains(errOut, want) {
		t.Fatalf("the summary was not printed under --yes:\nwant: %s\ngot:  %s", want, errOut)
	}
}

// An omitted --reason sends no field at all: the body is optional in the
// contract, and a `reason: ""` is a different thing from an absent one.
func TestReleaseCancelWithoutAReasonSendsNoReason(t *testing.T) {
	s := newMutServer(t)
	s.promotes = []string{"promoting"}
	h := newMutHarness(t, s)

	if _, errOut, code := h.run("release", "cancel", promID, "--yes"); code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if reason, ok := s.cancelReason(); ok {
		t.Fatalf("a reason was sent without --reason: %q", reason)
	}
}

// The summary names the state the promotion is going to land in, before the
// operator confirms — and does not promise a transition it cannot deliver when
// the promotion has already finished. The `completed` case also exercises the
// lookup's second arm: a finished promotion is in `recent`, not `active`.
func TestReleaseCancelSummaryNamesTheOutcome(t *testing.T) {
	cases := []struct {
		name   string
		status string
		want   string
	}{
		{"deploying fails into deploy_failed", "deploying",
			"It will be marked deploy_failed and cannot be resumed."},
		{"promoting fails into failed", "promoting",
			"It will be marked failed and cannot be resumed."},
		{"a finished promotion is not promised a transition", "completed",
			"It is already completed, which the server will refuse to cancel."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newMutServer(t)
			s.promotes = []string{c.status}
			h := newMutHarness(t, s)

			_, errOut, code := h.run("release", "cancel", promID, "--yes")
			if code != cliexit.OK {
				t.Fatalf("exit %d\n%s", code, errOut)
			}
			if !strings.Contains(errOut, c.want) {
				t.Fatalf("the summary does not say %q: %s", c.want, errOut)
			}
		})
	}
}

// The summary lookup is decoration, and decoration must not fail the command:
// the server is the authority on the id, and on whether it can be cancelled.
func TestReleaseCancelProceedsWhenTheSummaryLookupFails(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)
	base := s.Config.Handler
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/releases/promotions/active" {
			writeProblem(w, 500, "INTERNAL_ERROR", "boom",
				"urn:drift:problem:internal-error", "")
			return
		}
		base.ServeHTTP(w, r)
	})

	_, errOut, code := h.run("release", "cancel", promID, "--yes")
	if code != cliexit.OK {
		t.Fatalf("exit %d — a failed summary lookup failed the cancel\n%s", code, errOut)
	}
	if s.seen("cancel") != 1 {
		t.Fatalf("the cancel was not sent; calls: %v", s.calls)
	}
	if !strings.Contains(errOut, "This cancels promotion "+promID) {
		t.Fatalf("the summary did not fall back to the id: %s", errOut)
	}
}

// An id that is in neither the in-flight nor the recent listing is still
// cancelled: the server decides, and answers 404 if the id is not there.
func TestReleaseCancelProceedsWhenThePromotionIsNotListed(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)
	base := s.Config.Handler
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/releases/promotions/active" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"active": nil, "recent": []any{}})
			return
		}
		base.ServeHTTP(w, r)
	})

	_, errOut, code := h.run("release", "cancel", promID, "--yes")
	if code != cliexit.OK {
		t.Fatalf("exit %d — an id outside the listing was refused client-side\n%s", code, errOut)
	}
	if s.seen("cancel") != 1 {
		t.Fatalf("the cancel was not sent; calls: %v", s.calls)
	}
	if !strings.Contains(errOut, "This cancels promotion "+promID) {
		t.Fatalf("the summary did not fall back to the id: %s", errOut)
	}
}

// A cancel in a script REFUSES rather than cancelling into the void. The
// harness's streams are buffers, which is exactly what a redirect looks like.
func TestReleaseCancelRefusesWithoutYesOffATerminal(t *testing.T) {
	s := newMutServer(t)
	s.promotes = []string{"promoting"}
	h := newMutHarness(t, s)
	lookups := countRequests(s, promotionLookups)

	_, errOut, code := h.run("release", "cancel", promID)
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "--yes") {
		t.Fatalf("the refusal does not name the remedy: %s", errOut)
	}
	if !strings.Contains(errOut, "not interactive") {
		t.Fatalf("the refusal does not say why: %s", errOut)
	}
	if s.seen("cancel") != 0 {
		t.Fatalf("a cancel was sent despite the refusal: %v", s.calls)
	}
	// The refusal is decided locally, so it must not have spent a request on a
	// summary nobody will see — and must not turn an unreachable server into a
	// transport failure in place of the usage error the invocation earned.
	if n := lookups(); n != 0 {
		t.Fatalf("%d promotion lookup(s) were made for a run that cannot confirm", n)
	}
}

// The refusal is decided locally, so it comes BEFORE anything talks to the
// server — including discovery. Against a server that could not cancel anyway,
// or one that is not there at all, a missing --yes must still be the usage
// error that names the remedy rather than a capability or connection failure
// the operator cannot act on.
func TestReleaseCancelRefusesBeforeConnecting(t *testing.T) {
	cases := []struct {
		name string
		// noCancelFeature serves a discovery document without the capability, so
		// a Connect that happened would fail the gate instead of the prompt.
		noCancelFeature bool
	}{
		{"capable server", false},
		{"server without the capability", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newMutServer(t)
			s.noCancelFeature = c.noCancelFeature
			s.refreshDiscoveryDoc()
			h := newMutHarness(t, s)
			requests := countRequests(s, anyRequest)

			_, errOut, code := h.run("release", "cancel", promID)
			if code != cliexit.Usage {
				t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
			}
			if !strings.Contains(errOut, "--yes") {
				t.Fatalf("the refusal does not name the remedy: %s", errOut)
			}
			if n := requests(); n != 0 {
				t.Fatalf("%d request(s) were made before the refusal; calls: %v", n, s.calls)
			}
		})
	}
}

// The server trims the reason before it bounds it (`z.string().trim().min(1)
// .max(500)`), so the CLI must measure and send the trimmed value: padding is
// not part of what the operator meant to record, and it must not push a reason
// over a limit the server would not have applied to it.
func TestReleaseCancelTrimsTheReason(t *testing.T) {
	s := newMutServer(t)
	s.promotes = []string{"promoting"}
	h := newMutHarness(t, s)

	untrimmed := "   " + strings.Repeat("x", 500) + "  "
	if _, errOut, code := h.run("release", "cancel", promID,
		"--reason", untrimmed, "--yes"); code != cliexit.OK {
		t.Fatalf("exit %d — 500 characters wrapped in padding were refused\n%s", code, errOut)
	}
	if got, ok := s.cancelReason(); !ok || got != strings.Repeat("x", 500) {
		t.Fatalf("the body carries %d characters (present %v), want the trimmed 500",
			len(got), ok)
	}

	// Over the bound AFTER trimming is still over the bound.
	s2 := newMutServer(t)
	h2 := newMutHarness(t, s2)
	requests := countRequests(s2, anyRequest)
	_, errOut, code := h2.run("release", "cancel", promID,
		"--reason", " "+strings.Repeat("x", 501)+" ", "--yes")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "--reason") {
		t.Fatalf("the usage error does not name the flag: %s", errOut)
	}
	if n := requests(); n != 0 {
		t.Fatalf("%d request(s) reached the server for an over-long reason", n)
	}
}

// The server trims with JavaScript's `String.prototype.trim`, which is NOT
// Go's `strings.TrimSpace`: NEL (U+0085) is whitespace to Go and not to
// ECMAScript, and the BOM (U+FEFF) is the reverse. The body carries the value
// the SERVER would compute, so each difference is observable here.
func TestReleaseCancelTrimsLikeECMAScript(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   string
	}{
		{"NEL is not padding", "\u0085cancel\u0085", "\u0085cancel\u0085"},
		{"a NEL-only reason is not empty", "\u0085", "\u0085"},
		{"a BOM is padding", "\uFEFFcancel\uFEFF", "cancel"},
		{"a non-breaking space is padding", "\u00A0cancel\u00A0", "cancel"},
		{"an ideographic space is padding", "\u3000cancel\u3000", "cancel"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newMutServer(t)
			s.promotes = []string{"promoting"}
			h := newMutHarness(t, s)

			if _, errOut, code := h.run("release", "cancel", promID,
				"--reason", c.reason, "--yes"); code != cliexit.OK {
				t.Fatalf("exit %d\n%s", code, errOut)
			}
			if got, ok := s.cancelReason(); !ok || got != c.want {
				t.Fatalf("the body carries %q (present %v), want %q", got, ok, c.want)
			}
		})
	}
}

// A promotion that has already reached a terminal state is refused by the
// server, and its message is what the operator has to see.
func TestReleaseCancelConflictIsExitFive(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)
	cancelProblem(t, s, 409, "CONFLICT", "Cannot cancel promotion in completed state",
		"urn:drift:problem:invalid-transition", "the promotion is completed, which does not accept CANCEL")

	_, errOut, code := h.run("release", "cancel", promID, "--yes")
	if code != cliexit.Conflict {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Conflict, errOut)
	}
	if s.seen("cancel") != 1 {
		t.Fatalf("expected exactly one cancel call; calls: %v", s.calls)
	}
	if !strings.Contains(errOut, "Cannot cancel promotion") {
		t.Fatalf("the refusal message was not surfaced: %s", errOut)
	}
}

func TestReleaseCancelNotFoundIsExitThree(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)
	cancelProblem(t, s, 404, "NOT_FOUND", "Promotion not found",
		"urn:drift:problem:not-found", "No promotion with that id.")

	_, errOut, code := h.run("release", "cancel", promID, "--yes")
	if code != cliexit.NotFound {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.NotFound, errOut)
	}
	if !strings.Contains(errOut, "Promotion not found") {
		t.Fatalf("the server's message was not surfaced: %s", errOut)
	}
}

// The id and the reason are both checked client-side, and both BEFORE any
// request: the discovery fetch, the summary lookup and the cancel itself all
// come later, so a malformed invocation costs no round trip at all.
func TestReleaseCancelValidatesBeforeAnyRequest(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"not a uuid", []string{"release", "cancel", "not-a-uuid", "--yes"}, "not a UUID"},
		{"reason of 501 characters", []string{"release", "cancel", promID,
			"--reason", strings.Repeat("x", 501), "--yes"}, "--reason"},
		// 251 astral characters are 251 runes but 502 UTF-16 code units, which is
		// what the server's validator counts. A rune count would pass this one
		// and eat a 400.
		{"reason of 251 emoji", []string{"release", "cancel", promID,
			"--reason", strings.Repeat("\U0001F600", 251), "--yes"}, "--reason"},
		{"explicitly empty reason", []string{"release", "cancel", promID,
			"--reason", "", "--yes"}, "--reason"},
		// A BOM is padding to the server's trim, so a reason of only a BOM is
		// empty to `.min(1)` — refusing it here is the same answer, without the
		// round trip.
		{"reason of only a BOM", []string{"release", "cancel", promID,
			"--reason", "\uFEFF", "--yes"}, "--reason"},
		{"whitespace-only reason", []string{"release", "cancel", promID,
			"--reason", "   ", "--yes"}, "--reason"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newMutServer(t)
			h := newMutHarness(t, s)
			requests := countRequests(s, anyRequest)

			_, errOut, code := h.run(c.args...)
			if code != cliexit.Usage {
				t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
			}
			if !strings.Contains(errOut, c.want) {
				t.Fatalf("the usage error does not mention %q: %s", c.want, errOut)
			}
			if n := requests(); n != 0 {
				t.Fatalf("%d request(s) reached the server for an invalid invocation", n)
			}
		})
	}
}

// Exactly at the bound must go through: the check is the server's own limit,
// not a guess below it. Both spellings of 500 count the same, because the
// server counts UTF-16 code units.
func TestReleaseCancelAcceptsAReasonAtTheBound(t *testing.T) {
	cases := []struct {
		name   string
		reason string
	}{
		{"500 ascii characters", strings.Repeat("x", 500)},
		{"250 emoji, which are 500 code units", strings.Repeat("\U0001F600", 250)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newMutServer(t)
			s.promotes = []string{"promoting"}
			h := newMutHarness(t, s)

			if _, errOut, code := h.run("release", "cancel", promID,
				"--reason", c.reason, "--yes"); code != cliexit.OK {
				t.Fatalf("exit %d\n%s", code, errOut)
			}
			if got, ok := s.cancelReason(); !ok || got != c.reason {
				t.Fatalf("the reason did not survive the round trip (%d bytes, present %v)",
					len(got), ok)
			}
		})
	}
}

// A server that cannot cancel is refused the way every other gated command is,
// and the refusal names the capability rather than the operation.
func TestReleaseCancelIsGatedOnTheAdvertisedFeature(t *testing.T) {
	s := newMutServer(t)
	s.noCancelFeature = true
	s.refreshDiscoveryDoc()
	h := newMutHarness(t, s)

	_, errOut, code := h.run("release", "cancel", promID, "--yes")
	if code != cliexit.Error {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Error, errOut)
	}
	if !strings.Contains(errOut, "promotions.cancel") {
		t.Fatalf("the missing capability was not named: %s", errOut)
	}
	if s.seen("cancel") != 0 {
		t.Fatalf("a cancel was sent to a server that does not advertise the capability: %v", s.calls)
	}
}

func TestHotfixRequiresABranch(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)
	_, errOut, code := h.run("release", "promote", "hotfix", "widget", "--yes")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "--branch") {
		t.Fatalf("the missing flag was not named: %s", errOut)
	}
}

// --- capability gating ------------------------------------------------------

// A server whose discovery document does not advertise the write surface must
// refuse the write commands, naming the deployment and what it does advertise.
// This is the mechanism that catches a server upgraded in one direction only.
func TestWriteCommandsAreGatedOnTheAdvertisedFeature(t *testing.T) {
	srv := newFakeDrift(t, defaultDoc("")) // read-only feature list
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	_, errOut, code := h.run("env", "sleep", "proof-alpha")
	if code != cliexit.Error {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Error, errOut)
	}
	if !strings.Contains(errOut, "environments.write") {
		t.Fatalf("the missing feature was not named: %s", errOut)
	}
	if !strings.Contains(errOut, "environments.read") {
		t.Fatalf("what the server does advertise was not shown: %s", errOut)
	}
}

// --- golden output ----------------------------------------------------------

func TestGoldenOutput(t *testing.T) {
	cases := []struct {
		name string
		args []string
		// statuses scripts the environment status queue, when the case needs one.
		statuses []string
		// promotes scripts the promotion status queue, when the case needs one.
		promotes []string
	}{
		{"env_redeploy_table", []string{"env", "redeploy", "proof-alpha", "--no-wait"}, []string{"deploying"}, nil},
		{"env_sleep_table", []string{"env", "sleep", "proof-alpha"}, []string{"sleeping"}, nil},
		{"env_sleep_json", []string{"env", "sleep", "proof-alpha", "-o", "json"}, []string{"sleeping"}, nil},
		{"env_rm_wide", []string{"env", "rm", "proof-alpha", "--yes", "-o", "wide"}, []string{"destroying"}, nil},
		{"env_extend_table", []string{"env", "extend", "proof-alpha", "--hours", "12"}, []string{"running"}, nil},
		{"env_share_table", []string{"env", "share", "proof-alpha"}, []string{"running"}, nil},
		{"release_status_table", []string{"release", "status"}, nil, nil},
		{"release_status_json", []string{"release", "status", "-o", "json"}, nil, nil},
		{"release_history_table", []string{"release", "history"}, nil, nil},
		{"promote_rc_json", []string{"release", "promote", "rc", "widget", "--yes", "-o", "json"}, nil, nil},
		{"release_cancel_table", []string{"release", "cancel", promID, "--yes"}, nil, []string{"promoting"}},
		{"release_cancel_json", []string{"release", "cancel", promID, "--yes",
			"--reason", "retag workflow was cancelled", "-o", "json"}, nil, []string{"promoting"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newMutServer(t)
			if c.statuses != nil {
				s.statuses = c.statuses
			}
			if c.promotes != nil {
				s.promotes = c.promotes
			}
			h := newMutHarness(t, s)
			out, errOut, code := h.run(c.args...)
			if code != cliexit.OK {
				t.Fatalf("exit %d\n%s", code, errOut)
			}
			checkGolden(t, c.name+".golden", out)
		})
	}
}

// `--yes` waives the QUESTION, not the disclosure.
//
// `env create` acts on fields it worked out for itself, so skipping the summary
// meant creating an environment from guesses the operator never saw —
// contradicting the command's own documentation. The plan is printed either
// way, to stderr, and includes the pull request URL that is actually sent.
func TestYesStillPrintsThePlan(t *testing.T) {
	s := newMutServer(t)
	h := newMutHarness(t, s)

	_, errOut, code := h.run("env", "create", "--slug", "proof-alpha",
		"--repo", "widget:topic", "--ticket", "PROJ-1234", "--ttl", "12",
		"--pr", "4633", "--pr-title", "Grid SQL pushdown",
		"--pr-url", "https://github.com/acme/widget/pull/4633",
		"--yes", "--no-wait")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	checkGolden(t, "create_plan_stderr.golden", errOut)
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run `go test ./cmd -update` to create it)", err)
	}
	if !bytes.Equal([]byte(got), want) {
		t.Fatalf("output does not match %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
