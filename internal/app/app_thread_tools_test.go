package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// threadToolsFixture is a real store behind the adapter: threads, turns,
// items and payloads written through the production write paths, so the
// derived columns and the search index are the ones production produces.
type threadToolsFixture struct {
	app     *App
	adapter threadtools.App
	project store.Project
}

func newThreadToolsFixture(t *testing.T) *threadToolsFixture {
	t.Helper()
	app := newTestAppWithStore(t)
	// Exports land under the data directory; a fixture without one would
	// write into the repository working directory.
	app.configDir = t.TempDir()
	project := store.Project{ID: "tt-project", Path: t.TempDir(), Name: "Thread tools", CreatedAt: 1, UpdatedAt: 1}
	if _, err := app.store.CreateProject(project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return &threadToolsFixture{app: app, adapter: app.threadToolsAdapter(), project: project}
}

func (f *threadToolsFixture) thread(t *testing.T, id string, apply ...func(*store.Thread)) store.Thread {
	t.Helper()
	thread := store.Thread{
		ID: id, ProjectID: f.project.ID, Title: "Thread " + id,
		Provider: string(provider.Claude), Model: "claude-opus-4-7", Mode: threadmode.ModeChat,
		WorkspacePath: f.project.Path, RuntimeMode: string(provider.RuntimeApprovalRequired),
		CreatedAt: 1000, UpdatedAt: 1000,
	}
	for _, fn := range apply {
		fn(&thread)
	}
	if err := f.app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(%s): %v", id, err)
	}
	row, err := f.app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread(%s): %v", id, err)
	}
	return row
}

// turn writes one settled turn and its items, exactly as triage does.
func (f *threadToolsFixture) turn(t *testing.T, threadID string, turnIndex int, startedAt int64, items ...store.Item) {
	t.Helper()
	turnID := threadID + "-turn-" + string(rune('a'+turnIndex))
	if err := f.app.store.InsertTurn(store.Turn{TurnID: turnID, ThreadID: threadID, TurnIndex: turnIndex, StartedAt: startedAt}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	for i, item := range items {
		item.ThreadID = threadID
		item.TurnIndex = turnIndex
		if item.ItemIndex == 0 {
			item.ItemIndex = i
		}
		if item.Status == "" {
			item.Status = "completed"
		}
		var payload *store.Payload
		if item.PayloadID != "" {
			payload = &store.Payload{ID: item.PayloadID, Kind: item.PayloadKind, Data: []byte(item.PayloadMeta), CreatedAt: startedAt}
			item.PayloadMeta = ""
		}
		if _, err := f.app.store.UpsertItem(item, payload); err != nil {
			t.Fatalf("UpsertItem(%s): %v", item.ID, err)
		}
	}
	if err := f.app.store.UpdateTurnCompleted(turnID, startedAt+10, "end_turn", "", "", ""); err != nil {
		t.Fatalf("UpdateTurnCompleted: %v", err)
	}
}

// payloadItem builds a tool row whose body lives in a payload. The bytes
// ride in PayloadMeta and the fixture moves them into the payload blob.
func payloadItem(id, toolName, body string) store.Item {
	return store.Item{
		ID: id, Kind: "tool_call", Role: "assistant", ToolName: toolName, Summary: toolName,
		PayloadID: id + "-payload", PayloadKind: "tool_result", PayloadMeta: body,
		CreatedAt: 1, UpdatedAt: 1,
	}
}

func textItem(id, kind, text string) store.Item {
	role := "assistant"
	if kind == "user_text" {
		role = "user"
	}
	return store.Item{ID: id, Kind: kind, Role: role, Summary: text, CreatedAt: 1, UpdatedAt: 1}
}

func publicCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("want a public error, got nil")
	}
	code, _, ok := errorsx.PublicDetails(err)
	if !ok {
		t.Fatalf("error %v is not public", err)
	}
	return code
}

// TestThreadToolsAdapterReadsAThreadRow pins the projection: the derived
// columns the state derivation reads, the two names resolved beside their
// ids, and the pin tier the sidebar shows.
func TestThreadToolsAdapterReadsAThreadRow(t *testing.T) {
	f := newThreadToolsFixture(t)
	group, err := f.app.store.CreateThreadGroup(f.project.ID, "Release")
	if err != nil {
		t.Fatalf("CreateThreadGroup: %v", err)
	}
	thread := f.thread(t, "row-thread", func(th *store.Thread) {
		th.Title = "Windows launcher"
		th.Provider = string(provider.Codex)
		th.Model = "gpt-5"
		th.ReasoningEffort = "high"
		th.Branch = "feature/launcher"
		th.WorktreePath = f.project.Path + "/wt"
	})
	if _, err := f.app.store.SetThreadGroup([]string{thread.ID}, group.ID); err != nil {
		t.Fatalf("SetThreadGroup: %v", err)
	}
	f.turn(t, thread.ID, 0, 5_000, textItem("i1", "user_text", "port the fix"))

	got, err := f.adapter.Thread(t.Context(), thread.ID)
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if got.Title != "Windows launcher" || got.Provider != string(provider.Codex) || got.Model != "gpt-5" || got.Effort != "high" {
		t.Errorf("thread = %#v", got)
	}
	if got.Project != "Thread tools" || got.Group != "Release" || got.GroupID != group.ID {
		t.Errorf("names not resolved: %#v", got)
	}
	// The worktree is where the thread actually runs, so it is the
	// workspace the model is told about.
	if got.WorkspacePath != f.project.Path+"/wt" || got.Branch != "feature/launcher" {
		t.Errorf("workspace = %q branch = %q", got.WorkspacePath, got.Branch)
	}
	// The activity clock is the latest completed turn, not updated_at.
	if got.LastActivity != 5_010 {
		t.Errorf("lastActivity = %d, want the completed turn's clock", got.LastActivity)
	}
	if got.Pin != "" {
		t.Errorf("pin = %q, want unpinned", got.Pin)
	}

	// Pinned to the back burner: pinned_at is what makes a row pinned at
	// all, and pin_group is only the tier. A grouped row holds no pin of
	// its own, so the pin fixture is a second thread.
	pinned := f.thread(t, "pinned-thread")
	if _, _, err := f.app.store.PinThread(pinned.ID); err != nil {
		t.Fatalf("PinThread: %v", err)
	}
	if _, _, err := f.app.store.SetThreadPinGroup(pinned.ID, store.PinGroupBack); err != nil {
		t.Fatalf("SetThreadPinGroup: %v", err)
	}
	back, err := f.adapter.Thread(t.Context(), pinned.ID)
	if err != nil {
		t.Fatalf("Thread after pin: %v", err)
	}
	if back.Pin != threadtools.PinBack {
		t.Errorf("pin = %q, want %q", back.Pin, threadtools.PinBack)
	}

	// A thread this computer does not have is the documented not-found
	// code, which is what the refusal prose is built from.
	_, err = f.adapter.Thread(t.Context(), "no-such-thread")
	if code := publicCode(t, err); code != threadtools.CodeNotFound {
		t.Errorf("missing thread code = %q, want %q", code, threadtools.CodeNotFound)
	}
}

// TestThreadToolsAdapterResolvesRefs covers the three answers resolution
// can give: one match, an ambiguous prefix, and a scratch thread nobody
// may see.
func TestThreadToolsAdapterResolvesRefs(t *testing.T) {
	f := newThreadToolsFixture(t)
	first := f.thread(t, "abcd1111-one")
	second := f.thread(t, "abcd2222-two")
	f.thread(t, "ffff0000-other")

	// A full id resolves to exactly itself.
	res, err := f.adapter.ResolveThreadRef(t.Context(), first.ID)
	if err != nil {
		t.Fatalf("ResolveThreadRef: %v", err)
	}
	if len(res.Matches) != 1 || res.Matches[0].ThreadID != first.ID || res.Matches[0].Title != first.Title {
		t.Fatalf("full id = %#v", res.Matches)
	}

	// A prefix both threads share comes back as both candidates, in id
	// order, so the refusal can list them.
	res, err = f.adapter.ResolveThreadRef(t.Context(), "abcd")
	if err != nil {
		t.Fatalf("ResolveThreadRef(prefix): %v", err)
	}
	if len(res.Matches) != 2 || res.Matches[0].ThreadID != first.ID || res.Matches[1].ThreadID != second.ID {
		t.Fatalf("ambiguous prefix = %#v", res.Matches)
	}

	// No match is an empty resolution, not an error.
	res, err = f.adapter.ResolveThreadRef(t.Context(), "zzzz")
	if err != nil || len(res.Matches) != 0 || res.MovedTo != "" {
		t.Fatalf("miss = %#v, %v", res, err)
	}

	// A /side-chat scratch thread carries no request token and stays
	// invisible; the one a thread_ask minted is reachable.
	hidden := f.thread(t, "5c4a7c00-hidden", func(th *store.Thread) { th.Mode = threadmode.ModeScratch })
	if err := f.app.store.InsertScratchThread(store.ScratchThread{
		ThreadID: hidden.ID, SourceThreadID: first.ID, ReturnMode: threadmode.ModeChat,
	}); err != nil {
		t.Fatalf("InsertScratchThread: %v", err)
	}
	asked := f.thread(t, "5c4a7c11-asked", func(th *store.Thread) { th.Mode = threadmode.ModeScratch })
	if err := f.app.store.InsertScratchThread(store.ScratchThread{
		ThreadID: asked.ID, SourceThreadID: first.ID, ReturnMode: threadmode.ModeChat, RequestToken: "tok-1",
	}); err != nil {
		t.Fatalf("InsertScratchThread: %v", err)
	}
	if res, err = f.adapter.ResolveThreadRef(t.Context(), hidden.ID); err != nil || len(res.Matches) != 0 {
		t.Fatalf("a /side-chat scratch thread resolved: %#v, %v", res, err)
	}
	if res, err = f.adapter.ResolveThreadRef(t.Context(), asked.ID); err != nil || len(res.Matches) != 1 {
		t.Fatalf("an asked scratch thread did not resolve: %#v, %v", res, err)
	}
}

// TestThreadToolsAdapterResolvesWindows pins each window kind against a
// four-turn thread.
func TestThreadToolsAdapterResolvesWindows(t *testing.T) {
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "window-thread")
	for turn := range 4 {
		f.turn(t, thread.ID, turn, int64(1_000+turn*1_000),
			textItem("u"+string(rune('0'+turn)), "user_text", "ask"),
			textItem("a"+string(rune('0'+turn)), "assistant_text", "answer"))
	}
	ctx := t.Context()

	all, err := f.adapter.ResolveWindow(ctx, threadtools.WindowQuery{ThreadID: thread.ID, Kind: threadtools.WindowAll})
	if err != nil {
		t.Fatalf("ResolveWindow(all): %v", err)
	}
	if all.Empty || all.From != encodePosition(0, 0) || all.To != encodePosition(3, 1) {
		t.Fatalf("all = %#v", all)
	}
	// A non-empty window always names all three positions.
	if all.HighWater != all.To {
		t.Errorf("highWater = %d, want the last position %d", all.HighWater, all.To)
	}

	tail, err := f.adapter.ResolveWindow(ctx, threadtools.WindowQuery{ThreadID: thread.ID, Kind: threadtools.WindowTail, Turns: 2})
	if err != nil {
		t.Fatalf("ResolveWindow(tail): %v", err)
	}
	if tail.From != turnFloor(2) || tail.To != all.To || tail.HighWater != all.HighWater {
		t.Fatalf("tail = %#v", tail)
	}

	head, err := f.adapter.ResolveWindow(ctx, threadtools.WindowQuery{ThreadID: thread.ID, Kind: threadtools.WindowHead, Turns: 1})
	if err != nil {
		t.Fatalf("ResolveWindow(head): %v", err)
	}
	if head.From != all.From || head.To != turnCeil(0) {
		t.Fatalf("head = %#v", head)
	}

	around, err := f.adapter.ResolveWindow(ctx, threadtools.WindowQuery{ThreadID: thread.ID, Kind: threadtools.WindowAround, ItemID: "u2", Turns: 1})
	if err != nil {
		t.Fatalf("ResolveWindow(around): %v", err)
	}
	if around.From != turnFloor(1) || around.To != all.To {
		t.Fatalf("around = %#v", around)
	}

	// since by timestamp counts in turns: the window starts at the first
	// turn that began at or after it.
	since, err := f.adapter.ResolveWindow(ctx, threadtools.WindowQuery{ThreadID: thread.ID, Kind: threadtools.WindowSince, SinceUnixMs: 2_500})
	if err != nil {
		t.Fatalf("ResolveWindow(since): %v", err)
	}
	if since.From != turnFloor(2) {
		t.Fatalf("since = %#v", since)
	}
	// since by item starts at the row AFTER the anchor.
	sinceItem, err := f.adapter.ResolveWindow(ctx, threadtools.WindowQuery{ThreadID: thread.ID, Kind: threadtools.WindowSince, ItemID: "a2"})
	if err != nil {
		t.Fatalf("ResolveWindow(since item): %v", err)
	}
	if sinceItem.From != encodePosition(2, 1)+1 {
		t.Fatalf("since item = %#v", sinceItem)
	}
	// Nothing has happened since a timestamp past the last turn.
	quiet, err := f.adapter.ResolveWindow(ctx, threadtools.WindowQuery{ThreadID: thread.ID, Kind: threadtools.WindowSince, SinceUnixMs: 99_000})
	if err != nil || !quiet.Empty {
		t.Fatalf("since the future = %#v, %v", quiet, err)
	}

	// A thread with no items has no window at all.
	empty := f.thread(t, "empty-thread")
	bounds, err := f.adapter.ResolveWindow(ctx, threadtools.WindowQuery{ThreadID: empty.ID, Kind: threadtools.WindowAll})
	if err != nil || !bounds.Empty {
		t.Fatalf("empty thread = %#v, %v", bounds, err)
	}

	if _, err := f.adapter.ResolveWindow(ctx, threadtools.WindowQuery{ThreadID: thread.ID, Kind: "sideways"}); publicCode(t, err) != threadtools.CodeInvalidRequest {
		t.Errorf("an unknown window kind did not refuse publicly: %v", err)
	}
}

// TestThreadToolsAdapterPagesTheTranscript pins the two contract rules a
// page must honour: never more than Limit rows, and a clipped body that
// still reports its whole size.
func TestThreadToolsAdapterPagesTheTranscript(t *testing.T) {
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "transcript-thread")
	body := strings.Repeat("x", 5_000)
	f.turn(t, thread.ID, 0, 1_000,
		textItem("t0", "user_text", "first"),
		textItem("t1", "assistant_text", "second"),
		payloadItem("t2", "Bash", body))
	f.turn(t, thread.ID, 1, 2_000,
		textItem("t3", "user_text", "third"),
		store.Item{ID: "t4", Kind: "thinking", Role: "assistant", Summary: "pondering", CreatedAt: 1, UpdatedAt: 1})
	ctx := t.Context()

	bounds, err := f.adapter.ResolveWindow(ctx, threadtools.WindowQuery{ThreadID: thread.ID, Kind: threadtools.WindowAll})
	if err != nil {
		t.Fatalf("ResolveWindow: %v", err)
	}

	page, err := f.adapter.Transcript(ctx, threadtools.TranscriptQuery{
		ThreadID: thread.ID, From: bounds.From, To: bounds.To, Limit: 2,
	})
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("page = %d rows, want exactly Limit", len(page.Items))
	}
	if page.Items[0].ID != "t0" || page.Items[1].ID != "t1" {
		t.Fatalf("page order = %#v", page.Items)
	}
	if page.HighWater != bounds.HighWater {
		t.Errorf("highWater = %d, want %d", page.HighWater, bounds.HighWater)
	}
	// Prose rows always carry their body.
	if page.Items[0].Text != "first" || page.Items[0].Role != "user" {
		t.Errorf("user row = %#v", page.Items[0])
	}

	// A second page continues after the first, never repeating a row.
	next, err := f.adapter.Transcript(ctx, threadtools.TranscriptQuery{
		ThreadID: thread.ID, From: page.Items[1].Position + 1, To: bounds.To, Limit: 10,
		Include: []string{threadtools.IncludeToolOutputs, threadtools.IncludeThinking}, MaxItemBytes: 100,
	})
	if err != nil {
		t.Fatalf("Transcript(page 2): %v", err)
	}
	if len(next.Items) != 3 || next.Items[0].ID != "t2" {
		t.Fatalf("second page = %#v", next.Items)
	}
	tool := next.Items[0]
	// The body is clipped to MaxItemBytes and Size still reports the
	// whole stored payload, which is what points the model at thread_item.
	if len(tool.Text) != 100 || !tool.Clipped || tool.Size != int64(len(body)) {
		t.Fatalf("clipped tool row = %#v (text %d bytes)", tool, len(tool.Text))
	}
	if tool.Name != "Bash" || tool.Kind != "tool_call" {
		t.Errorf("tool row = %#v", tool)
	}
	// A kind the include list leaves out still yields its row and size,
	// without its body.
	bare, err := f.adapter.Transcript(ctx, threadtools.TranscriptQuery{
		ThreadID: thread.ID, From: bounds.From, To: bounds.To, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Transcript(no includes): %v", err)
	}
	var thinking threadtools.Item
	for _, item := range bare.Items {
		if item.ID == "t4" {
			thinking = item
		}
	}
	if thinking.Kind != "thinking" || thinking.Text != "" || thinking.Size != int64(len("pondering")) {
		t.Fatalf("thinking row = %#v", thinking)
	}
}

// TestThreadToolsAdapterReadsItemPayloadRanges pins the range reader the
// package streams a large item through, and the zero-byte metadata read
// it uses to learn the size first.
func TestThreadToolsAdapterReadsItemPayloadRanges(t *testing.T) {
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "payload-thread")
	body := "0123456789abcdef"
	f.turn(t, thread.ID, 0, 1_000, payloadItem("p0", "Read", body), textItem("p1", "assistant_text", "inline"))
	ctx := t.Context()

	// MaxBytes zero is a metadata read: the size with no bytes.
	meta, err := f.adapter.ItemPayload(ctx, threadtools.PayloadQuery{ThreadID: thread.ID, ItemID: "p0"})
	if err != nil {
		t.Fatalf("ItemPayload(metadata): %v", err)
	}
	if meta.Size != int64(len(body)) || len(meta.Bytes) != 0 || meta.Kind != "tool_call" {
		t.Fatalf("metadata read = %#v", meta)
	}

	mid, err := f.adapter.ItemPayload(ctx, threadtools.PayloadQuery{ThreadID: thread.ID, ItemID: "p0", Offset: 4, MaxBytes: 6})
	if err != nil {
		t.Fatalf("ItemPayload(range): %v", err)
	}
	if string(mid.Bytes) != "456789" || mid.Offset != 4 || mid.Size != int64(len(body)) {
		t.Fatalf("range read = %#v (%q)", mid, mid.Bytes)
	}

	// An offset at or past the end is an empty read with the size, never
	// an error: that is how the line walk learns it is done.
	past, err := f.adapter.ItemPayload(ctx, threadtools.PayloadQuery{ThreadID: thread.ID, ItemID: "p0", Offset: 99, MaxBytes: 10})
	if err != nil {
		t.Fatalf("ItemPayload(past end): %v", err)
	}
	if len(past.Bytes) != 0 || past.Size != int64(len(body)) {
		t.Fatalf("past-end read = %#v", past)
	}

	// A row whose body is the inline summary reads the same way.
	inline, err := f.adapter.ItemPayload(ctx, threadtools.PayloadQuery{ThreadID: thread.ID, ItemID: "p1", Offset: 1, MaxBytes: 3})
	if err != nil {
		t.Fatalf("ItemPayload(inline): %v", err)
	}
	if string(inline.Bytes) != "nli" || inline.Size != int64(len("inline")) {
		t.Fatalf("inline read = %#v (%q)", inline, inline.Bytes)
	}

	if _, err := f.adapter.ItemPayload(ctx, threadtools.PayloadQuery{ThreadID: thread.ID, ItemID: "ghost"}); publicCode(t, err) != threadtools.CodeNotFound {
		t.Errorf("a missing item did not refuse with not-found: %v", err)
	}
}

// TestThreadToolsAdapterSearches covers both halves of thread_search: the
// ranked query against the real FTS index, and the listing the adapter
// filters itself.
func TestThreadToolsAdapterSearches(t *testing.T) {
	f := newThreadToolsFixture(t)
	claudeThread := f.thread(t, "search-claude", func(th *store.Thread) { th.Title = "Launcher work" })
	codexThread := f.thread(t, "search-codex", func(th *store.Thread) {
		th.Title = "Packaging"
		th.Provider = string(provider.Codex)
	})
	archived := f.thread(t, "search-archived", func(th *store.Thread) { th.Title = "Old launcher notes" })
	f.turn(t, claudeThread.ID, 0, 5_000, textItem("s0", "user_text", "the launcher stalls on wsl"))
	f.turn(t, codexThread.ID, 0, 6_000, textItem("s1", "user_text", "packaging the launcher"))
	f.turn(t, archived.ID, 0, 1_000, textItem("s2", "user_text", "launcher history"))
	if _, _, err := f.app.store.ArchiveThread(archived.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	ctx := t.Context()
	// The boot build is what clears the progress row; run it so this test
	// reads a settled index rather than a partial one.
	if err := f.app.store.BuildSearchIndex(ctx); err != nil {
		t.Fatalf("BuildSearchIndex: %v", err)
	}

	page, err := f.adapter.SearchThreads(ctx, threadtools.SearchQuery{Query: "launcher", Limit: 10})
	if err != nil {
		t.Fatalf("SearchThreads: %v", err)
	}
	if page.Indexing {
		t.Error("a settled index still reported a build in progress")
	}
	ids := map[string]threadtools.Hit{}
	for _, hit := range page.Rows {
		ids[hit.Thread.ID] = hit
	}
	if len(ids) != 3 {
		t.Fatalf("query hits = %#v", page.Rows)
	}
	if hit := ids[claudeThread.ID]; hit.ItemID == "" || !strings.Contains(strings.ToLower(hit.Snippet), "launcher") {
		t.Errorf("hit carries no item or snippet: %#v", hit)
	}

	// The provider filter has no index column; the adapter applies it.
	page, err = f.adapter.SearchThreads(ctx, threadtools.SearchQuery{Query: "launcher", Provider: string(provider.Codex), Limit: 10})
	if err != nil {
		t.Fatalf("SearchThreads(provider): %v", err)
	}
	if len(page.Rows) != 1 || page.Rows[0].Thread.ID != codexThread.ID {
		t.Fatalf("provider filter = %#v", page.Rows)
	}

	// So does archived, which a query includes by default.
	no := false
	page, err = f.adapter.SearchThreads(ctx, threadtools.SearchQuery{Query: "launcher", Archived: &no, Limit: 10})
	if err != nil {
		t.Fatalf("SearchThreads(archived): %v", err)
	}
	for _, hit := range page.Rows {
		if hit.Thread.ID == archived.ID {
			t.Fatalf("archived=false returned an archived thread: %#v", hit.Thread)
		}
	}

	// A query-less search is a listing by last activity, newest first.
	page, err = f.adapter.SearchThreads(ctx, threadtools.SearchQuery{Limit: 10})
	if err != nil {
		t.Fatalf("SearchThreads(listing): %v", err)
	}
	if len(page.Rows) < 2 || page.Rows[0].Thread.ID != codexThread.ID || page.Rows[1].Thread.ID != claudeThread.ID {
		t.Fatalf("listing order = %#v", page.Rows)
	}

	// Limit bounds the page and More says rows remain.
	page, err = f.adapter.SearchThreads(ctx, threadtools.SearchQuery{Limit: 1})
	if err != nil {
		t.Fatalf("SearchThreads(limit): %v", err)
	}
	if len(page.Rows) != 1 || !page.More {
		t.Fatalf("limited listing = %d rows, more=%t", len(page.Rows), page.More)
	}
	// Offset continues that listing rather than repeating its first row.
	next, err := f.adapter.SearchThreads(ctx, threadtools.SearchQuery{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("SearchThreads(offset): %v", err)
	}
	if len(next.Rows) != 1 || next.Rows[0].Thread.ID == page.Rows[0].Thread.ID {
		t.Fatalf("offset page = %#v", next.Rows)
	}

	// A query the index cannot parse is a public refusal, not a 500.
	if _, err := f.adapter.SearchThreads(ctx, threadtools.SearchQuery{Query: `launcher"`, Limit: 5}); publicCode(t, err) != threadtools.CodeInvalidRequest {
		t.Errorf("a malformed query did not refuse publicly: %v", err)
	}
}

// TestThreadToolsAdapterReportsIndexingWhileTheBuildIsOutstanding pins the
// partial-result flag every search carries.
func TestThreadToolsAdapterReportsIndexingWhileTheBuildIsOutstanding(t *testing.T) {
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "indexing-thread")
	f.turn(t, thread.ID, 0, 1_000, textItem("x0", "user_text", "hello"))
	// A fresh database carries the migration's progress row, which is
	// exactly the state a boot starts in.
	page, err := f.adapter.SearchThreads(t.Context(), threadtools.SearchQuery{Limit: 5})
	if err != nil {
		t.Fatalf("SearchThreads: %v", err)
	}
	if !page.Indexing {
		t.Fatal("a search run with the build outstanding did not report Indexing")
	}
	if err := f.app.store.BuildSearchIndex(t.Context()); err != nil {
		t.Fatalf("BuildSearchIndex: %v", err)
	}
	page, err = f.adapter.SearchThreads(t.Context(), threadtools.SearchQuery{Limit: 5})
	if err != nil {
		t.Fatalf("SearchThreads after build: %v", err)
	}
	if page.Indexing {
		t.Fatal("a finished build still reports Indexing")
	}
}

// TestThreadToolsAdapterExportsAWindowToAFile pins the file the export
// tools hand back: it exists, it holds the window, and its digest is the
// digest of its bytes.
func TestThreadToolsAdapterExportsAWindowToAFile(t *testing.T) {
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "export-thread")
	f.turn(t, thread.ID, 0, 1_000, textItem("e0", "user_text", "question"), textItem("e1", "assistant_text", "answer"))
	ctx := t.Context()

	bounds, err := f.adapter.ResolveWindow(ctx, threadtools.WindowQuery{ThreadID: thread.ID, Kind: threadtools.WindowAll})
	if err != nil {
		t.Fatalf("ResolveWindow: %v", err)
	}
	file, err := f.adapter.ExportTranscript(ctx, threadtools.ExportQuery{ThreadID: thread.ID, Bounds: bounds})
	if err != nil {
		t.Fatalf("ExportTranscript: %v", err)
	}
	data, err := os.ReadFile(file.Path)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if !strings.HasPrefix(file.Path, f.app.configDir) {
		t.Errorf("export landed outside the data directory: %s", file.Path)
	}
	if int64(len(data)) != file.Size {
		t.Errorf("size = %d, file is %d bytes", file.Size, len(data))
	}
	sum := sha256.Sum256(data)
	if file.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %s, want %s", file.SHA256, hex.EncodeToString(sum[:]))
	}
	text := string(data)
	if !strings.Contains(text, "question") || !strings.Contains(text, "answer") {
		t.Errorf("export omitted the window: %q", text)
	}
	info, err := os.Stat(file.Path)
	if err != nil {
		t.Fatalf("stat export: %v", err)
	}
	// A transcript is conversation content; the file is the owner's.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("export mode = %v, want 0600", perm)
	}
}

// TestThreadToolsAdapterAnswersTheCatalog pins thread_options against the
// app's real provider catalogs and project rows.
func TestThreadToolsAdapterAnswersTheCatalog(t *testing.T) {
	f := newThreadToolsFixture(t)
	f.thread(t, "catalog-thread", func(th *store.Thread) {
		th.WorktreePath = f.project.Path + "/wt-1"
		th.Branch = "topic"
	})
	if _, err := f.app.store.CreateThreadGroup(f.project.ID, "Batch"); err != nil {
		t.Fatalf("CreateThreadGroup: %v", err)
	}

	catalog, err := f.adapter.Catalog(t.Context(), threadtools.CatalogQuery{ProjectID: f.project.ID})
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if !catalog.Reachable || catalog.OS == "" {
		t.Fatalf("catalog = %#v", catalog)
	}
	if len(catalog.Providers) != 2 {
		t.Fatalf("providers = %#v", catalog.Providers)
	}
	claudeOption := catalog.Providers[0]
	if claudeOption.ID != string(provider.Claude) || len(claudeOption.Models) == 0 || claudeOption.DefaultModel == "" {
		t.Fatalf("claude option = %#v", claudeOption)
	}
	if claudeOption.Models[0].Source == "" {
		t.Errorf("model %s carries no catalog provenance", claudeOption.Models[0].Slug)
	}
	// The Codex catalog probe is disabled in this fixture, which is the
	// production degradation too: an installed provider whose model list
	// cannot be read stays in the answer with no models rather than
	// vanishing or failing the whole call.
	codexOption := catalog.Providers[1]
	if codexOption.ID != string(provider.Codex) || codexOption.Name == "" || len(codexOption.Models) != 0 {
		t.Fatalf("codex option = %#v", codexOption)
	}

	// One provider narrows the answer to that provider.
	narrowed, err := f.adapter.Catalog(t.Context(), threadtools.CatalogQuery{Provider: string(provider.Codex)})
	if err != nil {
		t.Fatalf("Catalog(provider): %v", err)
	}
	if len(narrowed.Providers) != 1 || narrowed.Providers[0].ID != string(provider.Codex) {
		t.Fatalf("narrowed providers = %#v", narrowed.Providers)
	}

	if len(catalog.Projects) != 1 {
		t.Fatalf("projects = %#v", catalog.Projects)
	}
	project := catalog.Projects[0]
	if project.ID != f.project.ID || project.Path != f.project.Path {
		t.Fatalf("project = %#v", project)
	}
	// The root plus the worktree a thread actually runs in.
	if len(project.Workspaces) != 2 || project.Workspaces[0].Path != f.project.Path {
		t.Fatalf("workspaces = %#v", project.Workspaces)
	}
	if !project.Workspaces[1].Worktree || project.Workspaces[1].Branch != "topic" {
		t.Fatalf("worktree workspace = %#v", project.Workspaces[1])
	}
	if len(project.Groups) != 1 || project.Groups[0].Name != "Batch" {
		t.Fatalf("groups = %#v", project.Groups)
	}
}

// TestThreadToolsAdapterHasNoPeersInThisBuild pins the single-computer
// shape: no pairings are offered, and a forwarded computer id is refused
// in prose the model can read.
func TestThreadToolsAdapterHasNoPeersInThisBuild(t *testing.T) {
	f := newThreadToolsFixture(t)
	computers, err := f.adapter.PairedComputers(t.Context())
	if err != nil || len(computers) != 0 {
		t.Fatalf("PairedComputers = %#v, %v", computers, err)
	}
	_, err = f.adapter.Peer(t.Context(), "some-computer")
	if code := publicCode(t, err); code != threadtools.CodeUnreachable {
		t.Fatalf("Peer code = %q, want %q", code, threadtools.CodeUnreachable)
	}
	if !strings.Contains(err.Error(), "some-computer") {
		t.Errorf("refusal does not name the computer: %v", err)
	}
}

// TestThreadToolsPositionsRoundTrip pins the packed timeline coordinate,
// including the negative item index a head-healed prompt persists at.
func TestThreadToolsPositionsRoundTrip(t *testing.T) {
	for _, tc := range []struct{ turn, item int }{{0, 0}, {3, 7}, {12, -1}, {0, -4}} {
		position := encodePosition(tc.turn, tc.item)
		if position <= 0 {
			t.Errorf("encodePosition(%d,%d) = %d, want a positive position", tc.turn, tc.item, position)
		}
		turn, item := decodePosition(position)
		if turn != tc.turn || item != tc.item {
			t.Errorf("decodePosition(encodePosition(%d,%d)) = (%d,%d)", tc.turn, tc.item, turn, item)
		}
		if position < turnFloor(tc.turn) || position > turnCeil(tc.turn) {
			t.Errorf("position %d falls outside turn %d's range", position, tc.turn)
		}
	}
	// Ordering is what the whole scheme exists for.
	if encodePosition(1, -3) <= encodePosition(0, 99) {
		t.Error("a later turn's head-healed row sorted before an earlier turn's tail")
	}
}

// TestThreadToolsWriteStubsRefuseInPublicProse pins the one shared refusal
// the phase-4 writes carry today: a documented code and prose that says
// the capability is not here yet rather than looking like a broken call.
func TestThreadToolsWriteStubsRefuseInPublicProse(t *testing.T) {
	err := threadToolsWriteUnavailable("starting a conversation")
	code, message, ok := errorsx.PublicDetails(err)
	if !ok || code != threadtools.CodeInvalidRequest {
		t.Fatalf("write refusal = %q (public %t)", code, ok)
	}
	if !strings.Contains(message, "starting a conversation") || !strings.Contains(message, "not available yet") {
		t.Fatalf("write refusal prose = %q", message)
	}
}

// TestThreadToolsAdapterProjectsLiveState pins the four facts the state
// derivation reads, taken from the live router rather than the row: an
// idle thread, a thread mid-turn, and a thread parked on an approval.
func TestThreadToolsAdapterProjectsLiveState(t *testing.T) {
	f := newThreadToolsFixture(t)
	f.app.triage = triage.NewRouter(f.app.store, func(eventchan.Channel, any) {})
	idle := f.thread(t, "live-idle")
	running := f.thread(t, "live-running")
	blocked := f.thread(t, "live-blocked")
	for _, thread := range []store.Thread{running, blocked} {
		f.app.sessionManager().put(thread.ID, session{Provider: thread.Provider, Token: "token-" + thread.ID})
	}
	ctx := t.Context()

	state, err := f.adapter.LiveState(ctx, idle.ID)
	if err != nil {
		t.Fatalf("LiveState(idle): %v", err)
	}
	if state != (threadtools.LiveState{}) {
		t.Fatalf("an idle thread = %#v, want the zero value", state)
	}

	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: running.ID, TurnID: "turn-1", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	state, err = f.adapter.LiveState(ctx, running.ID)
	if err != nil {
		t.Fatalf("LiveState(running): %v", err)
	}
	if !state.ActiveTurn || state.PendingApprovals != 0 {
		t.Fatalf("running = %#v", state)
	}
	if derived := threadtools.State(f.mustThread(t, running.ID), state); derived != threadtools.StateRunning {
		t.Errorf("derived state = %q, want %q", derived, threadtools.StateRunning)
	}

	approval, err := json.Marshal(provider.ApprovalRequest{RequestID: "approval-1", Kind: "permission"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventApprovalRequest, ThreadID: blocked.ID, ItemID: "approval-1",
		Meta: approval, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("approval request: %v", err)
	}
	state, err = f.adapter.LiveState(ctx, blocked.ID)
	if err != nil {
		t.Fatalf("LiveState(blocked): %v", err)
	}
	if state.PendingApprovals != 1 {
		t.Fatalf("blocked = %#v, want one pending approval", state)
	}
	if derived := threadtools.State(f.mustThread(t, blocked.ID), state); derived != threadtools.StatePendingApproval {
		t.Errorf("derived state = %q, want %q", derived, threadtools.StatePendingApproval)
	}

	// The search rows carry the same live projection, which is what the
	// state filter is applied to.
	page, err := f.adapter.SearchThreads(ctx, threadtools.SearchQuery{State: threadtools.StatePendingApproval, Limit: 10})
	if err != nil {
		t.Fatalf("SearchThreads(state): %v", err)
	}
	if len(page.Rows) != 1 || page.Rows[0].Thread.ID != blocked.ID {
		t.Fatalf("state filter = %#v", page.Rows)
	}
}

func (f *threadToolsFixture) mustThread(t *testing.T, threadID string) threadtools.Thread {
	t.Helper()
	thread, err := f.adapter.Thread(t.Context(), threadID)
	if err != nil {
		t.Fatalf("Thread(%s): %v", threadID, err)
	}
	return thread
}

// longThread writes a thread with `turns` settled turns of `perTurn`
// text rows each, through the same write paths a live session uses. It
// exists for the two reads whose old implementations grew with the
// thread: the transcript's turn walk and the `since` anchor's bounded
// recent-turn listing.
func (f *threadToolsFixture) longThread(t *testing.T, id string, turns, perTurn int) store.Thread {
	t.Helper()
	thread := f.thread(t, id)
	for turn := range turns {
		turnID := id + "-turn-" + strconv.Itoa(turn)
		startedAt := int64(10_000 + turn*10)
		if err := f.app.store.InsertTurn(store.Turn{
			TurnID: turnID, ThreadID: id, TurnIndex: turn, StartedAt: startedAt,
		}); err != nil {
			t.Fatalf("InsertTurn(%d): %v", turn, err)
		}
		for index := range perTurn {
			kind := "assistant_text"
			if index == 0 {
				kind = "user_text"
			}
			item := textItem(fmt.Sprintf("%s-%d-%d", id, turn, index), kind, "row")
			item.ThreadID, item.TurnIndex, item.ItemIndex = id, turn, index
			item.Status = "completed"
			if err := f.app.store.InsertItem(item); err != nil {
				t.Fatalf("InsertItem(%d,%d): %v", turn, index, err)
			}
		}
		if err := f.app.store.UpdateTurnCompleted(turnID, startedAt+5, "end_turn", "", "", ""); err != nil {
			t.Fatalf("UpdateTurnCompleted(%d): %v", turn, err)
		}
	}
	return thread
}

// TestThreadToolsAdapterReadsALongThreadInABoundedNumberOfQueries is the
// scaling contract of the read half: a window and a page over a thread of
// several thousand rows cost the same round trips as over a thread of
// four, because the store answers both with ranged queries instead of a
// walk the adapter drives.
func TestThreadToolsAdapterReadsALongThreadInABoundedNumberOfQueries(t *testing.T) {
	f := newThreadToolsFixture(t)
	long := f.longThread(t, "long-thread", 600, 6)
	short := f.thread(t, "short-thread")
	f.turn(t, short.ID, 0, 1_000, textItem("s0", "user_text", "ask"), textItem("s1", "assistant_text", "answer"))
	ctx := t.Context()

	measure := func(threadID string) (threadtools.TranscriptSlice, uint64) {
		t.Helper()
		before := f.app.store.ReadCount()
		bounds, err := f.adapter.ResolveWindow(ctx, threadtools.WindowQuery{ThreadID: threadID, Kind: threadtools.WindowAll})
		if err != nil {
			t.Fatalf("ResolveWindow(%s): %v", threadID, err)
		}
		page, err := f.adapter.Transcript(ctx, threadtools.TranscriptQuery{
			ThreadID: threadID, From: bounds.From, To: bounds.To, Limit: 50,
		})
		if err != nil {
			t.Fatalf("Transcript(%s): %v", threadID, err)
		}
		return page, f.app.store.ReadCount() - before
	}

	longPage, longReads := measure(long.ID)
	shortPage, shortReads := measure(short.ID)

	if len(longPage.Items) != 50 {
		t.Fatalf("long page = %d rows, want the whole limit", len(longPage.Items))
	}
	if longPage.Items[0].ID != "long-thread-0-0" || longPage.Items[49].ID != "long-thread-8-1" {
		t.Fatalf("long page runs %s..%s", longPage.Items[0].ID, longPage.Items[49].ID)
	}
	if longPage.HighWater != encodePosition(599, 5) {
		t.Errorf("highWater = %d, want the last row of the last turn", longPage.HighWater)
	}
	if len(shortPage.Items) != 2 {
		t.Fatalf("short page = %d rows", len(shortPage.Items))
	}
	// The same handful of statements either way. A per-turn walk would
	// have spent one query on each of the 600 turns.
	if longReads != shortReads {
		t.Fatalf("a 3,600-row thread cost %d store reads, a 2-row thread %d", longReads, shortReads)
	}
	if longReads > 12 {
		t.Errorf("one window plus one page cost %d store reads", longReads)
	}

	// A page from the far end costs the same, and continues exactly where
	// the caller pointed it.
	before := f.app.store.ReadCount()
	tail, err := f.adapter.Transcript(ctx, threadtools.TranscriptQuery{
		ThreadID: long.ID, From: encodePosition(598, 0), To: encodePosition(599, 5), Limit: 50,
	})
	if err != nil {
		t.Fatalf("Transcript(tail): %v", err)
	}
	if reads := f.app.store.ReadCount() - before; reads > 6 {
		t.Errorf("a tail page cost %d store reads", reads)
	}
	if len(tail.Items) != 12 || tail.Items[0].ID != "long-thread-598-0" {
		t.Fatalf("tail page = %d rows starting at %s", len(tail.Items), tail.Items[0].ID)
	}
}

// TestThreadToolsAdapterResolvesSinceBeyondTheTurnListingCap pins the
// `since` anchor on a thread with more turns than MaxTurns: the anchor is
// the first turn that started at or after the timestamp, wherever it sits
// in the thread's history.
func TestThreadToolsAdapterResolvesSinceBeyondTheTurnListingCap(t *testing.T) {
	f := newThreadToolsFixture(t)
	long := f.longThread(t, "since-thread", 600, 2)
	ctx := t.Context()

	// Turn 3 started at 10_030. The old anchor walked only the newest
	// MaxTurns turns, so a timestamp this early resolved to the oldest
	// turn that listing held (turn 100) instead.
	bounds, err := f.adapter.ResolveWindow(ctx, threadtools.WindowQuery{
		ThreadID: long.ID, Kind: threadtools.WindowSince, SinceUnixMs: 10_021,
	})
	if err != nil {
		t.Fatalf("ResolveWindow(since): %v", err)
	}
	if bounds.Empty || bounds.From != turnFloor(3) {
		t.Fatalf("since = %#v, want the window to start at turn 3", bounds)
	}
	if bounds.To != encodePosition(599, 1) {
		t.Errorf("since window ends at %d, want the last row", bounds.To)
	}

	page, err := f.adapter.Transcript(ctx, threadtools.TranscriptQuery{
		ThreadID: long.ID, From: bounds.From, To: bounds.To, Limit: 3,
	})
	if err != nil {
		t.Fatalf("Transcript(since): %v", err)
	}
	if len(page.Items) != 3 || page.Items[0].ID != "since-thread-3-0" {
		t.Fatalf("since page = %#v", page.Items)
	}

	// A timestamp past the newest turn is a quiet thread, not a window
	// over whatever the listing happened to hold.
	quiet, err := f.adapter.ResolveWindow(ctx, threadtools.WindowQuery{
		ThreadID: long.ID, Kind: threadtools.WindowSince, SinceUnixMs: 99_000,
	})
	if err != nil || !quiet.Empty {
		t.Fatalf("since the future = %#v, %v", quiet, err)
	}
	// A timestamp older than the whole thread keeps every turn.
	whole, err := f.adapter.ResolveWindow(ctx, threadtools.WindowQuery{
		ThreadID: long.ID, Kind: threadtools.WindowSince, SinceUnixMs: 1,
	})
	if err != nil {
		t.Fatalf("ResolveWindow(since the beginning): %v", err)
	}
	// The window starts at the thread's first row, not at the turn floor
	// no row sits on.
	if whole.Empty || whole.From != encodePosition(0, 0) {
		t.Fatalf("since the beginning = %#v", whole)
	}
}

// TestThreadToolsAdapterListingFiltersInTheStore pins the filters that
// moved into SQL on the query-less listing: provider, since, archived and
// spawned-by, plus the paging that counts the rows the caller received.
func TestThreadToolsAdapterListingFiltersInTheStore(t *testing.T) {
	f := newThreadToolsFixture(t)
	claude := f.thread(t, "list-claude")
	f.turn(t, claude.ID, 0, 5_000, textItem("l0", "user_text", "ask"))
	codex := f.thread(t, "list-codex", func(th *store.Thread) { th.Provider = string(provider.Codex) })
	f.turn(t, codex.ID, 0, 6_000, textItem("l1", "user_text", "ask"))
	old := f.thread(t, "list-old")
	f.turn(t, old.ID, 0, 1_000, textItem("l2", "user_text", "ask"))
	archived := f.thread(t, "list-archived")
	f.turn(t, archived.ID, 0, 7_000, textItem("l3", "user_text", "ask"))
	if _, _, err := f.app.store.ArchiveThread(archived.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	// A thread the claude thread spawned, through the request ledger the
	// spawned_by_me filter reads.
	spawned := f.thread(t, "list-spawned")
	f.turn(t, spawned.ID, 0, 4_000, textItem("l4", "user_text", "ask"))
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: "tok-spawn", CallerThreadID: claude.ID, Kind: store.ThreadRequestSpawn,
		TargetThreadID: spawned.ID, State: store.ThreadRequestAccepted, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}
	ctx := t.Context()

	rows := func(q threadtools.SearchQuery) []string {
		t.Helper()
		q.Limit = 10
		page, err := f.adapter.SearchThreads(ctx, q)
		if err != nil {
			t.Fatalf("SearchThreads(%#v): %v", q, err)
		}
		ids := make([]string, 0, len(page.Rows))
		for _, hit := range page.Rows {
			ids = append(ids, hit.Thread.ID)
		}
		return ids
	}

	no := false
	for _, tc := range []struct {
		name string
		q    threadtools.SearchQuery
		want string
	}{
		{"provider", threadtools.SearchQuery{Provider: string(provider.Codex)}, "list-codex"},
		{"since", threadtools.SearchQuery{SinceUnixMs: 5_010}, "list-archived,list-codex,list-claude"},
		{"not archived", threadtools.SearchQuery{Archived: &no}, "list-codex,list-claude,list-spawned,list-old"},
		{"spawned by me", threadtools.SearchQuery{SpawnedBy: claude.ID}, "list-spawned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(rows(tc.q), ","); got != tc.want {
				t.Errorf("listing = %s, want %s", got, tc.want)
			}
		})
	}

	// Paging counts returned rows, so the cursor threadtools mints out of
	// one page continues exactly where it stopped.
	first, err := f.adapter.SearchThreads(ctx, threadtools.SearchQuery{Provider: string(provider.Claude), Limit: 2})
	if err != nil {
		t.Fatalf("SearchThreads(page 1): %v", err)
	}
	if len(first.Rows) != 2 || !first.More {
		t.Fatalf("page 1 = %d rows, more=%t", len(first.Rows), first.More)
	}
	next, err := f.adapter.SearchThreads(ctx, threadtools.SearchQuery{
		Provider: string(provider.Claude), Limit: 2, Offset: len(first.Rows),
	})
	if err != nil {
		t.Fatalf("SearchThreads(page 2): %v", err)
	}
	for _, seen := range first.Rows {
		for _, row := range next.Rows {
			if row.Thread.ID == seen.Thread.ID {
				t.Fatalf("page 2 repeats %s", row.Thread.ID)
			}
		}
	}
}
