package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	attachmentstore "agent-overflow/internal/attachment"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

func TestForkThreadClaudePersistsPendingForkStateAndClonesTimeline(t *testing.T) {
	app := newTestAppWithStore(t)

	source := testThread("thread-claude-fork-source")
	source.Provider = string(provider.Claude)
	source.SessionRef = "claude-session-123"
	fixture := newMidTurnForkFixture(t, "claude-session-123", midTurnSourceJSONL)
	source.WorkspacePath = fixture.workspace
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	forked, err := app.ForkThread(t.Context(), source.ID, nil)
	if err != nil {
		t.Fatalf("ForkThread() error = %v", err)
	}

	if forked.Title != source.Title+" (fork)" {
		t.Fatalf("fork title = %q, want %q", forked.Title, source.Title+" (fork)")
	}
	if forked.SessionRef != "" {
		t.Fatalf("fork session ref = %q, want empty for Claude deferred fork", forked.SessionRef)
	}
	if forked.PendingForkRef != source.SessionRef {
		t.Fatalf("fork pending ref = %q, want %q", forked.PendingForkRef, source.SessionRef)
	}
	if forked.ForkedFromThreadID != source.ID {
		t.Fatalf("forkedFromThreadId = %q, want %q", forked.ForkedFromThreadID, source.ID)
	}

	items, err := app.store.ListItems(forked.ID)
	if err != nil {
		t.Fatalf("ListItems() error = %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("len(fork items) = %d, want the two messages and the divider", len(items))
	}
	for _, it := range items {
		if it.ThreadID != forked.ID {
			t.Fatalf("forked item %s thread ID = %q, want %q", it.ID, it.ThreadID, forked.ID)
		}
	}
	if items[0].Summary != "first message" || items[1].Summary != "assistant reply" {
		t.Fatalf("forked item summaries = %q / %q", items[0].Summary, items[1].Summary)
	}
	divider := items[2]
	var origin struct {
		SourceThreadID string `json:"sourceThreadId"`
		SourceTitle    string `json:"sourceTitle"`
		SourceItemID   string `json:"sourceItemId"`
	}
	if err := json.Unmarshal([]byte(divider.Meta), &origin); err != nil {
		t.Fatalf("divider meta %q: %v", divider.Meta, err)
	}
	if divider.ToolName != forkDividerToolName || divider.Summary != "Forked from "+source.Title ||
		origin.SourceThreadID != source.ID || origin.SourceTitle != source.Title || origin.SourceItemID != items[1].ID {
		t.Fatalf("divider = %+v origin=%+v", divider, origin)
	}
}

// forkDividerToolName marks the row a pointer fork shows at its cut
// (store.CreatePointerFork); the frontend renders it as the fork divider.
const forkDividerToolName = "fork_origin"

// withoutForkDividers drops the divider rows a fork shows at its cut and at
// each ancestor fork's, leaving the conversation.
func withoutForkDividers(items []store.Item) []store.Item {
	out := make([]store.Item, 0, len(items))
	for _, it := range items {
		if it.ToolName != forkDividerToolName {
			out = append(out, it)
		}
	}
	return out
}

// forkConversationItems is ListItems without the fork dividers.
func forkConversationItems(s *store.Store, threadID string) ([]store.Item, error) {
	items, err := s.ListItems(threadID)
	return withoutForkDividers(items), err
}

func TestForkThreadCodexUsesStoredResumeStateWhenSessionInactive(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"codexBinaryPath": writeCodexForkBinary(t, "resume-provider-thread", "fork-provider-thread", ""),
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	source := testThread("thread-codex-fork-source")
	source.Provider = string(provider.Codex)
	source.SessionRef = "resume-provider-thread"
	source.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	forked, err := app.ForkThread(t.Context(), source.ID, nil)
	if err != nil {
		t.Fatalf("ForkThread() error = %v", err)
	}

	if forked.SessionRef != "fork-provider-thread" {
		t.Fatalf("fork session ref = %q, want %q", forked.SessionRef, "fork-provider-thread")
	}
	if forked.PendingForkRef != "" {
		t.Fatalf("fork pending ref = %q, want empty", forked.PendingForkRef)
	}
	if forked.ForkedFromThreadID != source.ID {
		t.Fatalf("forkedFromThreadId = %q, want %q", forked.ForkedFromThreadID, source.ID)
	}
}

func TestForkThreadRejectsThreadsWithoutMessages(t *testing.T) {
	app := newTestAppWithStore(t)

	source := testThread("thread-empty-fork-source")
	source.Provider = string(provider.Claude)
	source.SessionRef = "claude-session-123"
	fixture := newMidTurnForkFixture(t, "claude-session-123", midTurnSourceJSONL)
	source.WorkspacePath = fixture.workspace
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	_, err := app.ForkThread(t.Context(), source.ID, nil)
	if err == nil {
		t.Fatal("ForkThread() error = nil, want empty-thread failure")
	}
	if got := err.Error(); got != `fork thread: thread "thread-empty-fork-source" has no messages and cannot be forked` {
		t.Fatalf("ForkThread() error = %q", got)
	}
}

// TestForkThreadCodexCutsOutsideTheLiveSession: a `thread/fork` loads
// the CHILD into the app-server that answered, where it stays until that
// process exits. Cut on the source's live session, the fork's first send
// would find its thread "open in another Codex process". So the cut runs
// on a throwaway process even when the source is live, and the live
// session never sees a fork request.
func TestForkThreadCodexCutsOutsideTheLiveSession(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"codexBinaryPath": writeCodexForkAtBinary(t, codexForkMock{
			resumedThreadID: "resume-provider-thread",
			forkedThreadID:  "fork-from-throwaway",
		}),
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	source := testThread("thread-codex-active-source")
	source.Provider = string(provider.Codex)
	source.SessionRef = "resume-provider-thread"
	source.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	liveLog := filepath.Join(t.TempDir(), "live-requests.jsonl")
	session, err := codex.NewSession(context.Background(), source.ID, codex.Config{
		Binary:         writeCodexForkBinary(t, "resume-provider-thread", "fork-from-active-session", liveLog),
		WorkDir:        source.WorkspacePath,
		ResumeThreadID: source.SessionRef,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer session.Close()
	app.sessionManager().put(source.ID, sessionStateForCodex(session))

	forked, err := app.ForkThread(t.Context(), source.ID, nil)
	if err != nil {
		t.Fatalf("ForkThread() error = %v", err)
	}
	if forked.SessionRef != "fork-from-throwaway" {
		t.Fatalf("fork session ref = %q, want %q", forked.SessionRef, "fork-from-throwaway")
	}
	if data, err := os.ReadFile(liveLog); err == nil && strings.Contains(string(data), `"method":"thread/fork"`) {
		t.Fatalf("the live source session answered the fork:\n%s", data)
	}
	if _, ok := app.sessionManager().get(source.ID); !ok {
		t.Fatal("the source's live session was stopped by the fork")
	}
}

// TestForkThreadCodexBoundsAHangingAppServer: the cut runs under the SOURCE
// thread's action lock, so an app-server that accepts the connection and then
// answers nothing must cost the fork its own timeout and release the thread.
// Without the bound the call waits forever and the source thread can never be
// used again. The fork row does not survive the failure: the saga's cleanup
// stack removes the half-built thread.
func TestForkThreadCodexBoundsAHangingAppServer(t *testing.T) {
	restore := codexForkTimeout
	codexForkTimeout = 500 * time.Millisecond
	t.Cleanup(func() { codexForkTimeout = restore })

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"codexBinaryPath": writeHangingCodexForkBinary(t),
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	source := testThread("thread-codex-fork-hang")
	source.Provider = string(provider.Codex)
	source.SessionRef = "resume-provider-thread"
	source.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	started := time.Now()
	_, err := app.ForkThread(t.Context(), source.ID, nil)
	if err == nil {
		t.Fatal("ForkThread() error = nil, want the fork timeout")
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("ForkThread() error = %v, want a deadline failure", err)
	}
	if waited := time.Since(started); waited > 20*time.Second {
		t.Fatalf("ForkThread() waited %s, want roughly the fork timeout", waited)
	}

	threads, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads() error = %v", err)
	}
	for _, thread := range threads {
		if thread.ID != source.ID {
			t.Fatalf("thread %s survived the failed fork", thread.ID)
		}
	}
}

// writeHangingCodexForkBinary is an app-server that completes the handshake
// and then answers nothing, which is how a wedged process looks from AO: the
// pipe is open, the process is alive, and the request never returns.
func writeHangingCodexForkBinary(t *testing.T) string {
	t.Helper()
	script := `#!/bin/sh
while IFS= read -r line; do
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
        printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
    fi
done
`
	path := filepath.Join(t.TempDir(), "codex-hang.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write hanging codex binary: %v", err)
	}
	return path
}

// TestForkThreadCodexRejectsForkTailMismatch: `thread/fork` with a
// lastTurnId cut must produce a fork whose final turn IS the requested
// anchor. A server that answers with a different tail means the cut
// didn't land where AO asked; the fork must fail rather than create a
// thread whose provider history disagrees with its cloned items.
func TestForkThreadCodexRejectsForkTailMismatch(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"codexBinaryPath": writeCodexForkAtBinary(t, codexForkMock{
			resumedThreadID: "resume-provider-thread",
			forkedThreadID:  "fork-provider-thread",
			forkTailTurnID:  "turn-wrong",
		}),
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	source := testThread("thread-codex-fork-mismatch")
	source.Provider = string(provider.Codex)
	source.SessionRef = "resume-provider-thread"
	source.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "user:0", 0, "first", `{"provider_item_id":"provider-user-0"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:1", 1, "second", `{"provider_item_id":"provider-user-1"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:2", 2, "third", `{"provider_item_id":"provider-user-2"}`)
	insertCodexTurn(t, app.store, source.ID, 0, "turn-0")
	insertCodexTurn(t, app.store, source.ID, 1, "turn-1")
	insertCodexTurn(t, app.store, source.ID, 2, "turn-2")

	atTurn := 1
	_, err := app.ForkThread(t.Context(), source.ID, &atTurn)
	if err == nil || !strings.Contains(err.Error(), "expected anchor") {
		t.Fatalf("ForkThread() error = %v, want fork tail mismatch", err)
	}
}

// TestForkThreadClaudeAtTurnSlicesSessionJSONL exercises the fork-at-point
// path: a Claude source with a real on-disk session JSONL, forked at a
// specific user-prompt UUID. The new fork must:
//   - have its SessionRef set to a fresh UUID (not PendingForkRef)
//   - have a new <newID>.jsonl in the same project dir
//   - have items truncated through *atTurnIndex
func TestForkThreadClaudeAtTurnSlicesSessionJSONL(t *testing.T) {
	app := newTestAppWithStore(t)

	// Build a fake ~/.claude/projects layout under TempDir.
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "ws")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	canonical, _ := filepath.EvalSymlinks(workspace)
	abs, _ := filepath.Abs(canonical)
	slug := "-" + filepath.ToSlash(abs)[1:]
	for i, c := range slug {
		if c == '/' {
			slug = slug[:i] + "-" + slug[i+1:]
		}
	}
	projectDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	sessionID := "src-session-uuid"
	jsonlPath := filepath.Join(projectDir, sessionID+".jsonl")
	jsonl := `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"src-session-uuid","message":{"role":"user","content":"first prompt"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"src-session-uuid","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"src-session-uuid","message":{"role":"user","content":"second prompt"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src-session-uuid","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"src-session-uuid","message":{"role":"user","content":"third prompt"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"src-session-uuid","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`
	if err := os.WriteFile(jsonlPath, []byte(jsonl), 0o600); err != nil {
		t.Fatalf("write source jsonl: %v", err)
	}

	source := testThread("thread-claude-fork-at-turn")
	source.Provider = string(provider.Claude)
	source.SessionRef = sessionID
	source.WorkspacePath = workspace
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// Insert items at turn_index 0, 1, 2 — matching the JSONL prompts.
	now := time.Now().UnixMilli()
	for i := 0; i < 3; i++ {
		items := []store.Item{
			{ID: fmt.Sprintf("u%d", i), ThreadID: source.ID, TurnIndex: i, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: fmt.Sprintf("prompt %d", i), CreatedAt: now},
			{ID: fmt.Sprintf("a%d", i), ThreadID: source.ID, TurnIndex: i, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Summary: fmt.Sprintf("reply %d", i), Status: "completed", CreatedAt: now + 1},
		}
		for _, it := range items {
			if err := app.store.InsertItem(it); err != nil {
				t.Fatalf("InsertItem(%s): %v", it.ID, err)
			}
		}
	}

	// Fork at turn 1 — should clone items from turns 0 and 1, slice the
	// JSONL up through u1's UUID, set fork.SessionRef to a fresh ID.
	atTurn := 1
	forked, err := app.ForkThread(t.Context(), source.ID, &atTurn)
	if err != nil {
		t.Fatalf("ForkThread(at=1): %v", err)
	}
	if forked.SessionRef == "" || forked.SessionRef == sessionID {
		t.Fatalf("fork SessionRef should be a fresh UUID, got %q (source was %q)", forked.SessionRef, sessionID)
	}
	if forked.PendingForkRef != "" {
		t.Errorf("fork PendingForkRef should be empty when SessionRef is set, got %q", forked.PendingForkRef)
	}

	// Inherited items: 2 turns × 2 items = 4 (turn 2 dropped).
	items, err := forkConversationItems(app.store, forked.ID)
	if err != nil {
		t.Fatalf("ListItems(fork): %v", err)
	}
	if got, want := len(items), 4; got != want {
		t.Errorf("fork items = %d, want %d (turns 0+1 only)", got, want)
	}
	for _, it := range items {
		if it.TurnIndex > atTurn {
			t.Errorf("fork leaked item at turn_index %d (cap was %d)", it.TurnIndex, atTurn)
		}
	}

	// Forked JSONL must exist on disk.
	forkedPath := filepath.Join(projectDir, forked.SessionRef+".jsonl")
	if _, err := os.Stat(forkedPath); err != nil {
		t.Errorf("forked JSONL not created at %s: %v", forkedPath, err)
	}

	// Source JSONL must be byte-stable.
	srcAfter, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatalf("re-read source: %v", err)
	}
	if string(srcAfter) != jsonl {
		t.Errorf("source JSONL mutated by fork — should be untouched")
	}
}

// TestForkThreadDuringActiveTurnSnapshotsInsteadOfRefusing is the
// successor of the old active-turn refusal. Forking mid-turn is now a
// snapshot "as if interrupted right now": the fork is created, its
// cloned rows settle interrupted, and the source keeps its in-flight
// turn. The full mid-turn contract lives in app_fork_midturn_test.go;
// this keeps the original fixture's coverage of the plain
// no-session-file-yet shape.
func TestForkThreadDuringActiveTurnSnapshotsInsteadOfRefusing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	app := newTestAppWithStore(t)
	source := testThread("thread-fork-active")
	source.Provider = string(provider.Claude)
	source.SessionRef = "claude-active"
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	// Open a turn record without setting completed_at — that's what
	// GetActiveTurn checks.
	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "in-flight-turn",
		ThreadID:  source.ID,
		TurnIndex: 2,
		StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	forked, err := app.ForkThread(t.Context(), source.ID, nil)
	if err != nil {
		t.Fatalf("ForkThread during an active turn: %v", err)
	}
	// No session file exists under the isolated HOME, so the mid-turn
	// tail fork takes the sanctioned degenerate answer.
	if forked.SessionRef != "" || forked.PendingForkRef != "" {
		t.Errorf("fork refs = %q / %q, want both empty (no transcript to slice)", forked.SessionRef, forked.PendingForkRef)
	}
	if _, active, err := app.store.GetActiveTurn(forked.ID); err != nil {
		t.Fatalf("GetActiveTurn(fork): %v", err)
	} else if active {
		t.Error("fork inherited an open turn row — the settle must close it")
	}
	if _, active, err := app.store.GetActiveTurn(source.ID); err != nil {
		t.Fatalf("GetActiveTurn(source): %v", err)
	} else if !active {
		t.Error("the source's in-flight turn was settled — forking must never interrupt the source")
	}
}

func TestForkThreadFromMessageFirstMessageCreatesEmptyFork(t *testing.T) {
	app := newTestAppWithStore(t)
	attachments, err := attachmentstore.NewStore(attachmentstore.Config{RootDir: t.TempDir()}, app.store)
	if err != nil {
		t.Fatalf("attachment store: %v", err)
	}
	app.attachments = attachments
	source := testThread("thread-message-fork-first")
	source.Provider = string(provider.Claude)
	source.SessionRef = "source-session"
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	sourceAttachment := uploadTestAttachment(t, app, source.ID, "one.png", "image/png", tinyPNG())
	meta, err := json.Marshal(userMessageMeta{
		Attachments: []userMessageAttachmentMeta{
			{ID: sourceAttachment.ID, ThreadID: source.ID, Filename: "one.png", MimeType: "image/png"},
		},
	})
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := app.store.InsertItem(store.Item{
		ID:        "user-first",
		ThreadID:  source.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "first",
		Meta:      string(meta),
		CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}
	seedMessageAnchor(t, app.store, source.ID, "user-first", 0, "", "")

	forked, err := app.ForkThreadFromMessage(t.Context(), source.ID, "user-first")
	if err != nil {
		t.Fatalf("ForkThreadFromMessage: %v", err)
	}
	items, err := app.store.ListItems(forked.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("fork items = %+v, want empty", items)
	}
	if forked.SessionRef != "" || forked.PendingForkRef != "" {
		t.Fatalf("fork provider refs = %q/%q, want empty", forked.SessionRef, forked.PendingForkRef)
	}
	draft, ok, err := app.store.GetThreadDraft(forked.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if !ok || draft.Content != "first" {
		t.Fatalf("fork draft = %+v ok=%v, want selected prompt", draft, ok)
	}
	var attachmentIDs []string
	if err := json.Unmarshal([]byte(draft.Attachments), &attachmentIDs); err != nil {
		t.Fatalf("decode draft attachments: %v", err)
	}
	if len(attachmentIDs) != 1 || attachmentIDs[0] == sourceAttachment.ID {
		t.Fatalf("fork draft attachments = %v, want one cloned attachment id different from %q", attachmentIDs, sourceAttachment.ID)
	}
	forkAttachments, err := app.ListAttachments(forked.ID)
	if err != nil {
		t.Fatalf("ListAttachments(fork): %v", err)
	}
	if len(forkAttachments) != 1 || forkAttachments[0].ID != attachmentIDs[0] {
		t.Fatalf("fork attachments = %+v, want cloned draft attachment %q", forkAttachments, strings.Join(attachmentIDs, ","))
	}
}

// A user row without a persisted at-send anchor (record error, legacy
// row) synthesizes one from the item itself — fork-from-message must
// succeed instead of stranding the message.
func TestForkThreadFromMessageSynthesizesMissingAnchor(t *testing.T) {
	app := newTestAppWithStore(t)
	source := testThread("thread-message-fork-no-anchor")
	source.Provider = string(provider.Claude)
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.store.InsertItem(store.Item{
		ID:        "user-no-anchor",
		ThreadID:  source.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "first",
		CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}
	forked, err := app.ForkThreadFromMessage(t.Context(), source.ID, "user-no-anchor")
	if err != nil {
		t.Fatalf("ForkThreadFromMessage without anchor: %v", err)
	}
	draft, ok, err := app.store.GetThreadDraft(forked.ID)
	if err != nil || !ok {
		t.Fatalf("GetThreadDraft: ok=%v err=%v", ok, err)
	}
	if draft.Content != "first" {
		t.Fatalf("fork draft = %q, want selected prompt", draft.Content)
	}
}

func TestForkThreadFromMessageDoesNotCopyMessageAnchors(t *testing.T) {
	app := newTestAppWithStore(t)
	source := testThread("thread-message-fork-anchor-copy")
	source.Provider = string(provider.Claude)
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.store.InsertItem(store.Item{
		ID:        "user-anchored",
		ThreadID:  source.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "first",
		CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}
	seedMessageAnchor(t, app.store, source.ID, "user-anchored", 0, "", "")

	forked, err := app.ForkThreadFromMessage(t.Context(), source.ID, "user-anchored")
	if err != nil {
		t.Fatalf("ForkThreadFromMessage: %v", err)
	}
	anchors, err := app.store.ListMessageAnchors(forked.ID)
	if err != nil {
		t.Fatalf("ListMessageAnchors(fork): %v", err)
	}
	if len(anchors) != 0 {
		t.Fatalf("fork message anchors = %d, want 0", len(anchors))
	}
}

func TestForkThreadFromMessageRejectsMissingClaudeSessionForLaterTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	source := testThread("thread-message-fork-missing-session")
	source.Provider = string(provider.Claude)
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	for turn := 0; turn <= 1; turn++ {
		id := fmt.Sprintf("user-%d", turn)
		if err := app.store.InsertItem(store.Item{
			ID:        id,
			ThreadID:  source.ID,
			TurnIndex: turn,
			ItemIndex: 0,
			Kind:      "user_text",
			Role:      "user",
			Summary:   id,
			CreatedAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("InsertItem: %v", err)
		}
	}
	seedMessageAnchor(t, app.store, source.ID, "user-1", 1, "", "")
	if _, err := app.ForkThreadFromMessage(t.Context(), source.ID, "user-1"); err == nil || !strings.Contains(err.Error(), "missing a Claude session reference") {
		t.Fatalf("ForkThreadFromMessage error = %v, want missing session reference", err)
	}
}

func TestForkThreadFromMessageSlicesClaudeSessionByTurnBoundary(t *testing.T) {
	app := newTestAppWithStore(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	const sessionID = "source-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"source-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"source-session","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
`)
	source := testThread("thread-message-fork-slice-session")
	source.Provider = string(provider.Claude)
	source.SessionRef = sessionID
	source.WorkspacePath = workspace
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	for turn := 0; turn <= 1; turn++ {
		id := fmt.Sprintf("user-%d", turn)
		if err := app.store.InsertItem(store.Item{
			ID:        id,
			ThreadID:  source.ID,
			TurnIndex: turn,
			ItemIndex: 0,
			Kind:      "user_text",
			Role:      "user",
			Summary:   id,
			CreatedAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("InsertItem: %v", err)
		}
	}
	seedMessageAnchor(t, app.store, source.ID, "user-1", 1, "u1", "")

	forked, err := app.ForkThreadFromMessage(t.Context(), source.ID, "user-1")
	if err != nil {
		t.Fatalf("ForkThreadFromMessage: %v", err)
	}
	if forked.SessionRef == "" || forked.SessionRef == sessionID {
		t.Fatalf("forked session ref = %q, want sliced fork session", forked.SessionRef)
	}
	assertClaudeSessionText(t, workspace, forked.SessionRef, []string{"first"}, []string{"second"})
	items, err := forkConversationItems(app.store, forked.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 1 || items[0].Summary != "user-0" {
		t.Fatalf("fork items = %+v, want only turn 0 user", items)
	}
	// The slice already happened, so a pending ref left behind would make
	// the fork's first start fork the source a second time.
	if forked.PendingForkRef != "" || forked.PendingForkResumeAt != "" {
		t.Fatalf("sliced fork still pends a fork: %q/%q", forked.PendingForkRef, forked.PendingForkResumeAt)
	}
}

func TestForkThreadFromMessageSlicesClaudeSessionFromPendingForkRef(t *testing.T) {
	app := newTestAppWithStore(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	const sessionID = "pending-source-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"pending-source-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"pending-source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"pending-source-session","message":{"role":"user","content":"second"}}
`)
	source := testThread("thread-message-fork-pending-parent")
	source.Provider = string(provider.Claude)
	source.PendingForkRef = sessionID
	source.WorkspacePath = workspace
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	for turn := 0; turn <= 1; turn++ {
		id := fmt.Sprintf("user-%d", turn)
		if err := app.store.InsertItem(store.Item{
			ID:        id,
			ThreadID:  source.ID,
			TurnIndex: turn,
			ItemIndex: 0,
			Kind:      "user_text",
			Role:      "user",
			Summary:   id,
			CreatedAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("InsertItem: %v", err)
		}
	}
	seedMessageAnchor(t, app.store, source.ID, "user-1", 1, "u1", "")

	forked, err := app.ForkThreadFromMessage(t.Context(), source.ID, "user-1")
	if err != nil {
		t.Fatalf("ForkThreadFromMessage: %v", err)
	}
	if forked.SessionRef == "" || forked.SessionRef == sessionID {
		t.Fatalf("forked session ref = %q, want sliced session from pending fork source", forked.SessionRef)
	}
	assertClaudeSessionText(t, workspace, forked.SessionRef, []string{"first"}, []string{"second"})
	if forked.PendingForkRef != "" {
		t.Fatalf("forked pending ref = %q, want empty", forked.PendingForkRef)
	}
}

func TestForkThreadFromMessageCanForkOlderAnchorAfterClaudeSessionFork(t *testing.T) {
	app := newTestApp(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	const sessionID = "source-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"source-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"source-session","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"source-session","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`)
	source := createAppTestThread(t, app, "thread-message-fork-after-revert", "claude", workspace)
	source.SessionRef = sessionID
	if err := app.store.UpdateThread(source); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, source.ID, "user-0", 0, "first")
	insertUserItem(t, app.store, source.ID, "user-1", 1, "second")
	insertUserItem(t, app.store, source.ID, "user-2", 2, "third")
	seedMessageAnchor(t, app.store, source.ID, "user-1", 1, "u1", "a0")
	seedMessageAnchor(t, app.store, source.ID, "user-2", 2, "u2", "a1")

	if err := rollbackToMessage(app, source.ID, "user-2"); err != nil {
		t.Fatalf("rollbackToMessage: %v", err)
	}
	revertedSource, err := app.store.GetThread(source.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if revertedSource.SessionRef == "" || revertedSource.SessionRef == sessionID {
		t.Fatalf("source session after rollback = %q, want remapped fork session", revertedSource.SessionRef)
	}

	forked, err := app.ForkThreadFromMessage(t.Context(), source.ID, "user-1")
	if err != nil {
		t.Fatalf("ForkThreadFromMessage after rollback: %v", err)
	}
	assertClaudeSessionText(t, workspace, forked.SessionRef, []string{"first"}, []string{"second", "third"})
	items, err := forkConversationItems(app.store, forked.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 1 || items[0].Summary != "first" {
		t.Fatalf("fork items = %+v, want only first user item", items)
	}
}

// TestForkThreadAtTurnRejectsOutOfRange pins the validation guard.
func TestForkThreadAtTurnRejectsOutOfRange(t *testing.T) {
	app := newTestAppWithStore(t)
	source := testThread("thread-fork-bounds")
	source.Provider = string(provider.Claude)
	source.SessionRef = "claude-session-x"
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)
	// insertForkTestItems puts 2 items at turn_index=1, so lastTurn=1.

	for _, n := range []int{-1, 5, 99} {
		atTurn := n
		if _, err := app.ForkThread(t.Context(), source.ID, &atTurn); err == nil {
			t.Errorf("ForkThread(at=%d): expected error, got nil", n)
		}
	}
}

func insertForkTestItems(t *testing.T, st *store.Store, threadID string) {
	t.Helper()

	now := time.Now().UnixMilli()
	items := []store.Item{
		{
			ID:        "item-" + threadID + "-0",
			ThreadID:  threadID,
			TurnIndex: 1,
			ItemIndex: 0,
			Kind:      "user_text",
			Role:      "user",
			Summary:   "first message",
			CreatedAt: now,
		},
		{
			ID:        "item-" + threadID + "-1",
			ThreadID:  threadID,
			TurnIndex: 1,
			ItemIndex: 1,
			Kind:      "assistant_text",
			Role:      "assistant",
			Summary:   "assistant reply",
			CreatedAt: now + 1,
		},
	}
	for _, item := range items {
		if err := st.InsertItem(item); err != nil {
			t.Fatalf("InsertItem(%s) error = %v", item.ID, err)
		}
	}
}

// writeCodexForkBinary answers a resume and a fork. requestLogPath, when
// set, receives every request line.
func writeCodexForkBinary(t *testing.T, resumedThreadID string, forkedThreadID string, requestLogPath string) string {
	t.Helper()

	logRequest := ":"
	if requestLogPath != "" {
		logRequest = fmt.Sprintf(`/bin/echo "$line" >> '%s'`, requestLogPath)
	}
	script := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
    %s
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/resume"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/fork"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
    fi
done
`, logRequest, resumedThreadID, resumedThreadID, forkedThreadID)

	path := filepath.Join(t.TempDir(), "codex-fork.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func sessionStateForCodex(sess *codex.Session) session {
	return session{
		Provider: string(provider.Codex),
		Token:    "fork-active-token",
		Codex:    sess,
	}
}

// TestForkThreadRollsBackOnResumeFailure exercises A5's atomicity guard:
// when resolveForkResumeState fails (e.g. Claude source missing
// SessionRef, Codex session broken), the fork thread row must not exist
// in the DB afterwards. Before A5, the fork row survived and the user
// was left with an orphan thread they couldn't resume.
func TestForkThreadRollsBackOnResumeFailure(t *testing.T) {
	app := newTestAppWithStore(t)

	source := testThread("thread-broken-source")
	source.Provider = string(provider.Claude)
	// SessionRef intentionally empty — resolveForkResumeState will fail.
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	_, err := app.ForkThread(t.Context(), source.ID, nil)
	if err == nil {
		t.Fatal("expected ForkThread to fail when source is missing SessionRef")
	}

	// The fork row must NOT survive. Walk the threads table.
	list, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	for _, th := range list {
		if th.ForkedFromThreadID == source.ID {
			t.Errorf("orphan fork row survived cleanup: %+v", th)
		}
	}
}

// A fork row that is already gone is what cleanup wanted, so rolling back
// twice, or rolling back a fork that was never written, is not a failure.
func TestForkThreadCleanupIsIdempotentOnMissingFork(t *testing.T) {
	app := newTestAppWithStore(t)
	if err := app.cleanupForkThread("does-not-exist"); err != nil {
		t.Errorf("cleanupForkThread on missing fork should be nil, got %v", err)
	}
	if err := app.cleanupForkThread(""); err != nil {
		t.Errorf("cleanupForkThread on empty id should be nil, got %v", err)
	}
}

// A fork that cannot resolve its resume state fails with the reason, so the
// caller learns what was wrong rather than that something was.
func TestForkThreadPropagatesResumeAndCleanupErrors(t *testing.T) {
	app := newTestAppWithStore(t)

	source := testThread("thread-cleanup-err-source")
	source.Provider = string(provider.Claude)
	// SessionRef empty causes resolveForkResumeState to fail.
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	_, err := app.ForkThread(t.Context(), source.ID, nil)
	if err == nil {
		t.Fatal("expected error")
	}

	// The primary error must identify the resume problem.
	if !containsText(err.Error(), "no resumable Claude session") {
		t.Errorf("primary fork error not propagated: %v", err)
	}
}

// Attachment bytes the fork already wrote are part of the rollback, so a
// directory that will not go away has to reach the caller: the row is gone
// and the disk is not, and only the error says so.
func TestCleanupForkThreadReportsAttachmentFilesItCouldNotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	app := newTestAppWithStore(t)
	root := t.TempDir()
	attachments, err := attachmentstore.NewStore(attachmentstore.Config{RootDir: root}, app.store)
	if err != nil {
		t.Fatalf("attachment.NewStore: %v", err)
	}
	app.attachments = attachments

	fork := testThread("fork-cleanup-undeletable")
	if err := app.store.CreateThread(fork); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	threadDir := filepath.Join(attachments.Root(), fork.ID)
	if err := os.MkdirAll(threadDir, 0o755); err != nil {
		t.Fatalf("create thread attachment dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(threadDir, "note.txt"), []byte("bytes"), 0o644); err != nil {
		t.Fatalf("write attachment file: %v", err)
	}
	// A directory nothing may be unlinked from is the plainest way to make
	// the removal fail without mocking the store.
	if err := os.Chmod(threadDir, 0o555); err != nil {
		t.Fatalf("chmod thread attachment dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(threadDir, 0o755) })

	err = app.cleanupForkThread(fork.ID)
	if err == nil {
		t.Fatal("cleanup reported success while the fork's attachment files are still on disk")
	}
	if !containsText(err.Error(), fork.ID) {
		t.Errorf("cleanup error = %v, want the fork it could not finish removing", err)
	}
	if _, getErr := app.store.GetThread(fork.ID); getErr == nil {
		t.Error("the fork row survived a cleanup that only failed on its files")
	}
}

func containsText(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestForkThread_ExcludesBackgroundRunningRows pins Phase-4's fork
// exclusion contract. The forked thread must NOT carry over any
// `is_background=true AND status='running'` rows from the parent —
// those point at PTYs / subagents owned by the parent's provider
// subprocess, and the fork gets its own subprocess that can never
// reach them. The parent thread is untouched; its backgrounded
// launches keep running under its own session.
//
// Everything else copies normally: user text, assistant text,
// completed backgrounded rows, and non-background running rows (those
// DO copy — the reconciler's force-close will settle any that don't
// naturally complete, and they're valid to carry into the fork since
// the fork's own session inherits the conversational state anyway).
func TestForkThread_ExcludesBackgroundRunningRows(t *testing.T) {
	app := newTestAppWithStore(t)

	source := testThread("thread-fork-bg-exclusion-source")
	source.Provider = string(provider.Claude)
	source.SessionRef = "claude-session-bg"
	fixture := newMidTurnForkFixture(t, "claude-session-bg", midTurnSourceJSONL)
	source.WorkspacePath = fixture.workspace
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Seed a mix: user text + assistant text (copy normally) + running
	// backgrounded row (EXCLUDED) + completed backgrounded row (copy) +
	// running non-background row (copy).
	now := time.Now().UnixMilli()
	seedItems := []store.Item{
		{
			ID: "item-user-0", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 0,
			Kind: "user_text", Role: "user", Summary: "hi", CreatedAt: now,
		},
		{
			ID: "item-assistant-1", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 1,
			Kind: "assistant_text", Role: "assistant", Summary: "hello",
			Status: "completed", CreatedAt: now,
		},
		{
			ID: "item-bg-running", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 2,
			Kind: "tool_call", Role: "assistant", Status: "running",
			IsBackground: true, Summary: "Bash: sleep 60",
			ToolName: "Bash", CreatedAt: now,
		},
		{
			ID: "item-bg-done", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 3,
			Kind: "tool_call", Role: "assistant", Status: "completed",
			IsBackground: true, Summary: "Bash: echo done",
			ToolName: "Bash", CreatedAt: now,
		},
		{
			ID: "item-inline-running", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 4,
			Kind: "tool_call", Role: "assistant", Status: "running",
			Summary: "Read: /tmp/x", ToolName: "Read", CreatedAt: now,
		},
	}
	for _, it := range seedItems {
		if err := app.store.InsertItem(it); err != nil {
			t.Fatalf("InsertItem %s: %v", it.ID, err)
		}
	}

	forked, err := app.ForkThread(t.Context(), source.ID, nil)
	if err != nil {
		t.Fatalf("ForkThread: %v", err)
	}

	forkedItems, err := forkConversationItems(app.store, forked.ID)
	if err != nil {
		t.Fatalf("ListItems(forked): %v", err)
	}

	// Four rows are inherited; the running backgrounded one is hidden.
	if len(forkedItems) != 4 {
		var summaries []string
		for _, it := range forkedItems {
			summaries = append(summaries, fmt.Sprintf("%s[%s]", it.Kind, it.Summary))
		}
		t.Fatalf("forked items = %d (%v), want 4 (running bg row excluded)", len(forkedItems), summaries)
	}

	// Assert the specific exclusion: no forked row carries the bg-running
	// summary or the is_background+running combination.
	for _, it := range forkedItems {
		if it.IsBackground && it.Status == "running" {
			t.Errorf("forked thread carries a backgrounded running row: id=%s summary=%q status=%q",
				it.ID, it.Summary, it.Status)
		}
		if it.Summary == "Bash: sleep 60" {
			t.Errorf("forked thread copied the bg-running row by summary: %+v", it)
		}
	}

	// Parent thread is untouched — the bg-running row is still present.
	parentItems, err := app.store.ListItems(source.ID)
	if err != nil {
		t.Fatalf("ListItems(parent): %v", err)
	}
	var parentBgRunning *store.Item
	for i, it := range parentItems {
		if it.ID == "item-bg-running" {
			parentBgRunning = &parentItems[i]
			break
		}
	}
	if parentBgRunning == nil {
		t.Fatal("parent bg-running row was removed (fork must not mutate parent)")
	}
	if parentBgRunning.Status != "running" {
		t.Errorf("parent bg-running row status = %q, want running", parentBgRunning.Status)
	}
}

// TestForkDuringASourceDeleteIsRefused: a fork asked for while the
// source's delete drains its rows is refused with store.ErrForkSourceDeleted
// and a public sentence every origin shows, and leaves no thread behind.
// The app's delete holds the source's action lock from start to finish, so
// the fork waits and finds the source gone. A store delete that holds no
// action lock (a rollback's) leaves the lock free, and the store refuses
// the fork. A fork admitted before the delete began is detached by it
// (TestPointerForkAdmittedAsTheDeleteBeginsIsDetached).
func TestForkDuringASourceDeleteIsRefused(t *testing.T) {
	type refusal struct {
		op  string
		err error
	}
	requireRefused := func(t *testing.T, got refusal) {
		t.Helper()
		if !errors.Is(got.err, store.ErrForkSourceDeleted) {
			t.Fatalf("%s during the delete = %v, want store.ErrForkSourceDeleted", got.op, got.err)
		}
		code, message, public := errorsx.PublicDetails(got.err)
		if !public || code != "fork_source_deleted" || message != "This thread was deleted, so it cannot be forked." {
			t.Fatalf("%s error %q shows code=%q message=%q public=%v", got.op, got.err, code, message, public)
		}
	}
	seed := func(t *testing.T) (*App, string) {
		app := newTestAppWithStore(t)
		source := testThread("fork-during-delete")
		source.Provider = string(provider.Codex)
		source.SessionRef = "codex-thread"
		if err := app.store.CreateThread(source); err != nil {
			t.Fatal(err)
		}
		for i := range 600 {
			kind, role := "assistant_text", "assistant"
			if i%10 == 0 {
				kind, role = "user_text", "user"
			}
			if err := app.store.InsertItem(store.Item{
				ID: fmt.Sprintf("r%d", i), ThreadID: source.ID, TurnIndex: i / 10, ItemIndex: i % 10,
				Kind: kind, Role: role, Status: "completed", Summary: "row",
			}); err != nil {
				t.Fatal(err)
			}
		}
		return app, source.ID
	}
	requireNoThreads := func(t *testing.T, app *App) {
		t.Helper()
		threads, err := app.store.ListThreads()
		if err != nil {
			t.Fatal(err)
		}
		if len(threads) != 0 {
			t.Fatalf("threads after the refused forks = %+v, want none", threads)
		}
	}

	t.Run("the app's delete", func(t *testing.T) {
		app, sourceID := seed(t)
		refusals := make(chan refusal, 2)
		pauses := 0
		unlock := app.threadLocks().Lock(sourceID)
		err := app.deleteThreadTreePacedLocked(sourceID, func() {
			pauses++
			if pauses > 1 {
				return
			}
			go func() {
				_, err := app.ForkThread(context.Background(), sourceID, nil)
				refusals <- refusal{"ForkThread", err}
			}()
			go func() {
				_, err := app.ForkThreadFromMessage(context.Background(), sourceID, "r550")
				refusals <- refusal{"ForkThreadFromMessage", err}
			}()
		})
		unlock()
		if err != nil {
			t.Fatal(err)
		}
		if pauses == 0 {
			t.Fatal("the source drained in one chunk; the fixture must span several")
		}
		for range 2 {
			requireRefused(t, <-refusals)
		}
		requireNoThreads(t, app)
	})

	t.Run("a store delete without the action lock", func(t *testing.T) {
		app, sourceID := seed(t)
		var got []refusal
		pauses := 0
		if err := app.store.DeleteThreadPaced(sourceID, func() {
			pauses++
			if pauses > 1 {
				return
			}
			_, err := app.ForkThread(context.Background(), sourceID, nil)
			got = append(got, refusal{"ForkThread", err})
			_, err = app.ForkThreadFromMessage(context.Background(), sourceID, "r550")
			got = append(got, refusal{"ForkThreadFromMessage", err})
		}); err != nil {
			t.Fatal(err)
		}
		if pauses == 0 {
			t.Fatal("the source drained in one chunk; the fixture must span several")
		}
		for _, refused := range got {
			requireRefused(t, refused)
		}
		requireNoThreads(t, app)
	})
}
