package wait

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/steadfast-ly/drift-cli/internal/api"
)

// Progress reports what a wait is seeing.
//
// It writes to STDERR, always. A wait's output is a diagnostic — the command's
// data is whatever it prints when the wait finishes — and animating into stdout
// would put carriage returns and spinner frames into
// `drift env create ... -o json > env.json`.
//
// Two modes, decided by the caller from the STDERR stream alone rather than
// from stdout: on a terminal it animates one line in place, and everywhere else
// it writes one line per state change. The off-terminal form is deliberately
// not "the same thing without the spinner": a CI log wants a transition
// history, and repeating an unchanged state every three seconds for fifteen
// minutes would bury it.
type Progress struct {
	w       io.Writer
	animate bool

	mu       sync.Mutex
	state    string
	elapsed  time.Duration
	throttle time.Duration
	printed  int
	last     string
	stopped  bool

	stop chan struct{}
	done chan struct{}
}

// FrameInterval is how often the animated form redraws. Independent of the poll
// interval: at three seconds between polls a spinner that only advanced on a
// poll would look frozen.
const FrameInterval = 120 * time.Millisecond

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// NewProgress builds a reporter. `animate` should come from whether the writer
// is a terminal.
func NewProgress(w io.Writer, animate bool) *Progress {
	p := &Progress{w: w, animate: animate}
	if animate {
		p.stop = make(chan struct{})
		p.done = make(chan struct{})
		go p.spin()
	}
	return p
}

func (p *Progress) spin() {
	defer close(p.done)
	t := time.NewTicker(FrameInterval)
	defer t.Stop()
	frame := 0
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.draw(spinnerFrames[frame%len(spinnerFrames)])
			frame++
		}
	}
}

func (p *Progress) draw(spinner string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || p.state == "" {
		return
	}
	line := fmt.Sprintf("%s %s (%s)", spinner, p.state, p.elapsed.Round(time.Second))
	if p.throttle > 0 {
		line = fmt.Sprintf("%s %s — rate limited, retrying in %s",
			spinner, p.state, p.throttle.Round(time.Second))
	}
	p.writeLine(line)
}

// writeLine redraws the animated line in place. Called with the lock held.
//
// The previous line is erased by overwriting it with spaces rather than with an
// ANSI erase sequence: the animated path is only taken on a terminal, but a
// terminal that does not honour `\x1b[K` would be left with the tail of a
// longer previous state stuck on the end of a shorter one.
func (p *Progress) writeLine(s string) {
	pad := ""
	if n := p.printed - len([]rune(s)); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	fmt.Fprintf(p.w, "\r%s%s", s, pad)
	p.printed = len([]rune(s))
}

// Observed records a poll.
func (p *Progress) Observed(status api.EnvironmentStatus, elapsed time.Duration) {
	p.record(string(status), elapsed)
}

// ForPromotion adapts the same reporter to a promotion wait.
//
// A wrapper rather than a second method, because the two waits differ only in
// the TYPE of the state they report and Go will not let one method name carry
// both. Everything an operator sees — animation, transition lines, throttling
// notices — is the same code.
func (p *Progress) ForPromotion() PromotionReporter { return promotionProgress{p} }

type promotionProgress struct{ *Progress }

func (p promotionProgress) Observed(status api.PromotionStatus, elapsed time.Duration) {
	p.Progress.record(string(status), elapsed)
}

func (p *Progress) record(state string, elapsed time.Duration) {
	p.mu.Lock()
	changed := state != p.last
	p.state, p.elapsed, p.throttle = state, elapsed, 0
	p.last = state
	animate, stopped := p.animate, p.stopped
	p.mu.Unlock()

	if animate || stopped || !changed {
		return
	}
	fmt.Fprintf(p.w, "%s (%s)\n", state, elapsed.Round(time.Second))
}

// Throttled records that the wait is backing off for a rate limit.
//
// Reported in BOTH modes even though it is not a state change: a wait that
// appears to stall for a minute needs to say why, and "the server asked us to"
// is a different problem from "the deployment is stuck".
func (p *Progress) Throttled(retryAfter time.Duration) {
	p.mu.Lock()
	p.throttle = retryAfter
	animate, stopped := p.animate, p.stopped
	p.mu.Unlock()
	if animate || stopped {
		return
	}
	fmt.Fprintf(p.w, "rate limited; retrying in %s\n", retryAfter.Round(time.Second))
}

// Stop ends the animation and leaves the cursor on a fresh line. Idempotent,
// because it is called from a defer and may also be called by the caller.
func (p *Progress) Stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	animate := p.animate
	p.mu.Unlock()

	if !animate {
		return
	}
	close(p.stop)
	<-p.done
	// Erase the animated line entirely. What the command prints next is the
	// result, and a leftover "⠹ deploying (14s)" above it reads like part of it.
	p.mu.Lock()
	if p.printed > 0 {
		fmt.Fprintf(p.w, "\r%s\r", strings.Repeat(" ", p.printed))
		p.printed = 0
	}
	p.mu.Unlock()
}
