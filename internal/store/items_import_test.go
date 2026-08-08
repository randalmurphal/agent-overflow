package store

import (
	"strings"
	"testing"
)

// Original provider timestamps, all well in the past, so a restamp to
// now() would be unmissable.
const (
	importTurnStart    = 1_700_000_000_000
	importTurnComplete = 1_700_000_060_000
	importThreadUpdate = 1_600_000_000_000
)

func importBatchFixture(threadID string) ImportBatch {
	return ImportBatch{
		Turns: []Turn{{
			TurnID:    threadID + ":1",
			ThreadID:  threadID,
			TurnIndex: 1,
			StartedAt: importTurnStart,
		}},
		TurnCompletions: []TurnCompletion{{
			TurnID:             threadID + ":1",
			CompletedAt:        importTurnComplete,
			StopReason:         "end_turn",
			AssistantMessageID: "msg-1",
			TokenUsageJSON:     `{"inputTokens":10}`,
		}},
		Rows: []ImportRow{
			{
				Item: Item{
					ID: "item-user", TurnIndex: 1, ItemIndex: 0,
					Kind: "user_text", Role: "user", Status: "completed",
					Summary: "hello", CreatedAt: importTurnStart, UpdatedAt: importTurnStart,
				},
			},
			{
				Item: Item{
					ID: "item-tool", TurnIndex: 1, ItemIndex: 1,
					Kind: "tool_call", Role: "assistant", Status: "completed",
					Summary: "Bash", ToolName: "Bash",
					PayloadID: "payload-out", InputPayloadID: "payload-in",
					CreatedAt: importTurnStart + 10, UpdatedAt: importTurnComplete,
				},
				Payload: &Payload{
					ID: "payload-out", Kind: "command_output", Meta: `{"exitCode":0}`,
					Data: []byte("done\n"), CreatedAt: importTurnComplete,
				},
				InputPayload: &Payload{
					ID: "payload-in", Kind: "tool_input", Meta: "{}",
					Data: []byte(`{"command":"ls"}`), CreatedAt: importTurnStart + 10,
				},
			},
		},
		Usage: []UsageLedgerRow{{
			CreatedAt: importTurnComplete, ThreadID: threadID, ProjectID: defaultTestProjectID,
			TurnID: threadID + ":1", Provider: "claude", Model: "claude-opus-4-8",
			InputTokens: 10, OutputTokens: 4, CostSource: "none",
		}},
	}
}

// lifetimeUsage reads one thread's single unbucketed usage row, or a zero
// bucket when the thread has none.
func lifetimeUsage(t *testing.T, s *Store, threadID string) UsageBucket {
	t.Helper()
	buckets, err := s.QueryUsage(UsageQuery{ThreadID: threadID})
	if err != nil {
		t.Fatalf("query usage for %s: %v", threadID, err)
	}
	switch len(buckets) {
	case 0:
		return UsageBucket{}
	case 1:
		return buckets[0]
	default:
		t.Fatalf("ungrouped usage query returned %d buckets", len(buckets))
		return UsageBucket{}
	}
}

func newImportTargetThread(t *testing.T, s *Store, id string) {
	t.Helper()
	thread := importedThread(id, "claude")
	thread.UpdatedAt = importThreadUpdate
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("create import target thread: %v", err)
	}
}

func TestApplyImportBatchWritesEveryRowWithOriginalTimestamps(t *testing.T) {
	s := newTestStore(t)
	newImportTargetThread(t, s, "t-batch")

	if err := s.ApplyImportBatch("t-batch", importBatchFixture("t-batch")); err != nil {
		t.Fatalf("apply import batch: %v", err)
	}

	turns, err := s.ListRecentTurns("t-batch", 10)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	turn := turns[0]
	if turn.StartedAt != importTurnStart {
		t.Errorf("turn started_at = %d, want %d", turn.StartedAt, importTurnStart)
	}
	if turn.CompletedAt == nil || *turn.CompletedAt != importTurnComplete {
		t.Errorf("turn completed_at = %v, want %d", turn.CompletedAt, importTurnComplete)
	}
	if turn.StopReason != "end_turn" || turn.AssistantMessageID != "msg-1" {
		t.Errorf("turn settle = %q/%q", turn.StopReason, turn.AssistantMessageID)
	}
	if turn.TokenUsageJSON != `{"inputTokens":10}` {
		t.Errorf("turn token usage = %q", turn.TokenUsageJSON)
	}

	items, err := s.ListItems("t-batch")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].ID != "item-user" || items[1].ID != "item-tool" {
		t.Fatalf("item order = %q, %q", items[0].ID, items[1].ID)
	}
	if items[0].ThreadID != "t-batch" {
		t.Errorf("item thread id = %q, want t-batch", items[0].ThreadID)
	}
	if items[1].CreatedAt != importTurnStart+10 || items[1].UpdatedAt != importTurnComplete {
		t.Errorf("item timestamps = %d/%d", items[1].CreatedAt, items[1].UpdatedAt)
	}
	if items[1].PayloadKind != "command_output" {
		t.Errorf("joined payload kind = %q, want command_output", items[1].PayloadKind)
	}

	data, err := s.GetPayloadData("payload-out")
	if err != nil {
		t.Fatalf("get payload data: %v", err)
	}
	if string(data) != "done\n" {
		t.Errorf("payload data = %q", data)
	}
	input, err := s.GetPayloadMeta("payload-in")
	if err != nil {
		t.Fatalf("get input payload meta: %v", err)
	}
	if input.Kind != "tool_input" {
		t.Errorf("input payload kind = %q", input.Kind)
	}

	usage := lifetimeUsage(t, s, "t-batch")
	if usage.InputTokens != 10 || usage.OutputTokens != 4 {
		t.Errorf("usage totals = %+v", usage)
	}
}

// An import replays history that already happened. Floating the thread to
// the top of the sidebar (and marking it unread) is the opposite of what
// its original timestamps say.
func TestApplyImportBatchDoesNotBumpThreadActivity(t *testing.T) {
	s := newTestStore(t)
	newImportTargetThread(t, s, "t-batch-quiet")

	before, err := s.GetThread("t-batch-quiet")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if err := s.ApplyImportBatch("t-batch-quiet", importBatchFixture("t-batch-quiet")); err != nil {
		t.Fatalf("apply import batch: %v", err)
	}
	after, err := s.GetThread("t-batch-quiet")
	if err != nil {
		t.Fatalf("get thread after import: %v", err)
	}

	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("thread updated_at moved %d -> %d", before.UpdatedAt, after.UpdatedAt)
	}
	if after.UpdatedAt != importThreadUpdate {
		t.Errorf("thread updated_at = %d, want the original %d", after.UpdatedAt, importThreadUpdate)
	}
}

func TestApplyImportBatchIsOneTransaction(t *testing.T) {
	s := newTestStore(t)
	newImportTargetThread(t, s, "t-batch-atomic")

	batch := importBatchFixture("t-batch-atomic")
	// A second row reusing the first row's item id: the insert fails
	// half-way through, after turns, payloads, and one item have landed.
	batch.Rows = append(batch.Rows, ImportRow{
		Item: Item{
			ID: "item-user", TurnIndex: 1, ItemIndex: 2,
			Kind: "assistant_text", Role: "assistant", Status: "completed",
			Summary: "duplicate", CreatedAt: importTurnComplete, UpdatedAt: importTurnComplete,
		},
	})

	if err := s.ApplyImportBatch("t-batch-atomic", batch); err == nil {
		t.Fatal("ApplyImportBatch accepted a duplicate item id")
	}

	items, err := s.ListItems("t-batch-atomic")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("items survived a failed batch: %d", len(items))
	}
	turns, err := s.ListRecentTurns("t-batch-atomic", 10)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("turns survived a failed batch: %d", len(turns))
	}
	var payloads int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM payloads WHERE id IN ('payload-in', 'payload-out')`,
	).Scan(&payloads); err != nil {
		t.Fatalf("count payloads: %v", err)
	}
	if payloads != 0 {
		t.Errorf("payloads survived a failed batch: %d", payloads)
	}
	if usage := lifetimeUsage(t, s, "t-batch-atomic"); usage.InputTokens != 0 {
		t.Errorf("usage survived a failed batch: %+v", usage)
	}
}

// A refresh settles a turn a previous import already inserted, so the
// completion half must work against a pre-existing row — and must still
// fail loudly when the turn genuinely is not there.
func TestApplyImportBatchSettlesAPreviouslyImportedTurn(t *testing.T) {
	s := newTestStore(t)
	newImportTargetThread(t, s, "t-batch-refresh")

	first := importBatchFixture("t-batch-refresh")
	first.TurnCompletions = nil
	first.Rows = first.Rows[:1]
	first.Usage = nil
	if err := s.ApplyImportBatch("t-batch-refresh", first); err != nil {
		t.Fatalf("apply first batch: %v", err)
	}

	tail := importBatchFixture("t-batch-refresh")
	tail.Turns = nil
	tail.Rows = tail.Rows[1:]
	if err := s.ApplyImportBatch("t-batch-refresh", tail); err != nil {
		t.Fatalf("apply tail batch: %v", err)
	}

	turns, err := s.ListRecentTurns("t-batch-refresh", 10)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	if turns[0].CompletedAt == nil || *turns[0].CompletedAt != importTurnComplete {
		t.Errorf("turn completed_at = %v, want %d", turns[0].CompletedAt, importTurnComplete)
	}

	orphan := ImportBatch{TurnCompletions: []TurnCompletion{{
		TurnID: "t-batch-refresh:9", CompletedAt: importTurnComplete,
	}}}
	if err := s.ApplyImportBatch("t-batch-refresh", orphan); err == nil {
		t.Error("ApplyImportBatch settled a turn that does not exist")
	}
}

func TestApplyImportBatchRefusesForeignRows(t *testing.T) {
	s := newTestStore(t)
	newImportTargetThread(t, s, "t-batch-scope")
	newImportTargetThread(t, s, "t-batch-other")

	batch := importBatchFixture("t-batch-scope")
	batch.Rows[0].Item.ThreadID = "t-batch-other"
	err := s.ApplyImportBatch("t-batch-scope", batch)
	if err == nil {
		t.Fatal("ApplyImportBatch accepted an item belonging to another thread")
	}
	if !strings.Contains(err.Error(), "t-batch-other") {
		t.Errorf("error does not name the foreign thread: %v", err)
	}

	items, err := s.ListItems("t-batch-other")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("rows landed on the foreign thread: %d", len(items))
	}
}

// An item whose ThreadID the writer left blank is scoped to the batch's
// thread rather than rejected — the parameter is what names the target.
func TestApplyImportBatchScopesUnsetThreadIDs(t *testing.T) {
	s := newTestStore(t)
	newImportTargetThread(t, s, "t-batch-scopefill")

	batch := importBatchFixture("t-batch-scopefill")
	batch.Turns[0].ThreadID = ""
	batch.Rows[0].Item.ThreadID = ""
	batch.Usage[0].ThreadID = ""
	if err := s.ApplyImportBatch("t-batch-scopefill", batch); err != nil {
		t.Fatalf("apply import batch: %v", err)
	}

	items, err := s.ListItems("t-batch-scopefill")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if usage := lifetimeUsage(t, s, "t-batch-scopefill"); usage.InputTokens != 10 {
		t.Errorf("usage totals = %+v", usage)
	}
}

func TestApplyImportBatchRequiresAThreadID(t *testing.T) {
	s := newTestStore(t)
	if err := s.ApplyImportBatch("", ImportBatch{}); err == nil {
		t.Error("ApplyImportBatch accepted an empty thread id")
	}
}
