package main

import (
	"errors"
	"sort"
	"testing"
	"time"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
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
	thread.ProjectPath = "/tmp/project"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.CreateDiscussionDef(store.DiscussionDefinition{
		ID:        "def-project",
		Name:      "Architects",
		Scope:     "project",
		ProjectID: thread.ProjectPath,
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
	if updated.InteractionMode != "discussion" {
		t.Fatalf("InteractionMode = %q, want discussion", updated.InteractionMode)
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
	if children[0].InteractionMode != "discussion" || children[1].InteractionMode != "discussion" {
		t.Fatalf("child interaction modes = %q, %q; want discussion", children[0].InteractionMode, children[1].InteractionMode)
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
	if updated.InteractionMode != "default" || updated.DiscussionID != "" {
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
}

func TestPostChannelMessageAndGetChannelMessages(t *testing.T) {
	app := newTestAppWithStore(t)
	app.registry = discussion.NewRegistry(app.store)
	app.channels = discussion.NewChannelService(app.store)

	thread := store.Thread{
		ID:              "thread-channel",
		Title:           "Channel Thread",
		Provider:        string(provider.Codex),
		WorkspacePath:   "/tmp/workspace",
		ProjectPath:     "/tmp/project",
		Model:           "gpt-5.4",
		InteractionMode: "discussion",
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
