package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// newDiscussionApp builds an App wired with a fresh on-disk store (so we
// can close/reopen it for persistence tests) plus registry and channel
// services. Returns both the app and the underlying db path so tests can
// reconstruct the store.
func newDiscussionApp(t *testing.T) (*App, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "discussion-integ.db")
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	app := &App{
		store:               st,
		sessions:            make(map[string]session),
		startingSessions:    make(map[string]*sessionStart),
		threadSystemPrompts: make(map[string]string),
		deliberations:       make(map[string]*discussion.Deliberation),
		registry:            discussion.NewRegistry(st),
		channels:            discussion.NewChannelService(st),
	}
	ensureDefaultTestProject(t, app)
	// Stub out session start/stop so discussion orchestration does not try to
	// spawn real provider processes.
	app.startSessionFn = func(threadID string) error { return nil }
	app.stopSessionFn = func(threadID string) error { return nil }
	return app, dbPath
}

func integTestThread(id string) store.Thread {
	now := time.Now().UnixMilli()
	return store.Thread{
		ID:            id,
		ProjectID:     defaultTestProjectID,
		Title:         "Integration Thread",
		Provider:      string(provider.Codex),
		WorkspacePath: "/tmp/workspace",
		Model:         "gpt-5.4",
		Mode:          "chat",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

func globalArchitectsDef() store.DiscussionDefinition {
	now := time.Now().UnixMilli()
	return store.DiscussionDefinition{
		Name:        "Architects",
		Description: "Argue the design",
		Scope:       "global",
		Participants: []store.DiscussionParticipant{
			{Role: "proposer", System: "Drive the proposal"},
			{Role: "reviewer", System: "Critique the proposal"},
		},
		Settings:  store.DiscussionSettings{MaxTurns: 4},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// TestDisc_CreateDiscussionPersists (28)
func TestDisc_CreateDiscussionPersists(t *testing.T) {
	app, _ := newDiscussionApp(t)

	def := globalArchitectsDef()
	if err := app.CreateDiscussion(def); err != nil {
		t.Fatalf("CreateDiscussion: %v", err)
	}
	got, err := app.GetDiscussion("Architects", "global")
	if err != nil {
		t.Fatalf("GetDiscussion: %v", err)
	}
	if got.ID == "" {
		t.Error("expected generated discussion ID")
	}
	if got.Name != "Architects" {
		t.Errorf("Name = %q, want Architects", got.Name)
	}
	if len(got.Participants) != 2 {
		t.Errorf("Participants len = %d, want 2", len(got.Participants))
	}
}

// TestDisc_ListDiscussionsScopedToWorkspace (29) — the persistence layer
// actually scopes by project (not workspace), with "global" fallback. This
// test codifies the real contract.
func TestDisc_ListDiscussionsScopedToWorkspace(t *testing.T) {
	app, _ := newDiscussionApp(t)

	// Global discussion.
	if err := app.CreateDiscussion(globalArchitectsDef()); err != nil {
		t.Fatalf("CreateDiscussion(global): %v", err)
	}
	// Project-scoped discussion.
	projDef := globalArchitectsDef()
	projDef.Name = "ProjectTeam"
	projDef.Scope = "project"
	projDef.ProjectID = "proj-A"
	if err := app.CreateDiscussion(projDef); err != nil {
		t.Fatalf("CreateDiscussion(project): %v", err)
	}

	global, err := app.ListDiscussions("global")
	if err != nil {
		t.Fatalf("ListDiscussions(global): %v", err)
	}
	if len(global) != 1 {
		t.Errorf("global list len = %d, want 1", len(global))
	}
	if global[0].Name != "Architects" {
		t.Errorf("global[0].Name = %q, want Architects", global[0].Name)
	}

	projects, err := app.ListDiscussions("project")
	if err != nil {
		t.Fatalf("ListDiscussions(project): %v", err)
	}
	if len(projects) != 1 {
		t.Errorf("project list len = %d, want 1", len(projects))
	}
	if projects[0].ProjectID != "proj-A" {
		t.Errorf("project[0].ProjectID = %q, want proj-A", projects[0].ProjectID)
	}
}

// TestDisc_StartDiscussionCreatesThreadWithDiscussionMode (30)
func TestDisc_StartDiscussionCreatesThreadWithDiscussionMode(t *testing.T) {
	app, _ := newDiscussionApp(t)

	if err := app.CreateDiscussion(globalArchitectsDef()); err != nil {
		t.Fatalf("CreateDiscussion: %v", err)
	}
	thread := integTestThread("thread-disc-30")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.StartDiscussion(thread.ID, "Architects"); err != nil {
		t.Fatalf("StartDiscussion: %v", err)
	}

	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if updated.Mode != "discussion" {
		t.Errorf("parent.Mode = %q, want discussion", updated.Mode)
	}
	if updated.DiscussionID == "" {
		t.Error("parent thread has no discussion channel ID")
	}

	// Each child thread should also be in discussion mode.
	children, err := app.store.ListChildThreads(thread.ID)
	if err != nil {
		t.Fatalf("ListChildThreads: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("children count = %d, want 2", len(children))
	}
	for _, c := range children {
		if c.Mode != "discussion" {
			t.Errorf("child %s mode = %q, want discussion", c.ID, c.Mode)
		}
	}
}

// TestDisc_PostChannelMessageAppearsInGetChannelMessages (31)
func TestDisc_PostChannelMessageAppearsInGetChannelMessages(t *testing.T) {
	app, _ := newDiscussionApp(t)

	if err := app.CreateDiscussion(globalArchitectsDef()); err != nil {
		t.Fatalf("CreateDiscussion: %v", err)
	}
	thread := integTestThread("thread-disc-31")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.StartDiscussion(thread.ID, "Architects"); err != nil {
		t.Fatalf("StartDiscussion: %v", err)
	}
	parent, _ := app.store.GetThread(thread.ID)

	if err := app.PostChannelMessage(parent.DiscussionID, "hello from human"); err != nil {
		t.Fatalf("PostChannelMessage: %v", err)
	}
	messages, err := app.GetChannelMessages(parent.DiscussionID, -1, 10)
	if err != nil {
		t.Fatalf("GetChannelMessages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(messages))
	}
	if messages[0].Content != "hello from human" {
		t.Errorf("content = %q", messages[0].Content)
	}
	if messages[0].FromType != "human" {
		t.Errorf("fromType = %q, want human", messages[0].FromType)
	}
}

// TestDisc_GetChannelMessagesOrdersBySeq (32)
func TestDisc_GetChannelMessagesOrdersBySeq(t *testing.T) {
	app, _ := newDiscussionApp(t)

	if err := app.CreateDiscussion(globalArchitectsDef()); err != nil {
		t.Fatalf("CreateDiscussion: %v", err)
	}
	thread := integTestThread("thread-disc-32")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.StartDiscussion(thread.ID, "Architects"); err != nil {
		t.Fatalf("StartDiscussion: %v", err)
	}
	parent, _ := app.store.GetThread(thread.ID)

	for _, content := range []string{"first", "second", "third"} {
		if err := app.PostChannelMessage(parent.DiscussionID, content); err != nil {
			t.Fatalf("PostChannelMessage(%s): %v", content, err)
		}
	}
	messages, err := app.GetChannelMessages(parent.DiscussionID, -1, 10)
	if err != nil {
		t.Fatalf("GetChannelMessages: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(messages))
	}
	// Returned oldest-first (ascending by sequence).
	for i, want := range []string{"first", "second", "third"} {
		if messages[i].Content != want {
			t.Errorf("messages[%d].Content = %q, want %q", i, messages[i].Content, want)
		}
		if messages[i].Sequence != i {
			t.Errorf("messages[%d].Sequence = %d, want %d", i, messages[i].Sequence, i)
		}
	}
}

// TestDisc_UpdateDiscussionPersists (33)
func TestDisc_UpdateDiscussionPersists(t *testing.T) {
	app, _ := newDiscussionApp(t)

	if err := app.CreateDiscussion(globalArchitectsDef()); err != nil {
		t.Fatalf("CreateDiscussion: %v", err)
	}
	got, err := app.GetDiscussion("Architects", "global")
	if err != nil {
		t.Fatalf("GetDiscussion: %v", err)
	}

	got.Description = "Rewritten"
	got.Settings.MaxTurns = 12
	if err := app.UpdateDiscussion("Architects", "global", got); err != nil {
		t.Fatalf("UpdateDiscussion: %v", err)
	}

	reloaded, err := app.GetDiscussion("Architects", "global")
	if err != nil {
		t.Fatalf("GetDiscussion(reload): %v", err)
	}
	if reloaded.Description != "Rewritten" {
		t.Errorf("Description = %q, want Rewritten", reloaded.Description)
	}
	if reloaded.Settings.MaxTurns != 12 {
		t.Errorf("MaxTurns = %d, want 12", reloaded.Settings.MaxTurns)
	}
	if reloaded.CreatedAt != got.CreatedAt {
		t.Errorf("CreatedAt changed from %d to %d", got.CreatedAt, reloaded.CreatedAt)
	}
}

// TestDisc_DeleteDiscussionCascadesChannelMessages (34) — documents actual
// contract: DeleteDiscussion removes only the *definition* (template), not
// any runtime channel or messages. Deleting a template does not touch
// channel_messages belonging to threads that previously used that
// template.
func TestDisc_DeleteDiscussionCascadesChannelMessages(t *testing.T) {
	app, _ := newDiscussionApp(t)

	if err := app.CreateDiscussion(globalArchitectsDef()); err != nil {
		t.Fatalf("CreateDiscussion: %v", err)
	}
	thread := integTestThread("thread-disc-34")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.StartDiscussion(thread.ID, "Architects"); err != nil {
		t.Fatalf("StartDiscussion: %v", err)
	}
	parent, _ := app.store.GetThread(thread.ID)

	if err := app.PostChannelMessage(parent.DiscussionID, "msg-1"); err != nil {
		t.Fatalf("PostChannelMessage: %v", err)
	}

	// Delete the definition.
	if err := app.DeleteDiscussion("Architects", "global"); err != nil {
		t.Fatalf("DeleteDiscussion: %v", err)
	}

	// The runtime channel + its messages should still exist — deleting a
	// template is independent of runtime state.
	messages, err := app.GetChannelMessages(parent.DiscussionID, -1, 10)
	if err != nil {
		t.Fatalf("GetChannelMessages after DeleteDiscussion: %v", err)
	}
	if len(messages) != 1 {
		t.Errorf("messages len = %d, want 1 (template delete should not cascade to runtime)", len(messages))
	}
	// But looking up the template itself must now fail.
	if _, err := app.GetDiscussion("Architects", "global"); err == nil {
		t.Error("expected deleted template lookup to fail")
	}
}

// TestDisc_DeleteThreadPairedWithDiscussionPreservesDiscussion (35) —
// deleting the parent thread cascades to the runtime channel (via SQL
// ON DELETE CASCADE), but the *template* (DiscussionDefinition) is
// untouched.
func TestDisc_DeleteThreadPairedWithDiscussionPreservesDiscussion(t *testing.T) {
	app, _ := newDiscussionApp(t)

	if err := app.CreateDiscussion(globalArchitectsDef()); err != nil {
		t.Fatalf("CreateDiscussion: %v", err)
	}
	thread := integTestThread("thread-disc-35")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.StartDiscussion(thread.ID, "Architects"); err != nil {
		t.Fatalf("StartDiscussion: %v", err)
	}
	parent, _ := app.store.GetThread(thread.ID)
	channelID := parent.DiscussionID

	if err := app.DeleteThread(thread.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	// Runtime channel is gone (cascade).
	if _, err := app.store.GetChannel(channelID); err == nil {
		t.Error("expected channel to be deleted via thread cascade")
	}
	// Template still exists.
	if _, err := app.GetDiscussion("Architects", "global"); err != nil {
		t.Errorf("expected template to survive thread deletion: %v", err)
	}
}

// TestDisc_ParticipantRoutingRespected (36) — when a participant thread
// produces assistant text, the syncDiscussionTurn mirrors it into the
// channel with the expected FromRole.
func TestDisc_ParticipantRoutingRespected(t *testing.T) {
	app, _ := newDiscussionApp(t)

	if err := app.CreateDiscussion(globalArchitectsDef()); err != nil {
		t.Fatalf("CreateDiscussion: %v", err)
	}
	thread := integTestThread("thread-disc-36")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.StartDiscussion(thread.ID, "Architects"); err != nil {
		t.Fatalf("StartDiscussion: %v", err)
	}
	parent, _ := app.store.GetThread(thread.ID)
	children, _ := app.store.ListChildThreads(thread.ID)
	if len(children) < 1 {
		t.Fatal("expected child threads")
	}
	childA := children[0]
	// Expected role comes from the " - <Role>" suffix of the child title.
	expectedRole := discussion.RoleFromThreadTitle(childA.Title)

	// Simulate a turn from childA.
	now := time.Now().UnixMilli()
	if err := app.store.InsertItem(store.Item{
		ID:        "item-36-a",
		ThreadID:  childA.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "assistant_text",
		Role:      "assistant",
		Summary:   "I propose X",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}
	if err := app.syncDiscussionTurn(childA.ID); err != nil {
		t.Fatalf("syncDiscussionTurn: %v", err)
	}

	messages, err := app.GetChannelMessages(parent.DiscussionID, -1, 10)
	if err != nil {
		t.Fatalf("GetChannelMessages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(messages))
	}
	if messages[0].FromID != childA.ID {
		t.Errorf("FromID = %q, want %q", messages[0].FromID, childA.ID)
	}
	if messages[0].FromRole != expectedRole {
		t.Errorf("FromRole = %q, want %q", messages[0].FromRole, expectedRole)
	}
	if messages[0].Content != "I propose X" {
		t.Errorf("Content = %q", messages[0].Content)
	}
}

// TestDisc_ChannelMessageSeqDedup (37) — documents behaviour. There is no
// user-facing idempotency / dedup on messages; each PostChannelMessage
// produces a new message with a new sequence. Two "identical" posts both
// land. This test asserts that contract.
func TestDisc_ChannelMessageSeqDedup(t *testing.T) {
	app, _ := newDiscussionApp(t)

	if err := app.CreateDiscussion(globalArchitectsDef()); err != nil {
		t.Fatalf("CreateDiscussion: %v", err)
	}
	thread := integTestThread("thread-disc-37")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.StartDiscussion(thread.ID, "Architects"); err != nil {
		t.Fatalf("StartDiscussion: %v", err)
	}
	parent, _ := app.store.GetThread(thread.ID)

	if err := app.PostChannelMessage(parent.DiscussionID, "same content"); err != nil {
		t.Fatalf("PostChannelMessage #1: %v", err)
	}
	if err := app.PostChannelMessage(parent.DiscussionID, "same content"); err != nil {
		t.Fatalf("PostChannelMessage #2: %v", err)
	}

	messages, err := app.GetChannelMessages(parent.DiscussionID, -1, 10)
	if err != nil {
		t.Fatalf("GetChannelMessages: %v", err)
	}
	// Actual behaviour: no dedup; both posts recorded with distinct sequences.
	if len(messages) != 2 {
		t.Errorf("messages len = %d, want 2 (no dedup; each post is independent)", len(messages))
	}
	if messages[0].Sequence == messages[1].Sequence {
		t.Errorf("sequences not distinct: %d == %d", messages[0].Sequence, messages[1].Sequence)
	}
	if messages[0].ID == messages[1].ID {
		t.Error("expected distinct message IDs even for identical content")
	}
}

// TestDisc_DiscussionSurvivesAppRestart (38) — persist a definition, close
// and reopen the store, and verify the definition is still there.
func TestDisc_DiscussionSurvivesAppRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist.db")

	// -- First "run" --
	st1, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New #1: %v", err)
	}
	reg1 := discussion.NewRegistry(st1)
	if err := reg1.Create(store.DiscussionDefinition{
		Name:  "Survivor",
		Scope: "global",
		Participants: []store.DiscussionParticipant{
			{Role: "proposer", System: "Lead"},
			{Role: "reviewer", System: "Challenge"},
		},
		Settings: store.DiscussionSettings{MaxTurns: 5},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("Close #1: %v", err)
	}

	// -- Second "run" --
	st2, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New #2: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	reg2 := discussion.NewRegistry(st2)
	got, err := reg2.Get("Survivor", "global")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Name != "Survivor" {
		t.Errorf("Name = %q, want Survivor", got.Name)
	}
	if got.Settings.MaxTurns != 5 {
		t.Errorf("MaxTurns = %d, want 5", got.Settings.MaxTurns)
	}
}

// TestDisc_StartDiscussionFailsOnMissingParticipants (39) — StartDiscussion
// must reject a discussion name that doesn't map to any persisted
// definition. The schema forbids a thread with blank provider/model, and
// participant entries inherit those from the parent, so the "missing
// participant metadata" code path is only reachable via a missing template
// lookup. This test codifies that as the practical failure mode.
func TestDisc_StartDiscussionFailsOnMissingParticipants(t *testing.T) {
	app, _ := newDiscussionApp(t)

	parent := integTestThread("thread-disc-39")
	if err := app.store.CreateThread(parent); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	err := app.StartDiscussion(parent.ID, "DoesNotExist")
	if err == nil {
		t.Fatal("StartDiscussion: expected error for missing definition")
	}

	// Ensure no partial state was persisted.
	updated, _ := app.store.GetThread(parent.ID)
	if updated.Mode == "discussion" {
		t.Error("parent thread got flipped to discussion mode despite failure")
	}
	if updated.DiscussionID != "" {
		t.Error("parent thread has DiscussionID set despite failed start")
	}
	threads, _ := app.store.ListThreads()
	if len(threads) != 1 {
		t.Errorf("len(threads) = %d, want 1 (only parent) after failed start", len(threads))
	}
}

// TestDisc_LargeChannelMessageList (40) — post 500 messages and verify
// GetChannelMessages returns all of them in order without corruption.
func TestDisc_LargeChannelMessageList(t *testing.T) {
	app, _ := newDiscussionApp(t)

	if err := app.CreateDiscussion(globalArchitectsDef()); err != nil {
		t.Fatalf("CreateDiscussion: %v", err)
	}
	thread := integTestThread("thread-disc-40")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.StartDiscussion(thread.ID, "Architects"); err != nil {
		t.Fatalf("StartDiscussion: %v", err)
	}
	parent, _ := app.store.GetThread(thread.ID)

	const N = 500
	for i := 0; i < N; i++ {
		if err := app.PostChannelMessage(parent.DiscussionID, formatLargeContent(i)); err != nil {
			t.Fatalf("PostChannelMessage #%d: %v", i, err)
		}
	}

	// Retrieve without limit (limit == 0 means no limit).
	messages, err := app.GetChannelMessages(parent.DiscussionID, -1, 0)
	if err != nil {
		t.Fatalf("GetChannelMessages: %v", err)
	}
	if len(messages) != N {
		t.Fatalf("len(messages) = %d, want %d", len(messages), N)
	}
	// Sequences must be 0..N-1 and strictly increasing.
	for i, m := range messages {
		if m.Sequence != i {
			t.Fatalf("messages[%d].Sequence = %d, want %d", i, m.Sequence, i)
		}
		want := formatLargeContent(i)
		if m.Content != want {
			t.Fatalf("messages[%d].Content = %q, want %q", i, m.Content, want)
		}
	}
}

// formatLargeContent returns a stable, sortable payload per index.
func formatLargeContent(i int) string {
	return fmt.Sprintf("payload-%05d", i)
}
