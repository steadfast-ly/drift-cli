package cmd

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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
	// runScript scripts the GET .../e2e/{runId} body, one entry per poll; the
	// last entry repeats. Empty means "running" forever.
	runScript []map[string]any
	runCalls  int
	// runTimes is the arrival time of each GET .../e2e/{runId}, so a test can
	// assert the poll cadence.
	runTimes []time.Time
	// runCode forces a non-200 on the run-read endpoint (0 means 200).
	runCode int
	// lastBody is the decoded request body of the most recent e2e trigger,
	// so a test can assert exactly which fields the CLI sent.
	lastBody map[string]any
	// pollDelay holds each wait poll (audit-log or run read) for this long
	// before answering, so a test can prove the CLI bounds a poll by
	// --wait-timeout rather than by the server's response time.
	pollDelay time.Duration
	// pollBreak makes each wait poll fail at the transport layer: the handler
	// declares a Content-Length it never writes, so the client sees a read
	// error instead of a response. It pins that a poll failure which is NOT the
	// deadline keeps its existing transport-error mapping.
	pollBreak bool
}

// e2eRunRow is the JSON body of GET .../e2e/{runId}, mirroring the server's
// E2eRun model: forge fields and completedAt are null until the run finishes,
// and testsBranch is omitted when the run used the profile default.
func e2eRunRow(status, testsBranch string) map[string]any {
	row := map[string]any{
		"e2eRunId":        e2eRunID,
		"environmentId":   envID,
		"environmentSlug": "proof-alpha",
		"status":          status,
		"forgeRunId":      nil,
		"forgeRunUrl":     nil,
		"requestedBy":     "alice@example.com",
		"createdAt":       "2026-09-11T12:00:00Z",
		"completedAt":     nil,
	}
	if testsBranch != "" {
		row["testsBranch"] = testsBranch
	}
	switch status {
	case "passed", "failed", "error":
		row["completedAt"] = "2026-09-11T12:05:00Z"
	}
	return row
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
			body, _ := readJSON(r)
			s.mu.Lock()
			s.lastBody = body
			code := s.triggerCode
			s.mu.Unlock()
			if code == 400 {
				writeProblem(w, 400, "VALIDATION_ERROR", "testsBranch must name an existing branch",
					"urn:drift:problem:validation",
					"the branch 'no/such/branch' does not exist in the profile's e2e repository")
				return
			}
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
			// Echo the chosen branch on a non-default run, exactly as the
			// server's omit-when-default response does.
			resp := map[string]any{"environmentId": envID, "e2eRunId": e2eRunID}
			if tb, ok := body["testsBranch"].(string); ok && tb != "" {
				resp["testsBranch"] = tb
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// GET /api/v1/environments/{id}/e2e/{runId}. Only the exact pair the
		// harness hands out is a real read; any other pair gets the same typed
		// 404 the server answers for a run that is unknown or belongs to
		// another environment, so a CLI that sent the wrong ids fails here
		// rather than being silently served a row.
		if r.Method == http.MethodGet && strings.Contains(rest, "/e2e/") {
			s.mutServer.record("e2e-run")
			if s.holdPoll(w, r) {
				return
			}
			if r.URL.Path != "/api/v1/environments/"+envID+"/e2e/"+e2eRunID {
				writeProblem(w, 404, "NOT_FOUND", "E2e run not found",
					"urn:drift:problem:not-found",
					"no e2e run "+e2eRunID+" on this environment")
				return
			}
			s.mu.Lock()
			s.runTimes = append(s.runTimes, time.Now())
			idx := s.runCalls
			s.runCalls++
			code := s.runCode
			body := s.nextRunBody(idx)
			s.mu.Unlock()
			if code == 404 {
				writeProblem(w, 404, "NOT_FOUND", "E2e run not found",
					"urn:drift:problem:not-found",
					"no e2e run "+e2eRunID+" on this environment")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
			return
		}

		// GET /api/v1/audit-log
		if r.URL.Path == "/api/v1/audit-log" && r.Method == http.MethodGet {
			s.mutServer.record("audit-list")
			if s.holdPoll(w, r) {
				return
			}
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

// nextRunBody returns the body for the Nth run-read poll. Called with lock held.
func (s *e2eServer) nextRunBody(idx int) map[string]any {
	if len(s.runScript) == 0 {
		return e2eRunRow("running", "")
	}
	if idx >= len(s.runScript) {
		return s.runScript[len(s.runScript)-1]
	}
	return s.runScript[idx]
}

// runRequestTimes returns the arrival time of each GET .../e2e/{runId}, in
// order, so a test can assert the poll cadence the CLI actually used.
func (s *e2eServer) runRequestTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.runTimes...)
}

// delayPolls makes both wait sources hold each poll for d before answering.
func (s *e2eServer) delayPolls(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pollDelay = d
}

// breakPolls makes both wait sources fail each poll at the transport layer.
func (s *e2eServer) breakPolls() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pollBreak = true
}

// holdPoll applies the scripted poll delay and failure to one wait poll,
// returning true when the poll was answered or abandoned here and the handler
// must not continue.
func (s *e2eServer) holdPoll(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	delay, broken := s.pollDelay, s.pollBreak
	s.mu.Unlock()

	if delay > 0 {
		// Select on the request context so a CLI that abandons the poll at its
		// deadline releases the handler instead of holding the server open for
		// the whole delay (which would stall Close at test end).
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return true
		}
	}
	if broken {
		// A body far shorter than the declared Content-Length: the client gets
		// a read error, not a response.
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
		return true
	}
	return false
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

// auditEntryWithTests is auditEntry with a testsBranch detail, included only
// when non-empty — mirroring the server's omit-when-default audit records.
func auditEntryWithTests(runID, outcome, reason, testsBranch string) map[string]any {
	entry := auditEntry(runID, outcome, reason)
	if testsBranch != "" {
		entry["details"].(map[string]any)["testsBranch"] = testsBranch
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
	// The first poll was in flight, so the audit path reports the same
	// "waiting (…)" progress line the read path does — the parity the
	// read-path test claims.
	if !strings.Contains(errOut, "waiting (") {
		t.Fatalf("the in-flight progress line was not reported: %s", errOut)
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

// --- --wait run-read path (server advertises environments.e2e-read) ----------

// advertiseE2eRead flips the read capability on and refreshes the served
// discovery document, exactly as a server whose profile has an e2e block does.
func advertiseE2eRead(s *e2eServer) {
	s.mutServer.e2eRead = true
	s.mutServer.refreshDiscoveryDoc()
}

// Advertised: the wait reads the run resource and never touches the audit log.
func TestE2eWaitReadsRunWhenAdvertised(t *testing.T) {
	s := newE2eServer(t)
	advertiseE2eRead(s)
	s.runScript = []map[string]any{
		e2eRunRow("running", ""),
		e2eRunRow("passed", ""),
	}
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "5s")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "passed") {
		t.Fatalf("outcome not in output: %s", out)
	}
	if n := s.mutServer.seen("e2e-run"); n != 2 {
		t.Fatalf("expected 2 run reads (running, then passed), got %d; calls: %v", n, s.mutServer.calls)
	}
	if n := s.mutServer.seen("audit-list"); n != 0 {
		t.Fatalf("the audit log was polled despite the advertised read capability: %d calls; calls: %v",
			n, s.mutServer.calls)
	}
	// Progress parity with the audit path: an in-flight poll reports the
	// "waiting (…)" progress line on stderr, so a CI log reads the same
	// whichever source is followed. The full line prefix is asserted — a bare
	// "waiting" also appears in the timeout message, which would make this
	// pass without any progress line being printed at all.
	if !strings.Contains(errOut, "waiting (") {
		t.Fatalf("the in-flight progress line was not reported: %s", errOut)
	}
}

// Not advertised: the audit fallback runs, and the run resource is never read.
func TestE2eWaitFallsBackToAuditWhenReadNotAdvertised(t *testing.T) {
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
	if n := s.mutServer.seen("audit-list"); n != 1 {
		t.Fatalf("expected 1 audit poll, got %d; calls: %v", n, s.mutServer.calls)
	}
	if n := s.mutServer.seen("e2e-run"); n != 0 {
		t.Fatalf("the run was read although the server does not advertise the capability: %d calls; calls: %v",
			n, s.mutServer.calls)
	}
}

// A failed run maps to the same Conflict exit as the audit path. The run row
// carries no reason, so the forge run URL is the Detail when the server has one.
func TestE2eWaitRunFailedExitsConflict(t *testing.T) {
	s := newE2eServer(t)
	advertiseE2eRead(s)
	row := e2eRunRow("failed", "")
	row["forgeRunUrl"] = "https://forge.example.com/acme/widget/actions/runs/42"
	s.runScript = []map[string]any{row}
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "5s")
	if code != cliexit.Conflict {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Conflict, errOut)
	}
	if !strings.Contains(out, "failed") {
		t.Fatalf("outcome not in output: %s", out)
	}
	if !strings.Contains(errOut, "e2e run "+e2eRunID+" failed") {
		t.Fatalf("the failure message was not surfaced: %s", errOut)
	}
	if !strings.Contains(errOut, "forge run: https://forge.example.com/acme/widget/actions/runs/42") {
		t.Fatalf("the forge run URL was not surfaced as detail: %s", errOut)
	}
}

// An errored run maps to the Error exit with the audit path's exact wording.
func TestE2eWaitRunErrorExitsError(t *testing.T) {
	s := newE2eServer(t)
	advertiseE2eRead(s)
	s.runScript = []map[string]any{e2eRunRow("error", "")}
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "5s")
	if code != cliexit.Error {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Error, errOut)
	}
	if !strings.Contains(out, "error") {
		t.Fatalf("outcome not in output: %s", out)
	}
	if !strings.Contains(errOut, `finished with outcome "error"`) {
		t.Fatalf("the error outcome was not surfaced in the audit path's wording: %s", errOut)
	}
}

// dispatched is in flight just like running: the wait keeps polling through it.
func TestE2eWaitRunDispatchedThenPassed(t *testing.T) {
	s := newE2eServer(t)
	advertiseE2eRead(s)
	s.runScript = []map[string]any{
		e2eRunRow("dispatched", ""),
		e2eRunRow("passed", ""),
	}
	h := newMutHarness(t, s.mutServer)

	_, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "5s")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if n := s.mutServer.seen("e2e-run"); n != 2 {
		t.Fatalf("expected 2 run reads, got %d; calls: %v", n, s.mutServer.calls)
	}
}

// The read path times out exactly like the audit path -- exit 6, the same
// "timed out" line -- but with a Hint that points at the RUN. Sending a
// read-path operator to the audit log would send them after `audit-log.read`,
// the very dependency this path exists to avoid.
func TestE2eWaitRunTimeoutExits6(t *testing.T) {
	s := newE2eServer(t)
	advertiseE2eRead(s)
	// The run never leaves "running".
	s.runScript = []map[string]any{e2eRunRow("running", "")}
	h := newMutHarness(t, s.mutServer)

	const timeout = 300 * time.Millisecond
	start := time.Now()
	_, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", timeout.String())
	if code != cliexit.WaitTimeout {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.WaitTimeout, errOut)
	}
	if !strings.Contains(errOut, "timed out") {
		t.Fatalf("timeout message not in output: %s", errOut)
	}
	if !strings.Contains(errOut, "drift api GET /environments/{id}/e2e/{runId}") {
		t.Fatalf("the read path's hint (re-read the run resource with a runnable command) was not reported: %s", errOut)
	}
	if strings.Contains(errOut, "drift audit list") {
		t.Fatalf("the audit-log hint was used on the read path: %s", errOut)
	}
	// The deadline is the one the operator asked for, not a multiple of it: an
	// implementation that doubled it would still exit 6 with the right hint and
	// satisfy every assertion above. The 250ms of slack over the requested
	// timeout absorbs scheduling jitter on a loaded shared CI runner while a
	// doubled deadline (~600ms) still fails.
	if elapsed := time.Since(start); elapsed >= 550*time.Millisecond {
		t.Fatalf("the wait took %s for a %s timeout; the deadline is not the requested one", elapsed, timeout)
	}
}

// The read path polls at the cadence it was configured with, not one of its
// own: it never polls tighter than the interval asked for. Only the LOWER
// bound is asserted. The upper bound is deliberately not asserted: timing
// slack on a loaded shared CI runner would make it flaky, so a doubled
// interval is not caught by this test — the lower bound is what pins that the
// read path honours the configured interval rather than polling tighter.
func TestE2eWaitRunPollsAtConfiguredInterval(t *testing.T) {
	const interval = 50 * time.Millisecond

	s := newE2eServer(t)
	advertiseE2eRead(s)
	s.runScript = []map[string]any{
		e2eRunRow("running", ""),
		e2eRunRow("running", ""),
		e2eRunRow("passed", ""),
	}
	h := newMutHarness(t, s.mutServer)
	h.app.waitInterval = interval

	_, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "5s")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	times := s.runRequestTimes()
	if len(times) != 3 {
		t.Fatalf("expected 3 run reads (running, running, passed), got %d; calls: %v",
			len(times), s.mutServer.calls)
	}
	for i := 1; i < len(times); i++ {
		gap := times[i].Sub(times[i-1])
		if gap < interval {
			t.Fatalf("reads %d and %d were %s apart, tighter than the %s interval", i-1, i, gap, interval)
		}
	}
}

// A 429 on the run read is the server asking for time, not a failed wait: the
// poll honours Retry-After and carries on, exactly as the audit loop does.
func TestE2eWaitRunReadRateLimitBacksOff(t *testing.T) {
	s := newE2eServer(t)
	advertiseE2eRead(s)
	s.runScript = []map[string]any{e2eRunRow("passed", "")}

	reads := 0
	base := s.Config.Handler
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/e2e/") {
			reads++
			if reads == 1 {
				w.Header().Set("Retry-After", "1")
				writeProblem(w, 429, "TOO_MANY_REQUESTS", "Rate limit exceeded",
					"urn:drift:problem:rate-limited", "Retry in 1s.")
				return
			}
		}
		base.ServeHTTP(w, r)
	})

	h := newMutHarness(t, s.mutServer)
	start := time.Now()
	_, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "30s")
	if code != cliexit.OK {
		t.Fatalf("exit %d, want 0 — a 429 aborted the run read\n%s", code, errOut)
	}
	// It honoured the server's number rather than its own: a full second, not
	// the millisecond poll interval this harness uses.
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("backed off for %s, want at least the 1s the server asked for", elapsed)
	}
	if !strings.Contains(errOut, "rate limited") {
		t.Fatalf("the backoff was not reported: %s", errOut)
	}
	// The throttled attempt is not a read: only the successful poll reached
	// the run endpoint.
	if n := s.mutServer.seen("e2e-run"); n != 1 {
		t.Fatalf("expected 1 successful run read, got %d; calls: %v", n, s.mutServer.calls)
	}
}

// A 404 mid-wait is the typed not-found failure, not a silent retry: the run
// is gone (or belongs to another environment) and retrying would spin until
// the deadline.
func TestE2eWaitRunNotFoundIsTypedFailure(t *testing.T) {
	s := newE2eServer(t)
	advertiseE2eRead(s)
	s.runCode = 404
	h := newMutHarness(t, s.mutServer)

	_, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "5s")
	if code != cliexit.NotFound {
		t.Fatalf("exit %d, want %d (not found)\n%s", code, cliexit.NotFound, errOut)
	}
	if !strings.Contains(errOut, "not found") {
		t.Fatalf("the typed not-found message was not surfaced: %s", errOut)
	}
	if n := s.mutServer.seen("e2e-run"); n != 1 {
		t.Fatalf("a 404 was retried: %d reads; calls: %v", n, s.mutServer.calls)
	}
	if n := s.mutServer.seen("audit-list"); n != 0 {
		t.Fatalf("the audit fallback ran despite the advertised read capability; calls: %v", s.mutServer.calls)
	}
}

// The run row is the branch authority on the read path: it wins over the
// trigger echo, exactly as the completed audit entry does on the fallback.
func TestE2eWaitRunSurfacesTestsBranchOverEcho(t *testing.T) {
	s := newE2eServer(t)
	advertiseE2eRead(s)
	s.mutServer.e2eTestsBranch = true
	s.mutServer.refreshDiscoveryDoc()
	s.runScript = []map[string]any{e2eRunRow("passed", "feat/tests-b")}
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha", "--tests-branch", "feat/tests-a",
		"--wait", "--wait-timeout", "5s")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "feat/tests-b") {
		t.Fatalf("the run's testsBranch was not surfaced over the trigger echo:\n%s", out)
	}
	if strings.Contains(out, "feat/tests-a") {
		t.Fatalf("the trigger echo won over the run row:\n%s", out)
	}
}

// Off-spec server: the run row omits testsBranch. The trigger echo fills in, so
// the output still carries the requested branch.
func TestE2eWaitRunFallsBackToEchoWhenRowOmitsBranch(t *testing.T) {
	s := newE2eServer(t)
	advertiseE2eRead(s)
	s.mutServer.e2eTestsBranch = true
	s.mutServer.refreshDiscoveryDoc()
	s.runScript = []map[string]any{e2eRunRow("passed", "")}
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha", "--tests-branch", "feat/tests-a",
		"--wait", "--wait-timeout", "5s")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "feat/tests-a") {
		t.Fatalf("testsBranch not filled in from the trigger echo:\n%s", out)
	}
}

// The read path renders the SAME output as the audit path for the same
// outcome — same columns, same values, no new field. Reusing the audit path's
// golden is the assertion, so a divergence in either rendering fails here.
func TestE2eWaitRunPassedGoldenMatchesAuditPath(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"env_e2e_wait_passed_table.golden", []string{"env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "5s"}},
		{"env_e2e_wait_passed_json.golden", []string{"env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "5s", "-o", "json"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newE2eServer(t)
			advertiseE2eRead(s)
			s.runScript = []map[string]any{e2eRunRow("passed", "")}
			h := newMutHarness(t, s.mutServer)

			out, errOut, code := h.run(c.args...)
			if code != cliexit.OK {
				t.Fatalf("exit %d\n%s", code, errOut)
			}
			checkGolden(t, c.name, out)
		})
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
		// advertise models a server whose profile's e2e block is enabled
		// (the e2e-tests-branch capability is advertised).
		advertise bool
	}{
		{"env_e2e_table", []string{"env", "e2e", "proof-alpha"}, false},
		{"env_e2e_json", []string{"env", "e2e", "proof-alpha", "-o", "json"}, false},
		{"env_e2e_testsbranch_table", []string{"env", "e2e", "proof-alpha", "--tests-branch", "feature/tests"}, true},
		{"env_e2e_testsbranch_json", []string{"env", "e2e", "proof-alpha", "--tests-branch", "feature/tests", "-o", "json"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newE2eServer(t)
			if c.advertise {
				s.mutServer.e2eTestsBranch = true
				s.mutServer.refreshDiscoveryDoc()
			}
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

// --- --tests-branch ----------------------------------------------------------

// Flag unset: no testsBranch in the request body, and — because the plain
// mutServer does not advertise the e2e-tests-branch capability — no capability
// is required. This is the pre-feature request, byte-identical in effect.
func TestE2eTestsBranchUnsetSendsNoBranch(t *testing.T) {
	s := newE2eServer(t)
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if s.mutServer.seen("e2e") != 1 {
		t.Fatalf("expected one e2e call; calls: %v", s.mutServer.calls)
	}
	s.mu.Lock()
	body := s.lastBody
	s.mu.Unlock()
	if _, ok := body["testsBranch"]; ok {
		t.Fatalf("flag unset sent testsBranch in the body: %v", body)
	}
	_ = out
}

// Flag set against a server that advertises the capability: the request body
// carries the branch, and the trigger output shows the echoed value.
func TestE2eTestsBranchCarriesBranchAndEchoes(t *testing.T) {
	s := newE2eServer(t)
	s.mutServer.e2eTestsBranch = true
	s.mutServer.refreshDiscoveryDoc()
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha", "--tests-branch", "feature/tests")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	s.mu.Lock()
	body := s.lastBody
	s.mu.Unlock()
	if body["testsBranch"] != "feature/tests" {
		t.Fatalf("request body does not carry the branch: %v", body)
	}
	if !strings.Contains(out, "feature/tests") {
		t.Fatalf("echoed branch not in output:\n%s", out)
	}
}

// An EXPLICIT empty --tests-branch is a usage error before any Connect or
// trigger write. Treating it as omission would let an empty CI variable
// silently select the server's default behind the operator's back, which is
// exactly the explicit-choice-vs-omission boundary.
func TestE2eTestsBranchExplicitEmptyIsUsageError(t *testing.T) {
	cases := []struct {
		name string
		val  string
	}{
		{"empty", ""},
		{"whitespace-only", "   "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newE2eServer(t)
			h := newMutHarness(t, s.mutServer)

			_, errOut, code := h.run("env", "e2e", "proof-alpha", "--tests-branch", c.val)
			if code != cliexit.Usage {
				t.Fatalf("exit %d, want %d (usage)\n%s", code, cliexit.Usage, errOut)
			}
			if len(s.mutServer.calls) != 0 {
				t.Fatalf("an explicitly empty --tests-branch reached the server: %v", s.mutServer.calls)
			}
			if !strings.Contains(errOut, "--tests-branch") {
				t.Fatalf("the usage error does not name the flag:\n%s", errOut)
			}
		})
	}
}

// Flag set against a server WITHOUT the capability must be refused BEFORE any
// trigger — an old server would silently run default tests — with the same
// feature-unsupported failure as --migration-source.
func TestE2eTestsBranchRefusedOnServerWithoutCapability(t *testing.T) {
	s := newE2eServer(t)
	h := newMutHarness(t, s.mutServer)

	_, errOut, code := h.run("env", "e2e", "proof-alpha", "--tests-branch", "feature/tests")
	if code != cliexit.Error {
		t.Fatalf("exit %d, want %d (feature-unsupported)\n%s", code, cliexit.Error, errOut)
	}
	if len(s.mutServer.calls) != 0 {
		t.Fatalf("a request reached the server despite the refusal: %v", s.mutServer.calls)
	}
	if s.mutServer.seen("e2e") != 0 {
		t.Fatal("an explicit tests branch reached the trigger even though the server does not advertise the capability")
	}
	if !strings.Contains(errOut, "does not support") {
		t.Fatalf("the feature-unsupported failure was not reported:\n%s", errOut)
	}
	if !strings.Contains(errOut, "e2e-tests-branch") {
		t.Fatalf("the refusal does not name the capability:\n%s", errOut)
	}
}

// A nonexistent branch is the server's ValidationError to make: the CLI sends
// the value verbatim and maps the 400 through the standard validation exit
// path (exit 2). No client-side branch checking.
func TestE2eTestsBranchValidationErrorExitsUsage(t *testing.T) {
	s := newE2eServer(t)
	s.mutServer.e2eTestsBranch = true
	s.mutServer.refreshDiscoveryDoc()
	s.triggerCode = 400
	h := newMutHarness(t, s.mutServer)

	_, errOut, code := h.run("env", "e2e", "proof-alpha", "--tests-branch", "no/such/branch")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d (server validation)\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "existing branch") {
		t.Fatalf("the server's validation message was not surfaced:\n%s", errOut)
	}
}

// --wait surfaces the branch the server recorded from the completed audit
// entry's details map, not from the trigger echo. The audit value differs
// from the trigger echo so the assertion proves the audit detail won over
// the echo fallback.
func TestE2eWaitSurfacesTestsBranchFromAuditDetails(t *testing.T) {
	s := newE2eServer(t)
	s.mutServer.e2eTestsBranch = true
	s.mutServer.refreshDiscoveryDoc()
	s.auditScript = [][]map[string]any{
		{auditEntryWithTests(e2eRunID, "passed", "", "feat/tests-b")},
	}
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha", "--tests-branch", "feat/tests-a",
		"--wait", "--wait-timeout", "5s")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "feat/tests-b") {
		t.Fatalf("audit testsBranch not surfaced over the trigger echo:\n%s", out)
	}
	if strings.Contains(out, "feat/tests-a") {
		t.Fatalf("the trigger echo won over the audit detail:\n%s", out)
	}
}

// Off-spec server: the completed audit entry's details omit the testsBranch.
// The trigger echo fills in, so the output still carries the requested branch.
func TestE2eWaitFallsBackToEchoWhenAuditOmitsBranch(t *testing.T) {
	s := newE2eServer(t)
	s.mutServer.e2eTestsBranch = true
	s.mutServer.refreshDiscoveryDoc()
	s.auditScript = [][]map[string]any{
		{auditEntry(e2eRunID, "passed", "")}, // no testsBranch detail
	}
	h := newMutHarness(t, s.mutServer)

	out, errOut, code := h.run("env", "e2e", "proof-alpha", "--tests-branch", "feat/tests-a",
		"--wait", "--wait-timeout", "5s")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "feat/tests-a") {
		t.Fatalf("testsBranch not filled in from the trigger echo:\n%s", out)
	}
}

// --- --wait bounds each poll by the deadline ---------------------------------

// The audit-log path: a poll still in flight when --wait-timeout expires must
// be cut short BY the deadline. The handler answers a TERMINAL `passed` after
// five seconds, so with a 300ms --wait-timeout code that bounds the poll only
// by the per-request --timeout reports that verdict (exit 0) five seconds in —
// failing both assertions below.
func TestE2eWaitAuditPollIsBoundedByTheDeadline(t *testing.T) {
	s := newE2eServer(t)
	s.delayPolls(5 * time.Second)
	s.auditScript = [][]map[string]any{{auditEntry(e2eRunID, "passed", "")}}
	h := newMutHarness(t, s.mutServer)

	begin := time.Now()
	_, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "300ms")
	elapsed := time.Since(begin)
	if code != cliexit.WaitTimeout {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.WaitTimeout, errOut)
	}
	if !strings.Contains(errOut, e2eAuditTimeoutHint) {
		t.Fatalf("the audit path's timeout hint was not printed: %s", errOut)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("the poll ran %s past a 300ms --wait-timeout", elapsed)
	}
}

// The run-read path: the same claim, with the same terminal verdict — a
// `passed` run row — waiting behind the slow response.
func TestE2eWaitRunPollIsBoundedByTheDeadline(t *testing.T) {
	s := newE2eServer(t)
	advertiseE2eRead(s)
	s.delayPolls(5 * time.Second)
	s.runScript = []map[string]any{e2eRunRow("passed", "")}
	h := newMutHarness(t, s.mutServer)

	begin := time.Now()
	_, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "300ms")
	elapsed := time.Since(begin)
	if code != cliexit.WaitTimeout {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.WaitTimeout, errOut)
	}
	if !strings.Contains(errOut, e2eRunTimeoutHint) {
		t.Fatalf("the read path's timeout hint was not printed: %s", errOut)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("the poll ran %s past a 300ms --wait-timeout", elapsed)
	}
}

// A poll that fails for a reason OTHER than the deadline keeps its existing
// mapping: the transport error, not exit 6. Both sources are covered because
// each has its own error branch.
func TestE2eWaitPollFailureThatIsNotTheDeadlineStaysATransportError(t *testing.T) {
	cases := []struct {
		name string
		read bool
	}{
		{"audit-log", false},
		{"run-read", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newE2eServer(t)
			if c.read {
				advertiseE2eRead(s)
			}
			s.breakPolls()
			h := newMutHarness(t, s.mutServer)

			_, errOut, code := h.run("env", "e2e", "proof-alpha", "--wait", "--wait-timeout", "5s")
			if code != cliexit.Error {
				t.Fatalf("exit %d, want %d\n%s", code, cliexit.Error, errOut)
			}
			if !strings.Contains(errOut, "cannot reach") {
				t.Fatalf("the poll failure was not surfaced as a transport error: %s", errOut)
			}
			if strings.Contains(errOut, "timed out") {
				t.Fatalf("a non-deadline poll failure was reported as a wait timeout: %s", errOut)
			}
		})
	}
}
