package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/steadfast-ly/drift-cli/internal/api"
	"github.com/steadfast-ly/drift-cli/internal/client"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
	"github.com/steadfast-ly/drift-cli/internal/output"
	"github.com/steadfast-ly/drift-cli/internal/wait"
)

// waitPolicy is a command's default answer to "should this block?".
//
// Blocking is decided per COMMAND rather than globally, because the honest
// answer differs: an operator who asks for an environment wants the
// environment, so `create` blocks; an operator who asks for one to go away has
// nothing further to do with it, and destroy convergence is owned by a
// two-minute cron with an eight-minute escalation window, so `rm` blocking by
// default would routinely hold a terminal for over ten minutes for no benefit
// (DESIGN.md §5).
type waitPolicy struct {
	// Goal is the state `--wait` waits for.
	Goal api.EnvironmentStatus
	// Timeout is this command's default deadline.
	Timeout time.Duration
	// Blocks is whether the command waits when neither flag is given.
	Blocks bool
}

// The per-target defaults. Each timeout is sized to what the command actually
// triggers, not to one number that has to cover all of them.
var (
	// A container build per service, then an ArgoCD rollout.
	policyCreate = waitPolicy{api.EnvironmentStatusRunning, 30 * time.Minute, true}
	// Same shape as create: relaunch rebuilds.
	policyRelaunch = waitPolicy{api.EnvironmentStatusRunning, 30 * time.Minute, true}
	// No build — scale up and wait for ArgoCD to report healthy.
	policyWake = waitPolicy{api.EnvironmentStatusRunning, 10 * time.Minute, true}
	// One build, then the rollout that follows it.
	policyRetryBuild = waitPolicy{api.EnvironmentStatusRunning, 30 * time.Minute, true}
	// Destroy convergence is a cron with an escalation window; ten minutes is
	// routine, so the default is well clear of it.
	policyDestroy = waitPolicy{api.EnvironmentStatusDestroyed, 20 * time.Minute, false}
	// A scale-to-zero. Fast, but not waited on by default: nobody is blocked on
	// the result.
	policySleep = waitPolicy{api.EnvironmentStatusSleeping, 10 * time.Minute, false}
	// Cancel is a database transition; the wait exists only for symmetry.
	policyCancel = waitPolicy{api.EnvironmentStatusCanceled, 5 * time.Minute, false}
	// A build dispatched against a new branch.
	policySwapBranch = waitPolicy{api.EnvironmentStatusRunning, 30 * time.Minute, true}
	// Adding a service dispatches its first build.
	policyAddService = waitPolicy{api.EnvironmentStatusRunning, 30 * time.Minute, true}
)

// waitFlags is the `--wait` / `--no-wait` / `--wait-timeout` trio.
//
// Two booleans rather than one tri-state, because cobra's `Changed` is the only
// way to tell "the user passed --wait=false" from "the user passed nothing",
// and a command whose default is to block needs that difference.
type waitFlags struct {
	wait    bool
	noWait  bool
	timeout time.Duration
	cmd     *cobra.Command
}

func (w *waitFlags) register(cmd *cobra.Command, p waitPolicy) {
	w.cmd = cmd
	verb := "wait for the environment to reach " + string(p.Goal)
	cmd.Flags().BoolVar(&w.wait, "wait", p.Blocks, verb)
	cmd.Flags().BoolVar(&w.noWait, "no-wait", !p.Blocks, "return as soon as the server accepts the request")
	cmd.Flags().DurationVar(&w.timeout, "wait-timeout", p.Timeout, "how long to wait before giving up (exit 6)")
}

// validate rejects the contradiction.
//
// Checked here rather than with cobra's `MarkFlagsMutuallyExclusive`, which
// reports the clash as a plain error and so exits 1 — indistinguishable, from a
// script, from the server having failed. A malformed invocation is exit 2.
func (w *waitFlags) validate() error {
	if w.cmd == nil {
		return nil
	}
	if w.cmd.Flags().Changed("wait") && w.cmd.Flags().Changed("no-wait") {
		return usageErrorf("--wait and --no-wait contradict each other")
	}
	return nil
}

// shouldWait applies the precedence: an explicit flag wins, otherwise the
// command's own default.
func (w *waitFlags) shouldWait(p waitPolicy) bool {
	switch {
	case w.cmd != nil && w.cmd.Flags().Changed("no-wait"):
		return !w.noWait
	case w.cmd != nil && w.cmd.Flags().Changed("wait"):
		return w.wait
	default:
		return p.Blocks
	}
}

func (w *waitFlags) deadline(p waitPolicy) time.Duration {
	if w.timeout > 0 {
		return w.timeout
	}
	return p.Timeout
}

// envRef is an environment resolved to the id the mutation endpoints require.
//
// Every mutation path in the contract takes `environmentId` as a UUID; only
// `GET /environments/{ref}` accepts a slug. So a slug is resolved through the
// SERVER's own lookup rather than by listing and filtering client-side, which
// would disagree with it about which environment a reused slug names (the
// server's `getEnvironmentBySlug` excludes destroyed and canceled rows).
type envRef struct {
	ID     uuid.UUID
	Slug   string
	Status api.EnvironmentStatus
	Detail *api.EnvironmentDetail
}

// resolveEnv turns what the user typed into an id.
//
// A ref that already parses as a UUID still costs one GET, because the
// mutation's own 404 cannot distinguish "no such environment" from "this
// environment cannot be addressed", and because the services commands need the
// detail anyway. The read floor is `read-only`, so it never fails for a caller
// whose credential could perform the mutation.
func resolveEnv(ctx context.Context, sess *Session, ref string) (*envRef, error) {
	resp, err := sess.API.EnvironmentsGetWithResponse(ctx, ref)
	if err != nil {
		return nil, client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		e := client.Fail(resp, resp.Headers429)
		if resp.JSON404 != nil {
			e.Hint = fmt.Sprintf(
				"a slug resolves only live environments; if %q was destroyed or canceled, address it by id", ref)
		}
		return nil, e
	}
	d := *resp.JSON200
	return &envRef{
		ID: d.Environment.Id, Slug: d.Environment.Slug,
		Status: d.Environment.Status, Detail: &d,
	}, nil
}

// mutationColumns is the shape every lifecycle command prints.
//
// One row, always the same fields, so `drift env sleep x --json status` and
// `drift env wake x --json status` are the same contract and a script can treat
// the whole family uniformly.
func mutationColumns() []output.Column {
	return []output.Column{
		{Name: "action", Header: "Action"},
		{Name: "slug", Header: "Slug"},
		output.StatusColumn("status", "Status"),
		{Name: "waited", Header: "Waited"},
		{Name: "id", Header: "Id", Wide: true},
	}
}

// mutationResult renders the outcome of a lifecycle command.
func writeMutation(app *App, action string, e *envRef, status api.EnvironmentStatus, waited bool) error {
	cols := mutationColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	return app.Out.Write(&output.Doc{
		Columns: cols,
		Single:  true,
		Rows: []output.Row{{
			"action": action,
			"slug":   e.Slug,
			"status": string(status),
			"waited": waited,
			"id":     e.ID.String(),
		}},
	})
}

// runMutation is the shared body of every lifecycle command.
//
// `mutate` performs the call and returns the id the server acknowledged;
// everything else — resolution, the optional wait, the final read, the output
// — is identical across the family and lives here so that it cannot drift
// between twelve commands.
func runMutation(
	ctx context.Context, app *App, action, ref string,
	policy waitPolicy, flags *waitFlags,
	mutate func(ctx context.Context, sess *Session, e *envRef) error,
) error {
	if err := flags.validate(); err != nil {
		return err
	}
	sess, err := app.Connect(ctx, FeatureEnvironmentsWrite)
	if err != nil {
		return err
	}
	e, err := resolveEnv(ctx, sess, ref)
	if err != nil {
		return err
	}
	if err := mutate(ctx, sess, e); err != nil {
		return err
	}

	if !flags.shouldWait(policy) {
		// Report the state as it is NOW rather than the state before the
		// mutation. One extra read, and the difference between "sleeping" and a
		// stale "running" is the whole value of the line.
		status := currentStatus(ctx, sess, e)
		return writeMutation(app, action, e, status, false)
	}

	final, waitErr := waitForEnv(ctx, app, sess, e, policy.Goal, flags.deadline(policy), true)
	// The result is printed even on failure: the operator needs to know which
	// environment and what state it ended in, and a bare error line does not say.
	if werr := writeMutation(app, action, e, final, true); werr != nil && waitErr == nil {
		return werr
	}
	return waitErr
}

// currentStatus re-reads the environment after a mutation, degrading to the
// pre-mutation state rather than failing: the mutation SUCCEEDED, and turning a
// cosmetic follow-up read into a non-zero exit would be a lie about what
// happened.
func currentStatus(ctx context.Context, sess *Session, e *envRef) api.EnvironmentStatus {
	resp, err := sess.API.EnvironmentsGetWithResponse(ctx, e.ID.String())
	if err != nil || resp.JSON200 == nil {
		return e.Status
	}
	return resp.JSON200.Environment.Status
}

// waitForEnv polls an environment to a goal state.
//
// `commanded` says whether THIS invocation already asked for the transition. It
// is the difference between `drift env sleep x --wait`, where a state the
// machine cannot leave unaided means the write has not been observed yet, and
// `drift env wait x --for sleeping`, where nobody here asked for anything and
// the same observation is worth reporting as futile.
func waitForEnv(
	ctx context.Context, app *App, sess *Session, e *envRef,
	goal api.EnvironmentStatus, timeout time.Duration, commanded bool,
) (api.EnvironmentStatus, error) {
	// Animation follows the stream it is WRITTEN to. Progress goes to stderr, so
	// stderr decides — not stdout, which may well be a redirected JSON file
	// while the operator is still watching the terminal.
	progress := wait.NewProgress(app.Stderr, output.IsTerminal(app.Stderr) && app.Out.ErrColor)
	return wait.Wait(ctx, wait.Options{
		Goal:          goal,
		Timeout:       timeout,
		Interval:      app.waitInterval,
		FailureWindow: app.waitFailureWindow,
		Commanded:     commanded,
		Ref:           e.Slug,
		Reporter:      progress,
	}, func(ctx context.Context) (wait.Observation, error) {
		resp, err := sess.API.EnvironmentsGetWithResponse(ctx, e.ID.String())
		if err != nil {
			return wait.Observation{}, client.Transport(err, sess.Resolved.Endpoint)
		}
		if resp.JSON200 == nil {
			return wait.Observation{}, client.Fail(resp, resp.Headers429)
		}
		obs := wait.Observation{Status: resp.JSON200.Environment.Status}
		for _, b := range resp.JSON200.Builds {
			if wait.BuildInFlight(b.Status) {
				obs.BuildInFlight = true
				break
			}
		}
		return obs, nil
	})
}

// --- the commands -----------------------------------------------------------

func newEnvRmCommand(app *App) *cobra.Command {
	var yes bool
	flags := &waitFlags{}
	cmd := &cobra.Command{
		Use:     "rm <slug-or-id>",
		Aliases: []string{"delete", "destroy"},
		Short:   "Tear an environment down",
		Long: "Tear an environment down.\n\n" +
			"Returns as soon as the server accepts the request. Destroy convergence\n" +
			"is owned by a cron with an escalation window and routinely exceeds ten\n" +
			"minutes, so blocking by default would hold a terminal for the whole of\n" +
			"it; pass --wait to follow it to `destroyed`.\n\n" +
			"Destructive: confirms on a terminal, takes --yes, and refuses without\n" +
			"--yes when the session is not interactive.\n\n" + cliexit.Help,
		Args: exactArgs(1, "the environment slug or id"),
		RunE: func(c *cobra.Command, args []string) error {
			return runMutation(c.Context(), app, "rm", args[0], policyDestroy, flags,
				func(ctx context.Context, sess *Session, e *envRef) error {
					if err := app.Confirm(yes,
						fmt.Sprintf("This tears down %s (%s), including its namespace and data.", e.Slug, e.ID),
						"destroy this environment"); err != nil {
						return err
					}
					resp, err := sess.API.EnvironmentsDestroyWithResponse(ctx, e.ID)
					if err != nil {
						return client.Transport(err, sess.Resolved.Endpoint)
					}
					if resp.JSON200 == nil {
						return client.Fail(resp, resp.Headers429)
					}
					return nil
				})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	flags.register(cmd, policyDestroy)
	return cmd
}

func newEnvCancelCommand(app *App) *cobra.Command {
	flags := &waitFlags{}
	cmd := &cobra.Command{
		Use:   "cancel <slug-or-id>",
		Short: "Cancel an environment that is still building",
		Long: "Cancel an environment that is still building.\n\n" +
			"Only legal from `building`; a running environment is torn down with\n" +
			"`drift env rm`. Returns immediately.\n\n" + cliexit.Help,
		Args: exactArgs(1, "the environment slug or id"),
		RunE: func(c *cobra.Command, args []string) error {
			return runMutation(c.Context(), app, "cancel", args[0], policyCancel, flags,
				func(ctx context.Context, sess *Session, e *envRef) error {
					resp, err := sess.API.EnvironmentsCancelWithResponse(ctx, e.ID)
					if err != nil {
						return client.Transport(err, sess.Resolved.Endpoint)
					}
					if resp.JSON200 == nil {
						return client.Fail(resp, resp.Headers429)
					}
					return nil
				})
		},
	}
	flags.register(cmd, policyCancel)
	return cmd
}

func newEnvRelaunchCommand(app *App) *cobra.Command {
	var yes bool
	flags := &waitFlags{}
	cmd := &cobra.Command{
		Use:   "relaunch <slug-or-id>",
		Short: "Rebuild and redeploy every service",
		Long: "Rebuild and redeploy every service from the current branch heads.\n\n" +
			"Destructive: running pods are replaced, so anything in the\n" +
			"environment's ephemeral state is lost. Confirms on a terminal, takes\n" +
			"--yes, and refuses without --yes when the session is not interactive.\n\n" +
			"Blocks until the environment is running again.\n\n" + cliexit.Help,
		Args: exactArgs(1, "the environment slug or id"),
		RunE: func(c *cobra.Command, args []string) error {
			return runMutation(c.Context(), app, "relaunch", args[0], policyRelaunch, flags,
				func(ctx context.Context, sess *Session, e *envRef) error {
					if err := app.Confirm(yes,
						fmt.Sprintf("This rebuilds every service in %s and replaces its running pods.", e.Slug),
						"relaunch this environment"); err != nil {
						return err
					}
					resp, err := sess.API.EnvironmentsRelaunchWithResponse(ctx, e.ID)
					if err != nil {
						return client.Transport(err, sess.Resolved.Endpoint)
					}
					if resp.JSON200 == nil {
						return client.Fail(resp, resp.Headers429)
					}
					return nil
				})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	flags.register(cmd, policyRelaunch)
	return cmd
}

func newEnvSleepCommand(app *App) *cobra.Command {
	flags := &waitFlags{}
	cmd := &cobra.Command{
		Use:   "sleep <slug-or-id>",
		Short: "Scale an environment to zero",
		Long: "Scale a running environment to zero, keeping its data and its TTL.\n\n" +
			"Returns immediately; `drift env wake` brings it back.\n\n" + cliexit.Help,
		Args: exactArgs(1, "the environment slug or id"),
		RunE: func(c *cobra.Command, args []string) error {
			return runMutation(c.Context(), app, "sleep", args[0], policySleep, flags,
				func(ctx context.Context, sess *Session, e *envRef) error {
					resp, err := sess.API.EnvironmentsSleepWithResponse(ctx, e.ID)
					if err != nil {
						return client.Transport(err, sess.Resolved.Endpoint)
					}
					if resp.JSON200 == nil {
						return client.Fail(resp, resp.Headers429)
					}
					return nil
				})
		},
	}
	flags.register(cmd, policySleep)
	return cmd
}

func newEnvWakeCommand(app *App) *cobra.Command {
	flags := &waitFlags{}
	cmd := &cobra.Command{
		Use:   "wake <slug-or-id>",
		Short: "Bring a sleeping environment back",
		Long: "Scale a sleeping environment back up.\n\n" +
			"Blocks until it is running again — there is no build, so this is an\n" +
			"ArgoCD rollout and takes minutes rather than tens of minutes.\n\n" +
			cliexit.Help,
		Args: exactArgs(1, "the environment slug or id"),
		RunE: func(c *cobra.Command, args []string) error {
			return runMutation(c.Context(), app, "wake", args[0], policyWake, flags,
				func(ctx context.Context, sess *Session, e *envRef) error {
					resp, err := sess.API.EnvironmentsWakeWithResponse(ctx, e.ID)
					if err != nil {
						return client.Transport(err, sess.Resolved.Endpoint)
					}
					if resp.JSON200 == nil {
						return client.Fail(resp, resp.Headers429)
					}
					return nil
				})
		},
	}
	flags.register(cmd, policyWake)
	return cmd
}

func newEnvExtendCommand(app *App) *cobra.Command {
	var hours int
	flags := &waitFlags{}
	cmd := &cobra.Command{
		Use:   "extend <slug-or-id>",
		Short: "Push an environment's expiry out",
		Long: "Add hours to an environment's TTL.\n\n" +
			"The server caps the TOTAL lifetime, so a request that would exceed it\n" +
			"is refused with the remaining allowance rather than silently clamped.\n\n" +
			cliexit.Help,
		Args: exactArgs(1, "the environment slug or id"),
		RunE: func(c *cobra.Command, args []string) error {
			if hours <= 0 {
				return usageErrorf("--hours must be a positive number of hours")
			}
			return runMutation(c.Context(), app, "extend", args[0], waitPolicy{}, flags,
				func(ctx context.Context, sess *Session, e *envRef) error {
					resp, err := sess.API.EnvironmentsExtendWithResponse(ctx, e.ID,
						api.EnvironmentsExtendJSONRequestBody{AdditionalHours: hours})
					if err != nil {
						return client.Transport(err, sess.Resolved.Endpoint)
					}
					if resp.JSON200 == nil {
						return client.Fail(resp, resp.Headers429)
					}
					return nil
				})
		},
	}
	cmd.Flags().IntVar(&hours, "hours", 24, "hours to add to the TTL")
	// No wait flags: extend changes an expiry, not a lifecycle state, so there
	// is nothing to converge to and `--wait` would have no meaning.
	flags.cmd = cmd
	return cmd
}

func newEnvVisibilityCommand(app *App, public bool) *cobra.Command {
	use, short := "unshare <slug-or-id>", "Make an environment private again"
	if public {
		use, short = "share <slug-or-id>", "Make an environment publicly reachable"
	}
	flags := &waitFlags{}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long: short + ".\n\n" +
			"Visibility controls whether the environment's ingress is reachable\n" +
			"without the VPN. It is a property of the environment, not of the\n" +
			"caller, so it applies to everyone at once.\n\n" + cliexit.Help,
		Args: exactArgs(1, "the environment slug or id"),
		RunE: func(c *cobra.Command, args []string) error {
			action := "unshare"
			if public {
				action = "share"
			}
			return runMutation(c.Context(), app, action, args[0], waitPolicy{}, flags,
				func(ctx context.Context, sess *Session, e *envRef) error {
					resp, err := sess.API.EnvironmentsSetVisibilityWithResponse(ctx, e.ID,
						api.EnvironmentsSetVisibilityJSONRequestBody{IsPublic: public})
					if err != nil {
						return client.Transport(err, sess.Resolved.Endpoint)
					}
					if resp.JSON200 == nil {
						return client.Fail(resp, resp.Headers429)
					}
					return nil
				})
		},
	}
	flags.cmd = cmd
	return cmd
}

func newEnvAddServiceCommand(app *App) *cobra.Command {
	flags := &waitFlags{}
	cmd := &cobra.Command{
		Use:   "add-service <slug-or-id> <repo>:<branch>",
		Short: "Add a service to an existing environment",
		Long: "Add another repository's service to an existing environment.\n\n" +
			"REPO is a repository name, `owner/name`, display name or helm chart\n" +
			"key; it is resolved to an id client-side against the server's\n" +
			"repository list. BRANCH may contain slashes — only the FIRST colon\n" +
			"separates the two.\n\n" +
			"Blocks until the environment is running with the new service.\n\n" +
			cliexit.Help,
		Args: exactArgs(2, "the environment, then <repo>:<branch>"),
		RunE: func(c *cobra.Command, args []string) error {
			repoName, branch, err := splitRepoBranch(args[1])
			if err != nil {
				return err
			}
			return runMutation(c.Context(), app, "add-service", args[0], policyAddService, flags,
				func(ctx context.Context, sess *Session, e *envRef) error {
					repoID, err := resolveRepository(ctx, sess, repoName)
					if err != nil {
						return err
					}
					resp, err := sess.API.EnvironmentsAddServiceWithResponse(ctx, e.ID,
						api.EnvironmentsAddServiceJSONRequestBody{RepositoryId: repoID, Branch: branch})
					if err != nil {
						return client.Transport(err, sess.Resolved.Endpoint)
					}
					if resp.JSON200 == nil {
						return client.Fail(resp, resp.Headers429)
					}
					return nil
				})
		},
	}
	flags.register(cmd, policyAddService)
	return cmd
}

func newEnvRemoveServiceCommand(app *App) *cobra.Command {
	var yes bool
	flags := &waitFlags{}
	cmd := &cobra.Command{
		Use:   "remove-service <slug-or-id> <repo>",
		Short: "Remove a service from an environment",
		Long: "Remove one repository's service from an environment.\n\n" +
			"Destructive: the service's workload is removed from the namespace.\n" +
			"Confirms on a terminal and takes --yes.\n\n" + cliexit.Help,
		Args: exactArgs(2, "the environment, then the repository"),
		RunE: func(c *cobra.Command, args []string) error {
			return runMutation(c.Context(), app, "remove-service", args[0], waitPolicy{}, flags,
				func(ctx context.Context, sess *Session, e *envRef) error {
					svc, err := serviceFor(ctx, sess, e, args[1])
					if err != nil {
						return err
					}
					if err := app.Confirm(yes,
						fmt.Sprintf("This removes %s (branch %s) from %s.", args[1], svc.Branch, e.Slug),
						"remove this service"); err != nil {
						return err
					}
					resp, err := sess.API.EnvironmentsRemoveServiceWithResponse(ctx, e.ID, svc.Id)
					if err != nil {
						return client.Transport(err, sess.Resolved.Endpoint)
					}
					if resp.JSON200 == nil {
						return client.Fail(resp, resp.Headers429)
					}
					return nil
				})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	flags.cmd = cmd
	return cmd
}

func newEnvSwapBranchCommand(app *App) *cobra.Command {
	flags := &waitFlags{}
	cmd := &cobra.Command{
		Use:   "swap-branch <slug-or-id> <repo>:<new-branch>",
		Short: "Point a service at a different branch",
		Long: "Rebuild one of an environment's services from a different branch.\n\n" +
			"Only the FIRST colon separates the repository from the branch, so a\n" +
			"branch containing slashes or further colons is passed through intact.\n\n" +
			"Blocks until the environment is running on the new branch.\n\n" +
			cliexit.Help,
		Args: exactArgs(2, "the environment, then <repo>:<new-branch>"),
		RunE: func(c *cobra.Command, args []string) error {
			repoName, branch, err := splitRepoBranch(args[1])
			if err != nil {
				return err
			}
			return runMutation(c.Context(), app, "swap-branch", args[0], policySwapBranch, flags,
				func(ctx context.Context, sess *Session, e *envRef) error {
					svc, err := serviceFor(ctx, sess, e, repoName)
					if err != nil {
						return err
					}
					resp, err := sess.API.EnvironmentsSwapBranchWithResponse(ctx, e.ID, svc.Id,
						api.EnvironmentsSwapBranchJSONRequestBody{NewBranch: branch})
					if err != nil {
						return client.Transport(err, sess.Resolved.Endpoint)
					}
					if resp.JSON200 == nil {
						return client.Fail(resp, resp.Headers429)
					}
					return nil
				})
		},
	}
	flags.register(cmd, policySwapBranch)
	return cmd
}

func newEnvRetryBuildCommand(app *App) *cobra.Command {
	flags := &waitFlags{}
	cmd := &cobra.Command{
		Use:   "retry-build <slug-or-id> [repo]",
		Short: "Re-run a failed build",
		Long: "Re-dispatch the build for one of an environment's services.\n\n" +
			"REPO may be omitted when the environment has exactly one service.\n\n" +
			"Blocks until the environment is running.\n\n" + cliexit.Help,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			repoName := ""
			if len(args) > 1 {
				repoName = args[1]
			}
			return runMutation(c.Context(), app, "retry-build", args[0], policyRetryBuild, flags,
				func(ctx context.Context, sess *Session, e *envRef) error {
					svc, err := serviceFor(ctx, sess, e, repoName)
					if err != nil {
						return err
					}
					resp, err := sess.API.EnvironmentsRetryBuildWithResponse(ctx, e.ID, svc.Id)
					if err != nil {
						return client.Transport(err, sess.Resolved.Endpoint)
					}
					if resp.JSON200 == nil {
						return client.Fail(resp, resp.Headers429)
					}
					return nil
				})
		},
	}
	flags.register(cmd, policyRetryBuild)
	return cmd
}

// --- shared resolution ------------------------------------------------------

// splitRepoBranch parses `<repo>:<branch>` on the FIRST colon.
//
// First rather than last, because branch names legitimately contain colons and
// slashes (`release/2026:rc`) while repository identifiers do not. Splitting on
// the last colon would silently truncate such a branch.
func splitRepoBranch(s string) (repo, branch string, err error) {
	i := strings.Index(s, ":")
	if i <= 0 || i == len(s)-1 {
		return "", "", usageErrorf("expected <repo>:<branch>, got %q", s)
	}
	return s[:i], s[i+1:], nil
}

// serviceFor finds the environment service belonging to a named repository.
//
// Resolved through the environment's OWN service list rather than by guessing
// an id: the mutation endpoints take an `environmentRepoId`, which is the join
// row's id and not the repository's, and there is no operation that maps one to
// the other.
func serviceFor(ctx context.Context, sess *Session, e *envRef, repoName string) (*api.EnvironmentService, error) {
	services := e.Detail.Services
	if len(services) == 0 {
		return nil, &cliexit.ExitError{
			Code:    cliexit.Conflict,
			Message: fmt.Sprintf("%s has no services", e.Slug),
		}
	}
	if repoName == "" {
		if len(services) == 1 {
			return &services[0], nil
		}
		return nil, usageErrorf(
			"%s has %d services, so the repository must be named", e.Slug, len(services))
	}

	repoID, err := resolveRepository(ctx, sess, repoName)
	if err != nil {
		return nil, err
	}
	for i := range services {
		if services[i].RepositoryId == repoID {
			return &services[i], nil
		}
	}
	return nil, &cliexit.ExitError{
		Code:    cliexit.NotFound,
		Message: fmt.Sprintf("%s has no service from %s", e.Slug, repoName),
		Hint:    fmt.Sprintf("`drift env get %s` lists its services", e.Slug),
	}
}

// resolveRepository maps a repository NAME onto the id the API takes.
//
// Client-side by design (DESIGN.md §5): the API is addressed by UUID and users
// think in repository names. Four spellings are accepted because four are in
// circulation — `owner/name` from a git remote, the bare name from a directory,
// the display name from the web UI, and the helm chart key from gitops.
func resolveRepository(ctx context.Context, sess *Session, name string) (uuid.UUID, error) {
	repos, err := allRepositories(ctx, sess)
	if err != nil {
		return uuid.UUID{}, err
	}
	want := strings.ToLower(strings.TrimSpace(name))
	var matches []api.Repository
	for _, r := range repos {
		for _, candidate := range []string{r.FullName, r.Name, r.DisplayName, r.HelmChartKey} {
			if strings.ToLower(candidate) == want {
				matches = append(matches, r)
				break
			}
		}
	}

	switch len(matches) {
	case 1:
		return matches[0].Id, nil
	case 0:
		return uuid.UUID{}, &cliexit.ExitError{
			Code:    cliexit.NotFound,
			Message: fmt.Sprintf("no repository called %q", name),
			Detail:  "known services: " + strings.Join(serviceNames(repos), ", "),
			Hint:    "a service is named by its helm chart key, repository name or `owner/name`",
		}
	default:
		// Ambiguity is refused rather than resolved by an arbitrary rule: an
		// environment created against the wrong repository is expensive to
		// notice and expensive to undo.
		//
		// The alternatives are listed by HELM CHART KEY, which is the only one of
		// the four accepted spellings guaranteed to be unique — a monorepo has
		// several services sharing one `owner/name`, so listing the ambiguous
		// name back four times would be no help at all.
		return uuid.UUID{}, usageErrorf("%q names %d services: %s",
			name, len(matches), strings.Join(serviceNames(matches), ", "))
	}
}

// serviceNames lists repositories by the identifier that distinguishes them.
func serviceNames(repos []api.Repository) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		if seen[r.HelmChartKey] {
			continue
		}
		seen[r.HelmChartKey] = true
		out = append(out, r.HelmChartKey)
	}
	sort.Strings(out)
	return out
}

// allRepositories walks the paginated repository list.
//
// Paged rather than assuming one response holds everything: the contract caps
// `limit` at 50 and a deployment with more repositories than that would
// otherwise resolve names against a silently truncated list — which fails as
// "no such repository" for exactly the repositories that sort last.
// The loop is BOUNDED as well as paged. `hasMore` is server-supplied and the
// only other exit is an empty page, so a server that answers `hasMore: true`
// forever — a bug, a proxy replaying a cached page — would spin here
// indefinitely, burning the caller's rate-limit budget on a command that never
// returns. Fifty pages is 2,500 repositories, orders of magnitude past any real
// deployment, and hitting it is reported as the server fault it is.
const maxRepositoryPages = 50

func allRepositories(ctx context.Context, sess *Session) ([]api.Repository, error) {
	const page = 50
	var out []api.Repository
	offset := 0
	for i := 0; i < maxRepositoryPages; i++ {
		limit := page
		off := offset
		resp, err := sess.API.RepositoriesListWithResponse(ctx,
			&api.RepositoriesListParams{Limit: &limit, Offset: &off})
		if err != nil {
			return nil, client.Transport(err, sess.Resolved.Endpoint)
		}
		if resp.JSON200 == nil {
			return nil, client.Fail(resp, resp.Headers429)
		}
		out = append(out, resp.JSON200.Items...)
		if !resp.JSON200.Pagination.HasMore || len(resp.JSON200.Items) == 0 {
			return out, nil
		}
		offset += len(resp.JSON200.Items)
	}
	return nil, &cliexit.ExitError{
		Code:    cliexit.Error,
		Message: "the repository list did not end",
		Detail: fmt.Sprintf("read %d pages and the server still reported more; it is not paginating correctly",
			maxRepositoryPages),
		Hint: "name the repository by its exact helm chart key to skip the lookup",
	}
}
