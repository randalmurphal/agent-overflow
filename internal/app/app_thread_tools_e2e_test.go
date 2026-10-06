package app

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// TestThreadMCPHandshakeListsEveryToolAndTheGuide pins what a provider
// sees on connect: the thirteen tools in their documented order, and the
// decision guide as server instructions.
func TestThreadMCPHandshakeListsEveryToolAndTheGuide(t *testing.T) {
	t.Parallel()
	app, _, _ := newMCPTestApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })
	thread, token := remoteMCPThread(t, app, string(provider.Claude))
	endpoint := threadMCPEndpoint(t, app, thread, token)

	status, reply := remoteMCPRequest(t, endpoint, "initialize", map[string]any{})
	if status != http.StatusOK || reply["error"] != nil {
		t.Fatalf("initialize: %d %s", status, reply)
	}
	var handshake struct {
		ServerInfo   struct{ Name string } `json:"serverInfo"`
		Instructions string                `json:"instructions"`
	}
	if err := json.Unmarshal(reply["result"], &handshake); err != nil {
		t.Fatal(err)
	}
	if handshake.ServerInfo.Name != threadMCPName {
		t.Errorf("server name = %q, want %q", handshake.ServerInfo.Name, threadMCPName)
	}
	if handshake.Instructions != app.threadToolsServer().Instructions(app.threadToolsShape(thread.ID)) {
		t.Error("initialize did not carry the thread tools guide as its instructions")
	}

	status, reply = remoteMCPRequest(t, endpoint, "tools/list", map[string]any{})
	if status != http.StatusOK || reply["error"] != nil {
		t.Fatalf("tools/list: %d %s", status, reply)
	}
	var listing struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(reply["result"], &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Tools) != len(threadtools.ToolNames) {
		t.Fatalf("tools/list returned %d tools, want %d", len(listing.Tools), len(threadtools.ToolNames))
	}
	for i, tool := range listing.Tools {
		if tool.Name != threadtools.ToolNames[i] {
			t.Errorf("tool %d = %q, want %q", i, tool.Name, threadtools.ToolNames[i])
		}
		if tool.Description == "" || tool.InputSchema == nil {
			t.Errorf("tool %q is not fully described: %#v", tool.Name, tool)
		}
		// This app has no pairings, so no schema may offer a computer
		// parameter the call would then refuse.
		properties, _ := tool.InputSchema["properties"].(map[string]any)
		if _, present := properties["computer_id"]; present {
			t.Errorf("tool %q offers computer_id with no pairings in this build", tool.Name)
		}
	}
}

// TestThreadMCPReadToolsAnswerOverTheLoopbackTransport drives each read
// tool end to end the way a live session does: over the shared loopback
// HTTP transport, with the thread's capability token, against real store
// rows.
func TestThreadMCPReadToolsAnswerOverTheLoopbackTransport(t *testing.T) {
	t.Parallel()
	app, _, _ := newMCPTestApp(t)
	app.configDir = t.TempDir()
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })

	caller, token := remoteMCPThread(t, app, string(provider.Claude))
	target := store.Thread{
		ID: "e2e-target", ProjectID: caller.ProjectID, Title: "Windows launcher",
		Provider: string(provider.Claude), Model: "claude-opus-4-7", Mode: "chat",
		WorkspacePath: caller.WorkspacePath, CreatedAt: 1_000, UpdatedAt: 1_000,
	}
	if err := app.store.CreateThread(target); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{TurnID: "e2e-turn", ThreadID: target.ID, TurnIndex: 0, StartedAt: 2_000}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	body := strings.Repeat("payload ", 64)
	rows := []struct {
		item    store.Item
		payload *store.Payload
	}{
		{item: store.Item{ID: "e2e-user", ThreadID: target.ID, Kind: "user_text", Role: "user", Status: "completed", Summary: "the launcher stalls", ItemIndex: 0}},
		{item: store.Item{ID: "e2e-reply", ThreadID: target.ID, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "looking at it", ItemIndex: 1}},
		{
			item:    store.Item{ID: "e2e-tool", ThreadID: target.ID, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Bash", Summary: "Bash", ItemIndex: 2, PayloadID: "e2e-payload", PayloadKind: "tool_result"},
			payload: &store.Payload{ID: "e2e-payload", Kind: "tool_result", Data: []byte(body), CreatedAt: 2_000},
		},
	}
	for _, row := range rows {
		if _, err := app.store.UpsertItem(row.item, row.payload); err != nil {
			t.Fatalf("UpsertItem(%s): %v", row.item.ID, err)
		}
	}
	if err := app.store.UpdateTurnCompleted("e2e-turn", 2_100, "end_turn", "", "", ""); err != nil {
		t.Fatalf("UpdateTurnCompleted: %v", err)
	}
	endpoint := threadMCPEndpoint(t, app, caller, token)

	decode := func(raw json.RawMessage) map[string]any {
		t.Helper()
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return out
	}

	// thread_search: the query hits the FTS index and names the thread.
	search := decode(remoteMCPCall(t, endpoint, "thread_search", map[string]any{"query": "launcher", "limit": 5}, false))
	hits, _ := search["rows"].([]any)
	if len(hits) == 0 {
		t.Fatalf("thread_search found nothing: %v", search)
	}
	first, _ := hits[0].(map[string]any)
	if first["thread_id"] != target.ID || first["title"] != target.Title {
		t.Fatalf("first hit = %v", first)
	}

	// thread_search with no query is the listing, which reaches the same
	// rows without the index.
	listing := decode(remoteMCPCall(t, endpoint, "thread_search", map[string]any{"limit": 5}, false))
	if rows, _ := listing["rows"].([]any); len(rows) == 0 {
		t.Fatalf("thread_search listing was empty: %v", listing)
	}

	// thread_show: the transcript, addressed by an unambiguous prefix.
	show := decode(remoteMCPCall(t, endpoint, "thread_show", map[string]any{"thread_id": "e2e-targ"}, false))
	if show["thread_id"] != target.ID {
		t.Fatalf("thread_show resolved to %v", show["thread_id"])
	}
	transcript, _ := show["transcript"].(string)
	if !strings.Contains(transcript, "the launcher stalls") || !strings.Contains(transcript, "looking at it") {
		t.Fatalf("thread_show transcript = %q", transcript)
	}

	// thread_item: a byte range of one item's stored body.
	item := decode(remoteMCPCall(t, endpoint, "thread_item", map[string]any{
		"thread_id": target.ID, "item_id": "e2e-tool", "offset": 0, "max_bytes": 32,
	}, false))
	text, _ := item["text"].(string)
	if !strings.HasPrefix(body, text) || text == "" {
		t.Fatalf("thread_item text = %q, want a prefix of the payload", text)
	}
	if size, _ := item["size"].(float64); int(size) != len(body) {
		t.Fatalf("thread_item size = %v, want %d", item["size"], len(body))
	}

	// thread_options: the spawn catalog from the app's real registries.
	options := decode(remoteMCPCall(t, endpoint, "thread_options", map[string]any{}, false))
	// The single-computer shape: this computer answers for itself, with
	// no computer grouping and no computer parameter anywhere.
	if options["local"] != true {
		t.Fatalf("thread_options is not the single-computer shape: %v", options)
	}
	providers, _ := options["providers"].([]any)
	projects, _ := options["projects"].([]any)
	if len(providers) == 0 || len(projects) == 0 {
		t.Fatalf("thread_options carried no catalogs: %v", options)
	}
	if options["os"] != threadToolsOS() {
		t.Errorf("thread_options os = %v, want %s", options["os"], threadToolsOS())
	}

	// A thread that does not exist is the documented refusal, not a crash.
	refusal := remoteMCPCall(t, endpoint, "thread_show", map[string]any{"thread_id": "00000000-dead"}, true)
	if !strings.Contains(string(refusal), threadtools.CodeNotFound) {
		t.Fatalf("missing thread refusal = %s", refusal)
	}

	// A call whose session token no longer matches is refused: the
	// registration outlives one session start only as long as its token.
	app.sessionManager().put(caller.ID, session{Provider: caller.Provider, Token: "a-different-token"})
	stale := remoteMCPCall(t, endpoint, "thread_search", map[string]any{"limit": 1}, true)
	if !strings.Contains(string(stale), "thread_session_inactive") {
		t.Fatalf("stale-token refusal = %s", stale)
	}
}

// TestThreadMCPSpawnAndReplyRunOverTheLoopbackTransport is the write half
// end to end: one thread spawns work over the shared loopback transport, the
// thread that ran it answers through its own session's transport, and the
// answer reaches the sender in the reply to its next call.
func TestThreadMCPSpawnAndReplyRunOverTheLoopbackTransport(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	// The turn stays open, which is what lets the answer be a reply rather
	// than the turn's last words.
	f.mockClaudeHoldingTheTurn(t, "counting")
	callerToken := uuid.NewString()
	f.app.sessionManager().put(f.caller.ID, session{Token: callerToken, Provider: string(provider.Claude)})
	t.Cleanup(func() { _ = f.app.threadMCPServer().Close() })
	endpoint := threadMCPEndpoint(t, f.app, f.caller, callerToken)

	raw := remoteMCPCall(t, endpoint, "thread_spawn", map[string]any{
		"prompt": "count the rows the backfill touched", "title": "Row count", "wait_seconds": 0,
	}, false)
	var ack threadtools.RequestAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		t.Fatalf("decode spawn ack %s: %v", raw, err)
	}
	if ack.Token == "" || ack.ThreadID == "" || ack.Outcome != threadtools.OutcomeBackgrounded {
		t.Fatalf("spawn ack = %+v", ack)
	}
	waitUntil(t, 15*time.Second, func() bool {
		row, found, err := f.app.store.GetThreadRequestReceipt(ack.Token)
		return err == nil && found && row.State == store.ThreadReceiptRunning
	})

	// The spawned thread answers through its own session's transport, with
	// the token that session was started with.
	spawned, live := f.app.sessionManager().get(ack.ThreadID)
	if !live {
		t.Fatalf("the spawn did not start a session for %s", ack.ThreadID)
	}
	target, err := f.app.store.GetThread(ack.ThreadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	raw = remoteMCPCall(t, threadMCPEndpoint(t, f.app, target, spawned.Token), "thread_reply", map[string]any{
		"token": ack.Token, "text": "41,220 rows",
	}, false)
	var reply threadtools.ReplyAck
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("decode reply ack %s: %v", raw, err)
	}
	if !reply.Accepted || reply.SourceThreadID != f.caller.ID {
		t.Fatalf("reply ack = %+v", reply)
	}

	// The sender reads the answer in its next call, and reading it is what
	// delivers it: nothing is owed as a message afterwards.
	raw = remoteMCPCall(t, endpoint, "thread_status", map[string]any{"tokens": []string{ack.Token}}, false)
	var report threadtools.StatusReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decode status %s: %v", raw, err)
	}
	if len(report.Requests) != 1 || report.Requests[0].Answer != "41,220 rows" {
		t.Fatalf("status = %s", raw)
	}
	if report.Requests[0].State != store.ThreadRequestReplied {
		t.Errorf("state = %q, want replied", report.Requests[0].State)
	}
	// The mark is written once the response has left the server, which is
	// after this client has read it.
	waitUntilE2E(t, 10*time.Second, "the inline delivery is recorded", func() bool {
		return f.request(t, ack.Token).DeliveredHow == store.ThreadWakeInline
	})
	if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
		t.Fatalf("a wake was queued for an answer the sender just read: %+v", rows)
	}
}

// TestThreadSearchIndexBuildsAtBootAndJoinsOnShutdown pins the lifecycle
// of the background index build: it runs off the boot path and shutdown
// joins it, so nothing is still writing FTS rows when SQLite closes.
func TestThreadSearchIndexBuildsAtBootAndJoinsOnShutdown(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := store.Thread{
		ID: "index-thread", ProjectID: defaultTestProjectID, Title: "Launcher work",
		Provider: string(provider.Claude), Mode: "chat", CreatedAt: 1, UpdatedAt: 1,
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	// The migration seeds the progress row, so a fresh boot always has a
	// build to finish.
	building, err := app.store.SearchIndexing()
	if err != nil {
		t.Fatalf("SearchIndexing: %v", err)
	}
	if !building {
		t.Fatal("a fresh database reported no outstanding index build")
	}

	app.startThreadSearchIndex()
	// A page's first catalog reads release the build.
	if _, err := app.ListThreads(); err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if _, err := app.ListProjects(); err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	// The join is what shutdown does; after it the build has settled.
	app.waitThreadSearchIndex()

	building, err = app.store.SearchIndexing()
	if err != nil {
		t.Fatalf("SearchIndexing after the build: %v", err)
	}
	if building {
		t.Fatal("the boot build did not finish before the join returned")
	}

	// Started once: a second call is a no-op, and the join stays safe.
	app.startThreadSearchIndex()
	app.waitThreadSearchIndex()
}

// TestThreadSearchIndexJoinsAfterCancellation pins the interrupted case:
// the app context is cancelled, the build stops where it is, and the join
// still returns rather than leaving a goroutine writing past the store's
// close.
func TestThreadSearchIndexJoinsAfterCancellation(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	app.appCtx, app.appCancel = ctx, cancel
	cancel()

	app.startThreadSearchIndex()
	app.waitThreadSearchIndex()

	// An interrupted build leaves its progress row for the next boot.
	if _, err := app.store.SearchIndexing(); err != nil {
		t.Fatalf("SearchIndexing: %v", err)
	}
}

// TestThreadMCPReadsAnswerTheCommonQuestionsInOneCall drives the reads an
// agent coordinating other threads makes most, over the loopback transport
// against real store rows: what did a thread conclude, which threads are
// waiting on me, and what happened in a busy turn, without paging through
// a line per tool call.
func TestThreadMCPReadsAnswerTheCommonQuestionsInOneCall(t *testing.T) {
	t.Parallel()
	app, _, _ := newMCPTestApp(t)
	app.configDir = t.TempDir()
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })

	caller, token := remoteMCPThread(t, app, string(provider.Claude))
	target := store.Thread{
		ID: "e2e-review", ProjectID: caller.ProjectID, Title: "Review the parser",
		Provider: string(provider.Claude), Model: "claude-opus-4-7", Mode: "chat",
		WorkspacePath: caller.WorkspacePath, CreatedAt: 1_000, UpdatedAt: 1_000,
	}
	if err := app.store.CreateThread(target); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	group, err := app.store.CreateThreadGroup(caller.ProjectID, "MR Reviews")
	if err != nil {
		t.Fatalf("CreateThreadGroup: %v", err)
	}
	if _, err := app.store.SetThreadGroup([]string{target.ID}, group.ID); err != nil {
		t.Fatalf("SetThreadGroup: %v", err)
	}
	const answer = "Two findings, both in the lexer. Want me to push the fix?"
	for turn := range 3 {
		turnID := "e2e-review-turn-" + string(rune('0'+turn))
		startedAt := int64(2_000 + turn*100)
		if err := app.store.InsertTurn(store.Turn{TurnID: turnID, ThreadID: target.ID, TurnIndex: turn, StartedAt: startedAt}); err != nil {
			t.Fatalf("InsertTurn: %v", err)
		}
		prefix := "e2e-" + string(rune('a'+turn))
		index := 0
		upsert := func(item store.Item, payload *store.Payload) {
			t.Helper()
			item.ThreadID, item.TurnIndex, item.ItemIndex, item.Status = target.ID, turn, index, "completed"
			item.CreatedAt, item.UpdatedAt = startedAt+int64(index), startedAt+int64(index)
			index++
			if _, err := app.store.UpsertItem(item, payload); err != nil {
				t.Fatalf("UpsertItem(%s): %v", item.ID, err)
			}
		}
		upsert(store.Item{ID: prefix + "-user", Kind: "user_text", Role: "user", Summary: "review turn " + string(rune('0'+turn))}, nil)
		for call, tool := range []string{"Bash", "Read", "Bash", "Read", "Bash"} {
			id := prefix + "-tool-" + string(rune('0'+call))
			upsert(store.Item{ID: id, Kind: "tool_call", Role: "assistant", ToolName: tool, Summary: "go test ./internal/parser", PayloadID: id + "-out", PayloadKind: "tool_result"},
				&store.Payload{ID: id + "-out", Kind: "tool_result", Data: []byte(strings.Repeat("ok ", 1000)), CreatedAt: startedAt})
		}
		text := "turn " + string(rune('0'+turn)) + " done"
		if turn == 2 {
			text = answer
		}
		upsert(store.Item{ID: prefix + "-answer", Kind: "assistant_text", Role: "assistant", Summary: text}, nil)
		if err := app.store.UpdateTurnCompleted(turnID, startedAt+50, "end_turn", "", "", ""); err != nil {
			t.Fatalf("UpdateTurnCompleted: %v", err)
		}
	}
	endpoint := threadMCPEndpoint(t, app, caller, token)
	decode := func(raw json.RawMessage) map[string]any {
		t.Helper()
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return out
	}

	// What did it conclude: one call, the answer alone.
	last := decode(remoteMCPCall(t, endpoint, "thread_show", map[string]any{"thread_id": target.ID, "window": "last_answer"}, false))
	if last["transcript"] != "--- turn 2 ---\n[assistant e2e-c-answer] "+answer {
		t.Fatalf("last_answer = %q", last["transcript"])
	}

	// A busy turn is a line per run of calls, not a line per call.
	show := decode(remoteMCPCall(t, endpoint, "thread_show", map[string]any{"thread_id": target.ID}, false))
	transcript, _ := show["transcript"].(string)
	folded := "(5 tool calls: Bash 3, Read 2; 14.6 KB not shown)"
	if strings.Count(transcript, folded) != 3 || strings.Contains(transcript, "[tool ") {
		t.Fatalf("default transcript does not fold the calls:\n%s", transcript)
	}
	listed := decode(remoteMCPCall(t, endpoint, "thread_show", map[string]any{"thread_id": target.ID, "include": []string{"tool_calls"}, "turns": 1}, false))
	if want := "[tool e2e-c-tool-0] Bash: go test ./internal/parser (2.9 KB)"; !strings.Contains(listed["transcript"].(string), want) {
		t.Fatalf("tool_calls transcript is missing %q:\n%s", want, listed["transcript"])
	}

	// A tail that does not fit keeps the newest rows, and its cursor, with
	// the same parameters repeated, reads the rows before them.
	tail := decode(remoteMCPCall(t, endpoint, "thread_show", map[string]any{"thread_id": target.ID, "turns": 3, "max_bytes": 1024, "include": []string{"tool_outputs"}}, false))
	if text, _ := tail["transcript"].(string); !strings.HasSuffix(text, "[assistant e2e-c-answer] "+answer) || tail["done"] != false {
		t.Fatalf("tail page does not end at the answer (done %v):\n%s", tail["done"], text)
	}
	before := decode(remoteMCPCall(t, endpoint, "thread_show", map[string]any{"thread_id": target.ID, "turns": 3, "include": []string{"tool_outputs"}, "cursor": tail["cursor"]}, false))
	if text, _ := before["transcript"].(string); strings.Contains(text, answer) || !strings.Contains(text, "e2e-c-tool-") {
		t.Fatalf("the page before the tail repeats it or skipped back too far:\n%s", text)
	}

	// Which threads are waiting on me: the group narrows the listing, and
	// each row says who spoke last and how it ended.
	listing := decode(remoteMCPCall(t, endpoint, "thread_search", map[string]any{"group": "mr reviews"}, false))
	rows, _ := listing["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("group listing = %v", listing)
	}
	row := rows[0].(map[string]any)
	message, _ := row["last_message"].(map[string]any)
	if row["thread_id"] != target.ID || message["role"] != "assistant" || message["text"] != answer || message["at"] == "" {
		t.Fatalf("row = %v", row)
	}
	everything := decode(remoteMCPCall(t, endpoint, "thread_search", map[string]any{}, false))
	selves := 0
	for _, entry := range everything["rows"].([]any) {
		if entry.(map[string]any)["self"] == true {
			selves++
			if entry.(map[string]any)["thread_id"] != caller.ID {
				t.Errorf("a thread other than the caller is marked self: %v", entry)
			}
		}
	}
	if selves != 1 {
		t.Errorf("the caller's own row is marked %d times", selves)
	}
	refusal := remoteMCPCall(t, endpoint, "thread_search", map[string]any{"group": "No Such Group"}, true)
	if !strings.Contains(string(refusal), "no thread group") {
		t.Fatalf("unknown group refusal = %s", refusal)
	}

	// A group lookup is the groups, not every provider and worktree.
	options := decode(remoteMCPCall(t, endpoint, "thread_options", map[string]any{"what": "groups"}, false))
	if _, present := options["providers"]; present {
		t.Errorf("what groups carried providers: %v", options)
	}
	found := false
	for _, project := range options["projects"].([]any) {
		groups, _ := project.(map[string]any)["groups"].([]any)
		for _, g := range groups {
			found = found || g.(map[string]any)["group"] == "MR Reviews"
		}
	}
	if !found {
		t.Fatalf("what groups did not list the group: %v", options)
	}

	// The export lists every row with its label, so the file stays the way
	// to grep a whole thread.
	exported := decode(remoteMCPCall(t, endpoint, "thread_show", map[string]any{"thread_id": target.ID, "window": "all", "to_file": true}, false))
	path, _ := exported["file"].(map[string]any)["path"].(string)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if !strings.Contains(string(data), "[tool e2e-a-tool-0] Bash: go test ./internal/parser (3000 bytes not shown)\n") {
		t.Fatalf("export does not list the call with its label:\n%s", data[:min(len(data), 600)])
	}
}
