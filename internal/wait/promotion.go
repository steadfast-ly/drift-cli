package wait

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/steadfast/drift-cli/internal/api"
	"github.com/steadfast/drift-cli/internal/cliexit"
)

// PromotionObservation is one poll of a promotion.
type PromotionObservation struct {
	Status api.PromotionStatus
	// Message is the server's `statusMessage`, shown on failure because it is
	// where the workflow's own reason ends up.
	Message string
}

// PromotionPoller fetches a promotion's current state.
type PromotionPoller func(context.Context) (PromotionObservation, error)

// PromotionReporter receives promotion progress.
type PromotionReporter interface {
	Observed(status api.PromotionStatus, elapsed time.Duration)
	Throttled(retryAfter time.Duration)
	Stop()
}

// PromotionOptions configures a promotion wait.
type PromotionOptions struct {
	Timeout  time.Duration
	Interval time.Duration
	Ref      string
	Reporter PromotionReporter

	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// DefaultPromotionTimeout bounds a promotion wait. A promotion dispatches a
// GitHub Actions retag per repository and then waits on ArgoCD, so it is
// bounded by a workflow queue rather than by an image build.
const DefaultPromotionTimeout = 20 * time.Minute

// WaitPromotion polls a promotion until the machine says it has finished.
//
// No sustained-failure rule here, and that difference is deliberate rather than
// an omission. In the promotion machine `failed` and `deploy_failed` are both
// declared `final` — no outgoing edges at all — so a promotion reporting
// `deploy_failed` has finished badly and nothing will move it. Applying the
// environment's streak rule would delay a settled answer by three polls to
// guard against a recovery that the machine forbids.
func WaitPromotion(ctx context.Context, opts PromotionOptions, poll PromotionPoller) (api.PromotionStatus, error) {
	rep := opts.Reporter
	if rep == nil {
		rep = nopPromotionReporter{}
	}
	defer rep.Stop()

	now := time.Now
	if opts.now != nil {
		now = opts.now
	}
	sleep := realSleep
	if opts.sleep != nil {
		sleep = opts.sleep
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	// Zero means "the default", exactly as it does on the environment waits. It
	// previously meant "poll once and time out" here, so the same
	// `--wait-timeout 0` did two different things depending on which command it
	// was passed to.
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultPromotionTimeout
	}

	start := now()
	deadline := start.Add(timeout)
	var last api.PromotionStatus

	for {
		obs, err := poll(ctx)
		if err != nil {
			var ee *cliexit.ExitError
			if errors.As(err, &ee) && ee.Code == cliexit.RateLimited {
				// Floored at the poll interval and capped by the deadline, for
				// the same reason as the environment wait: a client that has
				// just been told it is sending too many requests must never come
				// back sooner than it would have anyway.
				backoff := backoffFor(ee.RetryAfter, interval)
				rep.Throttled(backoff)
				if now().Add(backoff).After(deadline) {
					return last, promotionTimeout(opts, last, now().Sub(start))
				}
				if serr := sleep(ctx, backoff); serr != nil {
					return last, canceled(serr)
				}
				continue
			}
			return last, err
		}

		last = obs.Status
		rep.Observed(obs.Status, now().Sub(start))

		if PromotionTerminal(obs.Status) {
			if PromotionFailed(obs.Status) {
				e := &cliexit.ExitError{
					Code:    cliexit.Conflict,
					Message: fmt.Sprintf("promotion %s %s", opts.Ref, obs.Status),
					Hint:    "`drift release status` shows the dispatched workflows and their runs",
				}
				if obs.Message != "" {
					e.Detail = obs.Message
				}
				return obs.Status, e
			}
			return obs.Status, nil
		}

		if !now().Add(interval).Before(deadline) {
			return last, promotionTimeout(opts, last, now().Sub(start))
		}
		if err := sleep(ctx, interval); err != nil {
			return last, canceled(err)
		}
	}
}

func promotionTimeout(opts PromotionOptions, last api.PromotionStatus, elapsed time.Duration) error {
	e := &cliexit.ExitError{
		Code:    cliexit.WaitTimeout,
		Message: fmt.Sprintf("timed out after %s waiting for promotion %s", elapsed.Round(time.Second), opts.Ref),
		Hint:    "the promotion is still running; `drift release status` follows it",
	}
	if last != "" {
		e.Detail = fmt.Sprintf("last observed status was %s", last)
	}
	return e
}

type nopPromotionReporter struct{}

func (nopPromotionReporter) Observed(api.PromotionStatus, time.Duration) {}
func (nopPromotionReporter) Throttled(time.Duration)                     {}
func (nopPromotionReporter) Stop()                                       {}
