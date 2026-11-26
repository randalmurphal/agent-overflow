package trackerapp

import (
	"context"
	"database/sql"
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerdash"
)

// Dashboards is the dashboards board scope offers: its kind (whether it
// plans sprints, whether its tracker has epics) picks the built-in one.
func (s *Service) Dashboards(ctx context.Context, scope Scope) ([]trackerdash.DashboardView, error) {
	if err := scope.boardOnly(); err != nil {
		return nil, err
	}
	root, err := s.readRoot(scope.Connection)
	if err != nil {
		return nil, err
	}
	defer s.leave()
	st, ok, err := s.store.GetMirrorBoard(root, string(scope.Board))
	switch {
	case err != nil:
		return nil, fmt.Errorf("trackerapp: read board %s from the mirror: %w", scope.Board, err)
	case !ok:
		return nil, fmt.Errorf("%w: board %s", ErrNotMirrored, scope.Board)
	}
	row, err := s.store.GetTrackerConnection(scope.Connection)
	if err != nil {
		return nil, fmt.Errorf("trackerapp: read connection %s: %w", scope.Connection, err)
	}
	return s.dash.Dashboards(ctx, trackerdash.Kind{PlansSprints: st.Board.PlansSprints, Epics: hasEpics(row.Kind)})
}

// EvaluateDashboard counts a dashboard of board scope, read with the
// status groups its project gives it.
func (s *Service) EvaluateDashboard(ctx context.Context, scope Scope, groups StatusGroups, r trackerdash.Request) (trackerdash.Answer, error) {
	b, err := s.dashboardBoard(scope, groups)
	if err != nil {
		return trackerdash.Answer{}, err
	}
	defer s.leave()
	return s.dash.Evaluate(ctx, b, r)
}

// DashboardDrill is the rows behind one number of a dashboard of board
// scope, from an evaluation of their own.
func (s *Service) DashboardDrill(ctx context.Context, scope Scope, groups StatusGroups, r trackerdash.DrillRequest) (trackerdash.DrillAnswer, error) {
	b, err := s.dashboardBoard(scope, groups)
	if err != nil {
		return trackerdash.DrillAnswer{}, err
	}
	defer s.leave()
	return s.dash.Drill(ctx, b, r)
}

// dashboardBoard is board scope as an evaluation reads it: the store's
// export of its rows with the project's status groups, whether its
// tracker has epics, and the connection's verified account for Mine. The
// caller leaves the service when the evaluation returns.
func (s *Service) dashboardBoard(scope Scope, groups StatusGroups) (trackerdash.Board, error) {
	if err := scope.boardOnly(); err != nil {
		return trackerdash.Board{}, err
	}
	root, err := s.readRoot(scope.Connection)
	if err != nil {
		return trackerdash.Board{}, err
	}
	row, err := s.store.GetTrackerConnection(scope.Connection)
	if err != nil {
		s.leave()
		return trackerdash.Board{}, fmt.Errorf("trackerapp: read connection %s: %w", scope.Connection, err)
	}
	fill := func(ctx context.Context, conn *sql.Conn) (store.MirrorExport, bool, error) {
		return s.store.ExportMirrorBoard(ctx, conn, root, string(scope.Board), groups.regroup)
	}
	return trackerdash.Board{Fill: fill, Epics: hasEpics(row.Kind), Viewer: row.VerifiedAccountID}, nil
}

// hasEpics is whether a tracker's tickets have epics: a Linear ticket's
// project is its team.
func hasEpics(kind string) bool { return kind == store.TrackerKindJira }
