package app

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/pump"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

// A project's tracker boards and board subscriptions. trackerapp owns
// both; these are their bound methods. A board subscription belongs to
// the client connection that made it: its handle is tied to that
// connection, whose end releases it (trackerSubscriptionTie). The reads
// that follow a subscribe name its handle, which a client pins to the
// computer that answered the subscribe (methodFamilies.ts, family
// "subscription"). The wire shapes are trackerapp's own.

// TrackerProjects lists the tracker projects (Linear teams) the
// connection's user can see, for choosing a project's boards. It asks
// the tracker, a page at a time, and the whole walk ends at the method's
// budget (trackerCatalogRun). Its context reaches the network, so it is
// not caller-bound.
//
//ao:scope tracker:read
//ao:route selected
func (a *App) TrackerProjects(ctx context.Context, connectionID string) ([]trackerapp.ProjectView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, err
	}
	ctx, cancel := a.trackerCatalogRun(ctx, "TrackerProjects", "Listing the tracker's projects")
	defer cancel()
	out, err := svc.Projects(ctx, connectionID)
	return out, trackerCatalogError(ctx, err)
}

// TrackerBoards lists a tracker project's boards (a Linear team's one),
// for choosing a project's boards. It asks the tracker, bounded as
// TrackerProjects is.
//
//ao:scope tracker:read
//ao:route selected
func (a *App) TrackerBoards(ctx context.Context, connectionID, project string) ([]trackerapp.BoardInfoView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, err
	}
	ctx, cancel := a.trackerCatalogRun(ctx, "TrackerBoards", "Listing the project's boards")
	defer cancel()
	out, err := svc.Boards(ctx, connectionID, project)
	return out, trackerCatalogError(ctx, err)
}

// trackerCatalogRun bounds a picker's walk of the tracker's pages, each
// of which the tracker client bounds alone, by the method's budget, so
// the server answers before its client stops waiting
// (transport.ClientWait).
func (a *App) trackerCatalogRun(ctx context.Context, method, doing string) (context.Context, context.CancelFunc) {
	budget := transport.MethodBudget(method)
	if a.trackerPickerBudget > 0 {
		budget = a.trackerPickerBudget
	}
	return withinBudget(ctx, budgetExceeded{doing: doing, budget: budget, then: "the tracker was too slow, so try again"})
}

// trackerCatalogError is the budget's own error once ctx ended at it,
// and otherwise err with its refusal's sentence (trackerError).
func trackerCatalogError(ctx context.Context, err error) error {
	if cut := budgetError(ctx, err); cut != err {
		return cut
	}
	return trackerError(err)
}

// ProjectTrackerBoards lists the boards a project's board picker shows, in
// its order, and which opens first.
//
//ao:scope tracker:read
func (a *App) ProjectTrackerBoards(ctx context.Context, projectID string) ([]trackerapp.ProjectBoard, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, err
	}
	out, err := svc.ProjectBoards(projectID)
	return out, trackerError(err)
}

// SetProjectTrackerBoards makes boards, in order, the boards a project's
// picker shows, and answers the list stored. A board it leaves out that
// no other project lists stops being kept on this computer, and its open
// views end.
//
//ao:scope tracker:operate
func (a *App) SetProjectTrackerBoards(ctx context.Context, projectID string, boards []trackerapp.ProjectBoard) ([]trackerapp.ProjectBoard, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, err
	}
	out, err := svc.SetProjectBoards(projectID, boards)
	return out, trackerError(err)
}

// SubscribeTrackerBoard subscribes the calling client to a board its
// project lists, with the first page of each section asked for. The
// answer comes from this computer's copy of the board; the board's reads
// of the tracker follow it, and tracker:board_updated tells what they
// change. The subscription is released by ReleaseTrackerSubscription or
// when the client's connection ends.
//
//ao:scope tracker:read
//ao:callerbound
func (a *App) SubscribeTrackerBoard(ctx context.Context, projectID, connectionID, boardID string, sections []trackerapp.Section) (trackerapp.BoardSubscription, error) {
	conn, err := transport.CallerConnection(ctx)
	if err != nil {
		return trackerapp.BoardSubscription{}, err
	}
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.BoardSubscription{}, err
	}
	scope := trackerapp.Scope{Connection: connectionID, Board: tracker.BoardID(boardID)}
	sub, err := svc.SubscribeBoard(projectID, scope, sections, trackerSubscriptionTie(conn, svc))
	return sub, trackerError(err)
}

// trackerSubscriptionTie ties a subscription handle to conn as trackerapp
// publishes it: conn's end releases the handle, and the handle's removal
// unties conn.
func trackerSubscriptionTie(conn *transport.ConnState, svc *trackerapp.Service) trackerapp.Tie {
	return func(id string) (func(), error) {
		tie, err := conn.Bind(trackerSubscriptionCleanupKey(id), func() { svc.ReleaseSubscription(id) })
		if err != nil {
			return nil, fmt.Errorf("tracker subscription: %w", err)
		}
		return tie.Untie, nil
	}
}

func trackerSubscriptionCleanupKey(id string) string { return "tracker-subscription:" + id }

// TrackerBoard reads a subscribed board whole again, with the first page
// of each section asked for: after an event that says to read it again,
// a reconnect, or a page that went stale.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerBoard(ctx context.Context, subscriptionID string, sections []trackerapp.Section) (trackerapp.BoardView, error) {
	svc, scope, err := a.trackerSubscriptionBoard(subscriptionID)
	if err != nil {
		return trackerapp.BoardView{}, err
	}
	out, err := svc.Board(scope, sections)
	return out, trackerError(err)
}

// TrackerSectionRows reads the next page of a subscribed board's section
// after the cursor; no cursor reads its first page.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerSectionRows(ctx context.Context, subscriptionID string, section trackerapp.Section, after *trackerapp.SectionCursor) (trackerapp.SectionPage, error) {
	svc, scope, err := a.trackerSubscriptionBoard(subscriptionID)
	if err != nil {
		return trackerapp.SectionPage{}, err
	}
	out, err := svc.SectionRows(scope, section, after)
	return out, trackerError(err)
}

// TrackerBoardPatch reads what a tracker:board_updated event changed in
// the sections a client holds: the named issues' rows in each, and every
// section's total unless held names the ones the board has now.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerBoardPatch(ctx context.Context, subscriptionID string, sections []trackerapp.Section, issues []string, held trackerapp.TotalsAt) (trackerapp.BoardPatch, error) {
	svc, scope, err := a.trackerSubscriptionBoard(subscriptionID)
	if err != nil {
		return trackerapp.BoardPatch{}, err
	}
	ids := make([]tracker.IssueID, len(issues))
	for i, id := range issues {
		ids[i] = tracker.IssueID(id)
	}
	out, err := svc.BoardPatch(scope, sections, ids, held)
	return out, trackerError(err)
}

// trackerSubscriptionBoard is the service and the board a board
// subscription's handle names.
func (a *App) trackerSubscriptionBoard(subscriptionID string) (*trackerapp.Service, trackerapp.Scope, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, trackerapp.Scope{}, err
	}
	scope, err := svc.SubscriptionBoard(subscriptionID)
	if err != nil {
		return nil, trackerapp.Scope{}, trackerError(err)
	}
	return svc, scope, nil
}

// SetTrackerSubscriptionVisible records whether the client shows what it
// subscribed to; a board nobody shows is not read.
//
//ao:scope tracker:read
//ao:route home
func (a *App) SetTrackerSubscriptionVisible(ctx context.Context, subscriptionID string, visible bool) error {
	svc, err := a.trackerService()
	if err != nil {
		return err
	}
	return trackerError(svc.SetSubscriptionVisible(subscriptionID, visible))
}

// RefreshTrackerSubscription reads the subscribed board from the tracker
// now, as the person's action, and returns when the read ran. What it
// found, a failure of the tracker included, arrives as events. The wait
// is bounded by the method's budget. If the board's pump was still in a
// read of its own all that time, this refresh never ran and the answer
// says to refresh again; if this refresh's own read is long, it goes on
// after the answer, its events arriving as they land.
//
//ao:scope tracker:read
//ao:route home
func (a *App) RefreshTrackerSubscription(ctx context.Context, subscriptionID string) error {
	svc, err := a.trackerService()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeoutCause(ctx, transport.MethodBudget("RefreshTrackerSubscription"), errTrackerRefreshWait)
	defer cancel()
	err = svc.RefreshSubscription(ctx, subscriptionID)
	switch {
	case err == nil || !errors.Is(context.Cause(ctx), errTrackerRefreshWait):
	case errors.Is(err, pump.ErrNotTaken):
		return errorsx.Public("tracker_refresh_not_run", "the board was busy with an earlier read from the tracker, so this refresh did not run; refresh again", err)
	default:
		return errorsx.Public("tracker_refresh_waiting", "the board is still reading from the tracker; it updates as each read finishes, so refresh again if what you expect does not appear", err)
	}
	return trackerError(err)
}

// errTrackerRefreshWait is the cause of a refresh's wait cut at its
// method's budget.
var errTrackerRefreshWait = errors.New("the refresh's wait reached its method's budget")

// ReleaseTrackerSubscription releases a subscription, from whichever
// connection asks; the connection that made it is untied with it. An
// unknown or already released ID is a no-op, since the connection's end
// may have released it first.
//
//ao:scope tracker:read
//ao:route home
func (a *App) ReleaseTrackerSubscription(ctx context.Context, subscriptionID string) error {
	svc, err := a.trackerService()
	if err != nil {
		return err
	}
	svc.ReleaseSubscription(subscriptionID)
	return nil
}

// trackerError gives each tracker refusal a person can cause or see a
// code a client branches on and a sentence that says what to do next,
// keeping the service's error as its cause. Other errors pass as they
// are.
func trackerError(err error) error {
	code, message := trackerRefusal(err)
	if code == "" {
		return err
	}
	return errorsx.Public(code, message, err)
}

// trackerRefusal is err's code and sentence, none for an error that is no
// refusal of the trackers'.
func trackerRefusal(err error) (code, message string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, trackerapp.ErrUnknownSubscription):
		return "tracker_subscription_unknown", "this board view is no longer open on this computer (released, or its connection or board went away); open the board again"
	case errors.Is(err, trackerapp.ErrSubscriptionEnded):
		return "tracker_subscription_ended", "this board view closed before the board was read: its connection was turned off or removed, or no project lists the board any more; open the board again"
	case errors.Is(err, trackerapp.ErrTooManySubscriptions):
		return "tracker_subscriptions_full", "this computer already has 64 boards open across its windows and browsers; close a board, then open this one"
	case errors.Is(err, trackerapp.ErrDisabled):
		return "tracker_connection_off", "this tracker connection is turned off; turn it on in the Issues settings, then try again"
	case errors.Is(err, trackerapp.ErrNotFound):
		return "tracker_connection_missing", "this tracker connection was removed from this computer; add it again in the Issues settings, or choose another board"
	case errors.Is(err, trackerapp.ErrNoProvider):
		return "tracker_connection_tempo", "a Tempo connection has no projects or boards; choose its Jira connection instead"
	case errors.Is(err, trackerapp.ErrNotListed):
		return "tracker_board_not_listed", "this project does not list that board; add it to the project's boards, or choose one it lists"
	case errors.Is(err, trackerapp.ErrNotMirrored):
		return "tracker_board_not_read", "this computer has not read this board from the tracker yet; it shows once the first read finishes"
	case errors.Is(err, trackerapp.ErrStalePage):
		return "tracker_page_stale", "the board changed since this section was read; read the section again from its start"
	case errors.Is(err, trackerapp.ErrProjectNotFound):
		return "tracker_project_missing", "this project no longer exists on this computer"
	case errors.Is(err, trackerapp.ErrInvalidBoards):
		return "tracker_boards_refused", "this board list cannot be saved; fix what the detail names and save again"
	}
	return "", ""
}
