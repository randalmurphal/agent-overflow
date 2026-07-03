package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestChannelCRUDAndMessageOrdering(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("thread-channel", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	now := time.Now().UnixMilli()
	channel := Channel{
		ID:        "channel-1",
		ThreadID:  thread.ID,
		Type:      "deliberation",
		Status:    "open",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.CreateChannel(channel); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	gotChannel, err := s.GetChannel(channel.ID)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if gotChannel.ID != channel.ID || gotChannel.ThreadID != channel.ThreadID || gotChannel.Status != channel.Status {
		t.Fatalf("GetChannel mismatch: got %+v want %+v", gotChannel, channel)
	}

	messages := []ChannelMessage{
		{ID: "msg-2", ChannelID: channel.ID, Sequence: 2, FromType: "agent", FromID: "thread-b", FromRole: "reviewer", Content: "second", CreatedAt: now + 2},
		{ID: "msg-0", ChannelID: channel.ID, Sequence: 0, FromType: "human", FromID: "user-1", Content: "first", CreatedAt: now + 1},
		{ID: "msg-1", ChannelID: channel.ID, Sequence: 1, FromType: "agent", FromID: "thread-a", FromRole: "proposer", Content: "middle", CreatedAt: now + 3},
	}
	for _, msg := range messages {
		if err := s.InsertChannelMessage(msg); err != nil {
			t.Fatalf("InsertChannelMessage(%s): %v", msg.ID, err)
		}
	}

	gotMessages, err := s.ListChannelMessages(channel.ID, -1, 0)
	if err != nil {
		t.Fatalf("ListChannelMessages(all): %v", err)
	}
	if len(gotMessages) != 3 {
		t.Fatalf("gotMessages len = %d, want 3", len(gotMessages))
	}
	if gotMessages[0].Sequence != 0 || gotMessages[1].Sequence != 1 || gotMessages[2].Sequence != 2 {
		t.Fatalf("unexpected sequence order: %+v", gotMessages)
	}
	if gotMessages[1].FromRole != "proposer" {
		t.Fatalf("gotMessages[1].FromRole = %q, want proposer", gotMessages[1].FromRole)
	}
	if gotMessages[0].FromRole != "" {
		t.Fatalf("gotMessages[0].FromRole = %q, want empty", gotMessages[0].FromRole)
	}

	filtered, err := s.ListChannelMessages(channel.ID, 0, 1)
	if err != nil {
		t.Fatalf("ListChannelMessages(filtered): %v", err)
	}
	if len(filtered) != 1 {
		t.Fatalf("filtered len = %d, want 1", len(filtered))
	}
	if filtered[0].Sequence != 1 {
		t.Fatalf("filtered[0].Sequence = %d, want 1", filtered[0].Sequence)
	}

	if err := s.UpdateChannelStatus(channel.ID, "closed"); err != nil {
		t.Fatalf("UpdateChannelStatus: %v", err)
	}

	closedChannel, err := s.GetChannel(channel.ID)
	if err != nil {
		t.Fatalf("GetChannel(closed): %v", err)
	}
	if closedChannel.Status != "closed" {
		t.Fatalf("closedChannel.Status = %q, want closed", closedChannel.Status)
	}
	if closedChannel.UpdatedAt <= channel.UpdatedAt {
		t.Fatalf("closedChannel.UpdatedAt = %d, want > %d", closedChannel.UpdatedAt, channel.UpdatedAt)
	}
}

func TestDeleteChannelRemovesChannelAndMessages(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("thread-delete-channel", "codex")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	now := time.Now().UnixMilli()
	channel := Channel{
		ID:        "channel-delete",
		ThreadID:  thread.ID,
		Type:      "deliberation",
		Status:    "open",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.CreateChannel(channel); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if err := s.InsertChannelMessage(ChannelMessage{
		ID:        "msg-delete",
		ChannelID: channel.ID,
		Sequence:  0,
		FromType:  "human",
		FromID:    "user",
		Content:   "cleanup",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("InsertChannelMessage: %v", err)
	}

	if err := s.DeleteChannel(channel.ID); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	if _, err := s.GetChannel(channel.ID); err == nil {
		t.Fatal("expected deleted channel lookup to fail")
	}

	messages, err := s.ListChannelMessages(channel.ID, -1, 0)
	if err != nil {
		t.Fatalf("ListChannelMessages(after delete): %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("len(messages) = %d, want 0 after channel delete", len(messages))
	}
}

// TestLastChannelMessageSeqFromNeverPostedIncludesSequenceZero guards
// the off-by-one this helper used to have: falling back to 0 (instead
// of -1) for "never posted" collided with the channel's legitimate
// first sequence number (also 0), which meant ListChannelMessages's
// exclusive `sequence > afterSeq` cursor silently dropped the very
// first message ever posted into a channel from any never-yet-posted
// participant's next-turn prompt — exactly the case a discussion's
// first speaker hits reading a human's kickoff message.
func TestLastChannelMessageSeqFromNeverPostedIncludesSequenceZero(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("thread-seq-zero", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	channel := Channel{
		ID:        "channel-seq-zero",
		ThreadID:  thread.ID,
		Type:      "deliberation",
		Status:    "open",
		MaxTurns:  8,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.CreateChannel(channel); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	if _, err := s.InsertChannelMessageAtomic(ChannelMessage{
		ID:        "msg-kickoff",
		ChannelID: channel.ID,
		FromType:  "human",
		FromID:    "user",
		Content:   "kickoff message at sequence 0",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("InsertChannelMessageAtomic: %v", err)
	}

	seq, err := s.LastChannelMessageSeqFrom(channel.ID, "participant-never-posted")
	if err != nil {
		t.Fatalf("LastChannelMessageSeqFrom: %v", err)
	}
	if seq != -1 {
		t.Fatalf("LastChannelMessageSeqFrom(never posted) = %d, want -1", seq)
	}

	messages, err := s.ListChannelMessages(channel.ID, seq, 0)
	if err != nil {
		t.Fatalf("ListChannelMessages: %v", err)
	}
	if len(messages) != 1 || messages[0].ID != "msg-kickoff" {
		t.Fatalf("ListChannelMessages(afterSeq=%d) = %+v, want the sequence-0 kickoff message included", seq, messages)
	}

	// A participant that HAS posted still gets its own real cursor.
	if _, err := s.InsertChannelMessageAtomic(ChannelMessage{
		ID:        "msg-reply",
		ChannelID: channel.ID,
		FromType:  "agent",
		FromID:    "participant-a",
		FromRole:  "Architect",
		Content:   "reply",
		CreatedAt: now + 1,
	}); err != nil {
		t.Fatalf("InsertChannelMessageAtomic(reply): %v", err)
	}
	seqAfterReply, err := s.LastChannelMessageSeqFrom(channel.ID, "participant-a")
	if err != nil {
		t.Fatalf("LastChannelMessageSeqFrom(participant-a): %v", err)
	}
	if seqAfterReply != 1 {
		t.Fatalf("LastChannelMessageSeqFrom(participant-a) = %d, want 1 (its own post's sequence)", seqAfterReply)
	}
}

func TestChannelMutationsReturnNotFoundForMissingRows(t *testing.T) {
	s := newTestStore(t)

	if err := s.UpdateChannelStatus("missing-channel", "closed"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("UpdateChannelStatus() error = %v, want sql.ErrNoRows", err)
	}
	if err := s.DeleteChannel("missing-channel"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("DeleteChannel() error = %v, want sql.ErrNoRows", err)
	}
}
