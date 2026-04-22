package main

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

var errDiscussionStartFailed = errors.New("discussion start failed")

func TestDiscussionBindingsCRUD(t *testing.T) {
	app := newTestAppWithStore(t)
	app.registry = discussion.NewRegistry(app.store)
	app.channels = discussion.NewChannelService(app.store, app.highlighter)

	def := store.DiscussionDefinition{
		Name:  "Critics",
		Scope: "global",
		Participants: []store.DiscussionParticipant{
			{Role: "proposer", System: "Propose changes"},
			{Role: "reviewer", System: "Review changes"},
		},
		Settings: store.DiscussionSettings{MaxTurns: 5},
	}
	if err := app.CreateDiscussion(def); err != nil {
		t.Fatalf("CreateDiscussion() error = %v", err)
	}

	got, err := app.GetDiscussion("Critics", "global")
	if err != nil {
		t.Fatalf("GetDiscussion() error = %v", err)
	}
	if got.ID == "" {
		t.Fatal("expected persisted discussion ID")
	}

	got.Description = "Updated"
	if err := app.UpdateDiscussion("Critics", "global", got); err != nil {
		t.Fatalf("UpdateDiscussion() error = %v", err)
	}

	list, err := app.ListDiscussions("global")
	if err != nil {
		t.Fatalf("ListDiscussions() error = %v", err)
	}
	if len(list) != 1 || list[0].Description != "Updated" {
		t.Fatalf("ListDiscussions() = %+v, want updated single definition", list)
	}

	if err := app.DeleteDiscussion("Critics", "global"); err != nil {
		t.Fatalf("DeleteDiscussion() error = %v", err)
	}
	if _, err := app.GetDiscussion("Critics", "global"); err == nil {
		t.Fatal("expected deleted discussion lookup to fail")
	}
}

func TestResolveDiscussionDefinitionFallsBackToGlobalWhenProjectDefinitionMissing(t *testing.T) {
	app := newTestAppWithStore(t)
	app.registry = discussion.NewRegistry(app.store)

	thread := testThread("thread-discussion-fallback")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	global := store.DiscussionDefinition{
		Name:  "Architects",
		Scope: "global",
		Participants: []store.DiscussionParticipant{
			{Role: "architect", System: "Design the change"},
			{Role: "reviewer", System: "Review the change"},
		},
		Settings:  store.DiscussionSettings{MaxTurns: 5},
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}
	if err := app.store.CreateDiscussionDef(global); err != nil {
		t.Fatalf("CreateDiscussionDef() error = %v", err)
	}

	def, err := app.resolveDiscussionDefinition(thread, "Architects")
	if err != nil {
		t.Fatalf("resolveDiscussionDefinition() error = %v", err)
	}
	if def.Scope != "global" {
		t.Fatalf("resolved scope = %q, want global", def.Scope)
	}
}

func TestResolveDiscussionDefinitionDoesNotHideProjectDefinitionErrors(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agent-overflow.db")

	st, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := st.Close(); closeErr != nil {
			t.Fatalf("Store.Close() error = %v", closeErr)
		}
	})

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := rawDB.Close(); closeErr != nil {
			t.Fatalf("rawDB.Close() error = %v", closeErr)
		}
	})

	app := &App{
		store:    st,
		registry: discussion.NewRegistry(st),
	}
	ensureDefaultTestProject(t, app)

	thread := testThread("thread-discussion-project-error")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	global := store.DiscussionDefinition{
		Name:  "Architects",
		Scope: "global",
		Participants: []store.DiscussionParticipant{
			{Role: "architect", System: "Design the change"},
			{Role: "reviewer", System: "Review the change"},
		},
		Settings:  store.DiscussionSettings{MaxTurns: 5},
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}
	if err := app.store.CreateDiscussionDef(global); err != nil {
		t.Fatalf("CreateDiscussionDef(global) error = %v", err)
	}

	if _, err := rawDB.Exec(`
		INSERT INTO discussion_definitions (
			id, name, description, scope, project_id, definition, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`,
		"def-project-bad",
		"Architects",
		"Broken project definition",
		"project",
		projectPathForThread(t, app, thread),
		"{bad json",
		time.Now().UnixMilli(),
		time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert malformed project discussion: %v", err)
	}

	_, err = app.resolveDiscussionDefinition(thread, "Architects")
	if err == nil {
		t.Fatal("resolveDiscussionDefinition() error = nil, want project definition decode error")
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("resolveDiscussionDefinition() error = %v, want decode error not not-found", err)
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "decode discussion definition") || !strings.Contains(got, "Architects") {
		t.Fatalf("resolveDiscussionDefinition() error = %v, want decode discussion definition context", err)
	}
}

func TestStartDiscussionCreatesChannelChildrenAndParticipantSessions(t *testing.T) {
	app := newTestAppWithStore(t)
	app.registry = discussion.NewRegistry(app.store)
	app.channels = discussion.NewChannelService(app.store, app.highlighter)

	var started []string
	app.startSessionFn = func(threadID string) error {
		started = append(started, threadID)
		return nil
	}

	thread := testThread("thread-discussion")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.CreateDiscussionDef(store.DiscussionDefinition{
		ID:        "def-project",
		Name:      "Architects",
		Scope:     "project",
		ProjectID: projectPathForThread(t, app, thread),
		Participants: []store.DiscussionParticipant{
			{Role: "architect", System: "Design the change"},
			{Role: "reviewer", System: "Review the change", Provider: string(provider.Claude), Model: "claude-sonnet-4"},
		},
		Settings:  store.DiscussionSettings{MaxTurns: 7},
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("CreateDiscussionDef() error = %v", err)
	}

	if err := app.StartDiscussion(thread.ID, "Architects"); err != nil {
		t.Fatalf("StartDiscussion() error = %v", err)
	}

	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if updated.Mode != "discussion" {
		t.Fatalf("Mode = %q, want discussion", updated.Mode)
	}
	if updated.DiscussionID == "" {
		t.Fatal("expected discussion channel ID on thread")
	}

	channel, err := app.store.GetChannel(updated.DiscussionID)
	if err != nil {
		t.Fatalf("GetChannel() error = %v", err)
	}
	if channel.ThreadID != thread.ID {
		t.Fatalf("channel.ThreadID = %q, want %q", channel.ThreadID, thread.ID)
	}
	if app.deliberations[channel.ID] == nil {
		t.Fatal("expected in-memory deliberation to be initialized")
	}

	threads, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads() error = %v", err)
	}

	var children []store.Thread
	for _, candidate := range threads {
		if candidate.ParentThreadID == thread.ID {
			children = append(children, candidate)
		}
	}
	if len(children) != 2 {
		t.Fatalf("len(children) = %d, want 2", len(children))
	}

	sort.Slice(children, func(i, j int) bool { return children[i].Title < children[j].Title })
	if children[0].Mode != "discussion" || children[1].Mode != "discussion" {
		t.Fatalf("child interaction modes = %q, %q; want discussion", children[0].Mode, children[1].Mode)
	}
	if children[0].DiscussionID != channel.ID || children[1].DiscussionID != channel.ID {
		t.Fatalf("child discussion IDs = %q, %q; want %q", children[0].DiscussionID, children[1].DiscussionID, channel.ID)
	}
	if children[0].Provider != string(provider.Codex) || children[0].Model != thread.Model {
		t.Fatalf("default child = %+v, want parent provider/model fallback", children[0])
	}
	if children[1].Provider != string(provider.Claude) || children[1].Model != "claude-sonnet-4" {
		t.Fatalf("override child = %+v, want explicit provider/model", children[1])
	}

	sort.Strings(started)
	if len(started) != 2 || started[0] == started[1] {
		t.Fatalf("started participant sessions = %v, want two distinct child sessions", started)
	}
	for _, child := range children {
		if prompt := app.threadSystemPrompt(child.ID); prompt == "" {
			t.Fatalf("expected system prompt for child %s", child.ID)
		}
	}
}

func TestStartDiscussionCleansUpOnParticipantSessionFailure(t *testing.T) {
	app := newTestAppWithStore(t)
	app.registry = discussion.NewRegistry(app.store)
	app.channels = discussion.NewChannelService(app.store, app.highlighter)

	thread := testThread("thread-discussion-fail")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.CreateDiscussionDef(store.DiscussionDefinition{
		ID:    "def-fail",
		Name:  "Architects",
		Scope: "global",
		Participants: []store.DiscussionParticipant{
			{Role: "architect", System: "Design the change"},
			{Role: "reviewer", System: "Review the change"},
		},
		Settings:  store.DiscussionSettings{MaxTurns: 7},
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("CreateDiscussionDef() error = %v", err)
	}

	startCount := 0
	app.startSessionFn = func(threadID string) error {
		startCount++
		if startCount == 2 {
			return errDiscussionStartFailed
		}
		return nil
	}
	app.stopSessionFn = func(threadID string) error { return nil }

	if err := app.StartDiscussion(thread.ID, "Architects"); err == nil {
		t.Fatal("expected StartDiscussion() error on participant session failure")
	}

	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if updated.Mode != "chat" || updated.DiscussionID != "" {
		t.Fatalf("parent thread after failed start = %+v, want no discussion state", updated)
	}

	threads, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads() error = %v", err)
	}
	if len(threads) != 1 {
		t.Fatalf("len(threads) = %d, want only parent thread after cleanup", len(threads))
	}
	if len(app.threadSystemPrompts) != 0 {
		t.Fatalf("threadSystemPrompts = %v, want empty after cleanup", app.threadSystemPrompts)
	}
	if len(app.deliberations) != 0 {
		t.Fatalf("deliberations = %v, want empty after cleanup", app.deliberations)
	}
}

func TestStartDiscussionMirrorsEarlyParticipantTurnDuringStartup(t *testing.T) {
	app := newTestAppWithStore(t)
	app.registry = discussion.NewRegistry(app.store)
	app.channels = discussion.NewChannelService(app.store, app.highlighter)
	app.triage = triage.NewRouter(app.store, func(string, any) {}, app.highlighter)

	thread := testThread("thread-discussion-early-turn")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.CreateDiscussionDef(store.DiscussionDefinition{
		ID:        "def-early-turn",
		Name:      "Architects",
		Scope:     "project",
		ProjectID: projectPathForThread(t, app, thread),
		Participants: []store.DiscussionParticipant{
			{Role: "architect", System: "Design the change"},
			{Role: "reviewer", System: "Review the change"},
		},
		Settings:  store.DiscussionSettings{MaxTurns: 6},
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("CreateDiscussionDef() error = %v", err)
	}

	startCount := 0
	app.startSessionFn = func(threadID string) error {
		startCount++
		if startCount != 1 {
			return nil
		}

		now := time.Now().UnixMilli()
		if err := app.store.InsertItem(store.Item{
			ID:        "early-turn-item",
			ThreadID:  threadID,
			TurnIndex: 0,
			ItemIndex: 0,
			Kind:      "assistant_text",
			Role:      "assistant",
			Summary:   "Lead with the migration boundary before branching out.",
			CreatedAt: now,
		}); err != nil {
			return err
		}

		app.sessionEventHandler(threadID, "session-"+threadID)(provider.ProviderEvent{
			Kind:      provider.EventTurnComplete,
			ThreadID:  threadID,
			Timestamp: time.UnixMilli(now),
		})
		return nil
	}

	if err := app.StartDiscussion(thread.ID, "Architects"); err != nil {
		t.Fatalf("StartDiscussion() error = %v", err)
	}

	parent, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if parent.DiscussionID == "" {
		t.Fatal("expected discussion channel ID on parent thread")
	}

	messages, err := app.GetChannelMessages(parent.DiscussionID, -1, 10)
	if err != nil {
		t.Fatalf("GetChannelMessages() error = %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1 mirrored early-turn message", len(messages))
	}
	if messages[0].FromRole != "Architect" {
		t.Fatalf("messages[0].FromRole = %q, want Architect", messages[0].FromRole)
	}
	if messages[0].Content != "Lead with the migration boundary before branching out." {
		t.Fatalf("messages[0].Content = %q", messages[0].Content)
	}
}

func TestPostChannelMessageAndGetChannelMessages(t *testing.T) {
	app := newTestAppWithStore(t)
	app.registry = discussion.NewRegistry(app.store)
	app.channels = discussion.NewChannelService(app.store, app.highlighter)

	thread := store.Thread{
		ID:              "thread-channel",
		ProjectID:     defaultTestProjectID,
		Title:           "Channel Thread",
		Provider:        string(provider.Codex),
		WorkspacePath:   "/tmp/workspace",
		Model:           "gpt-5.4",
		Mode: "discussion",
		CreatedAt:       time.Now().UnixMilli(),
		UpdatedAt:       time.Now().UnixMilli(),
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	channel, err := app.channels.Create(thread.ID, "deliberation")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := app.PostChannelMessage(channel.ID, "Human intervention"); err != nil {
		t.Fatalf("PostChannelMessage() error = %v", err)
	}

	messages, err := app.GetChannelMessages(channel.ID, -1, 10)
	if err != nil {
		t.Fatalf("GetChannelMessages() error = %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	if messages[0].FromType != "human" || messages[0].Content != "Human intervention" {
		t.Fatalf("message = %+v, want human intervention", messages[0])
	}
}

func TestDeleteThreadRemovesDiscussionChildrenAndRuntimeState(t *testing.T) {
	app := newTestAppWithStore(t)
	app.registry = discussion.NewRegistry(app.store)
	app.channels = discussion.NewChannelService(app.store, app.highlighter)

	thread := testThread("thread-discussion-delete")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.CreateDiscussionDef(store.DiscussionDefinition{
		ID:        "def-delete",
		Name:      "Architects",
		Scope:     "project",
		ProjectID: projectPathForThread(t, app, thread),
		Participants: []store.DiscussionParticipant{
			{Role: "architect", System: "Design the change"},
			{Role: "reviewer", System: "Review the change"},
		},
		Settings:  store.DiscussionSettings{MaxTurns: 6},
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("CreateDiscussionDef() error = %v", err)
	}

	app.startSessionFn = func(threadID string) error { return nil }
	if err := app.StartDiscussion(thread.ID, "Architects"); err != nil {
		t.Fatalf("StartDiscussion() error = %v", err)
	}

	parent, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread(parent): %v", err)
	}
	children, err := app.store.ListChildThreads(parent.ID)
	if err != nil {
		t.Fatalf("ListChildThreads(): %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("len(children) = %d, want 2", len(children))
	}

	app.mu.Lock()
	for _, child := range children {
		app.sessions[child.ID] = session{provider: child.Provider}
	}
	app.mu.Unlock()

	if err := app.DeleteThread(parent.ID); err != nil {
		t.Fatalf("DeleteThread() error = %v", err)
	}

	threads, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads() error = %v", err)
	}
	if len(threads) != 0 {
		t.Fatalf("len(threads) = %d, want 0 after parent deletion", len(threads))
	}

	if _, err := app.store.GetChannel(parent.DiscussionID); err == nil {
		t.Fatal("expected discussion channel to be deleted with parent thread")
	}
	if len(app.threadSystemPrompts) != 0 {
		t.Fatalf("threadSystemPrompts = %v, want empty after delete", app.threadSystemPrompts)
	}

	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.sessions) != 0 {
		t.Fatalf("sessions = %v, want empty after delete", app.sessions)
	}
	if len(app.deliberations) != 0 {
		t.Fatalf("deliberations = %v, want empty after delete", app.deliberations)
	}
}

func TestSessionEventHandlerMirrorsDiscussionTurnsIntoChannelAndConcludes(t *testing.T) {
	app := newTestAppWithStore(t)
	app.channels = discussion.NewChannelService(app.store, app.highlighter)
	app.triage = triage.NewRouter(app.store, func(string, any) {}, app.highlighter)

	now := time.Now().UnixMilli()
	parent := store.Thread{
		ID:              "thread-parent",
		ProjectID:     defaultTestProjectID,
		Title:           "Architecture Review",
		Provider:        string(provider.Codex),
		WorkspacePath:   "/tmp/workspace",
		Model:           "gpt-5.4",
		Mode: "discussion",
		DiscussionID:    "channel-1",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := app.store.CreateThread(parent); err != nil {
		t.Fatalf("CreateThread(parent) error = %v", err)
	}

	children := []store.Thread{
		{
			ID:             "thread-child-a",
			ProjectID:      parent.ProjectID,
			Title:          "Architecture Review - Architect",
			Provider:       string(provider.Codex),
			WorkspacePath:  parent.WorkspacePath,
			Model:          parent.Model,
			Mode:           "discussion",
			DiscussionID:   parent.DiscussionID,
			ParentThreadID: parent.ID,
			CreatedAt:      now,
			UpdatedAt:      now,
		},
		{
			ID:             "thread-child-b",
			ProjectID:      parent.ProjectID,
			Title:          "Architecture Review - Reviewer",
			Provider:       string(provider.Codex),
			WorkspacePath:  parent.WorkspacePath,
			Model:          parent.Model,
			Mode:           "discussion",
			DiscussionID:   parent.DiscussionID,
			ParentThreadID: parent.ID,
			CreatedAt:      now + 1,
			UpdatedAt:      now + 1,
		},
	}
	for _, child := range children {
		if err := app.store.CreateThread(child); err != nil {
			t.Fatalf("CreateThread(%s) error = %v", child.ID, err)
		}
	}

	if err := app.store.CreateChannel(store.Channel{
		ID:        parent.DiscussionID,
		ThreadID:  parent.ID,
		Type:      "deliberation",
		Status:    "open",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateChannel() error = %v", err)
	}
	app.installDeliberation(parent.DiscussionID, 2)

	firstHandler := app.sessionEventHandler(children[0].ID, "session-a")
	firstHandler(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  children[0].ID,
		Content:   "Start with a bounded migration.",
		Timestamp: time.UnixMilli(now),
	})
	firstHandler(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  children[0].ID,
		Timestamp: time.UnixMilli(now + 5),
	})

	messages, err := app.GetChannelMessages(parent.DiscussionID, -1, 10)
	if err != nil {
		t.Fatalf("GetChannelMessages(first) error = %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("len(messages) after first turn = %d, want 1", len(messages))
	}
	if messages[0].FromType != "agent" || messages[0].FromID != children[0].ID {
		t.Fatalf("first mirrored message = %+v, want agent post from first child", messages[0])
	}
	if messages[0].FromRole != "Architect" {
		t.Fatalf("first mirrored role = %q, want Architect", messages[0].FromRole)
	}
	if messages[0].Content != "Start with a bounded migration." {
		t.Fatalf("first mirrored content = %q", messages[0].Content)
	}

	state := app.deliberations[parent.DiscussionID].State()
	if state.TurnCount != 1 || state.Concluded {
		t.Fatalf("state after first turn = %+v, want turnCount=1 and not concluded", state)
	}

	secondHandler := app.sessionEventHandler(children[1].ID, "session-b")
	secondHandler(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  children[1].ID,
		Content:   "Agreed, and the rollout should stay incremental.",
		Timestamp: time.UnixMilli(now + 10),
	})
	secondHandler(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  children[1].ID,
		Timestamp: time.UnixMilli(now + 15),
	})

	messages, err = app.GetChannelMessages(parent.DiscussionID, -1, 10)
	if err != nil {
		t.Fatalf("GetChannelMessages(second) error = %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("len(messages) after second turn = %d, want 2", len(messages))
	}
	if messages[1].FromRole != "Reviewer" {
		t.Fatalf("second mirrored role = %q, want Reviewer", messages[1].FromRole)
	}

	channel, err := app.store.GetChannel(parent.DiscussionID)
	if err != nil {
		t.Fatalf("GetChannel() error = %v", err)
	}
	if channel.Status != "concluded" {
		t.Fatalf("channel.Status = %q, want concluded", channel.Status)
	}

	state = app.deliberations[parent.DiscussionID].State()
	if !state.Concluded || state.TurnCount != 2 {
		t.Fatalf("state after second turn = %+v, want concluded after 2 turns", state)
	}

	if err := app.PostChannelMessage(parent.DiscussionID, "Can we keep going?"); err == nil {
		t.Fatal("expected posting to concluded discussion channel to fail")
	}
}

// TestStartDiscussionRejectsThreadWithChatHistory guards the "pick a
// lane" rule: once a thread has carried a normal chat turn, promoting
// it into a discussion would hide the existing messages behind
// DiscussionView and leave them unreachable. The server refuses the
// transition and the frontend picker also hides the entry-point; this
// test covers the server half.
func TestStartDiscussionRejectsThreadWithChatHistory(t *testing.T) {
	app := newTestAppWithStore(t)
	app.registry = discussion.NewRegistry(app.store)
	app.channels = discussion.NewChannelService(app.store, app.highlighter)

	thread := testThread("thread-discussion-used-chat")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.store.CreateDiscussionDef(store.DiscussionDefinition{
		ID:        "def-used",
		Name:      "Architects",
		Scope:     "project",
		ProjectID: projectPathForThread(t, app, thread),
		Participants: []store.DiscussionParticipant{
			{Role: "architect", System: "Design the change"},
		},
		Settings:  store.DiscussionSettings{MaxTurns: 3},
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("CreateDiscussionDef: %v", err)
	}

	now := time.Now().UnixMilli()
	if err := app.store.InsertItem(store.Item{
		ID:        "used-chat-item-1",
		ThreadID:  thread.ID,
		TurnIndex: 1,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "hey",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}

	err := app.StartDiscussion(thread.ID, "Architects")
	if err == nil {
		t.Fatal("StartDiscussion on used chat error = nil, want rejection")
	}
	if !strings.Contains(err.Error(), "chat history") {
		t.Fatalf("StartDiscussion error = %v, want 'chat history' context", err)
	}

	after, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if after.Mode != "chat" {
		t.Fatalf("Mode = %q after rejected StartDiscussion, want chat (no mutation)", after.Mode)
	}
	if after.DiscussionID != "" {
		t.Fatalf("DiscussionID = %q after rejected StartDiscussion, want empty", after.DiscussionID)
	}
}

func TestStartDiscussionRejectsEmptyName(t *testing.T) {
	app := newTestAppWithStore(t)
	app.registry = discussion.NewRegistry(app.store)
	app.channels = discussion.NewChannelService(app.store, app.highlighter)

	thread := testThread("thread-discussion-empty-name")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	tests := []struct {
		name  string
		input string
	}{
		{name: "empty string", input: ""},
		{name: "whitespace only", input: "   "},
		{name: "tabs and spaces", input: "\t  \t"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := app.StartDiscussion(thread.ID, tt.input)
			if err == nil {
				t.Fatal("StartDiscussion() error = nil, want error for empty name")
			}
			if !strings.Contains(err.Error(), "discussion name is required") {
				t.Fatalf("StartDiscussion() error = %v, want 'discussion name is required'", err)
			}
		})
	}
}
