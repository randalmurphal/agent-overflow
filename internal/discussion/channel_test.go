package discussion

import (
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

// makeDiscussionThread constructs a store.Thread with the v13 shape,
// lazily ensuring a project row exists at /tmp/project.
func makeDiscussionThread(t *testing.T, st *store.Store, id string, provider string) store.Thread {
	t.Helper()
	project := testutil.EnsureProject(t, st, "/tmp/project")
	return store.Thread{
		ID:            id,
		ProjectID:     project.ID,
		Title:         "Discussion Thread",
		Provider:      provider,
		WorkspacePath: "/tmp/workspace",
		Model:         modelForProvider(provider),
		Mode:          "discussion",
		CreatedAt:     time.Now().UnixMilli(),
		UpdatedAt:     time.Now().UnixMilli(),
	}
}

func modelForProvider(p string) string {
	if p == "claude" {
		return "claude-sonnet-4-6"
	}
	return "gpt-5.4"
}

func TestChannelServiceOrdersMessagesAndCloses(t *testing.T) {
	st := newDiscussionTestStore(t)
	channelSvc := NewChannelService(st)
	thread := makeDiscussionThread(t, st, "thread-1", "codex")
	if err := st.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	channel, err := channelSvc.Create(thread.ID, "deliberation", 5)
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

func TestChannelServiceRejectsPostingToConcludedChannel(t *testing.T) {
	st := newDiscussionTestStore(t)
	channelSvc := NewChannelService(st)
	thread := makeDiscussionThread(t, st, "thread-concluded", "codex")
	if err := st.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	channel, err := channelSvc.Create(thread.ID, "deliberation", 5)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := st.UpdateChannelStatus(channel.ID, "concluded"); err != nil {
		t.Fatalf("UpdateChannelStatus(concluded) error = %v", err)
	}

	if _, err := channelSvc.PostMessage(PostMessageInput{
		ChannelID: channel.ID,
		FromType:  "human",
		FromID:    "user",
		Content:   "after conclusion",
	}); err == nil {
		t.Fatal("expected posting to concluded channel to fail")
	}
}

func TestChannelServiceDefaultsAndValidation(t *testing.T) {
	st := newDiscussionTestStore(t)
	channelSvc := NewChannelService(st)

	thread := makeDiscussionThread(t, st, "thread-2", "claude")
	if err := st.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	channel, err := channelSvc.Create(thread.ID, "", 0)
	if err != nil {
		t.Fatalf("Create(default type) error = %v", err)
	}
	if channel.Type != "deliberation" {
		t.Fatalf("channel.Type = %q, want deliberation", channel.Type)
	}
	if channel.MaxTurns != DefaultMaxTurns {
		t.Fatalf("channel.MaxTurns = %d, want normalized default %d", channel.MaxTurns, DefaultMaxTurns)
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
		t.Fatalf("PostMessage(one) error = %v", err)
	}
	if first.FromRole != "" {
		t.Fatalf("default FromRole should be blank, got %q", first.FromRole)
	}
}
