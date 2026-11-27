package app

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"sync"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmcp"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/trackerdash"
	"agent-overflow/internal/trackertools"
)

// ao-tracker-tools: the tickets of this computer's Jira and Linear
// connections for agent sessions (docs/architecture/tracker-backend.md#agent-tools).
// It rides the thread tools' transport and capability URLs. A session has
// it when it belongs to a project, its provider takes it, and the computer
// has a ticket source when the session starts; a source turned on later
// reaches the thread at its next session start, and one turned off later
// refuses each call with trackerapp's sentence. The thread's MCP menu
// follows the same rule: a running session's row is what that session was
// given, a stopped thread's what its next session would get.

const trackerMCPName = trackertools.MCPServerName

// trackerMCPAccess is what one capability URL acts as: the session it was
// issued to, and whether that session was given the time tools, which a
// Jira connection on as it started gives (trackertools.Tools).
type trackerMCPAccess struct {
	ThreadID     string
	SessionToken string
	Time         bool
}

type appTrackerMCP struct {
	once   sync.Once
	server *threadmcp.Server[trackerMCPAccess]
	tools  *trackertools.Server
	// approvals are the tracker writes waiting on the person.
	approvals trackerApprovals
}

// TrackerToolsAllowedTool is the Claude allowlist entry that admits every
// ao-tracker-tools call without a prompt.
const TrackerToolsAllowedTool = "mcp__" + trackerMCPName + "__*"

func (a *App) trackerMCPServer() *threadmcp.Server[trackerMCPAccess] {
	a.trackerMCP.once.Do(func() {
		a.trackerMCP.tools = trackertools.New(trackerToolsAdapter{a}, trackerToolsHost{a})
		a.trackerMCP.server = threadmcp.New(trackerMCPName, trackertools.Instructions, func(access trackerMCPAccess) []map[string]any {
			return a.trackerMCP.tools.Tools(access.Time)
		}, a.callTrackerMCP)
		// Antigravity sessions do not get these tools (SupportsTrackerMCP).
		a.trackerMCP.server.OmitAlwaysLoad()
	})
	return a.trackerMCP.server
}

// trackerToolsFor reports whether a session of thread starting now gets the
// tracker tools: a project thread on a provider that takes them, on a
// computer with a Jira or Linear connection turned on; and whether it
// gets the time tools, which a Jira connection turned on gives. An App
// built without the tracker service (Start builds it before any session)
// has no source. The connection rows are read first: a computer with none
// on decides without resolving the session's scope.
func (a *App) trackerToolsFor(thread store.Thread, scope sessionScope) (given, time bool, err error) {
	if !provider.CapabilitiesForProvider(thread.Provider).SupportsTrackerMCP || a.trackers == nil {
		return false, false, nil
	}
	sources, err := a.trackers.TicketSources()
	if err != nil || len(sources) == 0 {
		return false, false, err
	}
	if _, ok, err := scope(); err != nil || !ok {
		return false, false, err
	}
	return true, slices.ContainsFunc(sources, func(src trackerapp.TicketSource) bool { return src.Kind == store.TrackerKindJira }), nil
}

// trackerMCPConfigForThread registers the thread's session and returns its
// server entry, or none when the thread does not get the tools.
func (a *App) trackerMCPConfigForThread(thread store.Thread, sessionToken string, scope sessionScope) (map[string]any, error) {
	ok, time, err := a.trackerToolsFor(thread, scope)
	if err != nil || !ok {
		return nil, err
	}
	return a.trackerMCPServer().RegisterThread(thread.ID, sessionToken, trackerMCPAccess{ThreadID: thread.ID, SessionToken: sessionToken, Time: time})
}

// callTrackerMCP runs one tool. The session must still be the one the
// capability was issued to.
func (a *App) callTrackerMCP(w http.ResponseWriter, ctx context.Context, req threadmcp.Request, access trackerMCPAccess) {
	live, ok := a.sessionManager().get(access.ThreadID)
	if !ok || live.Token != access.SessionToken {
		threadmcp.WriteToolError(w, req.ID, errorsx.Public("tracker_session_inactive",
			"The agent session is no longer active. Resume the conversation before using the tracker tools.", nil))
		return
	}
	call, err := threadmcp.DecodeToolCall(req.Params)
	if err != nil {
		threadmcp.WriteError(w, req.ID, http.StatusOK, -32602, "invalid tools/call params")
		return
	}
	result, err := a.trackerMCP.tools.Call(ctx, trackertools.Caller{Thread: access.ThreadID, Session: access.SessionToken, Time: access.Time}, call.Name, call.Arguments)
	if err != nil {
		if _, _, public := errorsx.PublicDetails(err); !public {
			log.Printf("tracker tools: %s on %s: %v", call.Name, access.ThreadID, err)
			err = errorsx.Public(trackertools.CodeFailed, "The tracker tools could not complete that call.", err)
		}
		threadmcp.WriteToolError(w, req.ID, err)
		return
	}
	threadmcp.WriteToolJSON(w, req.ID, result)
}

// withTrackerMCPRow shows the tracker tools in a thread's MCP menu: for
// the running session liveSession ("" when none runs), whether that
// session was given them; otherwise, whether its next session would be.
// They follow the computer's connections rather than a per-conversation
// choice, so the row carries no toggle.
func (a *App) withTrackerMCPRow(thread store.Thread, rows []ThreadMCPServer, liveSession string, scope sessionScope) []ThreadMCPServer {
	var has bool
	source := mcpRowSourceConfig
	if liveSession != "" {
		has, source = a.trackerMCPServer().HoldsSession(thread.ID, liveSession), mcpRowSourceSession
	} else {
		var err error
		if has, _, err = a.trackerToolsFor(thread, scope); err != nil {
			log.Printf("tracker tools: menu row for %s: %v", thread.ID, err)
		}
	}
	if !has {
		return slices.DeleteFunc(rows, func(row ThreadMCPServer) bool { return row.Name == trackerMCPName })
	}
	for i := range rows {
		if rows[i].Name != trackerMCPName {
			continue
		}
		rows[i].Source = source
		rows[i].Disabled = false
		rows[i].ToggleDisabledReason = trackerToolsAlwaysOn
		return rows
	}
	return append(rows, ThreadMCPServer{
		Provider: thread.Provider, Name: trackerMCPName, Status: string(mcpstatus.StatusNotStarted),
		Source: source, ToggleDisabledReason: trackerToolsAlwaysOn,
	})
}

const trackerToolsAlwaysOn = "Tracker tools are part of every project session while a Jira or Linear connection is on."

// errTrackerToolsAlwaysOn refuses switching the tracker tools off a thread.
var errTrackerToolsAlwaysOn = errors.New("set thread mcp server enabled: tracker tools are part of every project session while a Jira or Linear connection is on")

// trackerToolsAdapter implements trackertools.Tracker over the tracker
// service. Each tracker read is bounded as the bound method making the
// same read is (trackerFormRead), and answers that method's refusal
// sentences; a detail read, which no bound method makes, has the default
// method budget.
type trackerToolsAdapter struct{ a *App }

func (t trackerToolsAdapter) TicketSources() ([]trackerapp.TicketSource, error) {
	svc, err := t.a.trackerService()
	if err != nil {
		return nil, err
	}
	return svc.TicketSources()
}

func (t trackerToolsAdapter) LinkedTicket(threadID string) (trackerapp.LinkedTicket, bool, error) {
	svc, err := t.a.trackerService()
	if err != nil {
		return trackerapp.LinkedTicket{}, false, err
	}
	return svc.LinkedTicket(threadID)
}

func (t trackerToolsAdapter) Lookup(ctx context.Context, connection string, keys []string) (trackerapp.LookupView, error) {
	return t.a.TrackerLookup(ctx, connection, keys)
}

func (t trackerToolsAdapter) Search(ctx context.Context, connection string, in trackerapp.SearchRequest) (trackerapp.SearchPageView, error) {
	return t.a.TrackerSearch(ctx, connection, in)
}

func (t trackerToolsAdapter) Comments(ctx context.Context, connection, issue, page string) (trackerapp.CommentPageView, error) {
	return t.a.TrackerComments(ctx, connection, issue, page)
}

func (t trackerToolsAdapter) IssueDetail(ctx context.Context, connection, issue string) (trackerapp.DetailView, error) {
	return trackerFormRead(t.a, ctx, "ticket_read", "Reading the ticket", func(ctx context.Context, svc *trackerapp.Service) (trackerapp.DetailView, error) {
		d, _, err := svc.UnmarkedIssueDetail(ctx, connection, tracker.IssueID(issue), trackerapp.Interactive)
		return d, err
	})
}

func (t trackerToolsAdapter) IssueDetails(ctx context.Context, connection string, issues []string) ([]trackerapp.TicketRead, error) {
	return trackerFormRead(t.a, ctx, "ticket_read", "Reading the tickets", func(ctx context.Context, svc *trackerapp.Service) ([]trackerapp.TicketRead, error) {
		return svc.UnmarkedIssueDetails(ctx, connection, issueIDsOf(issues), trackerapp.Interactive)
	})
}

func (t trackerToolsAdapter) IssueCards(ctx context.Context, connection string, issues []string) ([]trackerapp.TicketCardRead, error) {
	return trackerFormRead(t.a, ctx, "ticket_read", "Reading the tickets", func(ctx context.Context, svc *trackerapp.Service) ([]trackerapp.TicketCardRead, error) {
		return svc.IssueCards(ctx, connection, issueIDsOf(issues), trackerapp.Interactive)
	})
}

func (t trackerToolsAdapter) IssueRows(ctx context.Context, connection string, rows []trackerapp.IssueRow) ([]trackerapp.TicketCardRead, error) {
	return trackerFormRead(t.a, ctx, "ticket_read", "Reading the tickets", func(ctx context.Context, svc *trackerapp.Service) ([]trackerapp.TicketCardRead, error) {
		return svc.IssueRows(ctx, connection, rows, trackerapp.Interactive)
	})
}

// issueIDsOf are ids as issue IDs.
func issueIDsOf(ids []string) []tracker.IssueID {
	out := make([]tracker.IssueID, len(ids))
	for i, id := range ids {
		out[i] = tracker.IssueID(id)
	}
	return out
}

func (t trackerToolsAdapter) TicketsOfKey(key string) ([]trackerapp.KeyedTicket, error) {
	svc, err := t.a.trackerService()
	if err != nil {
		return nil, err
	}
	return svc.TicketsOfKey(key)
}

func (t trackerToolsAdapter) BoardSummary(connection, board string) (trackerapp.BoardSummary, error) {
	svc, err := t.a.trackerService()
	if err != nil {
		return trackerapp.BoardSummary{}, err
	}
	return svc.BoardSummaryOf(connection, board)
}

func (t trackerToolsAdapter) MirrorIssue(connection, issue string) (trackerapp.IssueView, bool, error) {
	svc, err := t.a.trackerService()
	if err != nil {
		return trackerapp.IssueView{}, false, err
	}
	v, err := svc.Issue(connection, tracker.IssueID(issue))
	if errors.Is(err, trackerapp.ErrNotMirrored) {
		return trackerapp.IssueView{}, false, nil
	}
	return v, err == nil, err
}

func (t trackerToolsAdapter) IssueTypes(ctx context.Context, connection, project string) ([]trackerapp.IssueTypeView, error) {
	return t.a.TrackerIssueTypes(ctx, connection, project)
}

// ReadTime is bounded as RefreshTrackerWorklogs, the read it makes when
// the days were not read moments ago.
func (t trackerToolsAdapter) ReadTime(ctx context.Context, connection, from, to string) (trackerapp.WorklogDays, error) {
	return trackerFormRead(t.a, ctx, "RefreshTrackerWorklogs", "Reading the logged time", func(ctx context.Context, svc *trackerapp.Service) (trackerapp.WorklogDays, error) {
		return svc.ReadTime(ctx, connection, from, to)
	})
}

// DraftTime stores the draft, with no tracker call; a refusal of its
// entries is the sentence alone, as the tools word theirs.
func (t trackerToolsAdapter) DraftTime(connection string, in trackerapp.TimeDraftRequest) (trackerapp.TimeDraftView, error) {
	svc, err := t.a.trackerService()
	if err != nil {
		return trackerapp.TimeDraftView{}, err
	}
	out, err := svc.DraftTime(connection, in)
	if errors.Is(err, trackerapp.ErrInvalidRequest) {
		return out, errorsx.Public(trackertools.CodeInvalidRequest, refusedInput(err), err)
	}
	return out, trackerError(err)
}

func (t trackerToolsAdapter) CreateFields(ctx context.Context, connection, project, issueType string) ([]trackerapp.FieldView, error) {
	return t.a.TrackerCreateFields(ctx, connection, project, issueType)
}

// The writes are bounded and worded as the bound methods making them are
// (trackerWrite); ctx carries the agent (trackerapp.ByAgent).

func (t trackerToolsAdapter) Create(ctx context.Context, connection string, in trackerapp.CreateRequest) (trackerapp.WriteView, error) {
	return t.a.CreateTrackerIssue(ctx, connection, in)
}

func (t trackerToolsAdapter) Edit(ctx context.Context, connection string, in trackerapp.EditRequest) (trackerapp.WriteView, error) {
	return t.a.EditTrackerIssue(ctx, connection, in)
}

func (t trackerToolsAdapter) Move(ctx context.Context, connection string, in trackerapp.MoveRequest) (trackerapp.WriteView, error) {
	return t.a.MoveTrackerIssue(ctx, connection, in)
}

func (t trackerToolsAdapter) SetEstimate(ctx context.Context, connection string, in trackerapp.EstimateRequest) (trackerapp.WriteView, error) {
	return t.a.SetTrackerEstimate(ctx, connection, in)
}

func (t trackerToolsAdapter) MoveToSprint(ctx context.Context, connection string, in trackerapp.SprintMoveRequest) (trackerapp.WriteView, error) {
	return t.a.MoveTrackerIssuesToSprint(ctx, connection, in)
}

func (t trackerToolsAdapter) AddComment(ctx context.Context, connection string, in trackerapp.CommentRequest) (trackerapp.WriteView, error) {
	return t.a.AddTrackerComment(ctx, connection, in)
}

// TryWidget counts an agent's widget as the dashboards count, under the
// evaluator's own deadline, and answers its refusal as the dashboard
// methods do (dashboardError).
func (t trackerToolsAdapter) TryWidget(ctx context.Context, project, connection, board string, w trackerapp.KeptWidget, thread string, show bool) (trackerdash.Tried, error) {
	svc, err := t.a.trackerService()
	if err != nil {
		return trackerdash.Tried{}, err
	}
	out, err := svc.TryWidget(ctx, project, trackerapp.Scope{Connection: connection, Board: tracker.BoardID(board)}, w, thread, show)
	return out, dashboardError(err)
}

// EndWrites runs a call's refresh within a write's budget
// (trackerWriteBudget), as each of its writes ran.
func (t trackerToolsAdapter) EndWrites(ctx context.Context) error {
	svc, err := t.a.trackerService()
	if err != nil {
		return err
	}
	budget := trackerWriteBudget
	if t.a.trackerBudget > 0 {
		budget = t.a.trackerBudget
	}
	ctx, cancel := withinBudget(ctx, budgetExceeded{doing: "Reading the changed tickets again", budget: budget, then: trackerSlow})
	defer cancel()
	return svc.EndWrites(ctx)
}

var _ trackertools.Tracker = trackerToolsAdapter{}
