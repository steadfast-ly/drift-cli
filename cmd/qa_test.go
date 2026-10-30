package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steadfast-ly/drift-cli/internal/api"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
	"github.com/steadfast-ly/drift-cli/internal/output"
)

const (
	qaSessionID   = "aaaaaaaa-0000-4000-8000-000000000001"
	qaPRID        = "bbbbbbbb-0000-4000-8000-000000000002"
	qaRepoID      = "cccccccc-0000-4000-8000-000000000003"
	qaEnvID       = "dddddddd-0000-4000-8000-000000000004"
	qaApprovalID  = "eeeeeeee-0000-4000-8000-000000000005"
	qaExemptionID = "ffffffff-0000-4000-8000-000000000006"
	// The second row of a monorepo exemption holds its own exemption.
	qaExemptionID2 = "ffffffff-0000-4000-8000-000000000010"
	qaEventID      = "11111111-0000-4000-8000-000000000007"
	// A monorepo PR: the same forge repo#n discovered under a second
	// registered Repository, so the card carries one row per registration.
	qaRepoID2 = "cccccccc-0000-4000-8000-000000000008"
	qaPRID2   = "bbbbbbbb-0000-4000-8000-000000000009"
	qaRepoID3 = "cccccccc-0000-4000-8000-000000000011"
	qaPRID3   = "bbbbbbbb-0000-4000-8000-000000000012"
	// The No ticket section's representative row, on a PR no Ticket links.
	qaNoTicketPRID = "bbbbbbbb-0000-4000-8000-000000000013"
	// The second registration of that PR, named alongside the first in the
	// item's blockedPullRequestIds.
	qaNoTicketPRID2 = "bbbbbbbb-0000-4000-8000-000000000014"
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
	// events is the session's timeline, in the wire's shape: an event's
	// before/after are ARRAYS of deployment services (or null).
	events []map[string]any
	// monorepoPR adds a second row of the same forge PR, under another
	// repository id.
	monorepoPR bool
	// thirdRow adds a third row of the same forge PR, for a run interrupted
	// with rows still undispatched.
	thirdRow bool
	// onExempt, when set, runs when an exempt call arrives (1-based), before
	// its response is written, so a test can cancel the command mid-loop.
	onExempt func(n int)
	// unlinkTransport and linkTransport drop the connection on every call, so
	// the row's outcome is unknown rather than refused.
	unlinkTransport bool
	linkTransport   bool
	// exemptFailAt fails that exempt call (1-based) with a 429, so a monorepo
	// run has a row that succeeded before the row that did not.
	exemptFailAt int
	// exemptConflictAt refuses that exempt call (1-based) with a 409: an HTTP
	// error the row did not change under.
	exemptConflictAt int
	// exemptTransportAt drops the connection on that exempt call (1-based), so
	// the row's outcome is unknown rather than refused.
	exemptTransportAt int
	// exemptIDs is the exemption id per call, so two rows are distinguishable.
	exemptIDs []string
	// linkResults is the server's result per link call (created, existing).
	linkResults []string
	// linkFailAll refuses every link call with a 500.
	linkFailAll bool
	// withVerdicts gives the ticket detail one approval and one exemption, for
	// the section order of `qa show`.
	withVerdicts bool
	// changeAcknowledged clears the session's unacknowledged-change flag while
	// leaving its timeline in place.
	changeAcknowledged bool
	// sessionSawChange and sessionVerified shape the session's own fields on
	// both the queue's card and the ticket detail.
	sessionSawChange bool
	sessionVerified  bool
	// truncatedRecheck is the recheck section's omitted count on the queue.
	truncatedRecheck int
	// noTicket adds the No ticket section: an open PR no Ticket links.
	noTicket bool
	// noTicketRegistrations and noTicketBlocked shape that PR's registration
	// counts, so a partly linked monorepo PR reads "1 of 2 registrations".
	noTicketRegistrations int
	noTicketBlocked       int
	// truncatedNoTicket is the No ticket section's omitted count.
	truncatedNoTicket int
	// noTicketMonorepo makes the No ticket item a monorepo PR: one item naming
	// both of its bare registrations.
	noTicketMonorepo bool
	// noTicketEmptyBlocked makes the No ticket item's blockedPullRequestIds
	// empty: an item the section lists but that names no row.
	noTicketEmptyBlocked bool
	// partlyLinkedPR makes acme/widget#77 a monorepo PR whose first
	// registration is linked to DT-1 and whose second is still bare: the card
	// carries the linked row, the No ticket item the bare one.
	partlyLinkedPR bool
	// acted records the pullRequestIds link and exempt were called with.
	acted []string
	// legacyQueue serves a pre-0.34 queue: no noTicket and no
	// truncated.noTicket, the shape a 0.33.1 server sends.
	legacyQueue bool
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

// actedOn records a PR the CLI sent a link or an exemption for.
func (s *qaServer) actedOn(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acted = append(s.acted, id)
}

// actedIDs is what link and exempt were called with, in order.
func (s *qaServer) actedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.acted...)
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
		s.mu.Lock()
		s.calls = append(s.calls, "queue")
		s.mu.Unlock()
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
			n := s.seen("exempt")
			s.mu.Lock()
			failAt, conflictAt := s.exemptFailAt, s.exemptConflictAt
			transportAt, ids := s.exemptTransportAt, s.exemptIDs
			hook := s.onExempt
			s.mu.Unlock()
			if hook != nil {
				hook(n)
			}
			if transportAt == n {
				if hj, ok := w.(http.Hijacker); ok {
					if conn, _, err := hj.Hijack(); err == nil {
						_ = conn.Close()
					}
				}
				return
			}
			if conflictAt == n {
				writeProblem(w, http.StatusConflict, "CONFLICT",
					"the row already holds an exemption", "urn:drift:problem:conflict",
					"This pull request is already exempt from QA.")
				return
			}
			if failAt == n {
				writeProblem(w, http.StatusTooManyRequests, "RATE_LIMITED",
					"Rate limit exceeded", "urn:drift:problem:rate-limited",
					"The credential has made too many requests.")
				return
			}
			id := qaExemptionID
			if n-1 < len(ids) {
				id = ids[n-1]
			}
			body := s.body("exempt")
			scope, gate := "ticket", any(nil)
			if body["scope"] == "pr" {
				if id, ok := body["pullRequestId"].(string); ok {
					s.actedOn(id)
				}
				scope = "pr"
				gate = map[string]any{
					"forgeRepo": "acme/widget", "sha": "abcdef1234567890",
					"pullRequestId": body["pullRequestId"],
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"exemptionId": id, "scope": scope,
				"ticketKeys": []string{"DT-1"}, "evaluations": []any{},
				"gateTarget": gate,
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
			n := s.seen("link")
			s.mu.Lock()
			results, failAll := s.linkResults, s.linkFailAll
			transport := s.linkTransport
			s.mu.Unlock()
			if transport {
				if hj, ok := w.(http.Hijacker); ok {
					if conn, _, err := hj.Hijack(); err == nil {
						_ = conn.Close()
					}
				}
				return
			}
			if failAll {
				writeProblem(w, http.StatusInternalServerError, "INTERNAL",
					"the link could not be recorded", "urn:drift:problem:internal",
					"The server failed to record the link.")
				return
			}
			result := "created"
			if n-1 < len(results) {
				result = results[n-1]
			}
			if id, ok := s.body("link")["pullRequestId"].(string); ok {
				s.actedOn(id)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pullRequestId": s.body("link")["pullRequestId"],
				"ticketKey":     "DT-1", "result": result,
				"evaluations": []any{},
			})
		case rest == "links/unlink":
			record("unlink", r)
			s.mu.Lock()
			transport := s.unlinkTransport
			s.mu.Unlock()
			if transport {
				if hj, ok := w.(http.Hijacker); ok {
					if conn, _, err := hj.Hijack(); err == nil {
						_ = conn.Close()
					}
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ticketKey": "DT-1", "forgeRepo": "acme/widget", "sha": "abcdef1234567890",
				"gateTarget": map[string]any{
					"forgeRepo": "acme/widget", "sha": "abcdef1234567890",
					"pullRequestId": s.body("unlink")["pullRequestId"],
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
	if s.monorepoPR {
		other := qaPRCardJSON()
		other["pullRequestId"] = qaPRID2
		other["repositoryId"] = qaRepoID2
		prs = append(prs, other)
	}
	if s.thirdRow {
		other := qaPRCardJSON()
		other["pullRequestId"] = qaPRID3
		other["repositoryId"] = qaRepoID3
		prs = append(prs, other)
	}
	if s.partlyLinkedPR && key == "DT-1" {
		other := qaPRCardJSON()
		other["pullRequestId"] = qaNoTicketPRID
		other["forgeRepo"] = "acme/widget"
		other["number"] = 77
		other["title"] = "Fix the flaky suite"
		other["headBranch"] = "fix-flaky-suite"
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

func qaNoTicketJSON() map[string]any {
	return map[string]any{
		"pullRequestId": qaNoTicketPRID, "repositoryId": qaRepoID,
		"blockedPullRequestIds": []string{qaNoTicketPRID},
		"forgeRepo":             "acme/widget", "number": 77,
		"title": "Fix the flaky suite", "authorLogin": "octo",
		"headBranch": "fix-flaky-suite", "updatedAt": "2026-10-28T09:00:00Z",
		"registrations": 1, "blockedRegistrations": 1,
		"url": "https://github.com/acme/widget/pull/77",
	}
}

func (s *qaServer) queueJSON() map[string]any {
	s.mu.Lock()
	sawChange, verified, truncated := s.sessionSawChange, s.sessionVerified, s.truncatedRecheck
	noTicket, registrations, blocked := s.noTicket, s.noTicketRegistrations, s.noTicketBlocked
	truncatedNoTicket, legacy := s.truncatedNoTicket, s.legacyQueue
	monorepo, partly := s.noTicketMonorepo, s.partlyLinkedPR
	emptyBlocked := s.noTicketEmptyBlocked
	s.mu.Unlock()
	truncatedCounts := map[string]any{
		"readyForQa": 0, "inTesting": 0, "recheck": truncated, "upcoming": 0,
	}
	q := map[string]any{
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
				"unacknowledgedChange": false, "sawChange": sawChange,
				"currentGenerationVerified": verified, "changeCount": 0,
				"banners": []any{},
			}},
		}},
		"recheck":   []any{},
		"upcoming":  []map[string]any{s.cardJSON("DT-3", "Awaiting review", "upcoming")},
		"truncated": truncatedCounts,
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
	if legacy {
		return q
	}
	items := []map[string]any{}
	if noTicket {
		item := qaNoTicketJSON()
		if registrations > 0 {
			item["registrations"] = registrations
		}
		if blocked > 0 {
			item["blockedRegistrations"] = blocked
		}
		if emptyBlocked {
			item["blockedPullRequestIds"] = []string{}
		}
		items = append(items, item)
	}
	if monorepo {
		item := qaNoTicketJSON()
		item["blockedPullRequestIds"] = []string{qaNoTicketPRID, qaNoTicketPRID2}
		item["registrations"] = 2
		item["blockedRegistrations"] = 2
		items = append(items, item)
	}
	if partly {
		item := qaNoTicketJSON()
		item["pullRequestId"] = qaNoTicketPRID2
		item["repositoryId"] = qaRepoID2
		item["blockedPullRequestIds"] = []string{qaNoTicketPRID2}
		item["registrations"] = 2
		item["blockedRegistrations"] = 1
		items = append(items, item)
	}
	q["noTicket"] = items
	truncatedCounts["noTicket"] = truncatedNoTicket
	return q
}

func qaSessionDetailJSON() map[string]any {
	return map[string]any{
		"id": qaSessionID, "ticketKey": "DT-1", "state": "open",
		"owner": "qa@example.com", "environmentId": qaEnvID,
		"environmentSlug": "proof-alpha", "startGeneration": 2,
		"currentGeneration": 3, "startedAt": "2026-10-20T10:00:00Z",
		"endedAt": nil, "endReason": nil, "pullRequestIds": []any{},
		"unacknowledgedChange": true, "events": []any{},
		"currentGenerationVerified": false, "sawChange": false,
	}
}

func (s *qaServer) sessionDetailJSON() map[string]any {
	d := qaSessionDetailJSON()
	d["events"] = s.timeline()
	s.mu.Lock()
	defer s.mu.Unlock()
	d["currentGenerationVerified"] = s.sessionVerified
	d["sawChange"] = s.sessionSawChange
	if s.endReason != "" {
		d["state"] = "ended"
		d["endReason"] = s.endReason
	}
	if s.changeAcknowledged {
		d["unacknowledgedChange"] = false
	}
	return d
}

// timeline is the session's events as the server sends them: [] when there are
// none, never null.
func (s *qaServer) timeline() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events == nil {
		return []map[string]any{}
	}
	return s.events
}

// qaEventJSON is one timeline entry. before/after are ARRAYS of deployment
// services, which is the shape the generated model cannot decode.
func qaEventJSON(kind string, generation int, before, after []map[string]any) map[string]any {
	return map[string]any{
		"id": qaEventID, "at": "2026-10-20T11:00:00Z", "generation": generation,
		"kind": kind, "before": before, "after": after,
	}
}

func qaServiceJSON(repoID, branch, sha, tag string) map[string]any {
	return map[string]any{
		"repositoryId": repoID, "branch": branch, "commitSha": sha, "imageTag": tag,
	}
}

func (s *qaServer) ticketDetailJSON() map[string]any {
	s.mu.Lock()
	withVerdicts := s.withVerdicts
	s.mu.Unlock()
	approvals, exemptions := []any{}, []any{}
	if withVerdicts {
		approvals = []any{map[string]any{
			"id": qaApprovalID, "ticketKey": "DT-1", "approver": "qa@example.com",
			"at": "2026-10-21T10:00:00Z", "generation": 3, "retested": true,
			"noRetestReason": nil, "note": nil, "pullRequests": []any{},
			"revokedAt": nil, "revokedBy": nil, "revokeReason": nil,
		}}
		exemptions = []any{map[string]any{
			"id": qaExemptionID, "scope": "ticket", "ticketKey": "DT-1",
			"pullRequestId": nil, "forgeRepo": nil, "number": nil,
			"reason": "covered by the e2e suite", "grantedBy": "qa@example.com",
			"grantedAuto": false, "at": "2026-10-21T10:00:00Z",
			"revokedAt": nil, "revokedBy": nil,
		}}
	}
	return map[string]any{
		"card":       s.cardJSON("DT-1", "Ship the widget", "ready"),
		"sessions":   []map[string]any{s.sessionDetailJSON()},
		"approvals":  approvals,
		"exemptions": exemptions,
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
	v["events"] = s.timeline()
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

// TestQaListMarksTruncatedSections pins both halves of a capped section: the
// table says how many the server held back, under the section's own rows, and
// JSON carries the counts under `truncated` rather than a pseudo-row.
func TestQaListMarksTruncatedSections(t *testing.T) {
	srv := newQaServer(t)
	srv.truncatedRecheck = 7
	h := newQaHarness(t, srv)

	out, errOut, code := h.run("qa", "list")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "+7 more") {
		t.Fatalf("the truncated recheck section is not marked:\n%s", out)
	}
	if strings.Contains(out, "+0 more") {
		t.Fatalf("a section with nothing omitted is marked anyway:\n%s", out)
	}

	out, _, code = h.run("qa", "list", "-o", "json")
	if code != cliexit.OK {
		t.Fatalf("exit %d", code)
	}
	var payload struct {
		Items []struct {
			Key string `json:"key"`
		} `json:"items"`
		Truncated struct {
			Ready    int `json:"ready"`
			Testing  int `json:"testing"`
			Recheck  int `json:"recheck"`
			Upcoming int `json:"upcoming"`
		} `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("not JSON: %s\n%s", err, out)
	}
	if payload.Truncated.Recheck != 7 {
		t.Fatalf("truncated.recheck = %d, want 7", payload.Truncated.Recheck)
	}
	if payload.Truncated.Ready != 0 || payload.Truncated.Testing != 0 || payload.Truncated.Upcoming != 0 {
		t.Fatalf("truncated = %+v, want only recheck set", payload.Truncated)
	}
	for _, item := range payload.Items {
		if strings.Contains(item.Key, "more") {
			t.Fatalf("the table's marker leaked into the items: %+v", payload.Items)
		}
	}
}

// TestQaListNoTicketSection pins the section for open PRs no Ticket links: its
// own table under the Ticket sections, the `+N more` row when the server capped
// it, and the machine format's `no_ticket` / `truncated.no_ticket`.
func TestQaListNoTicketSection(t *testing.T) {
	srv := newQaServer(t)
	srv.noTicket = true
	srv.noTicketRegistrations = 2
	srv.noTicketBlocked = 1
	srv.truncatedNoTicket = 3
	h := newQaHarness(t, srv)

	out, errOut, code := h.run("qa", "list")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	updated := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC).Local().Format("2006-01-02 15:04")
	for _, want := range []string{"NO TICKET (open pull requests with no live ticket link and no exemption)",
		"acme/widget#77", "Fix the flaky suite", "octo", updated, "1 of 2 registrations", "+3 more"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "upcoming") > strings.Index(out, "NO TICKET") ||
		strings.Index(out, "NO TICKET") > strings.Index(out, "CARRY-OVER") {
		t.Fatalf("the No ticket table is not between the sections and CARRY-OVER:\n%s", out)
	}

	out, _, code = h.run("qa", "list", "-o", "json")
	if code != cliexit.OK {
		t.Fatalf("exit %d", code)
	}
	var payload struct {
		Items    []map[string]any `json:"items"`
		NoTicket []struct {
			PullRequestID string `json:"pull_request_id"`
			RepositoryID  string `json:"repository_id"`
			Repo          string `json:"repo"`
			Number        int    `json:"number"`
			Title         string `json:"title"`
			Author        string `json:"author"`
			HeadBranch    string `json:"head_branch"`
			Updated       string `json:"updated"`
			Registrations int    `json:"registrations"`
			Blocked       int    `json:"blocked_registrations"`
			URL           string `json:"url"`
		} `json:"no_ticket"`
		Truncated struct {
			NoTicket int `json:"no_ticket"`
		} `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("not JSON: %s\n%s", err, out)
	}
	if len(payload.NoTicket) != 1 {
		t.Fatalf("no_ticket = %+v, want one item", payload.NoTicket)
	}
	item := payload.NoTicket[0]
	if item.PullRequestID != qaNoTicketPRID || item.RepositoryID != qaRepoID ||
		item.Repo != "acme/widget" || item.Number != 77 || item.Title != "Fix the flaky suite" ||
		item.Author != "octo" || item.HeadBranch != "fix-flaky-suite" ||
		item.Updated != "2026-10-28T09:00:00Z" || item.Registrations != 2 || item.Blocked != 1 ||
		item.URL != "https://github.com/acme/widget/pull/77" {
		t.Fatalf("no_ticket[0] = %+v", item)
	}
	if payload.Truncated.NoTicket != 3 {
		t.Fatalf("truncated.no_ticket = %d, want 3", payload.Truncated.NoTicket)
	}
	for _, row := range payload.Items {
		if row["key"] == "acme/widget#77" {
			t.Fatalf("the section leaked into items: %+v", row)
		}
	}
}

// TestQaListNoTicketBlockedIDs pins the payload's registration list: every
// bare row the item covers, as the server sent it, so a script can retry one by
// UUID.
func TestQaListNoTicketBlockedIDs(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*qaServer)
		want  []string
	}{
		{"declared", func(s *qaServer) { s.noTicket = true }, []string{qaNoTicketPRID}},
		{"every registration", func(s *qaServer) { s.noTicketMonorepo = true },
			[]string{qaNoTicketPRID, qaNoTicketPRID2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newQaServer(t)
			tc.setup(srv)
			h := newQaHarness(t, srv)

			out, errOut, code := h.run("qa", "list", "-o", "json")
			if code != cliexit.OK {
				t.Fatalf("exit %d\n%s", code, errOut)
			}
			var payload struct {
				NoTicket []struct {
					BlockedPullRequestIDs []string `json:"blocked_pull_request_ids"`
				} `json:"no_ticket"`
			}
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatalf("not JSON: %s\n%s", err, out)
			}
			if len(payload.NoTicket) != 1 {
				t.Fatalf("no_ticket = %+v, want one item", payload.NoTicket)
			}
			if got := payload.NoTicket[0].BlockedPullRequestIDs; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("blocked_pull_request_ids = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestQaListLegacyQueue pins a pre-0.34 payload: a server that sends neither
// noTicket nor truncated.noTicket decodes to an empty section, renders as
// before, and leaves resolution unchanged for a PR the queue does not list.
func TestQaListLegacyQueue(t *testing.T) {
	srv := newQaServer(t)
	srv.legacyQueue = true
	h := newQaHarness(t, srv)

	out, errOut, code := h.run("qa", "list")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if strings.Contains(out, "NO TICKET") {
		t.Fatalf("a server without the section grew one:\n%s", out)
	}
	for _, want := range []string{"DT-1", "DT-3", "CARRY-OVER"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}

	out, _, code = h.run("qa", "list", "-o", "json")
	if code != cliexit.OK {
		t.Fatalf("exit %d", code)
	}
	var payload struct {
		NoTicket  []any `json:"no_ticket"`
		Truncated struct {
			NoTicket int `json:"no_ticket"`
		} `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("not JSON: %s\n%s", err, out)
	}
	if len(payload.NoTicket) != 0 || payload.Truncated.NoTicket != 0 {
		t.Fatalf("no_ticket = %v, truncated.no_ticket = %d, want both empty",
			payload.NoTicket, payload.Truncated.NoTicket)
	}

	_, errOut, code = h.run("qa", "exempt", "--pr", "acme/widget#77", "--reason", "docs only", "--yes")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "no PR acme/widget#77 on the QA queue") {
		t.Fatalf("the not-found message changed:\n%s", errOut)
	}
	if n := srv.seen("exempt"); n != 0 {
		t.Fatalf("%d exempt request(s) sent for an unresolved PR", n)
	}
}

// TestQaResolvesNoTicketPR pins that repo#n resolution sees the No ticket
// section: exempt and link reach a PR no Ticket links, through the item's bare
// registration ids.
func TestQaResolvesNoTicketPR(t *testing.T) {
	t.Run("exempt", func(t *testing.T) {
		srv := newQaServer(t)
		srv.noTicket = true
		h := newQaHarness(t, srv)

		_, errOut, code := h.run("qa", "exempt", "--pr", "acme/widget#77", "--reason", "docs only", "--yes")
		if code != cliexit.OK {
			t.Fatalf("exit %d\n%s", code, errOut)
		}
		body := srv.body("exempt")
		if body["scope"] != "pr" || body["pullRequestId"] != qaNoTicketPRID {
			t.Fatalf("exempt body = %v, want the item's pullRequestId", body)
		}
	})

	t.Run("link", func(t *testing.T) {
		srv := newQaServer(t)
		srv.noTicket = true
		h := newQaHarness(t, srv)

		_, errOut, code := h.run("qa", "link", "DT-1", "acme/widget#77")
		if code != cliexit.OK {
			t.Fatalf("exit %d\n%s", code, errOut)
		}
		body := srv.body("link")
		if body["ticketKey"] != "DT-1" || body["pullRequestId"] != qaNoTicketPRID {
			t.Fatalf("link body = %v", body)
		}
	})
}

// TestQaNoTicketItemWithNoRows pins that a No ticket item naming no
// registrations is unresolvable: repo#n stops at the usage/not-found exit
// instead of resolving to zero rows and sending nothing.
func TestQaNoTicketItemWithNoRows(t *testing.T) {
	cases := []struct {
		name string
		args []string
		op   string
	}{
		{"exempt", []string{"qa", "exempt", "--pr", "acme/widget#77", "--reason", "docs only", "--yes"}, "exempt"},
		{"link", []string{"qa", "link", "DT-1", "acme/widget#77"}, "link"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newQaServer(t)
			srv.noTicket = true
			srv.noTicketEmptyBlocked = true
			h := newQaHarness(t, srv)

			_, errOut, code := h.run(tc.args...)
			if code != cliexit.Usage {
				t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
			}
			if !strings.Contains(errOut, "No ticket item for acme/widget#77 names no rows") {
				t.Fatalf("wrong message:\n%s", errOut)
			}
			if n := srv.seen(tc.op); n != 0 {
				t.Fatalf("%d %s call(s) sent for an item naming no rows", n, tc.op)
			}
		})
	}
}

// TestQaResolvesEveryNoTicketRegistration pins the monorepo case: the item
// names both bare registrations, so link and exempt send each id once, in
// order, from a single queue read.
func TestQaResolvesEveryNoTicketRegistration(t *testing.T) {
	cases := []struct {
		name string
		args []string
		op   string
	}{
		{"link", []string{"qa", "link", "DT-1", "acme/widget#77", "-o", "json"}, "link"},
		{"exempt", []string{"qa", "exempt", "--pr", "acme/widget#77", "--reason", "docs only", "--yes", "-o", "json"}, "exempt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newQaServer(t)
			srv.noTicketMonorepo = true
			h := newQaHarness(t, srv)

			out, errOut, code := h.run(tc.args...)
			if code != cliexit.OK {
				t.Fatalf("exit %d\n%s", code, errOut)
			}
			if n := srv.seen(tc.op); n != 2 {
				t.Fatalf("%d %s call(s), want one per registration:\n%s", n, tc.op, out)
			}
			if want := []string{qaNoTicketPRID, qaNoTicketPRID2}; !reflect.DeepEqual(srv.actedIDs(), want) {
				t.Fatalf("acted on %v, want %v", srv.actedIDs(), want)
			}
			if n := srv.seen("queue"); n != 1 {
				t.Fatalf("%d queue read(s), want one for both registrations", n)
			}
			var payload struct {
				Rows []struct {
					Outcome string `json:"outcome"`
				} `json:"rows"`
			}
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatalf("not JSON: %s\n%s", err, out)
			}
			if len(payload.Rows) != 2 {
				t.Fatalf("rows = %d, want one per registration:\n%s", len(payload.Rows), out)
			}
			for _, r := range payload.Rows {
				if r.Outcome != "ok" {
					t.Fatalf("row outcome = %q, want ok:\n%s", r.Outcome, out)
				}
			}
		})
	}
}

// TestQaExemptPartlyLinkedPR pins the forge-PR contract on a monorepo PR whose
// second registration is still bare: exempt --pr covers the card's linked row
// first, then the No ticket item's bare ids, in that order.
func TestQaExemptPartlyLinkedPR(t *testing.T) {
	srv := newQaServer(t)
	srv.partlyLinkedPR = true
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "exempt", "--pr", "acme/widget#77", "--reason", "docs only", "--yes")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	want := []string{qaNoTicketPRID, qaNoTicketPRID2}
	if got := srv.actedIDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("acted on %v, want the card's linked row then the item's bare id, %v", got, want)
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

// TestQaShowReportsSessionVerification pins the two per-session facts the
// table and JSON now carry: whether the generation the session runs is
// verified, and whether the session ever saw a change -- which outlives the
// acknowledgement, so it is not `unacknowledged_change`.
func TestQaShowReportsSessionVerification(t *testing.T) {
	cases := []struct {
		name          string
		verified, saw bool
	}{
		{"both set", true, true},
		{"neither set", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newQaServer(t)
			srv.sessionVerified = tc.verified
			srv.sessionSawChange = tc.saw
			h := newQaHarness(t, srv)

			// The wide-only `change` column is also a bool, so the count is
			// taken from the default table, where it is absent.
			out, errOut, code := h.run("qa", "show", "DT-1")
			if code != cliexit.OK {
				t.Fatalf("exit %d\n%s", code, errOut)
			}
			for _, header := range []string{"VERIFIED", "SAW CHANGE"} {
				if !strings.Contains(out, header) {
					t.Fatalf("the sessions table has no %s column:\n%s", header, out)
				}
			}
			row := ""
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, qaSessionID) {
					row = line
					break
				}
			}
			if row == "" {
				t.Fatalf("no session row:\n%s", out)
			}
			wantTrue := 0
			if tc.verified {
				wantTrue++
			}
			if tc.saw {
				wantTrue++
			}
			if got := strings.Count(row, "true"); got != wantTrue {
				t.Fatalf("session row has %d true cell(s), want %d:\n%s", got, wantTrue, row)
			}

			out, _, code = h.run("qa", "show", "DT-1", "-o", "json")
			if code != cliexit.OK {
				t.Fatalf("json: exit %d", code)
			}
			var payload struct {
				Sessions []struct {
					Verified  bool `json:"verified"`
					SawChange bool `json:"saw_change"`
				} `json:"sessions"`
			}
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatalf("not JSON: %s\n%s", err, out)
			}
			if len(payload.Sessions) != 1 ||
				payload.Sessions[0].Verified != tc.verified ||
				payload.Sessions[0].SawChange != tc.saw {
				t.Fatalf("sessions = %+v, want verified=%t saw_change=%t",
					payload.Sessions, tc.verified, tc.saw)
			}
		})
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

// TestQaApproveNamesTheChangeThatRequiresAReason pins why the missing retest
// statement is refused: a session that saw a change forces the reason, and the
// refusal says so; one that saw none keeps the generic wording.
func TestQaApproveNamesTheChangeThatRequiresAReason(t *testing.T) {
	srv := newQaServer(t)
	srv.sessionSawChange = true
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "approve", qaSessionID, "--revision", "3", "--yes")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	for _, want := range []string{"saw a change", "--retested", "--no-retest"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("missing %q:\n%s", want, errOut)
		}
	}
	if n := srv.seen("approve"); n != 0 {
		t.Fatalf("%d approve request(s) sent for a usage error", n)
	}

	quiet := newQaServer(t)
	hQuiet := newQaHarness(t, quiet)
	_, errOut, code = hQuiet.run("qa", "approve", qaSessionID, "--revision", "3", "--yes")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if strings.Contains(errOut, "saw a change") {
		t.Fatalf("a session that saw no change claims one:\n%s", errOut)
	}
	if !strings.Contains(errOut, "--no-retest") {
		t.Fatalf("the refusal does not name the flags:\n%s", errOut)
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

	out, errOut, code := h.run("qa", "end", qaSessionID, "--yes")
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

			out, errOut, code := h.run("qa", "end", qaSessionID, "--yes", "-o", "json")
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
	first, second := strings.Index(errOut, "acme/widget#42"), strings.Index(errOut, "other/widget#42")
	if first < 0 || second < 0 || first > second {
		t.Fatalf("the candidates are not listed by repository, in card order:\n%s", errOut)
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

// --- fix-round regressions --------------------------------------------------

func TestQaEndRequiresConfirmation(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "end", qaSessionID)
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "--yes") {
		t.Fatalf("the refusal does not name --yes:\n%s", errOut)
	}
	if n := srv.seen("end"); n != 0 {
		t.Fatalf("%d close request(s) sent without confirmation", n)
	}

	if _, errOut, code := h.run("qa", "end", qaSessionID, "--yes"); code != cliexit.OK {
		t.Fatalf("with --yes: exit %d\n%s", code, errOut)
	}
	if n := srv.seen("end"); n != 1 {
		t.Fatalf("close was called %d times, want 1", n)
	}
}

func TestQaShowKeepsSessionTimeline(t *testing.T) {
	srv := newQaServer(t)
	srv.events = []map[string]any{
		qaEventJSON("redeploy", 3,
			[]map[string]any{qaServiceJSON(qaRepoID, "main", "abcdef1234567890", "img-1")},
			[]map[string]any{qaServiceJSON(qaRepoID, "main", "fedcba0987654321", "img-2")}),
		qaEventJSON("migration-source-changed", 3, nil, nil),
		qaEventJSON("unverified", 3, nil, nil),
	}
	h := newQaHarness(t, srv)

	out, errOut, code := h.run("qa", "show", "DT-1", "-o", "json")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	var payload struct {
		SessionCount int `json:"session_count"`
		Sessions     []struct {
			Events []struct {
				Kind       string           `json:"kind"`
				Generation int              `json:"generation"`
				Before     []map[string]any `json:"before"`
				After      []map[string]any `json:"after"`
			} `json:"events"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("not JSON: %s\n%s", err, out)
	}
	if payload.SessionCount != 1 {
		t.Fatalf("session_count = %d, want 1", payload.SessionCount)
	}
	if len(payload.Sessions) != 1 || len(payload.Sessions[0].Events) != 3 {
		t.Fatalf("the timeline is not in the payload: %+v", payload.Sessions)
	}
	events := payload.Sessions[0].Events
	if events[0].Kind != "redeploy" || events[0].Generation != 3 {
		t.Fatalf("first event = %+v", events[0])
	}
	if len(events[0].Before) != 1 || events[0].Before[0]["branch"] != "main" {
		t.Fatalf("before = %+v, want the array the server sent", events[0].Before)
	}
	if len(events[0].After) != 1 || events[0].After[0]["image_tag"] != "img-2" {
		t.Fatalf("after = %+v", events[0].After)
	}
	if events[1].Kind != "migration-source-changed" || events[1].Before != nil {
		t.Fatalf("second event = %+v", events[1])
	}

	out, errOut, code = h.run("qa", "show", "DT-1", "-o", "wide")
	if code != cliexit.OK {
		t.Fatalf("wide: exit %d\n%s", code, errOut)
	}
	for _, want := range []string{
		"CHANGES (unacknowledged)", "redeploy",
		"main@abcdef1 (img-1) -> main@fedcba0 (img-2)",
		"migration source moved", "deployment not verified",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestQaUnacknowledgedEventsStartAfterTheAck(t *testing.T) {
	events := []api.QaSessionEvent{
		{Kind: "redeploy", Generation: 2},
		{Kind: "acknowledged", Generation: 2},
		{Kind: "unverified", Generation: 3},
	}
	got := qaUnacknowledgedEvents(events)
	if len(got) != 1 || got[0].Kind != "unverified" || got[0].Generation != 3 {
		t.Fatalf("got %+v, want only the event after the acknowledgement", got)
	}
}

func TestQaMonorepoPRRowsActTogether(t *testing.T) {
	srv := newQaServer(t)
	srv.monorepoPR = true
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "start", "DT-1", "--prs", "acme/widget#42", "--yes")
	if code != cliexit.OK {
		t.Fatalf("start: exit %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "acme/widget#42") {
		t.Fatalf("the confirmation does not name the PR once:\n%s", errOut)
	}
	prs, _ := srv.body("sessions")["prs"].([]any)
	if len(prs) != 2 || prs[0] != qaPRID || prs[1] != qaPRID2 {
		t.Fatalf("prs = %v, want both rows of the one forge PR", prs)
	}

	if _, errOut, code := h.run("qa", "unlink", "DT-1", "acme/widget#42", "--yes"); code != cliexit.OK {
		t.Fatalf("unlink: exit %d\n%s", code, errOut)
	}
	if n := srv.seen("unlink"); n != 2 {
		t.Fatalf("unlink ran %d times, want once per row", n)
	}

	if _, errOut, code := h.run("qa", "exempt", "--pr", "acme/widget#42", "--reason", "docs only", "--yes"); code != cliexit.OK {
		t.Fatalf("exempt: exit %d\n%s", code, errOut)
	}
	if n := srv.seen("exempt"); n != 2 {
		t.Fatalf("exempt ran %d times, want once per row", n)
	}

	if _, errOut, code := h.run("qa", "link", "DT-1", "acme/widget#42"); code != cliexit.OK {
		t.Fatalf("link: exit %d\n%s", code, errOut)
	}
	if n := srv.seen("link"); n != 2 {
		t.Fatalf("link ran %d times, want once per row", n)
	}
}

func TestQaApproveRetestedFalseNeedsAReason(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "approve", qaSessionID, "--revision", "3", "--retested=false", "--yes")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "say whether the revision was retested") {
		t.Fatalf("--retested=false counted as a statement:\n%s", errOut)
	}
	if n := srv.seen("approve"); n != 0 {
		t.Fatalf("%d approve request(s) sent", n)
	}

	if _, errOut, code := h.run("qa", "approve", qaSessionID,
		"--revision", "3", "--retested=false", "--no-retest", "flaky env", "--yes"); code != cliexit.OK {
		t.Fatalf("with a reason: exit %d\n%s", code, errOut)
	}
	body := srv.body("approve")
	if body["retested"] != false || body["noRetestReason"] != "flaky env" {
		t.Fatalf("body = %v", body)
	}
}

func TestQaQueuedPRZeroMatchHintsPerCommand(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "link", "DT-1", "acme/widget#99")
	if code != cliexit.Usage {
		t.Fatalf("link: exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "pass its UUID instead") {
		t.Fatalf("link's hint is missing:\n%s", errOut)
	}
	if strings.Contains(errOut, "--ticket") {
		t.Fatalf("link's hint mentions a flag link does not take:\n%s", errOut)
	}

	_, errOut, code = h.run("qa", "exempt", "--pr", "acme/widget#99", "--reason", "x", "--yes")
	if code != cliexit.Usage {
		t.Fatalf("exempt: exit %d, want %d\n%s", code, cliexit.Usage, errOut)
	}
	if !strings.Contains(errOut, "--ticket") {
		t.Fatalf("exempt's hint lost --ticket:\n%s", errOut)
	}
}

func TestQaAttachmentErrorsSayPerRequest(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)
	dir := t.TempDir()

	small := filepath.Join(dir, "one.txt")
	if err := os.WriteFile(small, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"qa", "comment", "DT-1", "--body", "x"}
	for range qaMaxAttachments + 1 {
		args = append(args, "--file", small)
	}
	_, errOut, code := h.run(args...)
	if code != cliexit.Usage {
		t.Fatalf("count cap: exit %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "per request") || strings.Contains(errOut, "per verdict") {
		t.Fatalf("a comment's cap is not worded per request:\n%s", errOut)
	}

	big := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(big, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, qaMaxAttachmentBytes+1); err != nil {
		t.Fatal(err)
	}
	_, errOut, code = h.run("qa", "comment", "DT-1", "--body", "x", "--file", big)
	if code != cliexit.Usage {
		t.Fatalf("size cap: exit %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "per request") || strings.Contains(errOut, "per verdict") {
		t.Fatalf("the size cap is not worded per request:\n%s", errOut)
	}
}

// TestQaExtraKeysNeverShadowColumns pins the machine contract: a key that is a
// column and a key that is Extra must never be the same name, or `-o json`
// shows a list where `--json <field>` and the table show a count.
//
// The comparison is by VALUE rather than by name: Extra is merged over the row
// before either projection is visible from outside, so a shadowing key is
// exactly what makes `-o json` and `--json <field>` disagree about one field.
func TestQaExtraKeysNeverShadowColumns(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	cases := []struct {
		name string
		args []string
		cols []output.Column
	}{
		{"show", []string{"qa", "show", "DT-1"}, qaShowColumns()},
		{"approve", []string{"qa", "approve", qaSessionID, "--revision", "3", "--retested", "--yes"}, qaApprovalOutcomeColumns()},
		{"reject", []string{"qa", "reject", qaSessionID, "--revision", "3", "--comment", "broken", "--yes"}, qaRejectionColumns()},
		{"exempt", []string{"qa", "exempt", "--ticket", "DT-1", "--reason", "docs", "--yes"}, qaExemptionOutcomeColumns()},
		{"unexempt", []string{"qa", "unexempt", qaExemptionID, "--yes"}, qaRevocationColumns()},
		{"revoke", []string{"qa", "revoke", "--approval", qaApprovalID, "--yes"}, qaRevocationColumns()},
		{"link", []string{"qa", "link", "DT-1", "acme/widget#42"}, qaLinkColumns()},
		{"unlink", []string{"qa", "unlink", "DT-1", "acme/widget#42", "--yes"}, qaUnlinkColumns()},
		{"send", []string{"qa", "send", "DT-1", "--yes"}, qaSendColumns()},
		{"comment", []string{"qa", "comment", "DT-1", "--body", "hi"}, qaCommentColumns()},
		{"carryover", []string{"qa", "carryover", "DT-1", "--yes"}, qaCarryOverColumns()},
		{"start", []string{"qa", "start", "DT-1", "--yes"}, qaSessionMutationColumns()},
		{"ack", []string{"qa", "ack", qaSessionID}, qaSessionMutationColumns()},
		{"end", []string{"qa", "end", qaSessionID, "--yes"}, qaSessionMutationColumns()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full, errOut, code := h.run(append(append([]string{}, tc.args...), "-o", "json")...)
			if code != cliexit.OK {
				t.Fatalf("exit %d\n%s", code, errOut)
			}
			var whole map[string]any
			if err := json.Unmarshal([]byte(full), &whole); err != nil {
				t.Fatalf("not JSON: %s\n%s", err, full)
			}
			for _, col := range tc.cols {
				one, errOut, code := h.run(append(append([]string{}, tc.args...), "--json", col.Name)...)
				if code != cliexit.OK {
					t.Fatalf("--json %s: exit %d\n%s", col.Name, code, errOut)
				}
				var projected map[string]any
				if err := json.Unmarshal([]byte(one), &projected); err != nil {
					t.Fatalf("--json %s is not JSON: %s\n%s", col.Name, err, one)
				}
				if !reflect.DeepEqual(projected[col.Name], whole[col.Name]) {
					t.Fatalf("field %q: -o json gives %T, --json gives %T (Extra shadows the column)",
						col.Name, whole[col.Name], projected[col.Name])
				}
			}
		})
	}
}

// TestQaShowMapsTheServerProblem guards the raw read's error path: it must
// answer with the server's own envelope, not a decode failure.
func TestQaShowMapsTheServerProblem(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)

	_, errOut, code := h.run("qa", "show", "DT-404")
	if code != cliexit.NotFound {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.NotFound, errOut)
	}
	if !strings.Contains(errOut, "No Ticket with that key") {
		t.Fatalf("the server's message is missing:\n%s", errOut)
	}
}

// TestQaExemptReportsPerRowOutcomes pins the per-row contract: a monorepo PR is
// several rows, the server commits each on its own, so every row carries its own
// outcome — ok, refused (an HTTP error, nothing changed) or unknown (no usable
// answer, the row may have changed).
func TestQaExemptReportsPerRowOutcomes(t *testing.T) {
	type rowPayload struct {
		PullRequestID string `json:"pull_request_id"`
		Outcome       string `json:"outcome"`
		ExitCode      int    `json:"exit_code"`
		Message       string `json:"message"`
		Hint          string `json:"hint"`
		ExemptionID   string `json:"exemption_id"`
	}
	type payload struct {
		ExemptionIDs []string     `json:"exemption_ids"`
		Rows         []rowPayload `json:"rows"`
		Partial      bool         `json:"partial"`
	}
	run := func(t *testing.T, srv *qaServer) (payload, string, int) {
		t.Helper()
		out, errOut, code := newQaHarness(t, srv).run("qa", "exempt", "--pr", "acme/widget#42",
			"--reason", "docs only", "--yes", "-o", "json")
		var p payload
		if out != "" {
			if err := json.Unmarshal([]byte(out), &p); err != nil {
				t.Fatalf("not JSON: %s\n%s", err, out)
			}
		}
		return p, errOut, code
	}

	t.Run("refused on row 2", func(t *testing.T) {
		srv := newQaServer(t)
		srv.monorepoPR = true
		srv.exemptIDs = []string{qaExemptionID}
		srv.exemptConflictAt = 2
		p, errOut, code := run(t, srv)
		if code != cliexit.Conflict {
			t.Fatalf("exit %d, want %d\n%s", code, cliexit.Conflict, errOut)
		}
		if n := srv.seen("exempt"); n != 2 {
			t.Fatalf("exempt ran %d times, want both rows attempted", n)
		}
		if !strings.Contains(errOut, "1 of 2 repository row(s) failed") {
			t.Fatalf("the partial run does not say it was partial:\n%s", errOut)
		}
		if len(p.Rows) != 2 {
			t.Fatalf("rows = %+v, want one entry per attempted row", p.Rows)
		}
		if p.Rows[0].Outcome != "ok" || p.Rows[0].ExemptionID != qaExemptionID {
			t.Fatalf("row 1 = %+v, want the row that changed", p.Rows[0])
		}
		if p.Rows[1].PullRequestID != qaPRID2 || p.Rows[1].Outcome != "refused" ||
			p.Rows[1].ExitCode != cliexit.Conflict || p.Rows[1].Message == "" {
			t.Fatalf("row 2 = %+v, want a refusal with the server's message", p.Rows[1])
		}
		if !p.Partial {
			t.Fatal("partial = false, want true when one row changed and one did not")
		}
		if len(p.ExemptionIDs) != 1 || p.ExemptionIDs[0] != qaExemptionID {
			t.Fatalf("exemption_ids = %v, want the row that changed", p.ExemptionIDs)
		}
	})

	t.Run("transport failure on row 2", func(t *testing.T) {
		srv := newQaServer(t)
		srv.monorepoPR = true
		srv.exemptTransportAt = 2
		p, errOut, code := run(t, srv)
		if code != cliexit.Error {
			t.Fatalf("exit %d, want %d\n%s", code, cliexit.Error, errOut)
		}
		if len(p.Rows) != 2 || p.Rows[1].Outcome != "unknown" {
			t.Fatalf("rows = %+v, want row 2 reported as unknown", p.Rows)
		}
		if !strings.Contains(p.Rows[1].Hint, "drift qa show") {
			t.Fatalf("row 2 hint = %q, want advice to check with `drift qa show`", p.Rows[1].Hint)
		}
		if p.Rows[1].ExitCode != cliexit.Error {
			t.Fatalf("row 2 exit_code = %d, want %d", p.Rows[1].ExitCode, cliexit.Error)
		}
		if !p.Partial {
			t.Fatal("partial = false, want true when one row changed and one did not")
		}
	})

	t.Run("every row changed", func(t *testing.T) {
		srv := newQaServer(t)
		srv.monorepoPR = true
		srv.exemptIDs = []string{qaExemptionID, qaExemptionID2}
		p, errOut, code := run(t, srv)
		if code != cliexit.OK {
			t.Fatalf("exit %d\n%s", code, errOut)
		}
		if p.Partial {
			t.Fatal("partial = true, want false when every row changed")
		}
		if len(p.Rows) != 2 || p.Rows[0].Outcome != "ok" || p.Rows[1].Outcome != "ok" {
			t.Fatalf("rows = %+v, want both rows ok", p.Rows)
		}
	})

	t.Run("table", func(t *testing.T) {
		srv := newQaServer(t)
		srv.monorepoPR = true
		srv.exemptIDs = []string{qaExemptionID}
		srv.exemptConflictAt = 2
		out, errOut, code := newQaHarness(t, srv).run("qa", "exempt", "--pr", "acme/widget#42",
			"--reason", "docs only", "--yes")
		if code != cliexit.Conflict {
			t.Fatalf("exit %d, want %d\n%s", code, cliexit.Conflict, errOut)
		}
		if !strings.Contains(out, qaExemptionID) {
			t.Fatalf("the exemption that was granted is not shown, so it cannot be revoked:\n%s", out)
		}
		for _, want := range []string{"FAILED ROWS", qaPRID2, "refused", "already holds an exemption"} {
			if !strings.Contains(out, want) {
				t.Fatalf("missing %q in the failed-row report:\n%s", want, out)
			}
		}
	})
}

// TestQaExemptShowsEveryExemptionID pins that each row's exemption is named in
// the table: revoking a monorepo exemption is per exemption id.
func TestQaExemptShowsEveryExemptionID(t *testing.T) {
	srv := newQaServer(t)
	srv.monorepoPR = true
	srv.exemptIDs = []string{qaExemptionID, qaExemptionID2}
	h := newQaHarness(t, srv)

	out, errOut, code := h.run("qa", "exempt", "--pr", "acme/widget#42",
		"--reason", "docs only", "--yes")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for _, want := range []string{qaExemptionID, qaExemptionID2} {
		if !strings.Contains(out, want) {
			t.Fatalf("exemption %s is not in the table:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "(2 repository rows)") {
		t.Fatalf("the confirmation does not say how many rows it changed:\n%s", errOut)
	}
}

// TestQaLinkReportsMixedResults keeps a per-row link result: one row can create
// the link while the other finds it already there.
func TestQaLinkReportsMixedResults(t *testing.T) {
	srv := newQaServer(t)
	srv.monorepoPR = true
	srv.linkResults = []string{"created", "existing"}
	h := newQaHarness(t, srv)

	out, errOut, code := h.run("qa", "link", "DT-1", "acme/widget#42", "-o", "json")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	var payload struct {
		PullRequestIDs []string `json:"pull_request_ids"`
		Rows           []struct {
			PullRequestID string `json:"pull_request_id"`
			Result        string `json:"result"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("not JSON: %s\n%s", err, out)
	}
	if len(payload.Rows) != 2 ||
		payload.Rows[0].PullRequestID != qaPRID || payload.Rows[0].Result != "created" ||
		payload.Rows[1].PullRequestID != qaPRID2 || payload.Rows[1].Result != "existing" {
		t.Fatalf("rows = %+v, want one result per row", payload.Rows)
	}
	if len(payload.PullRequestIDs) != 2 {
		t.Fatalf("pull_request_ids = %v, want both rows", payload.PullRequestIDs)
	}

	srv2 := newQaServer(t)
	srv2.monorepoPR = true
	srv2.linkResults = []string{"created", "existing"}
	out, errOut, code = newQaHarness(t, srv2).run("qa", "link", "DT-1", "acme/widget#42")
	if code != cliexit.OK {
		t.Fatalf("table: exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "created, existing") {
		t.Fatalf("the table does not show both results:\n%s", out)
	}
}

// TestQaUUIDNamesOneRow pins the addressing contract: repo#n names the forge PR
// and every one of its rows, while a UUID is the precise address of exactly one
// row — on the queue, off it, and after unlink.
func TestQaUUIDNamesOneRow(t *testing.T) {
	t.Run("on the queue", func(t *testing.T) {
		srv := newQaServer(t)
		srv.monorepoPR = true
		h := newQaHarness(t, srv)

		_, errOut, code := h.run("qa", "exempt", "--pr", qaPRID, "--reason", "docs", "--yes")
		if code != cliexit.OK {
			t.Fatalf("exempt: exit %d\n%s", code, errOut)
		}
		if n := srv.seen("exempt"); n != 1 {
			t.Fatalf("exempt ran %d times, want only the named row", n)
		}
		if got := srv.body("exempt")["pullRequestId"]; got != qaPRID {
			t.Fatalf("pullRequestId = %v, want the named row", got)
		}
		// No queue read was spent, so the confirmation keeps the UUID it was
		// given rather than inventing a repo#n for it.
		if !strings.Contains(errOut, qaPRID) {
			t.Fatalf("the confirmation lost the UUID it was given:\n%s", errOut)
		}

		// unlink already holds the Ticket's card, so the same UUID is named as
		// the repo#n it is on, with no extra request.
		_, errOut, code = h.run("qa", "unlink", "DT-1", qaPRID2, "--yes")
		if code != cliexit.OK {
			t.Fatalf("unlink: exit %d\n%s", code, errOut)
		}
		if !strings.Contains(errOut, "acme/widget#42") {
			t.Fatalf("the UUID confirmation does not name the row as repo#n:\n%s", errOut)
		}
		if n := srv.seen("unlink"); n != 1 {
			t.Fatalf("unlink ran %d times, want only the named row", n)
		}
		if got := srv.body("unlink")["pullRequestId"]; got != qaPRID2 {
			t.Fatalf("unlink pullRequestId = %v, want the named row", got)
		}
	})

	t.Run("off the queue", func(t *testing.T) {
		srv := newQaServer(t)
		h := newQaHarness(t, srv)

		_, errOut, code := h.run("qa", "link", "DT-1", qaPRID2)
		if code != cliexit.OK {
			t.Fatalf("link: exit %d\n%s", code, errOut)
		}
		if n := srv.seen("link"); n != 1 {
			t.Fatalf("link ran %d times, want the named row", n)
		}
		if got := srv.body("link")["pullRequestId"]; got != qaPRID2 {
			t.Fatalf("pullRequestId = %v, want the off-queue row", got)
		}
	})

	t.Run("off-queue UUID never matches a queued row", func(t *testing.T) {
		srv := newQaServer(t)
		srv.monorepoPR = true
		h := newQaHarness(t, srv)

		_, errOut, code := h.run("qa", "link", "DT-1", qaOtherPRID)
		if code != cliexit.OK {
			t.Fatalf("link: exit %d\n%s", code, errOut)
		}
		if n := srv.seen("link"); n != 1 {
			t.Fatalf("link ran %d times, want one row", n)
		}
		if got := srv.body("link")["pullRequestId"]; got != qaOtherPRID {
			t.Fatalf("pullRequestId = %v, want the UUID as given, never a queued row", got)
		}
	})
}

// TestQaShowHidesChangesThatNeedNoLook pins when the CHANGES section is shown:
// the server must flag the session and it must still be open. Its rows are the
// events after the last acknowledgement, and it precedes the verdict sections.
func TestQaShowHidesChangesThatNeedNoLook(t *testing.T) {
	cases := []struct {
		name   string
		events []map[string]any
		setup  func(*qaServer)
	}{
		{"flag cleared", []map[string]any{qaEventJSON("redeploy", 3, nil, nil)},
			func(s *qaServer) { s.changeAcknowledged = true }},
		{"session ended", []map[string]any{qaEventJSON("redeploy", 3, nil, nil)},
			func(s *qaServer) { s.endReason = "abandoned" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newQaServer(t)
			srv.events = tc.events
			if tc.setup != nil {
				tc.setup(srv)
			}
			out, errOut, code := newQaHarness(t, srv).run("qa", "show", "DT-1")
			if code != cliexit.OK {
				t.Fatalf("exit %d\n%s", code, errOut)
			}
			if strings.Contains(out, "CHANGES") {
				t.Fatalf("a change that needs no look was announced:\n%s", out)
			}
		})
	}

	srv := newQaServer(t)
	srv.withVerdicts = true
	srv.events = []map[string]any{
		qaEventJSON("acknowledged", 2, nil, nil),
		qaEventJSON("unverified", 3, nil, nil),
	}
	out, errOut, code := newQaHarness(t, srv).run("qa", "show", "DT-1")
	if code != cliexit.OK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	changes := strings.Index(out, "CHANGES")
	approvals := strings.Index(out, "APPROVALS")
	exemptions := strings.Index(out, "EXEMPTIONS")
	if changes < 0 || approvals < 0 || exemptions < 0 {
		t.Fatalf("a section is missing:\n%s", out)
	}
	if !(changes < approvals && approvals < exemptions) {
		t.Fatalf("sections out of order: CHANGES %d, APPROVALS %d, EXEMPTIONS %d\n%s",
			changes, approvals, exemptions, out)
	}
}

// TestQaConfirmationCountsRepositoryRows pins the row count in the confirmation
// for a monorepo PR's two rows, and that a single-row run says nothing about
// rows at all.
func TestQaConfirmationCountsRepositoryRows(t *testing.T) {
	cases := []struct {
		name string
		mono bool
		want string
	}{
		{"one row", false, ""},
		{"two rows", true, "(2 repository rows)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newQaServer(t)
			srv.monorepoPR = tc.mono
			h := newQaHarness(t, srv)

			for _, args := range [][]string{
				{"qa", "exempt", "--pr", "acme/widget#42", "--reason", "docs", "--yes"},
				{"qa", "unlink", "DT-1", "acme/widget#42", "--yes"},
			} {
				_, errOut, code := h.run(args...)
				if code != cliexit.OK {
					t.Fatalf("%v: exit %d\n%s", args, code, errOut)
				}
				if tc.want == "" {
					if strings.Contains(errOut, "repository row") {
						t.Fatalf("%v: a one-row run announced a row count:\n%s", args, errOut)
					}
					continue
				}
				if !strings.Contains(errOut, tc.want) {
					t.Fatalf("%v: the confirmation does not say %q:\n%s", args, tc.want, errOut)
				}
			}
		})
	}
}

// TestQaRowsCarryTheirOwnGateTarget keeps the gate target per row: a monorepo
// PR's rows bind different repositories, so one top-level target cannot
// describe them all.
func TestQaRowsCarryTheirOwnGateTarget(t *testing.T) {
	type row struct {
		PullRequestID string `json:"pull_request_id"`
		GateTarget    struct {
			PullRequestID string `json:"pull_request_id"`
		} `json:"gate_target"`
	}
	type payload struct {
		Rows []row `json:"rows"`
	}
	check := func(t *testing.T, args ...string) {
		t.Helper()
		out, errOut, code := newQaHarness(t, newQaServerWithMonorepo(t)).run(args...)
		if code != cliexit.OK {
			t.Fatalf("%v: exit %d\n%s", args, code, errOut)
		}
		var p payload
		if err := json.Unmarshal([]byte(out), &p); err != nil {
			t.Fatalf("%v: not JSON: %s\n%s", args, err, out)
		}
		if len(p.Rows) != 2 {
			t.Fatalf("%v: rows = %+v, want one per row", args, p.Rows)
		}
		for _, r := range p.Rows {
			if r.GateTarget.PullRequestID != r.PullRequestID {
				t.Fatalf("%v: row %s carries gate target %q, want its own",
					args, r.PullRequestID, r.GateTarget.PullRequestID)
			}
		}
	}
	check(t, "qa", "exempt", "--pr", "acme/widget#42", "--reason", "docs", "--yes", "-o", "json")
	check(t, "qa", "unlink", "DT-1", "acme/widget#42", "--yes", "-o", "json")
}

func newQaServerWithMonorepo(t *testing.T) *qaServer {
	t.Helper()
	srv := newQaServer(t)
	srv.monorepoPR = true
	srv.exemptIDs = []string{qaExemptionID, qaExemptionID2}
	return srv
}

// TestQaLinkReportsAllRowsFailed pins that a link whose every row the server
// refused still reports each row, leaves partial false, and exits non-zero.
func TestQaLinkReportsAllRowsFailed(t *testing.T) {
	srv := newQaServer(t)
	srv.monorepoPR = true
	srv.linkFailAll = true
	h := newQaHarness(t, srv)

	out, errOut, code := h.run("qa", "link", "DT-1", "acme/widget#42", "-o", "json")
	if code != cliexit.Error {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Error, errOut)
	}
	var payload struct {
		PullRequestIDs []string `json:"pull_request_ids"`
		Rows           []struct {
			PullRequestID string `json:"pull_request_id"`
			Outcome       string `json:"outcome"`
			ExitCode      int    `json:"exit_code"`
			Message       string `json:"message"`
		} `json:"rows"`
		Partial bool `json:"partial"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("not JSON: %s\n%s", err, out)
	}
	if len(payload.Rows) != 2 {
		t.Fatalf("rows = %+v, want both rows attempted", payload.Rows)
	}
	for _, r := range payload.Rows {
		if r.Outcome != "refused" || r.ExitCode != cliexit.Error || r.Message == "" {
			t.Fatalf("row %s = %+v, want a refusal with the server's message", r.PullRequestID, r)
		}
	}
	if payload.Partial {
		t.Fatal("partial = true, want false when no row changed")
	}
	if len(payload.PullRequestIDs) != 0 {
		t.Fatalf("pull_request_ids = %v, want none linked", payload.PullRequestIDs)
	}
}

// TestQaUnionEvaluationsLastWinsPerEpoch pins that the rows' readings collapse
// to one per ticket and epoch, keeping the last row's reading: a later row
// reports the newer state of the same epoch.
func TestQaUnionEvaluationsLastWinsPerEpoch(t *testing.T) {
	completion := &api.QaCompletion{State: api.QaCompletionStateDone}
	first := api.QaCompletionEvaluation{TicketKey: "DT-1", Epoch: 2, Created: completion}
	// A later reading of the same epoch reports the flags but may omit the
	// completion the evaluation created; that reading must not erase it.
	second := api.QaCompletionEvaluation{TicketKey: "DT-1", Epoch: 2, Evaluated: true, Invalidated: true}
	other := api.QaCompletionEvaluation{TicketKey: "DT-1", Epoch: 3, Evaluated: true}

	got := qaUnionEvaluations([][]api.QaCompletionEvaluation{{first}, {second, other}})
	if len(got) != 2 {
		t.Fatalf("got %d evaluations, want one per ticket and epoch: %+v", len(got), got)
	}
	if got[0].Epoch != 2 || !got[0].Evaluated || !got[0].Invalidated {
		t.Fatalf("epoch 2 = %+v, want the last row's reading", got[0])
	}
	if got[0].Created != completion {
		t.Fatalf("epoch 2 created = %+v, want the completion an earlier reading reported", got[0].Created)
	}
	if got[1].Epoch != 3 || !got[1].Evaluated {
		t.Fatalf("epoch 3 = %+v", got[1])
	}
}

// TestQaExemptInterruptReportsUndispatchedRows pins the interrupt contract: the
// row in flight is unknown, nothing after it is sent, and the rows that were
// never dispatched are reported as not_attempted.
func TestQaExemptInterruptReportsUndispatchedRows(t *testing.T) {
	srv := newQaServer(t)
	srv.monorepoPR = true
	srv.thirdRow = true
	srv.exemptTransportAt = 2
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.onExempt = func(n int) {
		if n == 2 {
			cancel()
		}
	}

	out, errOut, code := newQaHarness(t, srv).runContext(ctx,
		"qa", "exempt", "--pr", "acme/widget#42", "--reason", "docs only", "--yes", "-o", "json")
	if code != cliexit.Error {
		t.Fatalf("exit %d, want %d for an interrupted run\n%s", code, cliexit.Error, errOut)
	}
	var payload struct {
		Rows []struct {
			PullRequestID string `json:"pull_request_id"`
			Outcome       string `json:"outcome"`
			Hint          string `json:"hint"`
		} `json:"rows"`
		Partial bool `json:"partial"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("not JSON: %s\n%s", err, out)
	}
	if len(payload.Rows) != 3 {
		t.Fatalf("rows = %+v, want every row reported", payload.Rows)
	}
	want := []string{"ok", "unknown", "not_attempted"}
	for i, w := range want {
		if payload.Rows[i].Outcome != w {
			t.Fatalf("row %d outcome = %q, want %q: %+v", i+1, payload.Rows[i].Outcome, w, payload.Rows)
		}
	}
	if payload.Rows[2].PullRequestID != qaPRID3 {
		t.Fatalf("row 3 = %+v, want the row that was never sent", payload.Rows[2])
	}
	if !strings.Contains(payload.Rows[1].Hint, "DT-1") {
		t.Fatalf("row 2 hint = %q, want the matched Tickets named", payload.Rows[1].Hint)
	}
	if !payload.Partial {
		t.Fatal("partial = false, want true when one row is ok and another is not")
	}
}

// TestQaFailedRowUnknownOnUndecodableSuccess covers the 2xx branch: a success
// whose body carries no row result leaves the row unknown and names how to
// check it.
func TestQaFailedRowUnknownOnUndecodableSuccess(t *testing.T) {
	resp := &api.QaExemptResponse{HTTPResponse: &http.Response{StatusCode: http.StatusOK}}
	row := qaFailedRow(qaRowRef{Field: "pull_request_id", Value: qaPRID},
		resp, (*api.QaExemptResponse429Headers)(nil), "DT-1")
	if row.Outcome != qaRowUnknown {
		t.Fatalf("outcome = %q, want %q for a success without the row's result", row.Outcome, qaRowUnknown)
	}
	if row.Err == nil || row.Err.Code != cliexit.Error {
		t.Fatalf("err = %+v, want the general failure code", row.Err)
	}
	if !strings.Contains(row.Err.Hint, "drift qa show DT-1") {
		t.Fatalf("hint = %q, want the Ticket named", row.Err.Hint)
	}
}

// TestQaUnknownNamesTheTicket pins that a transport failure on link or unlink
// hands the Ticket the command already knows to the hint.
func TestQaUnknownNamesTheTicket(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		setup func(*qaServer)
	}{
		{"link", []string{"qa", "link", "DT-1", "acme/widget#42"},
			func(s *qaServer) { s.linkTransport = true }},
		{"unlink", []string{"qa", "unlink", "DT-1", "acme/widget#42", "--yes"},
			func(s *qaServer) { s.unlinkTransport = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newQaServer(t)
			tc.setup(srv)
			_, errOut, code := newQaHarness(t, srv).run(tc.args...)
			if code != cliexit.Error {
				t.Fatalf("exit %d, want %d\n%s", code, cliexit.Error, errOut)
			}
			if !strings.Contains(errOut, "drift qa show DT-1") {
				t.Fatalf("the unknown row's hint does not name the Ticket:\n%s", errOut)
			}
		})
	}
}

// TestQaShowNotesAFlagWithoutEvents pins that a flagged open session still gets
// a CHANGES row when the timeline has nothing after the last ack: the flag is
// the server's and the timeline can lag it.
func TestQaShowNotesAFlagWithoutEvents(t *testing.T) {
	cases := []struct {
		name   string
		events []map[string]any
	}{
		{"no events", nil},
		{"acknowledged but still flagged", []map[string]any{
			qaEventJSON("redeploy", 3, nil, nil),
			qaEventJSON("acknowledged", 3, nil, nil),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newQaServer(t)
			srv.events = tc.events
			out, errOut, code := newQaHarness(t, srv).run("qa", "show", "DT-1", "-o", "wide")
			if code != cliexit.OK {
				t.Fatalf("exit %d\n%s", code, errOut)
			}
			for _, want := range []string{"CHANGES (unacknowledged)", "unacknowledged", "no event after the last ack"} {
				if !strings.Contains(out, want) {
					t.Fatalf("a flagged session with no unacknowledged event got no notice (missing %q):\n%s", want, out)
				}
			}
		})
	}
}

// TestQaRowWriteErrorsSurface pins that a per-row mutation returns the write
// failure instead of reporting success: a broken stdout must not exit 0.
func TestQaRowWriteErrorsSurface(t *testing.T) {
	for _, args := range [][]string{
		{"qa", "link", "DT-1", "acme/widget#42"},
		{"qa", "unlink", "DT-1", "acme/widget#42", "--yes"},
	} {
		t.Run(args[1], func(t *testing.T) {
			srv := newQaServer(t)
			h := newQaHarness(t, srv)
			broken := &brokenWriter{}
			app := &App{
				Stdout: broken, Stderr: &bytes.Buffer{}, Stdin: strings.NewReader(""),
				Version: h.app.Version, HTTP: h.app.HTTP, store: h.store,
			}
			root := NewRootCommand(app)
			root.SetArgs(args)
			root.SetOut(broken)
			root.SetErr(&bytes.Buffer{})
			if err := root.Execute(); cliexit.CodeOf(err) != cliexit.Error {
				t.Fatalf("exit %d, want %d: the write error must surface",
					cliexit.CodeOf(err), cliexit.Error)
			}
		})
	}
}
