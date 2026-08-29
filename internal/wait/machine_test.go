package wait

import (
	"sort"
	"testing"

	"github.com/steadfast-ly/drift-cli/internal/api"
	"github.com/steadfast-ly/drift-cli/spec"
)

// The transition tables restate a server fact — the contract publishes states
// but not edges. These three tests are what keep the restatement honest: the
// state SET is compared against the vendored contract in both directions, so a
// state the server adds or removes fails here rather than being silently
// treated as "no outgoing edges", which every derived rule would then read as
// settled.
func TestEnvironmentMachineCoversTheContractsStates(t *testing.T) {
	got := make([]string, 0, len(envMachine))
	for s := range envMachine {
		got = append(got, string(s))
		if !s.Valid() {
			t.Errorf("state %q is not in the generated enum", s)
		}
	}
	sort.Strings(got)
	assertSameStates(t, "Environment.status", spec.SchemaEnum("Environment", "status"), got)
}

func TestBuildMachineCoversTheContractsStates(t *testing.T) {
	got := make([]string, 0, len(buildMachine))
	for s := range buildMachine {
		got = append(got, string(s))
		if !s.Valid() {
			t.Errorf("build status %q is not in the generated enum", s)
		}
	}
	sort.Strings(got)
	assertSameStates(t, "EnvironmentBuild.status", spec.SchemaEnum("EnvironmentBuild", "status"), got)
}

func TestPromotionMachineCoversTheContractsStates(t *testing.T) {
	got := make([]string, 0, len(promotionMachine))
	for s := range promotionMachine {
		got = append(got, string(s))
		if !s.Valid() {
			t.Errorf("promotion status %q is not in the generated enum", s)
		}
	}
	sort.Strings(got)
	assertSameStates(t, "Promotion.status", spec.SchemaEnum("Promotion", "status"), got)
}

func assertSameStates(t *testing.T, what string, want, got []string) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("the vendored spec declares no enum for %s", what)
	}
	if len(want) != len(got) {
		t.Fatalf("%s: machine has %v, contract has %v", what, got, want)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("%s: machine has %v, contract has %v", what, got, want)
		}
	}
}

// THE test. `deploy_failed` reads like a terminal failure and is not one: the
// machine has ARGOCD_HEALTHY and ARGOCD_PROGRESSING leading out of it, both
// server-raised, which is what makes `deploying -> deploy_failed -> running` an
// expected cycle rather than a broken deployment. If this ever inverts, every
// `--wait` in CI starts failing on a cosmetic ArgoCD blip.
func TestFailureStatesAreNotSettled(t *testing.T) {
	for _, s := range []api.EnvironmentStatus{
		api.EnvironmentStatusDeployFailed, api.EnvironmentStatusBuildFailed,
	} {
		if !Failure(s) {
			t.Errorf("%s should be classified a failure", s)
		}
		if Settled(s) {
			t.Errorf("%s is settled, so a wait would treat one observation as final", s)
		}
		if !Reachable(s, api.EnvironmentStatusRunning) {
			t.Errorf("%s can no longer recover to running", s)
		}
	}
}

func TestSettledStatesAreExactlyTheOnesNothingMoves(t *testing.T) {
	want := map[api.EnvironmentStatus]bool{
		api.EnvironmentStatusRunning:   true,
		api.EnvironmentStatusSleeping:  true,
		api.EnvironmentStatusCanceled:  true,
		api.EnvironmentStatusDestroyed: true,
	}
	for s := range envMachine {
		if Settled(s) != want[s] {
			t.Errorf("Settled(%s) = %v, want %v", s, Settled(s), want[s])
		}
	}
}

// Reachability over SYSTEM edges only is what lets a wait refuse instead of
// timing out. `sleeping` has a WAKE edge to `waking` and `canceled` has a
// DESTROY edge, but both need a command; nobody watching will ever see them
// taken.
func TestReachabilityIgnoresEdgesOnlyAUserCanTake(t *testing.T) {
	cases := []struct {
		from, goal api.EnvironmentStatus
		want       bool
	}{
		{api.EnvironmentStatusRequested, api.EnvironmentStatusRunning, true},
		{api.EnvironmentStatusBuilding, api.EnvironmentStatusRunning, true},
		{api.EnvironmentStatusDeployFailed, api.EnvironmentStatusRunning, true},
		{api.EnvironmentStatusWaking, api.EnvironmentStatusRunning, true},
		{api.EnvironmentStatusDestroying, api.EnvironmentStatusDestroyed, true},

		{api.EnvironmentStatusSleeping, api.EnvironmentStatusRunning, false},
		{api.EnvironmentStatusCanceled, api.EnvironmentStatusRunning, false},
		{api.EnvironmentStatusDestroyed, api.EnvironmentStatusRunning, false},
		{api.EnvironmentStatusRunning, api.EnvironmentStatusDestroyed, false},
		{api.EnvironmentStatusRunning, api.EnvironmentStatusSleeping, false},
	}
	for _, c := range cases {
		if got := Reachable(c.from, c.goal); got != c.want {
			t.Errorf("Reachable(%s, %s) = %v, want %v", c.from, c.goal, got, c.want)
		}
	}
}

func TestBuildInFlightIsOpenOutcomeNotOpenState(t *testing.T) {
	open := map[api.EnvironmentBuildStatus]bool{
		api.EnvironmentBuildStatusPending:    true,
		api.EnvironmentBuildStatusDispatched: true,
		api.EnvironmentBuildStatusQueued:     true,
		api.EnvironmentBuildStatusInProgress: true,
		// `failed` and `canceled` accept RETRIED, so they have an outgoing edge
		// — but taking it needs `drift env retry-build`, so the outcome is
		// settled and the build is not in flight.
		api.EnvironmentBuildStatusFailed:    false,
		api.EnvironmentBuildStatusCanceled:  false,
		api.EnvironmentBuildStatusCompleted: false,
		api.EnvironmentBuildStatusSkipped:   false,
	}
	for s, want := range open {
		if got := BuildInFlight(s); got != want {
			t.Errorf("BuildInFlight(%s) = %v, want %v", s, got, want)
		}
	}
}

// The promotion machine's failure states ARE final, which is why the promotion
// wait does not apply the environment's sustained-failure rule. If a recovery
// edge ever appears here, that decision has to be revisited.
func TestPromotionFailureStatesAreFinal(t *testing.T) {
	for _, s := range []api.PromotionStatus{
		api.PromotionStatusFailed, api.PromotionStatusDeployFailed, api.PromotionStatusCompleted,
	} {
		if !PromotionTerminal(s) {
			t.Errorf("%s is not terminal, so the promotion wait's no-streak rule is unsound", s)
		}
	}
	for _, s := range []api.PromotionStatus{
		api.PromotionStatusDispatched, api.PromotionStatusPromoting, api.PromotionStatusDeploying,
	} {
		if PromotionTerminal(s) {
			t.Errorf("%s is terminal, so a promotion wait would stop early", s)
		}
	}
	if !PromotionFailed(api.PromotionStatusDeployFailed) || PromotionFailed(api.PromotionStatusCompleted) {
		t.Error("promotion failure classification is wrong")
	}
}
