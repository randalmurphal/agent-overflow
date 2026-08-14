package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// These tests pin the command-output persistence buffer: streaming Codex
// outputDelta chunks accumulate in memory and land in SQLite + on the
// wire once per flush window (interval / byte threshold / lifecycle
// boundary), not once per chunk. Without the buffer every chunk cost an
// item read, a payload append, a full item upsert, and a wire upsert.

func commandOutputDelta(itemID, content string) provider.ProviderEvent {
	meta, _ := json.Marshal(map[string]any{"command": "go build", "exitCode": 0})
	return provider.ProviderEvent{
		Kind:      provider.EventCommandOutput,
		ThreadID:  "t1",
		ItemID:    itemID,
		Content:   content,
		Meta:      meta,
		Timestamp: time.Now(),
	}
}

// A burst of streaming deltas must stay buffered (no payload, no wire
// upserts), then land as ONE append + ONE upsert at the flush boundary.
func TestCommandOutputBurstBuffersUntilFlush(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	insertToolCallItem(t, st, "t1", "cmd-1", "Bash: go build", "command_execution", "running")

	for _, chunk := range []string{"one\n", "two\n", "three\n"} {
		if err := router.Handle(commandOutputDelta("cmd-1", chunk)); err != nil {
			t.Fatalf("handle %q: %v", chunk, err)
		}
	}

	if upserts := filterItemEventUpserts(emissions.snapshot()); len(upserts) != 0 {
		t.Fatalf("expected no upserts while buffered, got %d", len(upserts))
	}
	item, _, _ := st.GetThreadItem("t1", "cmd-1")
	if item.PayloadID != "" {
		t.Fatalf("expected no payload while buffered, got %s", item.PayloadID)
	}

	if err := router.FlushThread("t1"); err != nil {
		t.Fatalf("flush: %v", err)
	}

	upserts := filterItemEventUpserts(emissions.snapshot())
	if len(upserts) != 1 {
		t.Fatalf("expected exactly one upsert at flush, got %d", len(upserts))
	}
	item, _, _ = st.GetThreadItem("t1", "cmd-1")
	if item.PayloadID == "" {
		t.Fatal("expected payload linked after flush")
	}
	data, err := st.GetPayloadData(item.ThreadID, item.PayloadID)
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	if string(data) != "one\ntwo\nthree\n" {
		t.Fatalf("payload data = %q, want concatenated window", string(data))
	}

	// Flush again with an empty buffer: nothing new may land.
	if err := router.FlushThread("t1"); err != nil {
		t.Fatalf("idle flush: %v", err)
	}
	if upserts := filterItemEventUpserts(emissions.snapshot()); len(upserts) != 1 {
		t.Fatalf("idle flush emitted extra upserts: got %d", len(upserts))
	}
}

// An authoritative Replace snapshot (Codex aggregatedOutput) discards
// buffered deltas: the payload must hold exactly the snapshot, and a
// later flush must not append the stale window after it.
func TestCommandOutputReplaceDiscardsPendingBuffer(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	insertToolCallItem(t, st, "t1", "cmd-1", "Bash: go build", "command_execution", "running")

	if err := router.Handle(commandOutputDelta("cmd-1", "partial tail\n")); err != nil {
		t.Fatalf("delta: %v", err)
	}
	replace := commandOutputDelta("cmd-1", "full aggregated output\n")
	replace.Replace = true
	if err := router.Handle(replace); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := router.FlushThread("t1"); err != nil {
		t.Fatalf("flush: %v", err)
	}

	item, _, _ := st.GetThreadItem("t1", "cmd-1")
	data, err := st.GetPayloadData(item.ThreadID, item.PayloadID)
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	if string(data) != "full aggregated output\n" {
		t.Fatalf("payload data = %q, want replacement only (buffered tail discarded)", string(data))
	}
}

// Tool completion must flush the buffered tail before rebuilding the
// cumulative meta — covers Codex completions whose aggregatedOutput is
// absent, where buffered deltas are the only output bytes.
func TestCommandOutputCompletionFlushesBufferedTail(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{"toolName": "Bash"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-1",
		ItemType: "command_execution", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	for _, chunk := range []string{"line1\nline2\n", "line3\nline4\n"} {
		if err := router.Handle(commandOutputDelta("cmd-1", chunk)); err != nil {
			t.Fatalf("delta %q: %v", chunk, err)
		}
	}

	completeMeta, _ := json.Marshal(map[string]any{"exit_code": 0})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-1",
		Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	item, _, _ := st.GetThreadItem("t1", "cmd-1")
	if item.PayloadID == "" {
		t.Fatal("expected payload after completion flush")
	}
	data, err := st.GetPayloadData(item.ThreadID, item.PayloadID)
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	if string(data) != "line1\nline2\nline3\nline4\n" {
		t.Fatalf("payload data = %q, want full buffered output", string(data))
	}
	var meta CommandOutputMeta
	if err := json.Unmarshal([]byte(item.PayloadMeta), &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	// Cumulative rebuild over all four lines (+ trailing empty token),
	// not the last window's count — proves the flush ran BEFORE the
	// completion meta rebuild.
	if meta.LineCount != 5 {
		t.Fatalf("lineCount = %d, want cumulative 5", meta.LineCount)
	}
}

// A streaming delta whose row does NOT yet exist (outputDelta raced ahead
// of item/started, or no start was emitted) must create the row AND make
// the chunk visible immediately — no flush boundary required — so the user
// sees output rather than an empty pending row.
func TestCommandOutputFirstDeltaCreatesRowAndEmits(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	// Deliberately no insertToolCallItem / EventToolStart for "cmd-new".

	if err := router.Handle(commandOutputDelta("cmd-new", "first chunk\n")); err != nil {
		t.Fatalf("first delta: %v", err)
	}

	// Visible immediately, without a lifecycle flush.
	if upserts := filterItemEventUpserts(emissions.snapshot()); len(upserts) != 1 {
		t.Fatalf("expected one upsert on first-delta create, got %d", len(upserts))
	}
	item, found, _ := st.GetThreadItem("t1", "cmd-new")
	if !found {
		t.Fatal("expected the row to be created on the first delta")
	}
	if item.PayloadID == "" {
		t.Fatal("expected the chunk persisted immediately, not buffered")
	}
	data, err := st.GetPayloadData(item.ThreadID, item.PayloadID)
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	if string(data) != "first chunk\n" {
		t.Fatalf("payload data = %q, want the first chunk", string(data))
	}
}

// An authoritative Replace snapshot for an itemID with no existing row must
// create the row carrying exactly the replacement payload (the buffered
// delta path is bypassed for Replace, so the create fallback is the only
// way the row appears).
func TestCommandOutputReplaceCreatesMissingRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	replace := commandOutputDelta("cmd-missing", "aggregated output only\n")
	replace.Replace = true
	if err := router.Handle(replace); err != nil {
		t.Fatalf("replace: %v", err)
	}

	item, found, _ := st.GetThreadItem("t1", "cmd-missing")
	if !found {
		t.Fatal("expected Replace to create the missing row")
	}
	data, err := st.GetPayloadData(item.ThreadID, item.PayloadID)
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	if string(data) != "aggregated output only\n" {
		t.Fatalf("payload data = %q, want the replacement snapshot", string(data))
	}
}

// If the row vanishes between staging a window and flushing it (thread
// cleanup races the timer), the flush must drop the window without erroring
// and without emitting an upsert against the dead row.
func TestCommandOutputFlushDropsWhenItemGone(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	insertToolCallItem(t, st, "t1", "cmd-1", "Bash: go build", "command_execution", "running")

	// Stage a buffered window (row exists at window start).
	if err := router.Handle(commandOutputDelta("cmd-1", "buffered tail\n")); err != nil {
		t.Fatalf("delta: %v", err)
	}
	emissions.reset()

	// Row disappears before the flush lands.
	if err := st.DeleteThreadItem("t1", "cmd-1"); err != nil {
		t.Fatalf("delete item: %v", err)
	}

	if err := router.FlushThread("t1"); err != nil {
		t.Fatalf("flush after item gone must not error: %v", err)
	}
	if upserts := filterItemEventUpserts(emissions.snapshot()); len(upserts) != 0 {
		t.Fatalf("expected no upsert for a vanished row, got %d", len(upserts))
	}
}

// A window crossing the byte threshold flushes synchronously on the
// read loop — no explicit lifecycle flush required.
func TestCommandOutputThresholdFlushIsImmediate(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	insertToolCallItem(t, st, "t1", "cmd-1", "Bash: cat big", "command_execution", "running")

	big := strings.Repeat("x", commandOutputPersistByteThreshold)
	if err := router.Handle(commandOutputDelta("cmd-1", big)); err != nil {
		t.Fatalf("handle big delta: %v", err)
	}

	item, _, _ := st.GetThreadItem("t1", "cmd-1")
	if item.PayloadID == "" {
		t.Fatal("expected threshold crossing to flush without lifecycle boundary")
	}
	data, err := st.GetPayloadData(item.ThreadID, item.PayloadID)
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	if len(data) != commandOutputPersistByteThreshold {
		t.Fatalf("payload size = %d, want %d", len(data), commandOutputPersistByteThreshold)
	}
}

// The interval timer must flush a quiet buffer on its own. Polls the
// store only (the emit callback fires on the timer goroutine; reading
// the emissions slice here would race).
func TestCommandOutputTimerFlushDelivers(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	insertToolCallItem(t, st, "t1", "cmd-1", "Bash: slow", "command_execution", "running")

	if err := router.Handle(commandOutputDelta("cmd-1", "trickle\n")); err != nil {
		t.Fatalf("handle: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		item, _, _ := st.GetThreadItem("t1", "cmd-1")
		if item.PayloadID != "" {
			data, err := st.GetPayloadData(item.ThreadID, item.PayloadID)
			if err != nil {
				t.Fatalf("payload data: %v", err)
			}
			if string(data) != "trickle\n" {
				t.Fatalf("payload data = %q, want timer-flushed chunk", string(data))
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timer flush never delivered the buffered chunk")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
