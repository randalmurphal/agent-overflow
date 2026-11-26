package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/pump"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

// A project's tracker boards and the board and ticket subscriptions.
// trackerapp owns them; these are their bound methods. A subscription
// belongs to the client connection that made it: its handle is tied to
// that connection, whose end releases it (trackerSubscriptionTie). The
// reads that follow a subscribe name its handle, which a client pins to
// the computer that answered the subscribe (methodFamilies.ts, family
// "subscription"). The wire shapes are trackerapp's own.

// TrackerProjects lists the tracker projects (Linear teams) the
// connection's user can see, for choosing a project's boards. It asks
// the tracker, a page at a time, and the whole walk ends at the method's
// budget (trackerRun). Its context reaches the network, so it is
// not caller-bound.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerProjects(ctx context.Context, connectionID string) ([]trackerapp.ProjectView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, err
	}
	ctx, cancel := a.trackerRun(ctx, "TrackerProjects", "Listing the tracker's projects", trackerSlow)
	defer cancel()
	out, err := svc.Projects(ctx, connectionID)
	return out, trackerRunError(ctx, err)
}

// TrackerBoards lists a tracker project's boards (a Linear team's one),
// for choosing a project's boards. It asks the tracker, bounded as
// TrackerProjects is.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerBoards(ctx context.Context, connectionID, project string) ([]trackerapp.BoardInfoView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, err
	}
	ctx, cancel := a.trackerRun(ctx, "TrackerBoards", "Listing the project's boards", trackerSlow)
	defer cancel()
	out, err := svc.Boards(ctx, connectionID, project)
	return out, trackerRunError(ctx, err)
}

// trackerRun bounds a method's run of tracker requests (a form read, a
// picker's walk of pages, a write and its refresh), each of which the
// tracker client bounds alone, by the method's budget, so the server
// answers before its client stops waiting (transport.ClientWait). then
// says what the person does when the budget cuts it.
func (a *App) trackerRun(ctx context.Context, method, doing, then string) (context.Context, context.CancelFunc) {
	budget := transport.MethodBudget(method)
	if a.trackerBudget > 0 {
		budget = a.trackerBudget
	}
	return withinBudget(ctx, budgetExceeded{doing: doing, budget: budget, then: then})
}

// trackerSlow is a form read's advice when its budget cut it.
const trackerSlow = "the tracker was too slow, so try again"

// trackerRunError is the budget's own error once ctx, a method's run
// bounded by its budget, ended at it, and otherwise err with its
// refusal's sentence (trackerError).
func trackerRunError(ctx context.Context, err error) error {
	if cut := budgetError(ctx, err); cut != err {
		return cut
	}
	return trackerError(err)
}

// ProjectTrackerBoards lists the boards a project's board picker shows, in
// its order, and which opens first, each named and with its connection's
// kind, label and state.
//
//ao:scope tracker:read
func (a *App) ProjectTrackerBoards(ctx context.Context, projectID string) ([]trackerapp.BoardChoice, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, err
	}
	out, err := svc.ProjectBoards(projectID)
	return out, trackerError(err)
}

// TrackerBoardChoices lists the boards every project of this computer
// lists, as ProjectTrackerBoards names each: the Issues pane's board
// picker, which asks every attached computer.
//
//ao:scope tracker:read
//ao:route all
func (a *App) TrackerBoardChoices(ctx context.Context) ([]trackerapp.BoardChoice, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, err
	}
	out, err := svc.BoardChoices()
	return out, trackerError(err)
}

// SetProjectTrackerBoards makes boards, in order, the boards a project's
// picker shows, and answers the list stored as ProjectTrackerBoards reads
// it. held is the list the picker showed and built boards from; a save
// built from a list that is no longer the stored one is refused. A board
// it adds is stored with the name and tracker project the picker showed.
// A board it leaves out that no other project lists stops being kept on
// this computer, and its open views end.
//
//ao:scope tracker:operate
func (a *App) SetProjectTrackerBoards(ctx context.Context, projectID string, held []trackerapp.BoardRef, boards []trackerapp.ProjectBoard) ([]trackerapp.BoardChoice, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, err
	}
	out, err := svc.SetProjectBoards(projectID, held, boards)
	return out, trackerError(err)
}

// SubscribeTrackerBoard subscribes the calling client to a board its
// project lists, with the first page of each section asked for, or of the
// Board screen's sections when none is (trackerapp.Service.Board). The
// answer comes from this computer's copy of the board; the board's reads
// of the tracker follow it once a subscription shows it (visible here, or
// SetTrackerSubscriptionVisible later), and tracker:board_updated tells
// what they change. The subscription is released by
// ReleaseTrackerSubscription or when the client's connection ends.
//
//ao:scope tracker:read
//ao:callerbound
func (a *App) SubscribeTrackerBoard(ctx context.Context, projectID, connectionID, boardID string, sections []trackerapp.Section, visible bool) (trackerapp.BoardSubscription, error) {
	conn, err := transport.CallerConnection(ctx)
	if err != nil {
		return trackerapp.BoardSubscription{}, err
	}
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.BoardSubscription{}, err
	}
	scope := trackerapp.Scope{Connection: connectionID, Board: tracker.BoardID(boardID)}
	sub, err := svc.SubscribeBoard(projectID, scope, sections, visible, trackerSubscriptionTie(conn, svc))
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
// of each section asked for, or of the Board screen's sections when none
// is (trackerapp.Service.Board): after an event that says to read it
// again, a reconnect, or a page that went stale.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerBoard(ctx context.Context, subscriptionID string, sections []trackerapp.Section) (trackerapp.BoardView, error) {
	svc, scope, groups, err := a.trackerSubscriptionBoard(subscriptionID)
	if err != nil {
		return trackerapp.BoardView{}, err
	}
	out, err := svc.Board(scope, groups, sections)
	return out, trackerError(err)
}

// TrackerSectionRows reads the next page of a subscribed board's section
// after the cursor; no cursor reads its first page.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerSectionRows(ctx context.Context, subscriptionID string, section trackerapp.Section, after *trackerapp.SectionCursor) (trackerapp.SectionPage, error) {
	svc, scope, groups, err := a.trackerSubscriptionBoard(subscriptionID)
	if err != nil {
		return trackerapp.SectionPage{}, err
	}
	out, err := svc.SectionRows(scope, groups, section, after)
	return out, trackerError(err)
}

// TrackerBoardPatch reads what a tracker:board_updated event changed in
// the sections a client holds: the named issues' rows in each, and every
// section's total unless held names the ones the board has now.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerBoardPatch(ctx context.Context, subscriptionID string, sections []trackerapp.Section, issues []string, held trackerapp.TotalsAt) (trackerapp.BoardPatch, error) {
	svc, scope, groups, err := a.trackerSubscriptionBoard(subscriptionID)
	if err != nil {
		return trackerapp.BoardPatch{}, err
	}
	ids := make([]tracker.IssueID, len(issues))
	for i, id := range issues {
		ids[i] = tracker.IssueID(id)
	}
	out, err := svc.BoardPatch(scope, groups, sections, ids, held)
	return out, trackerError(err)
}

// trackerSubscriptionBoard is the service, the board a board
// subscription's handle names and the status groups its reads apply.
func (a *App) trackerSubscriptionBoard(subscriptionID string) (*trackerapp.Service, trackerapp.Scope, trackerapp.StatusGroups, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, trackerapp.Scope{}, trackerapp.StatusGroups{}, err
	}
	scope, groups, err := svc.SubscriptionBoard(subscriptionID)
	if err != nil {
		return nil, trackerapp.Scope{}, trackerapp.StatusGroups{}, trackerError(err)
	}
	return svc, scope, groups, nil
}

// SetTrackerSubscriptionVisible records whether the client shows what it
// subscribed to; a board or ticket nobody shows is not read.
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

// RefreshTrackerSubscription reads the subscribed board or ticket from
// the tracker now, as the person's action, and returns when the read ran.
// What it found, a failure of the tracker included, arrives as events.
// The wait is bounded by the method's budget. If the subscription's pump
// was still in a read of its own all that time, this refresh never ran
// and the answer says to refresh again; if this refresh's own read is
// long, it goes on after the answer, its events arriving as they land.
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
		return errorsx.Public("tracker_refresh_not_run", "this view was busy with an earlier read from the tracker, so this refresh did not run; refresh again", err)
	default:
		return errorsx.Public("tracker_refresh_waiting", "this refresh was still reading from the tracker when its answer was due; what it finds shows as it lands, so refresh again if what you expect does not appear", err)
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
// refusal of the trackers'. A tracker call that failed is told by its
// kind (tracker_failed_<kind>) with trackerapp's sentence for it, unless
// it refused the input or the request before sending it.
func trackerRefusal(err error) (code, message string) {
	var invalid *trackerapp.InvalidError
	var unreadable *trackerapp.SetupFileError
	var refusedSetup *trackerapp.SetupError
	var unsureWrite *trackerapp.SetupWriteError
	failed, isCall := errors.AsType[*trackerapp.CallFailure](err)
	switch {
	case err == nil:
		return "", ""
	case errors.As(err, &invalid):
		return "tracker_connection_invalid", invalid.Reason
	case errors.Is(err, store.ErrTrackerConnectionExists):
		return "tracker_connection_exists", "this computer already has that connection (the same Jira site and email, the same Linear workspace, or Tempo for that Jira connection); use the one listed"
	case errors.Is(err, tracker.ErrUnsupported):
		return "tracker_unsupported", "this tracker does not support that"
	case errors.Is(err, trackerapp.ErrInvalidRequest), errors.Is(err, tracker.ErrInvalidInput):
		return "tracker_request_invalid", "nothing was sent: " + refusedInput(err)
	case isCall:
		return "tracker_failed_" + string(failed.Failure.Kind), failed.Failure.Message
	case errors.Is(err, trackerapp.ErrUnknownSubscription):
		return "tracker_subscription_unknown", "this board or ticket view is no longer open on this computer (released, or its connection, board or ticket went away); open it again"
	case errors.Is(err, trackerapp.ErrSubscriptionEnded):
		return "tracker_subscription_ended", "this view closed before it was read: its connection was turned off or removed, or no project lists its board any more; open it again"
	case errors.Is(err, trackerapp.ErrTooManySubscriptions):
		return "tracker_subscriptions_full", "this computer already has 64 boards or 64 tickets open across its windows and browsers; close one, then open this one"
	case errors.Is(err, trackerapp.ErrDisabled):
		return "tracker_connection_off", "this tracker connection is turned off; turn it on in the Issues settings, then try again"
	case errors.Is(err, trackerapp.ErrNotFound):
		return "tracker_connection_missing", "this tracker connection was removed from this computer; add it again in the Issues settings, or choose another"
	case errors.Is(err, trackerapp.ErrNoProvider):
		return "tracker_connection_tempo", "a Tempo connection holds no projects, boards or tickets; choose its Jira connection instead"
	case errors.Is(err, trackerapp.ErrNotListed):
		return "tracker_board_not_listed", "this project does not list that board; add it to the project's boards, or choose one it lists"
	case errors.Is(err, trackerapp.ErrNotMirrored):
		return "tracker_board_not_read", "this computer has not read this board from the tracker yet; it shows once the first read finishes"
	case errors.Is(err, trackerapp.ErrStalePage):
		return "tracker_page_stale", "the board, or the project's status groups, changed since this section was read; read the section again from its start"
	case errors.Is(err, trackerapp.ErrProjectNotFound):
		return "tracker_project_missing", "this project no longer exists on this computer"
	case errors.Is(err, trackerapp.ErrBoardsChanged):
		return "tracker_boards_changed", "this change was not saved: the project's boards changed after this list was read"
	case errors.Is(err, trackerapp.ErrInvalidBoards):
		return "tracker_boards_refused", "this board list cannot be saved; fix what the detail names and save again"
	case errors.As(err, &unreadable):
		return "tracker_setup_unreadable", "this project's tracker setup cannot be read from " + unreadable.Path + " on the computer that holds the project (" +
			unreadable.Reason + "); fix the file or remove it, and it is read again at its next use"
	case errors.As(err, &unsureWrite):
		return "tracker_setup_unsure", "this change may or may not be stored: the computer that holds the project could not finish updating " +
			unsureWrite.Path + " (" + unsureWrite.Err.Error() + "); read the project's tracker setup again before you change it again"
	case errors.Is(err, trackerapp.ErrSetupNewer):
		return "tracker_setup_newer", "this part of the setup was saved by a newer version of Agent Overflow; update this computer to change it"
	case errors.As(err, &refusedSetup):
		return "tracker_setup_refused", "this setup cannot be saved: " + refusedSetup.Reason
	case errors.Is(err, trackerapp.ErrUnverified):
		return "tracker_connection_unverified", "this connection has no verified account, so whose time it is is not known; check the connection in the Issues settings"
	case errors.Is(err, trackerapp.ErrTimeEntryStale):
		return "tracker_time_entry_stale", "this time entry changed, was posted or was removed since it was read; read the day again"
	case errors.Is(err, trackerapp.ErrUnknownAttachment):
		return "tracker_attachment_unknown", "this ticket no longer shows that attachment; look at the ticket again"
	}
	return "", ""
}

// refusedInput is what an input refusal says was wrong: its text after
// the sentinel it wraps, which the conversion and the providers write and
// which quotes only the request.
func refusedInput(err error) string {
	text := err.Error()
	for _, sentinel := range []error{trackerapp.ErrInvalidRequest, tracker.ErrInvalidInput} {
		if i := strings.LastIndex(text, sentinel.Error()+": "); i >= 0 {
			return text[i+len(sentinel.Error())+2:]
		}
	}
	return text
}
