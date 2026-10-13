package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflowtools"
	"github.com/google/uuid"
)

// workflowMCPEndpoint registers a thread's session and returns the loopback
// URL its provider would be configured with.
func workflowMCPEndpoint(t *testing.T, a *App, thread store.Thread, token string) string {
	t.Helper()
	servers, err := a.workflowMCPConfigForThread(thread, token)
	if err != nil {
		t.Fatalf("workflowMCPConfigForThread: %v", err)
	}
	entry, ok := servers[workflowMCPName].(map[string]any)
	if !ok {
		t.Fatalf("workflow tools were not registered: %#v", servers)
	}
	url, _ := entry["url"].(string)
	if url == "" {
		t.Fatalf("workflow tools entry carries no url: %#v", entry)
	}
	return url
}

// workflowMCPToolNames lists the tools a session sees, over the real transport.
func workflowMCPToolNames(t *testing.T, endpoint string) []string {
	t.Helper()
	status, reply := remoteMCPRequest(t, endpoint, "tools/list", map[string]any{})
	if status != http.StatusOK || reply["error"] != nil {
		t.Fatalf("tools/list: %d %s", status, reply)
	}
	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(reply["result"], &listed); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// workflowPhaseThread wires a workflow phase thread the way the engine does:
// a run whose frozen snapshot declares the phase's grants, a phase row naming
// the thread, and the runtime mode its access maps to.
func workflowPhaseThread(t *testing.T, a *App, access def.Access, grants ...string) (store.Thread, string) {
	t.Helper()
	return workflowPhaseThreadInMode(t, a, access, workflowPhaseRuntimeMode(access), grants...)
}

// workflowPhaseThreadInMode is workflowPhaseThread with the session's runtime
// mode chosen apart from the phase's declared access, as a user changing the
// mode on a live phase thread leaves it.
func workflowPhaseThreadInMode(t *testing.T, a *App, access def.Access, mode provider.RuntimeMode, grants ...string) (store.Thread, string) {
	t.Helper()
	project, err := a.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "phase", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(engine.Snapshot{Workflow: def.Workflow{
		ID: "wf", Phases: []def.Phase{{ID: "build", Driver: def.DriverAgent, Access: access, Grants: grants}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	item := store.WorkItem{
		ID: uuid.NewString(), ProjectID: project.ID, Goal: "g", WorkflowID: "wf", WorkflowScope: "project",
		Snapshot: snapshot, State: string(engine.StateRunning), Source: "manual", CreatedAt: 1,
	}
	if err := a.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	thread := store.Thread{
		ID: uuid.NewString(), ProjectID: project.ID, Provider: string(provider.Claude),
		Mode: threadmode.ModeWorkflow, WorkspacePath: project.Path,
		RuntimeMode: string(mode),
	}
	if err := a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := a.store.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: item.ID, PhaseID: "build", Attempt: 1, ThreadID: thread.ID, Status: "running", StartedAt: 10,
	}); err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	a.sessionManager().put(thread.ID, session{Token: token, Provider: thread.Provider})
	return thread, token
}

// Every project session on the three providers gets the server; a thread with
// no project has nothing to run workflows in and gets none.
func TestWorkflowMCPRegistersForEveryProjectSession(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	t.Cleanup(func() { _ = app.workflowMCPServer().Close() })

	for _, name := range []string{string(provider.Claude), string(provider.ClaudeTUI), string(provider.Codex)} {
		thread, token := remoteMCPThread(t, app, name)
		endpoint := workflowMCPEndpoint(t, app, thread, token)
		if got := workflowMCPToolNames(t, endpoint); !slices.Equal(got, workflowtools.ToolNames()) {
			t.Fatalf("%s interactive session sees %v, want every tool", name, got)
		}
	}

	projectless := store.Thread{ID: uuid.NewString(), Provider: string(provider.Claude), WorkspacePath: t.TempDir()}
	if err := app.store.CreateThread(projectless); err != nil {
		t.Fatal(err)
	}
	servers, err := app.workflowMCPConfigForThread(projectless, uuid.NewString())
	if err != nil || len(servers) != 0 {
		t.Fatalf("a projectless thread got %#v, %v", servers, err)
	}
	if row := findServer(app.withWorkflowMCPRow(projectless, nil, true), workflowMCPName); row.Name != "" {
		t.Fatalf("a projectless thread lists the workflow tools row: %#v", row)
	}
}

// A phase session sees exactly the tools its frozen grants and its access
// admit, over the real transport, and a call outside them is refused by name.
func TestWorkflowMCPPhaseSessionSeesItsGrants(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	t.Cleanup(func() { _ = app.workflowMCPServer().Close() })

	readOnly, token := workflowPhaseThread(t, app, def.AccessReadOnly, string(def.GrantStartRun))
	endpoint := workflowMCPEndpoint(t, app, readOnly, token)
	want := []string{
		"workflow_list", "workflow_validate", "workflow_schema",
		"run_start", "run_status", "run_watch", "run_output", "run_inspect", "run_narrative",
		"run_control", "run_amend", "run_guide", "memory_list",
	}
	if got := workflowMCPToolNames(t, endpoint); !slices.Equal(got, want) {
		t.Fatalf("read-only start-run phase sees %v\nwant %v", got, want)
	}
	refused := string(remoteMCPCall(t, endpoint, "memory_add", map[string]any{"kind": "learning", "text": "x"}, true))
	if !strings.Contains(refused, "read-only") {
		t.Fatalf("memory_add from a read-only phase = %s", refused)
	}
	refused = string(remoteMCPCall(t, endpoint, "run_resolve", map[string]any{"run_id": "r", "decision": "answer", "answer": "x"}, true))
	if !strings.Contains(refused, `"resolve"`) {
		t.Fatalf("run_resolve without the grant = %s", refused)
	}

	writing, token := workflowPhaseThread(t, app, def.AccessWrite)
	endpoint = workflowMCPEndpoint(t, app, writing, token)
	if got := workflowMCPToolNames(t, endpoint); !slices.Contains(got, "memory_add") || !slices.Contains(got, "workflow_new") || slices.Contains(got, "run_start") {
		t.Fatalf("write phase with no grants sees %v", got)
	}
}

// A phase's access is the one its definition froze, whatever runtime mode its
// session is in: a write phase holds the writing tools in every mode, and a
// read-only phase is refused them in every mode, full access included.
func TestWorkflowMCPReadOnlyFollowsTheFrozenAccessNotTheRuntimeMode(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	t.Cleanup(func() { _ = app.workflowMCPServer().Close() })
	modes := []provider.RuntimeMode{
		provider.RuntimeReadOnly, provider.RuntimeApprovalRequired, provider.RuntimeAutoAcceptEdits,
		provider.RuntimeAuto, provider.RuntimeFullAccess,
	}
	for _, access := range []def.Access{def.AccessWrite, def.AccessReadOnly} {
		for _, mode := range modes {
			thread, token := workflowPhaseThreadInMode(t, app, access, mode)
			endpoint := workflowMCPEndpoint(t, app, thread, token)
			tools := workflowMCPToolNames(t, endpoint)
			writes := access == def.AccessWrite
			for _, name := range []string{"memory_add", "workflow_new"} {
				if slices.Contains(tools, name) != writes {
					t.Errorf("%s phase in %s mode: %s listed = %t", access, mode, name, !writes)
				}
			}
			answer := string(remoteMCPCall(t, endpoint, "memory_add", map[string]any{"kind": "learning", "text": "x"}, !writes))
			if refused := strings.Contains(answer, workflowtools.CodeReadOnly); refused == writes {
				t.Errorf("%s phase in %s mode: memory_add = %s", access, mode, answer)
			}
		}
	}
}

// A capability URL acts only for the session it was issued to: once that
// session is gone, the call is refused rather than run with stale authority.
func TestWorkflowMCPRefusesAnEndedSession(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	t.Cleanup(func() { _ = app.workflowMCPServer().Close() })
	thread, token := remoteMCPThread(t, app, string(provider.Claude))
	endpoint := workflowMCPEndpoint(t, app, thread, token)
	app.sessionManager().put(thread.ID, session{Token: uuid.NewString(), Provider: thread.Provider})
	refused := string(remoteMCPCall(t, endpoint, "workflow_list", map[string]any{}, true))
	if !strings.Contains(refused, "no longer active") {
		t.Fatalf("a replaced session's URL answered: %s", refused)
	}
}

// The claude-tui leg, at the app level: a claude-tui session is launched with
// the workflow tools and none of the app's other servers, with the allowlist
// entry, and the URL in its argv starts a run and waits for it through the
// real transport. No mock speaks claude-tui, so the terminal binary reading
// those flags is the manual provider smoke's to prove.
func TestClaudeTUISessionStartsAndWatchesARunThroughTheWorkflowTools(t *testing.T) {
	t.Parallel()
	fixture := newCLIFixture(t)
	app := fixture.app
	t.Cleanup(func() { _ = app.workflowMCPServer().Close() })
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeArgvRecordingClaude(t, argvPath)}); err != nil {
		t.Fatal(err)
	}
	thread, err := app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID: fixture.project.ID, Provider: string(provider.ClaudeTUI), Model: "claude-opus-4-7",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = app.StopSession(thread.ID) })

	// What the terminal binary was launched with: the workflow tools alone,
	// with their call ceiling, and their allowlist entry.
	argv := spawnedClaudeArgv(t, argvPath)
	if !hasArgPair(argv, "--allowedTools", WorkflowToolsAllowedTool) {
		t.Fatalf("argv has no `--allowedTools %s`: %v", WorkflowToolsAllowedTool, argv)
	}
	raw, ok := argValue(argv, "--mcp-config")
	if !ok {
		t.Fatalf("argv has no --mcp-config: %v", argv)
	}
	var config struct {
		MCPServers map[string]struct {
			URL     string `json:"url"`
			Timeout int64  `json:"timeout"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatalf("decode --mcp-config %s: %v", raw, err)
	}
	entry, ok := config.MCPServers[workflowMCPName]
	if len(config.MCPServers) != 1 || !ok {
		t.Fatalf("claude-tui was handed %s", raw)
	}
	if entry.Timeout != threadMCPCallCeiling.Milliseconds() {
		t.Fatalf("claude-tui entry has no call ceiling: %s", raw)
	}

	// The URL it was handed starts a run, binds it to this thread, and
	// watches it to rest.
	answer := remoteMCPCall(t, entry.URL, "run_start", map[string]any{
		"workflow_id": "tool-flow", "goal": "from the terminal", "wait_seconds": 10,
	}, false)
	var started struct {
		Start struct {
			ItemID        string `json:"itemId"`
			BoundThreadID string `json:"boundThreadId"`
		} `json:"start"`
		Outcome string `json:"outcome"`
		Run     struct {
			State   string `json:"state"`
			Resting bool   `json:"resting"`
		} `json:"run"`
	}
	if err := json.Unmarshal(answer, &started); err != nil {
		t.Fatalf("decode %s: %v", answer, err)
	}
	if started.Start.ItemID == "" || started.Start.BoundThreadID != thread.ID {
		t.Fatalf("start = %s", answer)
	}
	if started.Outcome != workflowtools.OutcomeResting || started.Run.State != string(engine.StateDone) || !started.Run.Resting {
		t.Fatalf("the wait did not see the run rest done: %s", answer)
	}
	// The wait returned the rest, so the rest does not also wake the thread:
	// nothing for this run reached the thread's queue or its sends.
	app.workflowApplication().WaitWake()
	rows, err := app.store.ListFlushQueueItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if strings.Contains(row.Message, started.Start.ItemID) {
			t.Fatalf("the rest the wait returned also woke the thread: %q", row.Message)
		}
	}
}

func TestClaudeTUIHostsOnlyTheWorkflowTools(t *testing.T) {
	t.Parallel()
	others := map[string]any{threadMCPName: map[string]any{}, remoteMCPName: map[string]any{}, "ao-browser-tools": map[string]any{}}
	servers, allowed := claudeTUIMCPServers(others)
	if servers != nil || allowed != nil {
		t.Fatalf("claude-tui without workflow tools got %#v %v", servers, allowed)
	}
	others[workflowMCPName] = map[string]any{"url": "http://workflows"}
	servers, allowed = claudeTUIMCPServers(others)
	if len(servers) != 1 || servers[workflowMCPName] == nil || !slices.Equal(allowed, []string{WorkflowToolsAllowedTool}) {
		t.Fatalf("claude-tui is handed %#v with allowlist %v", servers, allowed)
	}
}

// The Claude argv carries the allowlist entry, end to end through StartSession.
func TestStartSession_ClaudeAllowsTheWorkflowToolsWithoutAPrompt(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	t.Cleanup(func() { _ = app.workflowMCPServer().Close() })
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	binary := writeArgvRecordingClaude(t, argvPath)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	argv := spawnedClaudeArgv(t, argvPath)
	if !hasArgPair(argv, "--allowedTools", WorkflowToolsAllowedTool) {
		t.Fatalf("argv has no `--allowedTools %s`: %v", WorkflowToolsAllowedTool, argv)
	}
	if !slices.ContainsFunc(argv, func(arg string) bool { return strings.Contains(arg, `"`+workflowMCPName+`"`) }) {
		t.Fatalf("argv --mcp-config does not name %s: %v", workflowMCPName, argv)
	}
}

// The Codex half: the entry reaches thread/start approved and with the call
// ceiling, and the guide rides developer instructions.
func TestStartSession_CodexCarriesTheWorkflowToolsEntryAndGuide(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	t.Cleanup(func() { _ = app.workflowMCPServer().Close() })
	thread, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5.5", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "requests.log")
	binary := writeCodexRequestLogBinary(t, logPath)
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	params := waitForCodexRequest(t, logPath, "thread/start")
	config, _ := params["config"].(map[string]any)
	servers, _ := config["mcp_servers"].(map[string]any)
	entry, _ := servers[workflowMCPName].(map[string]any)
	if entry == nil || entry["default_tools_approval_mode"] != "approve" || entry["tool_timeout_sec"] == nil {
		t.Fatalf("codex workflow tools entry = %#v", entry)
	}
	instructions, _ := params["developerInstructions"].(string)
	if !strings.Contains(instructions, "These tools start, watch and steer Agent Overflow workflow runs") {
		t.Fatalf("developer instructions carry no workflow tools guide: %q", instructions)
	}
}

// The MCP menu shows the server without a toggle, and the toggle path refuses.
func TestWorkflowMCPRowHasNoToggle(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread, _ := remoteMCPThread(t, app, string(provider.Claude))
	row := findServer(app.withWorkflowMCPRow(thread, nil, true), workflowMCPName)
	if row.Name != workflowMCPName || row.ToggleDisabledReason == "" || row.Disabled {
		t.Fatalf("row = %#v", row)
	}
	if err := app.SetThreadMcpServerEnabled(thread.ID, workflowMCPName, false); !errors.Is(err, errWorkflowToolsAlwaysOn) {
		t.Fatalf("switching the workflow tools off = %v, want %v", err, errWorkflowToolsAlwaysOn)
	}
}

// waitOnRunFromThread calls run_status for a run as an interactive thread
// would, through the app's own adapter.
func waitOnRunFromThread(t *testing.T, h *wakeHarness, threadID, runID string, waitSeconds int) workflowtools.Wait {
	t.Helper()
	ctx := transport.WithCallerScope(t.Context(), transport.CallerScope{
		Kind: transport.ScopeKindInteractive, ThreadID: threadID, ProjectID: defaultTestProjectID,
	})
	args, err := json.Marshal(map[string]any{"run_id": runID, "wait_seconds": waitSeconds})
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.app.workflowToolsServer().Call(ctx, workflowtools.Caller{ThreadID: threadID}, "run_status", args)
	if err != nil {
		t.Fatalf("run_status: %v", err)
	}
	return result.(workflowtools.Wait)
}

// A wait that returns the run's rest is the delivery: the rest does not also
// wake the thread. Here the call reads the rest from the store before the
// engine's event has queued its wake, the order a fast rest takes.
func TestAWaitThatReturnedTheRestSuppressesItsWake(t *testing.T) {
	t.Parallel()
	h := newWakeHarness(t)
	thread := h.chatThread(t, "waiting-thread")
	item := h.run(t, "held-run", engine.StateRunning, "")
	if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.app.store.UpdateWorkItemState(item.ID, string(engine.StateDone), "", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	wait := waitOnRunFromThread(t, h, thread.ID, item.ID, 30)
	if wait.Outcome != workflowtools.OutcomeResting {
		t.Fatalf("wait = %+v", wait)
	}
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateDone,
	})
	h.drain()
	if sends, queued, _, _ := h.snapshot(); len(sends)+len(queued) != 0 {
		t.Fatalf("the rest the wait returned also woke the thread: sends=%q queued=%q", sends, queued)
	}
}

// A wait that ends with the run still working leaves the rest to the wake:
// it arrives once.
func TestABackgroundedWaitLeavesExactlyOneWake(t *testing.T) {
	t.Parallel()
	h := newWakeHarness(t)
	thread := h.chatThread(t, "waiting-thread")
	item := h.run(t, "slow-run", engine.StateRunning, "")
	if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	wait := waitOnRunFromThread(t, h, thread.ID, item.ID, 1)
	if wait.Outcome != workflowtools.OutcomeBackgrounded || wait.Notify == nil || !wait.Notify.Armed {
		t.Fatalf("wait = %+v", wait)
	}
	if err := h.app.store.UpdateWorkItemState(item.ID, string(engine.StateDone), "", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateDone,
	})
	h.drain()
	sends, queued, _, _ := h.snapshot()
	if len(sends)+len(queued) != 1 {
		t.Fatalf("a backgrounded wait's rest delivered sends=%d queued=%d, want one wake", len(sends), len(queued))
	}
}

// failingWriter takes headers, but every body write fails, as a client that
// hung up before the response went out leaves it.
type failingWriter struct {
	header http.Header
	status int
}

func (f *failingWriter) Header() http.Header       { return f.header }
func (f *failingWriter) WriteHeader(status int)    { f.status = status }
func (f *failingWriter) Write([]byte) (int, error) { return 0, errors.New("write: broken pipe") }

// A wait that read the rest delivers it only if its response reached the
// client. One that was never written leaves the rest to wake the thread once;
// one that was written is the delivery. Both response shapes the transport
// writes are covered: plain JSON and the event stream both provider CLIs ask
// for.
func TestARestingAnswerDeliversOnlyOnceWritten(t *testing.T) {
	t.Parallel()
	for _, accept := range []string{"application/json", "application/json, text/event-stream"} {
		for name, test := range map[string]struct {
			writer func() http.ResponseWriter
			wakes  int
		}{
			"never written": {func() http.ResponseWriter { return &failingWriter{header: http.Header{}} }, 1},
			"written":       {func() http.ResponseWriter { return httptest.NewRecorder() }, 0},
		} {
			t.Run(accept+"/"+name, func(t *testing.T) {
				t.Parallel()
				h := newWakeHarness(t)
				t.Cleanup(func() { _ = h.app.workflowMCPServer().Close() })
				thread := h.chatThread(t, "waiting-thread")
				token := uuid.NewString()
				h.app.sessionManager().put(thread.ID, session{Token: token, Provider: thread.Provider})
				item := h.run(t, "held-run", engine.StateRunning, "")
				if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
					t.Fatal(err)
				}
				if err := h.app.store.UpdateWorkItemState(item.ID, string(engine.StateDone), "", time.Now().UnixMilli()); err != nil {
					t.Fatal(err)
				}
				endpoint, err := url.Parse(workflowMCPEndpoint(t, h.app, thread, token))
				if err != nil {
					t.Fatal(err)
				}
				body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_status","arguments":{"run_id":"held-run","wait_seconds":30}}}`
				req := httptest.NewRequest(http.MethodPost, endpoint.Path, strings.NewReader(body))
				req.RemoteAddr = "[::1]:40000"
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", accept)
				writer := test.writer()
				h.app.workflowMCPServer().ServeHTTP(writer, req)
				if recorder, ok := writer.(*httptest.ResponseRecorder); ok {
					// The tool result is JSON text inside the JSON-RPC reply.
					if answer := strings.ReplaceAll(recorder.Body.String(), `\"`, `"`); !strings.Contains(answer, `"resting":true`) {
						t.Fatalf("the written answer does not carry the rest: %s", recorder.Body.String())
					}
				}

				h.app.afterWorkflowStateEvent(engine.StateEvent{
					ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateDone,
				})
				h.drain()
				if sends, queued, _, _ := h.snapshot(); len(sends)+len(queued) != test.wakes {
					t.Fatalf("delivered sends=%d queued=%d wakes, want %d", len(sends), len(queued), test.wakes)
				}
			})
		}
	}
}
