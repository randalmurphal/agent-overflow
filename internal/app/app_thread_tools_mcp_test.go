package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// threadMCPEndpoint registers a thread and returns the loopback URL its
// provider would be configured with.
func threadMCPEndpoint(t *testing.T, a *App, thread store.Thread, token string) string {
	t.Helper()
	servers, err := a.threadMCPConfigForThread(thread, token)
	if err != nil {
		t.Fatalf("threadMCPConfigForThread: %v", err)
	}
	entry, ok := servers[threadMCPName].(map[string]any)
	if !ok {
		t.Fatalf("thread tools were not registered: %#v", servers)
	}
	url, _ := entry["url"].(string)
	if url == "" {
		t.Fatalf("thread tools entry carries no url: %#v", entry)
	}
	return url
}

// TestThreadMCPRegistersForInteractiveSessionsAndNotPhases pins who gets
// the server. Every interactive Claude and Codex conversation does;
// a workflow phase runs a scripted step and has no business driving other
// threads, and a provider that cannot host MCP gets nothing.
func TestThreadMCPRegistersForInteractiveSessionsAndNotPhases(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })

	for _, name := range []string{string(provider.Claude), string(provider.Codex)} {
		thread, token := remoteMCPThread(t, app, name)
		servers, err := app.threadMCPConfigForThread(thread, token)
		if err != nil {
			t.Fatalf("%s: threadMCPConfigForThread: %v", name, err)
		}
		if _, ok := servers[threadMCPName]; !ok {
			t.Fatalf("%s interactive session got no thread tools: %#v", name, servers)
		}
	}

	// A workflow phase thread, wired the way the engine wires one.
	project, err := app.store.GetProject(defaultTestProjectID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	snapshot, err := json.Marshal(engine.Snapshot{Workflow: def.Workflow{
		ID: "wf", Phases: []def.Phase{{ID: "build", Driver: def.DriverAgent}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	item := store.WorkItem{
		ID: "tt-item", ProjectID: project.ID, Goal: "g", WorkflowID: "wf", WorkflowScope: "project",
		Snapshot: snapshot, State: string(engine.StateRunning), Source: "manual", CreatedAt: 1,
	}
	if err := app.store.CreateWorkItem(item); err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	phase := store.Thread{
		ID: "tt-phase-thread", ProjectID: project.ID, Provider: string(provider.Claude),
		Mode: threadmode.ModeWorkflow, WorkspacePath: project.Path,
	}
	if err := app.store.CreateThread(phase); err != nil {
		t.Fatalf("CreateThread(phase): %v", err)
	}
	if err := app.store.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: item.ID, PhaseID: "build", Attempt: 1, ThreadID: phase.ID, Status: "running", StartedAt: 10,
	}); err != nil {
		t.Fatalf("CreateWorkItemPhase: %v", err)
	}
	servers, err := app.threadMCPConfigForThread(phase, "phase-token")
	if err != nil {
		t.Fatalf("threadMCPConfigForThread(phase): %v", err)
	}
	if len(servers) != 0 {
		t.Fatalf("a workflow phase session got thread tools: %#v", servers)
	}
	// And the MCP menu does not offer the row either.
	if row := findServer(app.withThreadMCPRow(phase, nil, true), threadMCPName); row.Name != "" {
		t.Fatalf("a workflow phase thread lists the thread tools row: %#v", row)
	}

	// A provider that hosts no MCP servers at all.
	terminal, terminalToken := remoteMCPThread(t, app, string(provider.ClaudeTUI))
	servers, err = app.threadMCPConfigForThread(terminal, terminalToken)
	if err != nil || len(servers) != 0 {
		t.Fatalf("non-MCP provider = %#v, %v", servers, err)
	}
}

// TestThreadMCPProviderEntriesAdmitTheToolsWithoutPrompting pins the
// per-provider decoration. Claude takes a per-server timeout in
// milliseconds (the allowlist rides argv); Codex takes seconds and
// approves the server's tools in its own entry, which is what makes these
// tools work under `never` and under the prompting policies alike.
func TestThreadMCPProviderEntriesAdmitTheToolsWithoutPrompting(t *testing.T) {
	base := map[string]any{"type": "http", "url": "https://127.0.0.1:1/x", "headers": map[string]any{"a": "b"}}

	claudeEntry, ok := threadMCPServerConfig(string(provider.Claude), base).(map[string]any)
	if !ok {
		t.Fatalf("claude entry = %T", threadMCPServerConfig(string(provider.Claude), base))
	}
	if claudeEntry["timeout"] != threadMCPCallCeiling.Milliseconds() {
		t.Errorf("claude timeout = %v, want %d ms", claudeEntry["timeout"], threadMCPCallCeiling.Milliseconds())
	}
	if claudeEntry["url"] != base["url"] {
		t.Errorf("claude entry lost the url: %#v", claudeEntry)
	}

	codexEntry, ok := threadMCPServerConfig(string(provider.Codex), base).(map[string]any)
	if !ok {
		t.Fatalf("codex entry = %T", threadMCPServerConfig(string(provider.Codex), base))
	}
	if codexEntry["default_tools_approval_mode"] != "approve" {
		t.Errorf("codex approval mode = %v, want approve", codexEntry["default_tools_approval_mode"])
	}
	if codexEntry["tool_timeout_sec"] != int(threadMCPCallCeiling.Seconds()) {
		t.Errorf("codex tool_timeout_sec = %v, want %d", codexEntry["tool_timeout_sec"], int(threadMCPCallCeiling.Seconds()))
	}

	// The decoration never mutates the shared registration entry.
	if _, present := base["timeout"]; present {
		t.Error("threadMCPServerConfig mutated the shared entry")
	}

	// The ceiling has to sit above the longest wait a tool can park on,
	// or a parked thread_status would be killed by its own timeout.
	if threadMCPCallCeiling.Seconds() <= float64(threadtools.MaxWaitSeconds) {
		t.Errorf("call ceiling %s is not above the %ds maximum wait", threadMCPCallCeiling, threadtools.MaxWaitSeconds)
	}
	if ThreadToolsAllowedTool != "mcp__ao-thread-tools__*" {
		t.Errorf("allowlist entry = %q", ThreadToolsAllowedTool)
	}
}

// TestStartSession_ClaudeAllowsTheThreadToolsWithoutAPrompt asserts the
// whole chain — registration → Config.AllowedTools → buildArgs → argv — by
// recording the real argv the spawned binary was given.
func TestStartSession_ClaudeAllowsTheThreadToolsWithoutAPrompt(t *testing.T) {
	app, _ := setupE2EApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	binary := filepath.Join(t.TempDir(), "claude-argv.sh")
	script := "#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\" >> " + argvPath + "; done\ncat >/dev/null\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write argv-recording binary: %v", err)
	}
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(argvPath)
		if err == nil {
			argv := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
			for i, arg := range argv {
				if arg == "--allowedTools" && i+1 < len(argv) && argv[i+1] == ThreadToolsAllowedTool {
					return
				}
			}
			if len(argv) > 0 && time.Now().After(deadline) {
				t.Fatalf("argv has no `--allowedTools %s`: %v", ThreadToolsAllowedTool, argv)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the spawned argv (%v)", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// writeCodexRequestLogBinary is a mock codex app-server that appends every
// request to a log and answers the handshake plus thread/start.
func writeCodexRequestLogBinary(t *testing.T, logPath string) string {
	t.Helper()
	script := "#!/bin/bash\n" +
		"while IFS= read -r line; do\n" +
		"  printf '%s\\n' \"$line\" >> '" + logPath + "'\n" +
		"  id=$(printf '%s' \"$line\" | grep -o '\"id\":[0-9]*' | head -1 | grep -o '[0-9]*')\n" +
		"  if [ -z \"$id\" ]; then continue; fi\n" +
		"  if printf '%s' \"$line\" | grep -q '\"method\":\"config/read\"'; then\n" +
		"    printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"config\":{}}}\\n' \"$id\"\n" +
		"  else\n" +
		"    printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"thread\":{\"id\":\"mock-thread\",\"turns\":[]}}}\\n' \"$id\"\n" +
		"  fi\n" +
		"done\n"
	binary := filepath.Join(t.TempDir(), "codex-log.sh")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write codex request-log binary: %v", err)
	}
	return binary
}

// waitForCodexRequest polls the request log for one method's params.
func waitForCodexRequest(t *testing.T, logPath, method string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(logPath)
		if err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				var req struct {
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				if json.Unmarshal([]byte(line), &req) == nil && req.Method == method {
					return req.Params
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s request within 5s (%v)", method, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStartSession_CodexCarriesTheThreadToolsEntryAndGuide asserts the
// Codex half end to end: the mcp_servers entry reaches thread/start with
// the approval key and the timeout, and the decision guide rides
// developerInstructions because Codex has no server-instructions channel.
func TestStartSession_CodexCarriesTheThreadToolsEntryAndGuide(t *testing.T) {
	app, _ := setupE2EApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Codex), workspace, "gpt-5", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": writeCodexRequestLogBinary(t, logPath)}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	params := waitForCodexRequest(t, logPath, "thread/start")
	config, ok := params["config"].(map[string]any)
	if !ok {
		t.Fatalf("thread/start params[config] = %T", params["config"])
	}
	servers, ok := config["mcp_servers"].(map[string]any)
	if !ok {
		t.Fatalf("thread/start config[mcp_servers] = %T", config["mcp_servers"])
	}
	entry, ok := servers[threadMCPName].(map[string]any)
	if !ok {
		t.Fatalf("thread tools absent from the codex entry: %#v", servers)
	}
	if entry["default_tools_approval_mode"] != "approve" {
		t.Errorf("codex entry approval mode = %v, want approve", entry["default_tools_approval_mode"])
	}
	if entry["tool_timeout_sec"] != float64(int(threadMCPCallCeiling.Seconds())) {
		t.Errorf("codex entry tool_timeout_sec = %v", entry["tool_timeout_sec"])
	}

	guide, _ := params["developerInstructions"].(string)
	if guide == "" {
		t.Fatal("thread/start carried no developerInstructions, so the decision guide never reaches Codex")
	}
	if guide != app.threadToolsServer().Instructions(app.threadToolsShape(thread.ID)) {
		t.Errorf("developerInstructions is not the thread tools guide:\n%s", guide)
	}
}

// TestThreadToolsSwitchReachesLiveSessions pins the settings switch: a
// flip walks the live sessions and applies the change through the
// provider's own live-apply mechanism, on both providers.
func TestThreadToolsSwitchReachesLiveSessions(t *testing.T) {
	for _, name := range []string{string(provider.Claude), string(provider.Codex)} {
		t.Run(name, func(t *testing.T) {
			app, _, _ := newMCPTestApp(t)
			ctx, cancel := context.WithCancel(context.Background())
			app.appCtx = ctx
			t.Cleanup(func() { cancel(); _ = app.threadMCPServer().Close() })
			thread, token := remoteMCPThread(t, app, name)
			if _, err := app.threadMCPConfigForThread(thread, token); err != nil {
				t.Fatalf("register: %v", err)
			}
			capture := t.TempDir()
			if name == string(provider.Claude) {
				sess, err := claude.NewSession(ctx, thread.ID, claude.Config{
					Binary: writeClaudeMcpToggleCaptureBinary(t, capture), WorkDir: thread.WorkspacePath,
				}, func(provider.ProviderEvent) {})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = sess.Close() })
				app.sessionManager().put(thread.ID, session{Token: token, Provider: name, Claude: sess})
			} else {
				sess, err := newCodexSessionForThreadTools(t, ctx, thread, capture)
				if err != nil {
					t.Fatal(err)
				}
				app.sessionManager().put(thread.ID, session{Token: token, Provider: name, Codex: sess})
			}

			// The switch is on by default, so turning it off is the change.
			if _, err := app.UpdateSettings(context.Background(), map[string]any{"threadToolsEnabled": false}); err != nil {
				t.Fatalf("UpdateSettings: %v", err)
			}
			if app.threadToolsEnabledFor(thread.ID) {
				t.Fatal("the switch is off but the thread still reports the tools enabled")
			}
			if name == string(provider.Claude) {
				envelope := readClaudeMcpToggleCapture(t, capture, 3*time.Second)
				if envelope.Request["serverName"] != threadMCPName {
					t.Fatalf("toggled server = %v, want %s", envelope.Request["serverName"], threadMCPName)
				}
				if enabled, ok := envelope.Request["enabled"].(bool); !ok || enabled {
					t.Fatalf("toggle enabled = %v, want false", envelope.Request["enabled"])
				}
			} else if got := readCodexReloadCapture(t, capture, 3*time.Second); got != "config/mcpServer/reload" {
				t.Fatalf("codex live apply = %q", got)
			}

			// The listing agrees: the row is present and disabled, which is
			// what the MCP menu draws.
			row := findServer(app.withThreadMCPRow(thread, nil, true), threadMCPName)
			if row.Name == "" || !row.Disabled {
				t.Fatalf("row with the switch off = %#v", row)
			}
		})
	}
}

// TestThreadMCPCallRefusedWhileTheSwitchIsOff pins the refusal a racing
// call gets: the registration stays live so the switch can be flipped
// back, and the call is refused with its own documented code rather than
// looking like a broken tool.
func TestThreadMCPCallRefusedWhileTheSwitchIsOff(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })
	thread, token := remoteMCPThread(t, app, string(provider.Claude))
	endpoint := threadMCPEndpoint(t, app, thread, token)

	if _, err := app.UpdateSettings(context.Background(), map[string]any{"threadToolsEnabled": false}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	raw := remoteMCPCall(t, endpoint, "thread_search", map[string]any{"limit": 1}, true)
	if !strings.Contains(string(raw), threadToolsDisabledCode) {
		t.Fatalf("refusal does not carry %s: %s", threadToolsDisabledCode, raw)
	}
	// The tool list is empty too, so a fresh handshake sees no tools
	// rather than tools that all refuse.
	if tools := app.threadMCPTools(threadMCPAccess{ThreadID: thread.ID}); len(tools) != 0 {
		t.Fatalf("tools listed with the switch off: %d", len(tools))
	}

	// Back on, the same call answers.
	if _, err := app.UpdateSettings(context.Background(), map[string]any{"threadToolsEnabled": true}); err != nil {
		t.Fatalf("UpdateSettings(on): %v", err)
	}
	raw = remoteMCPCall(t, endpoint, "thread_search", map[string]any{"limit": 1}, false)
	if len(raw) == 0 {
		t.Fatal("thread_search answered nothing with the switch back on")
	}
}

// TestThreadMCPPerConversationToggleIsANDedWithTheSwitch pins the second
// half: a conversation the user opted out of stays off when the switch is
// on, and turning the switch on does not re-enable it.
func TestThreadMCPPerConversationToggleIsANDedWithTheSwitch(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })
	thread, token := remoteMCPThread(t, app, string(provider.Claude))

	if err := app.SetThreadMcpServerEnabled(thread.ID, threadMCPName, false); err != nil {
		t.Fatalf("SetThreadMcpServerEnabled: %v", err)
	}
	servers, err := app.threadMCPConfigForThread(thread, token)
	if err != nil || len(servers) != 0 {
		t.Fatalf("an opted-out conversation registered the server: %#v, %v", servers, err)
	}
	row := findServer(app.withThreadMCPRow(thread, nil, false), threadMCPName)
	if row.Name == "" || !row.Disabled {
		t.Fatalf("opted-out row = %#v", row)
	}

	// The settings switch does not overrule the conversation.
	app.setThreadToolsEnabled(true)
	if app.threadMCPServer().ThreadEnabled(thread.ID) {
		t.Fatal("the settings switch re-enabled a conversation the user turned off")
	}

	if err := app.SetThreadMcpServerEnabled(thread.ID, threadMCPName, true); err != nil {
		t.Fatalf("SetThreadMcpServerEnabled(on): %v", err)
	}
	servers, err = app.threadMCPConfigForThread(thread, token)
	if err != nil {
		t.Fatalf("threadMCPConfigForThread: %v", err)
	}
	if _, ok := servers[threadMCPName]; !ok {
		t.Fatalf("re-enabled conversation got no server: %#v", servers)
	}
}

// newCodexSessionForThreadTools starts a mock Codex session whose
// app-server records the live MCP reload the switch drives.
func newCodexSessionForThreadTools(t *testing.T, ctx context.Context, thread store.Thread, capture string) (*codex.Session, error) {
	t.Helper()
	sess, err := codex.NewSession(ctx, thread.ID, codex.Config{
		Binary:  writeCodexRefreshCaptureBinary(t, capture, "thread-tools-provider", ""),
		Model:   "gpt-5",
		WorkDir: thread.WorkspacePath,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess, nil
}
