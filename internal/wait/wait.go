package wait

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/steadfast/drift-cli/internal/api"
	"github.com/steadfast/drift-cli/internal/cliexit"
)

// DefaultInterval is how often a wait polls.
//
// Chosen against the server's own read limit (120 requests per minute per
// credential): three seconds is 20 per minute, so a wait uses a sixth of the
// budget and leaves room for the operator's other terminal. Waiting is not
// urgent work — the states being waited on change on the scale of tens of
// seconds — so polling faster would buy nothing and cost the one resource the
// server actually meters.
const DefaultInterval = 3 * time.Second

// DefaultFailureWindow is how long a failure state must persist before the wait
// believes it.
//
// TIME, not a poll count, and that distinction was a real bug. Three polls at a
// three-second interval spans six seconds, not the ten it looks like — and the
// in-flight-build mitigation cannot help the state this package was built
// around: `deploy_failed` is entered AFTER `BUILDS_COMPLETED`, so no build is in
// flight there by construction, and the reset only ever protects `build_failed`.
// That left the headline case with six seconds of tolerance against a server
// whose own notes call ArgoCD Degraded flapping routine, with recovery arriving
// on a webhook whose latency the CLI does not control.
//
// Thirty seconds costs half a minute on a path that has already failed and
// covers a reconcile loop with room to spare. Tie the wait to the clock and
// changing the poll interval no longer silently changes the tolerance.
const DefaultFailureWindow = 30 * time.Second

// DefaultFailureStreak is how many consecutive polls it takes to believe that a
// goal has become unreachable.
//
// A much weaker condition than a failure and it needs a much weaker guard: the
// only thing it protects against is a mutation's write racing its first poll,
// which resolves on the very next one. It does NOT gate the failure rule, which
// is on the window above.
const DefaultFailureStreak = 3

// Observation is one poll's answer.
type Observation struct {
	Status api.EnvironmentStatus
	// BuildInFlight is true when any of the environment's builds still has an
	// open outcome. It is the evidence that a failure state is being actively
	// recovered rather than sitting there.
	BuildInFlight bool
}

// Poller fetches the current state. Returning an error aborts the wait, except
// for a rate limit, which the Waiter honours and retries.
type Poller func(context.Context) (Observation, error)

// Reporter receives progress. Implementations decide whether to animate; the
// Waiter only says what it saw and when.
type Reporter interface {
	// Observed is called after every successful poll.
	Observed(status api.EnvironmentStatus, elapsed time.Duration)
	// Throttled is called when the server rate-limited a poll and the wait is
	// backing off for the interval it asked for.
	Throttled(retryAfter time.Duration)
	// Stop releases anything the reporter is animating. Always called.
	Stop()
}

// Options configures one wait.
type Options struct {
	// Goal is the state being waited for.
	Goal api.EnvironmentStatus
	// Timeout bounds the whole wait. Exceeding it is exit 6.
	Timeout time.Duration
	// Interval is the poll period. Zero means DefaultInterval.
	Interval time.Duration
	// FailureWindow is how long a failure state must persist before it is
	// believed. Zero means DefaultFailureWindow.
	FailureWindow time.Duration
	// FailureStreak is how many consecutive polls must agree that the goal is
	// unreachable. Zero means DefaultFailureStreak.
	FailureStreak int
	// Commanded records that the transition to Goal has ALREADY been requested
	// by this invocation.
	//
	// It is the one fact the machine cannot supply. `sleeping` and `canceled`
	// have no inbound system edge at all — the only ways in are SLEEP and
	// CANCEL, both operator events — so a reachability argument proves every
	// `sleep --wait` futile and every `cancel --wait` with it, which is exactly
	// backwards when this process just issued the command. When a command HAS
	// been issued, a state the machine cannot leave unaided is not evidence of
	// futility, only of a write that has not been observed yet, and the correct
	// failure is a timeout.
	Commanded bool
	// Ref is how the user named the thing being waited on, for messages.
	Ref string
	// Reporter receives progress. Optional.
	Reporter Reporter

	// now and sleep are injected by tests so that a wait covering fifteen
	// simulated minutes runs instantly and deterministically. Production leaves
	// them nil.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

func (o *Options) interval() time.Duration {
	if o.Interval > 0 {
		return o.Interval
	}
	return DefaultInterval
}

func (o *Options) streak() int {
	if o.FailureStreak > 0 {
		return o.FailureStreak
	}
	return DefaultFailureStreak
}

func (o *Options) failureWindow() time.Duration {
	if o.FailureWindow > 0 {
		return o.FailureWindow
	}
	return DefaultFailureWindow
}

// futile reports whether waiting can be PROVEN pointless from the machine.
//
// Proven, not merely unlikely: this is the only path that turns a wait into a
// failure without the server having failed, so it errs towards the timeout in
// every case where the answer depends on something the machine does not record.
//
//   - The goal is reachable over server-raised edges: not futile, obviously.
//   - The current state has no outgoing edges at all (`destroyed`): nothing will
//     ever happen again, whoever asks. Futile.
//   - The goal has no INBOUND system edge (`sleeping`, `canceled`): only a
//     command produces it, so its absence proves nothing about whether one is
//     on the way. Not futile — a stalled wait ends as a timeout.
//   - A command WAS issued by this invocation: the same reasoning, for a goal
//     the system can normally produce. `rm --wait` reading a stale `running`
//     once is not proof that the destroy it just requested will not land.
//   - Otherwise: no server-raised path exists and nobody here asked for one.
//     Futile, and this is the case worth catching — `env wait --for running` on
//     a sleeping environment answers in seconds instead of thirty minutes.
func (o *Options) futile(from api.EnvironmentStatus) bool {
	if Reachable(from, o.Goal) {
		return false
	}
	if len(envMachine[from]) == 0 {
		return true
	}
	if CommandOnly(o.Goal) || o.Commanded {
		return false
	}
	return true
}

func (o *Options) clock() func() time.Time {
	if o.now != nil {
		return o.now
	}
	return time.Now
}

func (o *Options) sleeper() func(context.Context, time.Duration) error {
	if o.sleep != nil {
		return o.sleep
	}
	return realSleep
}

func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// DefaultTimeoutFor is the timeout for waiting on a bare state, used by
// `drift env wait` when the user names a state but no timeout.
//
// `destroyed` is the outlier and the reason this is a function rather than one
// constant: destroy convergence is owned by a two-minute cron with an
// eight-minute escalation window, so a legitimate destroy routinely exceeds ten
// minutes and a shorter default would report exit 6 on a teardown that was
// working perfectly (DESIGN.md §5).
func DefaultTimeoutFor(goal api.EnvironmentStatus) time.Duration {
	switch goal {
	case api.EnvironmentStatusDestroyed, api.EnvironmentStatusDestroying:
		return 20 * time.Minute
	case api.EnvironmentStatusRunning, api.EnvironmentStatusDeploying,
		api.EnvironmentStatusBuilding:
		// Includes a container build, which is the slow part.
		return 30 * time.Minute
	default:
		return 10 * time.Minute
	}
}

// Wait polls until the goal is reached, a failure is confirmed, the goal
// becomes unreachable, or the timeout expires.
//
// The four outcomes are deliberately distinct exit codes: reaching the goal is
// 0, a confirmed failure or an unreachable goal is 5 (the operation failed),
// and running out of time is 6 (nothing is known — the thing may still be on
// its way).
func Wait(ctx context.Context, opts Options, poll Poller) (api.EnvironmentStatus, error) {
	rep := opts.Reporter
	if rep == nil {
		rep = nopReporter{}
	}
	defer rep.Stop()

	now := opts.clock()
	sleep := opts.sleeper()
	start := now()
	deadline := start.Add(opts.Timeout)

	// adverse counts CONSECUTIVE polls that argued for giving up, `since` is when
	// the current argument was first made, and `reason` records which argument
	// it is. A different reason restarts both: two polls saying "failed" and one
	// saying "unreachable" are not three polls agreeing on anything.
	adverse := 0
	reason := ""
	var since time.Time
	var last api.EnvironmentStatus

	for {
		obs, err := poll(ctx)
		if err != nil {
			var ee *cliexit.ExitError
			// A rate limit is not a failure of the thing being waited on, and
			// giving up on one would make `--wait` the least reliable way to use
			// the CLI at exactly the moment the server is busiest. Back off and
			// carry on; the deadline still applies, so a persistently throttled
			// wait ends as a timeout rather than spinning forever.
			if errors.As(err, &ee) && ee.Code == cliexit.RateLimited {
				backoff := backoffFor(ee.RetryAfter, opts.interval())
				rep.Throttled(backoff)
				if now().Add(backoff).After(deadline) {
					return last, timeoutError(opts, last, now().Sub(start))
				}
				if serr := sleep(ctx, backoff); serr != nil {
					return last, canceled(serr)
				}
				continue
			}
			return last, err
		}

		// A state this build has never heard of. The transition tables are
		// asserted against the VENDORED spec, which by definition cannot cover
		// the case where the server is ahead of it — and every rule below would
		// then answer from an empty edge list, reporting a state with no
		// transitions as settled and its goal as unreachable. Both claims would
		// be fabricated, so the wait says what is actually true instead.
		if !obs.Status.Valid() {
			return obs.Status, unknownStateError(opts, obs.Status)
		}

		last = obs.Status
		rep.Observed(obs.Status, now().Sub(start))

		if obs.Status == opts.Goal {
			return obs.Status, nil
		}

		switch {
		// A failure state the machine can still leave on its own is not yet a
		// failure. It becomes one by PERSISTING — measured on the clock, not in
		// polls — and an in-flight build is positive evidence of recovery, so it
		// resets the count outright rather than merely not incrementing it.
		case Failure(obs.Status) && !obs.BuildInFlight:
			adverse, reason, since = bump(adverse, reason, since, "failed", now())
			if adverse >= 2 && now().Sub(since) >= opts.failureWindow() {
				return obs.Status, failureError(opts, obs.Status, adverse, now().Sub(since))
			}
		// The goal cannot be reached from here by anything the server will do
		// unprompted, and no command of ours is outstanding that would explain
		// the gap. Subject to the streak because a mutation's write and the
		// first poll can race.
		case opts.futile(obs.Status):
			adverse, reason, since = bump(adverse, reason, since, "unreachable", now())
			if adverse >= opts.streak() {
				return obs.Status, unreachableError(opts, obs.Status)
			}
		default:
			adverse, reason, since = 0, "", time.Time{}
		}

		if !now().Add(opts.interval()).Before(deadline) {
			return last, timeoutError(opts, last, now().Sub(start))
		}
		if err := sleep(ctx, opts.interval()); err != nil {
			return last, canceled(err)
		}
	}
}

// backoffFor turns a server's `Retry-After` into a sleep this client will
// actually make.
//
// Two bounds, and the FLOOR is the one that matters. A `Retry-After` shorter
// than the poll interval — including the zero left behind by a header that was
// missing, negative, or too large to be meaningful — would make a client that
// has just been told it is sending too many requests send them FASTER than its
// own steady-state rate. The server's number is honoured only where it asks for
// more patience than the CLI already had.
func backoffFor(retryAfter, interval time.Duration) time.Duration {
	if retryAfter < interval {
		return interval
	}
	return retryAfter
}

// bump advances the adverse counter, restarting it when the argument changed.
func bump(n int, reason string, since time.Time, want string, now time.Time) (int, string, time.Time) {
	if reason != want {
		return 1, want, now
	}
	return n + 1, want, since
}

func canceled(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &cliexit.ExitError{Code: cliexit.WaitTimeout, Message: "the wait timed out", Err: err}
	}
	return &cliexit.ExitError{Code: cliexit.Error, Message: "the wait was interrupted", Err: err}
}

func timeoutError(opts Options, last api.EnvironmentStatus, elapsed time.Duration) error {
	e := &cliexit.ExitError{
		Code: cliexit.WaitTimeout,
		Message: fmt.Sprintf("timed out after %s waiting for %s to reach %s",
			elapsed.Round(time.Second), opts.Ref, opts.Goal),
		Hint: fmt.Sprintf("nothing was rolled back; run `drift env wait %s --for %s --timeout <duration>` to keep waiting",
			opts.Ref, opts.Goal),
	}
	if last != "" {
		e.Detail = fmt.Sprintf("last observed state was %s", last)
	}
	return e
}

// failureError states the EVIDENCE rather than a conclusion.
//
// The earlier wording claimed the failure "is not a transient ArgoCD blip",
// which the observation cannot establish: recovery arrives on a webhook whose
// latency this client does not control, so a long enough delay is
// indistinguishable from a permanent failure. What can honestly be said is how
// long it was watched and what normally happens in that time.
func failureError(opts Options, status api.EnvironmentStatus, polls int, held time.Duration) error {
	return &cliexit.ExitError{
		Code:    cliexit.Conflict,
		Message: fmt.Sprintf("%s is %s", opts.Ref, status),
		Detail: fmt.Sprintf(
			"held that state for %s across %d polls with no build in flight; an ArgoCD blip normally clears well inside that",
			held.Round(time.Second), polls),
		Hint: fmt.Sprintf("`drift env get %s` shows the builds; `drift env retry-build` re-runs one", opts.Ref),
	}
}

// unknownStateError reports a server that has moved ahead of this client.
func unknownStateError(opts Options, status api.EnvironmentStatus) error {
	return &cliexit.ExitError{
		Code:    cliexit.Error,
		Message: fmt.Sprintf("%s is in state %q, which this client does not know", opts.Ref, status),
		Detail: "the lifecycle states this build understands come from the contract it was generated " +
			"against, so it cannot say whether this one is on the way to " + string(opts.Goal) + " or not",
		Hint: "upgrade drift; `drift env get " + opts.Ref + "` still shows the state as the server reports it",
	}
}

func unreachableError(opts Options, status api.EnvironmentStatus) error {
	e := &cliexit.ExitError{
		Code:    cliexit.Conflict,
		Message: fmt.Sprintf("%s is %s and will not reach %s on its own", opts.Ref, status, opts.Goal),
		Detail: fmt.Sprintf(
			"no server-raised event leads from %s to %s, so waiting cannot succeed", status, opts.Goal),
		Hint: "the remaining transitions out of this state need an explicit command",
	}
	if cmd := commandFor(status, opts.Goal); cmd != "" {
		e.Hint = "run `drift env " + cmd + " " + opts.Ref + "` — that transition needs an explicit command"
	}
	return e
}

// commandFor names the CLI command that walks a user edge out of `from`
// towards `goal`, when there is exactly one obvious candidate. Advice only, so
// an unmapped event simply produces the generic hint.
func commandFor(from, goal api.EnvironmentStatus) string {
	byEvent := map[string]string{
		"WAKE": "wake", "SLEEP": "sleep", "DESTROY": "rm",
		"CANCEL": "cancel", "REBUILD": "relaunch",
	}
	for _, e := range envMachine[from] {
		if e.Origin == User && (e.To == goal || Reachable(e.To, goal)) {
			if name, ok := byEvent[e.Event]; ok {
				return name
			}
		}
	}
	return ""
}

type nopReporter struct{}

func (nopReporter) Observed(api.EnvironmentStatus, time.Duration) {}
func (nopReporter) Throttled(time.Duration)                       {}
func (nopReporter) Stop()                                         {}
