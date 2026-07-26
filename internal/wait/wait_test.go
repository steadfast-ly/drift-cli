package wait

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steadfast/drift-cli/internal/api"
	"github.com/steadfast/drift-cli/internal/client"
	"github.com/steadfast/drift-cli/internal/cliexit"
)

// fakeClock advances only when the wait sleeps, so a test covering half an hour
// of polling runs in microseconds and does so deterministically — no real
// timers, nothing to flake under -race or on a loaded CI box.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.t = c.t.Add(d)
	return nil
}

// script drives a wait through a fixed sequence of observations, repeating the
// last one forever so a test only has to write the interesting prefix.
type script struct {
	obs    []Observation
	errs   []error
	calls  int
	cancel func()
}

func (s *script) poll(context.Context) (Observation, error) {
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return Observation{}, s.errs[i]
	}
	if i >= len(s.obs) {
		i = len(s.obs) - 1
	}
	return s.obs[i], nil
}

func running() Observation  { return Observation{Status: api.EnvironmentStatusRunning} }
func building() Observation { return Observation{Status: api.EnvironmentStatusBuilding} }
func deployFailed() Observation {
	return Observation{Status: api.EnvironmentStatusDeployFailed}
}

func newOpts(goal api.EnvironmentStatus, c *fakeClock) Options {
	return Options{
		Goal:     goal,
		Timeout:  30 * time.Minute,
		Interval: 3 * time.Second,
		Ref:      "proof-alpha",
		now:      c.now,
		sleep:    c.sleep,
	}
}

func TestReachingTheGoalSucceeds(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	s := &script{obs: []Observation{
		{Status: api.EnvironmentStatusRequested},
		building(), {Status: api.EnvironmentStatusDeploying}, running(),
	}}
	got, err := Wait(context.Background(), newOpts(api.EnvironmentStatusRunning, c), s.poll)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != api.EnvironmentStatusRunning {
		t.Fatalf("final state %s", got)
	}
	if s.calls != 4 {
		t.Fatalf("polled %d times, want 4", s.calls)
	}
}

// THE case that will bite in CI. `deploying -> deploy_failed -> running` is an
// expected ArgoCD blip, documented server-side as a cosmetic cycle. A wait that
// gave up on the first `deploy_failed` would turn it into a red build.
func TestTransientDeployFailedDoesNotFail(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	s := &script{obs: []Observation{
		{Status: api.EnvironmentStatusDeploying},
		deployFailed(), // the blip: one poll, then ArgoCD recovers
		{Status: api.EnvironmentStatusDeploying},
		running(),
	}}
	got, err := Wait(context.Background(), newOpts(api.EnvironmentStatusRunning, c), s.poll)
	if err != nil {
		t.Fatalf("a single deploy_failed was treated as terminal: %v", err)
	}
	if got != api.EnvironmentStatusRunning {
		t.Fatalf("final state %s, want running", got)
	}
}

// The failure rule is measured on the CLOCK, not in polls. A blip that clears
// well inside the window is not a failure however many times it was sampled —
// which is the property a poll count cannot express: three polls at three
// seconds is six seconds of tolerance, not the ten it reads like.
func TestFailureShorterThanTheWindowDoesNotFail(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	// Nine polls of deploy_failed at 3s spans 24 seconds — eight of them, and
	// well short of the 30-second window.
	obs := make([]Observation, 0, 10)
	for i := 0; i < 9; i++ {
		obs = append(obs, deployFailed())
	}
	obs = append(obs, running())
	s := &script{obs: obs}
	opts := newOpts(api.EnvironmentStatusRunning, c)
	if _, err := Wait(context.Background(), opts, s.poll); err != nil {
		t.Fatalf("a blip inside the window was treated as terminal: %v", err)
	}
}

// Shortening the poll interval must NOT shorten the tolerance. Under a poll
// count it silently did.
func TestToleranceIsIndependentOfThePollInterval(t *testing.T) {
	for _, interval := range []time.Duration{time.Second, 3 * time.Second, 10 * time.Second} {
		c := &fakeClock{t: time.Unix(0, 0)}
		s := &script{obs: []Observation{deployFailed()}} // forever
		opts := newOpts(api.EnvironmentStatusRunning, c)
		opts.Interval = interval
		if _, err := Wait(context.Background(), opts, s.poll); cliexit.CodeOf(err) != cliexit.Conflict {
			t.Fatalf("interval %s: exit %d", interval, cliexit.CodeOf(err))
		}
		held := c.t.Sub(time.Unix(0, 0))
		if held < DefaultFailureWindow {
			t.Fatalf("interval %s: gave up after %s, less than the %s window",
				interval, held, DefaultFailureWindow)
		}
	}
}

func TestSustainedFailureIsAFailure(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	s := &script{obs: []Observation{deployFailed()}} // forever
	opts := newOpts(api.EnvironmentStatusRunning, c)
	_, err := Wait(context.Background(), opts, s.poll)
	if cliexit.CodeOf(err) != cliexit.Conflict {
		t.Fatalf("exit %d, want %d (%v)", cliexit.CodeOf(err), cliexit.Conflict, err)
	}
	var ee *cliexit.ExitError
	if !errors.As(err, &ee) || !strings.Contains(ee.Detail, "held that state for 30s") {
		t.Fatalf("the message does not state the evidence: %v", err)
	}
	// It must report what it SAW, not a conclusion it cannot support: recovery
	// arrives on a webhook whose latency this client does not control.
	if strings.Contains(ee.Detail, "is not a transient") {
		t.Fatalf("the message asserts more than the observation carries: %v", ee.Detail)
	}
}

// A failure state with a build still running is recovery in progress, not a
// failure — so the window restarts rather than merely pausing.
func TestInFlightBuildResetsTheFailureWindow(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	recovering := Observation{Status: api.EnvironmentStatusBuildFailed, BuildInFlight: true}
	obs := []Observation{{Status: api.EnvironmentStatusBuildFailed}}
	for i := 0; i < 20; i++ { // 60s, twice the window
		obs = append(obs, recovering)
	}
	obs = append(obs, running())
	s := &script{obs: obs}
	if _, err := Wait(context.Background(), newOpts(api.EnvironmentStatusRunning, c), s.poll); err != nil {
		t.Fatalf("a retry in flight was reported as a failure: %v", err)
	}
}

// Alternating reasons must not add up: polls saying "failed" and polls saying
// "unreachable" are not one sustained observation of anything.
func TestTheWindowRestartsWhenTheReasonChanges(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	obs := []Observation{}
	for i := 0; i < 9; i++ {
		obs = append(obs, deployFailed())
	}
	obs = append(obs, Observation{Status: api.EnvironmentStatusSleeping}) // different reason
	for i := 0; i < 9; i++ {
		obs = append(obs, deployFailed())
	}
	obs = append(obs, running())
	s := &script{obs: obs}
	// Not commanded, so `sleeping` is judged futile — but it takes three
	// consecutive polls to say so and there is only one.
	if _, err := Wait(context.Background(), newOpts(api.EnvironmentStatusRunning, c), s.poll); err != nil {
		t.Fatalf("mixed reasons were counted as one observation: %v", err)
	}
}

// An environment that can never reach the goal fails fast instead of burning
// the whole timeout — but still only after the streak, because a mutation's
// write and the first poll can race.
func TestUnreachableGoalFailsWithoutWaitingOutTheTimeout(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	s := &script{obs: []Observation{{Status: api.EnvironmentStatusCanceled}}}
	opts := newOpts(api.EnvironmentStatusRunning, c)
	opts.FailureStreak = 3
	_, err := Wait(context.Background(), opts, s.poll)
	if cliexit.CodeOf(err) != cliexit.Conflict {
		t.Fatalf("exit %d, want %d", cliexit.CodeOf(err), cliexit.Conflict)
	}
	if s.calls != 3 {
		t.Fatalf("polled %d times, want 3", s.calls)
	}
	if elapsed := c.t.Sub(time.Unix(0, 0)); elapsed > time.Minute {
		t.Fatalf("waited %s before refusing", elapsed)
	}
}

// The race the streak protects against: `env rm --wait` reads `running` once
// before the destroy lands. One stale read must not abort the wait.
func TestOneStaleReadBeforeAMutationLandsIsTolerated(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	s := &script{obs: []Observation{
		running(), // the write has not landed yet
		{Status: api.EnvironmentStatusDestroying},
		{Status: api.EnvironmentStatusDestroyed},
	}}
	opts := newOpts(api.EnvironmentStatusDestroyed, c)
	if _, err := Wait(context.Background(), opts, s.poll); err != nil {
		t.Fatalf("a stale pre-destroy read aborted the wait: %v", err)
	}
}

func TestTimeoutIsExitSix(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	s := &script{obs: []Observation{building()}} // never progresses
	opts := newOpts(api.EnvironmentStatusRunning, c)
	opts.Timeout = 30 * time.Second
	_, err := Wait(context.Background(), opts, s.poll)
	if cliexit.CodeOf(err) != cliexit.WaitTimeout {
		t.Fatalf("exit %d, want %d (%v)", cliexit.CodeOf(err), cliexit.WaitTimeout, err)
	}
	var ee *cliexit.ExitError
	if !errors.As(err, &ee) || !strings.Contains(ee.Detail, "building") {
		t.Fatalf("the timeout does not report the last state seen: %v", err)
	}
	// It must not overshoot the deadline: the last sleep is skipped rather than
	// taken and then noticed.
	if c.t.Sub(time.Unix(0, 0)) > 30*time.Second {
		t.Fatalf("overshot the deadline by %s", c.t.Sub(time.Unix(0, 0))-30*time.Second)
	}
}

// A 429 during a wait is the server asking for time, not a failure of the
// environment. It backs off by the server's own number.
func TestRateLimitBacksOffByRetryAfterAndContinues(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	limited := &cliexit.ExitError{
		Code: cliexit.RateLimited, Message: "Rate limit exceeded", RetryAfter: 42 * time.Second,
	}
	s := &script{
		errs: []error{nil, limited, nil},
		obs:  []Observation{building(), {}, running()},
	}
	rec := &recordingReporter{}
	opts := newOpts(api.EnvironmentStatusRunning, c)
	opts.Reporter = rec
	if _, err := Wait(context.Background(), opts, s.poll); err != nil {
		t.Fatalf("a 429 aborted the wait: %v", err)
	}
	// One poll interval plus the server's 42 seconds, and nothing else.
	if got := c.t.Sub(time.Unix(0, 0)); got != 45*time.Second {
		t.Fatalf("elapsed %s, want 45s (3s interval + 42s Retry-After)", got)
	}
	if len(rec.throttled) != 1 || rec.throttled[0] != 42*time.Second {
		t.Fatalf("the backoff was not reported: %v", rec.throttled)
	}
}

// A wait that would sleep past its own deadline waiting out a rate limit ends
// as a timeout rather than blowing through it.
func TestRateLimitBeyondTheDeadlineIsATimeout(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	limited := &cliexit.ExitError{Code: cliexit.RateLimited, RetryAfter: 10 * time.Minute}
	s := &script{errs: []error{limited}, obs: []Observation{running()}}
	opts := newOpts(api.EnvironmentStatusRunning, c)
	opts.Timeout = time.Minute
	_, err := Wait(context.Background(), opts, s.poll)
	if cliexit.CodeOf(err) != cliexit.WaitTimeout {
		t.Fatalf("exit %d, want %d", cliexit.CodeOf(err), cliexit.WaitTimeout)
	}
}

// A `Retry-After` SHORTER than the poll interval — including the zero left by a
// header that was missing, negative, or too large to be meaningful — must never
// make the client come back sooner than it would have anyway. Accelerating in
// response to "you are sending too many requests" is the worst possible
// reading of a rate limit.
func TestBackoffNeverPollsFasterThanTheInterval(t *testing.T) {
	for _, retryAfter := range []time.Duration{0, time.Nanosecond, time.Second, -5 * time.Second} {
		c := &fakeClock{t: time.Unix(0, 0)}
		limited := &cliexit.ExitError{Code: cliexit.RateLimited, RetryAfter: retryAfter}
		s := &script{errs: []error{limited}, obs: []Observation{running()}}
		rec := &recordingReporter{}
		opts := newOpts(api.EnvironmentStatusRunning, c)
		opts.Reporter = rec
		if _, err := Wait(context.Background(), opts, s.poll); err != nil {
			t.Fatalf("Retry-After %s: %v", retryAfter, err)
		}
		if len(rec.throttled) != 1 || rec.throttled[0] < opts.Interval {
			t.Fatalf("Retry-After %s backed off %v, less than the %s poll interval",
				retryAfter, rec.throttled, opts.Interval)
		}
		if got := c.t.Sub(time.Unix(0, 0)); got < opts.Interval {
			t.Fatalf("Retry-After %s: elapsed %s, less than one poll interval", retryAfter, got)
		}
	}
}

// The overflow that produced the acceleration in the first place: `Retry-After:
// 18446744074` seconds wraps `time.Duration` to 0.29s. It must be rejected at
// the boundary, not multiplied.
func TestAbsurdRetryAfterIsIgnoredRatherThanWrapped(t *testing.T) {
	for _, secs := range []int{1 << 40, 18446744074, -1} {
		var h struct{ RetryAfter int }
		h.RetryAfter = secs
		if got := clientRetryAfter(&h); got != 0 {
			t.Fatalf("Retry-After %d yielded %s, want 0", secs, got)
		}
	}
	// A value inside the cap still comes through untouched.
	var ok struct{ RetryAfter int }
	ok.RetryAfter = 42
	if got := clientRetryAfter(&ok); got != 42*time.Second {
		t.Fatalf("Retry-After 42 yielded %s", got)
	}
}

// `sleeping` and `canceled` have NO inbound system edge — the only ways in are
// the SLEEP and CANCEL commands. A wait for one of them can therefore never be
// declared futile from the machine, whether or not this process issued the
// command, because the machine has no opinion about commands in flight.
func TestCommandOnlyGoalsAreNeverDeclaredFutile(t *testing.T) {
	for _, goal := range []api.EnvironmentStatus{
		api.EnvironmentStatusSleeping, api.EnvironmentStatusCanceled,
	} {
		if !CommandOnly(goal) {
			t.Fatalf("%s should be command-only", goal)
		}
		for _, commanded := range []bool{true, false} {
			c := &fakeClock{t: time.Unix(0, 0)}
			s := &script{obs: []Observation{running()}} // never reaches the goal
			opts := newOpts(goal, c)
			opts.Commanded = commanded
			opts.Timeout = time.Minute
			_, err := Wait(context.Background(), opts, s.poll)
			if cliexit.CodeOf(err) != cliexit.WaitTimeout {
				t.Fatalf("goal %s commanded=%v: exit %d, want %d (%v)",
					goal, commanded, cliexit.CodeOf(err), cliexit.WaitTimeout, err)
			}
		}
	}
	// Not command-only: `running` and `destroyed` both have system edges into
	// them, so the futility argument still applies to them.
	for _, goal := range []api.EnvironmentStatus{
		api.EnvironmentStatusRunning, api.EnvironmentStatusDestroyed,
	} {
		if CommandOnly(goal) {
			t.Fatalf("%s should not be command-only", goal)
		}
	}
}

// A command this invocation issued excuses a state the machine cannot leave
// unaided: the write may simply not be visible yet.
func TestACommandedGoalIsNotDeclaredFutile(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	s := &script{obs: []Observation{running()}} // the destroy never becomes visible
	opts := newOpts(api.EnvironmentStatusDestroyed, c)
	opts.Commanded = true
	opts.Timeout = time.Minute
	if _, err := Wait(context.Background(), opts, s.poll); cliexit.CodeOf(err) != cliexit.WaitTimeout {
		t.Fatalf("exit %d, want %d", cliexit.CodeOf(err), cliexit.WaitTimeout)
	}
	// ...but with nothing commanded, the same observation is worth refusing.
	c2 := &fakeClock{t: time.Unix(0, 0)}
	s2 := &script{obs: []Observation{running()}}
	opts2 := newOpts(api.EnvironmentStatusDestroyed, c2)
	opts2.Timeout = time.Minute
	if _, err := Wait(context.Background(), opts2, s2.poll); cliexit.CodeOf(err) != cliexit.Conflict {
		t.Fatalf("exit %d, want %d", cliexit.CodeOf(err), cliexit.Conflict)
	}
}

// An absorbing state ends the wait whatever was commanded: nothing will ever
// happen again.
func TestAbsorbingStateIsAlwaysFutile(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	s := &script{obs: []Observation{{Status: api.EnvironmentStatusDestroyed}}}
	opts := newOpts(api.EnvironmentStatusRunning, c)
	opts.Commanded = true
	opts.Timeout = time.Hour
	if _, err := Wait(context.Background(), opts, s.poll); cliexit.CodeOf(err) != cliexit.Conflict {
		t.Fatalf("exit %d, want %d", cliexit.CodeOf(err), cliexit.Conflict)
	}
}

// A state that is not in the contract this build was generated against gets no
// reasoning at all. Both `Settled` and `Reachable` would answer from an empty
// edge list and the answers would be fabrications.
func TestUnknownStateIsSkewNotAConflict(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	s := &script{obs: []Observation{{Status: api.EnvironmentStatus("hibernating")}}}
	opts := newOpts(api.EnvironmentStatusRunning, c)
	_, err := Wait(context.Background(), opts, s.poll)
	if cliexit.CodeOf(err) != cliexit.Error {
		t.Fatalf("exit %d, want %d (%v)", cliexit.CodeOf(err), cliexit.Error, err)
	}
	var ee *cliexit.ExitError
	if !errors.As(err, &ee) || !strings.Contains(ee.Message, "does not know") {
		t.Fatalf("skew was not named: %v", err)
	}
	if s.calls != 1 {
		t.Fatalf("polled %d times; an unknown state should stop at once", s.calls)
	}
}

// Any other error aborts, unchanged: a 404 while waiting means the environment
// went away, and retrying would only produce the same 404 more slowly.
func TestNonRateLimitErrorsAbort(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	boom := &cliexit.ExitError{Code: cliexit.NotFound, Message: "Environment not found"}
	s := &script{errs: []error{boom}, obs: []Observation{running()}}
	_, err := Wait(context.Background(), newOpts(api.EnvironmentStatusRunning, c), s.poll)
	if cliexit.CodeOf(err) != cliexit.NotFound {
		t.Fatalf("exit %d, want %d", cliexit.CodeOf(err), cliexit.NotFound)
	}
}

func TestCancellationIsNotATimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &fakeClock{t: time.Unix(0, 0)}
	s := &script{obs: []Observation{building()}}
	opts := newOpts(api.EnvironmentStatusRunning, c)
	opts.sleep = func(context.Context, time.Duration) error {
		cancel()
		return ctx.Err()
	}
	_, err := Wait(ctx, opts, s.poll)
	if cliexit.CodeOf(err) != cliexit.Error {
		t.Fatalf("exit %d, want %d (%v)", cliexit.CodeOf(err), cliexit.Error, err)
	}
}

// --- promotions -------------------------------------------------------------

func TestPromotionFailureIsImmediateNotSustained(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	calls := 0
	poll := func(context.Context) (PromotionObservation, error) {
		calls++
		return PromotionObservation{
			Status: api.PromotionStatusDeployFailed, Message: "argocd reported Degraded",
		}, nil
	}
	_, err := WaitPromotion(context.Background(), PromotionOptions{
		Timeout: 20 * time.Minute, Interval: 3 * time.Second, Ref: "abc",
		now: c.now, sleep: c.sleep,
	}, poll)
	if cliexit.CodeOf(err) != cliexit.Conflict {
		t.Fatalf("exit %d, want %d", cliexit.CodeOf(err), cliexit.Conflict)
	}
	// The promotion machine declares deploy_failed final, so one observation is
	// enough — unlike an environment, where it is a recoverable blip.
	if calls != 1 {
		t.Fatalf("polled %d times, want 1", calls)
	}
	var ee *cliexit.ExitError
	if !errors.As(err, &ee) || ee.Detail != "argocd reported Degraded" {
		t.Fatalf("the server's statusMessage was dropped: %v", err)
	}
}

func TestPromotionCompletes(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	seq := []api.PromotionStatus{
		api.PromotionStatusDispatched, api.PromotionStatusPromoting,
		api.PromotionStatusDeploying, api.PromotionStatusCompleted,
	}
	i := 0
	poll := func(context.Context) (PromotionObservation, error) {
		s := seq[min(i, len(seq)-1)]
		i++
		return PromotionObservation{Status: s}, nil
	}
	got, err := WaitPromotion(context.Background(), PromotionOptions{
		Timeout: 20 * time.Minute, Interval: 3 * time.Second, now: c.now, sleep: c.sleep,
	}, poll)
	if err != nil || got != api.PromotionStatusCompleted {
		t.Fatalf("got %s, err %v", got, err)
	}
}

// --- progress ---------------------------------------------------------------

// Off a terminal, one line per state CHANGE — not one per poll. A fifteen
// minute wait polls three hundred times; a CI log wants the transitions.
func TestProgressOffTerminalPrintsOnlyChanges(t *testing.T) {
	var buf bytes.Buffer
	p := NewProgress(&buf, false)
	p.Observed(api.EnvironmentStatusBuilding, 0)
	p.Observed(api.EnvironmentStatusBuilding, 3*time.Second)
	p.Observed(api.EnvironmentStatusBuilding, 6*time.Second)
	p.Observed(api.EnvironmentStatusDeploying, 9*time.Second)
	p.Observed(api.EnvironmentStatusRunning, 12*time.Second)
	p.Stop()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	want := []string{"building (0s)", "deploying (9s)", "running (12s)"}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines %q, want %d", len(lines), lines, len(want))
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
	// No ANSI, no carriage returns: this is a log file.
	if strings.ContainsAny(buf.String(), "\r\x1b") {
		t.Fatal("animation control characters reached a non-terminal stream")
	}
}

func TestProgressStopIsIdempotent(t *testing.T) {
	var buf bytes.Buffer
	p := NewProgress(&buf, true)
	p.Observed(api.EnvironmentStatusBuilding, 0)
	p.Stop()
	p.Stop()
}

type recordingReporter struct {
	states    []api.EnvironmentStatus
	throttled []time.Duration
}

func (r *recordingReporter) Observed(s api.EnvironmentStatus, _ time.Duration) {
	r.states = append(r.states, s)
}
func (r *recordingReporter) Throttled(d time.Duration) { r.throttled = append(r.throttled, d) }
func (r *recordingReporter) Stop()                     {}

// clientRetryAfter reaches the client package's guard from here, so the bound
// is asserted on the code the CLI actually runs rather than on a copy of it.
func clientRetryAfter(h *struct{ RetryAfter int }) time.Duration {
	return client.RetryAfter(h)
}
