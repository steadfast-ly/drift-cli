package cliexit

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The exit codes are a PUBLIC contract: scripts branch on them. This test is
// the thing that makes renumbering one a deliberate, reviewable act.
func TestExitCodesAreFrozen(t *testing.T) {
	frozen := map[string]int{
		"OK": 0, "Error": 1, "Usage": 2, "NotFound": 3,
		"AuthRequired": 4, "Conflict": 5, "WaitTimeout": 6, "RateLimited": 7,
	}
	actual := map[string]int{
		"OK": OK, "Error": Error, "Usage": Usage, "NotFound": NotFound,
		"AuthRequired": AuthRequired, "Conflict": Conflict, "WaitTimeout": WaitTimeout,
		"RateLimited": RateLimited,
	}
	for name, want := range frozen {
		if actual[name] != want {
			t.Fatalf("%s = %d, want %d — this is a breaking change to a documented contract",
				name, actual[name], want)
		}
	}
}

func TestFromHTTPStatus(t *testing.T) {
	cases := []struct {
		status int
		want   int
		why    string
	}{
		{200, Error, "a success should never reach the mapper, but must not map to 0 by accident"},
		{400, Usage, "the server rejected input the CLI built, which is the user's invocation"},
		{422, Usage, "same as 400"},
		{401, AuthRequired, "missing, expired or rejected credential"},
		{403, Error, "authenticated but not permitted; re-authenticating will not help"},
		{404, NotFound, ""},
		{409, Conflict, "state-machine conflict"},
		{429, Error, "rate limited"},
		{500, Error, ""},
		{503, Error, ""},
	}
	for _, c := range cases {
		if got := FromHTTPStatus(c.status); got != c.want {
			t.Fatalf("FromHTTPStatus(%d) = %d, want %d (%s)", c.status, got, c.want, c.why)
		}
	}
}

// 403 must NOT be AuthRequired. Telling a user to log in again when their ROLE
// is the problem sends them round in a circle.
func TestForbiddenIsNotAuthRequired(t *testing.T) {
	if FromHTTPStatus(403) == AuthRequired {
		t.Fatal("403 mapped onto the authentication exit code")
	}
}

func TestCodeOf(t *testing.T) {
	if got := CodeOf(nil); got != OK {
		t.Fatalf("CodeOf(nil) = %d", got)
	}
	if got := CodeOf(errors.New("plain")); got != Error {
		t.Fatalf("CodeOf(plain) = %d, want %d", got, Error)
	}
	if got := CodeOf(New(NotFound, "gone")); got != NotFound {
		t.Fatalf("CodeOf(ExitError) = %d", got)
	}
	// Wrapped several layers deep, which is how errors actually arrive.
	deep := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", New(Conflict, "busy")))
	if got := CodeOf(deep); got != Conflict {
		t.Fatalf("CodeOf(wrapped) = %d, want %d", got, Conflict)
	}
}

// The wrapped cause exists for errors.Is and must never be part of the message:
// printing a raw Go error where the server gave a typed one is exactly what the
// error envelope exists to prevent.
func TestWrappedCauseIsReachableButNotRendered(t *testing.T) {
	cause := errors.New("dial tcp 10.0.0.1:443: i/o timeout")
	e := Wrap(Error, cause, "cannot reach %s", "https://drift.example.com")

	if !errors.Is(e, cause) {
		t.Fatal("the cause is not reachable through errors.Is")
	}
	if strings.Contains(e.Error(), "i/o timeout") {
		t.Fatalf("the raw cause leaked into the message: %q", e.Error())
	}
	if e.Error() != "cannot reach https://drift.example.com" {
		t.Fatalf("message = %q", e.Error())
	}
}

// The help text and the constants must not drift apart.
func TestHelpDocumentsEveryCode(t *testing.T) {
	for _, want := range []string{
		"0   success", "1   error", "2   usage", "3   not found",
		"4   authentication required", "5   state conflict", "6   wait timed out",
		"7   rate limited",
	} {
		if !strings.Contains(Help, want) {
			t.Fatalf("--help exit-code table is missing %q:\n%s", want, Help)
		}
	}
}

// REGRESSION. The lookup walked only `Unwrap() error`, so an ExitError joined
// with `errors.Join` — or any multi-error — was walked past and silently
// downgraded to exit 1. `errors.As` handles both forms.
func TestCodeOfFindsAJoinedExitError(t *testing.T) {
	joined := errors.Join(errors.New("first"), New(NotFound, "gone"))
	if got := CodeOf(joined); got != NotFound {
		t.Fatalf("CodeOf(joined) = %d, want %d", got, NotFound)
	}
	nested := fmt.Errorf("outer: %w", errors.Join(errors.New("a"), New(Conflict, "busy")))
	if got := CodeOf(nested); got != Conflict {
		t.Fatalf("CodeOf(nested join) = %d, want %d", got, Conflict)
	}
}

// FromProblem is the seam for the two cases a status cannot decide on its own.
// Today it agrees with FromHTTPStatus for everything, which is the point: the
// place to make the change exists before anything depends on the current answer.
func TestFromProblemMatchesStatusToday(t *testing.T) {
	for _, c := range []struct {
		code   string
		status int
	}{
		{"BAD_REQUEST", 400}, {"UNAUTHORIZED", 401}, {"FORBIDDEN", 403},
		{"NOT_FOUND", 404}, {"CONFLICT", 409}, {"TOO_MANY_REQUESTS", 429},
		{"INTERNAL_SERVER_ERROR", 500}, {"SERVICE_UNAVAILABLE", 503},
	} {
		if got, want := FromProblem(c.code, c.status), FromHTTPStatus(c.status); got != want {
			t.Fatalf("FromProblem(%q, %d) = %d, want %d", c.code, c.status, got, want)
		}
	}
	// Recorded decisions, so a change to either is deliberate:
	//   Exit 7 is allocated for rate limiting but not yet emitted: nothing on
	//   /api/v1 can produce a 429 until step 5, and a 429 from an intermediary is
	//   not drift rate-limiting the caller.
	if FromProblem("TOO_MANY_REQUESTS", 429) != Error {
		t.Fatal("429 no longer maps to Error; update the decision recorded in FromProblem")
	}
	if RateLimited != 7 {
		t.Fatalf("RateLimited = %d, want 7", RateLimited)
	}
	//   403 stays Error until `promote prd` can return a scope failure (step 6),
	//   where re-login through the elevation mint IS the remedy.
	if FromProblem("FORBIDDEN", 403) != Error {
		t.Fatal("403 no longer maps to Error; update the decision recorded in FromProblem")
	}
}
