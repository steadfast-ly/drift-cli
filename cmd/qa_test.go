package cmd

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steadfast-ly/drift-cli/internal/cliexit"
)

const (
	qaSessionID   = "aaaaaaaa-0000-4000-8000-000000000001"
	qaPRID        = "bbbbbbbb-0000-4000-8000-000000000002"
	qaRepoID      = "cccccccc-0000-4000-8000-000000000003"
	qaEnvID       = "dddddddd-0000-4000-8000-000000000004"
	qaApprovalID  = "eeeeeeee-0000-4000-8000-000000000005"
	qaExemptionID = "ffffffff-0000-4000-8000-000000000006"
)

// qaServer is a fake drift with the QA module enabled: the queue reads, every
// mutation the CLI drives, and the environment read that `--env` resolves
// through. Request bodies are kept per operation so a test asserts what was
// SENT, not what the CLI printed.
type qaServer struct {
	*fakeServer

	mu     sync.Mutex
	doc    []byte
	bodies map[string]map[string]any
	calls  []string
	// staleRevision makes approve answer a 409 whose data.revision is 4.
	staleRevision bool
	// tooLarge makes approve answer 413.
	tooLarge bool
	// rolePoll502 makes approve answer the fresh-authority 502.
	rolePoll502 bool
	// sendConflict makes send answer a 409 state conflict.
	sendConflict bool
	// ambiguousPR adds a second linked PR with another repo and the same
	// number, so a bare repo name matches both.
	ambiguousPR bool
	// prApprovedAtOtherHead and prGateFailure shape the linked PR's card.
	prApprovedAtOtherHead bool
	prGateFailure         bool
	// staleCompletion marks the card's completion non-current.
	staleCompletion bool
	// endReason ends the session with that reason.
	endReason string
	// healthError makes the tracker and sync phase report a failure.
	healthError string
	// envReadFails makes the environments read answer 503.
	envReadFails bool
	// noEnvironment drops the environment id from the session view.
	noEnvironment bool
}

const qaOtherPRID = "bbbbbbbb-0000-4000-8000-000000000099"

// setFeatures rebuilds the served discovery document. Called before the server
// starts serving in newQaServer, and by the gating tests that need a narrower
// capability list.
func (s *qaServer) setFeatures(features []string) {
	doc, _ := json.Marshal(map[string]any{
		"org": "acme", "version": "1.0.0", "auth": "sso",
		"services":               map[string]string{"api.v1": "/api/v1"},
		"features_supported":     features,
		"minimum_client_version": "0.1.0",
	})
	s.mu.Lock()
	s.doc = doc
	s.mu.Unlock()
}

func (s *qaServer) body(op string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[op]
}

func (s *qaServer) seen(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c == op {
			n++
		}
	}
	return n
}

func newQaServer(t *testing.T) *qaServer {
	t.Helper()
	s := &qaServer{bodies: map[string]map[string]any{}}
	s.setFeatures([]string{"environments.read", "qa", "qa.approve"})
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/drift.json", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		doc := s.doc
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	})

	record := func(op string, r *http.Request) {
		body, _ := readJSON(r)
		s.mu.Lock()
		s.calls = append(s.calls, op)
		s.bodies[op] = body
		s.mu.Unlock()
	}

	mux.HandleFunc("/api/v1/qa", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.queueJSON())
	})
	mux.HandleFunc("/api/v1/qa/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/qa/")
		parts := strings.Split(rest, "/")

		switch {
		case rest == "health":
			w.Header().Set("Content-Type", "application/json")
			s.mu.Lock()
			lastErr := any(nil)
			if s.healthError != "" {
				lastErr = s.healthError
			}
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tracker": map[string]any{
					"lastSuccessAt": "2026-10-24T10:00:00Z", "lastError": lastErr,
				},
				"phases": []map[string]any{{
					"phase": "prs", "lastSuccessAt": "2026-10-24T10:00:00Z",
					"lastError": lastErr, "cursor": "page-2", "claimedUntil": nil,
				}},
				"outbox":           map[string]any{"pending": 2, "claimed": 0, "failed": 1},
				"evaluationAlerts": []any{},
			})
		case rest == "exemptions":
			record("exempt", r)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"exemptionId": qaExemptionID, "scope": "ticket",
				"ticketKeys": []string{"DT-1"}, "evaluations": []any{},
				"gateTarget": nil,
			})
		case strings.HasPrefix(rest, "exemptions/"):
			record("unexempt", r)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(qaRevocationJSON())
		case rest == "revoke":
			record("revoke", r)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(qaRevocationJSON())
		case rest == "links":
			record("link", r)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pullRequestId": qaPRID, "ticketKey": "DT-1", "result": "linked",
				"evaluations": []any{},
			})
		case rest == "links/unlink":
			record("unlink", r)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ticketKey": "DT-1", "forgeRepo": "acme/widget", "sha": "abcdef1234567890",
				"gateTarget": map[string]any{
					"forgeRepo": "acme/widget", "sha": "abcdef1234567890",
					"pullRequestId": qaPRID,
				},
				"evaluations": []any{},
			})
		case parts[0] == "tickets" && len(parts) == 2:
			if strings.ToUpper(parts[1]) != "DT-1" {
				writeProblem(w, 404, "NOT_FOUND", "QA ticket not found",
					"urn:drift:problem:not-found", "No Ticket with that key.")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(s.ticketDetailJSON())
		case parts[0] == "tickets" && len(parts) == 3:
			record(parts[2], r)
			s.mu.Lock()
			sendConflict := s.sendConflict
			s.mu.Unlock()
			if parts[2] == "send" && sendConflict {
				writeProblem(w, http.StatusConflict, "CONFLICT",
					"QA ticket is not in the queue", "urn:drift:problem:conflict",
					"The ticket is already in the ready-for-QA status.")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			switch parts[2] {
			case "sessions":
				_ = json.NewEncoder(w).Encode(s.sessionViewJSON())
			case "comments":
				_ = json.NewEncoder(w).Encode(map[string]any{"ticketKey": "DT-1"})
			case "send":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ticketKey": "DT-1",
					"drain": map[string]any{
						"done": 2, "failed": 0, "retried": 1, "cancelled": 0,
						"conflicts": 0, "paused": false,
					},
				})
			case "carry-over":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ticketKey":   "DT-1",
					"carried":     []map[string]any{{"pullRequestId": qaPRID, "sha": "abcdef1"}},
					"evaluations": []any{},
				})
			}
		case parts[0] == "sessions" && len(parts) == 3:
			op := parts[2]
			if op == "close" {
				op = "end"
			}
			record(op, r)
			s.mu.Lock()
			stale, large, rolePoll := s.staleRevision, s.tooLarge, s.rolePoll502
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			switch {
			case op == "approve" && stale:
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"defined": true, "code": "CONFLICT", "status": 409,
					"message": "QA revision is now 4; the dialog showed 3",
					"data": map[string]any{
						"type": "urn:drift:problem:conflict", "revision": 4,
						"detail": "The request was valid but the resource moved on. Re-read it and retry.",
					},
				})
			case op == "approve" && rolePoll:
				writeProblem(w, http.StatusBadGateway,
					"BAD_GATEWAY",
					"Cannot confirm the qa role for this credential",
					"urn:drift:problem:external-service",
					"gatehouse did not answer the grants poll")
			case op == "approve" && large:
				writeProblem(w, 413, "PAYLOAD_TOO_LARGE",
					"The request body exceeds the 10 MB limit",
					"urn:drift:problem:validation", "The request body exceeds the mount's 10 MB limit.")
			case op == "approve":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"approvalId": qaApprovalID, "ticketKey": "DT-1",
					"covered":   []map[string]any{qaVerdictPRJSON()},
					"uncovered": []any{}, "evaluations": []any{},
				})
			case op == "reject":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"rejectionId": qaExemptionID, "ticketKey": "DT-1",
					"covered": []any{}, "uncovered": []map[string]any{qaVerdictPRJSON()},
					"revokedApprovalIds": []any{}, "evaluations": []any{},
				})
			default:
				_ = json.NewEncoder(w).Encode(s.sessionViewJSON())
			}
		default:
			http.NotFound(w, r)
		}
	})

	// --env resolves a slug through the environments read.
	mux.HandleFunc("/api/v1/environments/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		fails := s.envReadFails
		s.mu.Unlock()
		if fails {
			writeProblem(w, http.StatusServiceUnavailable, "UNAVAILABLE",
				"Environments are unavailable", "urn:drift:problem:unavailable",
				"The environments service did not answer.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"environment": map[string]any{
				"id": qaEnvID, "slug": "proof-alpha", "ticketId": "DT-1",
				"namespace": "pr-proof-alpha", "status": "running",
				"expiresAt": "2026-10-28T10:00:00Z", "ttlHours": 48,
				"sleptAt": nil, "isPublic": false,
			},
			"services": []any{}, "builds": []any{},
		})
	})

	s.fakeServer = &fakeServer{Server: httptest.NewServer(mux)}
	t.Cleanup(s.fakeServer.Close)
	return s
}

func qaPRCardJSON() map[string]any {
	return map[string]any{
		"pullRequestId": qaPRID, "repositoryId": qaRepoID,
		"forgeRepo": "acme/widget", "number": 42, "title": "Add the widget",
		"headBranch": "DT-1-widget", "headSha": "abcdef1234567890",
		"reviewState": "approved", "isOpen": true,
		"url": "https://github.com/acme/widget/pull/42",
		"gate": map[string]any{
			"state": "success", "description": "QA passed", "targetUrl": nil,
		},
		"approvedAtHead": false, "approvedAtOtherHead": false,
		"carriedAtHead": false, "mergedBeforeQa": false,
	}
}

func qaCardJSON(key, summary, category string) map[string]any {
	return map[string]any{
		"key": key, "summary": summary, "projectKey": "DT",
		"statusCategory": category, "statusName": "Ready for QA",
		"statusId": "10001", "enteredQueueAt": "2026-10-20T09:00:00Z",
		"url":            "https://jira.example.com/browse/" + key,
		"pullRequests":   []map[string]any{qaPRCardJSON()},
		"activeSessions": []any{},
		"partitions":     []any{},
		"failedWrites":   []any{},
		"verdictEpoch":   2, "inTrackerApprovedUnconfirmed": false,
		"completion": map[string]any{"state": "pending", "epoch": 2, "current": true},
	}
}

// cardJSON applies the server's card-shaped knobs to the base fixture.
func (s *qaServer) cardJSON(key, summary, category string) map[string]any {
	c := qaCardJSON(key, summary, category)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.staleCompletion {
		c["completion"] = map[string]any{"state": "done", "epoch": 2, "current": false}
	}
	prs := c["pullRequests"].([]map[string]any)
	if s.ambiguousPR {
		other := qaPRCardJSON()
		other["pullRequestId"] = qaOtherPRID
		other["forgeRepo"] = "other/widget"
		prs = append(prs, other)
	}
	if s.prApprovedAtOtherHead {
		prs[0]["approvedAtOtherHead"] = true
	}
	if s.prGateFailure {
		prs[0]["gate"] = map[string]any{"state": "failure", "description": "QA failed", "targetUrl": nil}
	}
	c["pullRequests"] = prs
	return c
}

func (s *qaServer) queueJSON() map[string]any {
	return map[string]any{
		"readyForQa": []map[string]any{s.cardJSON("DT-1", "Ship the widget", "ready")},
		"inTesting": []map[string]any{{
			"key": "DT-2", "summary": "Test the gadget", "projectKey": "DT",
			"statusCategory": "testing", "statusName": nil, "statusId": "10002",
			"enteredQueueAt": nil, "url": nil,
			"pullRequests": []any{}, "partitions": []any{}, "failedWrites": []any{},
			"verdictEpoch": 1, "inTrackerApprovedUnconfirmed": false,
			"activeSessions": []map[string]any{{
				"id": qaSessionID, "ticketKey": "DT-2", "state": "open",
				"owner": "qa@example.com", "environmentId": qaEnvID,
				"environmentSlug": "proof-alpha", "startGeneration": 1,
				"currentGeneration": 2, "startedAt": "2026-10-20T10:00:00Z",
				"endedAt": nil, "endReason": nil, "pullRequestIds": []any{},
				"unacknowledgedChange": false,
			}},
		}},
		"recheck":  []any{},
		"upcoming": []map[string]any{s.cardJSON("DT-3", "Awaiting review", "upcoming")},
		"carryOver": []map[string]any{{
			"ticketKey": "DT-0", "summary": "Pre-rollout work", "statusId": "10000",
			"pullRequests": []map[string]any{{
				"pullRequestId": qaPRID, "forgeRepo": "acme/widget", "number": 42,
				"title": "Add the widget", "headSha": "abcdef1234567890",
			}},
		}},
		"recentlyDecided": map[string]any{
			"approvals": []map[string]any{{
				"id": qaApprovalID, "ticketKey": "DT-9", "approver": "qa@example.com",
				"at": "2026-10-21T10:00:00Z", "generation": 3, "retested": true,
				"noRetestReason": nil, "note": nil, "pullRequests": []any{},
				"revokedAt": nil, "revokedBy": nil, "revokeReason": nil,
			}},
			"exemptions": []any{},
		},
	}
}

func qaSessionDetailJSON() map[string]any {
	return map[string]any{
		"id": qaSessionID, "ticketKey": "DT-1", "state": "open",
		"owner": "qa@example.com", "environmentId": qaEnvID,
		"environmentSlug": "proof-alpha", "startGeneration": 2,
		"currentGeneration": 3, "startedAt": "2026-10-20T10:00:00Z",
		"endedAt": nil, "endReason": nil, "pullRequestIds": []any{},
		"unacknowledgedChange": true, "events": []any{},
	}
}

func (s *qaServer) sessionDetailJSON() map[string]any {
	d := qaSessionDetailJSON()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.endReason != "" {
		d["state"] = "ended"
		d["endReason"] = s.endReason
	}
	return d
}

func (s *qaServer) ticketDetailJSON() map[string]any {
	return map[string]any{
		"card":       s.cardJSON("DT-1", "Ship the widget", "ready"),
		"sessions":   []map[string]any{s.sessionDetailJSON()},
		"approvals":  []any{},
		"exemptions": []any{},
		"rejections": []any{},
		"trackerWrites": []map[string]any{{
			"id": qaApprovalID, "kind": "approve", "state": "done", "seq": 1,
			"epoch": 2, "attempts": 1, "lastError": nil, "lastOutcome": "definite",
			"nextAttemptAt": nil, "sentAt": "2026-10-21T10:00:00Z",
			"completedAt": "2026-10-21T10:00:01Z",
		}},
	}
}

func qaSessionViewJSON() map[string]any {
	return map[string]any{
		"session": map[string]any{
			"id": qaSessionID, "ticketKey": "DT-1", "state": "open",
			"owner": "qa@example.com", "ownerUserId": nil, "environmentId": qaEnvID,
			"startGeneration": 2, "currentGeneration": 3,
			"startedAt": "2026-10-20T10:00:00Z", "endedAt": nil, "endReason": nil,
			"pullRequestIds": []any{}, "unacknowledgedChange": false,
		},
		"events": []any{},
	}
}

func qaVerdictPRJSON() map[string]any {
	return map[string]any{
		"pullRequestId": qaPRID, "repositoryId": qaRepoID,
		"forgeRepo": "acme/widget", "number": 42, "title": "Add the widget",
		"headBranch": "DT-1-widget", "headSha": "abcdef1234567890",
	}
}

func qaRevocationJSON() map[string]any {
	return map[string]any{
		"ticketKeys": []string{"DT-1"}, "revokedApprovalIds": []any{},
		"movedToQueue": false, "evaluations": []any{},
		"gateTarget": map[string]any{
			"forgeRepo": "acme/widget", "sha": "abcdef1234567890",
			"pullRequestId": qaPRID,
		},
	}
}

func newQaHarness(t *testing.T, s *qaServer) *harness {
	t.Helper()
	h := newHarness(t)
	h.setup(t, s.fakeServer, goodToken)
	return h
}

func (s *qaServer) sessionViewJSON() map[string]any {
	v := qaSessionViewJSON()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.noEnvironment {
		v["session"].(map[string]any)["environmentId"] = nil
	}
	return v
}

// --- reads ------------------------------------------------------------------

func TestQaListTableAndJSON(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	out, errOut, code := h.run("qa", "list")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for _, want := range []string{"ready", "DT-1", "Ship the widget", "testing", "DT-2", "DT-3", "CARRY-OVER", "DT-0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}

	out, _, code = h.run("qa", "list", "-o", "json")
	if code != cliexit.OK {
		t.Fatalf("exit %d", code)
	}
	var payload struct {
		Items []struct {
			Section    string `json:"section"`
			Key        string `json:"key"`
			Prs        int    `json:"prs"`
			Completion string `json:"completion"`
		} `json:"items"`
		CarryOver []struct {
			Key string `json:"key"`
		} `json:"carry_over"`
		RecentlyDecided struct {
			Approvals []struct {
				Id string `json:"id"`
			} `json:"approvals"`
		} `json:"recently_decided"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("not JSON: %s\n%s", err, out)
	}
	if len(payload.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(payload.Items))
	}
	if payload.Items[0].Section != "ready" || payload.Items[0].Key != "DT-1" || payload.Items[0].Prs != 1 {
		t.Fatalf("first row = %+v", payload.Items[0])
	}
	if payload.Items[0].Completion != "pending" {
		t.Fatalf("completion = %q, want pending", payload.Items[0].Completion)
	}
	if len(payload.CarryOver) != 1 || payload.CarryOver[0].Key != "DT-0" {
		t.Fatalf("carry_over = %+v", payload.CarryOver)
	}
	if len(payload.RecentlyDecided.Approvals) != 1 || payload.RecentlyDecided.Approvals[0].Id != qaApprovalID {
		t.Fatalf("recently_decided = %+v", payload.RecentlyDecided)
	}

	// --json projects onto exactly the named fields.
	out, _, code = h.run("qa", "list", "--json", "key,section")
	if code != cliexit.OK {
		t.Fatalf("exit %d", code)
	}
	var projected struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &projected); err != nil {
		t.Fatalf("not JSON: %s", err)
	}
	if len(projected.Items) == 0 {
		t.Fatal("no items")
	}
	if _, ok := projected.Items[0]["summary"]; ok {
		t.Fatalf("projection leaked an unnamed field: %v", projected.Items[0])
	}
	if projected.Items[0]["key"] != "DT-1" {
		t.Fatalf("key = %v", projected.Items[0]["key"])
	}
}

func TestQaShowRendersDetail(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	out, errOut, code := h.run("qa", "show", "DT-1")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for _, want := range []string{"DT-1", "Ship the widget", "LINKED PRS", "acme/widget", "42", "SESSIONS", qaSessionID} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}

	out, _, code = h.run("qa", "show", "DT-1", "-o", "json")
	if code != cliexit.OK {
		t.Fatalf("exit %d", code)
	}
	var payload struct {
		Key          string `json:"key"`
		VerdictEpoch int    `json:"verdict_epoch"`
		PullRequests []struct {
			Repo   string `json:"repo"`
			Number int    `json:"number"`
		} `json:"pull_requests"`
		Sessions []struct {
			Id    string `json:"id"`
			State string `json:"state"`
		} `json:"sessions"`
		TrackerWrites []struct {
			Kind string `json:"kind"`
		} `json:"tracker_writes"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("not JSON: %s\n%s", err, out)
	}
	if payload.Key != "DT-1" || len(payload.PullRequests) != 1 || payload.PullRequests[0].Number != 42 {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.VerdictEpoch != 2 {
		t.Fatalf("verdict_epoch = %d, want the card's 2", payload.VerdictEpoch)
	}
	if len(payload.Sessions) != 1 || payload.Sessions[0].Id != qaSessionID {
		t.Fatalf("sessions = %+v", payload.Sessions)
	}
	if len(payload.TrackerWrites) != 1 || payload.TrackerWrites[0].Kind != "approve" {
		t.Fatalf("tracker_writes = %+v", payload.TrackerWrites)
	}
}

func TestQaHealthReportsOutbox(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	out, errOut, code := h.run("qa", "health")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for _, want := range []string{"tracker", "phase prs", "outbox", "pending 2", "failed 1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

// --- approve ----------------------------------------------------------------

func TestQaApproveSendsFileAndRevision(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	dir := t.TempDir()
	path := filepath.Join(dir, "evidence.png")
	if err := os.WriteFile(path, []byte("png-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, errOut, code := h.run("qa", "approve", qaSessionID,
		"--revision", "3", "--retested", "--note", "looks good", "--file", path, "--yes")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, qaApprovalID) || !strings.Contains(out, "approve") {
		t.Fatalf("output lacks the approval:\n%s", out)
	}

	body := srv.body("approve")
	if body == nil {
		t.Fatal("no approve request reached the server")
	}
	if got := body["shownGeneration"]; got != float64(3) {
		t.Fatalf("shownGeneration = %v", got)
	}
	if body["retested"] != true {
		t.Fatalf("retested = %v", body["retested"])
	}
	if body["note"] != "looks good" {
		t.Fatalf("note = %v", body["note"])
	}
	if _, ok := body["noRetestReason"]; ok {
		t.Fatalf("noRetestReason sent for a retested approval: %v", body["noRetestReason"])
	}
	files, _ := body["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("files = %v", body["files"])
	}
	file, _ := files[0].(map[string]any)
	if file["filename"] != "evidence.png" {
		t.Fatalf("filename = %v", file["filename"])
	}
	if file["contentType"] != "image/png" {
		t.Fatalf("contentType = %v", file["contentType"])
	}
	if file["data"] != base64.StdEncoding.EncodeToString([]byte("png-bytes")) {
		t.Fatalf("data = %v", file["data"])
	}
}

func TestQaApproveStaleRevisionNamesTheNewOne(t *testing.T) {
	srv := newQaServer(t)
	srv.staleRevision = true
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "approve", qaSessionID,
		"--revision", "3", "--retested", "--yes")
	if code != cliexit.Conflict {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Conflict, errOut)
	}
	for _, want := range []string{"QA revision is now 4", "revision 4 is now current", "drift qa show"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("missing %q:\n%s", want, errOut)
		}
	}
}

func TestQaApproveExplainsPayloadTooLarge(t *testing.T) {
	srv := newQaServer(t)
	srv.tooLarge = true
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "approve", qaSessionID,
		"--revision", "3", "--retested", "--yes")
	if code == cliexit.OK {
		t.Fatal("a 413 must fail")
	}
	if !strings.Contains(errOut, "10 MB") || !strings.Contains(errOut, "7 MiB") {
		t.Fatalf("the 413 is not explained:\n%s", errOut)
	}
}

func TestQaApprovePrintsRolePollErrorVerbatim(t *testing.T) {
	srv := newQaServer(t)
	srv.rolePoll502 = true
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "approve", qaSessionID,
		"--revision", "3", "--retested", "--yes")
	if code != cliexit.Error {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Error, errOut)
	}
	for _, want := range []string{
		"Cannot confirm the qa role for this credential",
		"gatehouse did not answer the grants poll",
	} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("missing %q:\n%s", want, errOut)
		}
	}
}

func TestQaApproveRequiresRetestStatement(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	cases := [][]string{
		{"qa", "approve", qaSessionID, "--revision", "3", "--yes"},
		{"qa", "approve", qaSessionID, "--revision", "3", "--retested", "--no-retest", "why", "--yes"},
		{"qa", "approve", qaSessionID, "--revision", "3", "--no-retest", "  ", "--yes"},
		{"qa", "approve", qaSessionID, "--retested", "--yes"},
		{"qa", "approve", "not-a-uuid", "--revision", "3", "--retested", "--yes"},
	}
	for _, args := range cases {
		if _, _, code := h.run(args...); code != cliexit.Usage {
			t.Fatalf("%v: exit %d, want %d", args, code, cliexit.Usage)
		}
	}
	if n := srv.seen("approve"); n != 0 {
		t.Fatalf("%d approve request(s) sent for a usage error", n)
	}
}

func TestQaRejectRequiresComment(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	if _, _, code := h.run("qa", "reject", qaSessionID, "--revision", "3", "--yes"); code != cliexit.Usage {
		t.Fatalf("exit %d, want %d", code, cliexit.Usage)
	}
	if n := srv.seen("reject"); n != 0 {
		t.Fatalf("%d reject request(s) sent", n)
	}

	out, errOut, code := h.run("qa", "reject", qaSessionID,
		"--revision", "3", "--comment", "broken build", "--yes")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "reject") {
		t.Fatalf("output lacks the action:\n%s", out)
	}
	if got := srv.body("reject")["comment"]; got != "broken build" {
		t.Fatalf("comment = %v", got)
	}
}

// --- attachments ------------------------------------------------------------

func TestQaAttachmentsEnforceServerCaps(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)
	dir := t.TempDir()

	small := filepath.Join(dir, "one.txt")
	if err := os.WriteFile(small, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"qa", "approve", qaSessionID, "--revision", "1", "--retested", "--yes"}
	for range qaMaxAttachments + 1 {
		args = append(args, "--file", small)
	}
	_, errOut, code := h.run(args...)
	if code != cliexit.Usage || !strings.Contains(errOut, "at most 10") {
		t.Fatalf("count cap: exit %d\n%s", code, errOut)
	}

	big := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(big, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, qaMaxAttachmentBytes+1); err != nil {
		t.Fatal(err)
	}
	_, errOut, code = h.run("qa", "approve", qaSessionID,
		"--revision", "1", "--retested", "--file", big, "--yes")
	if code != cliexit.Usage || !strings.Contains(errOut, "7 MiB") {
		t.Fatalf("size cap: exit %d\n%s", code, errOut)
	}

	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, errOut, code = h.run("qa", "approve", qaSessionID,
		"--revision", "1", "--retested", "--file", empty, "--yes")
	if code != cliexit.Usage || !strings.Contains(errOut, "empty") {
		t.Fatalf("empty file: exit %d\n%s", code, errOut)
	}
	if n := srv.seen("approve"); n != 0 {
		t.Fatalf("%d approve request(s) sent despite client-side validation", n)
	}
}

// --- start, links, sessions -------------------------------------------------

func TestQaStartResolvesPRsAndEnvironment(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	out, errOut, code := h.run("qa", "start", "DT-1",
		"--prs", "acme/widget#42", "--env", "proof-alpha", "--yes")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "proof-alpha") || strings.Contains(out, qaEnvID) {
		t.Fatalf("environment is not printed as its slug:\n%s", out)
	}
	body := srv.body("sessions")
	if body == nil {
		t.Fatal("no start request reached the server")
	}
	prs, _ := body["prs"].([]any)
	if len(prs) != 1 || prs[0] != qaPRID {
		t.Fatalf("prs = %v", body["prs"])
	}
	if body["environmentId"] != qaEnvID {
		t.Fatalf("environmentId = %v", body["environmentId"])
	}
	if !strings.Contains(errOut, "acme/widget#42") {
		t.Fatalf("the confirmation summary does not name the PR:\n%s", errOut)
	}
}

func TestQaStartRejectsUnknownPRRef(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "start", "DT-1", "--prs", "acme/widget#99", "--yes")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "no linked PR") || !strings.Contains(errOut, "acme/widget#42") {
		t.Fatalf("the error does not list the linked PRs:\n%s", errOut)
	}
	if n := srv.seen("sessions"); n != 0 {
		t.Fatalf("%d start request(s) sent", n)
	}
}

func TestQaUnlinkResolvesTheLinkedPR(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	out, errOut, code := h.run("qa", "unlink", "DT-1", "widget#42", "--yes")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	body := srv.body("unlink")
	if body["ticketKey"] != "DT-1" || body["pullRequestId"] != qaPRID {
		t.Fatalf("unlink body = %v", body)
	}
	if !strings.Contains(out, "acme/widget") {
		t.Fatalf("output lacks the repo:\n%s", out)
	}
}

func TestQaEndMapsToClose(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	out, errOut, code := h.run("qa", "end", qaSessionID)
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if n := srv.seen("end"); n != 1 {
		t.Fatalf("close was called %d times", n)
	}
	if !strings.Contains(out, "end") || !strings.Contains(out, qaSessionID) {
		t.Fatalf("output lacks the session:\n%s", out)
	}
	if !strings.Contains(out, "proof-alpha") || strings.Contains(out, qaEnvID) {
		t.Fatalf("environment is not printed as its slug:\n%s", out)
	}

	out, errOut, code = h.run("qa", "ack", qaSessionID)
	if code != cliexit.OK {
		t.Fatalf("ack: exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "proof-alpha") || strings.Contains(out, qaEnvID) {
		t.Fatalf("ack does not print the environment slug:\n%s", out)
	}
}

func TestQaEnvironmentLabelsFallBackToTheID(t *testing.T) {
	cases := []struct {
		name          string
		features      []string
		envReadFails  bool
		noEnvironment bool
		want          any
	}{
		{
			name:     "capability absent",
			features: []string{"qa", "qa.approve"},
			want:     qaEnvID,
		},
		{
			name:         "environment read fails",
			features:     []string{"environments.read", "qa", "qa.approve"},
			envReadFails: true,
			want:         qaEnvID,
		},
		{
			name:          "no environment",
			features:      []string{"environments.read", "qa", "qa.approve"},
			noEnvironment: true,
			want:          nil,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			srv := newQaServer(t)
			srv.setFeatures(tt.features)
			srv.envReadFails = tt.envReadFails
			srv.noEnvironment = tt.noEnvironment
			h := newQaHarness(t, srv)

			out, errOut, code := h.run("qa", "end", qaSessionID, "-o", "json")
			if code != cliexit.OK {
				t.Fatalf("exit %d\n%s", code, errOut)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatalf("not JSON: %s", err)
			}
			got, ok := payload["environment"]
			if !ok || got != tt.want {
				t.Fatalf("environment = %v (present %v), want %v", got, ok, tt.want)
			}
		})
	}
}

func TestQaExemptScopes(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	for _, args := range [][]string{
		{"qa", "exempt", "--reason", "no ui change", "--yes"},
		{"qa", "exempt", "--pr", "acme/widget#42", "--ticket", "DT-1", "--reason", "x", "--yes"},
		{"qa", "exempt", "--ticket", "DT-1", "--yes"},
	} {
		if _, _, code := h.run(args...); code != cliexit.Usage {
			t.Fatalf("%v: exit %d, want %d", args, code, cliexit.Usage)
		}
	}
	if n := srv.seen("exempt"); n != 0 {
		t.Fatalf("%d exempt request(s) sent", n)
	}

	_, errOut, code := h.run("qa", "exempt", "--ticket", "DT-1", "--reason", "no ui change", "--yes")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	body := srv.body("exempt")
	if body["scope"] != "ticket" || body["ticketKey"] != "DT-1" || body["reason"] != "no ui change" {
		t.Fatalf("exempt body = %v", body)
	}

	_, errOut, code = h.run("qa", "exempt", "--pr", "acme/widget#42", "--reason", "docs only", "--yes")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	body = srv.body("exempt")
	if body["scope"] != "pr" || body["pullRequestId"] != qaPRID {
		t.Fatalf("exempt body = %v", body)
	}
}

// --- capability gating ------------------------------------------------------

func TestQaGroupGatedOnDiscovery(t *testing.T) {
	srv := newQaServer(t)
	srv.setFeatures([]string{"environments.read"})
	h := newQaHarness(t, srv)

	out, _, _ := h.run("qa", "--help")
	if !strings.Contains(out, "list") {
		t.Fatalf("qa list vanished from --help:\n%s", out)
	}

	_, errOut, code := h.run("qa", "list")
	if code == cliexit.OK {
		t.Fatal("a server without qa must refuse the group")
	}
	if !strings.Contains(errOut, `"qa"`) {
		t.Fatalf("the refusal does not name the feature:\n%s", errOut)
	}
}

func TestQaApproveGatedOnQaApprove(t *testing.T) {
	srv := newQaServer(t)
	srv.setFeatures([]string{"environments.read", "qa"})
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "approve", qaSessionID, "--revision", "1", "--retested", "--yes")
	if code == cliexit.OK {
		t.Fatal("approve must refuse without qa.approve")
	}
	if !strings.Contains(errOut, "qa.approve") {
		t.Fatalf("the refusal does not name the feature:\n%s", errOut)
	}
	if n := srv.seen("approve"); n != 0 {
		t.Fatalf("%d approve request(s) sent", n)
	}
}

// --- confirmation -----------------------------------------------------------

func TestQaMutationsRefuseWithoutYesWhenNotInteractive(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "approve", qaSessionID, "--revision", "1", "--retested")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "--yes") {
		t.Fatalf("the refusal does not name --yes:\n%s", errOut)
	}
	if n := srv.seen("approve"); n != 0 {
		t.Fatalf("%d approve request(s) sent without confirmation", n)
	}
}

func TestQaListUnknownJSONField(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "list", "--json", "key,slugg")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d", code, cliexit.Usage)
	}
	if !strings.Contains(errOut, "unknown field") {
		t.Fatalf("unhelpful error: %s", errOut)
	}
}

// --- review regressions -----------------------------------------------------

func TestQaShowWideRendersVerdictDetail(t *testing.T) {
	srv := newQaServer(t)
	srv.prApprovedAtOtherHead = true
	srv.prGateFailure = true
	srv.endReason = "slept"
	srv.staleCompletion = true
	h := newQaHarness(t, srv)

	out, errOut, code := h.run("qa", "show", "DT-1", "-o", "wide")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for _, want := range []string{"other head", "failure", "slept", "ended", "done (stale)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "0xc0") {
		t.Fatalf("a pointer leaked into the table:\n%s", out)
	}
}

func TestQaListRendersStaleCompletion(t *testing.T) {
	srv := newQaServer(t)
	srv.staleCompletion = true
	h := newQaHarness(t, srv)

	out, _, code := h.run("qa", "list", "-o", "json")
	if code != cliexit.OK {
		t.Fatalf("exit %d", code)
	}
	var payload struct {
		Items []struct {
			Completion string `json:"completion"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("not JSON: %s", err)
	}
	if payload.Items[0].Completion != "done (stale)" {
		t.Fatalf("completion = %q, want done (stale)", payload.Items[0].Completion)
	}
}

func TestQaStartRejectsAmbiguousBareRepoName(t *testing.T) {
	srv := newQaServer(t)
	srv.ambiguousPR = true
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "start", "DT-1", "--prs", "widget#42", "--yes")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "names 2 different PRs") {
		t.Fatalf("the ambiguity is not reported with the candidates:\n%s", errOut)
	}
	first, second := strings.Index(errOut, qaPRID), strings.Index(errOut, qaOtherPRID)
	if first < 0 || second < 0 || first > second {
		t.Fatalf("the candidates are not listed in card order:\n%s", errOut)
	}
	if n := srv.seen("sessions"); n != 0 {
		t.Fatalf("%d start request(s) sent", n)
	}
}

func TestQaPRRefGuards(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	cases := map[string]string{
		"widget":     "want repo#n",
		"#42":        "want repo#n",
		"widget#":    "want repo#n",
		"widget#abc": "invalid PR number",
		"widget#0":   "invalid PR number",
	}
	for ref, want := range cases {
		_, errOut, code := h.run("qa", "link", "DT-1", ref)
		if code != cliexit.Usage {
			t.Fatalf("%q: exit %d, want %d", ref, code, cliexit.Usage)
		}
		if !strings.Contains(errOut, want) {
			t.Fatalf("%q: missing %q:\n%s", ref, want, errOut)
		}
	}
	if n := srv.seen("link"); n != 0 {
		t.Fatalf("%d link request(s) sent for an invalid reference", n)
	}
}

func TestQaCommentSendsBodyAndFile(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	if _, _, code := h.run("qa", "comment", "DT-1"); code != cliexit.Usage {
		t.Fatalf("missing --body: exit %d, want %d", code, cliexit.Usage)
	}
	if n := srv.seen("comments"); n != 0 {
		t.Fatalf("%d comment request(s) sent without a body", n)
	}

	path := filepath.Join(t.TempDir(), "log.txt")
	if err := os.WriteFile(path, []byte("stack trace"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := h.run("qa", "comment", "DT-1", "--body", "looks odd", "--file", path)
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "comment") || !strings.Contains(out, "DT-1") {
		t.Fatalf("output lacks the action or key:\n%s", out)
	}
	body := srv.body("comments")
	if body["body"] != "looks odd" {
		t.Fatalf("body = %v", body["body"])
	}
	files, _ := body["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("files = %v", body["files"])
	}
	file, _ := files[0].(map[string]any)
	if file["filename"] != "log.txt" || file["contentType"] != "text/plain; charset=utf-8" {
		t.Fatalf("file = %v", file)
	}
}

func TestQaSendReportsServerConflict(t *testing.T) {
	srv := newQaServer(t)
	srv.sendConflict = true
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "send", "DT-1", "--yes")
	if code != cliexit.Conflict {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Conflict, errOut)
	}
	if !strings.Contains(errOut, "already in the ready-for-QA status") {
		t.Fatalf("the server detail is missing:\n%s", errOut)
	}
}

func TestQaHealthReportsFailureState(t *testing.T) {
	srv := newQaServer(t)
	srv.healthError = "tracker answered 503"
	h := newQaHarness(t, srv)

	out, errOut, code := h.run("qa", "health")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "fail") || !strings.Contains(out, "tracker answered 503") {
		t.Fatalf("a failing tracker does not surface:\n%s", out)
	}
}

func TestQaAttachmentsBoundaries(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)
	dir := t.TempDir()

	small := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(small, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"qa", "approve", qaSessionID, "--revision", "1", "--retested", "--yes"}
	for range qaMaxAttachments {
		args = append(args, "--file", small)
	}
	_, errOut, code := h.run(args...)
	if code != cliexit.OK {
		t.Fatalf("exactly %d files: exit %d\n%s", qaMaxAttachments, code, errOut)
	}
	files, _ := srv.body("approve")["files"].([]any)
	if len(files) != qaMaxAttachments {
		t.Fatalf("sent %d files, want %d", len(files), qaMaxAttachments)
	}

	exact := filepath.Join(dir, "exact.bin")
	if err := os.WriteFile(exact, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(exact, qaMaxAttachmentBytes); err != nil {
		t.Fatal(err)
	}
	_, errOut, code = h.run("qa", "approve", qaSessionID,
		"--revision", "1", "--retested", "--file", exact, "--yes")
	if code != cliexit.OK {
		t.Fatalf("exactly %d bytes: exit %d\n%s", qaMaxAttachmentBytes, code, errOut)
	}
}

func TestQaAttachmentsTotalBudgetRejected(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	half := filepath.Join(t.TempDir(), "half.bin")
	if err := os.WriteFile(half, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(half, qaMaxAttachmentBytes/2+1); err != nil {
		t.Fatal(err)
	}

	_, errOut, code := h.run("qa", "approve", qaSessionID,
		"--revision", "1", "--retested", "--file", half, "--file", half, "--yes")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "attachments total more than") {
		t.Fatalf("the per-verdict budget is not reported:\n%s", errOut)
	}
	if n := srv.seen("approve"); n != 0 {
		t.Fatalf("%d approve request(s) sent", n)
	}
}

func TestQaAttachmentsOpenAllBeforeSizing(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)
	dir := t.TempDir()

	unreadable := filepath.Join(dir, "unreadable.bin")
	if err := os.WriteFile(unreadable, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })
	if _, err := os.ReadFile(unreadable); err == nil {
		t.Skip("chmod 000 stays readable when privileged, so the open order is unobservable")
	}

	almost := filepath.Join(dir, "almost.bin")
	if err := os.WriteFile(almost, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(almost, qaMaxAttachmentBytes-1); err != nil {
		t.Fatal(err)
	}

	_, errOut, code := h.run("qa", "approve", qaSessionID,
		"--revision", "1", "--retested", "--file", unreadable, "--file", almost, "--yes")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "cannot read --file") {
		t.Fatalf("every path must be opened before the budget is measured:\n%s", errOut)
	}
	if n := srv.seen("approve"); n != 0 {
		t.Fatalf("%d approve request(s) sent", n)
	}
}

func TestQaAttachmentsRejectDirectory(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "approve", qaSessionID,
		"--revision", "1", "--retested", "--file", t.TempDir(), "--yes")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "is a directory") {
		t.Fatalf("unhelpful error:\n%s", errOut)
	}
	if n := srv.seen("approve"); n != 0 {
		t.Fatalf("%d approve request(s) sent", n)
	}
}

func TestQaReadAttachmentRefusesGrowthOnly(t *testing.T) {
	dir := t.TempDir()

	grew := filepath.Join(dir, "grew.bin")
	if err := os.WriteFile(grew, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(grew)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := qaReadAttachment(f, grew, 4); err == nil ||
		!strings.Contains(err.Error(), "changed while it was being read") {
		t.Fatalf("a file longer than its stat size must be refused: %v", err)
	}
	if pos, err := f.Seek(0, io.SeekCurrent); err != nil || pos != 5 {
		t.Fatalf("read %d bytes, want the bounded 5", pos)
	}

	shrunk := filepath.Join(dir, "shrunk.bin")
	if err := os.WriteFile(shrunk, []byte("ab"), 0o644); err != nil {
		t.Fatal(err)
	}
	sf, err := os.Open(shrunk)
	if err != nil {
		t.Fatal(err)
	}
	defer sf.Close()
	data, err := qaReadAttachment(sf, shrunk, 9)
	if err != nil {
		t.Fatalf("a file shorter than its stat size is read as it now is: %v", err)
	}
	if string(data) != "ab" {
		t.Fatalf("data = %q", data)
	}
}
