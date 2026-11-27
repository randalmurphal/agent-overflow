package app

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/trackerdash"
	"agent-overflow/internal/transport"
)

// A subscribed board's dashboards: the built-in, the ones its project
// keeps, and the widgets agents proposed on it (trackerapp/dashboards.go).
// trackerdash evaluates them in a copy of the board it makes for each
// call, so nothing outlives a call; the wire shapes are trackerdash's own.
// tracker:dashboards_updated tells every client of this computer that a
// board's dashboards changed.

// TrackerDashboardsView is what a subscribed board's Dashboards screen
// lays out: the dashboards it offers, the built-in first, each with its
// ranges and its widgets' layout and How this is counted, and the widgets
// agents proposed on the board with their threads (trackerdash.Listing),
// and the board's summary row: its pins, the number widgets' limits and
// the proposals Keep pins (trackerapp.BoardTilesView), which an older
// computer's answer lacks. Ask is why Ask your agent cannot start a thread
// on this computer, "" when it can (trackerToolsProvider).
type TrackerDashboardsView struct {
	Dashboards []trackerdash.DashboardView `json:"dashboards"`
	Proposals  []trackerdash.ProposalView  `json:"proposals"`
	Tiles      trackerapp.BoardTilesView   `json:"tiles"`
	Ask        string                      `json:"ask"`
}

// TrackerDashboards lists what a subscribed board's Dashboards screen lays
// out. It counts nothing, so it answers while every count is out.
//
//ao:scope tracker:read
//ao:route home
//ao:callerbound
func (a *App) TrackerDashboards(ctx context.Context, subscriptionID string) (TrackerDashboardsView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return TrackerDashboardsView{}, err
	}
	list, tiles, err := svc.Dashboards(ctx, subscriptionID)
	if err != nil {
		return TrackerDashboardsView{}, dashboardError(err)
	}
	out := TrackerDashboardsView{Dashboards: list.Dashboards, Proposals: list.Proposals, Tiles: tiles}
	if a.trackerToolsProvider(ctx) == "" {
		out.Ask = noTrackerToolsProvider
	}
	return out, nil
}

// noTrackerToolsProvider is why a thread that needs the tracker tools
// cannot start here.
const noTrackerToolsProvider = "Ask your agent needs a provider whose agents get the tracker tools, turned on in Settings and installed on this computer."

// trackerToolsProvider is the provider a thread that needs the tracker
// tools (ao-tracker-tools) starts on: the one a new thread starts on
// when its agents get them and this computer offers it (providerOffered),
// else the first in provider order that does; "" when none does.
func (a *App) trackerToolsProvider(ctx context.Context) string {
	has := func(name string) bool {
		return provider.CapabilitiesForProvider(name).SupportsTrackerMCP && a.providerOffered(name)
	}
	if last := a.seedChatModelProfile(ctx, "", "").Provider; has(last) {
		return last
	}
	for _, kind := range provider.AllProviders() {
		if has(string(kind)) {
			return string(kind)
		}
	}
	return ""
}

// StartTrackerDashboardThread makes the thread of a board's Ask your agent
// and answers its row: in the project the board subscription was made
// for, in its checkout, on the provider trackerToolsProvider picks, its
// draft, unsent, naming the board by its name and site/ID, which the
// tracker tools' dashboard_widget takes. A failure to seed the draft
// deletes the thread. Nothing else is written.
//
//ao:scope threads:operate
//ao:route home
func (a *App) StartTrackerDashboardThread(ctx context.Context, subscriptionID string) (store.Thread, error) {
	if a.shuttingDown.Load() {
		return store.Thread{}, ErrShuttingDown
	}
	if err := a.requireScope(ctx, transport.ScopeTrackerRead, "StartTrackerDashboardThread reads the board it names"); err != nil {
		return store.Thread{}, err
	}
	svc, err := a.trackerService()
	if err != nil {
		return store.Thread{}, err
	}
	board, err := svc.DashboardAsk(subscriptionID)
	if err != nil {
		return store.Thread{}, dashboardError(err)
	}
	providerName := a.trackerToolsProvider(ctx)
	if providerName == "" {
		return store.Thread{}, errorsx.Public("tracker_tools_no_provider", noTrackerToolsProvider, nil)
	}
	project, err := a.store.GetProject(board.Project)
	if err != nil {
		return store.Thread{}, fmt.Errorf("start dashboard thread: %w", err)
	}
	thread, cut, err := a.createThreadWithWorktree(ctx, CreateThreadOptions{ProjectID: project.ID, Provider: providerName}, false)
	if err != nil {
		return store.Thread{}, fmt.Errorf("start dashboard thread: %w", err)
	}
	if err := a.SaveDraft(ctx, thread.ID, dashboardAskSeed(board), nil, nil, nil); err != nil {
		return store.Thread{}, errors.Join(fmt.Errorf("start dashboard thread: seed the draft: %w", err), a.rollBackCreatedThread("start dashboard thread", project, thread, cut))
	}
	return a.store.GetThread(thread.ID)
}

// dashboardAskSeed is the draft Ask your agent leaves for the person to
// finish and send.
func dashboardAskSeed(b trackerapp.DashboardAskBoard) string {
	return fmt.Sprintf("About the dashboards of the board %s (%s): ", b.Name, b.Handle)
}

// EvaluateTrackerDashboard counts one dashboard of a subscribed board over
// a range, narrowed to a person or an epic, with the widgets proposed on
// the board.
//
//ao:scope tracker:read
//ao:route home
//ao:callerbound
func (a *App) EvaluateTrackerDashboard(ctx context.Context, subscriptionID string, request trackerdash.Request) (trackerdash.Answer, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerdash.Answer{}, err
	}
	out, err := svc.EvaluateDashboard(ctx, subscriptionID, request)
	return out, dashboardError(err)
}

// TrackerDashboardDrill answers one number of a dashboard with the rows
// behind it, a page at a time, from an evaluation of its own.
//
//ao:scope tracker:read
//ao:route home
//ao:callerbound
func (a *App) TrackerDashboardDrill(ctx context.Context, subscriptionID string, request trackerdash.DrillRequest) (trackerdash.DrillAnswer, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerdash.DrillAnswer{}, err
	}
	out, err := svc.DashboardDrill(ctx, subscriptionID, request)
	return out, dashboardError(err)
}

// KeepTrackerWidget keeps a widget an agent proposed on a subscribed
// board on one of the dashboards its project keeps, or on a new one, and
// answers that dashboard's ID.
//
//ao:scope tracker:operate
//ao:route home
func (a *App) KeepTrackerWidget(ctx context.Context, subscriptionID, proposal string, to trackerapp.KeepTarget) (string, error) {
	svc, err := a.trackerService()
	if err != nil {
		return "", err
	}
	out, err := svc.KeepProposal(subscriptionID, proposal, to)
	return out, dashboardError(err)
}

// DismissTrackerWidget drops a widget an agent proposed on a subscribed
// board.
//
//ao:scope tracker:operate
//ao:route home
func (a *App) DismissTrackerWidget(ctx context.Context, subscriptionID, proposal string) error {
	svc, err := a.trackerService()
	if err != nil {
		return err
	}
	return dashboardError(svc.DismissProposal(subscriptionID, proposal))
}

// RemoveTrackerWidget removes a widget from a dashboard a subscribed
// board's project keeps; the dashboard goes with its last widget.
//
//ao:scope tracker:operate
//ao:route home
func (a *App) RemoveTrackerWidget(ctx context.Context, subscriptionID, dashboard, widget string) error {
	svc, err := a.trackerService()
	if err != nil {
		return err
	}
	return dashboardError(svc.RemoveWidget(subscriptionID, dashboard, widget))
}

// SetTrackerWidgetLimit sets the limit of a number widget of a subscribed
// board's dashboards, or clears it for nil; over it, the widget and its
// tile on the board's summary row are marked.
//
//ao:scope tracker:operate
//ao:route home
func (a *App) SetTrackerWidgetLimit(ctx context.Context, subscriptionID, widget string, limit *float64) error {
	svc, err := a.trackerService()
	if err != nil {
		return err
	}
	return dashboardError(svc.SetWidgetLimit(ctx, subscriptionID, widget, limit))
}

// PinTrackerWidget pins a widget of a subscribed board's dashboards to the
// end of the board's summary row, at most five, or takes it off.
//
//ao:scope tracker:operate
//ao:route home
func (a *App) PinTrackerWidget(ctx context.Context, subscriptionID, widget string, pinned bool) error {
	svc, err := a.trackerService()
	if err != nil {
		return err
	}
	return dashboardError(svc.PinWidget(ctx, subscriptionID, widget, pinned))
}

// dashboardError is err with the evaluator's refusal as its public code
// and sentence, and any other tracker refusal as trackerError has it.
func dashboardError(err error) error {
	if r, ok := errors.AsType[*trackerdash.Refusal](err); ok {
		return errorsx.Public(r.Code, r.Sentence, err)
	}
	return trackerError(err)
}
