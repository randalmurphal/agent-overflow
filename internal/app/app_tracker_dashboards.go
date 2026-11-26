package app

import (
	"context"
	"errors"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/trackerdash"
)

// A subscribed board's dashboards: the built-in, the ones its project
// keeps, and the widgets agents proposed on it (trackerapp/dashboards.go).
// trackerdash evaluates them in a copy of the board it makes for each
// call, so nothing outlives a call; the wire shapes are trackerdash's own.
// tracker:dashboards_updated tells every client of this computer that a
// board's dashboards changed.

// TrackerDashboards lists the dashboards a subscribed board offers, the
// built-in first, each with its ranges and its widgets' layout and How
// this is counted.
//
//ao:scope tracker:read
//ao:route home
//ao:callerbound
func (a *App) TrackerDashboards(ctx context.Context, subscriptionID string) ([]trackerdash.DashboardView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, err
	}
	out, err := svc.Dashboards(ctx, subscriptionID)
	return out, dashboardError(err)
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
//ao:callerbound
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
//ao:callerbound
func (a *App) DismissTrackerWidget(ctx context.Context, subscriptionID, proposal string) error {
	svc, err := a.trackerService()
	if err != nil {
		return err
	}
	return dashboardError(svc.DismissProposal(subscriptionID, proposal))
}

// RemoveTrackerWidget removes a widget from a dashboard a subscribed
// board's project keeps.
//
//ao:scope tracker:operate
//ao:route home
//ao:callerbound
func (a *App) RemoveTrackerWidget(ctx context.Context, subscriptionID, dashboard, widget string) error {
	svc, err := a.trackerService()
	if err != nil {
		return err
	}
	return dashboardError(svc.RemoveWidget(subscriptionID, dashboard, widget))
}

// DeleteTrackerDashboard deletes a dashboard a subscribed board's project
// keeps, with its widgets.
//
//ao:scope tracker:operate
//ao:route home
//ao:callerbound
func (a *App) DeleteTrackerDashboard(ctx context.Context, subscriptionID, dashboard string) error {
	svc, err := a.trackerService()
	if err != nil {
		return err
	}
	return dashboardError(svc.DeleteDashboard(subscriptionID, dashboard))
}

// dashboardError is err with the evaluator's refusal as its public code
// and sentence, and any other tracker refusal as trackerError has it.
func dashboardError(err error) error {
	if r, ok := errors.AsType[*trackerdash.Refusal](err); ok {
		return errorsx.Public(r.Code, r.Sentence, err)
	}
	return trackerError(err)
}
