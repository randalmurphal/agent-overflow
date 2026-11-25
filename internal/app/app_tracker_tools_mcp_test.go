package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/trackertools"
	"github.com/google/uuid"
)

// withTicketSource gives a an issue tracker service, when it has none, and
// a Jira connection, turned on, to example.atlassian.net.
func withTicketSource(t *testing.T, a *App) trackerapp.Connection {
	t.Helper()
	if a.trackers == nil {
		if a.appCtx == nil {
			a.appCtx = t.Context()
		}
		if a.version == "" {
			a.version = "1.2.3"
		}
		if a.configDir == "" {
			a.configDir = t.TempDir()
		}
		if err := a.initTrackers(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.trackers.Stop)
	}
	conn, err := a.trackers.Add(trackerapp.NewConnection{Kind: store.TrackerKindJira, Site: "example.atlassian.net", Account: "user1@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// trackerMCPEndpoint registers a thread's session and returns the loopback
// URL its provider would be configured with; "" when the thread does not
// get the tracker tools.
func trackerMCPEndpoint(t *testing.T, a *App, thread store.Thread, token string) string {
	t.Helper()
	servers, err := a.trackerMCPConfigForThread(thread, token)
	if err != nil {
		t.Fatalf("trackerMCPConfigForThread: %v", err)
	}
	if len(servers) == 0 {
		return ""
	}
	entry, _ := servers[trackerMCPName].(map[string]any)
	endpoint, _ := entry["url"].(string)
	if endpoint == "" {
		t.Fatalf("tracker tools entry carries no url: %#v", servers)
	}
	return endpoint
}

// trackerMCPStreamedCall calls one tool as both provider CLIs do: a request
// whose Accept lists text/event-stream and whose envelope carries client
// metadata. It answers the tool result's text and whether it is an error.
func trackerMCPStreamedCall(t *testing.T, a *App, endpoint, name string, args any) (string, bool) {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{
		"name": name, "arguments": args,
		"_meta": map[string]any{"progressToken": 3, "claudecode/toolUseId": "toolu_1", "threadId": "provider-thread"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, u.Path, strings.NewReader(string(body)))
	req.RemoteAddr = "[::1]:40000"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	a.trackerMCPServer().ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("a streamed call answered %q: %s", ct, rec.Body.String())
	}
	var last string
	scanner := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<22)
	for scanner.Scan() {
		if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
			last = data
		}
	}
	var reply struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(last), &reply); err != nil || len(reply.Result.Content) != 1 {
		t.Fatalf("the stream's answer %q: %v", rec.Body.String(), err)
	}
	return reply.Result.Content[0].Text, reply.Result.IsError
}

// The tools reach a project session on Claude or Codex while the computer
// has a Jira or Linear connection on, and nothing else: not claude-tui or
// agy, not a projectless thread, not a computer whose only connection is
// off. A session with none pays nothing for them.
func TestTrackerMCPRegistersOnlyWithATicketSource(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	t.Cleanup(func() { _ = app.trackerMCPServer().Close() })
	claude, token := remoteMCPThread(t, app, string(provider.Claude))
	if endpoint := trackerMCPEndpoint(t, app, claude, token); endpoint != "" {
		t.Fatal("an App without the tracker service registered the tracker tools")
	}
	conn := withTicketSource(t, app)
	for _, name := range []string{string(provider.Claude), string(provider.Codex)} {
		thread, token := remoteMCPThread(t, app, name)
		endpoint := trackerMCPEndpoint(t, app, thread, token)
		if endpoint == "" {
			t.Fatalf("a %s project session has no tracker tools", name)
		}
		if got := workflowMCPToolNames(t, endpoint); !slices.Equal(got, trackertools.ToolNames()) {
			t.Fatalf("%s sees %v", name, got)
		}
	}
	for _, name := range []string{string(provider.ClaudeTUI), string(provider.Agy)} {
		thread, token := remoteMCPThread(t, app, name)
		if endpoint := trackerMCPEndpoint(t, app, thread, token); endpoint != "" {
			t.Fatalf("a %s session got the tracker tools", name)
		}
	}
	projectless := store.Thread{ID: uuid.NewString(), Provider: string(provider.Claude), WorkspacePath: t.TempDir()}
	if err := app.store.CreateThread(projectless); err != nil {
		t.Fatal(err)
	}
	if endpoint := trackerMCPEndpoint(t, app, projectless, uuid.NewString()); endpoint != "" {
		t.Fatal("a projectless thread got the tracker tools")
	}
	if _, err := app.trackers.SetEnabled(conn.ID, false); err != nil {
		t.Fatal(err)
	}
	if endpoint := trackerMCPEndpoint(t, app, claude, token); endpoint != "" {
		t.Fatal("a computer whose only connection is off registered the tracker tools")
	}
	if row := findServer(app.withTrackerMCPRow(claude, nil, true), trackerMCPName); row.Name != "" {
		t.Fatalf("the menu lists tools the session does not get: %#v", row)
	}
}

// Through the real transport, as the CLIs call it: a search and a read of
// the fake Jira site, a refusal the model can act on, and a URL whose
// session ended refused.
func TestTrackerMCPFindsAndReadsThroughTheTransport(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread := store.Thread{ID: uuid.NewString(), ProjectID: f.project, Provider: string(provider.Codex), WorkspacePath: t.TempDir()}
	if err := f.a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	f.a.sessionManager().put(thread.ID, session{Token: token, Provider: thread.Provider})
	endpoint := trackerMCPEndpoint(t, f.a, thread, token)
	if endpoint == "" {
		t.Fatal("the fixture's thread has no tracker tools")
	}

	found, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_find", map[string]any{"text": "story"})
	if isError || !strings.Contains(found, `{"key":"WEB-1","title":"First story","status":`) {
		t.Fatalf("ticket_find = %s (error %v)", found, isError)
	}
	read, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_read", map[string]any{"key": "web-1"})
	if isError || !strings.HasPrefix(read, `{"key":"WEB-1","title":"First story",`) || !strings.Contains(read, `"url":"https://example.atlassian.net/browse/WEB-1"`) {
		t.Fatalf("ticket_read = %s (error %v)", read, isError)
	}
	refused, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_read", map[string]any{})
	if !isError || refused != "[tracker_no_ticket] This thread is not linked to a ticket; name one with key." {
		t.Fatalf("a read with no key on an unlinked thread = %s (error %v)", refused, isError)
	}
	missing, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_read", map[string]any{"key": "WEB-99"})
	if !isError || missing != "[tracker_not_found] No ticket WEB-99 on example.atlassian.net." {
		t.Fatalf("a read of a key no connection has = %s (error %v)", missing, isError)
	}

	f.a.sessionManager().put(thread.ID, session{Token: uuid.NewString(), Provider: thread.Provider})
	if answer, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_find", map[string]any{"text": "story"}); !isError || !strings.Contains(answer, "no longer active") {
		t.Fatalf("a replaced session's URL answered: %s", answer)
	}
}

// The thread's linked ticket is what a read with no key reads.
func TestTrackerMCPReadsTheLinkedTicket(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread := store.Thread{ID: uuid.NewString(), ProjectID: f.project, Provider: string(provider.Claude), WorkspacePath: t.TempDir()}
	if err := f.a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	conn, err := f.a.store.GetTrackerConnection(f.conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	ticket := store.TicketRef{Kind: conn.Kind, Site: conn.Site, IssueID: "10102"}
	if _, err := f.a.trackers.LinkThread(t.Context(), thread.ID, ticket, store.TicketOriginManual); err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	f.a.sessionManager().put(thread.ID, session{Token: token, Provider: thread.Provider})
	endpoint := trackerMCPEndpoint(t, f.a, thread, token)
	read, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_read", map[string]any{})
	if isError || !strings.HasPrefix(read, `{"key":"WEB-2","title":"Second task",`) {
		t.Fatalf("a read with no key = %s (error %v)", read, isError)
	}
}

// The Claude argv carries the server and its allowlist entry, and the app
// guide names it; with no ticket source, neither.
func TestStartSession_ClaudeAllowsTheTrackerToolsWithoutAPrompt(t *testing.T) {
	t.Parallel()
	for _, source := range []bool{false, true} {
		app, _ := setupE2EApp(t)
		t.Cleanup(func() { _ = app.trackerMCPServer().Close() })
		if source {
			withTicketSource(t, app)
		}
		thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
		if err != nil {
			t.Fatalf("CreateThread: %v", err)
		}
		argvPath := filepath.Join(t.TempDir(), "argv.txt")
		if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeArgvRecordingClaude(t, argvPath)}); err != nil {
			t.Fatal(err)
		}
		if err := app.startSessionTakingLock(context.Background(), thread.ID); err != nil {
			t.Fatalf("StartSession: %v", err)
		}
		t.Cleanup(func() { stopSessionChecked(t, app, thread.ID) })
		argv := spawnedClaudeArgv(t, argvPath)
		if got := hasArgPair(argv, "--allowedTools", TrackerToolsAllowedTool); got != source {
			t.Fatalf("with a source %v: argv allows the tracker tools = %v: %v", source, got, argv)
		}
		if got := slices.ContainsFunc(argv, func(arg string) bool { return strings.Contains(arg, `"`+trackerMCPName+`"`) }); got != source {
			t.Fatalf("with a source %v: argv --mcp-config names %s = %v", source, trackerMCPName, got)
		}
		appendPath, found := argValue(argv, "--append-system-prompt-file")
		if !found {
			t.Fatalf("argv has no --append-system-prompt-file: %v", argv)
		}
		content, err := os.ReadFile(appendPath)
		if err != nil {
			t.Fatal(err)
		}
		// Claude reads the server's own guide from the handshake, so the
		// system prompt carries the app guide's sentence alone.
		guide := "`" + trackerMCPName + "` finds and reads tickets in the user's issue tracker."
		if got := strings.Count(string(content), guide); got != map[bool]int{false: 0, true: 1}[source] || strings.Contains(string(content), trackertools.Instructions) {
			t.Fatalf("with a source %v: the system prompt is %q", source, content)
		}
	}
}

// The Codex half: the entry reaches thread/start approved and with the call
// ceiling, and the guide rides developer instructions once.
func TestStartSession_CodexCarriesTheTrackerToolsEntryAndGuide(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	t.Cleanup(func() { _ = app.trackerMCPServer().Close() })
	withTicketSource(t, app)
	thread, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5.5", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "requests.log")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": writeCodexRequestLogBinary(t, logPath)}); err != nil {
		t.Fatal(err)
	}
	if err := app.startSessionTakingLock(context.Background(), thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { stopSessionChecked(t, app, thread.ID) })
	params := waitForCodexRequest(t, logPath, "thread/start")
	config, _ := params["config"].(map[string]any)
	servers, _ := config["mcp_servers"].(map[string]any)
	entry, _ := servers[trackerMCPName].(map[string]any)
	if entry == nil || entry["default_tools_approval_mode"] != "approve" || entry["tool_timeout_sec"] == nil {
		t.Fatalf("codex tracker tools entry = %#v", entry)
	}
	instructions, _ := params["developerInstructions"].(string)
	if strings.Count(instructions, trackertools.Instructions) != 1 {
		t.Fatalf("developer instructions carry the tracker tools guide %d times: %q", strings.Count(instructions, trackertools.Instructions), instructions)
	}
	if !strings.Contains(instructions, "`"+trackerMCPName+"` finds and reads tickets in the user's issue tracker.") {
		t.Fatalf("developer instructions do not name the tracker tools: %q", instructions)
	}
}

// The MCP menu shows the server without a toggle, and the toggle path refuses.
func TestTrackerMCPRowHasNoToggle(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	withTicketSource(t, app)
	thread, _ := remoteMCPThread(t, app, string(provider.Claude))
	row := findServer(app.withTrackerMCPRow(thread, nil, true), trackerMCPName)
	if row.Name != trackerMCPName || row.ToggleDisabledReason == "" || row.Disabled || row.Source != mcpRowSourceSession {
		t.Fatalf("row = %#v", row)
	}
	if err := app.SetThreadMcpServerEnabled(thread.ID, trackerMCPName, false); !errors.Is(err, errTrackerToolsAlwaysOn) {
		t.Fatalf("switching the tracker tools off = %v, want %v", err, errTrackerToolsAlwaysOn)
	}
}
