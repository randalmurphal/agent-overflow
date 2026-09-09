package app

import (
	"context"
	"fmt"
	"net/http"
	"slices"
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
	enabled, err := a.remoteMCPEnabled()
	if err != nil {
		return nil, err
	}
	if !enabled || !a.remoteMCPServer().ThreadEnabled(thread.ID) {
		return nil, nil
	}
	return a.remoteMCPServer().RegisterThread(thread.ID, remoteMCPAccess{thread.ID, sessionToken})
}

func (a *App) remoteMCPTools(access remoteMCPAccess) []map[string]any {
	if _, err := a.remoteMCPContext(context.Background(), access.ThreadID); err != nil {
		return nil
	}
	enabled, err := a.remoteMCPEnabled()
	if err != nil || !enabled {
		return nil
	}
	return remoteToolDefinitions
}

// An explicit destination opt-in controls registration and discovery. A saved
// pairing alone is insufficient; temporary network outages retain the opt-in.
func (a *App) remoteMCPEnabled() (bool, error) {
	rows, err := a.ListAgentComputers()
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.Enabled {
			return true, nil
		}
	}
	return false, nil
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

var remoteToolActions = map[string]string{"remote_computers": "discover", "remote_run": "run", "remote_status": "status", "remote_cancel": "cancel", "remote_fetch_artifact": "fetch artifact", "remote_jobs": "list jobs", "remote_read_log": "read", "remote_search_log": "read"}

// Every tool refusal leaves through remoteOperationError: reviewed public
// codes keep their prose, and any other cause stays in host logs behind a
// reference. Curated argument errors are public by construction.
func (a *App) callRemoteMCP(w http.ResponseWriter, ctx context.Context, req threadmcp.Request, access remoteMCPAccess) {
	live, ok := a.sessionManager().get(access.ThreadID)
	if !ok || live.Token != access.SessionToken {
		threadmcp.WriteToolError(w, req.ID, remoteOperationError("authorize", "", "", errorsx.Public("remote_session_inactive", "The agent session is no longer active. Resume the conversation before using remote tools. Accepted remote jobs keep running.", nil)))
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
	// Every refusal names the computer and request it addressed, so the model
	// can inspect or retry the right job.
	var computerID, requestID string
	decode := func(args any) bool {
		if err = threadmcp.DecodeArgs(call.Arguments, args); err == nil {
			if options, ok := args.(interface{ validate() error }); ok {
				err = options.validate()
			}
		}
		if err != nil {
			err = errorsx.Public("remote_invalid_request", err.Error(), nil)
		}
		return err == nil
	}
	switch call.Name {
	case "remote_computers":
		if decode(&struct{}{}) {
			result, err = a.AgentRemoteComputers(ctx)
		}
	case "remote_run":
		var args struct {
			remoteResultOptions
			ComputerID     string   `json:"computer_id"`
			RequestID      string   `json:"request_id"`
			ProjectID      string   `json:"project_id"`
			WorkspacePath  string   `json:"workspace_path"`
			Label          string   `json:"label"`
			Argv           []string `json:"argv"`
			TimeoutSeconds int      `json:"timeout_seconds"`
			Script         string   `json:"script"`
			Interpreter    []string `json:"interpreter"`
			Unlimited      bool     `json:"unlimited"`
		}
		if decode(&args) {
			computerID, requestID = args.ComputerID, args.RequestID
			if args.WaitSeconds == nil {
				wait := 1.0
				args.WaitSeconds = &wait
			}
			if args.TimeoutSeconds == 0 && !args.Unlimited {
				args.TimeoutSeconds = 3600
			}
			var command RemoteCommand
			command, err = a.AgentRemoteStart(ctx, AgentRemoteRequest{
				ComputerID: args.ComputerID,
				Label:      args.Label,
				Workspace:  gitapp.WorkspaceRef{ProjectID: args.ProjectID, WorkspacePath: args.WorkspacePath},
				Request:    remotejobs.Request{ID: args.RequestID, Argv: args.Argv, TimeoutSeconds: args.TimeoutSeconds, Script: args.Script, Interpreter: args.Interpreter, Unlimited: args.Unlimited},
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
		if decode(&args) {
			computerID, requestID = args.ComputerID, args.RequestID
			var command RemoteCommand
			command, err = a.agentRemoteResult(ctx, args.ComputerID, args.RequestID, call.Name == "remote_cancel")
			if err == nil {
				result, err = a.waitRemoteResult(ctx, args.ComputerID, command, args.remoteResultOptions)
			}
		}
	case "remote_fetch_artifact":
		var args struct {
			ComputerID string `json:"computer_id"`
			RequestID  string `json:"request_id"`
			Path       string `json:"path"`
		}
		if decode(&args) {
			computerID, requestID = args.ComputerID, args.RequestID
			result, err = a.AgentRemoteFetchArtifact(ctx, args.ComputerID, args.RequestID, args.Path)
		}
	case "remote_jobs":
		if decode(&struct{}{}) {
			var watches []store.RemoteWatch
			watches, err = a.ListThreadRemoteCommands(access.ThreadID)
			names := a.remoteComputerNames()
			rows := make([]remoteMCPWatch, 0, len(watches))
			for _, watch := range watches {
				rows = append(rows, remoteMCPWatch{RemoteWatch: watch, ComputerName: names[watch.ComputerID]})
			}
			result = rows
		}
	case "remote_read_log", "remote_search_log":
		var args struct {
			ComputerID string `json:"computer_id"`
			RequestID  string `json:"request_id"`
			Offset     *int64 `json:"offset"`
			MaxBytes   int    `json:"max_bytes"`
			Query      string `json:"query"`
		}
		if decode(&args) {
			computerID, requestID = args.ComputerID, args.RequestID
			if args.MaxBytes == 0 {
				args.MaxBytes = 16384
			}
			offset := int64(-1)
			if args.Offset != nil {
				offset = *args.Offset
			}
			if call.Name == "remote_read_log" {
				result, err = a.ReadThreadRemoteLog(ctx, access.ThreadID, args.ComputerID, args.RequestID, offset, args.MaxBytes)
			} else {
				if args.Offset == nil {
					offset = 0
				}
				var found RemoteLogSearch
				err = a.callThreadRemoteJob(ctx, access.ThreadID, args.ComputerID, args.RequestID, "RemoteCommandSearchLog", &found, args.RequestID, args.Query, offset, args.MaxBytes)
				result = found
			}
		}
	default:
		threadmcp.WriteError(w, req.ID, http.StatusOK, -32602, "unknown tool")
		return
	}
	if err != nil {
		threadmcp.WriteToolError(w, req.ID, remoteOperationError(remoteToolActions[call.Name], computerID, requestID, err))
		return
	}
	threadmcp.WriteToolJSON(w, req.ID, result)
}

func remoteTool(name, description string, properties map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	if name == "remote_run" || name == "remote_status" || name == "remote_cancel" {
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
	remoteTool("remote_computers", "List explicitly enabled computers, execution OS/architecture (including Windows host versus Linux WSL), available executable paths, and registered projects/worktrees. No automatic version or GPU probes. Offline computers return an error; never substitute another destination.", map[string]any{}),
	remoteTool("remote_run", "Execute exact argv or a script in the selected computer's project/workspace. Files and environment belong to that computer: explicitly sync changes and verify the checkout before testing; worktrees are optional. Shell syntax is literal unless you invoke a shell. Choose request_id before calling; after a lost reply, use remote_status or retry identical execution arguments with the SAME ID. Returns a durable running or finished receipt. AO queues a completion notification even if the finished result was returned here, covering lost replies; do not poll just for completion. Use remote_jobs to recover IDs. Keep the process in the foreground: no &, nohup, or shell detachment. AO manages background execution and cancellation, with four active jobs per computer. Jobs survive client disconnects, stop on destination restart, and never automatically rerun. Output is saved; use remote_read_log/remote_search_log for omitted output and remote_fetch_artifact to inspect generated files locally.", map[string]any{
		"computer_id":     map[string]any{"type": "string", "description": "Destination ID from remote_computers."},
		"project_id":      map[string]any{"type": "string", "description": "Registered project ID on that destination."},
		"workspace_path":  map[string]any{"type": "string", "description": "Optional registered workspace on the destination; defaults to the project checkout."},
		"request_id":      map[string]any{"type": "string", "format": "uuid", "description": "New UUID for this command; reuse for retries."},
		"label":           map[string]any{"type": "string", "maxLength": remoteJobLabelMaxRunes, "description": "Optional concise single-line job name, e.g. Windows integration tests. Defaults to the command. Presentation only: retries retain the original label."},
		"argv":            map[string]any{"type": "array", "minItems": 1, "maxItems": 256, "description": "Executable plus arguments, at most 64 KiB total. For larger commands use script with an explicit interpreter.", "items": map[string]any{"type": "string"}},
		"script":          map[string]any{"type": "string", "description": "Alternative to argv: exact script text up to 1 MiB, written privately on the destination and removed after execution. Requires interpreter; no automatic shell selection."},
		"interpreter":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Executable plus interpreter flags, e.g. [bash, -e] or [python3, -u]. AO appends the script file path."},
		"unlimited":       map[string]any{"type": "boolean", "default": false, "description": "Explicitly remove the job time limit for training/services; omit timeout_seconds. Does not survive destination restart."},
		"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": remotejobs.MaxTimeoutSeconds, "default": 3600},
	}, "computer_id", "project_id", "request_id"),
	remoteTool("remote_status", "Read a remote command receipt and retained output. Use the original computer and request IDs after a disconnect or lost reply. Only this conversation's commands are accessible, including after destination opt-out.", map[string]any{"computer_id": map[string]any{"type": "string"}, "request_id": map[string]any{"type": "string", "format": "uuid"}}, "computer_id", "request_id"),
	remoteTool("remote_cancel", "Cancel this conversation's remote command by its original computer and request IDs. Cancellation remains available after destination opt-out.", map[string]any{"computer_id": map[string]any{"type": "string"}, "request_id": map[string]any{"type": "string", "format": "uuid"}}, "computer_id", "request_id"),
	remoteTool("remote_jobs", "List this conversation’s tracked remote jobs, pending first, up to 256 recent jobs, including completed jobs. Recovers original computer/request IDs after context loss. notification=queued means the normal message queue owns delivery/recovery, not proof the agent read it. Receipts reflect the latest observation; use remote_status for a fresh check when needed. Connectivity errors do not mean the destination process stopped.", map[string]any{}),
	remoteTool("remote_read_log", "Read a bounded log range, or its tail (offset=-1, default). Output is retained on the destination up to 4 GiB/job and 20 GiB total. Offsets are absolute output byte offsets; nextOffset resumes reading. startOffset identifies expired/discarded prefix; expired means the log is no longer retained. Never load a large log into context; search or fetch an artifact instead.", remoteLogProperties(false), "computer_id", "request_id"),
	remoteTool("remote_search_log", "Search literal case-sensitive text in a bounded log page, with bounded match context. Continue at nextOffset until done. Search is not a regular expression.", remoteLogProperties(true), "computer_id", "request_id", "query"),
	remoteTool("remote_fetch_artifact", "Copy one file from the job’s original workspace to this computer, returning a local path, size and SHA-256 (no inline bytes). Use for images, HTML, reports and large outputs, then inspect the returned local file with normal tools. Relative path only; symlinks escaping the workspace and special files are refused. Maximum 1 GiB/file. Partial or changing transfers fail without publishing a mixed file. This does not sync checkouts or execute the artifact.", map[string]any{"computer_id": map[string]any{"type": "string"}, "request_id": map[string]any{"type": "string", "format": "uuid"}, "path": map[string]any{"type": "string", "description": "Relative file path within the original job workspace."}}, "computer_id", "request_id", "path"),
}

func (a *App) withRemoteMCPRow(thread store.Thread, rows []ThreadMCPServer, live bool) []ThreadMCPServer {
	if a.backends == nil || (thread.Provider != string(provider.Claude) && thread.Provider != string(provider.Codex)) {
		return rows
	}
	if len(a.remoteMCPTools(remoteMCPAccess{ThreadID: thread.ID})) == 0 {
		return slices.DeleteFunc(rows, func(row ThreadMCPServer) bool { return row.Name == remoteMCPName })
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
	if enabled {
		available, err := a.remoteMCPEnabled()
		if err != nil {
			return err
		}
		if !available {
			return fmt.Errorf("agent remote tools are off: enable a computer in Settings → Remote access → Agent remote tools")
		}
	}
	server := a.remoteMCPServer()
	previous := server.ThreadEnabled(thread.ID)
	server.SetThreadEnabled(thread.ID, enabled)
	if server.HasThread(thread.ID) {
		if err := a.mcpService().ApplyManagedServerEnabled(thread.ID, remoteMCPName, enabled); err != nil {
			server.SetThreadEnabled(thread.ID, previous)
			return err
		}
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
						if err := a.mcpService().ApplyManagedServerEnabled(id, remoteMCPName, true); err != nil && a.lifeCtx().Err() == nil {
							a.emitWireErrorToThread(id, "Remote tools could not refresh: "+mcpapp.SanitizeError(err.Error()))
						}
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

func remoteLogProperties(search bool) map[string]any {
	p := map[string]any{"computer_id": map[string]any{"type": "string"}, "request_id": map[string]any{"type": "string", "format": "uuid"}, "offset": map[string]any{"type": "integer", "minimum": -1}, "max_bytes": map[string]any{"type": "integer", "minimum": 1, "maximum": remotejobs.MaxLogReadBytes, "default": 16384}}
	if search {
		p["query"] = map[string]any{"type": "string", "description": "Literal text to search for."}
	}
	return p
}
