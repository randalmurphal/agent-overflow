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

// Dashboards is the dashboards the board subscription shows: the
// built-in its kind (whether it plans sprints, whether its tracker has
// epics) picks, then the project's own.
func (s *Service) Dashboards(ctx context.Context, subscription string) ([]trackerdash.DashboardView, error) {
	scope, key, err := s.boardHandle(subscription)
	if err != nil {
		return nil, err
	}
	own, err := s.ownOf(key, scope.Board, false)
	if err != nil {
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
	return s.dash.Dashboards(ctx, trackerdash.Kind{PlansSprints: st.Board.PlansSprints, Epics: hasEpics(key.site.tracker)}, own)
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
	fill := func(ctx context.Context, conn *sql.Conn, relations []string) (store.MirrorExport, bool, error) {
		return s.store.ExportMirrorBoard(ctx, conn, root, string(scope.Board), groups.regroup, relations)
	}
	return trackerdash.Board{Fill: fill, Epics: hasEpics(row.Kind), Viewer: row.VerifiedAccountID}, nil
}

// ownOf is what key's project keeps for board, and with proposals what
// agents proposed on it.
func (s *Service) ownOf(key setupKey, board tracker.BoardID, proposals bool) (trackerdash.Own, error) {
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
		s.propMu.Lock()
		for _, p := range s.proposals[proposalKey{key.slug, key.site, board}] {
			own.Proposals = append(own.Proposals, trackerdash.Proposal{Widget: add(p.widget), Thread: p.thread})
		}
		s.propMu.Unlock()
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
// dashboard's ID. The proposal goes once the dashboards hold it; a Keep
// of a proposal no longer held (kept or dismissed from another window, or
// gone with a restart) is refused.
func (s *Service) KeepProposal(subscription, proposal string, to KeepTarget) (string, error) {
	scope, key, err := s.boardHandle(subscription)
	if err != nil {
		return "", err
	}
	pk := proposalKey{key.slug, key.site, scope.Board}
	s.propMu.Lock()
	i := slices.IndexFunc(s.proposals[pk], func(p heldProposal) bool { return p.widget.ID == proposal })
	var p heldProposal
	if i >= 0 {
		p = s.proposals[pk][i]
	}
	s.propMu.Unlock()
	if i < 0 {
		return "", dashRefusal("dashboard_proposal_gone", "this proposed widget is no longer on the board: it was kept or dismissed in another window, or the app restarted; ask the agent to propose it again")
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
	err = s.editDashboards(key.slug, key.site, string(scope.Board), func(kept []KeptDashboard) ([]KeptDashboard, error) {
		if to.Dashboard == "" {
			return append(kept, KeptDashboard{ID: id, Title: strings.TrimSpace(to.Title), Widgets: []KeptWidget{p.widget}}), nil
		}
		j := slices.IndexFunc(kept, func(d KeptDashboard) bool { return d.ID == to.Dashboard })
		if j < 0 {
			return nil, dashRefusal("dashboard_gone", "that dashboard was deleted; keep the widget on another dashboard or a new one")
		}
		kept[j].Widgets = append(kept[j].Widgets, p.widget)
		return kept, nil
	})
	if err != nil {
		return "", err
	}
	s.dropProposal(pk, proposal)
	return id, nil
}

// DismissProposal drops the widget proposal proposed on the board
// subscription; one no longer held is already gone.
func (s *Service) DismissProposal(subscription, proposal string) error {
	scope, key, err := s.boardHandle(subscription)
	if err != nil {
		return err
	}
	s.dropProposal(proposalKey{key.slug, key.site, scope.Board}, proposal)
	return nil
}

// dropProposal drops proposal from pk's and tells the clients when it was
// held.
func (s *Service) dropProposal(pk proposalKey, proposal string) {
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
}

// RemoveWidget removes widget from dashboard, one the board
// subscription's project keeps; one already gone is removed.
func (s *Service) RemoveWidget(subscription, dashboard, widget string) error {
	scope, key, err := s.boardHandle(subscription)
	if err != nil {
		return err
	}
	return s.editDashboards(key.slug, key.site, string(scope.Board), func(kept []KeptDashboard) ([]KeptDashboard, error) {
		j := slices.IndexFunc(kept, func(d KeptDashboard) bool { return d.ID == dashboard })
		if j < 0 || !slices.ContainsFunc(kept[j].Widgets, func(w KeptWidget) bool { return w.ID == widget }) {
			return nil, errNoEdit
		}
		kept[j].Widgets = slices.DeleteFunc(kept[j].Widgets, func(w KeptWidget) bool { return w.ID == widget })
		return kept, nil
	})
}

// DeleteDashboard deletes dashboard, one the board subscription's project
// keeps, with its widgets; one already gone is deleted.
func (s *Service) DeleteDashboard(subscription, dashboard string) error {
	scope, key, err := s.boardHandle(subscription)
	if err != nil {
		return err
	}
	return s.editDashboards(key.slug, key.site, string(scope.Board), func(kept []KeptDashboard) ([]KeptDashboard, error) {
		if !slices.ContainsFunc(kept, func(d KeptDashboard) bool { return d.ID == dashboard }) {
			return nil, errNoEdit
		}
		return slices.DeleteFunc(kept, func(d KeptDashboard) bool { return d.ID == dashboard }), nil
	})
}

// TryWidget counts widget w on board scope as projectID's dashboards count
// it, over the board's default range in this computer's time zone, for
// an agent of thread. With show it is then proposed on the board: it
// shows on the board's dashboards for the person to keep or dismiss,
// under a widget ID of its own, and the answer's view carries that ID.
// The project must list the board (ErrNotListed), and a board that holds
// maxProposals is refused before anything is counted.
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
	full := func() bool {
		s.propMu.Lock()
		defer s.propMu.Unlock()
		return len(s.proposals[pk]) >= maxProposals
	}
	if show && full() {
		return trackerdash.Tried{}, proposalsFull()
	}
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
