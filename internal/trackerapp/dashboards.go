package trackerapp

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerdash"
)

// A board's dashboards as its subscription shows them: the built-in, the
// ones its project keeps (dashboard_setup.go), and the widgets agents
// proposed on it, which show on each of them until the person keeps or
// dismisses one. A proposal is held here, in memory, by project and
// board, at most maxProposals, and is gone when the app stops; nothing
// joins a dashboard without the person's Keep. Every change of either
// tells the clients (Deps.DashboardsChanged), which read the board's
// dashboards again.

// maxProposals is how many proposed widgets a board holds for a project.
const maxProposals = 5

// proposalKey names the proposals of one project's board.
type proposalKey struct {
	slug  string
	site  siteKey
	board tracker.BoardID
}

// heldProposal is one proposed widget and the thread that proposed it.
type heldProposal struct {
	widget KeptWidget
	thread string
}

// Dashboards is what the board subscription shows: the built-in
// dashboard its kind (whether it plans sprints, whether its tracker has
// epics) picks, the project's own, and the widgets agents proposed on the
// board, laid out with the unit the project counts the board in
// (StatusGroups.unit). It counts nothing.
func (s *Service) Dashboards(ctx context.Context, subscription string) (trackerdash.Listing, error) {
	scope, key, err := s.boardHandle(subscription)
	if err != nil {
		return trackerdash.Listing{}, err
	}
	groups, err := s.boardStatusGroups(key)
	if err != nil {
		return trackerdash.Listing{}, err
	}
	own, err := s.ownOf(key, scope.Board, true)
	if err != nil {
		return trackerdash.Listing{}, err
	}
	root, err := s.readRoot(scope.Connection)
	if err != nil {
		return trackerdash.Listing{}, err
	}
	defer s.leave()
	st, ok, err := s.store.GetMirrorBoard(root, string(scope.Board))
	switch {
	case err != nil:
		return trackerdash.Listing{}, fmt.Errorf("trackerapp: read board %s from the mirror: %w", scope.Board, err)
	case !ok:
		return trackerdash.Listing{}, fmt.Errorf("%w: board %s", ErrNotMirrored, scope.Board)
	}
	return s.dash.Dashboards(ctx, trackerdash.Kind{PlansSprints: st.Board.PlansSprints, Epics: hasEpics(key.site.tracker)}, int(groups.unit(tracker.EstimateUnit(st.Board.EstimateUnit))), own)
}

// DashboardAskBoard is a board as Ask your agent names it: the project
// whose thread asks, and the board by its name and its site/ID, as the
// tracker tools' dashboard_widget takes it.
type DashboardAskBoard struct {
	Project string
	Name    string
	Handle  string
}

// DashboardAsk is the board subscription as Ask your agent names it, in
// the project the subscription was made for.
func (s *Service) DashboardAsk(subscription string) (DashboardAskBoard, error) {
	scope, key, project, err := s.boardHandleProject(subscription)
	if err != nil {
		return DashboardAskBoard{}, err
	}
	root, err := s.readRoot(scope.Connection)
	if err != nil {
		return DashboardAskBoard{}, err
	}
	defer s.leave()
	st, ok, err := s.store.GetMirrorBoard(root, string(scope.Board))
	switch {
	case err != nil:
		return DashboardAskBoard{}, fmt.Errorf("trackerapp: read board %s from the mirror: %w", scope.Board, err)
	case !ok:
		return DashboardAskBoard{}, fmt.Errorf("%w: board %s", ErrNotMirrored, scope.Board)
	}
	return DashboardAskBoard{Project: project, Name: st.Board.Name, Handle: siteName(key.site.site) + "/" + string(scope.Board)}, nil
}

// EvaluateDashboard counts a dashboard of the board subscription and the
// widgets proposed on the board, read with the status groups its project
// gives it.
func (s *Service) EvaluateDashboard(ctx context.Context, subscription string, r trackerdash.Request) (trackerdash.Answer, error) {
	b, err := s.subscribedBoard(subscription)
	if err != nil {
		return trackerdash.Answer{}, err
	}
	defer s.leave()
	return s.dash.Evaluate(ctx, b, r)
}

// DashboardDrill is the rows behind one number of a dashboard of the
// board subscription, from an evaluation of their own.
func (s *Service) DashboardDrill(ctx context.Context, subscription string, r trackerdash.DrillRequest) (trackerdash.DrillAnswer, error) {
	b, err := s.subscribedBoard(subscription)
	if err != nil {
		return trackerdash.DrillAnswer{}, err
	}
	defer s.leave()
	return s.dash.Drill(ctx, b, r)
}

// subscribedBoard is the board subscription as an evaluation reads it,
// with its project's dashboards and the proposals on it. The caller
// leaves the service when the evaluation returns.
func (s *Service) subscribedBoard(subscription string) (trackerdash.Board, error) {
	scope, key, err := s.boardHandle(subscription)
	if err != nil {
		return trackerdash.Board{}, err
	}
	groups, err := s.boardStatusGroups(key)
	if err != nil {
		return trackerdash.Board{}, err
	}
	own, err := s.ownOf(key, scope.Board, true)
	if err != nil {
		return trackerdash.Board{}, err
	}
	b, err := s.dashboardBoard(scope, groups)
	b.Own = own
	return b, err
}

// dashboardBoard is board scope as an evaluation reads it: the store's
// export of its rows with the project's status groups, counted in the
// project's unit (StatusGroups.unit), whether its
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
	counts := func(estimateUnit int) int { return int(groups.unit(tracker.EstimateUnit(estimateUnit))) }
	fill := func(ctx context.Context, conn *sql.Conn, relations []string) (store.MirrorExport, bool, error) {
		return s.store.ExportMirrorBoard(ctx, conn, root, string(scope.Board), groups.regroup, counts, relations, cardShape)
	}
	return trackerdash.Board{Fill: fill, Epics: hasEpics(row.Kind), Viewer: row.VerifiedAccountID}, nil
}

// ownOf is what key's project keeps for board, and with proposals what
// agents proposed on it. With proposals it reads both under propMu, as a
// Keep changes them, so a widget a Keep moves is found in one of them.
func (s *Service) ownOf(key setupKey, board tracker.BoardID, proposals bool) (trackerdash.Own, error) {
	if proposals {
		s.propMu.Lock()
		defer s.propMu.Unlock()
	}
	s.setupMu.Lock()
	held, err := s.heldSetupLocked(key.slug)
	s.setupMu.Unlock()
	if err != nil {
		return trackerdash.Own{}, err
	}
	own := trackerdash.Own{Metrics: map[string]trackerdash.Metric{}}
	add := func(w KeptWidget) trackerdash.Widget {
		m := w.Metric
		m.Name = w.ID
		own.Metrics[w.ID] = m
		return trackerdash.Widget{ID: w.ID, Metric: w.ID, Form: w.Form, Width: w.Width}
	}
	for _, d := range dashboardsOf(held.dashboards, key.site, string(board)) {
		out := trackerdash.Dashboard{ID: d.ID, Title: d.Title}
		for _, w := range d.Widgets {
			out.Widgets = append(out.Widgets, add(w))
		}
		own.Dashboards = append(own.Dashboards, out)
	}
	if proposals {
		for _, p := range s.proposals[proposalKey{key.slug, key.site, board}] {
			own.Proposals = append(own.Proposals, trackerdash.Proposal{Widget: add(p.widget), Thread: p.thread})
		}
	}
	return own, nil
}

// KeepTarget is where Keep puts a proposed widget: at the end of the
// project's dashboard Dashboard, or of a new dashboard titled Title; one
// of the two.
type KeepTarget struct {
	Dashboard string `json:"dashboard,omitempty"`
	Title     string `json:"title,omitempty"`
}

// KeepProposal moves the widget proposal proposed on the board
// subscription onto a dashboard its project keeps, and answers that
// dashboard's ID. The move is one transition under propMu: the proposal
// is found, written to the setup file and dropped while no Dismiss, Keep
// or show can change the board's proposals, so a reader sees it either
// proposed or kept, and the clients are told once, after. A Keep of a
// proposal no longer held (kept or dismissed from another window, or
// gone with a restart) is refused; a write that fails leaves it proposed.
func (s *Service) KeepProposal(subscription, proposal string, to KeepTarget) (string, error) {
	scope, key, err := s.boardHandle(subscription)
	if err != nil {
		return "", err
	}
	if (to.Dashboard == "") == (to.Title == "") {
		return "", fmt.Errorf("%w: keep a widget on one dashboard, named or new", ErrInvalidRequest)
	}
	id := to.Dashboard
	if id == "" {
		if checkDashboardTitle(strings.TrimSpace(to.Title)) != nil {
			return "", dashRefusal("dashboard_title_invalid", fmt.Sprintf("a dashboard's name is 1 to %d characters", maxDashboardTitle))
		}
		id = newOwnID(dashboardPrefix)
	}
	pk := proposalKey{key.slug, key.site, scope.Board}
	s.propMu.Lock()
	err = s.keepLocked(pk, key, string(scope.Board), proposal, to, id)
	s.propMu.Unlock()
	if err != nil {
		return "", err
	}
	s.dashboardsChanged()
	return id, nil
}

// keepLocked is KeepProposal's transition; the caller holds propMu.
func (s *Service) keepLocked(pk proposalKey, key setupKey, board, proposal string, to KeepTarget, id string) error {
	i := slices.IndexFunc(s.proposals[pk], func(p heldProposal) bool { return p.widget.ID == proposal })
	if i < 0 {
		return dashRefusal("dashboard_proposal_gone", "this proposed widget is no longer on the board: it was kept or dismissed in another window, or the app restarted; ask the agent to propose it again")
	}
	widget := s.proposals[pk][i].widget
	_, err := s.editDashboards(key.slug, key.site, board, func(kept []KeptDashboard) ([]KeptDashboard, error) {
		if to.Dashboard == "" {
			return append(kept, KeptDashboard{ID: id, Title: strings.TrimSpace(to.Title), Widgets: []KeptWidget{widget}}), nil
		}
		j := slices.IndexFunc(kept, func(d KeptDashboard) bool { return d.ID == to.Dashboard })
		if j < 0 {
			return nil, dashRefusal("dashboard_gone", "that dashboard is gone; keep the widget on another dashboard or a new one")
		}
		kept[j].Widgets = append(kept[j].Widgets, widget)
		return kept, nil
	})
	if err != nil {
		return err
	}
	s.proposals[pk] = slices.Delete(s.proposals[pk], i, i+1)
	if len(s.proposals[pk]) == 0 {
		delete(s.proposals, pk)
	}
	return nil
}

// DismissProposal drops the widget proposal proposed on the board
// subscription, and tells the clients when it was held; one no longer
// held is already gone.
func (s *Service) DismissProposal(subscription, proposal string) error {
	scope, key, err := s.boardHandle(subscription)
	if err != nil {
		return err
	}
	pk := proposalKey{key.slug, key.site, scope.Board}
	s.propMu.Lock()
	before := len(s.proposals[pk])
	s.proposals[pk] = slices.DeleteFunc(s.proposals[pk], func(p heldProposal) bool { return p.widget.ID == proposal })
	dropped := len(s.proposals[pk]) < before
	if len(s.proposals[pk]) == 0 {
		delete(s.proposals, pk)
	}
	s.propMu.Unlock()
	if dropped {
		s.dashboardsChanged()
	}
	return nil
}

// RemoveWidget removes widget from dashboard, one the board
// subscription's project keeps; the dashboard goes with its last widget,
// and one already gone is removed.
func (s *Service) RemoveWidget(subscription, dashboard, widget string) error {
	scope, key, err := s.boardHandle(subscription)
	if err != nil {
		return err
	}
	changed, err := s.editDashboards(key.slug, key.site, string(scope.Board), func(kept []KeptDashboard) ([]KeptDashboard, error) {
		j := slices.IndexFunc(kept, func(d KeptDashboard) bool { return d.ID == dashboard })
		if j < 0 || !slices.ContainsFunc(kept[j].Widgets, func(w KeptWidget) bool { return w.ID == widget }) {
			return nil, errNoEdit
		}
		kept[j].Widgets = slices.DeleteFunc(kept[j].Widgets, func(w KeptWidget) bool { return w.ID == widget })
		if len(kept[j].Widgets) == 0 {
			kept = slices.Delete(kept, j, j+1)
		}
		return kept, nil
	})
	if changed {
		s.dashboardsChanged()
	}
	return err
}

// TryWidget counts widget w on board scope as projectID's dashboards count
// it, over the board's default range in this computer's time zone, for
// an agent of thread. With show it is then proposed on the board: it
// shows on the board's dashboards for the person to keep or dismiss,
// under a widget ID of its own, and the answer's view carries that ID.
// The project must list the board (ErrNotListed), and a show on a board
// that holds maxProposals is refused.
func (s *Service) TryWidget(ctx context.Context, projectID string, scope Scope, w KeptWidget, thread string, show bool) (trackerdash.Tried, error) {
	if err := scope.boardOnly(); err != nil {
		return trackerdash.Tried{}, err
	}
	if err := s.enter(); err != nil {
		return trackerdash.Tried{}, err
	}
	defer s.leave()
	slug, listed, err := s.store.ProjectTrackerBoardListed(projectID, scope.Connection, string(scope.Board))
	switch {
	case err != nil:
		return trackerdash.Tried{}, fmt.Errorf("trackerapp: read the boards of project %s: %w", projectID, err)
	case !listed:
		return trackerdash.Tried{}, fmt.Errorf("%w: project %s, board %s of %s", ErrNotListed, projectID, scope.Board, scope.Connection)
	}
	row, err := s.store.GetTrackerConnection(scope.Connection)
	if err != nil {
		return trackerdash.Tried{}, fmt.Errorf("trackerapp: read connection %s: %w", scope.Connection, err)
	}
	key := setupKey{slug: slug, site: siteKey{row.Kind, row.Site}}
	pk := proposalKey{slug, key.site, scope.Board}
	groups, err := s.boardStatusGroups(key)
	if err != nil {
		return trackerdash.Tried{}, err
	}
	b, err := s.dashboardBoard(scope, groups)
	if err != nil {
		return trackerdash.Tried{}, err
	}
	w.ID = newOwnID(widgetPrefix)
	m := w.Metric
	m.Name = w.ID
	tried, err := s.dash.Try(ctx, b, trackerdash.Widget{Form: w.Form, Width: w.Width}, m, "Local")
	s.leave()
	if err != nil || !show {
		return tried, err
	}
	s.propMu.Lock()
	if len(s.proposals[pk]) >= maxProposals {
		s.propMu.Unlock()
		return trackerdash.Tried{}, proposalsFull()
	}
	if s.proposals == nil {
		s.proposals = map[proposalKey][]heldProposal{}
	}
	s.proposals[pk] = append(s.proposals[pk], heldProposal{widget: w, thread: thread})
	s.propMu.Unlock()
	tried.View.ID = w.ID
	s.dashboardsChanged()
	return tried, nil
}

func proposalsFull() error {
	return dashRefusal("dashboard_proposals_full", fmt.Sprintf("the board already shows %d proposed widgets; ask the user to keep or dismiss one, then show this one", maxProposals))
}

// hasEpics is whether a tracker's tickets have epics: a Linear ticket's
// project is its team.
func hasEpics(kind string) bool { return kind == store.TrackerKindJira }
