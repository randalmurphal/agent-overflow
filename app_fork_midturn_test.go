package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// midTurnForkFixture is the on-disk half of a mid-turn Claude fork: a
// fake ~/.claude/projects layout with one session transcript in it.
// Mirrors the layout TestForkThreadClaudeAtTurnSlicesSessionJSONL
// builds by hand; nothing here reaches the developer's real home.
type midTurnForkFixture struct {
	home       string
	workspace  string
	projectDir string
	sessionID  string
	jsonlPath  string
}

func newMidTurnForkFixture(t *testing.T, sessionID, jsonl string) midTurnForkFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "ws")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	abs, err := filepath.Abs(canonical)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	slug := "-" + strings.ReplaceAll(filepath.ToSlash(abs)[1:], "/", "-")
	projectDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	fixture := midTurnForkFixture{
		home:       home,
		workspace:  workspace,
		projectDir: projectDir,
		sessionID:  sessionID,
		jsonlPath:  filepath.Join(projectDir, sessionID+".jsonl"),
	}
	if jsonl != "" {
		if err := os.WriteFile(fixture.jsonlPath, []byte(jsonl), 0o600); err != nil {
			t.Fatalf("write source jsonl: %v", err)
		}
	}
	return fixture
}

// attachLiveClaudeSession registers a live Claude session for the thread
// whose canonical leaf is `leaf`. The leaf tracker seeds from
// Config.ResumeAt, so the session reports a deterministic leaf without
// any wire traffic — and the mock binary never spawns a real CLI.
func attachLiveClaudeSession(t *testing.T, app *App, threadID, workspace, sessionID, leaf string) {
	t.Helper()
	sess, err := claude.NewSession(
		context.Background(),
		threadID,
		claude.Config{
			Binary:   writeClaudeControlPassthroughBinary(t),
			WorkDir:  workspace,
			Resume:   sessionID,
			ResumeAt: leaf,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if got := sess.CanonicalLeafUUID(); got != leaf {
		t.Fatalf("live session leaf = %q, want %q", got, leaf)
	}
	app.mu.Lock()
	app.sessions[threadID] = session{provider: string(provider.Claude), token: "mid-turn-fork", claude: sess}
	app.mu.Unlock()
}

// openTurn inserts an in-flight turn row (completed_at NULL) — what
// GetActiveTurn reads as "a turn is running right now".
func openTurn(t *testing.T, st *store.Store, threadID, turnID string, turnIndex int) {
	t.Helper()
	if err := st.InsertTurn(store.Turn{
		TurnID:    turnID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn(%s): %v", turnID, err)
	}
}

func itemByID(t *testing.T, items []store.Item, id string) store.Item {
	t.Helper()
	for _, it := range items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("item %q not found in %d rows", id, len(items))
	return store.Item{}
}

func itemBySummaryPrefix(t *testing.T, items []store.Item, prefix string) store.Item {
	t.Helper()
	for _, it := range items {
		if strings.HasPrefix(it.Summary, prefix) {
			return it
		}
	}
	t.Fatalf("no item with summary prefix %q in %d rows", prefix, len(items))
	return store.Item{}
}

const midTurnSourceJSONL = `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"mid-turn-session","message":{"role":"user","content":"first prompt"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"mid-turn-session","message":{"id":"m0","role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"mid-turn-session","message":{"role":"user","content":"second prompt"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"mid-turn-session","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"partial rep"}]}}
`

// seedMidTurnSourceRows lays down the SQLite half of a source thread
// caught mid-turn: a settled turn 0, then an open turn 1 carrying a
// streaming assistant_text, a running foreground tool_call, and a
// running BACKGROUND tool_call (the one the clone must drop entirely).
func seedMidTurnSourceRows(t *testing.T, st *store.Store, threadID string) {
	t.Helper()
	now := time.Now().UnixMilli()
	insertUserItemWithMeta(t, st, threadID, "src-u0", 0, "first prompt", `{"provider_item_id":"u0"}`)
	insertAssistantTextItem(t, st, threadID, "src-a0", 0, "reply 0")
	if err := st.InsertTurn(store.Turn{TurnID: threadID + ":0", ThreadID: threadID, TurnIndex: 0, StartedAt: now}); err != nil {
		t.Fatalf("InsertTurn(settled): %v", err)
	}
	if err := st.UpdateTurnCompleted(threadID+":0", now+1, "end_turn", "m0", "", ""); err != nil {
		t.Fatalf("UpdateTurnCompleted: %v", err)
	}

	insertUserItemWithMeta(t, st, threadID, "src-u1", 1, "second prompt", `{"provider_item_id":"u1"}`)
	rows := []store.Item{
		{
			ID: "src-stream", ThreadID: threadID, TurnIndex: 1, ItemIndex: 1,
			Kind: "assistant_text", Role: "assistant", Status: "streaming",
			Summary: "partial rep", CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "src-fg-tool", ThreadID: threadID, TurnIndex: 1, ItemIndex: 2,
			Kind: "tool_call", Role: "assistant", Status: "running",
			ToolName: "Read", Summary: "Read: /tmp/x", CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "src-bg-tool", ThreadID: threadID, TurnIndex: 1, ItemIndex: 3,
			Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: true,
			ToolName: "Bash", Summary: "Bash: sleep 600", CreatedAt: now, UpdatedAt: now,
		},
	}
	for _, it := range rows {
		if err := st.InsertItem(it); err != nil {
			t.Fatalf("InsertItem(%s): %v", it.ID, err)
		}
	}
	openTurn(t, st, threadID, threadID+":1", 1)
}

// TestForkThreadClaudeMidTurnTailSlicesAtLiveLeaf is the headline case:
// forking a Claude thread WHILE a turn streams. The fork is a snapshot
// "as if interrupted right now" — eager JSONL slice at the live
// session's canonical leaf, cloned rows settled with the standard
// interrupted treatment — and the SOURCE is left completely alone.
func TestForkThreadClaudeMidTurnTailSlicesAtLiveLeaf(t *testing.T) {
	app := newTestAppWithStore(t)
	fixture := newMidTurnForkFixture(t, "mid-turn-session", midTurnSourceJSONL)

	source := testThread("thread-claude-midturn")
	source.Provider = string(provider.Claude)
	source.SessionRef = fixture.sessionID
	source.WorkspacePath = fixture.workspace
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	seedMidTurnSourceRows(t, app.store, source.ID)
	attachLiveClaudeSession(t, app, source.ID, fixture.workspace, fixture.sessionID, "a1")

	forked, err := app.ForkThread(source.ID, nil)
	if err != nil {
		t.Fatalf("ForkThread(mid-turn tail): %v", err)
	}

	// Eager slice, never the lazy --fork-session path.
	if forked.PendingForkRef != "" {
		t.Errorf("fork PendingForkRef = %q, want empty (mid-turn must never take the lazy path)", forked.PendingForkRef)
	}
	if forked.SessionRef == "" || forked.SessionRef == fixture.sessionID {
		t.Fatalf("fork SessionRef = %q, want a fresh sliced session id", forked.SessionRef)
	}
	if _, err := os.Stat(filepath.Join(fixture.projectDir, forked.SessionRef+".jsonl")); err != nil {
		t.Errorf("sliced transcript missing: %v", err)
	}

	forkItems, err := app.store.ListItems(forked.ID)
	if err != nil {
		t.Fatalf("ListItems(fork): %v", err)
	}
	for _, it := range forkItems {
		if it.IsBackground && it.Status == "running" {
			t.Errorf("running background row cloned into the fork: %+v", it)
		}
		if it.Summary == "Bash: sleep 600" {
			t.Errorf("the running background launch was cloned: %+v", it)
		}
	}

	stream := itemBySummaryPrefix(t, forkItems, "partial rep")
	if stream.Status != "errored" {
		t.Errorf("cloned streaming row status = %q, want errored", stream.Status)
	}
	if !strings.HasSuffix(stream.Summary, " — interrupted") {
		t.Errorf("cloned streaming row summary = %q, want the interrupted suffix", stream.Summary)
	}
	tool := itemBySummaryPrefix(t, forkItems, "Read: /tmp/x")
	if tool.Status != "errored" || !strings.HasSuffix(tool.Summary, " — interrupted") {
		t.Errorf("cloned running tool_call = %q/%q, want errored + interrupted suffix", tool.Status, tool.Summary)
	}

	// The fork's turn row closes as interrupted; the settled turn 0 is
	// carried over verbatim.
	if _, active, err := app.store.GetActiveTurn(forked.ID); err != nil {
		t.Fatalf("GetActiveTurn(fork): %v", err)
	} else if active {
		t.Error("fork still has an active turn — the settle did not close it")
	}
	forkTurn, found, err := app.store.GetTurnByThreadIndex(forked.ID, 1)
	if err != nil || !found {
		t.Fatalf("GetTurnByThreadIndex(fork, 1) = %v, %v", found, err)
	}
	if forkTurn.CompletedAt == nil || forkTurn.StopReason != "interrupted" {
		t.Errorf("fork turn 1 = completedAt %v / stopReason %q, want closed + interrupted", forkTurn.CompletedAt, forkTurn.StopReason)
	}

	// UUID remap: the cloned user rows must point at the SLICE's reminted
	// uuids, not the source transcript's.
	forkUser := itemBySummaryPrefix(t, forkItems, "second prompt")
	if got := usermessage.ReadProviderItemID(forkUser.Meta); got == "" || got == "u1" {
		t.Errorf("cloned user row provider_item_id = %q, want a reminted uuid", got)
	}

	// The SOURCE is untouched: rows still running, turn still open, and
	// the transcript byte-stable.
	sourceItems, err := app.store.ListItems(source.ID)
	if err != nil {
		t.Fatalf("ListItems(source): %v", err)
	}
	if got := itemByID(t, sourceItems, "src-stream"); got.Status != "streaming" || got.Summary != "partial rep" {
		t.Errorf("source streaming row mutated: %q/%q", got.Status, got.Summary)
	}
	if got := itemByID(t, sourceItems, "src-fg-tool"); got.Status != "running" {
		t.Errorf("source running tool_call mutated: %q", got.Status)
	}
	if got := itemByID(t, sourceItems, "src-bg-tool"); got.Status != "running" {
		t.Errorf("source background launch mutated: %q", got.Status)
	}
	if _, active, err := app.store.GetActiveTurn(source.ID); err != nil {
		t.Fatalf("GetActiveTurn(source): %v", err)
	} else if !active {
		t.Error("source turn was settled by the fork — the source must keep streaming")
	}
	after, err := os.ReadFile(fixture.jsonlPath)
	if err != nil {
		t.Fatalf("re-read source transcript: %v", err)
	}
	if string(after) != midTurnSourceJSONL {
		t.Error("source transcript mutated by the fork")
	}
}

// TestForkThreadClaudeMidTurnFallsBackToOnDiskLeaf covers the write
// ordering race: the live session announced a leaf on stdout that the
// CLI has not appended to the transcript yet. The fork must not fail —
// it slices through the deepest leaf that IS on disk.
func TestForkThreadClaudeMidTurnFallsBackToOnDiskLeaf(t *testing.T) {
	app := newTestAppWithStore(t)
	fixture := newMidTurnForkFixture(t, "mid-turn-session", midTurnSourceJSONL)

	source := testThread("thread-claude-midturn-race")
	source.Provider = string(provider.Claude)
	source.SessionRef = fixture.sessionID
	source.WorkspacePath = fixture.workspace
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	seedMidTurnSourceRows(t, app.store, source.ID)
	// "a2" is a uuid the wire has produced but the file has not.
	attachLiveClaudeSession(t, app, source.ID, fixture.workspace, fixture.sessionID, "a2")

	start := time.Now()
	forked, err := app.ForkThread(source.ID, nil)
	if err != nil {
		t.Fatalf("ForkThread(mid-turn, leaf not on disk): %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("mid-turn fork took %s — the not-yet-flushed retry is unbounded", elapsed)
	}
	if forked.SessionRef == "" || forked.SessionRef == fixture.sessionID {
		t.Fatalf("fork SessionRef = %q, want a fresh sliced session id", forked.SessionRef)
	}
	if forked.PendingForkRef != "" {
		t.Errorf("fork PendingForkRef = %q, want empty", forked.PendingForkRef)
	}

	// The slice landed at the on-disk leaf (a1): the whole file survives,
	// with every uuid reminted and nothing invented for the missing a2.
	sliced, err := os.ReadFile(filepath.Join(fixture.projectDir, forked.SessionRef+".jsonl"))
	if err != nil {
		t.Fatalf("read sliced transcript: %v", err)
	}
	for _, want := range []string{"first prompt", "reply 0", "second prompt", "partial rep"} {
		if !strings.Contains(string(sliced), want) {
			t.Errorf("sliced transcript is missing %q — the cut did not reach the on-disk leaf", want)
		}
	}
	if strings.Contains(string(sliced), `"a2"`) {
		t.Error("sliced transcript references the never-flushed leaf a2")
	}
	if strings.Contains(string(sliced), `"uuid":"a1"`) {
		t.Error("sliced transcript kept a source uuid verbatim — the slice must remint every uuid")
	}
}

// TestForkThreadClaudeMidTurnWithoutSessionFileStartsFresh pins the
// sanctioned degenerate case: fork seconds after a send, before the CLI
// has written any transcript at all. The fork holds just the prompt and
// starts a fresh provider thread on its first send.
func TestForkThreadClaudeMidTurnWithoutSessionFileStartsFresh(t *testing.T) {
	app := newTestAppWithStore(t)
	fixture := newMidTurnForkFixture(t, "never-written-session", "")

	source := testThread("thread-claude-midturn-degenerate")
	source.Provider = string(provider.Claude)
	source.SessionRef = fixture.sessionID
	source.WorkspacePath = fixture.workspace
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "src-u0", 0, "just sent this", "")
	if err := app.store.InsertItem(store.Item{
		ID: "src-stream", ThreadID: source.ID, TurnIndex: 0, ItemIndex: 1,
		Kind: "assistant_text", Role: "assistant", Status: "streaming",
		Summary: "", CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}
	openTurn(t, app.store, source.ID, source.ID+":0", 0)

	forked, err := app.ForkThread(source.ID, nil)
	if err != nil {
		t.Fatalf("ForkThread(degenerate mid-turn): %v", err)
	}
	if forked.SessionRef != "" || forked.PendingForkRef != "" {
		t.Errorf("fork refs = %q / %q, want both empty (fresh provider thread)", forked.SessionRef, forked.PendingForkRef)
	}

	forkItems, err := app.store.ListItems(forked.ID)
	if err != nil {
		t.Fatalf("ListItems(fork): %v", err)
	}
	if len(forkItems) != 2 {
		t.Fatalf("fork items = %d, want 2 (the prompt + the settled stream row)", len(forkItems))
	}
	if got := itemBySummaryPrefix(t, forkItems, "just sent this"); got.Status != "completed" {
		t.Errorf("cloned prompt status = %q, want completed", got.Status)
	}
	stream := itemByID(t, forkItems, forkItems[1].ID)
	if stream.Status != "errored" || stream.Summary != "Interrupted" {
		t.Errorf("cloned empty stream row = %q/%q, want errored/Interrupted", stream.Status, stream.Summary)
	}
	turn, found, err := app.store.GetTurnByThreadIndex(forked.ID, 0)
	if err != nil || !found {
		t.Fatalf("GetTurnByThreadIndex(fork, 0) = %v, %v", found, err)
	}
	if turn.CompletedAt == nil || turn.StopReason != "interrupted" {
		t.Errorf("fork turn = %v/%q, want closed + interrupted", turn.CompletedAt, turn.StopReason)
	}
}

// TestForkThreadClaudeMidTurnAtActiveTurnBehavesAsTail: "keep through
// the running turn" IS the mid-turn tail fork. It must take the eager
// slice, never the lazy PendingForkRef shortcut that an at-or-past-tail
// anchor takes on an idle thread.
func TestForkThreadClaudeMidTurnAtActiveTurnBehavesAsTail(t *testing.T) {
	app := newTestAppWithStore(t)
	fixture := newMidTurnForkFixture(t, "mid-turn-session", midTurnSourceJSONL)

	source := testThread("thread-claude-midturn-anchored")
	source.Provider = string(provider.Claude)
	source.SessionRef = fixture.sessionID
	source.WorkspacePath = fixture.workspace
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	seedMidTurnSourceRows(t, app.store, source.ID)
	attachLiveClaudeSession(t, app, source.ID, fixture.workspace, fixture.sessionID, "a1")

	atTurn := 1 // the ACTIVE turn
	forked, err := app.ForkThread(source.ID, &atTurn)
	if err != nil {
		t.Fatalf("ForkThread(at the active turn): %v", err)
	}
	if forked.PendingForkRef != "" {
		t.Errorf("fork PendingForkRef = %q, want empty — an anchor at the active turn must slice, not defer", forked.PendingForkRef)
	}
	if forked.SessionRef == "" || forked.SessionRef == fixture.sessionID {
		t.Fatalf("fork SessionRef = %q, want a fresh sliced session id", forked.SessionRef)
	}
	forkItems, err := app.store.ListItems(forked.ID)
	if err != nil {
		t.Fatalf("ListItems(fork): %v", err)
	}
	if got := itemBySummaryPrefix(t, forkItems, "Read: /tmp/x"); got.Status != "errored" {
		t.Errorf("cloned running tool_call status = %q, want errored", got.Status)
	}
}

// TestForkThreadCodexMidTurnTailForksWithNoBoundary pins the Codex
// half: a mid-turn tail fork issues `thread/fork` with NO lastTurnId.
// codex then copies persisted history and appends the same
// turn-aborted marker a real interrupt writes — onto the fork's copy
// only. A lastTurnId naming the in-progress turn would be rejected.
func TestForkThreadCodexMidTurnTailForksWithNoBoundary(t *testing.T) {
	app := newTestAppWithStore(t)
	requestLog := filepath.Join(t.TempDir(), "fork-requests.ndjson")
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"codexBinaryPath": writeCodexForkAtBinary(t, codexForkMock{
			resumedThreadID: "resume-provider-thread",
			forkedThreadID:  "fork-provider-thread",
			requestLogPath:  requestLog,
		}),
	}); err != nil {
		t.Fatalf("settings Update: %v", err)
	}

	source := testThread("thread-codex-midturn")
	source.Provider = string(provider.Codex)
	source.SessionRef = "resume-provider-thread"
	source.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "user:0", 0, "first", `{"provider_item_id":"provider-user-0"}`)
	insertCodexTurn(t, app.store, source.ID, 0, "turn-0")
	insertUserItemWithMeta(t, app.store, source.ID, "user:1", 1, "second", `{"provider_item_id":"provider-user-1"}`)
	if err := app.store.InsertItem(store.Item{
		ID: "codex-running", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 1,
		Kind: "tool_call", Role: "assistant", Status: "running",
		ToolName: "shell", Summary: "Ran: ls", CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}
	// The open turn carries a provider turn id — InsertTurn stamps one at
	// insert — which is precisely the anchor codex would refuse.
	if err := app.store.InsertTurn(store.Turn{
		TurnID:         source.ID + ":1",
		ProviderTurnID: "turn-1",
		ThreadID:       source.ID,
		TurnIndex:      1,
		StartedAt:      time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn(open): %v", err)
	}

	forked, err := app.ForkThread(source.ID, nil)
	if err != nil {
		t.Fatalf("ForkThread(codex mid-turn): %v", err)
	}
	if forked.SessionRef != "fork-provider-thread" {
		t.Fatalf("fork SessionRef = %q, want the forked provider thread id", forked.SessionRef)
	}
	request := readCodexForkRequest(t, requestLog)
	if strings.Contains(request, "lastTurnId") {
		t.Errorf("mid-turn thread/fork carried a boundary: %s", request)
	}

	// And the settle still ran over the clone.
	forkItems, err := app.store.ListItems(forked.ID)
	if err != nil {
		t.Fatalf("ListItems(fork): %v", err)
	}
	if got := itemBySummaryPrefix(t, forkItems, "Ran: ls"); got.Status != "errored" {
		t.Errorf("cloned running codex tool_call status = %q, want errored", got.Status)
	}
}

// TestForkThreadCodexAnchoredForkRefusesTheInFlightTurn is the
// defensive assertion behind the tail normalization: nothing may hand
// forkCodexThread an anchor at or above the running turn, because codex
// rejects such a lastTurnId and the fork would end up with provider
// history that disagrees with its cloned items.
func TestForkThreadCodexAnchoredForkRefusesTheInFlightTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"codexBinaryPath": writeCodexForkBinary(t, "resume-provider-thread", "fork-provider-thread"),
	}); err != nil {
		t.Fatalf("settings Update: %v", err)
	}

	source := testThread("thread-codex-anchored-inflight")
	source.Provider = string(provider.Codex)
	source.SessionRef = "resume-provider-thread"
	source.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "user:0", 0, "first", `{"provider_item_id":"provider-user-0"}`)
	insertCodexTurn(t, app.store, source.ID, 0, "turn-0")
	if err := app.store.InsertTurn(store.Turn{
		TurnID: source.ID + ":1", ProviderTurnID: "turn-1", ThreadID: source.ID,
		TurnIndex: 1, StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn(open): %v", err)
	}

	atTurn := 1
	if _, err := app.forkCodexThread(source, &atTurn); err == nil ||
		!strings.Contains(err.Error(), "in-flight turn") {
		t.Fatalf("forkCodexThread(anchor = in-flight turn) = %v, want a refusal", err)
	}
}

// TestForkThreadFromMessageDuringActiveTurn: the message-keyed fork is
// no longer refused mid-turn either. Its anchor is strictly below the
// in-flight turn on the provider side, but the cloned prefix can still
// hold running rows, so the fork settles the same way.
func TestForkThreadFromMessageDuringActiveTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	fixture := newMidTurnForkFixture(t, "mid-turn-session", midTurnSourceJSONL)

	source := testThread("thread-message-fork-midturn")
	source.Provider = string(provider.Claude)
	source.SessionRef = fixture.sessionID
	source.WorkspacePath = fixture.workspace
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	seedMidTurnSourceRows(t, app.store, source.ID)
	attachLiveClaudeSession(t, app, source.ID, fixture.workspace, fixture.sessionID, "a1")

	// Fork from the mid-turn prompt: turn 0 survives whole, turn 1's
	// prefix (nothing before the prompt) does not.
	forked, err := app.ForkThreadFromMessage(source.ID, "src-u1")
	if err != nil {
		t.Fatalf("ForkThreadFromMessage(mid-turn): %v", err)
	}
	forkItems, err := app.store.ListItems(forked.ID)
	if err != nil {
		t.Fatalf("ListItems(fork): %v", err)
	}
	for _, it := range forkItems {
		if it.TurnIndex > 0 {
			t.Errorf("fork kept a row at turn %d — the anchor turn must not survive: %+v", it.TurnIndex, it)
		}
		if it.Status == "running" || it.Status == "streaming" {
			t.Errorf("fork carries an unsettled row: %+v", it)
		}
	}
	if _, active, err := app.store.GetActiveTurn(forked.ID); err != nil {
		t.Fatalf("GetActiveTurn(fork): %v", err)
	} else if active {
		t.Error("fork has an active turn after a mid-turn message fork")
	}

	// The source keeps streaming.
	if _, active, err := app.store.GetActiveTurn(source.ID); err != nil {
		t.Fatalf("GetActiveTurn(source): %v", err)
	} else if !active {
		t.Error("source turn was settled by the message fork")
	}
	if got := itemByID(t, mustListItems(t, app.store, source.ID), "src-stream"); got.Status != "streaming" {
		t.Errorf("source streaming row = %q, want untouched", got.Status)
	}
}

func mustListItems(t *testing.T, st *store.Store, threadID string) []store.Item {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems(%s): %v", threadID, err)
	}
	return items
}

// TestForkThreadMidTurnAnchorOnAnItemlessActiveTurnIsATailFork pins BOTH
// sides of the mid-turn anchor normalization.
//
// A just-started turn has no items yet, so an anchor naming it sits
// above the last turn that HAS items — but `LastTurnIndex` is MAX over
// items ∪ turns, so the range check still sees it. An anchor EQUAL to
// the in-flight turn normalizes to a tail fork (it means "keep through
// the running turn"); an anchor ABOVE it is out of range and must be
// refused exactly as it is on an idle thread, rather than silently
// becoming a tail fork because a turn happened to be running.
func TestForkThreadMidTurnAnchorOnAnItemlessActiveTurnIsATailFork(t *testing.T) {
	app := newTestAppWithStore(t)
	fixture := newMidTurnForkFixture(t, "mid-turn-session", midTurnSourceJSONL)

	source := testThread("thread-claude-midturn-itemless")
	source.Provider = string(provider.Claude)
	source.SessionRef = fixture.sessionID
	source.WorkspacePath = fixture.workspace
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "src-u0", 0, "first prompt", `{"provider_item_id":"u0"}`)
	insertAssistantTextItem(t, app.store, source.ID, "src-a0", 0, "reply 0")
	// Turn 1 exists but has produced nothing yet.
	openTurn(t, app.store, source.ID, source.ID+":1", 1)
	attachLiveClaudeSession(t, app, source.ID, fixture.workspace, fixture.sessionID, "a0")

	// Above the in-flight turn: refused, same message as the idle path.
	overshoot := 2
	if _, err := app.ForkThread(source.ID, &overshoot); err == nil ||
		!strings.Contains(err.Error(), "exceeds source last turn") {
		t.Fatalf("ForkThread(anchor above the in-flight turn) = %v, want an out-of-range refusal", err)
	}

	// Exactly the in-flight turn: a tail fork.
	atTurn := 1
	forked, err := app.ForkThread(source.ID, &atTurn)
	if err != nil {
		t.Fatalf("ForkThread(anchor on the itemless active turn): %v", err)
	}
	if forked.SessionRef == "" || forked.PendingForkRef != "" {
		t.Errorf("fork refs = %q / %q, want a sliced session ref and no lazy cursor", forked.SessionRef, forked.PendingForkRef)
	}
	if _, active, err := app.store.GetActiveTurn(forked.ID); err != nil {
		t.Fatalf("GetActiveTurn(fork): %v", err)
	} else if active {
		t.Error("fork inherited the open turn row")
	}
}

// TestForkThreadClaudeMidTurnColdScanIOFailureFailsTheFork pins the
// boundary between the sanctioned degenerate answer and a real fault.
// "No leaf yet" is an early state and yields a fresh provider thread;
// a transcript that cannot be READ is an I/O failure, and laundering it
// into a context-less fork with a fully-populated timeline would hand
// the user a thread that silently lost its history. The fork must fail
// and leave no rows behind.
func TestForkThreadClaudeMidTurnColdScanIOFailureFailsTheFork(t *testing.T) {
	app := newTestAppWithStore(t)
	fixture := newMidTurnForkFixture(t, "mid-turn-session", midTurnSourceJSONL)

	// No live session is attached, so the cut resolves through the cold
	// scan — which opens the file.
	if err := os.Chmod(fixture.jsonlPath, 0o000); err != nil {
		t.Fatalf("chmod source transcript: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(fixture.jsonlPath, 0o600) })
	if _, err := os.ReadFile(fixture.jsonlPath); err == nil {
		t.Skip("running with permission to read a 0000 file (root?) — cannot stage an unreadable transcript")
	}

	source := testThread("thread-claude-midturn-ioerr")
	source.Provider = string(provider.Claude)
	source.SessionRef = fixture.sessionID
	source.WorkspacePath = fixture.workspace
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	seedMidTurnSourceRows(t, app.store, source.ID)

	before, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads(before): %v", err)
	}

	if _, err := app.ForkThread(source.ID, nil); err == nil {
		t.Fatal("ForkThread(unreadable transcript) succeeded — a read fault must fail the fork, not start a fresh thread")
	} else if !strings.Contains(err.Error(), "scan claude session leaf") {
		t.Fatalf("ForkThread error = %v, want the cold-scan failure", err)
	}

	after, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads(after): %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("thread count %d -> %d: a half-built fork row survived the failure", len(before), len(after))
	}
}
