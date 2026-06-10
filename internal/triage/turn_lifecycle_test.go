package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func TestActiveTurnSnapshotTracksAndClearsLiveRound(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startedAt := time.UnixMilli(1_700_000_000_000)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnIndex: 3,
		Timestamp: startedAt,
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	snapshot, ok := router.ActiveTurnSnapshot("t1")
	if !ok {
		t.Fatalf("expected active turn snapshot")
	}
	if snapshot.ThreadID != "t1" || snapshot.TurnID == "" || snapshot.TurnIndex != 3 || snapshot.StartedAt != startedAt.UnixMilli() {
		t.Fatalf("snapshot = %+v, want thread=t1 non-empty turn id index=3 startedAt=%d", snapshot, startedAt.UnixMilli())
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.UnixMilli(1_700_000_000_500),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
	if snapshot, ok := router.ActiveTurnSnapshot("t1"); ok {
		t.Fatalf("active turn snapshot leaked after complete: %+v", snapshot)
	}
}

// TestTurnCompleteTruncatedFlipsRunningAndDrainsQueueAsErrored is the
// spec-critical contract for turn interruption. A single
// EventTurnComplete with provider.TruncatedTurnCompleteMeta must:
//
//  1. Flip every still-streaming NON-BACKGROUND item on that turn to
//     status=errored with summary suffixed by " — interrupted"
//     (em-dash + " interrupted"). Backgrounded tool_call launches are
//     EXEMPT per invariant 24 — they legitimately outlive the turn
//     and their status stays running.
//  2. Drain the interrupt queue AS ERRORED — every queued background
//     completion lands with status=errored and the interrupted suffix,
//     mirroring the streaming flip. The previous ordering (idle drain
//     first, forced drain last) left queued rows as 'completed',
//     which contradicted the spec; handleTurnComplete now forces the
//     queue drain BEFORE settling streaming so the idle-drain path
//     never sees the queue.
//  3. Leave the interrupt queue empty afterward so a late event can't
//     resurrect a settled turn.
//
// The setup (streaming text + backgrounded launch + task terminal
// queued) is the minimum that exercises the queue-drain codepath and
// the invariant 24 bg-launch exemption together.
func TestTurnCompleteTruncatedFlipsRunningAndDrainsQueueAsErrored(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// 1. A background tool_call start. Placed BEFORE the streaming text
	// because handleToolStart calls settleStreamingScope, which would
	// prematurely close an open text block.
	bgStartMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 10"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-running",
		ItemType: "Bash", Meta: bgStartMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg start: %v", err)
	}

	// 2. A streaming assistant_text item, opened via EventTextDelta. This
	// puts streamingItemCounts[t1] > 0, so the bg terminal that arrives
	// next queues instead of persisting.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "mid-sentence",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	// 3. EventBackgroundTaskTerminal for the background task — this
	// fires the sibling-row upsert, which goes onto the interrupt queue
	// because text is streaming. source=task_output exercises the
	// observation path that writes the sibling (task_updated alone
	// would only stash).
	bgTerminalMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-1",
		"tool_use_id": "bg-running",
		"status":      "completed",
		"exit_code":   0,
		"source":      "task_output",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-running",
		Meta: bgTerminalMeta, Content: "done body", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg terminal: %v", err)
	}

	// Sanity: the queued bg completion is NOT yet persisted; only the
	// streaming text and the launch row exist so far.
	before, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list before: %v", err)
	}
	for _, it := range before {
		if it.Kind == itemKindBackgroundDone {
			t.Fatalf("bg_done persisted too early: %+v", it)
		}
	}
	router.mu.Lock()
	queuedBefore := len(router.interruptQueue["t1"])
	router.mu.Unlock()
	if queuedBefore != 1 {
		t.Fatalf("expected 1 queued completion before turn-complete, got %d", queuedBefore)
	}

	// 4. A truncated EventTurnComplete must flip everything
	// interrupted and drain the queue.
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: &provider.TruncatedTurnCompleteMeta{},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete truncated: %v", err)
	}

	after, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list after: %v", err)
	}

	const interruptedSuffix = " — interrupted"
	var (
		sawText, sawBgLaunch, sawQueuedDone bool
	)
	for _, it := range after {
		t.Logf("after turn-complete: id=%s kind=%s status=%s summary=%q isBg=%v",
			it.ID, it.Kind, it.Status, it.Summary, it.IsBackground)
		switch it.Kind {
		case "assistant_text":
			sawText = true
			if it.Status != statusErrored {
				t.Errorf("streaming text status = %q, want errored", it.Status)
			}
			if !strings.HasSuffix(it.Summary, interruptedSuffix) {
				t.Errorf("streaming text summary missing %q suffix: %q",
					interruptedSuffix, it.Summary)
			}
		case itemKindToolCall:
			// Invariant 24: backgrounded launches must stay running
			// across a truncated turn. The sibling tool_completion
			// row (drained from the interrupt queue, below) is what
			// carries the interrupted marker for the task itself;
			// the launch row keeps status=running so
			// BackgroundTaskTray still renders the live task.
			sawBgLaunch = true
			if !it.IsBackground {
				t.Errorf("unexpected inline tool_call row with id=%s status=%s", it.ID, it.Status)
			}
			if it.Status != statusRunning {
				t.Errorf("bg launch status = %q, want running (invariant 24 exempts bg launches from truncated-turn flip)",
					it.Status)
			}
			if strings.HasSuffix(it.Summary, interruptedSuffix) {
				t.Errorf("bg launch summary picked up %q suffix despite exemption: %q",
					interruptedSuffix, it.Summary)
			}
		case itemKindBackgroundDone:
			// Spec: queued background completions drained during a
			// truncated turn-complete must land as errored with the
			// interrupted suffix, the same as the streaming items.
			// A completed-status row here means the old quiet-settle
			// path ran and reopened the regression.
			sawQueuedDone = true
			if it.Status != statusErrored {
				t.Errorf("queued bg_done status = %q, want errored", it.Status)
			}
			if !strings.HasSuffix(it.Summary, interruptedSuffix) {
				t.Errorf("queued bg_done summary missing %q suffix: %q",
					interruptedSuffix, it.Summary)
			}
		}
	}
	if !sawText {
		t.Error("no assistant_text row found after turn-complete")
	}
	if !sawBgLaunch {
		t.Error("no tool_call row found after turn-complete")
	}
	if !sawQueuedDone {
		t.Error("queued bg_done was not drained after truncation")
	}

	// 5. Post-drain, the interrupt queue must be empty so a late stray
	// event cannot reopen the turn.
	router.mu.Lock()
	remaining := len(router.interruptQueue["t1"])
	router.mu.Unlock()
	if remaining != 0 {
		t.Errorf("interrupt queue still has %d entries after truncation drain", remaining)
	}
}

// TestHandleEventTurnStart_InsertsTurnRow pins the Wave 2 turns-table
// wiring: a single EventTurnStart must (a) insert a turns row with
// completed_at=NULL, (b) emit provider:turn_started with the settle
// payload shape the frontend consumes, and (c) be idempotent under
// re-sent EventTurnStart (Claude re-init after an interrupt) — the
// second insert must not fail on the UNIQUE(thread_id, turn_index)
// constraint and must not reset started_at.
func TestHandleEventTurnStart_InsertsTurnRow(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	startedAt := time.UnixMilli(1_700_000_000_000)
	evt := provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnIndex: 5,
		Timestamp: startedAt,
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle turn start: %v", err)
	}

	expectedTurnID := "t1:5"
	turn, found, err := st.GetTurn(expectedTurnID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if !found {
		t.Fatal("expected turns row inserted after EventTurnStart")
	}
	if turn.ThreadID != "t1" || turn.TurnIndex != 5 {
		t.Errorf("turn shape = %+v, want thread=t1 idx=5", turn)
	}
	if turn.CompletedAt != nil {
		t.Errorf("expected completed_at=NULL, got %v", *turn.CompletedAt)
	}
	if turn.StartedAt != startedAt.UnixMilli() {
		t.Errorf("started_at = %d, want %d", turn.StartedAt, startedAt.UnixMilli())
	}

	// Emission shape. The frontend payload's TurnID is a per-round
	// uuid (allocated by setOpenRound at the top of handleTurnStart) —
	// distinct from the persisted logical-turn id (`expectedTurnID =
	// "t1:5"`) on purpose. Frontend idempotency keys on TurnID and
	// each wire round (re-round, retry) needs its own visible
	// identifier; persistence uses the logical id so all rounds of
	// one logical turn share a single `turns` row. See
	// internal/triage/AGENTS.md "Wire-round vs logical-turn".
	started := filterEmissions(*emissions, "provider:turn_started")
	if len(started) != 1 {
		t.Fatalf("expected 1 provider:turn_started emission, got %d", len(started))
	}
	payload, ok := started[0].data.(TurnStartedEvent)
	if !ok {
		t.Fatalf("emission payload type = %T, want TurnStartedEvent", started[0].data)
	}
	if payload.TurnID == "" {
		t.Errorf("payload.TurnID is empty — round id must be allocated on turn_start")
	}
	if payload.TurnID == expectedTurnID {
		t.Errorf("payload.TurnID = %q must NOT equal the persisted logical id (per-round uuid expected)", payload.TurnID)
	}
	if payload.TurnIndex != 5 {
		t.Errorf("payload.TurnIndex = %d, want 5", payload.TurnIndex)
	}
	if payload.StartedAt != startedAt.UnixMilli() {
		t.Errorf("payload.StartedAt = %d, want %d", payload.StartedAt, startedAt.UnixMilli())
	}

	// Idempotency: re-send the same EventTurnStart (Claude re-init path).
	// Must not error, must preserve started_at on the turns row.
	later := startedAt.Add(5 * time.Second)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnIndex: 5,
		Timestamp: later,
	}); err != nil {
		t.Fatalf("re-send turn start: %v", err)
	}
	turn2, found2, err := st.GetTurn(expectedTurnID)
	if err != nil {
		t.Fatalf("get turn after re-send: %v", err)
	}
	if !found2 {
		t.Fatal("turn row vanished on re-send")
	}
	if turn2.StartedAt != startedAt.UnixMilli() {
		t.Errorf("started_at mutated on re-send: got %d, want %d", turn2.StartedAt, startedAt.UnixMilli())
	}
}

// TestHandleEventTurnComplete_UpdatesTurnRow verifies the happy-path
// settle: start a turn, complete it with a rich typed payload, then
// assert the turns row captured every field and provider:turn_completed
// fired with the corresponding shape.
func TestHandleEventTurnComplete_UpdatesTurnRow(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	startedAt := time.UnixMilli(1_700_000_000_000)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 3,
		Timestamp: startedAt,
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	completedAt := startedAt.Add(2500 * time.Millisecond)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason:         "end_turn",
			AssistantMessageID: "msg_01abc",
			Usage:              &provider.TokenUsage{InputTokens: 123, OutputTokens: 45},
		},
		Timestamp: completedAt,
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	turn, found, err := st.GetTurn("t1:3")
	if err != nil || !found {
		t.Fatalf("get turn: found=%v err=%v", found, err)
	}
	if turn.CompletedAt == nil {
		t.Fatal("expected completed_at populated")
	}
	if *turn.CompletedAt != completedAt.UnixMilli() {
		t.Errorf("completed_at = %d, want %d", *turn.CompletedAt, completedAt.UnixMilli())
	}
	if turn.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", turn.StopReason)
	}
	if turn.AssistantMessageID != "msg_01abc" {
		t.Errorf("assistant_message_id = %q, want msg_01abc", turn.AssistantMessageID)
	}
	if !strings.Contains(turn.TokenUsageJSON, "inputTokens") {
		t.Errorf("token_usage_json missing inputTokens: %q", turn.TokenUsageJSON)
	}
	if turn.ErrorMessage != "" {
		t.Errorf("error_message = %q, want empty for happy path", turn.ErrorMessage)
	}

	// Emission shape. The completed payload's TurnID is the per-round
	// uuid that turn_started allocated — same round, same id —
	// distinct from the persisted logical id ("t1:3"). See
	// internal/triage/AGENTS.md "Wire-round vs logical-turn".
	started := filterEmissions(*emissions, "provider:turn_started")
	if len(started) != 1 {
		t.Fatalf("expected 1 provider:turn_started emission, got %d", len(started))
	}
	startedRoundID := started[0].data.(TurnStartedEvent).TurnID

	completed := filterEmissions(*emissions, "provider:turn_completed")
	if len(completed) != 1 {
		t.Fatalf("expected 1 provider:turn_completed emission, got %d", len(completed))
	}
	payload, ok := completed[0].data.(TurnCompletedEvent)
	if !ok {
		t.Fatalf("payload type = %T, want TurnCompletedEvent", completed[0].data)
	}
	if payload.TurnID == "t1:3" {
		t.Errorf("payload.TurnID = %q must NOT equal the persisted logical id (per-round uuid expected)", payload.TurnID)
	}
	if payload.TurnID != startedRoundID {
		t.Errorf("payload.TurnID = %q, want matching round id from turn_started %q", payload.TurnID, startedRoundID)
	}
	if payload.StopReason != "end_turn" {
		t.Errorf("payload.StopReason = %q, want end_turn", payload.StopReason)
	}
	if payload.AssistantMessageID != "msg_01abc" {
		t.Errorf("payload.AssistantMessageID = %q, want msg_01abc", payload.AssistantMessageID)
	}
	if !payload.CountsAsActivity {
		t.Error("payload.CountsAsActivity = false, want true for top-level turn complete")
	}
}

func TestHandleEventTurnComplete_NestedEmissionDoesNotCountAsActivity(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	startedAt := time.UnixMilli(1_700_000_000_000)
	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventTurnStart,
		ThreadID:        "t1",
		TurnIndex:       3,
		ParentToolUseID: "spawn-1",
		Timestamp:       startedAt,
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventTurnComplete,
		ThreadID:        "t1",
		ParentToolUseID: "spawn-1",
		TurnComplete:    &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
		Timestamp:       startedAt.Add(2500 * time.Millisecond),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	completed := filterEmissions(*emissions, "provider:turn_completed")
	if len(completed) != 1 {
		t.Fatalf("expected 1 provider:turn_completed emission, got %d", len(completed))
	}
	payload, ok := completed[0].data.(TurnCompletedEvent)
	if !ok {
		t.Fatalf("payload type = %T, want TurnCompletedEvent", completed[0].data)
	}
	if payload.CountsAsActivity {
		t.Error("payload.CountsAsActivity = true, want false for nested turn complete")
	}

	turn, found, err := st.GetTurn("t1:3")
	if err != nil || !found {
		t.Fatalf("get turn: found=%v err=%v", found, err)
	}
	if turn.CompletedAt != nil {
		t.Fatalf("nested turn complete should not settle top-level turn row, got completed_at=%d", *turn.CompletedAt)
	}
}

func TestHandleEventTurnCompleteDuplicatePersistsLateUsage(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startedAt := time.UnixMilli(1_700_000_000_000)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnIndex: 3,
		Timestamp: startedAt,
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    startedAt.Add(time.Second),
	}); err != nil {
		t.Fatalf("first complete: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:     provider.EventTurnComplete,
		ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{
			Usage: &provider.TokenUsage{InputTokens: 123, OutputTokens: 45},
		},
		Timestamp: startedAt.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("duplicate complete: %v", err)
	}

	turn, found, err := st.GetTurn("t1:3")
	if err != nil || !found {
		t.Fatalf("get turn: found=%v err=%v", found, err)
	}
	if !strings.Contains(turn.TokenUsageJSON, "inputTokens") {
		t.Fatalf("late token_usage_json not persisted: %q", turn.TokenUsageJSON)
	}
}

// TestHandleEventTurnComplete_ForceClosesOrphans pins invariant 23:
// at turn-complete, any status=running + is_background=false
// tool_call rows on the turn must flip to errored with a synthesized
// "turn ended with tool unresolved" summary. The safety net kicks in
// when a provider drops a tool_result.
func TestHandleEventTurnComplete_ForceClosesOrphans(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// Two inline (non-background) tool_calls open in this turn. Neither
	// gets a matching EventToolComplete — simulating a dropped result.
	for _, id := range []string{"inline-1", "inline-2"} {
		startMeta, _ := json.Marshal(map[string]any{
			"toolName": "Bash",
			"input":    map[string]any{"command": "whoami"},
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: "t1", ItemID: id,
			ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
	}

	// End the turn without any tool_complete.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	for _, id := range []string{"inline-1", "inline-2"} {
		item, ok, err := st.GetThreadItem("t1", id)
		if err != nil || !ok {
			t.Fatalf("missing %s: found=%v err=%v", id, ok, err)
		}
		if item.Status != statusErrored {
			t.Errorf("%s status = %q, want errored (force-close safety net)", id, item.Status)
		}
		if !strings.Contains(item.Summary, "turn ended with tool unresolved") {
			t.Errorf("%s summary missing force-close marker: %q", id, item.Summary)
		}
	}
}

// TestHandleEventTurnComplete_ExemptsBackgroundedLaunches pins
// invariant 24: backgrounded launches (is_background=true) must NOT
// be force-closed at turn-complete. The background work legitimately
// outlives its launching turn — the sibling tool_completion row
// arrives later via EventBackgroundTaskTerminal.
func TestHandleEventTurnComplete_ExemptsBackgroundedLaunches(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// One backgrounded launch (is_background=true, status=running).
	bgStartMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 60"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-exempt",
		ItemType: "Bash", Meta: bgStartMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg start: %v", err)
	}

	// One inline launch in parallel — exercises the force-close path
	// alongside the exemption so we're sure the iteration isn't
	// globally short-circuited by the first bg row.
	inlineMeta, _ := json.Marshal(map[string]any{
		"toolName": "Bash",
		"input":    map[string]any{"command": "true"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "inline-exempt",
		ItemType: "Bash", Meta: inlineMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("inline start: %v", err)
	}

	// End the turn.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	// Background launch stays running — it must NOT be force-closed.
	bg, ok, err := st.GetThreadItem("t1", "bg-exempt")
	if err != nil || !ok {
		t.Fatalf("missing bg-exempt: found=%v err=%v", ok, err)
	}
	if bg.Status != statusRunning {
		t.Errorf("bg launch status = %q, want running (invariant 24 exempts bg launches)", bg.Status)
	}
	if strings.Contains(bg.Summary, "turn ended with tool unresolved") {
		t.Errorf("bg launch picked up force-close marker despite is_background=true: %q", bg.Summary)
	}

	// Inline launch MUST be force-closed in the same pass.
	inline, ok, err := st.GetThreadItem("t1", "inline-exempt")
	if err != nil || !ok {
		t.Fatalf("missing inline-exempt: found=%v err=%v", ok, err)
	}
	if inline.Status != statusErrored {
		t.Errorf("inline launch status = %q, want errored", inline.Status)
	}
}

// TestHandleEventTurnComplete_InterruptedMapsCanonicalStopReason pins
// the Meta.aborted → stop_reason "interrupted" normalization. A turn
// the provider aborts must surface as interrupted in the turns row
// and on the frontend payload — regardless of whatever the provider's
// stop_reason happened to be (Claude's "error_during_execution"
// subtype or Codex's normalized interrupted stop reason.
func TestHandleEventTurnComplete_InterruptedMapsCanonicalStopReason(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// aborted=true must override any other stop_reason.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason: "end_turn",
			Aborted:    true,
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn complete aborted: %v", err)
	}

	turn, found, err := st.GetTurn("t1:1")
	if err != nil || !found {
		t.Fatalf("get turn: found=%v err=%v", found, err)
	}
	if turn.StopReason != "interrupted" {
		t.Errorf("stop_reason = %q, want interrupted (aborted overrides)", turn.StopReason)
	}

	completed := filterEmissions(*emissions, "provider:turn_completed")
	if len(completed) != 1 {
		t.Fatalf("expected 1 provider:turn_completed, got %d", len(completed))
	}
	payload := completed[0].data.(TurnCompletedEvent)
	if payload.StopReason != "interrupted" {
		t.Errorf("payload.StopReason = %q, want interrupted", payload.StopReason)
	}
	if !payload.Aborted {
		t.Error("payload.Aborted = false, want true")
	}
}

// TestMarkUserInterrupt_ExemptsBackgroundedLaunches pins invariant 24
// on the user-interrupt (Esc) path. A user hitting stop mid-turn flips
// every running/streaming NON-BACKGROUND item with a " — stopped"
// suffix, but backgrounded tool_call launches must stay running — the
// background task legitimately outlives the turn, and the sibling
// tool_completion row (written later via EventBackgroundTaskTerminal)
// is what carries the stopped marker if the task itself gets
// interrupted. Without the exemption the BackgroundTaskTray would
// flip the launch badge to errored the moment the user presses Esc,
// even though the task is still running in Claude.
func TestMarkUserInterrupt_ExemptsBackgroundedLaunches(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// One backgrounded launch (is_background=true, status=running).
	bgMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 60"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-stopped-exempt",
		ItemType: "Bash", Meta: bgMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg start: %v", err)
	}

	// One inline launch in parallel so we confirm the loop still flips
	// non-bg rows while respecting the exemption.
	inlineMeta, _ := json.Marshal(map[string]any{
		"toolName": "Bash",
		"input":    map[string]any{"command": "true"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "inline-stopped",
		ItemType: "Bash", Meta: inlineMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("inline start: %v", err)
	}

	errID, err := router.MarkUserInterrupt("t1")
	if err != nil {
		t.Fatalf("MarkUserInterrupt: %v", err)
	}
	if errID == "" {
		t.Fatal("MarkUserInterrupt returned empty error id")
	}

	// Backgrounded launch stays running — it must NOT be flipped.
	bg, ok, err := st.GetThreadItem("t1", "bg-stopped-exempt")
	if err != nil || !ok {
		t.Fatalf("missing bg-stopped-exempt: found=%v err=%v", ok, err)
	}
	if bg.Status != statusRunning {
		t.Errorf("bg launch status = %q, want running (invariant 24 exempts bg launches from user-interrupt flip)",
			bg.Status)
	}
	if strings.HasSuffix(bg.Summary, " — stopped") {
		t.Errorf("bg launch summary picked up stopped suffix despite exemption: %q", bg.Summary)
	}

	// Inline launch must flip to errored with the stopped suffix.
	inline, ok, err := st.GetThreadItem("t1", "inline-stopped")
	if err != nil || !ok {
		t.Fatalf("missing inline-stopped: found=%v err=%v", ok, err)
	}
	if inline.Status != statusErrored {
		t.Errorf("inline launch status = %q, want errored", inline.Status)
	}
	if !strings.HasSuffix(inline.Summary, " — stopped") {
		t.Errorf("inline launch summary = %q, want trailing ' — stopped'", inline.Summary)
	}

	// "Stopped by user" system error row is persisted.
	sys, ok, err := st.GetThreadItem("t1", errID)
	if err != nil || !ok {
		t.Fatalf("missing system error row %s: found=%v err=%v", errID, ok, err)
	}
	if sys.Summary != "Stopped by user" {
		t.Errorf("system error summary = %q, want 'Stopped by user'", sys.Summary)
	}
}

// TestHandleEventTurnStart_UsesWireTurnIDForCodex pins the
// resolveTurnID branch that prefers the provider-supplied `evt.TurnID`
// over the synthetic `<threadID>:<turnIndex>` fallback. Codex fills
// evt.TurnID from `turn/started.turn.id`; Claude has no wire-level
// turn_id and leaves the field empty, so the synthetic path kicks in
// (see TestHandleEventTurnStart_InsertsTurnRow which exercises that
// branch). This test exercises the Codex branch end-to-end:
//   - EventTurnStart with evt.TurnID="turn-codex-1" inserts a turns
//     row keyed by "turn-codex-1" (NOT "t1:0"). PERSISTED row id is
//     the logical-turn id from resolveTurnID.
//   - A follow-up EventTurnComplete with the SAME evt.TurnID updates
//     that row rather than creating or touching a sibling.
//   - The frontend payload (TurnStartedEvent / TurnCompletedEvent)
//     carries a per-round uuid as TurnID — frontend idempotency keys
//     on TurnID, so each wire round (re-round in the multi-result-
//     per-turn pattern, retried turn/started in Codex, etc.) gets a
//     fresh frontend-visible identifier even when the persisted row
//     keeps the same logical id. See internal/triage/AGENTS.md
//     "Wire-round vs logical-turn" for the cadence rules.
//
// Regression-guard rationale: the resolveTurnID helper is a single
// `if id != "" return id` branch. If someone swaps the condition or
// adds a provider check that falls through to the synthetic path,
// the turns table's primary key would flip between turn_index=0 and
// whatever Codex sent, splitting one logical turn into two rows.
// See docs/architecture/turn-lifecycle.md §Participants and
// turn_lifecycle.go resolveTurnID.
func TestHandleEventTurnStart_UsesWireTurnIDForCodex(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	const wireTurnID = "turn-codex-1"
	startedAt := time.UnixMilli(1_700_000_000_000)

	// Turn start with evt.TurnID populated — Codex's turn/started path.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnID:    wireTurnID,
		TurnIndex: 0,
		Timestamp: startedAt,
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// The turns row is keyed by the WIRE id, not the synthetic one.
	turn, found, err := st.GetTurn(wireTurnID)
	if err != nil {
		t.Fatalf("get turn by wire id: %v", err)
	}
	if !found {
		t.Fatalf("expected turn row keyed by wire id %q", wireTurnID)
	}
	if turn.ThreadID != "t1" {
		t.Errorf("turn.ThreadID = %q, want t1", turn.ThreadID)
	}
	if turn.StartedAt != startedAt.UnixMilli() {
		t.Errorf("turn.StartedAt = %d, want %d", turn.StartedAt, startedAt.UnixMilli())
	}

	// The synthetic id ("t1:0") MUST NOT have produced a separate row.
	// If we see a row under the fallback id alongside the wire-id row,
	// resolveTurnID has silently switched paths mid-pipeline.
	if _, synthFound, err := st.GetTurn("t1:0"); err != nil {
		t.Fatalf("get turn by synthetic id: %v", err)
	} else if synthFound {
		t.Errorf("resolveTurnID split the turn into a synthetic sibling row (id t1:0) — wire id %q should be the only row", wireTurnID)
	}

	// provider:turn_started payload carries a per-round uuid. The wire
	// id ("turn-codex-1") survives on the persisted row (asserted
	// above) but does NOT appear on the frontend payload — the
	// frontend uses its own per-round identity for indicator /
	// composer / Stop-button gating, decoupled from whatever the
	// provider names the logical turn.
	started := filterEmissions(*emissions, "provider:turn_started")
	if len(started) != 1 {
		t.Fatalf("expected 1 provider:turn_started emission, got %d", len(started))
	}
	startedPayload, ok := started[0].data.(TurnStartedEvent)
	if !ok {
		t.Fatalf("started payload type = %T, want TurnStartedEvent", started[0].data)
	}
	if startedPayload.TurnID == "" {
		t.Errorf("provider:turn_started TurnID is empty — frontend round id must be allocated")
	}
	if startedPayload.TurnID == wireTurnID {
		t.Errorf("provider:turn_started TurnID = %q, want a per-round uuid distinct from the wire id (frontend round id MUST NOT leak the persisted logical-turn id)", startedPayload.TurnID)
	}
	startedRoundID := startedPayload.TurnID

	// EventTurnComplete with the SAME wire id updates the EXISTING row
	// rather than forking a new synthetic row. This is the round-trip
	// invariant: both ends of a turn share a single row keyed by the
	// provider's identifier.
	completedAt := startedAt.Add(3 * time.Second)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		TurnID:    wireTurnID,
		TurnIndex: 0,
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason:         "end_turn",
			AssistantMessageID: "msg_complete_1",
		},
		Timestamp: completedAt,
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	settled, found, err := st.GetTurn(wireTurnID)
	if err != nil || !found {
		t.Fatalf("get settled turn: found=%v err=%v", found, err)
	}
	if settled.CompletedAt == nil {
		t.Fatal("expected completed_at set after turn complete")
	}
	if *settled.CompletedAt != completedAt.UnixMilli() {
		t.Errorf("completed_at = %d, want %d", *settled.CompletedAt, completedAt.UnixMilli())
	}
	if settled.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", settled.StopReason)
	}
	if settled.AssistantMessageID != "msg_complete_1" {
		t.Errorf("assistant_message_id = %q, want msg_complete_1", settled.AssistantMessageID)
	}
	// Started_at must survive the update unchanged — the complete
	// path must not rewrite the wall clock.
	if settled.StartedAt != startedAt.UnixMilli() {
		t.Errorf("started_at mutated on complete: %d, want %d", settled.StartedAt, startedAt.UnixMilli())
	}

	// The synthetic id still must not appear — the complete event must
	// not spawn a second row via the fallback path.
	if _, synthFound, _ := st.GetTurn("t1:0"); synthFound {
		t.Error("EventTurnComplete with wire id spuriously created a synthetic sibling row (id t1:0)")
	}

	// provider:turn_completed payload carries the SAME per-round uuid
	// that turn_started emitted — the round opened by handleTurnStart
	// closes here. A different value would mean the round bookkeeping
	// (currentRoundByThread / takeOpenRound) is mis-routing rounds.
	completed := filterEmissions(*emissions, "provider:turn_completed")
	if len(completed) != 1 {
		t.Fatalf("expected 1 provider:turn_completed emission, got %d", len(completed))
	}
	completedPayload, ok := completed[0].data.(TurnCompletedEvent)
	if !ok {
		t.Fatalf("completed payload type = %T, want TurnCompletedEvent", completed[0].data)
	}
	if completedPayload.TurnID == wireTurnID {
		t.Errorf("provider:turn_completed TurnID = %q, want a per-round uuid distinct from the wire id (frontend round id MUST NOT leak the persisted logical-turn id)", completedPayload.TurnID)
	}
	if completedPayload.TurnID != startedRoundID {
		t.Errorf("provider:turn_completed TurnID = %q, want matching round id from turn_started %q", completedPayload.TurnID, startedRoundID)
	}
	if completedPayload.AssistantMessageID != "msg_complete_1" {
		t.Errorf("provider:turn_completed AssistantMessageID = %q, want msg_complete_1", completedPayload.AssistantMessageID)
	}
}

// --- Orphan error-result surfacing (pre-init / no-round failures) ---
//
// The incident shape these pin: a Claude session restarted onto an
// unusable --resume-session-at cursor emits a single
// result{error_during_execution} BEFORE system/init and nothing else.
// No round opened, no turns row exists, yet the failure must reach the
// user as a persisted error item (whose provider:item_event upsert is
// what clears the frontend's optimistic pending-send "Working" state).

// TestTurnCompleteErrorNoRoundPersistsErrorItem covers the head
// attribution path: the failing start was provoked by a send whose
// pending marker carries the dispatcher-stamped turn index.
func TestTurnCompleteErrorNoRoundPersistsErrorItem(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.RegisterPendingSend("t1", "user:5", 5)

	if err := router.Handle(provider.ProviderEvent{
		Kind:     provider.EventTurnComplete,
		ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason:   "error",
			ErrorMessage: "No message found with message.uuid of: ea6789cd",
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle orphan error result: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want exactly the orphan error item", len(items))
	}
	if items[0].Kind != "error" || items[0].TurnIndex != 5 {
		t.Fatalf("item kind=%q turn=%d, want error item attributed to pending-send turn 5", items[0].Kind, items[0].TurnIndex)
	}
	if !strings.Contains(items[0].Summary, "No message found") {
		t.Fatalf("summary = %q, want the wire error message", items[0].Summary)
	}

	// The item upsert is the chosen frontend surface; no round existed,
	// so no provider:turn_completed may fire.
	if completes := filterEmissions(*emissions, "provider:turn_completed"); len(completes) != 0 {
		t.Fatalf("provider:turn_completed fired %d times for a roundless error result", len(completes))
	}
	upserts := filterEmissions(*emissions, "provider:item_event")
	if len(upserts) == 0 {
		t.Fatalf("no provider:item_event emitted for the orphan error item")
	}

	// The pending head is not consumed — CleanupThread or the wire user
	// echo owns the pop.
	if !router.HasPendingSendForThread("t1") {
		t.Fatalf("pending send was consumed by the orphan error branch")
	}
}

// TestTurnCompleteErrorNoRoundFallsBackToLastTurnIndex covers the
// attribution fallback when no pending send exists (e.g. the failing
// start was a manual Reconnect, not a send).
func TestTurnCompleteErrorNoRoundFallsBackToLastTurnIndex(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	seedUserTextRow(t, st, "t1", 2, "hi", "")

	if err := router.Handle(provider.ProviderEvent{
		Kind:     provider.EventTurnComplete,
		ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason:   "error",
			ErrorMessage: "spawn failed hard",
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle orphan error result: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var errorItem *store.Item
	for i := range items {
		if items[i].Kind == "error" {
			errorItem = &items[i]
		}
	}
	if errorItem == nil {
		t.Fatalf("no error item persisted; items = %+v", items)
	}
	if errorItem.TurnIndex != 2 {
		t.Fatalf("error item turn = %d, want LastTurnIndex fallback 2", errorItem.TurnIndex)
	}
}

// TestTurnCompleteErrorNoRoundSkipsQueueFlush: a session that just
// reported a hard failure cannot serve queued sends — the boundary
// flush must not dispatch into it.
func TestTurnCompleteErrorNoRoundSkipsQueueFlush(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "queued message"))

	if err := router.Handle(provider.ProviderEvent{
		Kind:     provider.EventTurnComplete,
		ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason:   "error",
			ErrorMessage: "No message found with message.uuid of: ea6789cd",
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle orphan error result: %v", err)
	}

	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("flush dispatcher fired %d times at a dead-session boundary", len(calls))
	}
	if !router.HasQueuedFlushItems("t1") {
		t.Fatalf("queued item vanished; it must stay for the next session")
	}
}

// TestLateErrorResultOnSettledTurnFoldsWithoutErrorItem pins the
// discriminator between the orphan branch and the legitimate late-fold
// path: a trailing result{is_error} arriving AFTER a soft close
// settled the turn must fold its error onto the turns row via
// persistLateTurnPayload — not mint an orphan error item.
func TestLateErrorResultOnSettledTurnFoldsWithoutErrorItem(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	// Soft close settles the logical turn (claims settlement, clears
	// the open turn and round).
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("soft close: %v", err)
	}

	// Trailing wire result carrying the error.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason:   "error",
			ErrorMessage: "request failed after retries",
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("trailing error result: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, it := range items {
		if it.Kind == "error" {
			t.Fatalf("late error result on a settled turn minted an error item: %+v", it)
		}
	}
	turn, found, err := st.GetTurnByThreadIndex("t1", 1)
	if err != nil || !found {
		t.Fatalf("turns row lookup: found=%v err=%v", found, err)
	}
	if turn.StopReason != "error" || !strings.Contains(turn.ErrorMessage, "request failed") {
		t.Fatalf("turns row stop=%q error=%q, want late error fold", turn.StopReason, turn.ErrorMessage)
	}
}

// TestMarkThreadActiveResetsSettlementLedger pins the repair-restart
// staleness hole: the context-repair restart skips CleanupThread, so
// without MarkThreadActive clearing settledTurns, a replacement session
// dying pre-init would have its orphan error result misrouted into
// persistLateTurnPayload by the PRIOR session's settlement marker —
// folding the error onto a finished turn's row instead of persisting
// the error item the user needs to see.
func TestMarkThreadActiveResetsSettlementLedger(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Prior session: turn 2 runs and settles normally.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 2,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("settle turn 2: %v", err)
	}

	// Repair restart: the host re-activates the thread (no CleanupThread
	// on this path) and dispatches the next send, which registers its
	// deferred pending marker for turn 3.
	router.MarkThreadActive("t1")
	router.RegisterPendingSend("t1", "user:3", 3)

	// The replacement session dies pre-init with only an error result.
	if err := router.Handle(provider.ProviderEvent{
		Kind:     provider.EventTurnComplete,
		ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason:   "error",
			ErrorMessage: "No message found with message.uuid of: ea6789cd",
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle orphan error result: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var errorItem *store.Item
	for i := range items {
		if items[i].Kind == "error" {
			errorItem = &items[i]
		}
	}
	if errorItem == nil {
		t.Fatalf("orphan error result persisted no error item; the stale settlement ledger swallowed it")
	}
	if errorItem.TurnIndex != 3 {
		t.Fatalf("error item turn = %d, want pending-send attribution 3", errorItem.TurnIndex)
	}

	// The settled prior turn must be untouched — no late-fold mutation.
	turn, found, err := st.GetTurnByThreadIndex("t1", 2)
	if err != nil || !found {
		t.Fatalf("turns row lookup: found=%v err=%v", found, err)
	}
	if turn.StopReason != "end_turn" || turn.ErrorMessage != "" {
		t.Fatalf("turn 2 row stop=%q error=%q, want the original settle untouched", turn.StopReason, turn.ErrorMessage)
	}
}

// TestTurnCompleteErrorWithOpenRoundUnchanged: when a round IS open,
// the error result takes exactly the pre-existing path — round emit
// with the error payload, turns-row settle — and the orphan branch
// stays out of the way (no extra error item).
func TestTurnCompleteErrorWithOpenRoundUnchanged(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason:   "error",
			ErrorMessage: "mid-turn API failure",
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("error turn complete: %v", err)
	}

	completes := filterEmissions(*emissions, "provider:turn_completed")
	if len(completes) != 1 {
		t.Fatalf("provider:turn_completed = %d emissions, want 1", len(completes))
	}
	payload, ok := completes[0].data.(TurnCompletedEvent)
	if !ok {
		t.Fatalf("payload type %T", completes[0].data)
	}
	if payload.ErrorMessage == "" {
		t.Fatalf("round-completed payload lost the error message")
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, it := range items {
		if it.Kind == "error" {
			t.Fatalf("open-round error result minted an orphan error item: %+v", it)
		}
	}
}
