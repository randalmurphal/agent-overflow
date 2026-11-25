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
	"agent-overflow/internal/trackertools"
)

// ao-tracker-tools: the tickets of this computer's Jira and Linear
// connections for agent sessions (docs/architecture/tracker-backend.md#agent-tools).
// It rides the thread tools' transport and capability URLs. A session has
// it when it belongs to a project, its provider takes it, and the computer
// has a ticket source when the session starts; a source turned on later
// reaches the thread at its next session start, and one turned off later
// refuses each call with trackerapp's sentence.

const trackerMCPName = trackertools.MCPServerName

// trackerMCPAccess is what one capability URL acts as: the session it was
// issued to.
type trackerMCPAccess struct {
	ThreadID     string
	SessionToken string
}

type appTrackerMCP struct {
	once   sync.Once
	server *threadmcp.Server[trackerMCPAccess]
	tools  *trackertools.Server
}

// TrackerToolsAllowedTool is the Claude allowlist entry that admits every
// ao-tracker-tools call without a prompt.
const TrackerToolsAllowedTool = "mcp__" + trackerMCPName + "__*"

func (a *App) trackerMCPServer() *threadmcp.Server[trackerMCPAccess] {
	a.trackerMCP.once.Do(func() {
		a.trackerMCP.tools = trackertools.New(trackerToolsAdapter{a})
		a.trackerMCP.server = threadmcp.New(trackerMCPName, trackertools.Instructions, func(trackerMCPAccess) []map[string]any {
			return a.trackerMCP.tools.Tools()
		}, a.callTrackerMCP)
	})
	return a.trackerMCP.server
}

// trackerToolsFor reports whether thread's sessions get the tracker tools:
// a project thread on a provider that takes them, on a computer with a
// Jira or Linear connection turned on. An App built without the tracker
// service (Start builds it before any session) has no source.
func (a *App) trackerToolsFor(thread store.Thread) (bool, error) {
	if !provider.CapabilitiesForProvider(thread.Provider).SupportsTrackerMCP || a.trackers == nil {
		return false, nil
	}
	if _, ok, err := a.workflowCallerScope(thread); err != nil || !ok {
		return false, err
	}
	sources, err := a.trackers.TicketSources()
	return len(sources) > 0, err
}

// trackerMCPConfigForThread registers the thread's session and returns its
// server entry, or none when the thread does not get the tools.
func (a *App) trackerMCPConfigForThread(thread store.Thread, sessionToken string) (map[string]any, error) {
	ok, err := a.trackerToolsFor(thread)
	if err != nil || !ok {
		return nil, err
	}
	return a.trackerMCPServer().RegisterThread(thread.ID, sessionToken, trackerMCPAccess{ThreadID: thread.ID, SessionToken: sessionToken})
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
	result, err := a.trackerMCP.tools.Call(ctx, access.ThreadID, call.Name, call.Arguments)
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

// withTrackerMCPRow shows the tracker tools in a thread's MCP menu while
// its sessions get them. They follow the computer's connections rather
// than a per-conversation choice, so the row carries no toggle.
func (a *App) withTrackerMCPRow(thread store.Thread, rows []ThreadMCPServer, live bool) []ThreadMCPServer {
	if ok, err := a.trackerToolsFor(thread); err != nil || !ok {
		if err != nil {
			log.Printf("tracker tools: menu row for %s: %v", thread.ID, err)
		}
		return slices.DeleteFunc(rows, func(row ThreadMCPServer) bool { return row.Name == trackerMCPName })
	}
	source := mcpRowSourceConfig
	if live {
		source = mcpRowSourceSession
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
		d, _, err := svc.IssueDetail(ctx, connection, tracker.IssueID(issue), trackerapp.Interactive)
		return d, err
	})
}

var _ trackertools.Tracker = trackerToolsAdapter{}
