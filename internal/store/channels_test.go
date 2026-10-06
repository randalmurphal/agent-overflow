package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync"
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

// TestChannelMessageSequenceAssignment pins how InsertChannelMessageAtomic
// numbers messages and how the readers scope them: per channel, from 0, one
// past the channel's highest sequence even across a gap, and returned as
// stored.
func TestChannelMessageSequenceAssignment(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("thread-channel-seq", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ch-a", "ch-b"} {
		if err := s.CreateChannel(Channel{ID: id, ThreadID: thread.ID, Type: "deliberation", Status: "open", CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	var posted []ChannelMessage
	post := func(msg ChannelMessage, want int) {
		t.Helper()
		msg.CreatedAt = int64(100 + len(posted))
		got, err := s.InsertChannelMessageAtomic(msg)
		if err != nil {
			t.Fatalf("post %s: %v", msg.ID, err)
		}
		if got != want {
			t.Fatalf("post %s returned sequence %d, want %d", msg.ID, got, want)
		}
		msg.Sequence = want
		posted = append(posted, msg)
	}
	post(ChannelMessage{ID: "a0", ChannelID: "ch-a", FromType: "human", FromID: "user", Content: "kickoff"}, 0)
	post(ChannelMessage{ID: "a1", ChannelID: "ch-a", FromType: "agent", FromID: "p1", FromRole: "Architect", Content: "one", Meta: `{"k":1}`}, 1)
	post(ChannelMessage{ID: "a2", ChannelID: "ch-a", FromType: "agent", FromID: "p2", FromRole: "Critic", Content: "two"}, 2)
	post(ChannelMessage{ID: "a3", ChannelID: "ch-a", FromType: "agent", FromID: "p1", FromRole: "Architect", Content: "three"}, 3)
	// Another channel numbers from 0.
	post(ChannelMessage{ID: "b0", ChannelID: "ch-b", FromType: "agent", FromID: "p2", FromRole: "Critic", Content: "b zero"}, 0)
	gap := ChannelMessage{ID: "b7", ChannelID: "ch-b", Sequence: 7, FromType: "human", FromID: "user", Content: "b seven", CreatedAt: 200}
	if err := s.InsertChannelMessage(gap); err != nil {
		t.Fatal(err)
	}
	posted = append(posted, gap)
	// The next sequence follows the highest, not the message count.
	post(ChannelMessage{ID: "b8", ChannelID: "ch-b", FromType: "agent", FromID: "p2", FromRole: "Critic", Content: "b eight"}, 8)

	for channel, want := range map[string][]ChannelMessage{"ch-a": posted[:4], "ch-b": posted[4:]} {
		got, err := s.ListChannelMessages(channel, -1, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s lists\n%+v\nwant\n%+v", channel, got, want)
		}
	}

	for _, tc := range []struct {
		channel, from string
		want          int
	}{
		{"ch-a", "p1", 3},
		{"ch-a", "p2", 2},
		{"ch-a", "user", 0},
		{"ch-b", "p1", -1},
		{"ch-b", "p2", 8},
	} {
		if got, err := s.LastChannelMessageSeqFrom(tc.channel, tc.from); err != nil || got != tc.want {
			t.Fatalf("LastChannelMessageSeqFrom(%s, %s) = %d, %v; want %d", tc.channel, tc.from, got, err, tc.want)
		}
	}

	for _, tc := range []struct {
		channel, fromType string
		want              int
	}{
		{"ch-a", "agent", 3},
		{"ch-a", "human", 1},
		{"ch-b", "agent", 2},
		{"ch-b", "system", 0},
	} {
		if got, err := s.CountChannelMessagesByType(tc.channel, tc.fromType); err != nil || got != tc.want {
			t.Fatalf("CountChannelMessagesByType(%s, %s) = %d, %v; want %d", tc.channel, tc.fromType, got, err, tc.want)
		}
	}
}

// TestInsertChannelMessageAtomicNumbersConcurrentPosts pins the reason the
// insert computes its own sequence: concurrent posts each get a distinct
// one instead of colliding on UNIQUE(channel_id, sequence).
func TestInsertChannelMessageAtomicNumbersConcurrentPosts(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("thread-channel-concurrent", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateChannel(Channel{ID: "ch", ThreadID: thread.ID, Type: "deliberation", Status: "open", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	const posts = 32
	sequences := make([]int, posts)
	errs := make([]error, posts)
	var wg sync.WaitGroup
	for i := range posts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sequences[i], errs[i] = s.InsertChannelMessageAtomic(ChannelMessage{
				ID: fmt.Sprintf("m%d", i), ChannelID: "ch", FromType: "agent", FromID: "p", Content: "x", CreatedAt: 1,
			})
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	listed, err := s.ListChannelMessages("ch", -1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != posts {
		t.Fatalf("listed %d messages, want %d", len(listed), posts)
	}
	for seq, msg := range listed {
		if msg.Sequence != seq {
			t.Fatalf("sequences are not 0..%d: %+v", posts-1, listed)
		}
		var i int
		if _, err := fmt.Sscanf(msg.ID, "m%d", &i); err != nil || sequences[i] != seq {
			t.Fatalf("message %s stored at %d, returned %v (%v)", msg.ID, seq, sequences, err)
		}
	}
}
