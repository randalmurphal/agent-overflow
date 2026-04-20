package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/discussion"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/terminal"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"

	"github.com/google/uuid"
)

// setupCascadeApp builds an App wired up with every subsystem a delete path
// touches: attachments, drafts, terminals, checkpoints, discussion registry.
// Unlike setupE2EApp, this flavour turns on every real component so that a
// DeleteThread exercises the full cascade.
func setupCascadeApp(t *testing.T) (*App, *capturedEventBus, string) {
	t.Helper()

	bus := newCapturedEventBus()

	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "cascade.db")
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	app := &App{
		store:               st,
		settings:            settings.NewService(dbDir),
		sessions:            make(map[string]session),
		startingSessions:    make(map[string]*sessionStart),
		threadSystemPrompts: make(map[string]string),
		deliberations:       make(map[string]*discussion.Deliberation),
		configDir:           dbDir,
	}
	app.triage = triage.NewRouter(st, bus.emit)
	app.triage.SetEventHook(bus.observeRouterEvent)
	app.checkpoints = checkpoint.NewStore()
	app.triage.SetCheckpointStore(app.checkpoints)
	app.registry = discussion.NewRegistry(st)
	app.channels = discussion.NewChannelService(st)
	app.terminals = terminal.NewManager(nil, nil)
	ensureDefaultTestProject(t, app)

	attachmentStore, err := attachment.NewStore(attachment.Config{
		RootDir: filepath.Join(dbDir, "attachments"),
	}, st)
	if err != nil {
		t.Fatalf("attachment.NewStore: %v", err)
	}
	app.attachments = attachmentStore

	t.Cleanup(func() {
		if app.terminals != nil {
			_ = app.terminals.Shutdown()
		}
	})

	return app, bus, dbDir
}

// populateThreadForCascade seeds a thread with the full menu of dependents
// the deletion path touches. Returns the attachment on-disk paths so the test
// can verify they are cleaned up.
func populateThreadForCascade(t *testing.T, app *App, thread store.Thread) []string {
	t.Helper()

	// 3 items + 3 payloads.
	for i := 0; i < 3; i++ {
		payloadID := uuid.NewString()
		payload := store.Payload{
			ID:        payloadID,
			Kind:      "diff",
			Meta:      `{}`,
			Data:      []byte("diff data " + thread.ID),
			CreatedAt: time.Now().UnixMilli(),
		}
		if err := app.store.InsertPayload(payload); err != nil {
			t.Fatalf("InsertPayload %d: %v", i, err)
		}
		item := store.Item{
			ID:        uuid.NewString(),
			ThreadID:  thread.ID,
			TurnIndex: 1,
			ItemIndex: i,
			Kind:      "tool_call",
			Role:      "assistant",
			Summary:   "diff",
			PayloadID: payloadID,
			CreatedAt: time.Now().UnixMilli(),
		}
		if err := app.store.InsertItem(item); err != nil {
			t.Fatalf("InsertItem %d: %v", i, err)
		}
	}

	// 2 attachments on disk + in DB.
	var paths []string
	for i := 0; i < 2; i++ {
		b64 := base64.StdEncoding.EncodeToString([]byte{
			0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
			0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		})
		record, err := app.attachments.Upload(thread.ID, "img.png", "image/png", b64, time.Now().UnixMilli())
		if err != nil {
			t.Fatalf("Upload %d: %v", i, err)
		}
		path := filepath.Join(app.configDir, "attachments", record.RelativePath)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("attachment not on disk: %v", err)
		}
		paths = append(paths, path)
	}

	// 1 draft.
	if err := app.store.UpsertThreadDraft(store.ThreadDraft{
		ThreadID:  thread.ID,
		Content:   "draft content",
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertThreadDraft: %v", err)
	}

	return paths
}

// TestCascade_DeleteThreadRemovesAllDependents ensures that DeleteThread
// cleans up every dependent row and on-disk artifact owned by the thread.
func TestCascade_DeleteThreadRemovesAllDependents(t *testing.T) {
	app, _, _ := setupCascadeApp(t)

	workspace := testutil.InitGitRepo(t)
	thread := e2eThreadCascade("thread-cascade-full", provider.Claude, workspace)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	attachmentPaths := populateThreadForCascade(t, app, thread)

	// 2 captured checkpoints with real git refs.
	for turn := 0; turn < 2; turn++ {
		ref, err := app.checkpoints.CaptureBaseline(context.Background(), workspace, thread.ID, turn)
		if err != nil {
			t.Fatalf("CaptureBaseline turn %d: %v", turn, err)
		}
		cp := store.Checkpoint{
			ID:            uuid.NewString(),
			ThreadID:      thread.ID,
			TurnIndex:     turn,
			RefName:       ref,
			CapturedAt:    time.Now().UnixMilli(),
			WorkspacePath: workspace,
		}
		if err := app.store.SaveCheckpoint(cp); err != nil {
			t.Fatalf("SaveCheckpoint: %v", err)
		}
	}

	// 2 open terminals.
	for i := 0; i < 2; i++ {
		if _, err := app.terminals.Open(thread.ID, terminal.SessionOptions{
			Shell: "/bin/sh",
			Args:  []string{"-c", "sleep 60"},
			Cwd:   workspace,
			Rows:  24,
			Cols:  80,
		}); err != nil {
			t.Fatalf("Open terminal %d: %v", i, err)
		}
	}
	if got := len(app.terminals.List(thread.ID)); got != 2 {
		t.Fatalf("opened terminals = %d, want 2", got)
	}

	// Pre-delete assertions.
	if items, _ := app.store.ListItems(thread.ID); len(items) != 3 {
		t.Fatalf("pre: items = %d, want 3", len(items))
	}
	if attachments, _ := app.store.ListAttachments(thread.ID); len(attachments) != 2 {
		t.Fatalf("pre: attachments = %d, want 2", len(attachments))
	}
	if _, ok, _ := app.store.GetThreadDraft(thread.ID); !ok {
		t.Fatal("pre: draft missing")
	}
	if cps, _ := app.store.ListCheckpoints(thread.ID); len(cps) != 2 {
		t.Fatalf("pre: checkpoints = %d, want 2", len(cps))
	}

	// Act.
	if err := app.DeleteThread(thread.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	// Post-delete: every dependent is gone.
	if _, err := app.store.GetThread(thread.ID); err == nil {
		t.Fatal("thread row still present after DeleteThread")
	}
	if items, _ := app.store.ListItems(thread.ID); len(items) != 0 {
		t.Fatalf("post: items = %d, want 0 (FK CASCADE)", len(items))
	}
	if attachments, _ := app.store.ListAttachments(thread.ID); len(attachments) != 0 {
		t.Fatalf("post: attachments = %d, want 0", len(attachments))
	}
	if _, ok, _ := app.store.GetThreadDraft(thread.ID); ok {
		t.Fatal("post: draft still present")
	}
	if cps, _ := app.store.ListCheckpoints(thread.ID); len(cps) != 0 {
		t.Fatalf("post: checkpoints = %d, want 0", len(cps))
	}
	// Terminals closed.
	if got := len(app.terminals.List(thread.ID)); got != 0 {
		t.Fatalf("post: terminals = %d, want 0", got)
	}
	// Checkpoint git refs removed.
	for turn := 0; turn < 2; turn++ {
		refName := checkpoint.RefForThreadTurn(thread.ID, turn)
		err := testutil.RunGitAllowError(workspace, "rev-parse", "--verify", refName)
		if err == nil {
			t.Fatalf("post: checkpoint ref %s still present", refName)
		}
	}
	// On-disk attachment files are swept alongside the DB cascade.
	for _, p := range attachmentPaths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("post: attachment file %s still on disk after DeleteThread", p)
		}
	}
	// And the per-thread attachment directory is gone too.
	threadAttachmentDir := filepath.Join(app.configDir, "attachments", thread.ID)
	if _, err := os.Stat(threadAttachmentDir); !os.IsNotExist(err) {
		t.Fatalf("post: thread attachment dir %s still present", threadAttachmentDir)
	}
}

// TestCascade_ThreadDeleteIsIdempotent: deleting twice returns either a clean
// idempotent success or a clear error. It must not panic.
func TestCascade_ThreadDeleteIsIdempotent(t *testing.T) {
	app, _, _ := setupCascadeApp(t)

	thread := e2eThreadCascade("thread-cascade-idemp", provider.Claude, t.TempDir())
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.DeleteThread(thread.ID); err != nil {
		t.Fatalf("first DeleteThread: %v", err)
	}

	// Second delete: deleteThreadTree uses sql.ErrNoRows as idempotent-success.
	err := app.DeleteThread(thread.ID)
	if err != nil {
		// If the production code ever transitions to returning an error here,
		// it must be clear (not a panic). Accept any non-nil error as long as
		// it's not a bare runtime panic.
		if strings.Contains(err.Error(), "runtime error") {
			t.Fatalf("second DeleteThread panicked: %v", err)
		}
	}

	// App must still be usable after.
	if _, err := app.ListThreads(); err != nil {
		t.Fatalf("ListThreads after double-delete: %v", err)
	}
}

// TestCascade_DeletingWithActiveSession: an active session is torn down by
// DeleteThread; any items persisted up to that point remain (but the thread
// row is removed, so FK CASCADE nukes them).
func TestCascade_DeletingWithActiveSession(t *testing.T) {
	app, _, _ := setupCascadeApp(t)

	workspace := t.TempDir()
	thread := e2eThreadCascade("thread-cascade-active", provider.Claude, workspace)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Binary that sits on stdin. We never SendMessage to keep things simple.
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{}})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("Update settings: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// Simulate a persisted item that was written mid-session.
	if err := app.store.InsertItem(store.Item{
		ID: uuid.NewString(), ThreadID: thread.ID, TurnIndex: 1, ItemIndex: 0,
		Kind: "user_text", Role: "user", Summary: "before delete",
		CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}

	if err := app.DeleteThread(thread.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	app.mu.Lock()
	_, active := app.sessions[thread.ID]
	app.mu.Unlock()
	if active {
		t.Fatal("session not torn down on thread delete")
	}

	// Thread row plus cascaded items are gone.
	if _, err := app.store.GetThread(thread.ID); err == nil {
		t.Fatal("thread row still present")
	}
	items, _ := app.store.ListItems(thread.ID)
	if len(items) != 0 {
		t.Fatalf("items still present after cascade: %d", len(items))
	}
}

// TestCascade_ForkPreservesOriginalState: ForkThread creates a new thread with
// copied items + payloads; the source stays untouched. Drafts are NOT copied.
func TestCascade_ForkPreservesOriginalState(t *testing.T) {
	app, _, _ := setupCascadeApp(t)

	source := e2eThreadCascade("thread-cascade-fork-src", provider.Claude, t.TempDir())
	source.SessionRef = "claude-sess-fork"
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Populate items + a draft on the source.
	for i := 0; i < 2; i++ {
		if err := app.store.InsertItem(store.Item{
			ID: uuid.NewString(), ThreadID: source.ID, TurnIndex: 1, ItemIndex: i,
			Kind: "user_text", Role: "user", Summary: "seed",
			CreatedAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("InsertItem %d: %v", i, err)
		}
	}
	if err := app.store.UpsertThreadDraft(store.ThreadDraft{
		ThreadID: source.ID, Content: "source draft", UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertThreadDraft: %v", err)
	}

	forked, err := app.ForkThread(source.ID)
	if err != nil {
		t.Fatalf("ForkThread: %v", err)
	}

	// Source still has its items, its draft, and a valid thread row.
	if _, err := app.store.GetThread(source.ID); err != nil {
		t.Fatalf("source row gone after fork: %v", err)
	}
	srcItems, _ := app.store.ListItems(source.ID)
	if len(srcItems) != 2 {
		t.Fatalf("source items after fork = %d, want 2", len(srcItems))
	}
	if draft, ok, _ := app.store.GetThreadDraft(source.ID); !ok || draft.Content != "source draft" {
		t.Fatalf("source draft lost: ok=%v draft=%+v", ok, draft)
	}

	// Fork has its own item IDs but the same summaries.
	forkedItems, _ := app.store.ListItems(forked.ID)
	if len(forkedItems) != 2 {
		t.Fatalf("fork items = %d, want 2", len(forkedItems))
	}
	srcIDs := map[string]bool{}
	for _, it := range srcItems {
		srcIDs[it.ID] = true
	}
	for _, it := range forkedItems {
		if srcIDs[it.ID] {
			t.Fatalf("fork reuses source item ID %s", it.ID)
		}
		if it.ThreadID != forked.ID {
			t.Fatalf("fork item threadID = %q, want %q", it.ThreadID, forked.ID)
		}
	}
	// Drafts are not copied across forks.
	if _, ok, _ := app.store.GetThreadDraft(forked.ID); ok {
		t.Fatal("fork inherited a draft from source; production behaviour is: drafts don't follow forks")
	}
}

// TestCascade_UnarchiveRestoresToSidebar verifies the archive → list hides →
// unarchive → list shows round-trip.
func TestCascade_UnarchiveRestoresToSidebar(t *testing.T) {
	app, _, _ := setupCascadeApp(t)

	thread := e2eThreadCascade("thread-cascade-unarchive", provider.Claude, t.TempDir())
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	// App.ListThreads filters out threads with zero items so the sidebar
	// never surfaces drafts. Seed one real item so the unarchive round-trip
	// is visible through that binding.
	if err := app.store.InsertItem(store.Item{
		ID:        "thread-cascade-unarchive-item",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "hello",
		CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}

	if err := app.ArchiveThread(thread.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	active, err := app.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	for _, tt := range active {
		if tt.ID == thread.ID {
			t.Fatalf("archived thread still listed: %+v", tt)
		}
	}

	refreshed, err := app.UnarchiveThread(thread.ID)
	if err != nil {
		t.Fatalf("UnarchiveThread: %v", err)
	}
	if refreshed.Archived {
		t.Fatalf("UnarchiveThread returned Archived=true")
	}

	active, err = app.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads post-unarchive: %v", err)
	}
	var found bool
	for _, tt := range active {
		if tt.ID == thread.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("unarchived thread missing from ListThreads")
	}
}

// e2eThreadCascade is a cascade-specific thread builder that lets us set
// provider + workspace independently.
func e2eThreadCascade(id string, prov provider.ProviderKind, workspace string) store.Thread {
	now := time.Now().UnixMilli()
	return store.Thread{
		ID:              id,
		ProjectID:     defaultTestProjectID,
		Title:           "Cascade Thread",
		Provider:        string(prov),
		WorkspacePath:   workspace,
		Model:           "claude-opus-4-7",
		Mode: "chat",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}
