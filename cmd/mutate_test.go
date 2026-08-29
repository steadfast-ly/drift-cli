package cmd

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steadfast/drift-cli/internal/auth"
	"github.com/steadfast/drift-cli/internal/cliexit"
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
	// rateLimit fires a 429 on the Nth matching mutation, once.
	rateLimitAfter int
	retryAfter     int
	mutations      int
	// extraRepos are appended to the repository list, for the ambiguity case.
	extraRepos []map[string]any
	// prd403Elevation causes the prd promote endpoint to return a 403 with the
	// elevation-required problem type.
	prd403Elevation bool
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
	mux := http.NewServeMux()

	doc, _ := json.Marshal(map[string]any{
		"org": "acme", "version": "1.0.0", "auth": "sso",
		"services": map[string]string{"api.v1": "/api/v1"},
		"features_supported": []string{
			"environments.read", "environments.write", "repositories.read",
			"releases.read", "promotions.rc", "promotions.hotfix", "promotions.prd",
		},
		"minimum_client_version": "0.1.0",
	})
	mux.HandleFunc("/.well-known/drift.json", func(w http.ResponseWriter, _ *http.Request) {
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
					"id": envID, "slug": "proof-alpha", "ticketId": "AUS-10001",
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
	// Nothing reached the server.
	if n := s.seen("DELETE") + s.seen("relaunch") + s.seen("promote:rc"); n != 0 {
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
	for _, want := range []string{"/credentials", "drift auth login"} {
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
		// promotes scripts the promotion status queue, when the case needs one.
		statuses []string
	}{
		{"env_sleep_table", []string{"env", "sleep", "proof-alpha"}, []string{"sleeping"}},
		{"env_sleep_json", []string{"env", "sleep", "proof-alpha", "-o", "json"}, []string{"sleeping"}},
		{"env_rm_wide", []string{"env", "rm", "proof-alpha", "--yes", "-o", "wide"}, []string{"destroying"}},
		{"env_extend_table", []string{"env", "extend", "proof-alpha", "--hours", "12"}, []string{"running"}},
		{"env_share_table", []string{"env", "share", "proof-alpha"}, []string{"running"}},
		{"release_status_table", []string{"release", "status"}, nil},
		{"release_status_json", []string{"release", "status", "-o", "json"}, nil},
		{"release_history_table", []string{"release", "history"}, nil},
		{"promote_rc_json", []string{"release", "promote", "rc", "widget", "--yes", "-o", "json"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newMutServer(t)
			if c.statuses != nil {
				s.statuses = c.statuses
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
		"--repo", "widget:topic", "--ticket", "AUS-10151", "--ttl", "12",
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
