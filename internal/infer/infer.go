// Package infer works out what `drift env create` should create from the
// directory the operator is standing in.
//
// The premise is that an operator on a branch has already said everything the
// server needs: the repository is the git remote, the branch is HEAD, the
// ticket is in the branch name, and `gh` knows the pull request. Retyping all
// of that as flags is the difference between a command people use and one they
// look up.
//
// Every inference is a SUGGESTION. Each field has a flag that overrides it, the
// whole set is shown before anything is created, and nothing here can fail a
// command — a missing `gh`, a detached HEAD, a remote in an unusual form all
// degrade to "not inferred", never to an error. The failure mode of guessing
// wrong is an environment with a bad name; the failure mode of refusing to run
// because `gh` is absent is a CLI nobody can use on a fresh machine.
package infer

import (
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// MaxSlugLength is the server's limit on a slug (`openapi.json`, the create
// body's `slug.maxLength`). Slugs become Kubernetes namespace components, which
// is where the ceiling comes from.
const MaxSlugLength = 29

// PullRequest is what `gh` knows about the branch's PR.
type PullRequest struct {
	Number int
	Title  string
	URL    string
}

// Result is everything inferable, with nothing required.
type Result struct {
	// InRepo is false outside a git working tree. It is the switch that decides
	// whether inference is offered at all.
	InRepo bool
	// Remote is `owner/name` from the origin remote, empty when there is no
	// remote or its URL is not a form this understands.
	Remote string
	Branch string
	Slug   string
	Ticket string
	PR     *PullRequest

	// Notes records what could not be inferred and why, so `env create` can say
	// so rather than silently producing a half-filled plan.
	Notes []string
}

// Runner executes an external command. Injected so the tests drive the real
// parsing and formatting without a git repository or a GitHub account.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs the command for real.
//
// stderr is DISCARDED rather than surfaced: every caller here treats a non-zero
// exit as "not inferable", and letting git's "fatal: not a git repository" onto
// the CLI's stderr would make the normal case of running outside a repo look
// like a fault.
//
// `WaitDelay` is what actually makes the context deadline bite, and leaving it
// out was a bug that a deadline alone did not fix. `CommandContext` kills the
// process it started, but `Output` also waits for the stdout pipe to close —
// and a grandchild that inherited that pipe (a `gh` wrapper script, a git
// credential helper) keeps it open after its parent is gone, so the read blocks
// forever on a process nobody is waiting for. `WaitDelay` closes the pipes a
// moment after the kill, turning a hang into an error the caller degrades from.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

// Detect gathers everything it can. It never returns an error: the absence of
// an inference is data, and it is reported in Notes.
func Detect(ctx context.Context, run Runner) Result {
	if run == nil {
		run = ExecRunner
	}
	var r Result

	if _, err := run(ctx, "git", "rev-parse", "--git-dir"); err != nil {
		r.Notes = append(r.Notes, "not inside a git repository")
		return r
	}
	r.InRepo = true

	if out, err := run(ctx, "git", "remote", "get-url", "origin"); err == nil {
		if owner, name, ok := ParseRemote(strings.TrimSpace(string(out))); ok {
			r.Remote = owner + "/" + name
		} else {
			r.Notes = append(r.Notes, "the origin remote is not a recognisable GitHub URL")
		}
	} else {
		r.Notes = append(r.Notes, "this repository has no origin remote")
	}

	if out, err := run(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		branch := strings.TrimSpace(string(out))
		// A detached HEAD reports the literal string "HEAD", which is not a
		// branch anybody can build.
		if branch != "" && branch != "HEAD" {
			r.Branch = branch
			r.Slug = Slugify(branch)
			r.Ticket = Ticket(branch)
		} else {
			r.Notes = append(r.Notes, "HEAD is detached, so there is no branch to build")
		}
	}

	// `gh` is optional and its absence is unremarkable — it is not installed on
	// most CI images and plenty of developers do not use it.
	if r.Branch != "" {
		r.PR = detectPR(ctx, run, r.Branch)
		if r.PR == nil {
			r.Notes = append(r.Notes, "no pull request found for this branch (gh missing, unauthenticated, or no PR yet)")
		}
	}
	return r
}

// detectPR asks `gh` for the pull request open against this branch.
//
// `gh pr list --head` rather than `gh pr view <branch>`, and the difference is
// not stylistic. `gh pr view` accepts a NUMBER, a URL or a branch and decides
// which by looking at the argument, so on a branch named `4321` it returns pull
// request #4321 — a different, unrelated PR whose title and URL would then be
// attached to the environment. `--head` is unambiguous: it is a branch filter
// and nothing else.
func detectPR(ctx context.Context, run Runner, branch string) *PullRequest {
	out, err := run(ctx, "gh", "pr", "list", "--head", branch, "--state", "open",
		"--limit", "1", "--json", "number,title,url")
	if err != nil {
		return nil
	}
	var prs []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(out, &prs); err != nil || len(prs) == 0 || prs[0].Number <= 0 {
		return nil
	}
	return &PullRequest{Number: prs[0].Number, Title: prs[0].Title, URL: prs[0].URL}
}

// remotePattern matches the tail of a GitHub remote URL in any of the forms git
// actually stores: `https://host/owner/name(.git)`, `git@host:owner/name.git`,
// `ssh://git@host/owner/name.git`. Host aliases (`git@github-alt:…`, used to
// select an SSH identity) are matched too — the host is not what is being
// extracted.
var remotePattern = regexp.MustCompile(`(?:[:/])([^/:]+)/([^/]+?)(?:\.git)?/?$`)

// ParseRemote extracts `owner`, `name` from a remote URL.
func ParseRemote(url string) (owner, name string, ok bool) {
	url = strings.TrimSpace(url)
	if url == "" {
		return "", "", false
	}
	// A remote must NAME A HOST — either a scheme or the scp-like `user@host:`
	// form. Without this, a local bare repository at `/srv/git/thing` matches the
	// tail pattern and is reported as the repository `git/thing`, which is not a
	// GitHub repository at all and would then fail to resolve against the
	// server's list with a confusing message.
	if !strings.Contains(url, "://") && !strings.Contains(url, "@") {
		return "", "", false
	}
	m := remotePattern.FindStringSubmatch(url)
	if m == nil || m[1] == "" || m[2] == "" {
		return "", "", false
	}
	return m[1], m[2], true
}

var (
	nonSlug     = regexp.MustCompile(`[^a-z0-9]+`)
	ticketMatch = regexp.MustCompile(`(?i)(^|[^a-z0-9])([a-z][a-z0-9]+)-([0-9]+)($|[^0-9])`)
)

// Slugify turns a branch name into a slug the server will accept.
//
// The target is the contract's own pattern — `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`,
// at most 29 characters — so a branch like `feature/PROJ-1234_grid-pushdown`
// becomes `feature-proj-1234-grid-pushdo`. Truncation re-trims, because
// cutting mid-word can leave a trailing hyphen and the pattern forbids one.
func Slugify(branch string) string {
	s := nonSlug.ReplaceAllString(strings.ToLower(strings.TrimSpace(branch)), "-")
	s = strings.Trim(s, "-")
	if len(s) > MaxSlugLength {
		s = s[:MaxSlugLength]
		s = strings.TrimRight(s, "-")
	}
	return s
}

// Ticket extracts an issue key from a branch name, normalised to the form the
// contract requires (`^[A-Z][A-Z0-9]+-\d+$`).
//
// Case-insensitive on the way in because branch names are conventionally
// lowercase — `proj-1234-grid-pushdown` carries the ticket just as much as
// `PROJ-1234-grid-pushdown` does — and upper-cased on the way out because the
// server's pattern is not.
func Ticket(branch string) string {
	m := ticketMatch.FindStringSubmatch(branch)
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[2]) + "-" + m[3]
}

// ValidSlug reports whether a slug satisfies the contract's pattern, so a bad
// one is a usage error naming the rule rather than a 400 from the server.
var validSlug = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func ValidSlug(s string) bool {
	return s != "" && len(s) <= MaxSlugLength && validSlug.MatchString(s)
}

// ValidTicket reports whether a ticket id satisfies the contract's pattern.
var validTicket = regexp.MustCompile(`^[A-Z][A-Z0-9]+-\d+$`)

func ValidTicket(s string) bool { return validTicket.MatchString(s) }
