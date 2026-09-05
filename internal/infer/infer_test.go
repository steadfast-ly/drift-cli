package infer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseRemoteHandlesEveryFormGitStores(t *testing.T) {
	cases := []struct {
		url, owner, name string
		ok               bool
	}{
		{"https://github.com/auditsight/cyclops.git", "auditsight", "cyclops", true},
		{"https://github.com/auditsight/cyclops", "auditsight", "cyclops", true},
		{"git@github.com:auditsight/cyclops.git", "auditsight", "cyclops", true},
		// Host aliases select an SSH identity; the host is not what is extracted.
		{"git@github-au:auditsight/findstar.git", "auditsight", "findstar", true},
		{"ssh://git@github.com/auditsight/cyclops.git", "auditsight", "cyclops", true},
		{"https://github.com/auditsight/cyclops/", "auditsight", "cyclops", true},
		{"", "", "", false},
		{"cyclops", "", "", false},
	}
	for _, c := range cases {
		owner, name, ok := ParseRemote(c.url)
		if ok != c.ok || owner != c.owner || name != c.name {
			t.Errorf("ParseRemote(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.url, owner, name, ok, c.owner, c.name, c.ok)
		}
	}
}

func TestSlugifyProducesSomethingTheContractAccepts(t *testing.T) {
	cases := []struct{ branch, want string }{
		{"PROJ-1234-grid-pushdown", "proj-1234-grid-pushdown"},
		{"feature/PROJ-1234_grid", "feature-proj-1234-grid"},
		{"fix/thing.with.dots", "fix-thing-with-dots"},
		{"---leading-and-trailing---", "leading-and-trailing"},
		{"UPPER", "upper"},
		// Truncation must not leave a trailing hyphen: the pattern forbids one.
		{"a-branch-name-that-is-far-too-long-for-a-namespace", "a-branch-name-that-is-far-too"},
		{"cut-exactly-at-the-hyphen-xxx-yyy", "cut-exactly-at-the-hyphen-xxx"},
	}
	for _, c := range cases {
		got := Slugify(c.branch)
		if got != c.want {
			t.Errorf("Slugify(%q) = %q, want %q", c.branch, got, c.want)
		}
		if !ValidSlug(got) {
			t.Errorf("Slugify(%q) = %q, which the contract's pattern rejects", c.branch, got)
		}
	}
}

func TestSlugifyOnSomethingWithNoUsableCharacters(t *testing.T) {
	if got := Slugify("___"); got != "" {
		t.Fatalf("Slugify(%q) = %q, want empty", "___", got)
	}
	if ValidSlug("") {
		t.Fatal("an empty slug must not validate")
	}
}

func TestTicketIsCaseInsensitiveInAndUpperCaseOut(t *testing.T) {
	cases := []struct{ branch, want string }{
		{"PROJ-1234-grid-pushdown", "PROJ-1234"},
		{"proj-1234-grid-pushdown", "PROJ-1234"},
		{"feature/PROJ-7", "PROJ-7"},
		{"fix/some-thing", ""},
		// A single leading letter fails the contract's `[A-Z][A-Z0-9]+` and must
		// not be offered.
		{"a-1-thing", ""},
		{"no-numbers-here", ""},
	}
	for _, c := range cases {
		got := Ticket(c.branch)
		if got != c.want {
			t.Errorf("Ticket(%q) = %q, want %q", c.branch, got, c.want)
		}
		if got != "" && !ValidTicket(got) {
			t.Errorf("Ticket(%q) = %q, which the contract's pattern rejects", c.branch, got)
		}
	}
}

// fakeRun scripts external commands by their first two arguments.
func fakeRun(responses map[string]string, fail map[string]bool) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name
		if len(args) > 0 {
			key += " " + args[0]
		}
		if len(args) > 1 && name == "git" {
			key += " " + args[1]
		}
		if fail[key] {
			return nil, errors.New("exit status 1")
		}
		v, ok := responses[key]
		if !ok {
			return nil, errors.New("exit status 1")
		}
		return []byte(v), nil
	}
}

func TestDetectFillsEverythingWhenEverythingIsThere(t *testing.T) {
	run := fakeRun(map[string]string{
		"git rev-parse --git-dir":    ".git\n",
		"git remote get-url":         "git@github.com:acme/widget.git\n",
		"git rev-parse --abbrev-ref": "PROJ-1234-grid-pushdown\n",
		"gh pr":                      `[{"number":4633,"title":"Grid SQL pushdown","url":"https://github.com/acme/widget/pull/4633"}]`,
	}, nil)

	r := Detect(context.Background(), run)
	if !r.InRepo {
		t.Fatal("InRepo false")
	}
	if r.Remote != "acme/widget" {
		t.Errorf("Remote = %q", r.Remote)
	}
	if r.Branch != "PROJ-1234-grid-pushdown" {
		t.Errorf("Branch = %q", r.Branch)
	}
	if r.Slug != "proj-1234-grid-pushdown" {
		t.Errorf("Slug = %q", r.Slug)
	}
	if r.Ticket != "PROJ-1234" {
		t.Errorf("Ticket = %q", r.Ticket)
	}
	if r.PR == nil || r.PR.Number != 4633 || r.PR.Title != "Grid SQL pushdown" {
		t.Errorf("PR = %+v", r.PR)
	}
	if len(r.Notes) != 0 {
		t.Errorf("unexpected notes: %v", r.Notes)
	}
}

// A machine with no `gh` is the common case, not an error. Everything else
// must still be inferred, and the gap must be stated.
func TestDetectDegradesWhenGhIsAbsent(t *testing.T) {
	run := fakeRun(map[string]string{
		"git rev-parse --git-dir":    ".git\n",
		"git remote get-url":         "https://github.com/auditsight/findstar\n",
		"git rev-parse --abbrev-ref": "aus-10999-thing\n",
	}, nil)

	r := Detect(context.Background(), run)
	if r.PR != nil {
		t.Fatal("a PR was invented without gh")
	}
	if r.Slug != "aus-10999-thing" || r.Ticket != "AUS-10999" {
		t.Fatalf("inference stopped at the missing gh: %+v", r)
	}
	if !strings.Contains(strings.Join(r.Notes, " "), "pull request") {
		t.Fatalf("the gap was not reported: %v", r.Notes)
	}
}

func TestDetectOutsideAGitRepository(t *testing.T) {
	run := fakeRun(nil, map[string]bool{"git rev-parse --git-dir": true})
	r := Detect(context.Background(), run)
	if r.InRepo || r.Branch != "" || r.Remote != "" {
		t.Fatalf("inferred something outside a repository: %+v", r)
	}
	if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "not inside a git repository") {
		t.Fatalf("notes = %v", r.Notes)
	}
}

// A detached HEAD reports the literal "HEAD", which is not a branch anything
// can be built from.
func TestDetectRefusesADetachedHead(t *testing.T) {
	run := fakeRun(map[string]string{
		"git rev-parse --git-dir":    ".git\n",
		"git remote get-url":         "git@github.com:auditsight/cyclops.git\n",
		"git rev-parse --abbrev-ref": "HEAD\n",
	}, nil)
	r := Detect(context.Background(), run)
	if r.Branch != "" || r.Slug != "" {
		t.Fatalf("a detached HEAD produced a branch: %+v", r)
	}
	if !strings.Contains(strings.Join(r.Notes, " "), "detached") {
		t.Fatalf("notes = %v", r.Notes)
	}
}

func TestDetectReportsAnUnrecognisableRemote(t *testing.T) {
	run := fakeRun(map[string]string{
		"git rev-parse --git-dir":    ".git\n",
		"git remote get-url":         "/srv/git/bare-repo\n",
		"git rev-parse --abbrev-ref": "main\n",
	}, nil)
	r := Detect(context.Background(), run)
	if r.Remote != "" {
		t.Fatalf("Remote = %q, want empty", r.Remote)
	}
	if !strings.Contains(strings.Join(r.Notes, " "), "origin remote") {
		t.Fatalf("notes = %v", r.Notes)
	}
}

// `gh` that answers with something unparseable must be treated as no answer,
// not as a PR numbered zero.
func TestDetectIgnoresUnusableGhOutput(t *testing.T) {
	run := fakeRun(map[string]string{
		"git rev-parse --git-dir":    ".git\n",
		"git remote get-url":         "git@github.com:acme/thing.git\n",
		"git rev-parse --abbrev-ref": "topic\n",
		"gh pr":                      "not json",
	}, nil)
	if pr := Detect(context.Background(), run).PR; pr != nil {
		t.Fatalf("PR = %+v, want nil", pr)
	}
}

// A branch whose name is a number must not be read as a PR NUMBER.
//
// `gh pr view 4321` returns pull request #4321 — a different, unrelated PR
// whose title and URL would then be attached to the environment. `gh pr list
// --head` is a branch filter and nothing else, so the argument's SHAPE cannot
// change what is asked for.
func TestPRLookupIsAlwaysABranchFilterNeverAPullRequestNumber(t *testing.T) {
	var got []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch {
		case name == "git" && len(args) > 1 && args[1] == "--git-dir":
			return []byte(".git\n"), nil
		case name == "git" && args[0] == "remote":
			return []byte("git@github.com:acme/thing.git\n"), nil
		case name == "git" && len(args) > 1 && args[1] == "--abbrev-ref":
			return []byte("4321\n"), nil
		case name == "gh":
			got = append([]string{name}, args...)
			return []byte(`[]`), nil
		}
		return nil, errors.New("exit status 1")
	}

	r := Detect(context.Background(), run)
	if r.Branch != "4321" {
		t.Fatalf("Branch = %q", r.Branch)
	}
	if r.PR != nil {
		t.Fatalf("a PR was attached from an empty list: %+v", r.PR)
	}
	line := strings.Join(got, " ")
	if !strings.Contains(line, "--head 4321") {
		t.Fatalf("the branch was not passed as a branch filter: %q", line)
	}
	if strings.Contains(line, "pr view") {
		t.Fatalf("`gh pr view` would read %q as a pull request number: %q", r.Branch, line)
	}
}

// The whole of inference respects a deadline. `gh pr list` makes a network
// call, and the command context has none of its own.
func TestDetectHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run := func(c context.Context, _ string, _ ...string) ([]byte, error) {
		if err := c.Err(); err != nil {
			return nil, err
		}
		return []byte(".git\n"), nil
	}
	r := Detect(ctx, run)
	if r.InRepo {
		t.Fatal("inference ran past a cancelled context")
	}
}
