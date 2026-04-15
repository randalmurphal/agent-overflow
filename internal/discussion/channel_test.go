package discussion

import (
	"testing"
	"time"

	"agent-overflow/internal/store"
)

func TestChannelServiceOrdersMessagesAndCloses(t *testing.T) {
	st := newDiscussionTestStore(t)
	channelSvc := NewChannelService(st)
	thread := store.Thread{
		ID:              "thread-1",
		Title:           "Discussion Thread",
		Provider:        "codex",
		WorkspacePath:   "/tmp/workspace",
		ProjectPath:     "/tmp/project",
		Model:           "gpt-5.4",
		InteractionMode: "discussion",
		CreatedAt:       time.Now().UnixMilli(),
		UpdatedAt:       time.Now().UnixMilli(),
	}
	if err := st.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	channel, err := channelSvc.Create(thread.ID, "deliberation")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	first, err := channelSvc.PostMessage(PostMessageInput{
		ChannelID: channel.ID,
		FromType:  "agent",
		FromID:    "thread-a",
		FromRole:  "proposer",
		Content:   "first",
	})
	if err != nil {
		t.Fatalf("PostMessage(first) error = %v", err)
	}
	second, err := channelSvc.PostMessage(PostMessageInput{
		ChannelID: channel.ID,
		FromType:  "agent",
		FromID:    "thread-b",
		FromRole:  "reviewer",
		Content:   "second",
	})
	if err != nil {
		t.Fatalf("PostMessage(second) error = %v", err)
	}
	if first.Sequence != 0 || second.Sequence != 1 {
		t.Fatalf("sequences = %d,%d want 0,1", first.Sequence, second.Sequence)
	}

	messages, err := channelSvc.GetMessages(channel.ID, -1, 0)
	if err != nil {
		t.Fatalf("GetMessages() error = %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(messages))
	}
	if messages[0].Content != "first" || messages[1].Content != "second" {
		t.Fatalf("messages out of order: %+v", messages)
	}

	if err := channelSvc.Close(channel.ID); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := channelSvc.PostMessage(PostMessageInput{
		ChannelID: channel.ID,
		FromType:  "human",
		FromID:    "user",
		Content:   "after close",
	}); err == nil {
		t.Fatal("expected posting to closed channel to fail")
	}
}

func TestChannelServiceDefaultsAndValidation(t *testing.T) {
	st := newDiscussionTestStore(t)
	channelSvc := NewChannelService(st)

	thread := store.Thread{
		ID:              "thread-2",
		Title:           "Thread",
		Provider:        "claude",
		WorkspacePath:   "/tmp/workspace",
		ProjectPath:     "/tmp/project",
		Model:           "claude-sonnet-4-6",
		InteractionMode: "discussion",
		CreatedAt:       time.Now().UnixMilli(),
		UpdatedAt:       time.Now().UnixMilli(),
	}
	if err := st.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	channel, err := channelSvc.Create(thread.ID, "")
	if err != nil {
		t.Fatalf("Create(default type) error = %v", err)
	}
	if channel.Type != "deliberation" {
		t.Fatalf("channel.Type = %q, want deliberation", channel.Type)
	}

	if _, err := channelSvc.PostMessage(PostMessageInput{}); err == nil {
		t.Fatal("expected validation error for empty message input")
	}

	first, err := channelSvc.PostMessage(PostMessageInput{
		ChannelID: channel.ID,
		FromType:  "human",
		FromID:    "user",
		Content:   "one",
	})
	if err != nil {
		t.Fatalf("PostMessage(first) error = %v", err)
	}
	if _, err := channelSvc.PostMessage(PostMessageInput{
		ChannelID: channel.ID,
		FromType:  "human",
		FromID:    "user",
		Content:   "two",
	}); err != nil {
		t.Fatalf("PostMessage(second) error = %v", err)
	}

	filtered, err := channelSvc.GetMessages(channel.ID, first.Sequence, 1)
	if err != nil {
		t.Fatalf("GetMessages(filtered) error = %v", err)
	}
	if len(filtered) != 1 || filtered[0].Content != "two" {
		t.Fatalf("filtered messages = %+v, want second message only", filtered)
	}
}
