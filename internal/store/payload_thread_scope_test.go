package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestPayloadIdentityAndMutationAreThreadScoped(t *testing.T) {
	s := newTestStore(t)
	for _, threadID := range []string{"thread-a", "thread-b", "thread-empty"} {
		mustCreateThread(t, s, threadID)
	}

	insert := func(threadID, itemID, body string) {
		t.Helper()
		if err := s.InsertItemWithPayload(Item{
			ID: itemID, ThreadID: threadID, TurnIndex: 0, ItemIndex: 0,
			Kind: "thinking", Role: "assistant", Status: "completed",
			PayloadID: "thinking:think:0:0", CreatedAt: 1, UpdatedAt: 1,
		}, Payload{
			ID: "thinking:think:0:0", Kind: "thinking", Meta: "{}",
			Data: []byte(body), CreatedAt: 1,
		}); err != nil {
			t.Fatalf("insert %s payload: %v", threadID, err)
		}
	}
	insert("thread-a", "think:0:0", "authored by A")
	insert("thread-b", "think:0:0", "authored by B")

	if err := s.AppendPayloadData("thread-a", "thinking:think:0:0", []byte(" + append"), `{"owner":"a"}`, 2); err != nil {
		t.Fatalf("append A: %v", err)
	}
	a, err := s.GetPayloadData("thread-a", "thinking:think:0:0")
	if err != nil {
		t.Fatalf("read A: %v", err)
	}
	b, err := s.GetPayloadData("thread-b", "thinking:think:0:0")
	if err != nil {
		t.Fatalf("read B: %v", err)
	}
	if string(a) != "authored by A + append" || string(b) != "authored by B" {
		t.Fatalf("payload isolation failed: A=%q B=%q", a, b)
	}

	if err := s.ReplacePayloadData("thread-empty", "thinking:think:0:0", []byte("wrong"), "{}", 3); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("wrong-thread replace error = %v, want sql.ErrNoRows", err)
	}
	b, err = s.GetPayloadData("thread-b", "thinking:think:0:0")
	if err != nil || string(b) != "authored by B" {
		t.Fatalf("wrong-thread mutation changed B: data=%q err=%v", b, err)
	}

	var rows int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM payloads WHERE id = 'thinking:think:0:0'`,
	).Scan(&rows); err != nil {
		t.Fatalf("count same-id payloads: %v", err)
	}
	if rows != 2 {
		t.Fatalf("same payload id rows = %d, want 2 thread-owned rows", rows)
	}
}

func TestItemCannotReferenceAnotherThreadsPayload(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "thread-a")
	mustCreateThread(t, s, "thread-b")
	if err := seedPayloadRow(s, "thread-a", Payload{
		ID: "payload", Kind: "thinking", Meta: "{}", Data: []byte("A"), CreatedAt: 1,
	}); err != nil {
		t.Fatalf("seed A payload: %v", err)
	}

	err := s.InsertItem(Item{
		ID: "item", ThreadID: "thread-b", TurnIndex: 0, ItemIndex: 0,
		Kind: "thinking", Role: "assistant", PayloadID: "payload", CreatedAt: 1,
	})
	if err == nil {
		t.Fatal("cross-thread payload reference succeeded")
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("cross-thread payload reference error = %v, want FK constraint", err)
	}
}
