package main

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	app.channels = discussion.NewChannelService(app.store)

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
	app.channels = discussion.NewChannelService(app.store)

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
	app.channels = discussion.NewChannelService(app.store)

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
	app.channels = discussion.NewChannelService(app.store)
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	// The synthesized early turn-complete below advances the FSM and
	// arms a next-speaker prompt on a background goroutine
	// (promptDiscussionSpeakerAsync). Stub the dispatch so the test
	// stays deterministic instead of falling through to a real send
	// with no registered provider session.
	app.sendMessageFn = func(string, string, []string) error { return nil }

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

		app.sessionEventHandler(threadID, "session-"+threadID, "")(provider.ProviderEvent{
			Kind:         provider.EventTurnComplete,
			ThreadID:     threadID,
			TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
			Timestamp:    time.UnixMilli(now),
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
	app.channels = discussion.NewChannelService(app.store)

	thread := store.Thread{
		ID:            "thread-channel",
		ProjectID:     defaultTestProjectID,
		Title:         "Channel Thread",
		Provider:      string(provider.Codex),
		WorkspacePath: "/tmp/workspace",
		Model:         "gpt-5.4",
		Mode:          "discussion",
		CreatedAt:     time.Now().UnixMilli(),
		UpdatedAt:     time.Now().UnixMilli(),
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	channel, err := app.channels.Create(thread.ID, "deliberation", 5)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if _, err := app.PostChannelMessage(channel.ID, "Human intervention"); err != nil {
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
	app.channels = discussion.NewChannelService(app.store)

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
	app.channels = discussion.NewChannelService(app.store)
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	// The first participant's turn-complete below advances the FSM and
	// arms a next-speaker prompt on a background goroutine
	// (promptDiscussionSpeakerAsync). Stub the dispatch so the test
	// stays deterministic instead of falling through to a real send
	// with no registered provider session.
	app.sendMessageFn = func(string, string, []string) error { return nil }

	// Threads + channels have a circular FK relationship
	// (channel.thread_id → threads.id, threads.discussion_id → channels.id).
	// Production resolves it by creating threads first without
	// discussion_id, creating the channel, then UPDATE-ing each
	// thread's discussion_id. Tests follow the same pattern.
	now := time.Now().UnixMilli()
	parent := store.Thread{
		ID:            "thread-parent",
		ProjectID:     defaultTestProjectID,
		Title:         "Architecture Review",
		Provider:      string(provider.Codex),
		WorkspacePath: "/tmp/workspace",
		Model:         "gpt-5.4",
		Mode:          "discussion",
		CreatedAt:     now,
		UpdatedAt:     now,
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

	const channelID = "channel-1"
	if err := app.store.CreateChannel(store.Channel{
		ID:        channelID,
		ThreadID:  parent.ID,
		Type:      "deliberation",
		Status:    "open",
		MaxTurns:  2,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateChannel() error = %v", err)
	}
	parent.DiscussionID = channelID
	if err := app.store.UpdateThread(parent); err != nil {
		t.Fatalf("UpdateThread(parent) error = %v", err)
	}
	for i := range children {
		children[i].DiscussionID = channelID
		if err := app.store.UpdateThread(children[i]); err != nil {
			t.Fatalf("UpdateThread(%s) error = %v", children[i].ID, err)
		}
	}
	app.installDeliberation(channelID, []string{children[0].ID, children[1].ID}, 2)

	firstHandler := app.sessionEventHandler(children[0].ID, "session-a", "")
	firstHandler(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  children[0].ID,
		Content:   "Start with a bounded migration.",
		Timestamp: time.UnixMilli(now),
	})
	firstHandler(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     children[0].ID,
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
		Timestamp:    time.UnixMilli(now + 5),
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

	secondHandler := app.sessionEventHandler(children[1].ID, "session-b", "")
	secondHandler(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  children[1].ID,
		Content:   "Agreed, and the rollout should stay incremental.",
		Timestamp: time.UnixMilli(now + 10),
	})
	secondHandler(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     children[1].ID,
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
		Timestamp:    time.UnixMilli(now + 15),
	})

	messages, err = app.GetChannelMessages(parent.DiscussionID, -1, 10)
	if err != nil {
		t.Fatalf("GetChannelMessages(second) error = %v", err)
	}
	// Reaching MaxTurns now posts a third, system-authored conclusion
	// message into the channel (concludeDiscussionChannel) alongside
	// the two agent turns.
	if len(messages) != 3 {
		t.Fatalf("len(messages) after second turn = %d, want 3 (two agent turns + conclusion notice)", len(messages))
	}
	if messages[1].FromRole != "Reviewer" {
		t.Fatalf("second mirrored role = %q, want Reviewer", messages[1].FromRole)
	}
	if messages[2].FromType != "system" || messages[2].FromRole != "moderator" {
		t.Fatalf("conclusion message = %+v, want system/moderator", messages[2])
	}
	if !strings.Contains(messages[2].Content, "2-turn limit") {
		t.Fatalf("conclusion message content = %q, want mention of the 2-turn limit", messages[2].Content)
	}

	channel, err := app.store.GetChannel(parent.DiscussionID)
	if err != nil {
		t.Fatalf("GetChannel() error = %v", err)
	}
	if channel.Status != "concluded" {
		t.Fatalf("channel.Status = %q, want concluded", channel.Status)
	}

	// A successful conclusion drops the FSM from a.deliberations — a
	// concluded channel has nothing left to coordinate, and retaining
	// the entry until thread deletion would leak it.
	if _, ok := app.deliberation(parent.DiscussionID); ok {
		t.Fatal("expected the concluded deliberation to be removed from a.deliberations")
	}

	// GetChannelState must still serve a coherent concluded snapshot
	// via buildChannelState's SQLite fallback branch.
	payload, err := app.GetChannelState(parent.DiscussionID)
	if err != nil {
		t.Fatalf("GetChannelState() after conclusion error = %v", err)
	}
	if payload.Status != "concluded" {
		t.Fatalf("payload.Status = %q, want concluded", payload.Status)
	}
	if payload.TurnCount != 2 || payload.MaxTurns != 2 {
		t.Fatalf("payload turn counts = %d/%d, want 2/2", payload.TurnCount, payload.MaxTurns)
	}
	if len(payload.Participants) != 2 ||
		payload.Participants[0].ThreadID != children[0].ID ||
		payload.Participants[1].ThreadID != children[1].ID {
		t.Fatalf("payload.Participants = %+v, want both children in roster order", payload.Participants)
	}

	if _, err := app.PostChannelMessage(parent.DiscussionID, "Can we keep going?"); err == nil {
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
	app.channels = discussion.NewChannelService(app.store)

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

func TestStartDiscussionByIDRejectsProjectDefinitionFromAnotherProject(t *testing.T) {
	app := newTestAppWithStore(t)
	app.registry = discussion.NewRegistry(app.store)
	app.channels = discussion.NewChannelService(app.store)

	source, err := createTestThread(t, app, "claude", "/tmp/discussion-source-project", "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("create source thread: %v", err)
	}
	target, err := createTestThread(t, app, "claude", "/tmp/discussion-target-project", "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("create target thread: %v", err)
	}

	def := store.DiscussionDefinition{
		ID:        "source-project-def",
		Name:      "Project Architects",
		Scope:     "project",
		ProjectID: projectPathForThread(t, app, source),
		Participants: []store.DiscussionParticipant{
			{Role: "architect", System: "Design the change"},
			{Role: "reviewer", System: "Review the change"},
		},
		Settings:  store.DiscussionSettings{MaxTurns: 3},
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}
	if err := app.store.CreateDiscussionDef(def); err != nil {
		t.Fatalf("CreateDiscussionDef: %v", err)
	}

	err = app.StartDiscussionByID(target.ID, def.ID)
	if err == nil {
		t.Fatal("StartDiscussionByID cross-project error = nil, want rejection")
	}
	if !strings.Contains(err.Error(), "different project") {
		t.Fatalf("StartDiscussionByID error = %v, want different project context", err)
	}
}

func TestStartDiscussionRejectsEmptyName(t *testing.T) {
	app := newTestAppWithStore(t)
	app.registry = discussion.NewRegistry(app.store)
	app.channels = discussion.NewChannelService(app.store)

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

// sendCapture is a small thread-safe recorder for app.sendMessageFn
// calls, used by the turn-driving tests below to observe what the
// (stubbed) provider dispatch actually received without racing the
// background goroutine that makes the call.
type sendCapture struct {
	threadID string
	content  string
}

func newSendCaptureChan() (chan sendCapture, func(string, string, []string) error) {
	ch := make(chan sendCapture, 8)
	return ch, func(threadID, content string, _ []string) error {
		ch <- sendCapture{threadID: threadID, content: content}
		return nil
	}
}

func awaitSendCapture(t *testing.T, ch chan sendCapture) sendCapture {
	t.Helper()
	select {
	case call := <-ch:
		return call
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a discussion turn prompt dispatch")
		return sendCapture{}
	}
}

func assertNoSendCapture(t *testing.T, ch chan sendCapture, window time.Duration) {
	t.Helper()
	select {
	case unexpected := <-ch:
		t.Fatalf("unexpected extra prompt dispatch: %+v", unexpected)
	case <-time.After(window):
	}
}

// TestPostChannelMessageClaimsCurrentSpeakerAndSuppressesWhileAwaiting
// covers PostChannelMessage's turn-driving contract end to end: a
// human post kicks off the first participant's turn exactly once, the
// dispatched prompt carries the human's own message, and a second
// human post landing before that participant replies does not
// double-prompt.
func TestPostChannelMessageClaimsCurrentSpeakerAndSuppressesWhileAwaiting(t *testing.T) {
	app := newTestAppWithStore(t)
	app.registry = discussion.NewRegistry(app.store)
	app.channels = discussion.NewChannelService(app.store)
	app.startSessionFn = func(string) error { return nil }

	sendCh, sendFn := newSendCaptureChan()
	app.sendMessageFn = sendFn

	var mu sync.Mutex
	var emitted []string
	app.testEmitHook = func(name string, _ any) {
		mu.Lock()
		defer mu.Unlock()
		emitted = append(emitted, name)
	}

	thread := testThread("thread-discussion-prompt-once")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.CreateDiscussionDef(store.DiscussionDefinition{
		ID:        "def-prompt-once",
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
	if err := app.StartDiscussion(thread.ID, "Architects"); err != nil {
		t.Fatalf("StartDiscussion() error = %v", err)
	}

	parent, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	children, err := app.store.ListChildThreads(parent.ID)
	if err != nil {
		t.Fatalf("ListChildThreads() error = %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("len(children) = %d, want 2", len(children))
	}

	if _, err := app.PostChannelMessage(parent.DiscussionID, "Let's evaluate the migration boundary."); err != nil {
		t.Fatalf("PostChannelMessage() error = %v", err)
	}

	mu.Lock()
	gotEvents := append([]string(nil), emitted...)
	mu.Unlock()
	if !containsString(gotEvents, "discussion:message") || !containsString(gotEvents, "discussion:state") {
		t.Fatalf("emitted events = %v, want discussion:message and discussion:state", gotEvents)
	}

	first := awaitSendCapture(t, sendCh)
	if first.threadID != children[0].ID {
		t.Fatalf("first prompt threadID = %q, want %q (first participant in roster order)", first.threadID, children[0].ID)
	}
	if !strings.Contains(first.content, "Let's evaluate the migration boundary.") {
		t.Fatalf("first prompt content = %q, want to contain the human's kickoff message", first.content)
	}
	if !strings.Contains(first.content, "Human:") {
		t.Fatalf("first prompt content = %q, want a Human: label", first.content)
	}

	d, ok := app.deliberation(parent.DiscussionID)
	if !ok {
		t.Fatal("expected an installed deliberation")
	}
	if state := d.State(); !state.AwaitingResponse || state.CurrentSpeaker != children[0].ID {
		t.Fatalf("state after first prompt = %+v, want AwaitingResponse=true, CurrentSpeaker=%q", state, children[0].ID)
	}

	// A second human post while the first participant hasn't replied
	// yet (AwaitingResponse) must not dispatch a second prompt.
	if _, err := app.PostChannelMessage(parent.DiscussionID, "Any early thoughts?"); err != nil {
		t.Fatalf("PostChannelMessage() (second) error = %v", err)
	}
	assertNoSendCapture(t, sendCh, 200*time.Millisecond)
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestSyncDiscussionTurnPromptsNextSpeakerWithMessagesSinceOwnLastPost
// covers the unseen-messages windowing promptDiscussionSpeaker builds
// from LastChannelMessageSeqFrom: a participant's SECOND prompt must
// contain only what was posted after its own previous post, not the
// entire channel history again.
func TestSyncDiscussionTurnPromptsNextSpeakerWithMessagesSinceOwnLastPost(t *testing.T) {
	app := newTestAppWithStore(t)
	app.registry = discussion.NewRegistry(app.store)
	app.channels = discussion.NewChannelService(app.store)
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	app.startSessionFn = func(string) error { return nil }

	sendCh, sendFn := newSendCaptureChan()
	app.sendMessageFn = sendFn

	thread := testThread("thread-discussion-window")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.CreateDiscussionDef(store.DiscussionDefinition{
		ID:        "def-window",
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
	if err := app.StartDiscussion(thread.ID, "Architects"); err != nil {
		t.Fatalf("StartDiscussion() error = %v", err)
	}

	parent, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	children, err := app.store.ListChildThreads(parent.ID)
	if err != nil {
		t.Fatalf("ListChildThreads() error = %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("len(children) = %d, want 2", len(children))
	}
	architect, reviewer := children[0], children[1]

	// Kick off the discussion; architect gets prompted first.
	if _, err := app.PostChannelMessage(parent.DiscussionID, "Topic: evaluate the migration boundary."); err != nil {
		t.Fatalf("PostChannelMessage(kickoff) error = %v", err)
	}
	awaitSendCapture(t, sendCh)

	// Architect's turn completes. Its first-ever prompt to reviewer
	// naturally carries the whole history so far (reviewer has never
	// posted, so it hasn't "seen" anything yet).
	now := time.Now().UnixMilli()
	if err := app.store.InsertItem(store.Item{
		ID: "item-architect-1", ThreadID: architect.ID, TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant", Summary: "Architect's opening take.", CreatedAt: now,
	}); err != nil {
		t.Fatalf("InsertItem(architect): %v", err)
	}
	if err := app.syncDiscussionTurn(architect.ID); err != nil {
		t.Fatalf("syncDiscussionTurn(architect): %v", err)
	}
	toReviewer := awaitSendCapture(t, sendCh)
	if toReviewer.threadID != reviewer.ID {
		t.Fatalf("prompt threadID = %q, want %q (reviewer)", toReviewer.threadID, reviewer.ID)
	}
	if !strings.Contains(toReviewer.content, "Topic: evaluate the migration boundary.") {
		t.Fatalf("reviewer's first prompt = %q, want the human kickoff included", toReviewer.content)
	}
	if !strings.Contains(toReviewer.content, "Architect's opening take.") {
		t.Fatalf("reviewer's first prompt = %q, want the architect's reply included", toReviewer.content)
	}

	// Reviewer's turn completes. Architect's SECOND prompt must only
	// contain what was posted after architect's own last post — not
	// the kickoff or architect's own prior reply again.
	if err := app.store.InsertItem(store.Item{
		ID: "item-reviewer-1", ThreadID: reviewer.ID, TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant", Summary: "Reviewer's rebuttal.", CreatedAt: now + 1,
	}); err != nil {
		t.Fatalf("InsertItem(reviewer): %v", err)
	}
	if err := app.syncDiscussionTurn(reviewer.ID); err != nil {
		t.Fatalf("syncDiscussionTurn(reviewer): %v", err)
	}
	toArchitect := awaitSendCapture(t, sendCh)
	if toArchitect.threadID != architect.ID {
		t.Fatalf("prompt threadID = %q, want %q (architect)", toArchitect.threadID, architect.ID)
	}
	if !strings.Contains(toArchitect.content, "Reviewer's rebuttal.") {
		t.Fatalf("architect's second prompt = %q, want the reviewer's reply included", toArchitect.content)
	}
	if strings.Contains(toArchitect.content, "Topic: evaluate the migration boundary.") {
		t.Fatalf("architect's second prompt = %q, want the already-seen kickoff excluded", toArchitect.content)
	}
	if strings.Contains(toArchitect.content, "Architect's opening take.") {
		t.Fatalf("architect's second prompt = %q, want architect's own prior post excluded", toArchitect.content)
	}
}

// TestSyncDiscussionTurnToolOnlyStillAdvancesAndPromptsNextSpeaker
// covers the stall fix: a turn with no assistant text (e.g. the
// participant only ran tools) must not post anything into the channel,
// but the FSM still has to advance and prompt the next speaker —
// otherwise a tool-only turn would silently leave the deliberation
// awaiting a reply forever.
func TestSyncDiscussionTurnToolOnlyStillAdvancesAndPromptsNextSpeaker(t *testing.T) {
	app := newTestAppWithStore(t)
	app.registry = discussion.NewRegistry(app.store)
	app.channels = discussion.NewChannelService(app.store)
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	app.startSessionFn = func(string) error { return nil }

	sendCh, sendFn := newSendCaptureChan()
	app.sendMessageFn = sendFn

	thread := testThread("thread-discussion-tool-only")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.CreateDiscussionDef(store.DiscussionDefinition{
		ID:        "def-tool-only",
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
	if err := app.StartDiscussion(thread.ID, "Architects"); err != nil {
		t.Fatalf("StartDiscussion() error = %v", err)
	}

	parent, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	children, err := app.store.ListChildThreads(parent.ID)
	if err != nil {
		t.Fatalf("ListChildThreads() error = %v", err)
	}
	architect, reviewer := children[0], children[1]

	if _, err := app.PostChannelMessage(parent.DiscussionID, "Kick things off."); err != nil {
		t.Fatalf("PostChannelMessage() error = %v", err)
	}
	awaitSendCapture(t, sendCh)

	// Architect's turn ran tools only — a tool_call item, no
	// assistant_text — and completed. No InsertItem of kind
	// assistant_text at all for turn 0.
	now := time.Now().UnixMilli()
	if err := app.store.InsertItem(store.Item{
		ID: "item-architect-tool", ThreadID: architect.ID, TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Summary: "ran `go build`", CreatedAt: now,
	}); err != nil {
		t.Fatalf("InsertItem(tool_call): %v", err)
	}
	if err := app.syncDiscussionTurn(architect.ID); err != nil {
		t.Fatalf("syncDiscussionTurn(architect, tool-only): %v", err)
	}

	messages, err := app.GetChannelMessages(parent.DiscussionID, -1, 0)
	if err != nil {
		t.Fatalf("GetChannelMessages() error = %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1 (only the human kickoff; the tool-only turn posts nothing)", len(messages))
	}

	d, ok := app.deliberation(parent.DiscussionID)
	if !ok {
		t.Fatal("expected an installed deliberation")
	}
	if state := d.State(); state.TurnCount != 1 {
		t.Fatalf("TurnCount after tool-only turn = %d, want 1 (the FSM still advanced)", state.TurnCount)
	}

	toReviewer := awaitSendCapture(t, sendCh)
	if toReviewer.threadID != reviewer.ID {
		t.Fatalf("prompt threadID = %q, want %q (reviewer prompted despite the tool-only turn)", toReviewer.threadID, reviewer.ID)
	}
}

// TestDeliberationForChannelRebuildsFromStoreAfterRestart simulates an
// app restart: the channel, its participant threads, and channel
// message history all exist in SQLite, but no in-memory Deliberation
// was ever installed for it (a.deliberations starts empty, exactly
// like a freshly-started process). deliberationForChannel must
// reconstruct an equivalent FSM purely from store state.
func TestDeliberationForChannelRebuildsFromStoreAfterRestart(t *testing.T) {
	app := newTestAppWithStore(t)
	app.channels = discussion.NewChannelService(app.store)

	now := time.Now().UnixMilli()
	parent := store.Thread{
		ID: "thread-restart-parent", ProjectID: defaultTestProjectID, Title: "Design Review",
		Provider: string(provider.Codex), WorkspacePath: "/tmp/workspace", Model: "gpt-5.4",
		Mode: "discussion", CreatedAt: now, UpdatedAt: now,
	}
	if err := app.store.CreateThread(parent); err != nil {
		t.Fatalf("CreateThread(parent): %v", err)
	}

	roles := []string{"Architect", "Reviewer", "Scribe"}
	children := make([]store.Thread, len(roles))
	for i, role := range roles {
		children[i] = store.Thread{
			ID:             "thread-restart-" + role,
			ProjectID:      parent.ProjectID,
			Title:          parent.Title + " - " + role,
			Provider:       parent.Provider,
			WorkspacePath:  parent.WorkspacePath,
			Model:          parent.Model,
			Mode:           "discussion",
			ParentThreadID: parent.ID,
			CreatedAt:      now + int64(i),
			UpdatedAt:      now + int64(i),
		}
		if err := app.store.CreateThread(children[i]); err != nil {
			t.Fatalf("CreateThread(%s): %v", role, err)
		}
	}

	const channelID = "channel-restart"
	if err := app.store.CreateChannel(store.Channel{
		ID: channelID, ThreadID: parent.ID, Type: "deliberation", Status: "open",
		MaxTurns: 4, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateChannel(): %v", err)
	}
	parent.DiscussionID = channelID
	if err := app.store.UpdateThread(parent); err != nil {
		t.Fatalf("UpdateThread(parent): %v", err)
	}
	for i := range children {
		children[i].DiscussionID = channelID
		if err := app.store.UpdateThread(children[i]); err != nil {
			t.Fatalf("UpdateThread(%s): %v", children[i].ID, err)
		}
	}

	// Message history: human kickoff, then architect and reviewer each
	// post once. Scribe hasn't spoken yet, so round-robin order says
	// scribe is next.
	post := func(fromType, fromID, fromRole, content string) {
		t.Helper()
		if _, err := app.channels.PostMessage(discussion.PostMessageInput{
			ChannelID: channelID, FromType: fromType, FromID: fromID, FromRole: fromRole, Content: content,
		}); err != nil {
			t.Fatalf("PostMessage(%s): %v", fromID, err)
		}
	}
	post("human", "user", "human", "Topic: evaluate the migration boundary.")
	post("agent", children[0].ID, "Architect", "Architect's take.")
	post("agent", children[1].ID, "Reviewer", "Reviewer's take.")

	// No installDeliberation call — this is the crux of the restart
	// scenario: a.deliberations has nothing for this channel.
	if _, ok := app.deliberation(channelID); ok {
		t.Fatal("expected no in-memory deliberation before rebuild — test setup bug")
	}

	rebuilt, err := app.deliberationForChannel(channelID)
	if err != nil {
		t.Fatalf("deliberationForChannel(): %v", err)
	}
	gotParticipants := rebuilt.Participants()
	wantParticipants := []string{children[0].ID, children[1].ID, children[2].ID}
	if len(gotParticipants) != len(wantParticipants) {
		t.Fatalf("Participants() = %v, want %v", gotParticipants, wantParticipants)
	}
	for i := range wantParticipants {
		if gotParticipants[i] != wantParticipants[i] {
			t.Fatalf("Participants()[%d] = %q, want %q (roster order)", i, gotParticipants[i], wantParticipants[i])
		}
	}

	state := rebuilt.State()
	if state.TurnCount != 2 {
		t.Fatalf("TurnCount = %d, want 2 (two agent posts)", state.TurnCount)
	}
	if state.MaxTurns != 4 {
		t.Fatalf("MaxTurns = %d, want 4 (from the channel row)", state.MaxTurns)
	}
	if state.CurrentSpeaker != children[2].ID {
		t.Fatalf("CurrentSpeaker = %q, want %q (scribe, next after reviewer)", state.CurrentSpeaker, children[2].ID)
	}
	if state.AwaitingResponse {
		t.Fatal("AwaitingResponse must come back false across a restart")
	}
	if state.Concluded {
		t.Fatal("2 of 4 turns must not be concluded")
	}

	// A second resolution must reuse the same instance rather than
	// rebuilding again (double-checked locking in deliberationForChannel).
	again, err := app.deliberationForChannel(channelID)
	if err != nil {
		t.Fatalf("deliberationForChannel() (second call): %v", err)
	}
	if again != rebuilt {
		t.Fatal("expected the second resolution to reuse the same rebuilt Deliberation instance")
	}

	// The public GetChannelState binding must reflect the same
	// rebuilt state.
	payload, err := app.GetChannelState(channelID)
	if err != nil {
		t.Fatalf("GetChannelState(): %v", err)
	}
	if payload.TurnCount != 2 || payload.MaxTurns != 4 || payload.CurrentSpeakerThreadID != children[2].ID {
		t.Fatalf("GetChannelState() = %+v, want turnCount=2 maxTurns=4 currentSpeaker=%q", payload, children[2].ID)
	}
	if payload.CurrentSpeakerRole != "Scribe" {
		t.Fatalf("GetChannelState().CurrentSpeakerRole = %q, want Scribe", payload.CurrentSpeakerRole)
	}
	if len(payload.Participants) != 3 {
		t.Fatalf("len(GetChannelState().Participants) = %d, want 3", len(payload.Participants))
	}
}

// TestSyncDiscussionTurnAfterRestartCountsTriggeringTurnOnce guards
// the restart-rebuild double-count: syncDiscussionTurn must resolve
// the deliberation BEFORE mirroring the participant's post into the
// channel. When the process restarted mid-turn (a.deliberations is
// cold), deliberationForChannel reconstructs TurnCount from
// CountChannelMessagesByType("agent") — if the triggering turn's row
// was already committed, the rebuilt count includes it and the
// subsequent RecordPost increments it AGAIN, so N prior turns become
// N+2 instead of N+1 and the discussion concludes a turn early.
func TestSyncDiscussionTurnAfterRestartCountsTriggeringTurnOnce(t *testing.T) {
	app := newTestAppWithStore(t)
	app.channels = discussion.NewChannelService(app.store)
	// The advance arms the next speaker's prompt on a background
	// goroutine; stub the dispatch so the test stays deterministic.
	app.sendMessageFn = func(string, string, []string) error { return nil }

	parent, children := seedDiscussionChannel(t, app, "restart-count", []string{"Architect", "Reviewer"}, 6)
	channelID := parent.DiscussionID

	// N = 2 prior agent turns already in the channel history. No
	// installDeliberation — a.deliberations is cold, exactly like a
	// freshly restarted process.
	const priorAgentTurns = 2
	post := func(fromID, fromRole, content string) {
		t.Helper()
		if _, err := app.channels.PostMessage(discussion.PostMessageInput{
			ChannelID: channelID, FromType: "agent", FromID: fromID, FromRole: fromRole, Content: content,
		}); err != nil {
			t.Fatalf("PostMessage(%s): %v", fromID, err)
		}
	}
	post(children[0].ID, "Architect", "Architect's take.")
	post(children[1].ID, "Reviewer", "Reviewer's take.")

	// The architect (next in round-robin after the reviewer) completes
	// its turn only NOW — the mid-turn session the restart cut across.
	if err := app.store.InsertItem(store.Item{
		ID: "item-restart-count", ThreadID: children[0].ID, TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant", Summary: "Architect's follow-up.", CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertItem(): %v", err)
	}
	if err := app.syncDiscussionTurn(children[0].ID); err != nil {
		t.Fatalf("syncDiscussionTurn(): %v", err)
	}

	d, ok := app.deliberation(channelID)
	if !ok {
		t.Fatal("expected a rebuilt deliberation after syncDiscussionTurn")
	}
	if got := d.State().TurnCount; got != priorAgentTurns+1 {
		t.Fatalf("TurnCount = %d, want %d (the triggering turn must count exactly once)", got, priorAgentTurns+1)
	}
}

// seedDiscussionChannel creates a parent thread, one child thread per
// role, and an open deliberation channel linking them — the persisted
// shape StartDiscussion produces, without going through the
// definition/session machinery. Deliberately does NOT call
// installDeliberation: tests that need a live FSM install their own,
// and restart-shaped tests need the map cold. Returns the parent
// (DiscussionID set) and the children in roster order.
func seedDiscussionChannel(t *testing.T, app *App, idPrefix string, roles []string, maxTurns int) (store.Thread, []store.Thread) {
	t.Helper()

	now := time.Now().UnixMilli()
	parent := store.Thread{
		ID: "thread-" + idPrefix + "-parent", ProjectID: defaultTestProjectID, Title: "Design Review",
		Provider: string(provider.Codex), WorkspacePath: "/tmp/workspace", Model: "gpt-5.4",
		Mode: "discussion", CreatedAt: now, UpdatedAt: now,
	}
	if err := app.store.CreateThread(parent); err != nil {
		t.Fatalf("CreateThread(parent): %v", err)
	}

	children := make([]store.Thread, len(roles))
	for i, role := range roles {
		children[i] = store.Thread{
			ID:             "thread-" + idPrefix + "-" + role,
			ProjectID:      parent.ProjectID,
			Title:          parent.Title + " - " + role,
			Provider:       parent.Provider,
			WorkspacePath:  parent.WorkspacePath,
			Model:          parent.Model,
			Mode:           "discussion",
			ParentThreadID: parent.ID,
			CreatedAt:      now + int64(i),
			UpdatedAt:      now + int64(i),
		}
		if err := app.store.CreateThread(children[i]); err != nil {
			t.Fatalf("CreateThread(%s): %v", role, err)
		}
	}

	channelID := "channel-" + idPrefix
	if err := app.store.CreateChannel(store.Channel{
		ID: channelID, ThreadID: parent.ID, Type: "deliberation", Status: "open",
		MaxTurns: maxTurns, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateChannel(): %v", err)
	}
	parent.DiscussionID = channelID
	if err := app.store.UpdateThread(parent); err != nil {
		t.Fatalf("UpdateThread(parent): %v", err)
	}
	for i := range children {
		children[i].DiscussionID = channelID
		if err := app.store.UpdateThread(children[i]); err != nil {
			t.Fatalf("UpdateThread(%s): %v", children[i].ID, err)
		}
	}
	return parent, children
}

// TestSyncDiscussionTurnEmitsExactlyOneStatePerAdvance guards the
// discussion:state cadence: one turn advance produces exactly one
// state emission — the post-claim snapshot from
// claimAndPromptNextSpeaker — not an additional pre-claim snapshot
// that the claim's own emission would immediately supersede.
func TestSyncDiscussionTurnEmitsExactlyOneStatePerAdvance(t *testing.T) {
	app := newTestAppWithStore(t)
	app.channels = discussion.NewChannelService(app.store)

	sendCh, sendFn := newSendCaptureChan()
	app.sendMessageFn = sendFn

	var mu sync.Mutex
	stateEmits := 0
	app.testEmitHook = func(name string, _ any) {
		if name != "discussion:state" {
			return
		}
		mu.Lock()
		stateEmits++
		mu.Unlock()
	}

	parent, children := seedDiscussionChannel(t, app, "one-state", []string{"Architect", "Reviewer"}, 6)
	channelID := parent.DiscussionID
	app.installDeliberation(channelID, []string{children[0].ID, children[1].ID}, 6)

	if err := app.store.InsertItem(store.Item{
		ID: "item-one-state", ThreadID: children[0].ID, TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant", Summary: "Architect's take.", CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertItem(): %v", err)
	}
	if err := app.syncDiscussionTurn(children[0].ID); err != nil {
		t.Fatalf("syncDiscussionTurn(): %v", err)
	}

	// The claim's state emission happens synchronously before the
	// async dispatch hand-off, and a successful dispatch emits nothing
	// further — once the prompt lands, the count is final.
	awaitSendCapture(t, sendCh)

	mu.Lock()
	got := stateEmits
	mu.Unlock()
	if got != 1 {
		t.Fatalf("discussion:state emissions across one turn advance = %d, want exactly 1", got)
	}
}

// TestPostChannelMessageSelfHealsWedgedConcludedDeliberation covers
// the conclusion-persistence failure wedge: RecordPost flips the FSM
// to Concluded BEFORE concludeDiscussionChannel persists, so a
// persistence failure strands a concluded FSM against a channel row
// still "open" — TryClaimCurrentSpeaker refuses every claim and the
// discussion would wedge forever. The next human post is the retry
// trigger: maybePromptNextDiscussionSpeaker re-attempts the
// conclusion instead of claiming.
func TestPostChannelMessageSelfHealsWedgedConcludedDeliberation(t *testing.T) {
	app := newTestAppWithStore(t)
	app.channels = discussion.NewChannelService(app.store)

	sendCh, sendFn := newSendCaptureChan()
	app.sendMessageFn = sendFn

	var mu sync.Mutex
	var emitted []string
	app.testEmitHook = func(name string, _ any) {
		mu.Lock()
		emitted = append(emitted, name)
		mu.Unlock()
	}

	parent, children := seedDiscussionChannel(t, app, "wedged", []string{"Architect", "Reviewer"}, 2)
	channelID := parent.DiscussionID

	// Install the wedge shape directly: an FSM already concluded
	// (turnCount >= maxTurns) while the channel row is still "open".
	app.mu.Lock()
	app.deliberations[channelID] = discussion.RestoreDeliberation(
		channelID, 2, []string{children[0].ID, children[1].ID}, 2, "")
	app.mu.Unlock()

	if _, err := app.PostChannelMessage(channelID, "Anyone there?"); err != nil {
		t.Fatalf("PostChannelMessage() error = %v", err)
	}

	channel, err := app.store.GetChannel(channelID)
	if err != nil {
		t.Fatalf("GetChannel(): %v", err)
	}
	if channel.Status != "concluded" {
		t.Fatalf("channel.Status = %q, want concluded (self-heal must flip the row)", channel.Status)
	}

	messages, err := app.GetChannelMessages(channelID, -1, 0)
	if err != nil {
		t.Fatalf("GetChannelMessages(): %v", err)
	}
	last := messages[len(messages)-1]
	if last.FromType != "system" || !strings.Contains(last.Content, "2-turn limit") {
		t.Fatalf("last message = %+v, want the system conclusion notice", last)
	}

	mu.Lock()
	gotEvents := append([]string(nil), emitted...)
	mu.Unlock()
	if !containsString(gotEvents, "discussion:state") {
		t.Fatalf("emitted events = %v, want discussion:state after the self-heal", gotEvents)
	}

	// A successful re-conclusion also removes the FSM (same as the
	// normal conclude path).
	if _, ok := app.deliberation(channelID); ok {
		t.Fatal("expected the healed deliberation to be removed from a.deliberations")
	}

	// And no prompt was dispatched — a concluded discussion never
	// claims a speaker.
	assertNoSendCapture(t, sendCh, 200*time.Millisecond)
}

// TestDiscussionPromptDispatchFailureUnclaimsAndAllowsRetry covers the
// failed-dispatch recovery loop end to end: a dispatch failure must
// un-claim the speaker (AwaitingResponse cleared), re-emit
// discussion:state, surface the failure as thread error state on the
// parent thread, and leave the deliberation retryable — a second
// human post re-prompts the SAME CurrentSpeaker.
func TestDiscussionPromptDispatchFailureUnclaimsAndAllowsRetry(t *testing.T) {
	app := newTestAppWithStore(t)
	app.channels = discussion.NewChannelService(app.store)

	// Capture triage emissions so the test can await the wire-error
	// surfacing (promptDiscussionSpeakerAsync → emitWireErrorToThread →
	// triage persist + provider:item_event) without sleeping.
	triageEmitCh := make(chan string, 32)
	app.triage = triage.NewRouter(app.store, func(name string, _ any) {
		select {
		case triageEmitCh <- name:
		default:
		}
	})

	stateCh := make(chan struct{}, 16)
	app.testEmitHook = func(name string, _ any) {
		if name != "discussion:state" {
			return
		}
		select {
		case stateCh <- struct{}{}:
		default:
		}
	}

	dispatchErr := errors.New("provider session gone")
	sendCh := make(chan sendCapture, 8)
	var sendMu sync.Mutex
	sendCalls := 0
	app.sendMessageFn = func(threadID, content string, _ []string) error {
		sendMu.Lock()
		sendCalls++
		n := sendCalls
		sendMu.Unlock()
		if n == 1 {
			return dispatchErr
		}
		sendCh <- sendCapture{threadID: threadID, content: content}
		return nil
	}

	parent, children := seedDiscussionChannel(t, app, "dispatch-retry", []string{"Architect", "Reviewer"}, 6)
	channelID := parent.DiscussionID
	app.installDeliberation(channelID, []string{children[0].ID, children[1].ID}, 6)

	if _, err := app.PostChannelMessage(channelID, "Kick off."); err != nil {
		t.Fatalf("PostChannelMessage(kickoff) error = %v", err)
	}

	// Two state snapshots land for the failed cycle: the synchronous
	// post-claim emission, then the async un-claim after the dispatch
	// error.
	for i, label := range []string{"post-claim", "un-claim"} {
		select {
		case <-stateCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for the %s discussion:state (emission %d)", label, i+1)
		}
	}

	// The failure surfaces on the parent thread. persistItem writes to
	// SQLite before emitting, so once the provider:item_event lands the
	// error row is queryable.
	deadline := time.After(2 * time.Second)
	for {
		var name string
		select {
		case name = <-triageEmitCh:
		case <-deadline:
			t.Fatal("timed out waiting for the wire-error emission on the parent thread")
		}
		if name == "provider:item_event" {
			break
		}
	}
	errorItem, found, err := app.store.FindTurnItem(parent.ID, 0, "error")
	if err != nil {
		t.Fatalf("FindTurnItem(parent, error): %v", err)
	}
	if !found {
		t.Fatal("expected a persisted error item on the parent thread")
	}
	if !strings.Contains(errorItem.Summary, "discussion turn prompt failed") {
		t.Fatalf("error item summary = %q, want the discussion turn prompt failure", errorItem.Summary)
	}

	d, ok := app.deliberation(channelID)
	if !ok {
		t.Fatal("expected an installed deliberation")
	}
	if state := d.State(); state.AwaitingResponse || state.CurrentSpeaker != children[0].ID {
		t.Fatalf("state after failed dispatch = %+v, want AwaitingResponse=false and CurrentSpeaker=%q", state, children[0].ID)
	}

	// A second human post retries the SAME speaker.
	if _, err := app.PostChannelMessage(channelID, "Try again."); err != nil {
		t.Fatalf("PostChannelMessage(retry) error = %v", err)
	}
	retry := awaitSendCapture(t, sendCh)
	if retry.threadID != children[0].ID {
		t.Fatalf("retry prompt threadID = %q, want %q (the same current speaker)", retry.threadID, children[0].ID)
	}
}

// TestGetChannelStatePayloadShapeForLiveDiscussion covers
// GetChannelState's payload for a freshly-started, still-live
// discussion (the common case — no restart involved).
func TestGetChannelStatePayloadShapeForLiveDiscussion(t *testing.T) {
	app := newTestAppWithStore(t)
	app.registry = discussion.NewRegistry(app.store)
	app.channels = discussion.NewChannelService(app.store)
	app.startSessionFn = func(string) error { return nil }

	thread := testThread("thread-discussion-state-shape")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.CreateDiscussionDef(store.DiscussionDefinition{
		ID:        "def-state-shape",
		Name:      "Architects",
		Scope:     "project",
		ProjectID: projectPathForThread(t, app, thread),
		Participants: []store.DiscussionParticipant{
			{Role: "architect", System: "Design the change", Provider: string(provider.Claude), Model: "claude-sonnet-4"},
			{Role: "reviewer", System: "Review the change"},
		},
		Settings:  store.DiscussionSettings{MaxTurns: 6},
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("CreateDiscussionDef() error = %v", err)
	}
	if err := app.StartDiscussion(thread.ID, "Architects"); err != nil {
		t.Fatalf("StartDiscussion() error = %v", err)
	}

	parent, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	children, err := app.store.ListChildThreads(parent.ID)
	if err != nil {
		t.Fatalf("ListChildThreads() error = %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("len(children) = %d, want 2", len(children))
	}

	payload, err := app.GetChannelState(parent.DiscussionID)
	if err != nil {
		t.Fatalf("GetChannelState() error = %v", err)
	}
	if payload.ChannelID != parent.DiscussionID || payload.ThreadID != parent.ID {
		t.Fatalf("payload channel/thread IDs = %q/%q, want %q/%q", payload.ChannelID, payload.ThreadID, parent.DiscussionID, parent.ID)
	}
	if payload.Status != "open" {
		t.Fatalf("payload.Status = %q, want open", payload.Status)
	}
	if payload.TurnCount != 0 {
		t.Fatalf("payload.TurnCount = %d, want 0 before any participant has posted", payload.TurnCount)
	}
	if payload.MaxTurns != 6 {
		t.Fatalf("payload.MaxTurns = %d, want 6", payload.MaxTurns)
	}
	if payload.AwaitingResponse {
		t.Fatal("payload.AwaitingResponse = true, want false before anyone has been prompted")
	}
	if payload.CurrentSpeakerThreadID != children[0].ID {
		t.Fatalf("payload.CurrentSpeakerThreadID = %q, want %q (first participant)", payload.CurrentSpeakerThreadID, children[0].ID)
	}
	if payload.CurrentSpeakerRole != "Architect" {
		t.Fatalf("payload.CurrentSpeakerRole = %q, want Architect", payload.CurrentSpeakerRole)
	}
	if len(payload.Participants) != 2 {
		t.Fatalf("len(payload.Participants) = %d, want 2", len(payload.Participants))
	}
	byID := make(map[string]ChannelParticipantState, len(payload.Participants))
	for _, p := range payload.Participants {
		byID[p.ThreadID] = p
	}
	architect, ok := byID[children[0].ID]
	if !ok {
		t.Fatalf("payload.Participants missing %q", children[0].ID)
	}
	if architect.Role != "Architect" || architect.Provider != string(provider.Claude) || architect.Model != "claude-sonnet-4" {
		t.Fatalf("architect participant = %+v, want role=Architect provider=%s model=claude-sonnet-4", architect, provider.Claude)
	}
	reviewer, ok := byID[children[1].ID]
	if !ok {
		t.Fatalf("payload.Participants missing %q", children[1].ID)
	}
	if reviewer.Role != "Reviewer" || reviewer.Provider != thread.Provider || reviewer.Model != thread.Model {
		t.Fatalf("reviewer participant = %+v, want role=Reviewer provider/model inherited from parent", reviewer)
	}
}

// insertAssistantTurn appends an assistant_text item for threadID at
// the given turn index and drives it through syncDiscussionTurn — the
// same path a real participant's completed turn takes
// (sessionEventHandler → EventTurnComplete → syncDiscussionTurn).
func insertAssistantTurn(t *testing.T, app *App, threadID string, turnIndex int, content string) {
	t.Helper()
	if err := app.store.InsertItem(store.Item{
		ID:        threadID + "-turn-" + strconv.Itoa(turnIndex),
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		ItemIndex: 0,
		Kind:      "assistant_text",
		Role:      "assistant",
		Summary:   content,
		CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertItem(%s, turn %d): %v", threadID, turnIndex, err)
	}
	if err := app.syncDiscussionTurn(threadID); err != nil {
		t.Fatalf("syncDiscussionTurn(%s): %v", threadID, err)
	}
}

// TestUnanimousConclusionProposalsEndDiscussionEarly is the headline
// early-exit test: two participants each end their latest message with
// a CONCLUDE line. The first proposal alone must not conclude anything
// (only one of two participants has proposed) — the channel stays
// open and GetChannelState reflects the lone proposal via
// ProposedConclusion. The second proposal reaches unanimity: the
// channel concludes, the system message names both participants'
// summaries, and the FSM is dropped from a.deliberations exactly like
// the MaxTurns conclusion path.
func TestUnanimousConclusionProposalsEndDiscussionEarly(t *testing.T) {
	app := newTestAppWithStore(t)
	app.channels = discussion.NewChannelService(app.store)
	app.sendMessageFn = func(string, string, []string) error { return nil }

	parent, children := seedDiscussionChannel(t, app, "unanimous", []string{"Architect", "Reviewer"}, 8)
	channelID := parent.DiscussionID
	architect, reviewer := children[0], children[1]
	app.installDeliberation(channelID, []string{architect.ID, reviewer.ID}, 8)

	insertAssistantTurn(t, app, architect.ID, 0,
		"Let's wrap this up.\nCONCLUDE: we agree on the migration boundary.")

	channel, err := app.store.GetChannel(channelID)
	if err != nil {
		t.Fatalf("GetChannel() after first proposal: %v", err)
	}
	if channel.Status != "open" {
		t.Fatalf("channel.Status after first (non-unanimous) proposal = %q, want open", channel.Status)
	}

	payload, err := app.GetChannelState(channelID)
	if err != nil {
		t.Fatalf("GetChannelState() after first proposal: %v", err)
	}
	byID := make(map[string]ChannelParticipantState, len(payload.Participants))
	for _, p := range payload.Participants {
		byID[p.ThreadID] = p
	}
	if !byID[architect.ID].ProposedConclusion {
		t.Fatalf("architect.ProposedConclusion = false, want true after its CONCLUDE post")
	}
	if byID[reviewer.ID].ProposedConclusion {
		t.Fatalf("reviewer.ProposedConclusion = true, want false — reviewer hasn't posted yet")
	}

	insertAssistantTurn(t, app, reviewer.ID, 0,
		"Agreed on all points.\nCONCLUDE: no further concerns, ready to close out.")

	channel, err = app.store.GetChannel(channelID)
	if err != nil {
		t.Fatalf("GetChannel() after second proposal: %v", err)
	}
	if channel.Status != "concluded" {
		t.Fatalf("channel.Status after unanimous proposals = %q, want concluded", channel.Status)
	}

	messages, err := app.GetChannelMessages(channelID, -1, 0)
	if err != nil {
		t.Fatalf("GetChannelMessages(): %v", err)
	}
	last := messages[len(messages)-1]
	if last.FromType != "system" || last.FromRole != "moderator" {
		t.Fatalf("last message = %+v, want the system conclusion notice", last)
	}
	if !strings.Contains(last.Content, "all participants proposed to conclude") {
		t.Fatalf("conclusion content = %q, want the unanimous-form header", last.Content)
	}
	if !strings.Contains(last.Content, "Architect: we agree on the migration boundary.") {
		t.Fatalf("conclusion content = %q, want the architect's summary", last.Content)
	}
	if !strings.Contains(last.Content, "Reviewer: no further concerns, ready to close out.") {
		t.Fatalf("conclusion content = %q, want the reviewer's summary", last.Content)
	}

	// The agent messages mirrored into the channel must keep the
	// CONCLUDE line verbatim (transcript honesty) — only the FSM's
	// bookkeeping strips it out, not the posted content.
	if !strings.Contains(messages[0].Content, "CONCLUDE:") {
		t.Fatalf("architect's mirrored message = %q, want the CONCLUDE line preserved", messages[0].Content)
	}

	if _, ok := app.deliberation(channelID); ok {
		t.Fatal("expected the concluded-by-consensus deliberation to be removed from a.deliberations")
	}
}

// TestConclusionProposalWithdrawnByLatestPlainMessage covers the
// latest-stance rule end to end: a participant that proposed to
// conclude and then posts again WITHOUT a CONCLUDE line rescinds its
// earlier stance, so a later unanimous-looking sequence (the other
// participant now proposing) must NOT conclude the discussion.
func TestConclusionProposalWithdrawnByLatestPlainMessage(t *testing.T) {
	app := newTestAppWithStore(t)
	app.channels = discussion.NewChannelService(app.store)
	app.sendMessageFn = func(string, string, []string) error { return nil }

	parent, children := seedDiscussionChannel(t, app, "withdraw", []string{"Architect", "Reviewer"}, 8)
	channelID := parent.DiscussionID
	architect, reviewer := children[0], children[1]
	app.installDeliberation(channelID, []string{architect.ID, reviewer.ID}, 8)

	// A proposes to conclude.
	insertAssistantTurn(t, app, architect.ID, 0, "CONCLUDE: I think we're done here.")
	// B replies with an ordinary message — no marker.
	insertAssistantTurn(t, app, reviewer.ID, 0, "Actually, one more consideration on rollout.")
	// A follows up with a plain message too — this withdraws A's
	// earlier proposal (latest stance wins).
	insertAssistantTurn(t, app, architect.ID, 1, "Good point, let's cover rollout risk too.")
	// B now proposes to conclude. Only B has a live proposal (A's was
	// withdrawn), so this must NOT reach unanimity.
	insertAssistantTurn(t, app, reviewer.ID, 1, "CONCLUDE: rollout risk addressed, agreed to close.")

	channel, err := app.store.GetChannel(channelID)
	if err != nil {
		t.Fatalf("GetChannel(): %v", err)
	}
	if channel.Status != "open" {
		t.Fatalf("channel.Status = %q, want open (architect's withdrawal must block unanimity)", channel.Status)
	}

	payload, err := app.GetChannelState(channelID)
	if err != nil {
		t.Fatalf("GetChannelState(): %v", err)
	}
	byID := make(map[string]ChannelParticipantState, len(payload.Participants))
	for _, p := range payload.Participants {
		byID[p.ThreadID] = p
	}
	if byID[architect.ID].ProposedConclusion {
		t.Fatal("architect.ProposedConclusion = true, want false — withdrawn by its later plain message")
	}
	if !byID[reviewer.ID].ProposedConclusion {
		t.Fatal("reviewer.ProposedConclusion = false, want true — its latest message carried CONCLUDE")
	}

	if _, ok := app.deliberation(channelID); !ok {
		t.Fatal("expected the deliberation to still be live — the discussion has not concluded")
	}
}

// TestDeliberationForChannelReseedsConclusionProposalsFromHistory covers
// the restart re-seeding path: a channel's history shows a participant's
// LATEST agent message carrying a CONCLUDE line, but a.deliberations is
// cold (as after a process restart) — deliberationForChannel's rebuild
// must re-seed that proposal into the fresh FSM from history alone, not
// just recompute TurnCount/CurrentSpeaker. Without the re-seed, the
// rebuilt FSM would have an empty ConclusionProposals map and the
// second participant's CONCLUDE would land as a lone 1-of-2 proposal
// instead of reaching unanimity.
func TestDeliberationForChannelReseedsConclusionProposalsFromHistory(t *testing.T) {
	app := newTestAppWithStore(t)
	app.channels = discussion.NewChannelService(app.store)
	app.sendMessageFn = func(string, string, []string) error { return nil }

	parent, children := seedDiscussionChannel(t, app, "restart-reseed", []string{"Architect", "Reviewer"}, 8)
	channelID := parent.DiscussionID
	architect, reviewer := children[0], children[1]

	// Seed the pre-restart history directly (no installDeliberation —
	// a.deliberations is cold, exactly like a freshly started process
	// that inherits an in-progress discussion from SQLite).
	if _, err := app.channels.PostMessage(discussion.PostMessageInput{
		ChannelID: channelID, FromType: "agent", FromID: architect.ID, FromRole: "Architect",
		Content: "Let's wrap up.\nCONCLUDE: aligned on the plan.",
	}); err != nil {
		t.Fatalf("PostMessage(architect): %v", err)
	}

	if _, ok := app.deliberation(channelID); ok {
		t.Fatal("expected no in-memory deliberation before rebuild — test setup bug")
	}

	rebuilt, err := app.deliberationForChannel(channelID)
	if err != nil {
		t.Fatalf("deliberationForChannel(): %v", err)
	}
	rebuiltState := rebuilt.State()
	if got, want := rebuiltState.ConclusionProposals[architect.ID], "aligned on the plan."; got != want {
		t.Fatalf("re-seeded proposal for architect = %q, want %q", got, want)
	}
	if rebuiltState.Concluded {
		t.Fatal("expected the rebuilt FSM to NOT be concluded — only 1 of 2 participants proposed")
	}

	// Reviewer now proposes to conclude via the normal turn path. This
	// must reach unanimity ONLY because the rebuild re-seeded
	// architect's proposal from history.
	insertAssistantTurn(t, app, reviewer.ID, 0, "CONCLUDE: agreed, nothing further to add.")

	channel, err := app.store.GetChannel(channelID)
	if err != nil {
		t.Fatalf("GetChannel(): %v", err)
	}
	if channel.Status != "concluded" {
		t.Fatalf("channel.Status = %q, want concluded (proves the restart re-seed)", channel.Status)
	}

	messages, err := app.GetChannelMessages(channelID, -1, 0)
	if err != nil {
		t.Fatalf("GetChannelMessages(): %v", err)
	}
	last := messages[len(messages)-1]
	if !strings.Contains(last.Content, "Architect: aligned on the plan.") {
		t.Fatalf("conclusion content = %q, want the re-seeded architect summary", last.Content)
	}
	if !strings.Contains(last.Content, "Reviewer: agreed, nothing further to add.") {
		t.Fatalf("conclusion content = %q, want the reviewer summary", last.Content)
	}
}

// TestConcludeDiscussionEndsOpenDiscussionWithModeratorMessage is the
// headline test for the human "conclude now" affordance: calling
// ConcludeDiscussion on an open discussion posts the moderator-cause
// system message, flips the channel row to concluded, drops the FSM
// from a.deliberations, and returns a coherent post-conclusion
// snapshot — all independent of turn count or CONCLUDE proposals.
func TestConcludeDiscussionEndsOpenDiscussionWithModeratorMessage(t *testing.T) {
	app := newTestAppWithStore(t)
	app.channels = discussion.NewChannelService(app.store)

	parent, children := seedDiscussionChannel(t, app, "conclude-now", []string{"Architect", "Reviewer"}, 8)
	channelID := parent.DiscussionID
	architect, reviewer := children[0], children[1]
	app.installDeliberation(channelID, []string{architect.ID, reviewer.ID}, 8)

	payload, err := app.ConcludeDiscussion(channelID)
	if err != nil {
		t.Fatalf("ConcludeDiscussion() error = %v", err)
	}
	if payload.Status != "concluded" {
		t.Fatalf("payload.Status = %q, want concluded", payload.Status)
	}
	if payload.ChannelID != channelID || payload.ThreadID != parent.ID {
		t.Fatalf("payload channel/thread IDs = %q/%q, want %q/%q", payload.ChannelID, payload.ThreadID, channelID, parent.ID)
	}

	channel, err := app.store.GetChannel(channelID)
	if err != nil {
		t.Fatalf("GetChannel() error = %v", err)
	}
	if channel.Status != "concluded" {
		t.Fatalf("channel.Status = %q, want concluded", channel.Status)
	}

	messages, err := app.GetChannelMessages(channelID, -1, 0)
	if err != nil {
		t.Fatalf("GetChannelMessages() error = %v", err)
	}
	last := messages[len(messages)-1]
	if last.FromType != "system" || last.FromID != "deliberation" || last.FromRole != "moderator" {
		t.Fatalf("last message = %+v, want system/deliberation/moderator", last)
	}
	const wantModeratorText = "Discussion concluded: ended by the moderator."
	if last.Content != wantModeratorText {
		t.Fatalf("last message content = %q, want exactly %q", last.Content, wantModeratorText)
	}

	if _, ok := app.deliberation(channelID); ok {
		t.Fatal("expected ConcludeDiscussion to remove the FSM from a.deliberations")
	}

	if _, err := app.PostChannelMessage(channelID, "still talking?"); err == nil {
		t.Fatal("expected PostChannelMessage on a moderator-concluded channel to fail")
	}
}

// TestConcludeDiscussionOnAlreadyConcludedChannelErrors covers the
// idempotency guard: a second ConcludeDiscussion call against a channel
// that's already concluded must fail and must NOT append a second
// conclusion message.
func TestConcludeDiscussionOnAlreadyConcludedChannelErrors(t *testing.T) {
	app := newTestAppWithStore(t)
	app.channels = discussion.NewChannelService(app.store)

	parent, children := seedDiscussionChannel(t, app, "conclude-twice", []string{"Architect", "Reviewer"}, 8)
	channelID := parent.DiscussionID
	app.installDeliberation(channelID, []string{children[0].ID, children[1].ID}, 8)

	if _, err := app.ConcludeDiscussion(channelID); err != nil {
		t.Fatalf("ConcludeDiscussion() first call error = %v", err)
	}
	messagesAfterFirst, err := app.GetChannelMessages(channelID, -1, 0)
	if err != nil {
		t.Fatalf("GetChannelMessages() after first conclude error = %v", err)
	}
	if len(messagesAfterFirst) != 1 {
		t.Fatalf("len(messages) after first conclude = %d, want 1 (the moderator notice)", len(messagesAfterFirst))
	}

	if _, err := app.ConcludeDiscussion(channelID); err == nil {
		t.Fatal("expected ConcludeDiscussion on an already-concluded channel to error")
	}

	messagesAfterSecond, err := app.GetChannelMessages(channelID, -1, 0)
	if err != nil {
		t.Fatalf("GetChannelMessages() after second conclude error = %v", err)
	}
	if len(messagesAfterSecond) != 1 {
		t.Fatalf("len(messages) after failed second conclude = %d, want still 1 (no duplicate notice)", len(messagesAfterSecond))
	}
}

// TestConcludeDiscussionDropsLateParticipantTurnMirrorBenignly covers
// the mid-turn race this feature makes benign: a participant can be
// mid-turn when the human concludes, so its eventual turn-complete
// still flows into syncDiscussionTurn. The top-of-function open-check
// there reads the now-concluded channel row and returns nil without
// attempting any mirror — the reply stays visible only in the
// participant's own child thread's items.
func TestConcludeDiscussionDropsLateParticipantTurnMirrorBenignly(t *testing.T) {
	app := newTestAppWithStore(t)
	app.channels = discussion.NewChannelService(app.store)

	parent, children := seedDiscussionChannel(t, app, "conclude-race", []string{"Architect", "Reviewer"}, 8)
	channelID := parent.DiscussionID
	architect, reviewer := children[0], children[1]
	app.installDeliberation(channelID, []string{architect.ID, reviewer.ID}, 8)

	if _, err := app.ConcludeDiscussion(channelID); err != nil {
		t.Fatalf("ConcludeDiscussion() error = %v", err)
	}
	messagesAfterConclude, err := app.GetChannelMessages(channelID, -1, 0)
	if err != nil {
		t.Fatalf("GetChannelMessages() after conclude error = %v", err)
	}

	// Reviewer's provider session was mid-turn when the moderator
	// concluded; its turn-complete arrives after the fact with a fresh
	// assistant_text item, exactly like a real late reply.
	if err := app.store.InsertItem(store.Item{
		ID: reviewer.ID + "-late-turn", ThreadID: reviewer.ID, TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant", Summary: "Sorry, missed that we wrapped up.",
		CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertItem(late reviewer turn): %v", err)
	}
	if err := app.syncDiscussionTurn(reviewer.ID); err != nil {
		t.Fatalf("syncDiscussionTurn(reviewer) after conclusion error = %v, want nil (benign drop)", err)
	}

	messagesAfterLateTurn, err := app.GetChannelMessages(channelID, -1, 0)
	if err != nil {
		t.Fatalf("GetChannelMessages() after late turn error = %v", err)
	}
	if len(messagesAfterLateTurn) != len(messagesAfterConclude) {
		t.Fatalf("len(messages) after late turn = %d, want unchanged %d (nothing mirrored)",
			len(messagesAfterLateTurn), len(messagesAfterConclude))
	}
}
