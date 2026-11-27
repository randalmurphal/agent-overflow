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
// tells the board's subscriptions of the project, on the board's event,
// that its dashboards changed and whether what they count did
// (tellDashboards): a limit changes how a number is drawn, not the
// number.

// maxProposals is how many proposed widgets a board holds for a project.
const maxProposals = 5

// proposalKey names the proposals of one project's board.
type proposalKey struct {
	slug  string
	site  siteKey
	board tracker.BoardID
}

// heldProposal is one proposed widget, the thread that proposed it, and
// what the agent asked for its tile (TileMarks): its limit in the unit
// the widget's number was in when it was proposed, nil for none, and
// whether Keep pins it.
type heldProposal struct {
	widget KeptWidget
	thread string
	limit  *TileLimit
	pin    bool
}

// TileMarks is what an agent asks for the tile of a widget it proposes:
// the limit of a number widget, nil for none, and whether Keep pins it to
// the board's summary row.
type TileMarks struct {
	Limit *float64
	Pin   bool
}

// Dashboards is what the board subscription shows: the built-in
// dashboard its kind (whether it plans sprints, whether its tracker has
// epics) picks, the project's own, and the widgets agents proposed on the
// board, laid out with the unit the project counts the board in
// (StatusGroups.unit), with the limits of the board's summary row as the
// project keeps them and the agents asked for them (tilesView). It counts
// nothing.
func (s *Service) Dashboards(ctx context.Context, subscription string) (trackerdash.Listing, BoardTilesView, error) {
	scope, key, err := s.boardHandle(subscription)
	if err != nil {
		return trackerdash.Listing{}, BoardTilesView{}, err
	}
	tb, err := s.tileBoardOf(scope, key)
	if err != nil {
		return trackerdash.Listing{}, BoardTilesView{}, err
	}
	own, err := s.ownOf(key, scope.Board, true)
	if err != nil {
		return trackerdash.Listing{}, BoardTilesView{}, err
	}
	l, err := s.dash.Dashboards(ctx, tb.kind, tb.estimateUnit, own.Own)
	if err != nil {
		return trackerdash.Listing{}, BoardTilesView{}, err
	}
	return l, tilesView(own.tiles, tileWidgets(tb.kind, own.kept), tb.estimateUnit, own.proposed, l), nil
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
	b, err := s.subscribedBoard(subscription, r)
	if err != nil {
		return trackerdash.Answer{}, err
	}
	defer s.leave()
	return s.dash.Evaluate(ctx, b, r)
}

// DashboardDrill is the rows behind one number of a dashboard of the
// board subscription, from an evaluation of their own.
func (s *Service) DashboardDrill(ctx context.Context, subscription string, r trackerdash.DrillRequest) (trackerdash.DrillAnswer, error) {
	b, err := s.subscribedBoard(subscription, r.Request)
	if err != nil {
		return trackerdash.DrillAnswer{}, err
	}
	defer s.leave()
	return s.dash.Drill(ctx, b, r)
}

// subscribedBoard is the board subscription as an evaluation of r reads
// it, with its project's dashboards and the proposals on it. A request
// that names widgets counts a summary row's, at most maxPins, whether it
// is a count's or a drill's. The caller leaves the service when the
// evaluation returns.
func (s *Service) subscribedBoard(subscription string, r trackerdash.Request) (trackerdash.Board, error) {
	if len(r.Widgets) > maxPins {
		return trackerdash.Board{}, fmt.Errorf("%w: a summary row counts at most %d widgets, not %d", ErrInvalidRequest, maxPins, len(r.Widgets))
	}
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
	b.Own = own.Own
	return b, err
}

// dashboardBoard is board scope as an evaluation reads it: the store's
// export of its rows with the project's status groups, its
// EstimateUnit the unit the project counts the board in (StatusGroups.unit)
// while the copy holds the board's own estimates, whether its
// tracker has epics, the connection's verified account for Mine, and
// while the project counts credit, why the evidence unit's last read of
// who worked on the tickets failed as a whole (UnitEvidence). The caller
// leaves the service when the evaluation returns.
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
		export, ok, err := s.store.ExportMirrorBoard(ctx, conn, root, string(scope.Board), groups.regroup, relations, s.window().UnixMilli(), millis(s.now()), cardShape)
		export.EstimateUnit = int(groups.unit(tracker.EstimateUnit(export.EstimateUnit)))
		return export, ok, err
	}
	b := trackerdash.Board{Fill: fill, Epics: hasEpics(row.Kind), Credit: groups.credit, Viewer: row.VerifiedAccountID}
	if groups.credit {
		if b.EvidenceFailure, err = s.store.GetMirrorEvidenceFailure(root); err != nil {
			s.leave()
			return trackerdash.Board{}, fmt.Errorf("trackerapp: read the evidence read's outcome of %s: %w", scope.Connection, err)
		}
	}
	return b, nil
}

// boardOwn is what a project keeps for a board as an evaluation reads it
// (trackerdash.Own), with its dashboards and tiles as kept and, with
// proposals, the proposals as held.
type boardOwn struct {
	trackerdash.Own
	kept     []KeptDashboard
	tiles    BoardTiles
	proposed []heldProposal
}

// ownOf is what key's project keeps for board, and with proposals what
// agents proposed on it. With proposals it reads both under propMu, as a
// Keep changes them, so a widget a Keep moves is found in one of them.
func (s *Service) ownOf(key setupKey, board tracker.BoardID, proposals bool) (boardOwn, error) {
	if proposals {
		s.propMu.Lock()
		defer s.propMu.Unlock()
	}
	s.setupMu.Lock()
	held, err := s.heldSetupLocked(key.slug)
	s.setupMu.Unlock()
	if err != nil {
		return boardOwn{}, err
	}
	own := trackerdash.Own{Metrics: map[string]trackerdash.Metric{}}
	add := func(w KeptWidget) trackerdash.Widget {
		m := w.Metric
		m.Name = w.ID
		own.Metrics[w.ID] = m
		return trackerdash.Widget{ID: w.ID, Metric: w.ID, Form: w.Form, Width: w.Width}
	}
	kept := dashboardsOf(held.kept.dashboards, key.site, string(board))
	for _, d := range kept {
		out := trackerdash.Dashboard{ID: d.ID, Title: d.Title}
		for _, w := range d.Widgets {
			out.Widgets = append(out.Widgets, add(w))
		}
		own.Dashboards = append(own.Dashboards, out)
	}
	out := boardOwn{kept: kept, tiles: cloneTiles(tilesOf(held.kept.tiles, key.site, string(board)))}
	if proposals {
		out.proposed = slices.Clone(s.proposals[proposalKey{key.slug, key.site, board}])
		for _, p := range out.proposed {
			own.Proposals = append(own.Proposals, trackerdash.Proposal{Widget: add(p.widget), Thread: p.thread})
		}
	}
	out.Own = own
	return out, nil
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
	tb, err := s.tileBoardOf(scope, key)
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
	err = s.keepLocked(pk, key, string(scope.Board), tb, proposal, to, id)
	s.propMu.Unlock()
	if err != nil {
		return "", err
	}
	s.tellDashboards(key.slug, true, boardIs(key.site, string(scope.Board)))
	return id, nil
}

// keepLocked is KeepProposal's transition; the caller holds propMu.
func (s *Service) keepLocked(pk proposalKey, key setupKey, board string, tb tileBoard, proposal string, to KeepTarget, id string) error {
	i := slices.IndexFunc(s.proposals[pk], func(p heldProposal) bool { return p.widget.ID == proposal })
	if i < 0 {
		return dashRefusal("dashboard_proposal_gone", "this proposed widget is no longer on the board: it was kept or dismissed in another window, or the app restarted; ask the agent to propose it again")
	}
	p := s.proposals[pk][i]
	widget := p.widget
	_, err := s.editBoard(key.slug, key.site, board, tb.kind, func(b *boardKept, _ map[string]tileWidget) error {
		if to.Dashboard == "" {
			b.dashboards = append(b.dashboards, KeptDashboard{ID: id, Title: strings.TrimSpace(to.Title), Widgets: []KeptWidget{widget}})
		} else {
			j := slices.IndexFunc(b.dashboards, func(d KeptDashboard) bool { return d.ID == to.Dashboard })
			if j < 0 {
				return dashRefusal("dashboard_gone", "that dashboard is gone; keep the widget on another dashboard or a new one")
			}
			b.dashboards[j].Widgets = append(b.dashboards[j].Widgets, widget)
		}
		// What the agent asked for its tile comes with it, the limit in the
		// unit it was proposed in; a pin joins the row when it has room, as
		// the screen said before the Keep.
		if p.limit != nil {
			if b.tiles.Limits == nil {
				b.tiles.Limits = map[string]TileLimit{}
			}
			b.tiles.Limits[widget.ID] = *p.limit
		}
		// A full row refuses the pin and the widget is kept unpinned,
		// as the screen said before the Keep; the widget is new, so it
		// is not pinned already.
		if p.pin {
			_ = admitPin(&b.tiles, widget.ID)
		}
		return nil
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
		s.tellDashboards(key.slug, true, boardIs(key.site, string(scope.Board)))
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
	tb, err := s.tileBoardOf(scope, key)
	if err != nil {
		return err
	}
	changed, err := s.editBoard(key.slug, key.site, string(scope.Board), tb.kind, func(b *boardKept, _ map[string]tileWidget) error {
		j := slices.IndexFunc(b.dashboards, func(d KeptDashboard) bool { return d.ID == dashboard })
		if j < 0 || !slices.ContainsFunc(b.dashboards[j].Widgets, func(w KeptWidget) bool { return w.ID == widget }) {
			return errNoEdit
		}
		b.dashboards[j].Widgets = slices.DeleteFunc(b.dashboards[j].Widgets, func(w KeptWidget) bool { return w.ID == widget })
		if len(b.dashboards[j].Widgets) == 0 {
			b.dashboards = slices.Delete(b.dashboards, j, j+1)
		}
		// Its tile and its limit go with it (editBoard's pruneTiles).
		return nil
	})
	if changed {
		s.tellDashboards(key.slug, true, boardIs(key.site, string(scope.Board)))
	}
	return err
}

// TryWidget counts widget w on board scope as projectID's dashboards count
// it, over the board's default range in this computer's time zone, for
// an agent of thread. With show it is then proposed on the board: it
// shows on the board's dashboards for the person to keep or dismiss,
// under a widget ID of its own, and the answer's view carries that ID;
// marks are what Keep gives its tile.
// The project must list the board (ErrNotListed), and a show on a board
// that holds maxProposals is refused.
func (s *Service) TryWidget(ctx context.Context, projectID string, scope Scope, w KeptWidget, marks TileMarks, thread string, show bool) (trackerdash.Tried, error) {
	if err := scope.boardOnly(); err != nil {
		return trackerdash.Tried{}, err
	}
	if err := checkMarks(w, marks); err != nil {
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
	p := heldProposal{widget: w, thread: thread, pin: marks.Pin}
	if marks.Limit != nil {
		p.limit = &TileLimit{Value: *marks.Limit, Unit: tried.Unit}
	}
	s.proposals[pk] = append(s.proposals[pk], p)
	s.propMu.Unlock()
	tried.View.ID = w.ID
	s.tellDashboards(slug, true, boardIs(key.site, string(scope.Board)))
	return tried, nil
}

func proposalsFull() error {
	return dashRefusal("dashboard_proposals_full", fmt.Sprintf("the board already shows %d proposed widgets; ask the user to keep or dismiss one, then show this one", maxProposals))
}

// hasEpics is whether a tracker's tickets have epics: a Linear ticket's
// project is its team.
func hasEpics(kind string) bool { return kind == store.TrackerKindJira }
