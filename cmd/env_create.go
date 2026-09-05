package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/steadfast-ly/drift-cli/internal/api"
	"github.com/steadfast-ly/drift-cli/internal/client"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
	"github.com/steadfast-ly/drift-cli/internal/infer"
	"github.com/steadfast-ly/drift-cli/internal/wait"
)

// inferTimeout bounds the whole of the working-directory inference — every
// `git` and `gh` invocation together. Generous for local commands, short enough
// that a `gh` blocked on a dead network does not hang the CLI.
const inferTimeout = 5 * time.Second

// createFlags is every field of a create, each of which overrides whatever the
// working directory implied.
type createFlags struct {
	slug     string
	ticket   string
	repos    []string
	ttlHours int
	public   bool
	prNumber int
	prTitle  string
	prURL    string
	yes      bool
	noInfer  bool
	wait     waitFlags
}

func newEnvCreateCommand(app *App) *cobra.Command {
	f := &createFlags{}
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a preview environment",
		Long: "Create a preview environment.\n\n" +
			"Run from a checkout, everything is inferred from where you are\n" +
			"standing: the repository from the `origin` remote, the branch from\n" +
			"HEAD, the slug and ticket from the branch name, and the pull request\n" +
			"number, title and URL from `gh` when it is installed and\n" +
			"authenticated. Every field has a flag that overrides it.\n\n" +
			"OUTSIDE a git repository, or when the session is not interactive,\n" +
			"nothing is inferred and --slug and --repo are required. Inference is\n" +
			"a convenience for a human at a keyboard who can see what it proposed\n" +
			"and say no; a script must say what it means.\n\n" +
			"--repo takes `name:branch` and repeats, which is how a multi-service\n" +
			"environment is described. The name is resolved to an id client-side\n" +
			"against the server's repository list.\n\n" +
			"Blocks until the environment is running.\n\n" + cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runEnvCreate(c.Context(), app, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.slug, "slug", "", "environment slug (inferred from the branch name)")
	fl.StringVar(&f.ticket, "ticket", "", "issue key, e.g. PROJ-1234 (inferred from the branch name)")
	fl.StringArrayVar(&f.repos, "repo", nil, "`name:branch` to include; repeat for a multi-service environment")
	fl.IntVar(&f.ttlHours, "ttl", 0, "lifetime in hours (server default 48, maximum 120)")
	fl.BoolVar(&f.public, "public", false, "make the environment reachable without the VPN")
	fl.IntVar(&f.prNumber, "pr", 0, "pull request number (inferred via gh)")
	fl.StringVar(&f.prTitle, "pr-title", "", "pull request title (inferred via gh)")
	fl.StringVar(&f.prURL, "pr-url", "", "pull request URL (inferred via gh)")
	fl.BoolVar(&f.yes, "yes", false, "skip the confirmation prompt")
	fl.BoolVar(&f.noInfer, "no-infer", false, "ignore the working directory and use only flags")
	f.wait.register(cmd, policyCreate)
	return cmd
}

// plan is what will be created, after inference and overrides.
type plan struct {
	Slug   string
	Ticket string
	TTL    int
	Public bool
	Repos  []planRepo
	PR     *infer.PullRequest
	// Inferred records which fields came from the working directory, so the
	// confirmation can show what was guessed rather than what was typed.
	Inferred map[string]bool
}

type planRepo struct {
	Name   string
	Branch string
	ID     uuid.UUID
}

func runEnvCreate(ctx context.Context, app *App, f *createFlags) error {
	// Inference is offered only to a human who can see it and refuse. A
	// non-interactive session cannot be shown a plan, so it does not get one —
	// and the same rule covers a directory that is not a checkout.
	//
	// The interactivity test is the same `golang.org/x/term` predicate that
	// decides whether a destructive command may prompt, so `> /dev/null` reads
	// as scripted here too rather than quietly inferring a slug into a file.
	var detected infer.Result
	inferring := !f.noInfer && app.Interactive()
	if inferring {
		// BOUNDED, on its own deadline. cobra is driven through `root.Execute()`,
		// so the command context has no deadline of its own, and `--timeout`
		// configures the HTTP client rather than these subprocesses. `gh pr view`
		// makes a network call, so a VPN flap is enough to hang `env create`
		// indefinitely before it has issued a single request. Every inference
		// already degrades to "not inferred", which makes a short deadline free.
		detectCtx, cancel := context.WithTimeout(ctx, inferTimeout)
		detected = infer.Detect(detectCtx, infer.ExecRunner)
		cancel()
		if !detected.InRepo {
			inferring = false
		}
	}

	if err := f.wait.validate(); err != nil {
		return err
	}

	// Reported BEFORE the plan is built, so that a gap explains the usage error
	// it causes. Emitted after it, the note for a detached HEAD arrived only on
	// the runs that had already succeeded, and the failure told the operator to
	// read notes that were never printed.
	for _, n := range detected.Notes {
		app.Out.Infof("note: %s", n)
	}

	p, err := buildPlan(f, detected, inferring)
	if err != nil {
		return err
	}

	sess, err := app.Connect(ctx, FeatureEnvironmentsWrite, FeatureRepositoriesRead)
	if err != nil {
		return err
	}
	// Names are resolved BEFORE the confirmation, so what is shown is what the
	// server will act on rather than a name that turns out not to exist.
	for i := range p.Repos {
		id, err := resolveRepository(ctx, sess, p.Repos[i].Name)
		if err != nil {
			return err
		}
		p.Repos[i].ID = id
	}

	if err := app.Confirm(f.yes, p.summary(), "create this environment"); err != nil {
		return err
	}

	resp, err := sess.API.EnvironmentsCreateWithResponse(ctx, p.body())
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		e := client.Fail(resp, resp.Headers429)
		if resp.JSON409 != nil {
			e.Hint = fmt.Sprintf(
				"a slug is unique among live environments; `drift env get %s` shows the one holding it", p.Slug)
		}
		return e
	}

	ref := &envRef{ID: resp.JSON200.EnvironmentId, Slug: p.Slug, Status: api.EnvironmentStatusRequested}
	if !f.wait.shouldWait(policyCreate) {
		return writeMutation(app, "create", ref, currentStatus(ctx, sess, ref), false)
	}
	final, waitErr := waitForEnv(ctx, app, sess, ref,
		policyCreate.Goal, f.wait.deadline(policyCreate), true)
	if werr := writeMutation(app, "create", ref, final, true); werr != nil && waitErr == nil {
		return werr
	}
	return waitErr
}

// body renders the plan as the contract's create request.
//
// The repos element is an ANONYMOUS struct in the generated client, so it is
// filled in place after growing the slice rather than by writing the type out
// again. Restating it here would be a second copy of a generated shape that
// nothing checks — exactly the drift `make check-generated` exists to prevent.
func (p *plan) body() api.EnvironmentsCreateJSONRequestBody {
	body := api.EnvironmentsCreateJSONRequestBody{Slug: p.Slug}
	if p.Ticket != "" {
		ticket := p.Ticket
		body.TicketId = &ticket
	}
	if p.TTL > 0 {
		ttl := p.TTL
		body.TtlHours = &ttl
	}
	if p.Public {
		public := true
		body.IsPublic = &public
	}

	body.Repos = grow(body.Repos, len(p.Repos))
	for i, r := range p.Repos {
		body.Repos[i].RepositoryId = r.ID
		body.Repos[i].Branch = r.Branch
		// The pull request describes ONE branch, so it rides on every service
		// only in the case that produced it: a single inferred repository built
		// from the branch the PR is open against. `buildPlan` is where that is
		// decided; by here `p.PR` is either meant for these services or nil.
		if p.PR != nil {
			n, title, url := p.PR.Number, p.PR.Title, p.PR.URL
			body.Repos[i].PrNumber = &n
			if title != "" {
				body.Repos[i].PrTitle = &title
			}
			if url != "" {
				body.Repos[i].PrUrl = &url
			}
		}
	}
	return body
}

// grow extends a slice by n zero values without naming its element type, which
// is what makes it usable against the generated client's anonymous structs.
func grow[S ~[]E, E any](s S, n int) S { return append(s, make(S, n)...) }

// buildPlan merges inference with flags and refuses anything the contract will.
func buildPlan(f *createFlags, d infer.Result, inferring bool) (*plan, error) {
	p := &plan{TTL: f.ttlHours, Public: f.public, Inferred: map[string]bool{}}

	switch {
	case f.slug != "":
		p.Slug = f.slug
	case inferring && d.Slug != "":
		p.Slug = d.Slug
		p.Inferred["slug"] = true
	}
	switch {
	case f.ticket != "":
		p.Ticket = f.ticket
	case inferring && d.Ticket != "":
		p.Ticket = d.Ticket
		p.Inferred["ticket"] = true
	}

	switch {
	case len(f.repos) > 0:
		for _, spec := range f.repos {
			name, branch, err := splitRepoBranch(spec)
			if err != nil {
				return nil, err
			}
			p.Repos = append(p.Repos, planRepo{Name: name, Branch: branch})
		}
	case inferring && d.Remote != "" && d.Branch != "":
		p.Repos = append(p.Repos, planRepo{Name: d.Remote, Branch: d.Branch})
		p.Inferred["repo"] = true
	}

	// A PR given on the command line wins whole; otherwise the inferred one is
	// used only when a single repository was ALSO inferred, since attaching one
	// branch's pull request to a hand-listed set of services would be a
	// fabrication.
	switch {
	case f.prNumber > 0:
		p.PR = &infer.PullRequest{Number: f.prNumber, Title: f.prTitle, URL: f.prURL}
	case inferring && d.PR != nil && p.Inferred["repo"]:
		p.PR = d.PR
		p.Inferred["pr"] = true
	}

	if err := p.validate(inferring); err != nil {
		return nil, err
	}
	return p, nil
}

// validate refuses locally what the server would refuse remotely.
//
// Checked client-side because the messages are better here: the CLI can name
// the flag to pass and the rule that was broken, where a 400 can only describe
// the field it received.
func (p *plan) validate(inferring bool) error {
	if p.Slug == "" {
		return missingField("--slug", "slug", inferring)
	}
	if !infer.ValidSlug(p.Slug) {
		return usageErrorf(
			"slug %q is not usable: at most %d characters of lower-case letters, digits and hyphens, "+
				"starting and ending with a letter or digit", p.Slug, infer.MaxSlugLength)
	}
	if p.Ticket != "" && !infer.ValidTicket(p.Ticket) {
		return usageErrorf("ticket %q is not an issue key (expected something like PROJ-1234)", p.Ticket)
	}
	if len(p.Repos) == 0 {
		return missingField("--repo name:branch", "repository", inferring)
	}
	if p.TTL < 0 || p.TTL > 120 {
		return usageErrorf("--ttl must be between 1 and 120 hours")
	}
	seen := map[string]bool{}
	for _, r := range p.Repos {
		key := strings.ToLower(r.Name)
		if seen[key] {
			return usageErrorf("--repo names %s twice", r.Name)
		}
		seen[key] = true
	}
	return nil
}

// missingField explains a gap in the terms of why it exists, because "slug is
// required" is unhelpful to someone who expected it to be inferred.
func missingField(flag, what string, inferring bool) error {
	e := &cliexit.ExitError{
		Code:    cliexit.Usage,
		Message: fmt.Sprintf("no %s: pass %s", what, flag),
	}
	if inferring {
		e.Detail = "it could not be inferred from this working directory"
		e.Hint = "the notes above say what was missing"
	} else {
		e.Detail = "inference is only offered inside a git repository on an interactive terminal"
		e.Hint = "pass the fields explicitly, which is also what a script should do"
	}
	return e
}

// summary is what the operator is asked to approve.
//
// Inferred fields are marked, because the difference between a value the
// operator typed and one the CLI guessed from a branch name is exactly what
// they are being asked to check.
func (p *plan) summary() string {
	var b strings.Builder
	b.WriteString("Create environment:\n")
	fmt.Fprintf(&b, "  slug     %s%s\n", p.Slug, inferredMark(p.Inferred["slug"]))
	if p.Ticket != "" {
		fmt.Fprintf(&b, "  ticket   %s%s\n", p.Ticket, inferredMark(p.Inferred["ticket"]))
	}
	for _, r := range p.Repos {
		fmt.Fprintf(&b, "  service  %s @ %s%s\n", r.Name, r.Branch, inferredMark(p.Inferred["repo"]))
	}
	if p.PR != nil {
		title := p.PR.Title
		if title == "" {
			title = "(no title)"
		}
		fmt.Fprintf(&b, "  pr       #%d %s%s\n", p.PR.Number, title, inferredMark(p.Inferred["pr"]))
		// The URL is SENT, so it is shown. Displaying the number and title while
		// silently attaching a link the operator never saw is the one part of
		// the plan they could not check.
		if p.PR.URL != "" {
			fmt.Fprintf(&b, "           %s\n", p.PR.URL)
		}
	}
	if p.TTL > 0 {
		fmt.Fprintf(&b, "  ttl      %dh\n", p.TTL)
	} else {
		b.WriteString("  ttl      48h (server default)\n")
	}
	if p.Public {
		b.WriteString("  access   public — reachable without the VPN\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func inferredMark(yes bool) string {
	if yes {
		return "   (inferred)"
	}
	return ""
}

// --- env wait ---------------------------------------------------------------

func newEnvWaitCommand(app *App) *cobra.Command {
	var forState string

	cmd := &cobra.Command{
		Use:   "wait <slug-or-id>",
		Short: "Wait for an environment to reach a state",
		Long: "Block until an environment reaches a state, then exit.\n\n" +
			"Exists for both directions: to follow a mutation that was started\n" +
			"with --no-wait, and to wait on an environment somebody else created.\n\n" +
			"Failure is decided from the lifecycle machine rather than from the\n" +
			"state's NAME. `deploy_failed` is not terminal — the server documents\n" +
			"`deploying -> deploy_failed -> running` as an expected ArgoCD blip —\n" +
			"so a failure state is only believed once it persists across several\n" +
			"consecutive polls with no build in flight.\n\n" +
			"A state the environment can never reach on its own fails immediately\n" +
			"rather than burning the timeout: waiting for `running` on a sleeping\n" +
			"environment cannot succeed, because WAKE needs a command.\n\n" +
			"Exit 6 on timeout.\n\n" + cliexit.Help,
		Args: exactArgs(1, "the environment slug or id"),
		RunE: func(c *cobra.Command, args []string) error {
			goal, ok := wait.ParseState(forState)
			if !ok {
				return usageErrorf("unknown state %q; valid states: %s",
					forState, strings.Join(wait.WaitableStates(), ", "))
			}
			d, err := c.Flags().GetDuration("timeout")
			if err != nil {
				return usageErrorf("%s", err.Error())
			}
			if !c.Flags().Changed("timeout") {
				d = wait.DefaultTimeoutFor(goal)
			}
			return runEnvWait(c.Context(), app, args[0], goal, d)
		},
	}
	cmd.Flags().StringVar(&forState, "for", string(api.EnvironmentStatusRunning),
		"state to wait for: "+strings.Join(wait.WaitableStates(), ", "))
	// A LOCAL --timeout shadows the root's per-request deadline for this command
	// only. `drift env wait --for destroyed --timeout 20m` is the spelling
	// DESIGN.md specifies, and a wait is the one command where the interesting
	// deadline is the wait's rather than the individual request's; the per-request
	// default still applies underneath.
	cmd.Flags().Duration("timeout", 0,
		"how long to wait before giving up (default depends on --for; destroyed gets 20m)")
	return cmd
}

func runEnvWait(ctx context.Context, app *App, ref string, goal api.EnvironmentStatus, timeout time.Duration) error {
	sess, err := app.Connect(ctx, FeatureEnvironmentsRead)
	if err != nil {
		return err
	}
	e, err := resolveEnv(ctx, sess, ref)
	if err != nil {
		return err
	}
	// NOT commanded: `drift env wait` observes work somebody else started, so a
	// goal with no server-raised path into it is worth reporting rather than
	// waiting out — that is what makes `--for running` on a sleeping environment
	// answer in seconds.
	final, waitErr := waitForEnv(ctx, app, sess, e, goal, timeout, false)
	if werr := writeMutation(app, "wait", e, final, true); werr != nil && waitErr == nil {
		return werr
	}
	return waitErr
}
