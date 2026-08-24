package sessionimport

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// writer_test.go — the writer's own rules: provenance, clocks, turn
// sealing, refresh append, and the fail-loud boundary. Row SHAPE is
// pinned by parity_test.go against the live router; nothing here
// re-asserts a summary or an id format that the parity test already
// compares.

func TestBuildStampsImportProvenanceOnEveryRow(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "claude", t.TempDir())

	events := []importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventUserText, ThreadID: testThreadID,
			Content: "hello", Timestamp: at(0),
		}, SourceUUID: "uuid-user"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: testThreadID,
			ItemID: "m1", Content: "hi", Timestamp: at(1),
		}, SourceUUID: "line:4096", SourceOffset: 4200},
	}

	batch, warnings, err := NewWriter(st, thread).Build(events)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", warnings)
	}
	if len(batch.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(batch.Rows))
	}
	if got := metaKey(t, batch.Rows[0].Item.Meta, itemmeta.ImportSourceUUIDKey); got != "uuid-user" {
		t.Errorf("uuid provenance = %q, want uuid-user", got)
	}
	// The Codex spelling is a real uuid, not a derivative of SourceOffset:
	// the offset is the RESUME position (one past the line's newline) and
	// stamping provenance from it would name a different line.
	if got := metaKey(t, batch.Rows[1].Item.Meta, itemmeta.ImportSourceUUIDKey); got != "line:4096" {
		t.Errorf("codex provenance = %q, want line:4096", got)
	}
}

func TestBuildPreservesSourcedCommandResultAttribution(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "codex", t.TempDir())
	meta, err := json.Marshal(provider.CommandResultMeta{
		AgentResult: &provider.CommandAgentResultMeta{
			LaunchID: "review-launch", SourceKind: "review", SourceName: "Code review",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	batch, warnings, err := NewWriter(st, thread).Build([]importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventUserText, ThreadID: testThreadID,
			Content: "/review", Timestamp: at(0),
		}, SourceUUID: "line:1"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventCommandResult, ThreadID: testThreadID,
			ItemID: "review-result", Content: "No findings.", Meta: meta, Timestamp: at(1),
		}, SourceUUID: "line:2"},
	})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("build: warnings=%+v err=%v", warnings, err)
	}
	for _, row := range batch.Rows {
		if row.Item.Kind != kindCommandResult {
			continue
		}
		if !strings.Contains(row.Item.Meta, `"sourceKind":"review"`) ||
			!strings.Contains(row.Item.Meta, `"launchId":"review-launch"`) {
			t.Fatalf("command result meta = %s", row.Item.Meta)
		}
		return
	}
	t.Fatal("no command result row")
}

// SourceOffset is a resume position, never a substitute for provenance. A
// reader that set only the offset is a reader bug, and the whole import is
// refused rather than stamped with a coordinate nobody agreed on.
func TestBuildRefusesAnEventWithNoSourceUUID(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "codex", t.TempDir())

	_, _, err := NewWriter(st, thread).Build([]importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventUserText, ThreadID: testThreadID,
			Content: "hello", Timestamp: at(0),
		}, SourceOffset: 4096},
	})
	if err == nil {
		t.Fatal("want a refusal for an event carrying only a source offset")
	}
	if !strings.Contains(err.Error(), "source uuid") {
		t.Fatalf("error should name the missing coordinate: %v", err)
	}
}

func TestBuildStampsImportUnavailableFromEventMeta(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "claude", t.TempDir())

	events := []importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: testThreadID,
			ItemID: "toolu_1", ItemType: "Bash",
			Meta: json.RawMessage(`{"toolName":"Bash","input":{"command":"ls"},` +
				`"import_unavailable":"tool-output-gc"}`),
			Timestamp: at(0),
		}, SourceUUID: "uuid-1"},
	}

	batch, _, err := NewWriter(st, thread).Build(events)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	meta := batch.Rows[0].Item.Meta
	if got := metaKey(t, meta, itemmeta.ImportUnavailableKey); got != "tool-output-gc" {
		t.Errorf("import_unavailable = %q, want tool-output-gc", got)
	}
	// The marker is the writer's own control key; it must be re-stamped
	// through itemmeta rather than left inside the stored provider meta
	// under a second spelling. Its only representation is the top-level
	// key, and the rest of the provider meta survives.
	if strings.Count(meta, itemmeta.ImportUnavailableKey) != 1 {
		t.Errorf("import_unavailable appears more than once in %s", meta)
	}
	if got := metaKey(t, meta, "toolName"); got != "Bash" {
		t.Errorf("provider meta lost toolName: %s", meta)
	}
}

func TestBuildUsesEventTimestampsNotNow(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "claude", t.TempDir())

	batch, _, err := NewWriter(st, thread).Build([]importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventTurnStart, ThreadID: testThreadID, Timestamp: at(0),
		}, SourceUUID: "uuid-0"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventUserText, ThreadID: testThreadID,
			Content: "go", Timestamp: at(1),
		}, SourceUUID: "uuid-1"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: testThreadID, Timestamp: at(9),
			TurnComplete: &provider.WireTurnCompleteMeta{
				StopReason: "end_turn",
				ModelUsage: []provider.ModelTokenUsage{{
					Model:      "claude-sonnet-4-5",
					TokenUsage: provider.TokenUsage{InputTokens: 10, OutputTokens: 5},
				}},
			},
		}, SourceUUID: "uuid-9"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	item := batch.Rows[0].Item
	if item.CreatedAt != baseMillis+1 || item.UpdatedAt != baseMillis+1 {
		t.Errorf("item clock = (%d, %d), want the event's %d", item.CreatedAt, item.UpdatedAt, baseMillis+1)
	}
	if len(batch.Turns) != 1 || batch.Turns[0].StartedAt != baseMillis {
		t.Fatalf("turn started_at = %+v, want %d", batch.Turns, baseMillis)
	}
	if len(batch.TurnCompletions) != 1 || batch.TurnCompletions[0].CompletedAt != baseMillis+9 {
		t.Fatalf("turn completed_at = %+v, want %d", batch.TurnCompletions, baseMillis+9)
	}
	if len(batch.Usage) != 1 {
		t.Fatalf("want 1 usage row, got %d", len(batch.Usage))
	}
	usage := batch.Usage[0]
	if usage.CreatedAt != baseMillis+9 {
		t.Errorf("usage created_at = %d, want the turn's completion time %d", usage.CreatedAt, baseMillis+9)
	}
	if usage.CostSource != "none" {
		t.Errorf("usage cost source = %q, want none — no session file carries a wire cost", usage.CostSource)
	}
	if usage.ProjectID != testProjectID || usage.Provider != "claude" || usage.TurnID != batch.Turns[0].TurnID {
		t.Errorf("usage attribution = %+v", usage)
	}
}

// A turn the session file never closed must not reach SQLite with a NULL
// completed_at: RecoverCrashedTurns would flip it to "interrupted" on the
// next boot, rewriting imported history as a crash.
func TestBuildSealsTurnTheSessionNeverClosed(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "claude", t.TempDir())

	batch, _, err := NewWriter(st, thread).Build([]importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventUserText, ThreadID: testThreadID,
			Content: "go", Timestamp: at(1),
		}, SourceUUID: "uuid-1"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: testThreadID,
			ItemID: "m1", Content: "working", Timestamp: at(4),
		}, SourceUUID: "uuid-4"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(batch.TurnCompletions) != 1 {
		t.Fatalf("want the open turn sealed, got %+v", batch.TurnCompletions)
	}
	completion := batch.TurnCompletions[0]
	if completion.CompletedAt != baseMillis+4 {
		t.Errorf("sealed completed_at = %d, want the turn's last activity %d", completion.CompletedAt, baseMillis+4)
	}
	if completion.StopReason != "" {
		t.Errorf("sealed stop reason = %q, want empty — the file reported none", completion.StopReason)
	}

	if err := st.ApplyImportBatch(thread.ID, batch); err != nil {
		t.Fatalf("apply: %v", err)
	}
	turns, err := st.ListRecentTurns(thread.ID, 10)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	for _, turn := range turns {
		if turn.CompletedAt == nil {
			t.Fatalf("turn %s persisted with a NULL completed_at", turn.TurnID)
		}
	}
}

// A refresh appends to a thread that already holds imported rows: turn
// indices continue past the last one instead of colliding with it.
// TestBuildAppendsAfterATurnsOnlyHistory pins the turn-index seed against
// a thread that holds TURNS but no items.
//
// A branch can convert to a turn and no rows at all (a turn whose every
// record was a skipped reasoning block), and a refresh of that thread has
// to continue past the turn it already wrote. Gating the seed on "does the
// thread have items" restarted the count at 1 and collided on the turns
// primary key. LastTurnIndex already unions items ∪ turns, so it is the
// whole seed.
func TestBuildAppendsAfterATurnsOnlyHistory(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "claude", t.TempDir())

	turnsOnly := []importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventTurnStart, ThreadID: testThreadID, Timestamp: at(1),
		}, SourceUUID: "uuid-1"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: testThreadID, Timestamp: at(2),
			TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
		}, SourceUUID: "uuid-2"},
	}
	batch, _, err := NewWriter(st, thread).Build(turnsOnly)
	if err != nil {
		t.Fatalf("build turns-only batch: %v", err)
	}
	if len(batch.Turns) != 1 || batch.Turns[0].TurnIndex != 1 {
		t.Fatalf("first batch turns = %+v, want exactly turn_index 1", batch.Turns)
	}
	if len(batch.Rows) != 0 {
		t.Fatalf("first batch wrote %d rows, want a turns-only batch", len(batch.Rows))
	}
	if err := st.ApplyImportBatch(thread.ID, batch); err != nil {
		t.Fatalf("apply turns-only batch: %v", err)
	}

	second, _, err := NewWriter(st, thread).Build([]importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventUserText, ThreadID: testThreadID,
			Content: "after the empty turn", Timestamp: at(20),
		}, SourceUUID: "uuid-20"},
	})
	if err != nil {
		t.Fatalf("build after a turns-only history: %v", err)
	}
	if len(second.Turns) != 1 || second.Turns[0].TurnIndex != 2 {
		t.Fatalf("second batch turns = %+v, want turn_index 2", second.Turns)
	}
	if err := st.ApplyImportBatch(thread.ID, second); err != nil {
		t.Fatalf("apply after a turns-only history: %v", err)
	}
}

func TestBuildRefusesATurnIDTheThreadAlreadyHolds(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "codex", t.TempDir())

	turn := func(kind provider.EventKind, offset int64, uuid string) importir.Event {
		evt := provider.ProviderEvent{
			Kind: kind, ThreadID: testThreadID, TurnID: "turn-1", Timestamp: at(offset),
		}
		if kind == provider.EventTurnComplete {
			evt.TurnComplete = &provider.WireTurnCompleteMeta{StopReason: "end_turn"}
		}
		return importir.Event{ProviderEvent: evt, SourceUUID: uuid}
	}
	buildAndApply(t, st, thread, []importir.Event{
		turn(provider.EventTurnStart, 1, "uuid-1"),
		turn(provider.EventTurnComplete, 2, "uuid-2"),
	})

	// The same wire turn again, as a refresh of a mid-turn import sees it.
	_, _, err := NewWriter(st, thread).Build([]importir.Event{
		turn(provider.EventTurnStart, 3, "uuid-3"),
		turn(provider.EventTurnComplete, 4, "uuid-4"),
	})
	if err == nil {
		t.Fatal("re-opening turn-1: want a refusal, got nil")
	}
	if !strings.Contains(err.Error(), "turn-1") {
		t.Errorf("refusal = %q, want it to name the colliding turn", err)
	}
}

func TestBuildAppendsAfterExistingHistory(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "claude", t.TempDir())

	first := []importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventUserText, ThreadID: testThreadID,
			Content: "one", Timestamp: at(1),
		}, SourceUUID: "uuid-1"},
	}
	buildAndApply(t, st, thread, first)

	second := []importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventUserText, ThreadID: testThreadID,
			Content: "two", Timestamp: at(20),
		}, SourceUUID: "uuid-20"},
	}
	batch, _, err := NewWriter(st, thread).Build(second)
	if err != nil {
		t.Fatalf("build refresh: %v", err)
	}
	if len(batch.Turns) != 1 || batch.Turns[0].TurnIndex != 2 {
		t.Fatalf("refresh turn = %+v, want turn_index 2", batch.Turns)
	}
	if got := batch.Rows[0].Item.ID; got != "user:2" {
		t.Errorf("refresh user row id = %q, want user:2", got)
	}
	if err := st.ApplyImportBatch(thread.ID, batch); err != nil {
		t.Fatalf("apply refresh: %v", err)
	}
	items, err := st.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 rows after refresh, got %d", len(items))
	}
	for _, item := range items {
		if item.ItemIndex != 0 {
			t.Errorf("row %s item_index = %d; indices are allocated per turn from 0", item.ID, item.ItemIndex)
		}
	}
}

func TestBuildAccumulatesConsecutiveDeltasIntoOneBlock(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "claude", t.TempDir())

	batch, _, err := NewWriter(st, thread).Build([]importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: testThreadID,
			ItemID: "m1", Content: "Hello ", Timestamp: at(1),
		}, SourceUUID: "uuid-1"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: testThreadID,
			ItemID: "m1", Content: "world.", Timestamp: at(2),
		}, SourceUUID: "uuid-2"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: testThreadID,
			ItemID: "m2", Content: "Second block.", Timestamp: at(3),
		}, SourceUUID: "uuid-3"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(batch.Rows) != 2 {
		t.Fatalf("want 2 assistant rows, got %d", len(batch.Rows))
	}
	if got := batch.Rows[0].Item.Summary; got != "Hello world." {
		t.Errorf("accumulated summary = %q", got)
	}
	if got := string(batch.Rows[0].Payload.Data); got != "Hello world." {
		t.Errorf("accumulated payload = %q", got)
	}
	if batch.Rows[0].Item.UpdatedAt != baseMillis+2 {
		t.Errorf("block updated_at = %d, want the last delta's %d", batch.Rows[0].Item.UpdatedAt, baseMillis+2)
	}
}

func TestBuildRefusesStructurallyBrokenInput(t *testing.T) {
	tests := []struct {
		name    string
		events  []importir.Event
		wantErr string
	}{
		{
			name: "tool completion with no launch",
			events: []importir.Event{{ProviderEvent: provider.ProviderEvent{
				Kind: provider.EventToolComplete, ThreadID: testThreadID,
				ItemID: "toolu_missing", Timestamp: at(1),
			}, SourceUUID: "uuid-1"}},
			wantErr: "has no launch",
		},
		{
			name: "item event with no timestamp",
			events: []importir.Event{{ProviderEvent: provider.ProviderEvent{
				Kind: provider.EventUserText, ThreadID: testThreadID, Content: "hi",
			}, SourceUUID: "uuid-1"}},
			wantErr: "no timestamp",
		},
		{
			name: "item event with no source coordinate",
			events: []importir.Event{{ProviderEvent: provider.ProviderEvent{
				Kind: provider.EventUserText, ThreadID: testThreadID,
				Content: "hi", Timestamp: at(1),
			}}},
			wantErr: "source uuid",
		},
		{
			name: "turn complete with no typed payload",
			events: []importir.Event{
				{ProviderEvent: provider.ProviderEvent{
					Kind: provider.EventUserText, ThreadID: testThreadID,
					Content: "hi", Timestamp: at(1),
				}, SourceUUID: "uuid-1"},
				{ProviderEvent: provider.ProviderEvent{
					Kind: provider.EventTurnComplete, ThreadID: testThreadID, Timestamp: at(2),
				}, SourceUUID: "uuid-2"},
			},
			wantErr: "no typed payload",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			thread := seedThread(t, st, testThreadID, "claude", t.TempDir())
			_, _, err := NewWriter(st, thread).Build(tc.events)
			if err == nil {
				t.Fatalf("want an error mentioning %q, got none", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestBuildWarnsRatherThanFailingOnRecoverableInput(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "claude", t.TempDir())

	_, warnings, err := NewWriter(st, thread).Build([]importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventUserText, ThreadID: testThreadID,
			Content: "hi", Timestamp: at(1),
		}, SourceUUID: "uuid-1"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventDiff, ThreadID: testThreadID,
			ItemID: "toolu_absent", Content: "@@ -1 +1 @@", Timestamp: at(2),
		}, SourceUUID: "uuid-2"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventBackgroundTaskTerminal, ThreadID: testThreadID,
			ItemID: "toolu_absent", Timestamp: at(3),
		}, SourceUUID: "uuid-3"},
	})
	if err != nil {
		t.Fatalf("recoverable input must not fail the import: %v", err)
	}
	codes := map[string]bool{}
	for _, warning := range warnings {
		codes[warning.Code] = true
	}
	for _, want := range []string{"import.unanchored-payload", "import.orphan-background-terminal"} {
		if !codes[want] {
			t.Errorf("missing warning %s; got %+v", want, warnings)
		}
	}
}

func TestBuildRefusesDuplicateItemIDs(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "claude", t.TempDir())

	_, _, err := NewWriter(st, thread).Build([]importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventNotification, ThreadID: testThreadID,
			ItemID: "note-1", Content: "first", Timestamp: at(1),
		}, SourceUUID: "uuid-1"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventNotification, ThreadID: testThreadID,
			ItemID: "note-1", Content: "second", Timestamp: at(2),
		}, SourceUUID: "uuid-2"},
	})
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("want a duplicate-id refusal, got %v", err)
	}
}

// Codex keys a turn's plan item as `<turnID>-plan` and re-persists a
// completed snapshot under that SAME id every time the plan changes —
// live item/completed is an upsert, so a re-completed plan replaces the
// row instead of tripping the duplicate-id refusal (found by the corpus
// smoke on a real rollout carrying two snapshots of one plan).
func TestBuildUpsertsAReCompletedProposedPlan(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "codex", t.TempDir())

	batch, _, err := NewWriter(st, thread).Build([]importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventTurnStart, ThreadID: testThreadID,
			TurnID: "turn-1", TurnIndex: 1, Timestamp: at(1),
		}, SourceUUID: "line:0", SourceOffset: 10},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventProposedPlan, ThreadID: testThreadID,
			TurnID: "turn-1", ItemID: "turn-1-plan", ItemType: "plan",
			Content: "# First Draft\n\n1. old step", Timestamp: at(2),
		}, SourceUUID: "line:10", SourceOffset: 20},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventProposedPlan, ThreadID: testThreadID,
			TurnID: "turn-1", ItemID: "turn-1-plan", ItemType: "plan",
			Content: "# Final Plan\n\n1. new step", Timestamp: at(3),
		}, SourceUUID: "line:20", SourceOffset: 30},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: testThreadID,
			TurnID: "turn-1", Timestamp: at(4),
			TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
		}, SourceUUID: "line:30", SourceOffset: 40},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var plans []store.ImportRow
	for _, r := range batch.Rows {
		if r.Item.ID == "turn-1-plan" {
			plans = append(plans, r)
		}
	}
	if len(plans) != 1 {
		t.Fatalf("plan rows = %d, want the one upserted row", len(plans))
	}
	plan := plans[0]
	if plan.Payload == nil || string(plan.Payload.Data) != "# Final Plan\n\n1. new step" {
		t.Fatalf("plan payload = %+v, want the later snapshot", plan.Payload)
	}
	if plan.Item.Summary != "Final Plan" {
		t.Fatalf("plan summary = %q, want the later title", plan.Item.Summary)
	}
	if plan.Item.UpdatedAt != at(3).UnixMilli() {
		t.Fatalf("plan updated_at = %d, want the later snapshot's time", plan.Item.UpdatedAt)
	}
	if !strings.Contains(plan.Item.Meta, "line:20") {
		t.Fatalf("plan provenance = %s, want the later record's coordinate", plan.Item.Meta)
	}
	// The carve-out is for plan rows only: any other kind under a reused
	// id is still the structural collision.
	_, _, err = NewWriter(st, thread).Build([]importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventNotification, ThreadID: testThreadID,
			ItemID: "turn-1-plan", Content: "note", Timestamp: at(1),
		}, SourceUUID: "line:0", SourceOffset: 10},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventProposedPlan, ThreadID: testThreadID,
			TurnID: "turn-1", ItemID: "turn-1-plan", ItemType: "plan",
			Content: "# Plan", Timestamp: at(2),
		}, SourceUUID: "line:10", SourceOffset: 20},
	})
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("want a duplicate-id refusal for a non-plan row, got %v", err)
	}
}

// Codex's wait_agent / resume_agent completions get their own
// tool_completion sibling: the launch says the parent waited, the
// sibling says what it was told.
func TestBuildSplitsCodexWaitAgentCompletion(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, "thread-codex", "codex", t.TempDir())

	batch, _, err := NewWriter(st, thread).Build([]importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: thread.ID,
			ItemID: "call_wait_1", ItemType: "wait_agent",
			Meta:      json.RawMessage(`{"toolName":"wait_agent","input":{"agent_id":"a1"}}`),
			Timestamp: at(1),
		}, SourceUUID: "line:100", SourceOffset: 100},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventToolComplete, ThreadID: thread.ID,
			ItemID: "call_wait_1", Content: "agent finished",
			Meta:      json.RawMessage(`{"toolName":"wait_agent"}`),
			Timestamp: at(2),
		}, SourceUUID: "line:200", SourceOffset: 200},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(batch.Rows) != 2 {
		t.Fatalf("want a launch + sibling, got %d rows", len(batch.Rows))
	}
	launch, sibling := batch.Rows[0].Item, batch.Rows[1].Item
	if launch.Status != "completed" {
		t.Errorf("launch status = %q, want completed", launch.Status)
	}
	if sibling.Kind != "tool_completion" || sibling.CompletionOf != launch.ID {
		t.Errorf("sibling = %+v, want a tool_completion of %s", sibling, launch.ID)
	}
	if sibling.TurnIndex != launch.TurnIndex {
		t.Errorf("sibling turn %d != launch turn %d", sibling.TurnIndex, launch.TurnIndex)
	}

	// A claude thread settles the same shape in place — the split is
	// provider-scoped, not tool-name-scoped.
	claudeThread := seedThread(t, st, "thread-claude-wait", "claude", t.TempDir())
	claudeBatch, _, err := NewWriter(st, claudeThread).Build([]importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: claudeThread.ID,
			ItemID: "call_wait_1", ItemType: "wait_agent",
			Meta:      json.RawMessage(`{"toolName":"wait_agent"}`),
			Timestamp: at(1),
		}, SourceUUID: "uuid-1"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventToolComplete, ThreadID: claudeThread.ID,
			ItemID: "call_wait_1", Content: "done", Timestamp: at(2),
		}, SourceUUID: "uuid-2"},
	})
	if err != nil {
		t.Fatalf("build claude: %v", err)
	}
	if len(claudeBatch.Rows) != 1 {
		t.Fatalf("claude wait_agent produced %d rows, want the launch settled in place", len(claudeBatch.Rows))
	}
}

// A backgrounded Claude Task keeps its launch running through the
// placeholder tool_result; the observed terminal writes the sibling.
func TestBuildSettlesBackgroundTaskOnTerminal(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "claude", t.TempDir())

	events := []importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: testThreadID,
			ItemID: "toolu_task_1", ItemType: "Task",
			Meta:      json.RawMessage(`{"toolName":"Task","is_background":true,"input":{"description":"scan"}}`),
			Timestamp: at(1),
		}, SourceUUID: "uuid-1"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventToolComplete, ThreadID: testThreadID,
			ItemID: "toolu_task_1", Content: "launched", Timestamp: at(2),
		}, SourceUUID: "uuid-2"},
	}
	batch, _, err := NewWriter(st, thread).Build(events)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(batch.Rows) != 1 || batch.Rows[0].Item.Status != statusRunning {
		t.Fatalf("backgrounded launch = %+v, want a single still-running row", batch.Rows)
	}

	events = append(events, importir.Event{
		ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventBackgroundTaskTerminal, ThreadID: testThreadID,
			ItemID: "toolu_task_1", Content: "scan complete", Timestamp: at(3),
		}, SourceUUID: "uuid-3",
	})
	batch, _, err = NewWriter(st, thread).Build(events)
	if err != nil {
		t.Fatalf("build with terminal: %v", err)
	}
	if len(batch.Rows) != 2 {
		t.Fatalf("want launch + sibling, got %d rows", len(batch.Rows))
	}
	if batch.Rows[0].Item.Status != statusCompleted {
		t.Errorf("launch status = %q, want completed", batch.Rows[0].Item.Status)
	}
	if batch.Rows[1].Item.CompletionOf != "toolu_task_1" {
		t.Errorf("sibling = %+v, want a completion of the launch", batch.Rows[1].Item)
	}
}

// The background carve-out belongs to a launch that is actually in this
// file. An `import_unavailable` orphan synthesizes its own launch row, and
// the terminal that would settle a backgrounded one is outside the imported
// range by construction — so parking that row on `running` (which the
// turn-boundary force-close then exempts, invariant 24) leaves a card
// spinning for the life of the thread. A placeholder always settles.
func TestBuildSettlesABackgroundFlaggedOrphanCompletion(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "claude", t.TempDir())

	batch, _, err := NewWriter(st, thread).Build([]importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventUserText, ThreadID: testThreadID,
			Content: "go", Timestamp: at(0),
		}, SourceUUID: "uuid-0"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventToolComplete, ThreadID: testThreadID,
			ItemID: "toolu_orphan_bg", ItemType: "Task", Content: "launched",
			Meta: json.RawMessage(`{"toolName":"Task","is_background":true,` +
				`"import_unavailable":"tool-output-gc"}`),
			Timestamp: at(1),
		}, SourceUUID: "uuid-1"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: testThreadID, Timestamp: at(9),
			TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
		}, SourceUUID: "uuid-9"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(batch.Rows) != 2 {
		t.Fatalf("want the prompt plus one placeholder tool row, got %d", len(batch.Rows))
	}
	placeholder := batch.Rows[1].Item
	if placeholder.ID != "toolu_orphan_bg" || placeholder.Kind != kindToolCall {
		t.Fatalf("row = %+v, want the synthesized tool_call launch", placeholder)
	}
	if placeholder.Status == statusRunning {
		t.Errorf("placeholder status = %q — nothing in this import can ever settle it", placeholder.Status)
	}
	if got := metaKey(t, placeholder.Meta, itemmeta.ImportUnavailableKey); got != "tool-output-gc" {
		t.Errorf("import_unavailable = %q, want the marker that made the orphan legal", got)
	}
}

// A backgrounded launch that IS in the file keeps its carve-out: a real
// terminal can still arrive and it owns the outcome.
func TestBuildKeepsTheCarveOutForARealBackgroundLaunch(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "claude", t.TempDir())

	batch, _, err := NewWriter(st, thread).Build([]importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: testThreadID,
			ItemID: "toolu_bg", ItemType: "Task",
			Meta:      json.RawMessage(`{"toolName":"Task","is_background":true,"input":{"description":"watch"}}`),
			Timestamp: at(1),
		}, SourceUUID: "uuid-1"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventToolComplete, ThreadID: testThreadID,
			ItemID: "toolu_bg", Content: "launched", Timestamp: at(2),
		}, SourceUUID: "uuid-2"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(batch.Rows) != 1 || batch.Rows[0].Item.Status != statusRunning {
		t.Fatalf("backgrounded launch = %+v, want a single still-running row", batch.Rows)
	}
}

func TestBuildNestsSubagentRowsUnderTheirLaunch(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "claude", t.TempDir())

	batch, _, err := NewWriter(st, thread).Build([]importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: testThreadID,
			ItemID: "toolu_task_1", ItemType: "Task",
			Meta:      json.RawMessage(`{"toolName":"Task","input":{"description":"scan"}}`),
			Timestamp: at(1),
		}, SourceUUID: "uuid-1"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: testThreadID,
			ItemID: "child-msg-1", Content: "child output",
			ParentToolUseID: "toolu_task_1", Timestamp: at(2),
		}, SourceUUID: "uuid-2"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	child := batch.Rows[1].Item
	if child.ParentID != "toolu_task_1" {
		t.Errorf("child parent = %q, want toolu_task_1", child.ParentID)
	}
	if !strings.Contains(child.ID, "toolu_task_1") {
		t.Errorf("child id %q is not scoped to its launch", child.ID)
	}
}

func TestBuildRejectsWriterWithoutStoreOrThread(t *testing.T) {
	if _, _, err := NewWriter(nil, store.Thread{ID: testThreadID}).Build(nil); err == nil {
		t.Error("want an error for a writer with no store")
	}
	st := newTestStore(t)
	if _, _, err := NewWriter(st, store.Thread{}).Build(nil); err == nil {
		t.Error("want an error for a writer whose thread has no id")
	}
}

func metaKey(t *testing.T, meta, key string) string {
	t.Helper()
	if meta == "" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(meta), &obj); err != nil {
		t.Fatalf("decode meta %q: %v", meta, err)
	}
	value, _ := obj[key].(string)
	return value
}

// Invariant 23: a turn boundary settles the timeline whether or not the
// provider reported every tool_result. An imported thread has no live
// session to settle it later, so the writer owns the same safety net —
// with invariant 24's exemption for backgrounded launches.
func TestBuildForceClosesUnresolvedToolsAtTurnEnd(t *testing.T) {
	events := []importir.Event{
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventUserText, ThreadID: testThreadID,
			Content: "go", Timestamp: at(0),
		}, SourceUUID: "uuid-0"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: testThreadID,
			ItemID: "toolu_stuck", ItemType: "Bash",
			Meta:      json.RawMessage(`{"toolName":"Bash","input":{"command":"sleep 1"}}`),
			Timestamp: at(1),
		}, SourceUUID: "uuid-1"},
		{ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: testThreadID,
			ItemID: "toolu_bg", ItemType: "Task",
			Meta:      json.RawMessage(`{"toolName":"Task","is_background":true,"input":{"description":"watch"}}`),
			Timestamp: at(2),
		}, SourceUUID: "uuid-2"},
	}
	settled := append(append([]importir.Event(nil), events...), importir.Event{
		ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: testThreadID, Timestamp: at(9),
			TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
		}, SourceUUID: "uuid-9",
	})

	// Both the explicit turn-complete and the batch seal (a session file
	// that simply stops) must apply the same settle.
	for _, tc := range []struct {
		name   string
		events []importir.Event
	}{
		{"explicit turn complete", settled},
		{"session file ends mid-turn", events},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			thread := seedThread(t, st, testThreadID, "claude", t.TempDir())
			batch, _, err := NewWriter(st, thread).Build(tc.events)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			byID := map[string]store.Item{}
			for _, row := range batch.Rows {
				byID[row.Item.ID] = row.Item
			}
			stuck := byID["toolu_stuck"]
			if stuck.Status != statusErrored {
				t.Errorf("unresolved tool status = %q, want errored", stuck.Status)
			}
			if !strings.Contains(stuck.Summary, "turn ended with tool unresolved") {
				t.Errorf("unresolved tool summary = %q, want the force-close marker", stuck.Summary)
			}
			if bg := byID["toolu_bg"]; bg.Status != statusRunning {
				t.Errorf("backgrounded launch status = %q, want it left running", bg.Status)
			}
		})
	}
}
