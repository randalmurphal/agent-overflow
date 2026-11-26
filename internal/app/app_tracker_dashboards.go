package app

import (
	"context"
	"errors"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/trackerdash"
)

// A subscribed board's dashboards. trackerdash evaluates them in a copy
// of the board it makes for each call, so nothing outlives a call; the
// wire shapes are trackerdash's own.

// TrackerDashboards lists the dashboards a subscribed board offers, each
// with its ranges and its widgets' layout and How this is counted.
//
//ao:scope tracker:read
//ao:route home
//ao:callerbound
func (a *App) TrackerDashboards(ctx context.Context, subscriptionID string) ([]trackerdash.DashboardView, error) {
	svc, scope, _, err := a.trackerSubscriptionBoard(subscriptionID)
	if err != nil {
		return nil, err
	}
	out, err := svc.Dashboards(ctx, scope)
	return out, dashboardError(err)
}

// EvaluateTrackerDashboard counts one dashboard of a subscribed board over
// a range, narrowed to a person or an epic.
//
//ao:scope tracker:read
//ao:route home
//ao:callerbound
func (a *App) EvaluateTrackerDashboard(ctx context.Context, subscriptionID string, request trackerdash.Request) (trackerdash.Answer, error) {
	svc, scope, groups, err := a.trackerSubscriptionBoard(subscriptionID)
	if err != nil {
		return trackerdash.Answer{}, err
	}
	out, err := svc.EvaluateDashboard(ctx, scope, groups, request)
	return out, dashboardError(err)
}

// TrackerDashboardDrill answers one number of a dashboard with the rows
// behind it, a page at a time, from an evaluation of its own.
//
//ao:scope tracker:read
//ao:route home
//ao:callerbound
func (a *App) TrackerDashboardDrill(ctx context.Context, subscriptionID string, request trackerdash.DrillRequest) (trackerdash.DrillAnswer, error) {
	svc, scope, groups, err := a.trackerSubscriptionBoard(subscriptionID)
	if err != nil {
		return trackerdash.DrillAnswer{}, err
	}
	out, err := svc.DashboardDrill(ctx, scope, groups, request)
	return out, dashboardError(err)
}

// dashboardError is err with the evaluator's refusal as its public code
// and sentence, and any other tracker refusal as trackerError has it.
func dashboardError(err error) error {
	if r, ok := errors.AsType[*trackerdash.Refusal](err); ok {
		return errorsx.Public(r.Code, r.Sentence, err)
	}
	return trackerError(err)
}
