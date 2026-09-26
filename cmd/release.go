package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/steadfast-ly/drift-cli/internal/api"
	"github.com/steadfast-ly/drift-cli/internal/client"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
	"github.com/steadfast-ly/drift-cli/internal/output"
	"github.com/steadfast-ly/drift-cli/internal/wait"
)

func newReleaseCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "release",
		Aliases: []string{"releases"},
		Short:   "Inspect release state and promote",
		Long: "Inspect release state and promote services.\n\n" +
			"`status` shows what is deployed where; `history` lists past\n" +
			"promotions; `promote rc` retags stg images as rc, `promote hotfix`\n" +
			"builds a branch straight to rc, bypassing stg, for emergencies,\n" +
			"`promote prd` promotes from rc to production (requires an\n" +
			"elevated credential), and `cancel` fails a promotion that is\n" +
			"stuck so it stops holding the concurrency guard.\n\n" + cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(
		newReleaseStatusCommand(app),
		newReleaseHistoryCommand(app),
		newReleasePromoteCommand(app),
		newReleaseCancelCommand(app),
	)
	return cmd
}

// --- status -----------------------------------------------------------------

func releaseColumns() []output.Column {
	return []output.Column{
		{Name: "service", Header: "Service"},
		{Name: "stg_tag", Header: "stg"},
		output.StatusColumn("stg_health", "stg health"),
		{Name: "rc_tag", Header: "rc"},
		output.StatusColumn("rc_health", "rc health"),
		{Name: "stg_commit", Header: "stg commit", Wide: true},
		{Name: "rc_commit", Header: "rc commit", Wide: true},
		{Name: "rc_replicas", Header: "rc ready", Wide: true},
	}
}

func newReleaseStatusCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show what is deployed to stg and rc",
		Long: "Show the promoted image tags and commits read from gitops, joined\n" +
			"with live pod health from Kubernetes.\n\n" +
			"A failure on either side degrades to an error on the affected\n" +
			"namespace rather than failing the request, so a partial answer is\n" +
			"normal and is reported as such on stderr.\n\n" +
			"An in-flight promotion, if there is one, is shown alongside.\n\n" +
			cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return runReleaseStatus(c.Context(), app) },
	}
}

func runReleaseStatus(ctx context.Context, app *App) error {
	cols := releaseColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	sess, err := app.Connect(ctx, FeatureReleasesRead)
	if err != nil {
		return err
	}

	resp, err := sess.API.ReleasesStateWithResponse(ctx)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return client.Fail(resp, resp.Headers429)
	}
	state := *resp.JSON200

	// The active promotion is a separate operation; a failure to read it must
	// not lose the release state the operator asked for, so it degrades to a
	// note rather than to an error.
	var active *api.Promotion
	if a, aerr := sess.API.ReleasesPromotionsActiveWithResponse(ctx,
		&api.ReleasesPromotionsActiveParams{}); aerr == nil && a.JSON200 != nil {
		active = a.JSON200.Active
	}

	rows := joinNamespaces(state)
	doc := &output.Doc{
		Columns:      cols,
		Rows:         rows,
		EmptyMessage: "No services are deployed.",
		Extra: map[string]any{
			"active": promotionPayload(active),
			"fetchedAt": map[string]any{
				"stg": state.Stg.FetchedAt, "rc": state.Rc.FetchedAt,
			},
		},
	}
	if err := app.Out.Write(doc); err != nil {
		return err
	}

	for ns, e := range map[string]*string{"stg": state.Stg.Error, "rc": state.Rc.Error} {
		if e != nil && *e != "" {
			app.Out.Warnf("warning: the %s namespace reported: %s", ns, *e)
		}
	}
	if active != nil && app.Out.EffectiveFormat() == output.FormatTable {
		app.Out.Infof("\nA %s promotion is in flight: %s (%s).",
			active.PromotionType, active.Id, active.Status)
	}
	return nil
}

// joinNamespaces produces one row per service, merging the two namespaces and
// their health.
//
// Joined on `helmChartKey`, which is the only identifier both sides share:
// gitops names a chart, Kubernetes names a deployment, and the server already
// keys its health lookup the same way. A service present in only one namespace
// still gets a row, with the other side blank, because "not promoted yet" is
// exactly what an operator running this command is looking for.
func joinNamespaces(state api.ReleaseState) []output.Row {
	type merged struct {
		stg, rc   *api.ServiceVersion
		stgH, rcH *api.ServiceHealth
	}
	byKey := map[string]*merged{}
	order := []string{}
	touch := func(key string) *merged {
		m, ok := byKey[key]
		if !ok {
			m = &merged{}
			byKey[key] = m
			order = append(order, key)
		}
		return m
	}
	for i := range state.Stg.Services {
		touch(state.Stg.Services[i].HelmChartKey).stg = &state.Stg.Services[i]
	}
	for i := range state.Rc.Services {
		touch(state.Rc.Services[i].HelmChartKey).rc = &state.Rc.Services[i]
	}
	for i := range state.Stg.Health {
		touch(state.Stg.Health[i].HelmChartKey).stgH = &state.Stg.Health[i]
	}
	for i := range state.Rc.Health {
		touch(state.Rc.Health[i].HelmChartKey).rcH = &state.Rc.Health[i]
	}

	rows := make([]output.Row, 0, len(order))
	for _, key := range order {
		m := byKey[key]
		row := output.Row{"service": key}
		if m.stg != nil {
			row["stg_tag"] = m.stg.ImageTag
			row["stg_commit"] = shortSHA(m.stg.CommitSha)
		}
		if m.rc != nil {
			row["rc_tag"] = m.rc.ImageTag
			row["rc_commit"] = shortSHA(m.rc.CommitSha)
		}
		if m.stgH != nil {
			row["stg_health"] = string(m.stgH.Status)
		}
		if m.rcH != nil {
			row["rc_health"] = string(m.rcH.Status)
			row["rc_replicas"] = fmt.Sprintf("%d/%d", m.rcH.ReadyReplicas, m.rcH.TotalReplicas)
		}
		rows = append(rows, row)
	}
	return rows
}

// --- history ----------------------------------------------------------------

func promotionColumns() []output.Column {
	return []output.Column{
		{Name: "created", Header: "Created"},
		{Name: "type", Header: "Type"},
		output.StatusColumn("status", "Status"),
		{Name: "services", Header: "Services"},
		{Name: "by", Header: "By"},
		{Name: "branch", Header: "Hotfix branch", Wide: true},
		{Name: "completed", Header: "Completed", Wide: true},
		{Name: "message", Header: "Message", Wide: true},
		{Name: "id", Header: "Id", Wide: true},
	}
}

func promotionRow(p api.Promotion) output.Row {
	return output.Row{
		"created":   p.CreatedAt,
		"type":      string(p.PromotionType),
		"status":    string(p.Status),
		"services":  p.Services,
		"by":        p.CreatedBy,
		"branch":    p.HotfixBranch,
		"completed": p.CompletedAt,
		"message":   p.StatusMessage,
		"id":        p.Id.String(),
	}
}

// promotionPayload renders a promotion for the machine formats, or nil.
func promotionPayload(p *api.Promotion) any {
	if p == nil {
		return nil
	}
	return mapRow(promotionRow(*p))
}

func newReleaseHistoryCommand(app *App) *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "history",
		Short: "List past promotions",
		Long: "List past promotions, newest first.\n\n" +
			"Paginated server-side: --limit is capped at 50 and --offset walks the\n" +
			"pages.\n\n" + cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runReleaseHistory(c.Context(), app, limit, offset)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum number of promotions to return (server default 20, max 50)")
	cmd.Flags().IntVar(&offset, "offset", 0, "number of promotions to skip")
	return cmd
}

func runReleaseHistory(ctx context.Context, app *App, limit, offset int) error {
	cols := promotionColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	if err := validatePage(limit, offset); err != nil {
		return err
	}
	sess, err := app.Connect(ctx, FeatureReleasesRead)
	if err != nil {
		return err
	}

	params := &api.ReleasesPromotionsHistoryParams{}
	if limit > 0 {
		params.Limit = &limit
	}
	if offset > 0 {
		params.Offset = &offset
	}
	resp, err := sess.API.ReleasesPromotionsHistoryWithResponse(ctx, params)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return client.Fail(resp, resp.Headers429)
	}

	page := *resp.JSON200
	rows := make([]output.Row, 0, len(page.Items))
	for _, p := range page.Items {
		rows = append(rows, promotionRow(p))
	}
	if err := app.Out.Write(&output.Doc{
		Columns: cols, Rows: rows,
		Extra: map[string]any{"pagination": map[string]any{
			"limit": page.Pagination.Limit, "offset": page.Pagination.Offset,
			"hasMore": page.Pagination.HasMore,
		}},
		EmptyMessage: "No promotions yet.",
	}); err != nil {
		return err
	}
	if page.Pagination.HasMore {
		app.Out.Infof("More results available: re-run with --offset %d.",
			page.Pagination.Offset+len(page.Items))
	}
	return nil
}

// --- promote ----------------------------------------------------------------

func newReleasePromoteCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "promote",
		Short: "Promote services",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(
		newPromoteRcCommand(app),
		newPromoteHotfixCommand(app),
		newPromotePrdCommand(app),
	)
	return cmd
}

// promotionWaitFlags mirrors waitFlags for a promotion, which converges on a
// different machine with different states.
type promotionWaitFlags struct {
	wait    bool
	noWait  bool
	timeout time.Duration
	cmd     *cobra.Command
}

func (w *promotionWaitFlags) register(cmd *cobra.Command) {
	w.cmd = cmd
	cmd.Flags().BoolVar(&w.wait, "wait", true, "wait for the promotion to finish deploying")
	cmd.Flags().BoolVar(&w.noWait, "no-wait", false, "return as soon as the workflows are dispatched")
	cmd.Flags().DurationVar(&w.timeout, "wait-timeout", wait.DefaultPromotionTimeout,
		"how long to wait before giving up (exit 6)")
}

func (w *promotionWaitFlags) validate() error {
	if w.cmd != nil && w.cmd.Flags().Changed("wait") && w.cmd.Flags().Changed("no-wait") {
		return usageErrorf("--wait and --no-wait contradict each other")
	}
	return nil
}

func (w *promotionWaitFlags) shouldWait() bool {
	if w.cmd != nil && w.cmd.Flags().Changed("no-wait") {
		return !w.noWait
	}
	return w.wait
}

func newPromoteRcCommand(app *App) *cobra.Command {
	var yes bool
	flags := &promotionWaitFlags{}
	cmd := &cobra.Command{
		Use:   "rc <service>...",
		Short: "Retag stg images as rc",
		Long: "Promote services from stg to rc.\n\n" +
			"Each named service's CURRENT stg image is retagged as rc and the\n" +
			"retag workflow dispatched, grouped by GitHub repository so a monorepo\n" +
			"is dispatched once. Services are named by helm chart key — the same\n" +
			"names `drift release status` prints.\n\n" +
			"Refused with a state conflict while an rc promotion is already in\n" +
			"flight, and with a not-found if a service is unregistered or absent\n" +
			"from stg.\n\n" +
			"Destructive: confirms on a terminal, takes --yes, and refuses without\n" +
			"--yes when the session is not interactive.\n\n" +
			"Blocks until the promotion finishes deploying.\n\n" + cliexit.Help,
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			services := normalizeServices(args)
			return runPromotion(c.Context(), app, promotion{
				Feature:  FeaturePromotionsRc,
				Kind:     "rc",
				Services: services,
				Summary: fmt.Sprintf("This retags the current stg image of %s as rc and deploys it.",
					strings.Join(services, ", ")),
				Question: "promote to rc",
				Yes:      yes,
				Wait:     flags,
				Call: func(ctx context.Context, sess *Session) (*api.PromotionMutation, *cliexit.ExitError) {
					resp, err := sess.API.ReleasesPromoteRcWithResponse(ctx,
						api.ReleasesPromoteRcJSONRequestBody{HelmChartKeys: services})
					if err != nil {
						return nil, client.Transport(err, sess.Resolved.Endpoint)
					}
					if resp.JSON200 == nil {
						return nil, client.Fail(resp, resp.Headers429)
					}
					return resp.JSON200, nil
				},
			})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	flags.register(cmd)
	return cmd
}

func newPromoteHotfixCommand(app *App) *cobra.Command {
	var yes bool
	var branch string
	flags := &promotionWaitFlags{}
	cmd := &cobra.Command{
		Use:   "hotfix <service>... --branch <branch>",
		Short: "Build a branch straight to rc, bypassing stg",
		Long: "Promote a branch directly to rc without passing through stg.\n\n" +
			"The named branch's HEAD is resolved in each service's repository and\n" +
			"that repository's hotfix workflow dispatched against it, tagging the\n" +
			"result `rc-<sha>`.\n\n" +
			"FOR EMERGENCIES. The build does not pass through stg, so nothing has\n" +
			"validated it before it reaches rc.\n\n" +
			"Destructive: confirms on a terminal, takes --yes, and refuses without\n" +
			"--yes when the session is not interactive.\n\n" + cliexit.Help,
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if strings.TrimSpace(branch) == "" {
				return usageErrorf("--branch is required: a hotfix names the branch to build")
			}
			services := normalizeServices(args)
			return runPromotion(c.Context(), app, promotion{
				Feature:  FeaturePromotionsHotfix,
				Kind:     "hotfix",
				Services: services,
				Summary: fmt.Sprintf(
					"This builds %s from branch %q straight to rc, BYPASSING stg — nothing will have validated it.",
					strings.Join(services, ", "), branch),
				Question: "dispatch this hotfix",
				Yes:      yes,
				Wait:     flags,
				Call: func(ctx context.Context, sess *Session) (*api.PromotionMutation, *cliexit.ExitError) {
					resp, err := sess.API.ReleasesPromoteRcHotfixWithResponse(ctx,
						api.ReleasesPromoteRcHotfixJSONRequestBody{HelmChartKeys: services, Branch: branch})
					if err != nil {
						return nil, client.Transport(err, sess.Resolved.Endpoint)
					}
					if resp.JSON200 == nil {
						return nil, client.Fail(resp, resp.Headers429)
					}
					return resp.JSON200, nil
				},
			})
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "branch to build (required)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	flags.register(cmd)
	return cmd
}

func newPromotePrdCommand(app *App) *cobra.Command {
	var yes bool
	flags := &promotionWaitFlags{}
	cmd := &cobra.Command{
		Use:   "prd <service>...",
		Short: "Promote services from rc to production",
		Long: "Promote services from rc to production.\n\n" +
			"Each named service's CURRENT rc image is retagged as prd and the\n" +
			"retag workflow dispatched, grouped by GitHub repository so a monorepo\n" +
			"is dispatched once. Services are named by helm chart key — the same\n" +
			"names `drift release status` prints.\n\n" +
			"REQUIRES ELEVATION. Production promotion needs a short-lived\n" +
			"credential scoped to `promote:prd`, minted through an interactive\n" +
			"browser sign-in at /credentials with a fifteen-minute lifetime.\n" +
			"A leaked long-lived token cannot reach production because it lacks\n" +
			"the scope, and CI structurally cannot promote because minting\n" +
			"requires interactive SSO.\n\n" +
			"Destructive: confirms on a terminal, takes --yes, and refuses without\n" +
			"--yes when the session is not interactive.\n\n" +
			"Blocks until the promotion finishes deploying.\n\n" + cliexit.Help,
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			services := normalizeServices(args)
			return runPromotion(c.Context(), app, promotion{
				Feature:  FeaturePromotionsPrd,
				Kind:     "prd",
				Services: services,
				Summary: fmt.Sprintf("This promotes the current rc image of %s to production.",
					strings.Join(services, ", ")),
				Question: "promote to production",
				Yes:      yes,
				Wait:     flags,
				Call: func(ctx context.Context, sess *Session) (*api.PromotionMutation, *cliexit.ExitError) {
					resp, err := sess.API.ReleasesPromotePrdWithResponse(ctx,
						api.ReleasesPromotePrdJSONRequestBody{HelmChartKeys: services})
					if err != nil {
						return nil, client.Transport(err, sess.Resolved.Endpoint)
					}
					if resp.JSON200 == nil {
						return nil, client.Fail(resp, resp.Headers429)
					}
					return resp.JSON200, nil
				},
			})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	flags.register(cmd)

	cmd.AddCommand(newPromotePrdHotfixCommand(app))
	return cmd
}

func newPromotePrdHotfixCommand(app *App) *cobra.Command {
	var yes bool
	var branch string
	flags := &promotionWaitFlags{}
	cmd := &cobra.Command{
		Use:   "hotfix <service>... --branch <branch>",
		Short: "Build a branch straight to prd, bypassing rc",
		Long: "Promote a branch directly to production without passing through rc.\n\n" +
			"The named branch's HEAD is resolved in each service's repository and\n" +
			"that repository's hotfix workflow dispatched against it, tagging the\n" +
			"result `prd-<sha>`.\n\n" +
			"FOR EMERGENCIES. The build does not pass through rc, so nothing has\n" +
			"validated it before it reaches production.\n\n" +
			"REQUIRES ELEVATION. See `drift release promote prd --help`.\n\n" +
			"Destructive: confirms on a terminal, takes --yes, and refuses without\n" +
			"--yes when the session is not interactive.\n\n" + cliexit.Help,
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if strings.TrimSpace(branch) == "" {
				return usageErrorf("--branch is required: a hotfix names the branch to build")
			}
			services := normalizeServices(args)
			return runPromotion(c.Context(), app, promotion{
				Feature:  FeaturePromotionsPrd,
				Kind:     "prd hotfix",
				Services: services,
				Summary: fmt.Sprintf(
					"This builds %s from branch %q straight to production, BYPASSING rc — nothing will have validated it.",
					strings.Join(services, ", "), branch),
				Question: "dispatch this prd hotfix",
				Yes:      yes,
				Wait:     flags,
				Call: func(ctx context.Context, sess *Session) (*api.PromotionMutation, *cliexit.ExitError) {
					resp, err := sess.API.ReleasesPromotePrdHotfixWithResponse(ctx,
						api.ReleasesPromotePrdHotfixJSONRequestBody{HelmChartKeys: services, Branch: branch})
					if err != nil {
						return nil, client.Transport(err, sess.Resolved.Endpoint)
					}
					if resp.JSON200 == nil {
						return nil, client.Fail(resp, resp.Headers429)
					}
					return resp.JSON200, nil
				},
			})
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "branch to build (required)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	flags.register(cmd)
	return cmd
}

// normalizeServices trims and de-duplicates the service list, preserving the
// order given. The server caps the list at 50 and rejects empty strings; a
// duplicate would dispatch the same workflow twice.
func normalizeServices(args []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(args))
	for _, a := range args {
		s := strings.TrimSpace(a)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// promotion is one promotion command's worth of policy, so the shared body
// below is written once.
type promotion struct {
	Feature  string
	Kind     string
	Services []string
	Summary  string
	Question string
	Yes      bool
	Wait     *promotionWaitFlags
	Call     func(context.Context, *Session) (*api.PromotionMutation, *cliexit.ExitError)
}

func promotionResultColumns() []output.Column {
	return []output.Column{
		{Name: "action", Header: "Action"},
		output.StatusColumn("status", "Status"),
		{Name: "services", Header: "Services"},
		{Name: "dispatches", Header: "Dispatches"},
		{Name: "waited", Header: "Waited"},
		{Name: "id", Header: "Id", Wide: true},
	}
}

func runPromotion(ctx context.Context, app *App, p promotion) error {
	cols := promotionResultColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	if len(p.Services) == 0 {
		return usageErrorf("name at least one service to promote")
	}
	if err := p.Wait.validate(); err != nil {
		return err
	}

	sess, err := app.Connect(ctx, p.Feature)
	if err != nil {
		return err
	}
	if err := app.Confirm(p.Yes, p.Summary, p.Question); err != nil {
		return err
	}

	result, callErr := p.Call(ctx, sess)
	if callErr != nil {
		return callErr
	}

	write := func(status api.PromotionStatus, waited bool) error {
		return app.Out.Write(&output.Doc{
			Columns: cols, Single: true,
			Rows: []output.Row{{
				"action":     "promote " + p.Kind,
				"status":     string(status),
				"services":   p.Services,
				"dispatches": result.DispatchCount,
				"waited":     waited,
				"id":         result.PromotionId.String(),
			}},
		})
	}

	if !p.Wait.shouldWait() {
		return write(api.PromotionStatusDispatched, false)
	}

	progress := wait.NewProgress(app.Stderr, output.IsTerminal(app.Stderr) && app.Out.ErrColor)
	final, waitErr := wait.WaitPromotion(ctx, wait.PromotionOptions{
		Timeout:  p.Wait.timeout,
		Interval: app.waitInterval,
		Ref:      result.PromotionId.String(),
		Reporter: progress.ForPromotion(),
	}, func(ctx context.Context) (wait.PromotionObservation, error) {
		return pollPromotion(ctx, sess, result.PromotionId.String())
	})
	if werr := write(final, true); werr != nil && waitErr == nil {
		return werr
	}
	return waitErr
}

// findPromotion reads one promotion from the in-flight/recent listing.
//
// `promotions/active` carries the in-flight promotion plus recent history, and
// a promotion moves from the first to the second when it finishes — so both are
// searched. Reading only `active` would see a promotion vanish at the moment it
// completed, which for a wait is a timeout on a promotion that succeeded, and
// for a cancel is a summary with no services and no status.
//
// A nil promotion with a nil error means the id is in neither list. That is not
// a failure here: the two callers need different answers from it — the wait
// treats it as the promotion having left the window, and the cancel's summary
// degrades to naming the id alone, because the server is the authority on
// whether the id exists at all.
func findPromotion(ctx context.Context, sess *Session, id string) (*api.Promotion, error) {
	resp, err := sess.API.ReleasesPromotionsActiveWithResponse(ctx, &api.ReleasesPromotionsActiveParams{})
	if err != nil {
		return nil, client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return nil, client.Fail(resp, resp.Headers429)
	}
	if a := resp.JSON200.Active; a != nil && a.Id.String() == id {
		return a, nil
	}
	for i := range resp.JSON200.Recent {
		if resp.JSON200.Recent[i].Id.String() == id {
			return &resp.JSON200.Recent[i], nil
		}
	}
	return nil, nil
}

// pollPromotion reads one promotion's current status.
func pollPromotion(ctx context.Context, sess *Session, id string) (wait.PromotionObservation, error) {
	p, err := findPromotion(ctx, sess, id)
	if err != nil {
		return wait.PromotionObservation{}, err
	}
	if p == nil {
		return wait.PromotionObservation{}, &cliexit.ExitError{
			Code:    cliexit.NotFound,
			Message: "the promotion is no longer listed as active or recent",
			Hint:    "`drift release history` will still have it",
		}
	}
	obs := wait.PromotionObservation{Status: p.Status}
	if p.StatusMessage != nil {
		obs.Message = *p.StatusMessage
	}
	return obs, nil
}

// --- cancel -----------------------------------------------------------------

// maxCancelReason is the contract's bound on `--reason`: the cancel operation's
// request body declares `minLength: 1, maxLength: 500`, enforced server-side by
// the schema validator, whose `.max` measures UTF-16 code units. Checked
// client-side so a value the server would reject with a 400 names the flag
// instead.
const maxCancelReason = 500

func promotionCancelColumns() []output.Column {
	return []output.Column{
		{Name: "action", Header: "Action"},
		{Name: "id", Header: "Id"},
		{Name: "previous", Header: "Previous"},
		output.StatusColumn("status", "Status"),
	}
}

func newReleaseCancelCommand(app *App) *cobra.Command {
	var reason string
	var yes bool
	cmd := &cobra.Command{
		Use:   "cancel <promotion-id>",
		Short: "Fail a promotion that is stuck",
		Long: "Cancel a promotion that is stuck.\n\n" +
			"A promotion whose retag workflow was cancelled, whose target was\n" +
			"rolled back by hand, or that never received its ArgoCD\n" +
			"notification stays in flight forever, and the concurrency guard\n" +
			"refuses the next promotion of the same services while it does.\n" +
			"`cancel` fails it: `dispatched` and `promoting` move to `failed`,\n" +
			"`deploying` moves to `deploy_failed`. There is no separate\n" +
			"`cancelled` state, and a cancelled promotion cannot be resumed —\n" +
			"promote the services again instead.\n\n" +
			"A promotion that has already reached a terminal state is refused\n" +
			"with a state conflict (exit 5), an unknown id with a not-found\n" +
			"(exit 3), and a credential below the release role with a\n" +
			"forbidden (exit 1).\n\n" +
			"Requires the promotion id: `drift release status` prints the\n" +
			"in-flight one, and `drift release history -o wide` lists past ids.\n\n" +
			"Destructive: confirms on a terminal, takes --yes, and refuses\n" +
			"without --yes when the session is not interactive.\n\n" + cliexit.Help,
		Args: exactArgs(1, "the promotion id"),
		RunE: func(c *cobra.Command, args []string) error {
			normalized, err := validateCancelReason(c.Flags().Changed("reason"), reason)
			if err != nil {
				return err
			}
			return runReleaseCancel(c.Context(), app, args[0], normalized, yes)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "",
		"why the promotion is being cancelled, recorded on the promotion and in the audit log "+
			"(1-500 characters; an emoji counts as two)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

// validateCancelReason checks a `--reason` the server would reject, and returns
// the value to send.
//
// `set` is whether the flag was actually given, because the two ends of the
// bound differ in what omission means. An OMITTED reason is legitimate — the
// body field is optional — but an explicitly empty or whitespace-only one is
// not: it is what `--reason "$REASON"` produces when a CI variable expanded to
// nothing, and silently recording no reason would lose the one thing the
// operator was asked for.
//
// The server's schema is `z.string().trim().min(1).max(500)`: it trims FIRST
// and bounds the result, so the bound applies to the trimmed value and the
// trimmed value is what gets recorded. Measuring the raw flag would refuse a
// reason that is only over the limit because of the padding around it, and
// sending the raw value would record padding the operator never meant.
//
// The trim is `trimECMAScript`, not `strings.TrimSpace`: the value sent has to
// be the value the server computes, down to which characters count as padding.
func validateCancelReason(set bool, reason string) (string, error) {
	if !set {
		return "", nil
	}
	trimmed := trimECMAScript(reason)
	if trimmed == "" {
		return "", usageErrorf("--reason must not be empty or whitespace")
	}
	if n := utf16Units(trimmed); n > maxCancelReason {
		return "", usageErrorf(
			"--reason must be at most %d characters, an emoji counting as two (got %d)",
			maxCancelReason, n)
	}
	return trimmed, nil
}

// utf16Units counts a string the way the server's schema validator does.
//
// The validator's `.max(500)` measures a JavaScript string, i.e. UTF-16 code
// units, in which a code point above the BMP — an emoji, a flag, some CJK
// extensions — is TWO. A rune count would pass a 300-emoji reason the server
// answers 400 to, which is exactly the round trip this check exists to save;
// a byte count would reject multi-byte scripts the server accepts.
func utf16Units(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

// trimECMAScript trims a string the way the server's `z.string().trim()` does,
// which is JavaScript's `String.prototype.trim`.
//
// `strings.TrimSpace` is NOT that function: it asks `unicode.IsSpace`, so it
// strips U+0085 (NEL), which ECMAScript keeps, and keeps U+FEFF (BOM), which
// ECMAScript strips. Both differences are observable in the body — a reason
// padded with NEL would be recorded a character shorter than the server means,
// and one padded with a BOM would be refused here as whitespace-only when the
// server would have accepted the remainder.
//
// The set is ECMAScript's WhiteSpace + LineTerminator productions: the
// LineTerminators U+000A, U+000D, U+2028 and U+2029, the WhiteSpace characters
// U+0009, U+000B, U+000C, U+0020, U+00A0 and U+FEFF, and the Unicode `Zs`
// category — which carries U+00A0 and U+3000 today and any space separator
// added later, which is why it is asked as a category rather than enumerated.
func trimECMAScript(s string) string {
	return strings.TrimFunc(s, isECMAScriptSpace)
}

func isECMAScriptSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00A0', '\u2028', '\u2029', '\uFEFF':
		return true
	}
	return unicode.Is(unicode.Zs, r)
}

// cancelSummaryIDOnly is the summary for a promotion whose details were not —
// or could not be — read. It says the one thing that is certainly true.
func cancelSummaryIDOnly(id string) string {
	return fmt.Sprintf("This cancels promotion %s. It cannot be resumed.", id)
}

// cancelSummary describes, in the operator's terms, the promotion about to be
// failed.
//
// The lookup behind it is BEST-EFFORT and never fails the command: the id is
// authoritative on the server, `promotions/active` is a separate operation, and
// refusing a cancel the server would accept because a summary could not be
// decorated would be the tail wagging the dog. A promotion the lookup does not
// find — or one it cannot read — is named by id alone.
func cancelSummary(ctx context.Context, sess *Session, id string) string {
	p, err := findPromotion(ctx, sess, id)
	if err != nil || p == nil {
		return cancelSummaryIDOnly(id)
	}

	services := strings.Join(p.Services, ", ")
	if services == "" {
		services = "-"
	}
	// The outcome is the failure state the lifecycle machine allows from where
	// the promotion stands, which is the one the response will report back.
	outcome := "It will be marked failed and cannot be resumed."
	switch p.Status {
	case api.PromotionStatusDeploying:
		outcome = "It will be marked deploy_failed and cannot be resumed."
	case api.PromotionStatusDispatched, api.PromotionStatusPromoting:
	default:
		// Terminal already. The server refuses this with a 409 naming the
		// state, and promising a transition here would contradict it.
		outcome = fmt.Sprintf("It is already %s, which the server will refuse to cancel.", p.Status)
	}
	return fmt.Sprintf("This cancels the %s promotion %s (services: %s; status: %s). %s",
		p.PromotionType, id, services, p.Status, outcome)
}

func runReleaseCancel(ctx context.Context, app *App, idArg, reason string, yes bool) error {
	cols := promotionCancelColumns()
	if err := output.ValidateFields(app.Out.JSONFields, cols); err != nil {
		return usageErrorf("%s", err.Error())
	}
	// Client-side and BEFORE any request: a malformed id is the operator's
	// typo, and making them decode a problem envelope to be told so is worse
	// than saying it here.
	id, err := uuid.Parse(idArg)
	if err != nil {
		return usageErrorf("invalid promotion id %q: not a UUID", idArg)
	}

	// The refusal for a run that cannot answer the question happens BEFORE
	// anything talks to the server, and that ordering is the point: this
	// invocation has already earned a usage error, and deciding it after Connect
	// reports a connection failure against an unreachable server, or a
	// capability refusal against an old one, for what is really a missing --yes.
	//
	// Confirm always refuses in this branch, so the return is unconditional —
	// and it is Confirm, rather than a hand-built ExitError, so the message, the
	// hint and the exit code cannot diverge from the prompt's.
	if !yes && !app.Interactive() {
		return app.Confirm(false, cancelSummaryIDOnly(id.String()), "cancel this promotion")
	}

	sess, err := app.Connect(ctx, FeaturePromotionsCancel)
	if err != nil {
		return err
	}
	// From here the summary WILL be shown — `--yes` prints it and returns, a
	// terminal prints it and asks — so the lookup that decorates it is worth the
	// request. It remains best-effort: a promotion the listing does not carry,
	// or a listing that cannot be read, degrades to the id alone.
	if err := app.Confirm(yes, cancelSummary(ctx, sess, id.String()), "cancel this promotion"); err != nil {
		return err
	}

	body := api.ReleasesPromotionsCancelJSONRequestBody{}
	if reason != "" {
		body.Reason = &reason
	}
	resp, err := sess.API.ReleasesPromotionsCancelWithResponse(ctx, id, body)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return client.Fail(resp, resp.Headers429)
	}
	m := *resp.JSON200

	return app.Out.Write(&output.Doc{
		Columns: cols, Single: true,
		Rows: []output.Row{{
			"action":   "cancel",
			"id":       m.PromotionId.String(),
			"previous": string(m.PreviousStatus),
			"status":   string(m.Status),
		}},
	})
}
