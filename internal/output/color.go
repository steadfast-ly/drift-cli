package output

import (
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// ColorEnabled decides whether ANSI colour may be emitted to w.
//
// Precedence, and why:
//
//  1. `CLICOLOR_FORCE` (non-empty, not "0") — an explicit request wins over
//     every heuristic, including the pipe check. This is how a user gets colour
//     through `less -R`.
//  2. `NO_COLOR` (present, any value, per no-color.org) — an explicit refusal.
//     Checked after FORCE because a user who sets both this run is asking for
//     it this run.
//  3. `TERM=dumb` — the terminal says it cannot render escapes.
//  4. CI detection — build logs are captured, not watched, and escapes there
//     are noise in a diff.
//  5. Is w a terminal at all.
//
// Colour is the ONLY thing that changes off-TTY. Column widths, field order and
// values are identical, so `drift env list | grep` sees what the operator saw.
func ColorEnabled(w io.Writer) bool {
	if v := os.Getenv("CLICOLOR_FORCE"); v != "" && v != "0" {
		return true
	}
	// Per no-color.org the variable disables colour when present AND non-empty.
	// An empty value is explicitly not a request — treating `NO_COLOR=` as one
	// would break the documented way to unset it for a single command.
	if v, ok := os.LookupEnv("NO_COLOR"); ok && v != "" {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	if isCI() {
		return false
	}
	return IsTerminal(w)
}

// isCI recognises the common markers. `CI` alone covers GitHub Actions, GitLab,
// CircleCI and Travis; the rest are systems that do not set it.
func isCI() bool {
	for _, k := range []string{"CI", "BUILD_NUMBER", "TEAMCITY_VERSION", "JENKINS_URL"} {
		if v := os.Getenv(k); v != "" && v != "0" && !strings.EqualFold(v, "false") {
			return true
		}
	}
	return false
}

// IsTerminal reports whether w is an interactive terminal.
//
// `golang.org/x/term` rather than a `ModeCharDevice` stat: /dev/null is a
// character device, so the stat version answered "terminal" for
// `drift env list > /dev/null`, and ANSI escapes went into redirected output.
// The same predicate decides whether it is safe to PROMPT, and step 4's rule
// that destructive commands refuse without --yes off a TTY rests on it, so
// being wrong about /dev/null is not merely cosmetic.
func IsTerminal(w io.Writer) bool { return isTerminalFile(w) }

// IsTerminalReader is IsTerminal for an input stream.
//
// Separate only because `io.Reader` and `io.Writer` share no interface; it is
// the same predicate on the same file descriptor, so a prompt and a colour
// decision can never disagree about what a terminal is.
func IsTerminalReader(r io.Reader) bool { return isTerminalFile(r) }

func isTerminalFile(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// ANSI codes, used only for status emphasis. Deliberately a short list: a CLI
// that paints half its output is harder to read, not easier.
const (
	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiDim    = "\x1b[2m"
)

// colorize wraps s in an ANSI code when colour is on.
func colorize(on bool, code, s string) string {
	if !on || code == "" {
		return s
	}
	return code + s + ansiReset
}

// statusColor maps an environment or build status onto an emphasis colour.
// Unknown statuses render plain, which is what keeps a server that adds a state
// from producing garbage here.
func statusColor(s string) string {
	switch s {
	// Environment and build states.
	case "running", "completed":
		return ansiGreen
	case "build_failed", "deploy_failed", "failed":
		return ansiRed
	case "building", "deploying", "waking", "destroying", "requested",
		"pending", "dispatched", "queued", "in_progress":
		return ansiYellow
	case "destroyed", "canceled", "sleeping", "skipped":
		return ansiDim
	// Diagnostic states from `doctor` and `auth status`. Sharing one colour
	// table keeps a green "ok" and a green "running" the same green, and means
	// the WORD is the machine contract rather than a state name borrowed to get
	// the right colour.
	case "ok", "authenticated", "no auth required":
		return ansiGreen
	case "fail", "rejected", "unreachable", "not authenticated", "no context":
		return ansiRed
	case "warn":
		return ansiYellow
	case "skip":
		return ansiDim
	default:
		return ""
	}
}
