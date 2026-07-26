// Package cliexit defines the CLI's exit-code contract and the error type that
// carries one.
//
// The codes are a public interface: scripts branch on them, so they are frozen
// here, documented in `drift --help`, and mapped from the server's `status`
// field in exactly one place (`FromHTTPStatus`). Nothing else in the CLI is
// allowed to invent a code.
package cliexit

import (
	"errors"
	"fmt"
	"time"
)

// The exit-code contract (DESIGN.md §5). Frozen — additive only.
const (
	// OK is a successful run.
	OK = 0
	// Error is any failure with no more specific code.
	Error = 1
	// Usage is a malformed invocation: unknown flag, missing argument, bad value.
	Usage = 2
	// NotFound is a resource the server could not resolve.
	NotFound = 3
	// AuthRequired is a missing, expired or rejected credential.
	AuthRequired = 4
	// Conflict is a state-machine conflict or an operation the server refused.
	Conflict = 5
	// WaitTimeout is a `--wait` that ran out of time.
	WaitTimeout = 6
	// RateLimited is a per-credential rate limit, with a retry worth making.
	//
	// REACHABLE as of the mutation surface: `/api/v1` limits per credential and
	// answers 429 with a `Retry-After` in whole seconds, declared on every
	// operation in the contract. A 429 that arrives without a decodable drift
	// envelope is NOT reported as this code — an intermediary (an ALB, a WAF)
	// throttling the connection is not drift rate-limiting the caller, and
	// `Problem` keeps that case on the generic failure path.
	RateLimited = 7
)

// Help is the exit-code table rendered into `--help`. Kept next to the
// constants so the two cannot drift apart.
const Help = `Exit codes:
  0   success
  1   error
  2   usage (bad flags or arguments)
  3   not found
  4   authentication required
  5   state conflict or operation failed
  6   wait timed out
  7   rate limited (retry after the interval the server names)`

// Error carries a message and the exit code the process should end with.
//
// `Detail` is the server's `data.detail` when there was one. It is rendered on
// its own line rather than concatenated, because it is frequently a sentence of
// its own and the two read badly joined.
type ExitError struct {
	Code    int
	Message string
	Detail  string
	// Hint is CLI-side advice (e.g. "run `drift auth login`"), never
	// server-supplied.
	Hint string
	// Err is an optional wrapped cause, for `errors.Is`/`errors.As`. It is NOT
	// printed: a raw Go error where the server gave a typed one is exactly what
	// this type exists to prevent.
	Err error
	// RetryAfter is the interval the server asked the caller to wait, taken from
	// the `Retry-After` response header on a 429. Zero when the server did not
	// say. Rendered as advice, and consumed by `--wait` so a poll loop backs off
	// to the server's number rather than its own.
	RetryAfter time.Duration
}

func (e *ExitError) Error() string { return e.Message }

func (e *ExitError) Unwrap() error { return e.Err }

// New builds an ExitError with a formatted message.
func New(code int, format string, args ...any) *ExitError {
	return &ExitError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap builds an ExitError around a cause. The cause is retained for
// `errors.Is` but never rendered.
func Wrap(code int, err error, format string, args ...any) *ExitError {
	return &ExitError{Code: code, Message: fmt.Sprintf(format, args...), Err: err}
}

// CodeOf extracts the exit code an error should produce. A nil error is 0; an
// error that is not an ExitError is a generic failure.
func CodeOf(err error) int {
	if err == nil {
		return OK
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return Error
}

// FromProblem maps a server error envelope onto the exit-code contract.
//
// This is the ONLY place the mapping exists, and it takes the envelope's `code`
// as well as its `status` because two cases cannot be told apart from the
// status alone:
//
//   - **403.** Today every 403 is "your role is below the floor", where
//     re-authenticating does not help and telling the user to log in again
//     sends them in a circle — so it is a plain Error. At step 6 the same status
//     will also mean "this credential lacks the `promote:prd` scope", where
//     re-login through the elevation mint IS the remedy and AuthRequired is
//     correct. The discrimination has to be on `code`, not on `status`, and the
//     seam exists now so that adding it later is a one-line change rather than
//     a breaking reinterpretation of an exit code scripts already depend on.
//     No code string is guessed here: the server has no promotion procedures
//     yet, so there is nothing to match on and nothing is invented.
//
//   - **429.** Now emitted as RateLimited, because the server means it: limits
//     are per credential and every operation in the contract declares a 429
//     carrying `Retry-After`. Reached only through this function, so a 429 that
//     did not arrive as a drift envelope stays on the generic path — an
//     intermediary throttling the connection is a different condition with a
//     different remedy, and `Problem` keeps the two apart.
func FromProblem(code string, status int) int {
	// Reserved for the step-6 elevation case; deliberately empty rather than
	// populated with a guess.
	_ = code
	return FromHTTPStatus(status)
}

// FromHTTPStatus maps an HTTP status onto the exit-code contract.
//
// The server's envelope carries its own `status` integer, which is what gets
// passed here — not the transport status — so a proxy that rewrites the status
// line cannot change the CLI's answer.
func FromHTTPStatus(status int) int {
	switch status {
	case 400, 422:
		// The server validated input the CLI built, so from the user's
		// perspective this is a usage error.
		return Usage
	case 401:
		return AuthRequired
	case 403:
		// Authenticated but not permitted. Deliberately NOT AuthRequired:
		// re-authenticating will not help, and telling a user to log in again
		// when their role is the problem sends them in a circle.
		return Error
	case 404:
		return NotFound
	case 409:
		return Conflict
	case 429:
		return RateLimited
	default:
		return Error
	}
}
