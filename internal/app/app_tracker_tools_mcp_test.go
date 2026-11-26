package app

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/trackertools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"github.com/google/uuid"
)

// withTicketSource gives a an issue tracker service, when it has none, and
// a Jira connection, turned on, to example.atlassian.net.
func withTicketSource(t *testing.T, a *App) trackerapp.Connection {
	t.Helper()
	withTrackerService(t, a)
	conn, err := a.trackers.Add(trackerapp.NewConnection{Kind: store.TrackerKindJira, Site: "example.atlassian.net", Account: "user1@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// withTrackerService gives a an issue tracker service with no connection,
// when it has none.
func withTrackerService(t *testing.T, a *App) {
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
}

// trackerMCPEndpoint registers a thread's session and returns the loopback
// URL its provider would be configured with; "" when the thread does not
// get the tracker tools.
func trackerMCPEndpoint(t *testing.T, a *App, thread store.Thread, token string) string {
	t.Helper()
	servers, err := a.trackerMCPConfigForThread(thread, token, a.sessionScopeOf(thread))
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
		if got := workflowMCPToolNames(t, endpoint); !slices.Equal(got, trackertools.ToolNames(true)) {
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
	if row := findServer(app.withTrackerMCPRow(claude, nil, "", app.sessionScopeOf(claude)), trackerMCPName); row.Name != "" {
		t.Fatalf("the menu lists tools the next session does not get: %#v", row)
	}
}

// The menu row of a running session is what that session was given, and a
// stopped thread's row is what its next session would get: a connection
// turned on or off meanwhile changes only the second. A session that was
// never given the tools has no row, so no Reconnect is offered for them.
func TestTrackerMCPRowFollowsTheRunningSession(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	t.Cleanup(func() { _ = app.trackerMCPServer().Close() })
	withTrackerService(t, app)
	without, withoutToken := remoteMCPThread(t, app, string(provider.Claude))
	if trackerMCPEndpoint(t, app, without, withoutToken) != "" {
		t.Fatal("a session started with no connection got the tracker tools")
	}
	conn := withTicketSource(t, app)
	with, withToken := remoteMCPThread(t, app, string(provider.Claude))
	if trackerMCPEndpoint(t, app, with, withToken) == "" {
		t.Fatal("a session started with a connection has no tracker tools")
	}
	row := func(thread store.Thread, liveSession string) ThreadMCPServer {
		return findServer(app.withTrackerMCPRow(thread, nil, liveSession, app.sessionScopeOf(thread)), trackerMCPName)
	}
	if got := row(without, withoutToken); got.Name != "" {
		t.Fatalf("connection turned on: the session started without one lists the tools: %#v", got)
	}
	if got := row(without, ""); got.Name != trackerMCPName || got.Source != mcpRowSourceConfig {
		t.Fatalf("connection turned on: the thread's next session is not listed with them: %#v", got)
	}
	if _, err := app.trackers.SetEnabled(conn.ID, false); err != nil {
		t.Fatal(err)
	}
	if got := row(with, withToken); got.Name != trackerMCPName || got.Source != mcpRowSourceSession {
		t.Fatalf("connection turned off: the session that has the tools does not list them: %#v", got)
	}
	if got := row(with, ""); got.Name != "" {
		t.Fatalf("connection turned off: the thread's next session is listed with the tools: %#v", got)
	}
	app.trackerMCPServer().RevokeThread(with.ID, withToken)
	if got := row(with, withToken); got.Name != "" {
		t.Fatalf("revoked: the session lists tools it no longer holds: %#v", got)
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

// ticket_read by key of a ticket no board, link or open ticket keeps,
// before any board read, names its parent as the read gave it.
func TestTrackerMCPReadNamesTheParentOfAnUnkeptTicket(t *testing.T) {
	t.Parallel()
	site := trackerBoardSite()
	site.Projects = append(site.Projects, trackerfake.JiraProject{ID: "10001", Key: "OPS", Name: "Ops"})
	day := site.Issues[0].Created
	site.Issues = append(site.Issues,
		trackerfake.JiraIssue{ID: "10104", Key: "WEB-4", Project: "WEB", Type: "10000", Summary: "The epic", Status: "10000", Created: day, Updated: day},
		trackerfake.JiraIssue{ID: "10105", Key: "OPS-1", Project: "OPS", Type: "10002", Summary: "An ops task", Parent: "10104", Status: "10000", Created: day, Updated: day})
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: site})
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread := store.Thread{ID: uuid.NewString(), ProjectID: f.project, Provider: string(provider.Codex), WorkspacePath: t.TempDir()}
	if err := f.a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	f.a.sessionManager().put(thread.ID, session{Token: token, Provider: thread.Provider})
	endpoint := trackerMCPEndpoint(t, f.a, thread, token)
	read, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_read", map[string]any{"key": "OPS-1"})
	if isError || !strings.HasPrefix(read, `{"key":"OPS-1","title":"An ops task",`) || !strings.Contains(read, `"parent":"WEB-4 The epic"`) {
		t.Fatalf("ticket_read = %s (error %v)", read, isError)
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
		guide := "`" + trackerMCPName + "` finds, reads, and changes tickets in the user's issue tracker."
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
	if !strings.Contains(instructions, "`"+trackerMCPName+"` finds, reads, and changes tickets in the user's issue tracker.") {
		t.Fatalf("developer instructions do not name the tracker tools: %q", instructions)
	}
}

// The tool list and guide are what every session with the server reads
// before it calls anything, so their size is a decision. This measures them
// as a client receives them through the transport, and fails past the bound
// saying the sizes it found. The session has a Jira connection, so its list
// carries the time tools, 881 of its bytes (268 and 611, and the commas
// that join them); dashboard_widget is 304.
func TestTrackerMCPToolListAndGuideStayWithinTheirBound(t *testing.T) {
	t.Parallel()
	const bound = 3_235
	app := newTestAppWithStore(t)
	t.Cleanup(func() { _ = app.trackerMCPServer().Close() })
	withTicketSource(t, app)
	thread, token := remoteMCPThread(t, app, string(provider.Claude))
	endpoint := trackerMCPEndpoint(t, app, thread, token)
	status, listed := remoteMCPRequest(t, endpoint, "tools/list", map[string]any{})
	if status != http.StatusOK || listed["error"] != nil {
		t.Fatalf("tools/list: %d %s", status, listed)
	}
	if strings.Contains(string(listed["result"]), "_meta") {
		t.Fatalf("tools/list carries a marker for clients that never get this server: %s", listed["result"])
	}
	status, initialized := remoteMCPRequest(t, endpoint, "initialize", map[string]any{})
	if status != http.StatusOK || initialized["error"] != nil {
		t.Fatalf("initialize: %d %s", status, initialized)
	}
	var guide struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(initialized["result"], &guide); err != nil {
		t.Fatal(err)
	}
	if size := len(listed["result"]) + len(guide.Instructions); size > bound {
		t.Fatalf("the tool list (%d bytes) and guide (%d bytes) are %d bytes, over the bound of %d", len(listed["result"]), len(guide.Instructions), size, bound)
	}
	t.Logf("tool list %d bytes, guide %d bytes", len(listed["result"]), len(guide.Instructions))
	// The bytes every session pays for, pinned: a change to them is a
	// change to what each session costs, made on purpose.
	if sum := fmt.Sprintf("%x", sha256.Sum256(append(slices.Clone(listed["result"]), guide.Instructions...))); sum != trackerToolListSum {
		t.Errorf("the tool list and guide changed: sha256 %s, pinned %s", sum, trackerToolListSum)
	}
}

// trackerToolListSum is the sha256 of the tracker tools' tools/list
// result followed by their guide.
const trackerToolListSum = "bb20c17711cb95ef4a4261dfd5dc09d7c3c50add06bf4741380ab4df95b641c0"

// brokenPhaseThread is a workflow phase thread whose session scope cannot
// be resolved: its attempt row names a phase its run's snapshot does not
// have. Any resolution of its scope fails, so a check that resolves it
// shows.
func brokenPhaseThread(t *testing.T, a *App) store.Thread {
	t.Helper()
	project, err := a.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "source", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(engine.Snapshot{Workflow: def.Workflow{ID: "wf", Phases: []def.Phase{{ID: "build", Driver: def.DriverAgent}}}})
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
		ID: uuid.NewString(), ProjectID: project.ID, ProjectPath: project.Path, WorkspacePath: project.Path,
		Provider: string(provider.Claude), Mode: threadmode.ModeWorkflow,
	}
	if err := a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := a.store.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: item.ID, PhaseID: "ghost", Attempt: 1, ThreadID: thread.ID, Status: "running", StartedAt: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.workflowCallerScope(thread); err == nil {
		t.Fatal("the broken phase thread's scope resolved")
	}
	return thread
}

// A computer with no connection on decides that a session gets no tracker
// tools from the connection rows alone: a workflow phase's scope, which
// decodes its run's snapshot, is not read.
func TestTrackerToolsLookForAConnectionBeforeTheSessionScope(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	withTrackerService(t, app)
	phase := brokenPhaseThread(t, app)
	if ok, _, err := app.trackerToolsFor(phase, func() (transport.CallerScope, bool, error) {
		t.Fatal("the session scope was resolved with no connection on")
		return transport.CallerScope{}, false, nil
	}); err != nil || ok {
		t.Fatalf("no connection on: tracker tools = %v, %v; want none, decided without the scope", ok, err)
	}
}

// The MCP menu shows the server without a toggle, and the toggle path refuses.
func TestTrackerMCPRowHasNoToggle(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	withTicketSource(t, app)
	t.Cleanup(func() { _ = app.trackerMCPServer().Close() })
	thread, token := remoteMCPThread(t, app, string(provider.Claude))
	if trackerMCPEndpoint(t, app, thread, token) == "" {
		t.Fatal("the session has no tracker tools")
	}
	row := findServer(app.withTrackerMCPRow(thread, nil, token, app.sessionScopeOf(thread)), trackerMCPName)
	if row.Name != trackerMCPName || row.ToggleDisabledReason == "" || row.Disabled || row.Source != mcpRowSourceSession {
		t.Fatalf("row = %#v", row)
	}
	if err := app.SetThreadMcpServerEnabled(thread.ID, trackerMCPName, false); !errors.Is(err, errTrackerToolsAlwaysOn) {
		t.Fatalf("switching the tracker tools off = %v, want %v", err, errTrackerToolsAlwaysOn)
	}
}

// trackerToolsLinear is a Linear workspace whose team team holds its
// issue 1, "Quartz lantern", assigned to User 2, commented on by User 2
// and moved by User 3: people the app has read nothing of while no board
// of the workspace is listed.
func trackerToolsLinear(team string) trackerfake.LinearWorkspace {
	days := func(n int) time.Time {
		return time.Now().UTC().Truncate(time.Second).Add(time.Duration(n) * 24 * time.Hour)
	}
	lid := func(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", n) }
	return trackerfake.LinearWorkspace{
		URLKey: "example", Name: "Example", APIKey: "lin_api_fake00001", Viewer: lid(1),
		Users: []trackerfake.LinearUser{
			{ID: lid(1), Name: "User 1", Email: "user1@example.com"},
			{ID: lid(2), Name: "User 2", Email: "user2@example.com"},
			{ID: lid(3), Name: "User 3", Email: "user3@example.com"},
		},
		Teams: []trackerfake.LinearTeam{{ID: lid(10), Key: team, Name: "Engineering", Timezone: "UTC", Members: []string{lid(1), lid(2), lid(3)}}},
		States: []trackerfake.LinearState{
			{ID: lid(20), Team: lid(10), Name: "Backlog", Type: "backlog", Position: 0},
			{ID: lid(21), Team: lid(10), Name: "Todo", Type: "unstarted", Position: 1},
		},
		Issues: []trackerfake.LinearIssue{{
			ID: lid(60), Team: lid(10), Number: 1, Title: "Quartz lantern", State: lid(21), Assignee: lid(2), Creator: lid(1),
			CreatedAt: days(-10), UpdatedAt: days(-9),
			History: []trackerfake.LinearHistory{{ID: lid(70), CreatedAt: days(-9), Actor: lid(3), FromState: lid(20), ToState: lid(21)}},
		}},
		Comments: []trackerfake.LinearComment{{ID: lid(80), Issue: lid(60), User: lid(2), Body: "Looks right.", CreatedAt: days(-8), UpdatedAt: days(-8)}},
	}
}

// A Linear search row and read carry the names the tracker has, though
// the app has read no board of the workspace: status and assignee on the
// row; assignee, comment author and history actor on the read.
func TestTrackerMCPNamesLinearPeopleWithAnEmptyMirror(t *testing.T) {
	t.Parallel()
	ws := trackerToolsLinear("ENG")
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: trackerBoardSite(), linear: &ws})
	f.addLinear(t, ws)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread := store.Thread{ID: uuid.NewString(), ProjectID: f.project, Provider: string(provider.Claude), WorkspacePath: t.TempDir()}
	if err := f.a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	f.a.sessionManager().put(thread.ID, session{Token: token, Provider: thread.Provider})
	endpoint := trackerMCPEndpoint(t, f.a, thread, token)

	found, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_find", map[string]any{"text": "Quartz lantern"})
	if isError {
		t.Fatalf("ticket_find: %s", found)
	}
	var page struct {
		Tickets []struct {
			Key, Title, Status, Assignee string
		}
	}
	if err := json.Unmarshal([]byte(found), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Tickets) != 1 || page.Tickets[0].Status != "Todo" || page.Tickets[0].Assignee != "User 2" {
		t.Errorf("ticket_find row = %s; want status Todo and assignee User 2", found)
	}

	read, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_read", map[string]any{"key": "ENG-1", "history": true})
	if isError {
		t.Fatalf("ticket_read: %s", read)
	}
	var ticket struct {
		Assignee string
		Comments []struct{ By string }
		History  []struct{ By, Field string }
	}
	if err := json.Unmarshal([]byte(read), &ticket); err != nil {
		t.Fatal(err)
	}
	if ticket.Assignee != "User 2" || len(ticket.Comments) != 1 || ticket.Comments[0].By != "User 2" ||
		len(ticket.History) != 1 || ticket.History[0].By != "User 3" {
		t.Errorf("ticket_read = %s; want assignee User 2, the comment by User 2, the change by User 3", read)
	}
}

// With Jira and Linear both holding WEB-1, a search hit opens the ticket
// it found, the read says which site it read, and the bare key is refused
// naming both.
func TestTrackerMCPTwoConnectionsShareAKey(t *testing.T) {
	t.Parallel()
	ws := trackerToolsLinear("WEB")
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: trackerBoardSite(), linear: &ws})
	f.addLinear(t, ws)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread := store.Thread{ID: uuid.NewString(), ProjectID: f.project, Provider: string(provider.Codex), WorkspacePath: t.TempDir()}
	if err := f.a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	f.a.sessionManager().put(thread.ID, session{Token: token, Provider: thread.Provider})
	endpoint := trackerMCPEndpoint(t, f.a, thread, token)

	found, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_find", map[string]any{"text": "Quartz lantern"})
	if isError || !strings.Contains(found, `{"key":"example/WEB-1","title":"Quartz lantern"`) {
		t.Fatalf("ticket_find = %s", found)
	}
	read, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_read", map[string]any{"key": "example/WEB-1"})
	if isError || !strings.HasPrefix(read, `{"key":"example/WEB-1","title":"Quartz lantern"`) {
		t.Fatalf("ticket_read of the hit = %s", read)
	}
	read, isError = trackerMCPStreamedCall(t, f.a, endpoint, "ticket_read", map[string]any{"key": "example.atlassian.net/WEB-1"})
	if isError || !strings.HasPrefix(read, `{"key":"example.atlassian.net/WEB-1","title":"First story"`) {
		t.Fatalf("ticket_read of Jira's WEB-1 = %s", read)
	}
	refusal, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_read", map[string]any{"key": "WEB-1"})
	if want := "[tracker_ambiguous] WEB-1 is on example.atlassian.net and example; name one: example.atlassian.net/WEB-1 or example/WEB-1."; !isError || refusal != want {
		t.Fatalf("ticket_read of the bare key = %s\nwant %s", refusal, want)
	}
}
