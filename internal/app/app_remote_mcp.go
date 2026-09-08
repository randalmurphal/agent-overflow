package app

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/mcpapp"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmcp"
	"agent-overflow/internal/transport"
)

const remoteMCPName = "ao-remote-tools"

type remoteMCPAccess struct{ ThreadID, SessionToken string }

type appRemoteMCP struct {
	revision atomic.Uint64
	wakeOnce sync.Once
	wake     chan struct{}
	wg       sync.WaitGroup
	once     sync.Once
	server   *threadmcp.Server[remoteMCPAccess]
}

func (a *App) remoteMCPServer() *threadmcp.Server[remoteMCPAccess] {
	a.remoteMCP.once.Do(func() {
		a.remoteMCP.server = threadmcp.New(remoteMCPName, "", a.remoteMCPTools, a.callRemoteMCP)
	})
	return a.remoteMCP.server
}

func (a *App) remoteMCPConfigForThread(thread store.Thread, sessionToken string) (map[string]any, error) {
	if a.backends == nil || (thread.Provider != string(provider.Claude) && thread.Provider != string(provider.Codex)) {
		return nil, nil
	}
	scope, ok, err := a.deriveCallerScope(thread)
	if err != nil {
		return nil, err
	}
	if !ok || (scope.IsPhase() && !scope.HasGrant("remote-commands")) {
		return nil, nil
	}
	return a.remoteMCPServer().RegisterThread(thread.ID, remoteMCPAccess{thread.ID, sessionToken})
}

func (a *App) remoteMCPTools(access remoteMCPAccess) []map[string]any {
	if _, err := a.remoteMCPContext(context.Background(), access.ThreadID); err != nil {
		return nil
	}
	// Pairings, not transient reachability, determine membership. Offline peers
	// still need discovery/errors and accepted jobs still need status/cancel after
	// opt-out. Every start separately checks the destination's current opt-in.
	rows, err := a.ListAgentComputers()
	if err != nil || len(rows) == 0 {
		return nil
	}
	return remoteToolDefinitions
}

func (a *App) remoteMCPContext(ctx context.Context, threadID string) (context.Context, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, err
	}
	if err = a.store.CheckThreadExecutionAccess(thread); err != nil {
		return nil, err
	}
	scope, ok, err := a.deriveCallerScope(thread)
	if err != nil {
		return nil, err
	}
	if !ok || (scope.IsPhase() && !scope.HasGrant("remote-commands")) {
		return nil, errorsx.Public("remote_permission_required", "This conversation does not have permission to use remote commands. Check its workflow grants and enable ao-remote-tools in the MCP menu.", nil)
	}
	return transport.WithCallerScope(ctx, scope), nil
}

func (a *App) callRemoteMCP(w http.ResponseWriter, ctx context.Context, req threadmcp.Request, access remoteMCPAccess) {
	live, ok := a.sessionManager().get(access.ThreadID)
	if !ok || live.Token != access.SessionToken {
		threadmcp.WriteToolError(w, req.ID, errors.New("The agent session is no longer active. Resume the conversation before using remote tools. Accepted remote jobs keep running."))
		return
	}
	ctx, err := a.remoteMCPContext(ctx, access.ThreadID)
	if err != nil {
		threadmcp.WriteToolError(w, req.ID, remoteOperationError("authorize", "", "", err))
		return
	}
	call, err := threadmcp.DecodeToolCall(req.Params)
	if err != nil {
		threadmcp.WriteError(w, req.ID, http.StatusOK, -32602, "invalid tools/call params")
		return
	}
	var result any
	switch call.Name {
	case "remote_computers":
		var args struct{}
		if err = threadmcp.DecodeArgs(call.Arguments, &args); err == nil {
			result, err = a.AgentRemoteComputers(ctx)
		}
	case "remote_run":
		var args struct {
			remoteResultOptions
			ComputerID     string   `json:"computer_id"`
			ProjectID      string   `json:"project_id"`
			WorkspacePath  string   `json:"workspace_path"`
			RequestID      string   `json:"request_id"`
			Argv           []string `json:"argv"`
			TimeoutSeconds int      `json:"timeout_seconds"`
		}
		if err = threadmcp.DecodeArgs(call.Arguments, &args); err == nil {
			if err = args.remoteResultOptions.validate(); err != nil {
				break
			}
			if args.WaitSeconds == nil {
				wait := 1.0
				args.WaitSeconds = &wait
			}
			if args.TimeoutSeconds == 0 {
				args.TimeoutSeconds = 3600
			}
			var command RemoteCommand
			command, err = a.AgentRemoteStart(ctx, AgentRemoteRequest{
				ComputerID: args.ComputerID,
				Workspace:  gitapp.WorkspaceRef{ProjectID: args.ProjectID, WorkspacePath: args.WorkspacePath},
				Request:    remotejobs.Request{ID: args.RequestID, Argv: args.Argv, TimeoutSeconds: args.TimeoutSeconds},
			})
			if err == nil {
				result, err = a.waitRemoteResult(ctx, args.ComputerID, command, args.remoteResultOptions)
			}

		}
	case "remote_status", "remote_cancel":
		var args struct {
			remoteResultOptions
			ComputerID string `json:"computer_id"`
			RequestID  string `json:"request_id"`
		}
		if err = threadmcp.DecodeArgs(call.Arguments, &args); err == nil {
			if err = args.remoteResultOptions.validate(); err != nil {
				break
			}
			var command RemoteCommand
			command, err = a.agentRemoteResult(ctx, args.ComputerID, args.RequestID, call.Name == "remote_cancel")
			if err == nil {
				result, err = a.waitRemoteResult(ctx, args.ComputerID, command, args.remoteResultOptions)
			}
		}
	default:
		threadmcp.WriteError(w, req.ID, http.StatusOK, -32602, "unknown tool")
		return
	}
	if err != nil {
		threadmcp.WriteToolError(w, req.ID, err)
		return
	}
	threadmcp.WriteToolJSON(w, req.ID, result)
}

func remoteTool(name, description string, properties map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	if name != "remote_computers" {
		properties["max_output_bytes"] = map[string]any{"type": "integer", "minimum": 0, "maximum": store.RemoteJobOutputLimit, "default": defaultRemoteOutputBytes, "description": "Maximum output tail in this reply; zero returns metadata only. Increase to inspect more retained output. omittedOutputBytes counts retained bytes omitted from this reply; truncated separately means the destination discarded older log data."}
		waitDefault := 0
		if name == "remote_run" {
			waitDefault = 1
		}
		properties["wait_seconds"] = map[string]any{"type": "number", "minimum": 0, "maximum": 10, "default": waitDefault, "description": "Optionally wait up to this many seconds for completion. A running receipt is successful acceptance, not failure. No automatic cancellation on timeout."}
	}
	return map[string]any{"name": name, "description": description, "inputSchema": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}
}

var remoteToolDefinitions = []map[string]any{
	remoteTool("remote_computers", "List explicitly enabled computers and their registered project IDs and paths. Offline computers return an error; never substitute another destination.", map[string]any{}),
	remoteTool("remote_run", "Run exact argv in a project on an enabled computer. The destination has its own files and environment; no shell interpolation occurs unless argv explicitly invokes a shell. Returns a durable receipt, not necessarily a finished result. Poll remote_status. The job survives client disconnects. Choose request_id BEFORE calling; after a lost reply, query that ID or retry identical arguments with the SAME ID to avoid duplicate execution. Up to four commands per computer; retained output is a 128 KiB tail.", map[string]any{
		"computer_id":     map[string]any{"type": "string", "description": "Destination ID from remote_computers."},
		"project_id":      map[string]any{"type": "string", "description": "Registered project ID on that destination."},
		"workspace_path":  map[string]any{"type": "string", "description": "Optional registered workspace on the destination; defaults to the project checkout."},
		"request_id":      map[string]any{"type": "string", "format": "uuid", "description": "New UUID for this command; reuse for retries."},
		"argv":            map[string]any{"type": "array", "minItems": 1, "maxItems": 256, "description": "Executable plus arguments, at most 64 KiB total. For larger commands, save and invoke a script on the destination.", "items": map[string]any{"type": "string"}},
		"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": remotejobs.MaxTimeoutSeconds, "default": 3600},
	}, "computer_id", "project_id", "request_id", "argv"),
	remoteTool("remote_status", "Read a remote command receipt and retained output. Use the original computer and request IDs after a disconnect or lost reply. Only this conversation's commands are accessible, including after destination opt-out.", map[string]any{"computer_id": map[string]any{"type": "string"}, "request_id": map[string]any{"type": "string", "format": "uuid"}}, "computer_id", "request_id"),
	remoteTool("remote_cancel", "Cancel this conversation's remote command by its original computer and request IDs. Cancellation remains available after destination opt-out.", map[string]any{"computer_id": map[string]any{"type": "string"}, "request_id": map[string]any{"type": "string", "format": "uuid"}}, "computer_id", "request_id"),
}

func (a *App) withRemoteMCPRow(thread store.Thread, rows []ThreadMCPServer, live bool) []ThreadMCPServer {
	if a.backends == nil || (thread.Provider != string(provider.Claude) && thread.Provider != string(provider.Codex)) {
		return rows
	}
	if len(a.remoteMCPTools(remoteMCPAccess{ThreadID: thread.ID})) == 0 {
		return rows
	}
	enabled := a.remoteMCPServer().ThreadEnabled(thread.ID)
	source := mcpRowSourceConfig
	if live {
		source = mcpRowSourceSession
	}
	for i := range rows {
		if rows[i].Name != remoteMCPName {
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
	return append(rows, ThreadMCPServer{Provider: thread.Provider, Name: remoteMCPName, Status: string(status), Disabled: !enabled, Source: source})
}

func (a *App) setRemoteThreadMCPEnabled(thread store.Thread, enabled bool) error {
	if a.backends == nil {
		return errNoBackendProfiles
	}
	server := a.remoteMCPServer()
	previous := server.ThreadEnabled(thread.ID)
	server.SetThreadEnabled(thread.ID, enabled)
	if err := a.mcpService().ApplyManagedServerEnabled(thread.ID, remoteMCPName, enabled); err != nil {
		server.SetThreadEnabled(thread.ID, previous)
		return err
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.Provider(thread.Provider), Name: remoteMCPName})
	if enabled {
		a.signalRemotePeers()
	}
	return nil
}

// Configuration changes refresh tool discovery without injecting instructions,
// polling peers, or restarting a provider turn. Disabled thread tools stay off.
func (a *App) signalRemotePeers() {
	a.remoteMCP.revision.Add(1)
	select {
	case a.remoteMCPWake() <- struct{}{}:
	default:
	}
}
func (a *App) revokeRemoteMCP(threadID, token string) {
	a.remoteMCPServer().RevokeThread(threadID, remoteMCPAccess{threadID, token})
}

// Refreshes are event-driven and coalesced. They never probe peers or restart
// an active turn, and the worker exits before provider/credential teardown.
func (a *App) startRemoteMCPRefresh() {
	if a.backends == nil {
		return
	}
	a.remoteMCP.wg.Add(1)
	go func() {
		defer a.remoteMCP.wg.Done()
		for {
			select {
			case <-a.lifeCtx().Done():
				return
			case <-a.remoteMCPWake():
				for id, live := range a.sessionManager().snapshot() {
					if a.shuttingDown.Load() {
						return
					}
					if !a.remoteMCPServer().HasThread(id) || !a.remoteMCPServer().ThreadEnabled(id) {
						continue
					}
					if live.Claude != nil {
						if err := a.ReconnectMcpServer(id, remoteMCPName); err != nil && a.lifeCtx().Err() == nil {
							a.emitWireErrorToThread(id, "Remote tools could not refresh: "+mcpapp.SanitizeError(err.Error()))
						}
					} else if live.Codex != nil {
						_ = a.mcpService().ApplyManagedServerEnabled(id, remoteMCPName, true)
					}
				}
			}
		}
	}()
}
func (a *App) remoteMCPWake() chan struct{} {
	a.remoteMCP.wakeOnce.Do(func() { a.remoteMCP.wake = make(chan struct{}, 1) })
	return a.remoteMCP.wake
}
