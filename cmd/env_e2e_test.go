package cmd

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/steadfast-ly/drift-cli/internal/cliexit"
)

const (
	e2eRunID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

// e2eServer extends mutServer with an e2e trigger endpoint and a scripted
// audit-log endpoint for the --wait poll path.
type e2eServer struct {
	*mutServer
	mu          sync.Mutex
	auditCalls  int
	auditScript [][]map[string]any // one per poll; last entry repeats
	triggerCode int                // 0 means 200
}

func newE2eServer(t *testing.T) *e2eServer {
	t.Helper()
	base := newMutServer(t)
	s := &e2eServer{mutServer: base}

	// Intercept the e2e trigger and audit-log endpoints.
	originalHandler := base.Config.Handler
	base.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// POST /api/v1/environments/{id}/e2e
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/environments/")
		if r.Method == http.MethodPost && strings.HasSuffix(rest, "/e2e") {
			s.mutServer.record("e2e")
			s.mu.Lock()
			code := s.triggerCode
			s.mu.Unlock()
			if code == 409 {
				writeProblem(w, 409, "CONFLICT", "Cannot trigger e2e: run already active",
					"urn:drift:problem:invalid-transition", "")
				return
			}
			if code == 502 {
				writeProblem(w, 502, "BAD_GATEWAY", "Workflow dispatch failed",
					"urn:drift:problem:external-service", "GitHub Actions dispatch returned 404")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"environmentId": envID,
				"e2eRunId":      e2eRunID,
			})
			return
		}

		// GET /api/v1/audit-log
		if r.URL.Path == "/api/v1/audit-log" && r.Method == http.MethodGet {
			s.mutServer.record("audit-list")
			s.mu.Lock()
			idx := s.auditCalls
			s.auditCalls++
			items := s.nextAuditItems(idx)
			s.mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":      items,
				"pagination": map[string]any{"limit": 5, "offset": 0, "hasMore": false},
			})
			return
		}

		originalHandler.ServeHTTP(w, r)
	})

	return s
}

// nextAuditItems returns the items for the Nth audit poll. Called with lock held.
func (s *e2eServer) nextAuditItems(idx int) []map[string]any {
	if len(s.auditScript) == 0 {
		return []map[string]any{}
	}
	if idx >= len(s.auditScript) {
		return s.auditScript[len(s.auditScript)-1]
	}
	return s.auditScript[idx]
}

func auditEntry(runID, outcome, reason string) map[string]any {
	entry := map[string]any{
		"id":              1,
		"action":          "environment.e2e_completed",
		"actor":           nil,
		"actorUserId":     nil,
		"buildId":         nil,
		"environmentId":   envID,
		"environmentSlug": "proof-alpha",
		"repositoryId":    nil,
		"promotionId":     nil,
		"timestamp":       "2026-09-11T12:00:00Z",
		"details": map[string]any{
			"e2eRunId": runID,
			"outcome":  outcome,
		},
	}
	if reason != "" {
		entry["details"].(map[string]any)["reason"] = reason
	}
	return entry
}

// --- e2e trigger (no wait) ---------------------------------------------------

func TestE2eTriggerSuccess(t *testing.T) {
	s := newE2eServer(t)
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if s.mutServer.seen("e2e") != 1 {
		t.Fatalf("expected one e2e call; calls: %v", s.mutServer.calls)
	}
	if !strings.Contains(out, e2eRunID) {
		t.Fatalf("run id not in output: %s", out)
	}
	if !strings.Contains(out, "proof-alpha") {
		t.Fatalf("slug not in output: %s", out)
	}
}

func TestE2eTriggerSuccessJSON(t *testing.T) {
	s := newE2eServer(t)
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha", "-o", "json")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %s\n%s", err, out)
	}
	if parsed["e2eRunId"] != e2eRunID {
		t.Fatalf("e2eRunId mismatch: %v", parsed["e2eRunId"])
	}
	if parsed["slug"] != "proof-alpha" {
		t.Fatalf("slug mismatch: %v", parsed["slug"])
	}
	if parsed["outcome"] != nil {
		t.Fatalf("outcome should be null without --wait: %v", parsed["outcome"])
	}
}

func TestE2eConflictExits5(t *testing.T) {
	s := newE2eServer(t)
	s.triggerCode = 409
	h := newMutHarness(t, s.mutServer)

	_, errOut, code := h.run("env", "e2e", "proof-alpha")
	if code != cliexit.Conflict {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Conflict, errOut)
	}
	if !strings.Contains(errOut, "run already active") {
		t.Fatalf("conflict message not surfaced: %s", errOut)
	}
}

func TestE2e502ExitsNonZero(t *testing.T) {
	s := newE2eServer(t)
	s.triggerCode = 502
	h := newMutHarness(t, s.mutServer)

	_, errOut, code := h.run("env", "e2e", "proof-alpha")
	if code == cliexit.OK {
		t.Fatalf("expected non-zero exit for 502\n%s", errOut)
	}
	if !strings.Contains(errOut, "dispatch failed") {
		t.Fatalf("502 message not surfaced: %s", errOut)
	}
}

func TestE2eNotFoundExits3(t *testing.T) {
	s := newE2eServer(t)
	h := newMutHarness(t, s.mutServer)

	_, _, code := h.run("env", "e2e", "nonexistent")
	if code != cliexit.NotFound {
		t.Fatalf("exit %d, want %d", code, cliexit.NotFound)
	}
}

// --- --wait happy path -------------------------------------------------------

func TestE2eWaitPassedExitsZero(t *testing.T) {
	s := newE2eServer(t)
	// First poll: empty (run not complete yet). Second poll: entry with passed.
	s.auditScript = [][]map[string]any{
		{},
		{auditEntry(e2eRunID, "passed", "")},
	}
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "5s")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "passed") {
		t.Fatalf("outcome not in output: %s", out)
	}
	if !strings.Contains(out, e2eRunID) {
		t.Fatalf("run id not in output: %s", out)
	}
}

// The edge case: the run completed between trigger and the first poll, so the
// audit entry is already present. Zero empty polls, one match, exit 0.
func TestE2eWaitImmediateCompletionExitsZero(t *testing.T) {
	s := newE2eServer(t)
	s.auditScript = [][]map[string]any{
		{auditEntry(e2eRunID, "passed", "")},
	}
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "5s")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "passed") {
		t.Fatalf("outcome not in output: %s", out)
	}
	// Exactly one audit poll — the entry was there on the first try.
	if n := s.mutServer.seen("audit-list"); n != 1 {
		t.Fatalf("expected 1 audit poll, got %d; calls: %v", n, s.mutServer.calls)
	}
}

// --- --wait failed outcome ---------------------------------------------------

func TestE2eWaitFailedExitsNonZero(t *testing.T) {
	s := newE2eServer(t)
	s.auditScript = [][]map[string]any{
		{},
		{auditEntry(e2eRunID, "failed", "assertion_failure")},
	}
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "5s")
	if code != cliexit.Conflict {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Conflict, errOut)
	}
	if !strings.Contains(out, "failed") {
		t.Fatalf("outcome not in output: %s", out)
	}
	if !strings.Contains(errOut, "assertion_failure") {
		t.Fatalf("reason not in error output: %s", errOut)
	}
}

func TestE2eWaitErrorOutcomeExitsNonZero(t *testing.T) {
	s := newE2eServer(t)
	s.auditScript = [][]map[string]any{
		{auditEntry(e2eRunID, "error", "dispatch_failed")},
	}
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "5s")
	if code != cliexit.Error {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Error, errOut)
	}
	if !strings.Contains(out, "error") {
		t.Fatalf("outcome not in output: %s", out)
	}
	if !strings.Contains(errOut, "dispatch_failed") {
		t.Fatalf("reason not in error output: %s", errOut)
	}
}

// --- --wait timeout ----------------------------------------------------------

func TestE2eWaitTimeoutExits6(t *testing.T) {
	s := newE2eServer(t)
	// No matching audit entry ever appears.
	s.auditScript = [][]map[string]any{{}}
	h := newMutHarness(t, s.mutServer)

	_, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "20ms")
	if code != cliexit.WaitTimeout {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.WaitTimeout, errOut)
	}
	if !strings.Contains(errOut, "timed out") {
		t.Fatalf("timeout message not in output: %s", errOut)
	}
}

// --- default policy: no wait -------------------------------------------------

func TestE2eDefaultIsNoWait(t *testing.T) {
	s := newE2eServer(t)
	h := newMutHarness(t, s.mutServer)

	// Without --wait, the command returns immediately and does not poll the
	// audit log at all.
	out, errOut, code := h.run("env", "e2e", "proof-alpha")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if s.mutServer.seen("audit-list") != 0 {
		t.Fatalf("audit-log was polled despite no --wait; calls: %v", s.mutServer.calls)
	}
	_ = out
}

// --- --wait ignores entries for a different run id ---------------------------

func TestE2eWaitIgnoresUnrelatedAuditEntries(t *testing.T) {
	otherRunID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	s := newE2eServer(t)
	s.auditScript = [][]map[string]any{
		// First poll: an entry for a DIFFERENT run id.
		{auditEntry(otherRunID, "passed", "")},
		// Second poll: the matching entry.
		{
			auditEntry(otherRunID, "passed", ""),
			auditEntry(e2eRunID, "passed", ""),
		},
	}
	h := newMutHarness(t, s.mutServer)

	_, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "5s")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if s.mutServer.seen("audit-list") != 2 {
		t.Fatalf("expected 2 audit polls, got %d; calls: %v",
			s.mutServer.seen("audit-list"), s.mutServer.calls)
	}
}

// --- --wait and --no-wait contradict -----------------------------------------

func TestE2eWaitAndNoWaitContradict(t *testing.T) {
	s := newE2eServer(t)
	h := newMutHarness(t, s.mutServer)
	_, _, code := h.run("env", "e2e", "proof-alpha", "--wait", "--no-wait")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d", code, cliexit.Usage)
	}
}

// --- golden output -----------------------------------------------------------

func TestE2eGoldenOutput(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"env_e2e_table", []string{"env", "e2e", "proof-alpha"}},
		{"env_e2e_json", []string{"env", "e2e", "proof-alpha", "-o", "json"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newE2eServer(t)
			h := newMutHarness(t, s.mutServer)
			out, errOut, code := h.run(c.args...)
			if code != cliexit.OK {
				t.Fatalf("exit %d\n%s", code, errOut)
			}
			checkGolden(t, c.name+".golden", out)
		})
	}
}

// --- wait happy path golden output -------------------------------------------

func TestE2eWaitPassedGolden(t *testing.T) {
	s := newE2eServer(t)
	s.auditScript = [][]map[string]any{
		{auditEntry(e2eRunID, "passed", "")},
	}
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "5s")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	checkGolden(t, "env_e2e_wait_passed_table.golden", out)
}

func TestE2eWaitPassedGoldenJSON(t *testing.T) {
	s := newE2eServer(t)
	s.auditScript = [][]map[string]any{
		{auditEntry(e2eRunID, "passed", "")},
	}
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "5s", "-o", "json")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	checkGolden(t, "env_e2e_wait_passed_json.golden", out)
}

// Prevent the "e2e returns immediately" test from having a false-positive: the
// mutServer's discovery document must have the feature we gate on.
func TestE2eWriteFeatureIsAdvertised(t *testing.T) {
	s := newE2eServer(t)
	h := newMutHarness(t, s.mutServer)
	_, errOut, code := h.run("env", "e2e", "proof-alpha")
	if code != cliexit.OK {
		t.Fatalf("exit %d — environments.write not in discovery doc?\n%s", code, errOut)
	}
}
