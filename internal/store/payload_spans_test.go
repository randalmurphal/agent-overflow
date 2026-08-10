package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

func insertSpanTestPayload(t *testing.T, s *Store, threadID, itemID, payloadID string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := s.InsertItemWithPayload(Item{
		ID:        itemID,
		ThreadID:  threadID,
		ItemIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "diff",
		PayloadID: payloadID,
		CreatedAt: now,
		UpdatedAt: now,
	}, Payload{
		ID:        payloadID,
		Kind:      "tool_result",
		Meta:      "{}",
		Data:      []byte("diff --git a/a b/a"),
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert item+payload: %v", err)
	}
}

func TestUpdatePayloadSpansRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t1", "codex")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	insertSpanTestPayload(t, s, "t1", "item-1", "p1")

	// Fresh rows read as "not computed".
	spans, err := s.GetPayloadSpans("p1")
	if err != nil {
		t.Fatalf("get payload spans: %v", err)
	}
	if spans != "" {
		t.Fatalf("fresh payload spans = %q, want empty", spans)
	}

	if err := s.UpdatePayloadSpans("t1", "p1", `{"hv":"v","files":[1]}`, `{"hv":"v","files":[2]}`); err != nil {
		t.Fatalf("update payload spans: %v", err)
	}
	spans, err = s.GetPayloadSpans("p1")
	if err != nil {
		t.Fatalf("get payload spans: %v", err)
	}
	if spans != `{"hv":"v","files":[2]}` {
		t.Fatalf("spans = %q", spans)
	}

	// preview_spans rides the item join.
	item, found, err := s.GetThreadItem("t1", "item-1")
	if err != nil || !found {
		t.Fatalf("get thread item: %v found=%v", err, found)
	}
	if item.PayloadPreviewSpans != `{"hv":"v","files":[1]}` {
		t.Fatalf("item preview spans = %q", item.PayloadPreviewSpans)
	}

	// A missing payload surfaces as wrapped sql.ErrNoRows so the async
	// span worker can treat a deletion race as a benign drop.
	err = s.UpdatePayloadSpans("t1", "missing", "a", "b")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("update missing payload: err = %v, want sql.ErrNoRows", err)
	}
}

// TestPayloadSpanColumnsClearOnRewrite covers the two rewrite paths a
// payload row takes after spans landed: the in-place authoritative
// replace and the INSERT OR REPLACE upsert. Both must reset the span
// columns — the persist tap recomputes them for the new content.
// AppendPayloadData deliberately does NOT clear: per-file content
// addressing keeps blobs for still-identical file segments valid and
// makes the rest inert.
func TestPayloadSpanColumnsClearOnRewrite(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t1", "codex")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	insertSpanTestPayload(t, s, "t1", "item-1", "p1")
	now := time.Now().UnixMilli()

	seed := func() {
		t.Helper()
		if err := s.UpdatePayloadSpans("t1", "p1", "preview-blob", "full-blob"); err != nil {
			t.Fatalf("seed payload spans: %v", err)
		}
	}
	readBoth := func() (string, string) {
		t.Helper()
		spans, err := s.GetPayloadSpans("p1")
		if err != nil {
			t.Fatalf("get payload spans: %v", err)
		}
		item, found, err := s.GetThreadItem("t1", "item-1")
		if err != nil || !found {
			t.Fatalf("get thread item: %v found=%v", err, found)
		}
		return item.PayloadPreviewSpans, spans
	}

	// Replace clears both columns in the same transaction.
	seed()
	if err := s.ReplacePayloadData("t1", "p1", []byte("new content"), "{}", now); err != nil {
		t.Fatalf("replace payload data: %v", err)
	}
	if preview, spans := readBoth(); preview != "" || spans != "" {
		t.Fatalf("replace must clear spans, got preview=%q spans=%q", preview, spans)
	}

	// The item upsert's INSERT OR REPLACE resets the row to column
	// defaults. This is the only path that reaches upsertPayloadTx: a
	// bare payload upsert is deliberately not exported, because it would
	// reset these window-visible columns without naming a thread to
	// invalidate.
	seed()
	if _, err := s.UpsertItem(Item{
		ID: "item-1", ThreadID: "t1", ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Status: "completed",
		Summary: "diff", PayloadID: "p1", CreatedAt: now, UpdatedAt: now,
	}, &Payload{
		ID: "p1", Kind: "tool_result", Meta: "{}",
		Data: []byte("upserted"), CreatedAt: now,
	}); err != nil {
		t.Fatalf("upsert item+payload: %v", err)
	}
	if preview, spans := readBoth(); preview != "" || spans != "" {
		t.Fatalf("upsert must reset spans, got preview=%q spans=%q", preview, spans)
	}

	// Append retains them (content addressing keeps validity per file).
	seed()
	if err := s.AppendPayloadData("t1", "p1", []byte(" more"), "{}", now); err != nil {
		t.Fatalf("append payload data: %v", err)
	}
	if preview, spans := readBoth(); preview != "preview-blob" || spans != "full-blob" {
		t.Fatalf("append must retain spans, got preview=%q spans=%q", preview, spans)
	}
}
