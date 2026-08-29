package cmd

import (
	"bufio"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/steadfast-ly/drift-cli/internal/cliexit"
	"github.com/steadfast-ly/drift-cli/internal/output"
)

// Interactive reports whether this invocation may prompt a human.
//
// ALL THREE streams must be terminals, and that is stricter than it first
// looks. The prompt is written to stderr and the answer read from stdin, so
// those two are obviously required; stdout is required as well because a
// redirected stdout is the signature of a script, and a script that stops to
// ask a question it cannot receive an answer to is a hung job rather than a
// safe one. `drift env rm x > out.txt` therefore refuses without `--yes`
// instead of prompting, which is the fail-closed direction.
//
// The predicate is `output.IsTerminal`, which is `golang.org/x/term` — NOT a
// `ModeCharDevice` stat. /dev/null is a character device, so the stat form
// answers "terminal" for `> /dev/null` and the confirmation for a destructive
// command would be skipped on the exact redirect people use to silence one.
func (a *App) Interactive() bool {
	return output.IsTerminal(a.Stdout) &&
		output.IsTerminal(a.Stderr) &&
		output.IsTerminalReader(a.Stdin)
}

// Confirm gates a destructive operation.
//
// Three outcomes, and the third is the one that matters: `--yes` proceeds, a
// terminal asks, and anything else REFUSES. A non-interactive invocation that
// silently proceeded would make `drift env rm` in a script a different, more
// dangerous command than the same line typed by hand.
//
// `summary` is what is about to happen, in the operator's terms, and it is
// printed in ALL THREE cases — including under `--yes`.
//
// That last part was a real gap rather than a nicety. `env create` infers its
// slug, ticket, repository and pull request from the working directory, and
// with `--yes` skipping the summary an environment was created from four
// guessed fields with none of them shown, contradicting the command's own
// documentation. `--yes` waives the QUESTION, not the disclosure. It goes to
// stderr, so it never contaminates `-o json`.
func (a *App) Confirm(yes bool, summary, question string) error {
	if yes {
		fmt.Fprintln(a.Stderr, summary)
		return nil
	}
	if !a.Interactive() {
		return &cliexit.ExitError{
			Code:    cliexit.Usage,
			Message: "refusing to " + question + " without confirmation",
			Detail:  summary + "\nthis is a destructive operation and the session is not interactive",
			Hint:    "pass --yes to confirm, or run it from a terminal",
		}
	}

	fmt.Fprintln(a.Stderr, summary)
	fmt.Fprintf(a.Stderr, "%s? [y/N] ", capitalize(question))

	sc := bufio.NewScanner(a.Stdin)
	if !sc.Scan() {
		fmt.Fprintln(a.Stderr)
		return &cliexit.ExitError{Code: cliexit.Usage, Message: "cancelled"}
	}
	switch strings.ToLower(strings.TrimSpace(sc.Text())) {
	case "y", "yes":
		return nil
	default:
		// Exit 1, not 0: a script that asked for something and did not get it
		// has not succeeded. Not exit 2 either — the invocation was well formed,
		// the operator simply said no.
		return &cliexit.ExitError{Code: cliexit.Error, Message: "cancelled"}
	}
}

// capitalize upper-cases the first RUNE.
//
// `strings.ToUpper(s[:1])` slices bytes: it panics on an empty string and
// mangles any prompt whose first character is multi-byte.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}
