package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/buildvariant"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/mcpapp"
	"agent-overflow/internal/mcpargs"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmcp"
	"agent-overflow/internal/transport"
)

const remoteMCPName = "ao-remote-tools"

type remoteMCPAccess struct {
	ThreadID, SessionToken string
	MaxWaitSeconds         int
}

func (a remoteMCPAccess) maxWaitSeconds() int {
	if a.MaxWaitSeconds > 0 && a.MaxWaitSeconds < maxRemoteWaitSeconds {
		return a.MaxWaitSeconds
	}
	return maxRemoteWaitSeconds
}

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
	if a.backends == nil || !provider.CapabilitiesForProvider(thread.Provider).SupportsAppManagedMCP {
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
	maxWait := provider.CapabilitiesForProvider(thread.Provider).EffectiveMaxMCPWaitSeconds(maxRemoteWaitSeconds)
	return a.remoteMCPServer().RegisterThread(thread.ID, sessionToken, remoteMCPAccess{
		ThreadID:       thread.ID,
		SessionToken:   sessionToken,
		MaxWaitSeconds: maxWait,
	})
}

func (a *App) remoteMCPTools(access remoteMCPAccess) []map[string]any {
	if _, err := a.remoteMCPContext(context.Background(), access); err != nil {
		return nil
	}
	enabled, err := a.remoteMCPEnabled()
	if err != nil || !enabled {
		return nil
	}
	return remoteToolDefinitionsFor(access.maxWaitSeconds())
}

// An explicit destination opt-in controls registration and discovery. A saved
// pairing alone is insufficient; temporary network outages retain the opt-in.
// A build without remote access never offers the remote tools.
func (a *App) remoteMCPEnabled() (bool, error) {
	if !buildvariant.RemoteAccess {
		return false, nil
	}
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

// remoteMCPContext carries the calling session's scope, naming the session
// (CallerScope.Session), so the work the call starts is that session's
// alone (admitWork).
func (a *App) remoteMCPContext(ctx context.Context, access remoteMCPAccess) (context.Context, error) {
	thread, err := a.store.GetThread(access.ThreadID)
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
	scope.Session = access.SessionToken
	return transport.WithCallerScope(ctx, scope), nil
}

var remoteToolActions = map[string]string{"remote_computers": "discover", "remote_run": "run", "remote_status": "status", "remote_cancel": "cancel", "remote_fetch_artifact": "fetch artifact", "remote_fetch_log": "fetch log", "remote_jobs": "list jobs", "remote_read_log": "read", "remote_search_log": "read"}

// remoteMCPCallCeiling is what each provider is told to tolerate for one
// ao-remote-tools call: the longest wait plus destination round trips. Claude
// otherwise moves a call to its own background task after two minutes and
// aborts a silent one after five; Codex fails one after five minutes. Both
// would hand the agent a second, contradictory account of a job AO already
// tracks. The loopback server's write timeout stays above this value.
const remoteMCPCallCeiling = 20 * time.Minute

// remoteMCPServerConfig decorates the shared server entry with the
// provider's own per-server call limit. Names and units differ per provider;
// the ceiling does not.
func remoteMCPServerConfig(providerName string, config any) any {
	entry, ok := config.(map[string]any)
	if !ok {
		return config
	}
	out := make(map[string]any, len(entry)+1)
	for key, value := range entry {
		out[key] = value
	}
	switch providerName {
	case string(provider.Claude):
		out["timeout"] = remoteMCPCallCeiling.Milliseconds()
	case string(provider.Codex):
		out["tool_timeout_sec"] = int(remoteMCPCallCeiling.Seconds())
		// agy 1.5.16 ACP session/new has no timeoutSeconds override and enforces
		// a hardcoded 180s default; provider.Capabilities.MaxMCPWaitSeconds caps
		// wait_seconds at 150s for agy threads instead.
	}
	return out
}

// withRemoteMCPClaudeEnv defaults Claude Code's threshold for moving an MCP
// call into its own background task above the ceiling, so AO's wait, not
// Claude's task list, owns a parked remote_run. A custom value wins.
func withRemoteMCPClaudeEnv(env provider.EnvInput) provider.EnvInput {
	return env.Default(provider.ClaudeMCPAutoBackgroundEnv, strconv.FormatInt(remoteMCPCallCeiling.Milliseconds(), 10))
}

// Every tool refusal leaves through remoteOperationError: reviewed public
// codes keep their prose, and any other cause stays in host logs behind a
// reference. Curated argument errors are public by construction.
func (a *App) callRemoteMCP(w http.ResponseWriter, ctx context.Context, req threadmcp.Request, access remoteMCPAccess) {
	live, ok := a.sessionManager().get(access.ThreadID)
	if !ok || live.Token != access.SessionToken {
		threadmcp.WriteToolError(w, req.ID, a.remoteOperationError("authorize", "", "", errorsx.Public("remote_session_inactive", "The agent session is no longer active. Resume the conversation before using remote tools. Accepted remote jobs keep running.", nil)))
		return
	}
	ctx, err := a.remoteMCPContext(ctx, access)
	if err != nil {
		threadmcp.WriteToolError(w, req.ID, a.remoteOperationError("authorize", "", "", err))
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
	maxWait := access.maxWaitSeconds()
	decode := func(args any) bool {
		if err = mcpargs.Decode(call.Arguments, args); err == nil {
			if options, ok := args.(interface{ validateWithMaxWait(int) error }); ok {
				err = options.validateWithMaxWait(maxWait)
			}
		}
		if err != nil {
			err = errorsx.Public("remote_invalid_request", err.Error(), nil)
		}
		return err == nil
	}
	// A reply carrying the settled receipt is the delivery. Once the reply
	// has gone out, a delivered one dismisses the watch and an undelivered
	// one leaves it pending for the watcher. The wait stays registered until
	// then, because the watcher leaves a job alone only while a call is parked
	// on it. Delivery is the transport's answer (threadmcp.AfterResponse), not
	// the request context: a client that reads the reply and hangs up at once
	// has received it.
	var settled *remoteMCPResult
	backgrounded := false
	endWait := func() {}
	defer threadmcp.AfterResponse(ctx, func(delivered bool) {
		if settled != nil && delivered {
			if err := a.store.DismissRemoteWatch(computerID, requestID); err != nil {
				log.Printf("remote job %s: dismiss delivered completion: %v", requestID, err)
			} else {
				a.emitBackgroundChanged(access.ThreadID)
			}
		}
		if a.remoteReplySettledForTest != nil {
			a.remoteReplySettledForTest()
		}
		endWait()
		// The wait's end is what moves a running job into the background:
		// the tray row changes state without the job itself changing.
		if backgrounded {
			a.emitBackgroundChanged(access.ThreadID)
		}
	})
	waitFor := func(command RemoteCommand, options remoteResultOptions) {
		var waitCtx context.Context
		waitCtx, endWait = a.beginRemoteWait(ctx, access.ThreadID, computerID, requestID)
		var reply remoteMCPResult
		reply, err = a.waitRemoteResult(ctx, waitCtx, computerID, command, options)
		if err == nil {
			if reply.State != "running" && reply.Notification == "pending" {
				// This reply is the delivery; say so instead of promising a
				// message that its dismissal prevents.
				reply.Notification = "dismissed"
			}
			result = reply
			if reply.State != "running" {
				settled = &reply
			} else {
				backgrounded = true
			}
		}
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
		}
		if decode(&args) {
			computerID, requestID = args.ComputerID, args.RequestID
			if args.WaitSeconds == nil {
				wait := float64(min(defaultRemoteRunWaitSeconds, maxWait))
				args.WaitSeconds = &wait
			}
			var command RemoteCommand
			command, err = a.AgentRemoteStart(ctx, AgentRemoteRequest{
				ComputerID: args.ComputerID,
				Label:      args.Label,
				Workspace:  gitapp.WorkspaceRef{ProjectID: args.ProjectID, WorkspacePath: args.WorkspacePath},
				Request:    remotejobs.Request{ID: args.RequestID, Argv: args.Argv, TimeoutSeconds: args.TimeoutSeconds, Script: args.Script, Interpreter: args.Interpreter},
			})
			if err == nil {
				waitFor(command, args.remoteResultOptions)
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
				waitFor(command, args.remoteResultOptions)
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
	case "remote_fetch_log":
		var args struct {
			ComputerID string `json:"computer_id"`
			RequestID  string `json:"request_id"`
		}
		if decode(&args) {
			computerID, requestID = args.ComputerID, args.RequestID
			result, err = a.AgentRemoteFetchLog(ctx, args.ComputerID, args.RequestID)
		}
	case "remote_jobs":
		if decode(&struct{}{}) {
			result, err = a.ListThreadRemoteCommands(access.ThreadID)
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
		threadmcp.WriteToolError(w, req.ID, a.remoteOperationError(remoteToolActions[call.Name], computerID, requestID, err))
		return
	}
	threadmcp.WriteToolJSON(w, req.ID, result)
}

func remoteToolWithMaxWait(name, description string, maxWait int, properties map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	if maxWait <= 0 || maxWait > maxRemoteWaitSeconds {
		maxWait = maxRemoteWaitSeconds
	}
	if name == "remote_run" || name == "remote_status" || name == "remote_cancel" {
		properties["max_output_bytes"] = map[string]any{"type": "integer", "minimum": 0, "maximum": store.RemoteJobOutputLimit, "default": defaultRemoteOutputBytes, "description": "Max output tail bytes (0 = metadata only)."}
		waitDefault := 0
		waitDescription := "Wait seconds before returning running receipt."
		if name == "remote_run" {
			waitDefault = min(defaultRemoteRunWaitSeconds, maxWait)
			waitDescription = "Wait seconds (0 = return now; running jobs return backgrounded and message on finish)."
		}
		properties["wait_seconds"] = map[string]any{"type": "number", "minimum": 0, "maximum": maxWait, "default": waitDefault, "description": waitDescription}
	}
	return map[string]any{"name": name, "description": description, "inputSchema": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}
}

func remoteToolDefinitionsFor(maxWait int) []map[string]any {
	if maxWait <= 0 || maxWait >= maxRemoteWaitSeconds {
		return remoteToolDefinitions
	}
	return buildRemoteToolDefinitions(maxWait)
}

var remoteToolDefinitions = buildRemoteToolDefinitions(maxRemoteWaitSeconds)

func buildRemoteToolDefinitions(maxWait int) []map[string]any {
	return []map[string]any{
		remoteToolWithMaxWait("remote_computers", "List enabled computers, OS/arch (Windows vs WSL), executables and projects/worktrees. Never substitute offline destinations.", maxWait, map[string]any{}),
		remoteToolWithMaxWait("remote_run", "Run exact argv or script in a remote workspace (sync checkout first; literal argv unless invoking a shell; foreground only, no &/nohup; 4 jobs/computer). Reuse request_id on retry. Backgrounded jobs message on finish; do not poll.", maxWait, map[string]any{
			"computer_id":     map[string]any{"type": "string"},
			"project_id":      map[string]any{"type": "string"},
			"workspace_path":  map[string]any{"type": "string"},
			"request_id":      map[string]any{"type": "string", "format": "uuid", "description": "UUID; reuse on retries."},
			"label":           map[string]any{"type": "string", "maxLength": remoteJobLabelMaxRunes},
			"argv":            map[string]any{"type": "array", "minItems": 1, "maxItems": 256, "description": "Executable + args (<=64 KiB).", "items": map[string]any{"type": "string"}},
			"script":          map[string]any{"type": "string", "description": "Script text (<=1 MiB); requires interpreter."},
			"interpreter":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Interpreter + flags."},
			"timeout_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": remotejobs.MaxTimeoutSeconds, "default": 0},
		}, "computer_id", "project_id", "request_id"),
		remoteToolWithMaxWait("remote_status", "Read receipt and retained output for this conversation's remote job.", maxWait, map[string]any{"computer_id": map[string]any{"type": "string"}, "request_id": map[string]any{"type": "string", "format": "uuid"}}, "computer_id", "request_id"),
		remoteToolWithMaxWait("remote_cancel", "Stop this conversation's remote job (SIGTERM then SIGKILL).", maxWait, map[string]any{"computer_id": map[string]any{"type": "string"}, "request_id": map[string]any{"type": "string", "format": "uuid"}}, "computer_id", "request_id"),
		remoteToolWithMaxWait("remote_jobs", "List tracked remote jobs for this conversation (pending first) to recover IDs/status.", maxWait, map[string]any{}),
		remoteToolWithMaxWait("remote_read_log", "Read a bounded log range or tail (offset=-1). Use remote_search_log/remote_fetch_log for large logs.", maxWait, remoteLogProperties(false), "computer_id", "request_id"),
		remoteToolWithMaxWait("remote_search_log", "Case-sensitive literal search in a log page; continue at nextOffset.", maxWait, remoteLogProperties(true), "computer_id", "request_id", "query"),
		remoteToolWithMaxWait("remote_fetch_log", "Download a finished job's full log locally (returns path, size, SHA-256).", maxWait, map[string]any{"computer_id": map[string]any{"type": "string"}, "request_id": map[string]any{"type": "string", "format": "uuid"}}, "computer_id", "request_id"),
		remoteToolWithMaxWait("remote_fetch_artifact", "Download one relative-path workspace file (<=1 GiB) locally (returns path, size, SHA-256).", maxWait, map[string]any{"computer_id": map[string]any{"type": "string"}, "request_id": map[string]any{"type": "string", "format": "uuid"}, "path": map[string]any{"type": "string", "description": "Relative workspace path."}}, "computer_id", "request_id", "path"),
	}
}

func (a *App) withRemoteMCPRow(thread store.Thread, rows []ThreadMCPServer, live bool) []ThreadMCPServer {
	if a.backends == nil || !provider.CapabilitiesForProvider(thread.Provider).SupportsAppManagedMCP {
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
		rows[i].ToggleDisabledReason = ""
		rows[i].Disabled = !enabled
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
	a.remoteMCPServer().RevokeThread(threadID, token)
	a.cancelRemoteWaits(threadID)
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
					// The thread server's schemas and guide follow the
					// same pairing change, and its registration is
					// independent of the remote server's.
					a.refreshThreadMCPOnWake(id, live)
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
