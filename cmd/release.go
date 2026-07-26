package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steadfast/drift-cli/internal/api"
	"github.com/steadfast/drift-cli/internal/client"
	"github.com/steadfast/drift-cli/internal/cliexit"
	"github.com/steadfast/drift-cli/internal/output"
	"github.com/steadfast/drift-cli/internal/wait"
)

func newReleaseCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "release",
		Aliases: []string{"releases"},
		Short:   "Inspect release state and promote to rc",
		Long: "Inspect release state and promote services.\n\n" +
			"`status` shows what is deployed where; `history` lists past\n" +
			"promotions; `promote rc` retags stg images as rc and `promote hotfix`\n" +
			"builds a branch straight to rc, bypassing stg, for emergencies.\n\n" +
			"Production promotion is NOT on this surface — see\n" +
			"`drift release promote prd` for why.\n\n" + cliexit.Help,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(
		newReleaseStatusCommand(app),
		newReleaseHistoryCommand(app),
		newReleasePromoteCommand(app),
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
		Short: "Promote services to rc",
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

// newPromotePrdCommand exists so that typing the obvious thing gets an
// explanation rather than "unknown command".
//
// It is NOT a stub of the operation: nothing is called, nothing is faked, and
// the exit code says the CLI cannot do this. Production promotion needs a
// short-TTL credential scoped to `promote:prd`, minted through an interactive
// browser round trip, and that mint does not exist on this server yet — so the
// only correct behaviour is to say where the operation lives and why it is not
// here. Omitting the command entirely would leave an operator mid-incident
// staring at a usage error that does not mention production at all.
func newPromotePrdCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "prd <service>...",
		Short: "Promote to production (web only for now)",
		Long: "Promote services from rc to production.\n\n" +
			"NOT AVAILABLE FROM THE CLI. Production promotion requires elevation —\n" +
			"a browser round trip minting a credential scoped to `promote:prd` with\n" +
			"a fifteen-minute lifetime — so that a leaked long-lived token cannot\n" +
			"reach production and CI structurally cannot promote. Neither the mint\n" +
			"nor the operation is on `/api/v1` yet.\n\n" +
			"Use the web UI. This command exists so that typing it explains itself\n" +
			"instead of failing as an unknown subcommand.\n\n" + cliexit.Help,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			e := &cliexit.ExitError{
				Code:    cliexit.Error,
				Message: "production promotion is not available from the CLI",
				Detail: "it requires a short-lived credential scoped to `promote:prd`, minted through an " +
					"interactive browser sign-in; neither that mint nor the operation is on /api/v1 yet",
			}
			if r, rerr := app.Resolve(); rerr == nil {
				e.Hint = "promote from the web UI at " + strings.TrimRight(r.Endpoint, "/") + "/releases"
			} else {
				e.Hint = "promote from the web UI"
			}
			return e
		},
	}
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

// pollPromotion reads one promotion's current status.
//
// `promotions/active` carries the in-flight promotion plus recent history, and
// the promotion being waited on moves from the first to the second when it
// finishes — so both are searched. Reading only `active` would see the
// promotion vanish at the moment it completed and report a timeout on a
// promotion that succeeded.
func pollPromotion(ctx context.Context, sess *Session, id string) (wait.PromotionObservation, error) {
	resp, err := sess.API.ReleasesPromotionsActiveWithResponse(ctx, &api.ReleasesPromotionsActiveParams{})
	if err != nil {
		return wait.PromotionObservation{}, client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		return wait.PromotionObservation{}, client.Fail(resp, resp.Headers429)
	}
	candidates := resp.JSON200.Recent
	if resp.JSON200.Active != nil {
		candidates = append([]api.Promotion{*resp.JSON200.Active}, candidates...)
	}
	for _, c := range candidates {
		if c.Id.String() != id {
			continue
		}
		obs := wait.PromotionObservation{Status: c.Status}
		if c.StatusMessage != nil {
			obs.Message = *c.StatusMessage
		}
		return obs, nil
	}
	return wait.PromotionObservation{}, &cliexit.ExitError{
		Code:    cliexit.NotFound,
		Message: "the promotion is no longer listed as active or recent",
		Hint:    "`drift release history` will still have it",
	}
}
