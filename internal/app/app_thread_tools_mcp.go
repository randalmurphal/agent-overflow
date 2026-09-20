package app

import (
	"context"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/mcpapp"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmcp"
	"agent-overflow/internal/threadtools"
)

const threadMCPName = "ao-thread-tools"

// threadToolsDisabledCode is the refusal a call gets while the switch or
// the per-conversation toggle is off. It is a documented code so the
// model reads it as a capability that is off rather than a broken call.
const threadToolsDisabledCode = "thread_tools_disabled"

type threadMCPAccess struct{ ThreadID, SessionToken string }

type appThreadMCP struct {
	once   sync.Once
	server *threadmcp.Server[threadMCPAccess]
	tools  *threadtools.Server
}

// threadMCPCallCeiling is what each provider is told to tolerate for one
// ao-thread-tools call. A wait tops out at threadtools.MaxWaitSeconds and
// the transport keeps the response streaming throughout, so the ceiling
// only has to sit above that with room for the destination round trips.
const threadMCPCallCeiling = 20 * time.Minute

func (a *App) threadMCPServer() *threadmcp.Server[threadMCPAccess] {
	a.threadMCP.once.Do(func() {
		a.threadMCP.tools = threadtools.New(a.threadToolsAdapter())
		server := threadmcp.New(threadMCPName, "", a.threadMCPTools, a.callThreadMCP)
		// The tool list and the guide both follow the shape, which is
		// recomputed per handshake: pairing a computer changes what a
		// running session sees without adding or removing a tool.
		server.SetInstructionsFunc(func(access threadMCPAccess) string {
			return a.threadMCP.tools.Instructions(a.threadToolsShape(access.ThreadID))
		})
		server.SetDisabledError(errorsx.Public(threadToolsDisabledCode,
			"Thread tools are turned off for this conversation. Turn them on in Settings → Agents → Thread tools, or in this conversation's MCP menu.", nil))
		a.threadMCP.server = server
	})
	return a.threadMCP.server
}

func (a *App) threadToolsServer() *threadtools.Server {
	a.threadMCPServer()
	return a.threadMCP.tools
}

// threadToolsShape is what the schemas and the guide are computed from:
// the computers this one is paired with, and the calling thread's own
// live provider settings, which a spawn inherits.
func (a *App) threadToolsShape(threadID string) threadtools.Shape {
	shape := threadtools.Shape{}
	adapter := a.threadToolsAdapter()
	if computers, err := adapter.PairedComputers(context.Background()); err == nil {
		shape.Computers = computers
	}
	thread, err := adapter.Thread(context.Background(), threadID)
	if err != nil {
		return shape
	}
	shape.Defaults = threadtools.SpawnDefaults{
		Provider:    thread.Provider,
		Model:       thread.Model,
		Effort:      thread.Effort,
		Mode:        thread.Mode,
		RuntimeMode: thread.RuntimeMode,
	}
	return shape
}

func (a *App) threadMCPTools(access threadMCPAccess) []map[string]any {
	if !a.threadToolsEnabledFor(access.ThreadID) {
		return nil
	}
	return a.threadToolsServer().Tools(a.threadToolsShape(access.ThreadID))
}

// threadToolsEnabledFor is the effective per-thread answer. The settings
// switch governs this computer's own sessions; an open request from a
// paired computer is the other half, so a switched-off computer still
// answers calls that arrive from elsewhere.
func (a *App) threadToolsEnabledFor(threadID string) bool {
	return a.threadToolsSwitchOn() || a.threadToolsOpenForeign(threadID)
}

func (a *App) threadToolsSwitchOn() bool {
	if a.settings == nil {
		return true
	}
	return a.settings.BackendScreen().Get().ThreadToolsEnabled
}

// threadToolsOpenForeign reports whether this thread must keep serving
// thread tools regardless of the switch.
//
// A thread answering a paired computer's request has the tools on for that
// request's life, so it can reply to what it was asked: the sender's own
// computer said yes by making the request, and this computer's switch
// governs what its own agents may start, not whether they may answer.
func (a *App) threadToolsOpenForeign(threadID string) bool {
	if threadID == "" || a.store == nil {
		return false
	}
	receipts, err := a.store.ListThreadRequestReceiptsForThread(threadID)
	if err != nil {
		log.Printf("thread tools: read receipts for %s: %v", threadID, err)
		return false
	}
	for _, receipt := range receipts {
		if receipt.OwnerDeviceID == threadReceiptLocalOwner {
			continue
		}
		switch receipt.State {
		case store.ThreadReceiptAccepted, store.ThreadReceiptRunning:
			return true
		}
	}
	return false
}

// refreshThreadToolsAdmission re-applies the effective switch to one live
// session. A thread that has just taken a paired computer's request serves
// the tools whether or not this computer's own switch is on, and that
// change has to reach a session that is already running.
func (a *App) refreshThreadToolsAdmission(threadID string) {
	if threadID == "" || a.threadToolsSwitchOn() {
		return
	}
	server := a.threadMCPServer()
	if !server.HasThread(threadID) || !server.ThreadEnabled(threadID) {
		return
	}
	if err := a.mcpService().ApplyManagedServerEnabled(threadID, threadMCPName, a.threadToolsEnabledFor(threadID)); err != nil && a.lifeCtx().Err() == nil {
		log.Printf("thread tools: admit foreign request on %s: %v", threadID, err)
	}
}

// threadMCPConfigForThread registers the calling thread and returns its
// MCP server entry. Every interactive Claude and Codex session gets one
// whether or not the switch is on: the switch is applied to the live
// server, so flipping it reaches a running session without a restart.
// Workflow phase sessions are excluded; they run a scripted step and have
// no business driving other threads.
func (a *App) threadMCPConfigForThread(thread store.Thread, sessionToken string) (map[string]any, error) {
	if thread.Provider != string(provider.Claude) && thread.Provider != string(provider.Codex) {
		return nil, nil
	}
	scope, ok, err := a.deriveCallerScope(thread)
	if err != nil {
		return nil, err
	}
	if !ok || scope.IsPhase() {
		return nil, nil
	}
	if !a.threadMCPServer().ThreadEnabled(thread.ID) {
		return nil, nil
	}
	return a.threadMCPServer().RegisterThread(thread.ID, threadMCPAccess{thread.ID, sessionToken})
}

// threadMCPServerConfig decorates the shared entry with what each provider
// needs to run these tools without prompting the user for every call and
// without failing a parked one. Claude takes a per-server timeout in
// milliseconds and the allowlist through argv; Codex takes seconds and
// approves the server's tools in its own entry.
func threadMCPServerConfig(providerName string, config any) any {
	entry, ok := config.(map[string]any)
	if !ok {
		return config
	}
	out := make(map[string]any, len(entry)+2)
	for key, value := range entry {
		out[key] = value
	}
	switch providerName {
	case string(provider.Claude):
		out["timeout"] = threadMCPCallCeiling.Milliseconds()
	case string(provider.Codex):
		out["tool_timeout_sec"] = int(threadMCPCallCeiling.Seconds())
		// `approve` short-circuits Codex's approval check before the
		// policy or the sandbox is consulted, so these tools work under
		// `never` and under the prompting policies alike.
		out["default_tools_approval_mode"] = "approve"
	}
	return out
}

// ThreadToolsAllowedTool is the Claude allowlist entry that admits every
// ao-thread-tools call without a prompt. One argv value, wildcarded over
// the server's tools so adding a tool needs no second edit.
const ThreadToolsAllowedTool = "mcp__" + threadMCPName + "__*"

func (a *App) revokeThreadMCP(threadID, token string) {
	a.threadMCPServer().RevokeThread(threadID, threadMCPAccess{threadID, token})
}

// callThreadMCP runs one tool. Argument validation, rendering and refusal
// prose all belong to threadtools; this maps the call onto the calling
// thread and turns a public error into a tool error with its code.
func (a *App) callThreadMCP(w http.ResponseWriter, ctx context.Context, req threadmcp.Request, access threadMCPAccess) {
	live, ok := a.sessionManager().get(access.ThreadID)
	if !ok || live.Token != access.SessionToken {
		threadmcp.WriteToolError(w, req.ID, errorsx.Public("thread_session_inactive",
			"The agent session is no longer active. Resume the conversation before using thread tools.", nil))
		return
	}
	if !a.threadToolsEnabledFor(access.ThreadID) {
		threadmcp.WriteToolError(w, req.ID, errorsx.Public(threadToolsDisabledCode,
			"Thread tools are turned off for this conversation.", nil))
		return
	}
	call, err := threadmcp.DecodeToolCall(req.Params)
	if err != nil {
		threadmcp.WriteError(w, req.ID, http.StatusOK, -32602, "invalid tools/call params")
		return
	}
	caller, err := a.threadToolsCaller(access.ThreadID)
	if err != nil {
		threadmcp.WriteToolError(w, req.ID, err)
		return
	}
	// Work a call defers until its answer is written: the inline delivery
	// marks, and the deletion of a scratch thread whose own agent is still
	// inside this call.
	ctx, pending := withThreadToolsPending(ctx)
	result, err := a.threadToolsServer().Call(ctx, caller, call.Name, call.Arguments)
	if err != nil {
		if _, _, public := errorsx.PublicDetails(err); !public {
			log.Printf("thread tools: %s on %s: %v", call.Name, access.ThreadID, err)
			err = errorsx.Public(threadtools.CodeInvalidRequest, "Thread tools could not complete that call on this computer.", err)
		}
		threadmcp.WriteToolError(w, req.ID, err)
		return
	}
	threadmcp.WriteToolJSON(w, req.ID, result)
	pending.run()
}

// threadToolsCaller names the calling thread. Everything a result, a
// footer or an attribution chip says about the sender comes from here.
func (a *App) threadToolsCaller(threadID string) (threadtools.Caller, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return threadtools.Caller{}, errorsx.Public(threadtools.CodeNotFound, "Thread tools could not read the calling thread.", err)
	}
	backendID, _ := a.backendIdentity()
	return threadtools.Caller{
		ThreadID:     thread.ID,
		Title:        thread.Title,
		ComputerID:   backendID,
		ComputerName: a.backendDisplayName(),
	}, nil
}

func (a *App) withThreadMCPRow(thread store.Thread, rows []ThreadMCPServer, live bool) []ThreadMCPServer {
	if thread.Provider != string(provider.Claude) && thread.Provider != string(provider.Codex) {
		return rows
	}
	scope, ok, err := a.deriveCallerScope(thread)
	if err != nil || !ok || scope.IsPhase() {
		return slices.DeleteFunc(rows, func(row ThreadMCPServer) bool { return row.Name == threadMCPName })
	}
	enabled := a.threadMCPServer().ThreadEnabled(thread.ID) && a.threadToolsEnabledFor(thread.ID)
	source := mcpRowSourceConfig
	if live {
		source = mcpRowSourceSession
	}
	for i := range rows {
		if rows[i].Name != threadMCPName {
			continue
		}
		rows[i].Source = source
		if !enabled {
			rows[i].Disabled = true
			rows[i].Status = string(mcpstatus.StatusDisabled)
			rows[i].Tools = nil
		}
		return rows
	}
	status := mcpstatus.StatusNotStarted
	if !enabled {
		status = mcpstatus.StatusDisabled
	}
	return append(rows, ThreadMCPServer{Provider: thread.Provider, Name: threadMCPName, Status: string(status), Disabled: !enabled, Source: source})
}

// setThreadToolsThreadMCPEnabled is the per-conversation toggle in the MCP
// menu. It is ANDed with the settings switch: a conversation the user
// turned off stays off when the switch is on.
func (a *App) setThreadToolsThreadMCPEnabled(thread store.Thread, enabled bool) error {
	server := a.threadMCPServer()
	previous := server.ThreadEnabled(thread.ID)
	server.SetThreadEnabled(thread.ID, enabled)
	if server.HasThread(thread.ID) {
		if err := a.mcpService().ApplyManagedServerEnabled(thread.ID, threadMCPName, enabled && a.threadToolsEnabledFor(thread.ID)); err != nil {
			server.SetThreadEnabled(thread.ID, previous)
			return err
		}
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.Provider(thread.Provider), Name: threadMCPName})
	return nil
}

// setThreadToolsEnabled applies a change to the settings switch to every
// live session. The server-wide flag stays on for the process lifetime:
// the effective answer is per thread, because a paired computer's reach
// keeps a thread open when this computer's own switch is off.
func (a *App) setThreadToolsEnabled(on bool) {
	server := a.threadMCPServer()
	for threadID := range a.sessionManager().snapshot() {
		if !server.HasThread(threadID) {
			continue
		}
		effective := on || a.threadToolsOpenForeign(threadID)
		// The per-conversation toggle is the other half; a conversation
		// the user turned off must not come back on with the switch.
		effective = effective && server.ThreadEnabled(threadID)
		if err := a.mcpService().ApplyManagedServerEnabled(threadID, threadMCPName, effective); err != nil && a.lifeCtx().Err() == nil {
			log.Printf("thread tools: apply switch on %s: %v", threadID, err)
		}
	}
}

// refreshThreadMCPOnWake re-publishes the thread tools on one live
// session after a pairing change. The schemas and the guide are computed
// from the shape, so a computer appearing or leaving changes what a
// running session sees; the tool list reloads in place and no turn is
// restarted.
func (a *App) refreshThreadMCPOnWake(threadID string, live session) {
	server := a.threadMCPServer()
	if !server.HasThread(threadID) || !server.ThreadEnabled(threadID) || !a.threadToolsEnabledFor(threadID) {
		return
	}
	var err error
	switch {
	case live.Claude != nil:
		err = a.ReconnectMcpServer(threadID, threadMCPName)
	case live.Codex != nil:
		err = a.mcpService().ApplyManagedServerEnabled(threadID, threadMCPName, true)
	default:
		return
	}
	if err != nil && a.lifeCtx().Err() == nil {
		a.emitWireErrorToThread(threadID, "Thread tools could not refresh: "+mcpapp.SanitizeError(err.Error()))
	}
}
