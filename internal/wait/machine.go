// Package wait implements `--wait`: polling a mutation to its outcome, and
// deciding what an observed state MEANS.
//
// The deciding is the hard part, and it is why this package exists separately
// from the commands. drift's states are not a flat list of "good" and "bad"
// names — they are an XState machine, and two of the names that read as
// failures are not failures at all:
//
//	deploying -> deploy_failed -> running
//
// is an expected, documented cycle. ArgoCD reports Degraded during a rollout
// blip, the server records `deploy_failed`, ArgoCD recovers, the server records
// `running`. A wait that treats the name `deploy_failed` as terminal turns a
// cosmetic blip into a red CI job (DESIGN.md §5).
//
// So this package works from the EDGES, not the names. Every rule below —
// what is settled, what can still recover, what can never reach the goal — is
// computed from the transition tables, and the tables are asserted against the
// contract's own enums so a state the server adds cannot be silently ignored.
package wait

import (
	"sort"
	"strings"

	"github.com/steadfast-ly/drift-cli/internal/api"
)

// Origin says who raises an event.
//
// This is the distinction the whole package turns on. A wait can only wait for
// things that happen BY THEMSELVES; an edge that exists but needs a human to
// walk it is, from a poller's point of view, no edge at all. `sleeping` has an
// outgoing WAKE edge to `waking`, but sitting there watching a sleeping
// environment will never see it taken.
type Origin int

const (
	// System events are raised by the server: a build webhook, an ArgoCD health
	// report, the destroy cron. Polling can observe these happen.
	System Origin = iota
	// User events are raised by an operator or an API call. Polling cannot.
	User
)

// edge is one transition.
type edge struct {
	Event  string
	To     api.EnvironmentStatus
	Origin Origin
}

// envMachine is the environment lifecycle, transcribed from the server's
// `src/lib/environment-machine.ts`.
//
// TRANSCRIBED, not fetched: the contract publishes the STATES (as the
// `EnvironmentStatus` enum) but not the edges, and there is no operation that
// serves the machine. That makes this the one place in the CLI where a server
// fact is restated, so it carries two guards — `machine_test.go` asserts the
// state set here is exactly the contract's enum, in both directions, so a state
// added or removed server-side fails the build rather than being silently
// mishandled; and every derived set below is computed, so no second list can
// fall out of step with this one.
//
// The classification of each event as System or User mirrors the server's own
// `USER_EVENTS` list, which is what its `TRANSIENT_STATES` is derived from.
var envMachine = map[api.EnvironmentStatus][]edge{
	api.EnvironmentStatusRequested: {
		{"DISPATCH_SUCCEEDED", api.EnvironmentStatusBuilding, System},
		{"DISPATCH_ALL_FAILED", api.EnvironmentStatusBuildFailed, System},
		{"DESTROY", api.EnvironmentStatusDestroying, User},
	},
	api.EnvironmentStatusBuilding: {
		{"BUILDS_FAILED", api.EnvironmentStatusBuildFailed, System},
		{"BUILDS_COMPLETED", api.EnvironmentStatusDeploying, System},
		{"DEPLOY_FAILED", api.EnvironmentStatusDeployFailed, System},
		{"DESTROY", api.EnvironmentStatusDestroying, User},
		{"CANCEL", api.EnvironmentStatusCanceled, User},
		{"EXTEND", api.EnvironmentStatusBuilding, User},
		{"REBUILD", api.EnvironmentStatusBuilding, User},
	},
	// Recoverable: a build that completes late, or an ArgoCD health report,
	// moves this on without anyone doing anything.
	api.EnvironmentStatusBuildFailed: {
		{"BUILDS_COMPLETED", api.EnvironmentStatusDeploying, System},
		{"ARGOCD_HEALTHY", api.EnvironmentStatusRunning, System},
		{"REBUILD", api.EnvironmentStatusBuilding, User},
		{"DESTROY", api.EnvironmentStatusDestroying, User},
		{"CANCEL", api.EnvironmentStatusCanceled, User},
	},
	api.EnvironmentStatusDeploying: {
		{"ARGOCD_HEALTHY", api.EnvironmentStatusRunning, System},
		{"DEPLOY_FAILED", api.EnvironmentStatusDeployFailed, System},
		{"ARGOCD_DEGRADED", api.EnvironmentStatusDeployFailed, System},
		{"ARGOCD_PROGRESSING", api.EnvironmentStatusDeploying, System},
		{"EXTEND", api.EnvironmentStatusDeploying, User},
		{"DESTROY", api.EnvironmentStatusDestroying, User},
		{"REBUILD", api.EnvironmentStatusBuilding, User},
		{"TOGGLE_VISIBILITY", api.EnvironmentStatusDeploying, User},
	},
	api.EnvironmentStatusRunning: {
		{"REBUILD", api.EnvironmentStatusBuilding, User},
		{"DESTROY", api.EnvironmentStatusDestroying, User},
		{"EXTEND", api.EnvironmentStatusRunning, User},
		{"TOGGLE_VISIBILITY", api.EnvironmentStatusRunning, User},
		{"SLEEP", api.EnvironmentStatusSleeping, User},
	},
	api.EnvironmentStatusSleeping: {
		{"WAKE", api.EnvironmentStatusWaking, User},
		{"REBUILD", api.EnvironmentStatusBuilding, User},
		{"DESTROY", api.EnvironmentStatusDestroying, User},
		{"EXTEND", api.EnvironmentStatusSleeping, User},
	},
	api.EnvironmentStatusWaking: {
		{"ARGOCD_HEALTHY", api.EnvironmentStatusRunning, System},
		{"ARGOCD_DEGRADED", api.EnvironmentStatusDeployFailed, System},
		{"ARGOCD_PROGRESSING", api.EnvironmentStatusWaking, System},
		{"DEPLOY_FAILED", api.EnvironmentStatusDeployFailed, System},
		{"DESTROY", api.EnvironmentStatusDestroying, User},
		{"EXTEND", api.EnvironmentStatusWaking, User},
		{"REBUILD", api.EnvironmentStatusBuilding, User},
	},
	// The blip. ARGOCD_HEALTHY and ARGOCD_PROGRESSING are both system edges out
	// of here, which is exactly why a single observation of this state is not a
	// failure.
	api.EnvironmentStatusDeployFailed: {
		{"ARGOCD_HEALTHY", api.EnvironmentStatusRunning, System},
		{"ARGOCD_PROGRESSING", api.EnvironmentStatusDeploying, System},
		{"REBUILD", api.EnvironmentStatusBuilding, User},
		{"DESTROY", api.EnvironmentStatusDestroying, User},
		{"TOGGLE_VISIBILITY", api.EnvironmentStatusDeployFailed, User},
	},
	api.EnvironmentStatusDestroying: {
		{"CLEANUP_COMPLETE", api.EnvironmentStatusDestroyed, System},
	},
	api.EnvironmentStatusDestroyed: {},
	api.EnvironmentStatusCanceled: {
		{"DESTROY", api.EnvironmentStatusDestroying, User},
	},
}

// Settled reports whether a state changes only when somebody acts on it.
//
// The complement of the server's `TRANSIENT_STATES`, and the closest thing to
// "done" that a poller can honestly assert. `destroyed` has no edges at all;
// `running`, `sleeping` and `canceled` have only user edges. Note what is NOT
// settled: both `_failed` states, because the machine can leave them on its
// own.
func Settled(s api.EnvironmentStatus) bool {
	for _, e := range envMachine[s] {
		if e.Origin == System {
			return false
		}
	}
	return true
}

// Failure reports whether a state is a failure of the lifecycle.
//
// The suffix rule is the server's own (`FAILED_STATES` is
// `ENVIRONMENT_STATUSES.filter((s) => s.endsWith("_failed"))`), mirrored rather
// than re-derived so the two cannot disagree about a state added later. What
// the CLI does with the answer is the part that matters, and it is not "stop":
// a failure state that is not `Settled` can still recover, so `Waiter` requires
// it to persist before believing it.
func Failure(s api.EnvironmentStatus) bool {
	return strings.HasSuffix(string(s), "_failed")
}

// Reachable reports whether `goal` can be reached from `from` by system events
// alone — that is, whether waiting can possibly work.
//
// Breadth-first over system edges only. This is what turns "wait for `running`
// on an environment somebody just canceled" from a fifteen-minute timeout into
// an immediate, accurate refusal: `canceled`'s only exit is a user DESTROY, so
// no amount of waiting reaches `running`.
func Reachable(from, goal api.EnvironmentStatus) bool {
	if from == goal {
		return true
	}
	seen := map[api.EnvironmentStatus]bool{from: true}
	queue := []api.EnvironmentStatus{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range envMachine[cur] {
			if e.Origin != System || seen[e.To] {
				continue
			}
			if e.To == goal {
				return true
			}
			seen[e.To] = true
			queue = append(queue, e.To)
		}
	}
	return false
}

// CommandOnly reports whether a state can only ever be ENTERED by an operator
// event — that no server-raised edge anywhere targets it.
//
// True of `sleeping` and `canceled`: the only ways in are SLEEP and CANCEL. The
// consequence for waiting is the opposite of what a reachability test suggests.
// Because no system edge produces such a state, `Reachable` says it is
// unreachable from everywhere, and a wait that treated that as proof would
// refuse every `sleep --wait` and `cancel --wait` — including the ones whose
// command the server has already accepted. Absence of a system path says
// nothing about whether a command is on its way, so it must not be read as
// evidence.
func CommandOnly(goal api.EnvironmentStatus) bool {
	for _, edges := range envMachine {
		for _, e := range edges {
			if e.Origin == System && e.To == goal {
				return false
			}
		}
	}
	return true
}

// WaitableStates lists the states `--for` accepts: every state some system
// event leads to, plus the settled states a mutation puts an environment in.
//
// Derived rather than listed so that `drift env wait --for <typo>` names the
// real alternatives, and so a contract change moves this automatically.
func WaitableStates() []string {
	out := make([]string, 0, len(envMachine))
	for s := range envMachine {
		out = append(out, string(s))
	}
	sort.Strings(out)
	return out
}

// ParseState validates a `--for` value against the contract's enum.
func ParseState(s string) (api.EnvironmentStatus, bool) {
	v := api.EnvironmentStatus(strings.TrimSpace(s))
	if !v.Valid() {
		return "", false
	}
	return v, true
}

// --- builds -----------------------------------------------------------------

// buildMachine is the build lifecycle from the server's
// `src/lib/build-machine.ts`. Only the edges matter here, not their targets:
// the single question the CLI asks of a build is whether its outcome is still
// open.
var buildMachine = map[api.EnvironmentBuildStatus][]edge{
	api.EnvironmentBuildStatusPending: {
		{Event: "DISPATCHED", Origin: System}, {Event: "SKIPPED", Origin: System},
		{Event: "DISPATCH_FAILED", Origin: System}, {Event: "CANCELED", Origin: System},
	},
	api.EnvironmentBuildStatusDispatched: {
		{Event: "QUEUED", Origin: System}, {Event: "STARTED", Origin: System},
		{Event: "COMPLETED", Origin: System}, {Event: "FAILED", Origin: System},
		{Event: "CANCELED", Origin: System},
	},
	api.EnvironmentBuildStatusQueued: {
		{Event: "STARTED", Origin: System}, {Event: "COMPLETED", Origin: System},
		{Event: "FAILED", Origin: System}, {Event: "CANCELED", Origin: System},
	},
	api.EnvironmentBuildStatusInProgress: {
		{Event: "COMPLETED", Origin: System}, {Event: "FAILED", Origin: System},
		{Event: "CANCELED", Origin: System},
	},
	api.EnvironmentBuildStatusCompleted: {},
	api.EnvironmentBuildStatusFailed: {
		{Event: "RETRIED", Origin: User},
	},
	api.EnvironmentBuildStatusCanceled: {
		{Event: "RETRIED", Origin: User},
	},
	api.EnvironmentBuildStatusSkipped: {},
}

// BuildInFlight reports whether a build's outcome is still open.
//
// The server calls the complement `RESOLVED_BUILD_STATUSES` and derives it the
// same way: a status with no events, or only the user-initiated RETRIED, is
// settled. An in-flight build is the CLI's evidence that a `build_failed` or
// `deploy_failed` environment is actively being recovered, which is why it
// resets the failure count rather than merely being displayed.
func BuildInFlight(s api.EnvironmentBuildStatus) bool {
	for _, e := range buildMachine[s] {
		if e.Origin == System {
			return true
		}
	}
	return false
}

// --- promotions -------------------------------------------------------------

// promotionMachine is the promotion lifecycle from the server's
// `src/lib/promotion-machine.ts`.
//
// The contrast with the environment machine is the whole reason it is written
// out. Here `failed` and `deploy_failed` are declared `final` — they have no
// outgoing edges at all, system or otherwise. A promotion that reports
// `deploy_failed` is not blipping, it has finished badly, and applying the
// environment's sustained-failure rule to it would make every failed promotion
// take N extra polls to report for no reason.
var promotionMachine = map[api.PromotionStatus][]string{
	api.PromotionStatusDispatched:   {"WORKFLOW_STARTED", "WORKFLOW_FAILED"},
	api.PromotionStatusPromoting:    {"ALL_WORKFLOWS_COMPLETED", "WORKFLOW_FAILED"},
	api.PromotionStatusDeploying:    {"ARGOCD_HEALTHY", "ARGOCD_DEGRADED", "ARGOCD_PROGRESSING"},
	api.PromotionStatusCompleted:    {},
	api.PromotionStatusFailed:       {},
	api.PromotionStatusDeployFailed: {},
}

// PromotionTerminal reports whether a promotion has finished, either way.
// Every event in the promotion machine is server-raised, so "no outgoing edges"
// and "nothing more will happen" are the same statement.
func PromotionTerminal(s api.PromotionStatus) bool {
	return len(promotionMachine[s]) == 0
}

// PromotionFailed reports whether a finished promotion finished badly.
// Same suffix-and-name rule the server uses; safe here in a way it is not for
// environments precisely because these states are final.
func PromotionFailed(s api.PromotionStatus) bool {
	return s == api.PromotionStatusFailed || s == api.PromotionStatusDeployFailed
}
