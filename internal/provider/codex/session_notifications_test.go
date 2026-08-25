package codex

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

func TestDispatchLineChildNotificationSetsParentToolUseID(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"child-provider-1","turnId":"turn-child-1","itemId":"msg-1","delta":"working"}}`))

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].ParentToolUseID != "call-collab-1" {
		t.Fatalf("ParentToolUseID: got %q, want %q", events[0].ParentToolUseID, "call-collab-1")
	}
	if events[0].ThreadID != "parent-thread" {
		t.Fatalf("ThreadID: got %q, want %q", events[0].ThreadID, "parent-thread")
	}
}

func TestDispatchLineSuppressesChildTurnLifecycle(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"child-provider-1","turn":{"id":"turn-child-1"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"child-provider-1","turn":{"id":"turn-child-1","status":"completed"}}}`))

	if len(events) != 2 {
		t.Fatalf("expected running and terminal child status events, got %+v", events)
	}
	for _, event := range events {
		if event.Kind != provider.EventSubagentStatus || event.ItemID != "call-collab-1" {
			t.Fatalf("unexpected child lifecycle event: %+v", event)
		}
	}
	var meta map[string]string
	if err := json.Unmarshal(events[1].Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["agent_path"] != "child-provider-1" || meta["status"] != "completed" {
		t.Fatalf("meta = %+v, want child-provider-1 completed", meta)
	}
}

// TestDispatchLineChildTokenUsageBecomesScopedProgress replaces the old
// TestDispatchLineSuppressesChildTokenUsage. A child's
// `thread/tokenUsage/updated` is still forbidden from reaching the
// PARENT's meter (ADR-002: Codex subagents flatten onto the parent, so
// the child's window would overwrite it) — but dropping it entirely also
// threw away the only live signal Codex gives for a running child. It is
// now re-emitted as a scoped EventSubagentProgress naming the spawn
// tool_use (docs/specs/agent-visibility.md).
func TestDispatchLineChildTokenUsageBecomesScopedProgress(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:  "parent-thread",
		pending:   make(map[int64]chan json.RawMessage),
		usageAcct: newUsageAccounting(false),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		},
	}

	// Real-shaped breakdown: a child on its third round, most of whose
	// input is a cached re-read. `total.totalTokens` (147000) is the
	// number the card used to show and the one this test now refuses.
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"child-provider-1","tokenUsage":{` +
		`"last":{"totalTokens":50000,"inputTokens":49000,"cachedInputTokens":46000,"cacheWriteInputTokens":500,"outputTokens":1000,"reasoningOutputTokens":400},` +
		`"total":{"totalTokens":147000,"inputTokens":144000,"cachedInputTokens":138000,"cacheWriteInputTokens":500,"outputTokens":3000,"reasoningOutputTokens":1200},` +
		`"modelContextWindow":200000}}}`))

	if len(events) != 1 {
		t.Fatalf("events = %+v, want exactly one scoped progress tick", events)
	}
	event := events[0]
	if event.Kind != provider.EventSubagentProgress {
		t.Fatalf("kind = %q, want %q", event.Kind, provider.EventSubagentProgress)
	}
	if event.ThreadID != "parent-thread" {
		t.Fatalf("threadID = %q, want the AO parent thread", event.ThreadID)
	}
	if event.ItemID != "call-collab-1" {
		t.Fatalf("itemID = %q, want the spawn tool_use", event.ItemID)
	}
	if event.ParentToolUseID != "" {
		t.Fatalf("parentToolUseID = %q, want empty for a depth-1 child", event.ParentToolUseID)
	}
	var progress provider.SubagentProgressMeta
	if err := json.Unmarshal(event.Meta, &progress); err != nil {
		t.Fatalf("decode progress meta: %v", err)
	}
	// The child's cumulative spend, counted once: fresh input
	// (144000 - 138000) + cache writes (500) + all output ever (3000) =
	// 9500. Not `total.totalTokens` (147000), which re-counts the cached
	// prompt every round; not `last.totalTokens` (50000), which is a
	// context size and drops the earlier rounds' output.
	if progress.TaskID != "child-provider-1" || progress.TotalTokens != 9500 {
		t.Fatalf("progress = %+v, want the child thread id and its cumulative spend", progress)
	}
	if progress.ToolUses != 0 || progress.DurationMs != 0 {
		t.Fatalf("progress = %+v, Codex reports neither tool count nor elapsed", progress)
	}

	// The parent's own per-turn accounting never saw it.
	if s.usageAcct.latestSet {
		t.Fatalf("child usage reached the parent meter: %+v", s.usageAcct)
	}
}

// TestDispatchLineChildTokenUsageWithoutTotalEmitsNothing pins that a
// frame carrying no cumulative total is silence rather than a zeroing
// tick — the consumer merges ticks, so a zero would be indistinguishable
// from "this agent has spent nothing" if it were ever applied.
func TestDispatchLineChildTokenUsageWithoutTotalEmitsNothing(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:  "parent-thread",
		pending:   make(map[int64]chan json.RawMessage),
		usageAcct: newUsageAccounting(false),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"child-provider-1","tokenUsage":{"last":{"totalTokens":50000},"modelContextWindow":200000}}}`))

	if len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
	if s.usageAcct.latestSet {
		t.Fatalf("child usage reached the parent meter: %+v", s.usageAcct)
	}
}

// TestDispatchLineNestedChildTokenUsageCarriesTheSpawnsOwnParent pins the
// depth-2 case: the tick names the nested spawn AND the spawn that owns
// it, resolved from the canonical agent path, so a consumer can nest the
// card without a store lookup.
func TestDispatchLineNestedChildTokenUsageCarriesTheSpawnsOwnParent(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:  "parent-thread",
		pending:   make(map[int64]chan json.RawMessage),
		usageAcct: newUsageAccounting(false),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{
				"child-1":      "spawn-reviewer",
				"grandchild-1": "spawn-deep",
			},
			agentPathByThread: map[string]string{
				"child-1":      "/root/reviewer",
				"grandchild-1": "/root/reviewer/deep",
			},
			childParentByAgentPath: map[string]string{
				"/root/reviewer":      "spawn-reviewer",
				"/root/reviewer/deep": "spawn-deep",
			},
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"grandchild-1","tokenUsage":{"last":{"inputTokens":1100,"outputTokens":40},"total":{"totalTokens":1200,"inputTokens":1100,"outputTokens":100},"modelContextWindow":200000}}}`))

	if len(events) != 1 {
		t.Fatalf("events = %+v, want one progress tick", events)
	}
	if events[0].ItemID != "spawn-deep" || events[0].ParentToolUseID != "spawn-reviewer" {
		t.Fatalf("scope = item %q parent %q, want spawn-deep under spawn-reviewer", events[0].ItemID, events[0].ParentToolUseID)
	}
}

// TestDispatchLineParentTokenUsageStillMetersTheParent is the control:
// unsuppressing the CHILD channel must not change what a parent-thread
// notification does.
func TestDispatchLineParentTokenUsageStillMetersTheParent(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:  "parent-thread",
		pending:   make(map[int64]chan json.RawMessage),
		usageAcct: newUsageAccounting(false),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		},
	}
	s.setRootThreadID("provider-parent")

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"provider-parent","tokenUsage":{"last":{"totalTokens":8400},"total":{"totalTokens":11839},"modelContextWindow":200000}}}`))

	if len(events) != 1 || events[0].Kind != provider.EventTokenUsage {
		t.Fatalf("events = %+v, want one EventTokenUsage", events)
	}
	if !s.usageAcct.latestSet || s.usageAcct.latest.TotalTokens != 11839 {
		t.Fatalf("parent accounting = %+v, want the cumulative total observed", s.usageAcct)
	}
}

// TestDispatchLineSuppressesChildCompacted ensures child-thread compaction
// notifications do not pollute parent state.
func TestDispatchLineSuppressesChildCompacted(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/compacted","params":{"threadId":"child-provider-1"}}`))

	if len(events) != 0 {
		t.Fatalf("expected no events for child compaction, got %+v", events)
	}
}

// TestDispatchLineSuppressesChildContextCompactionItem ensures the newer
// `contextCompaction` item lifecycle cannot leak a child compaction divider
// onto the parent thread. Older Codex builds emitted `thread/compacted`;
// current builds emit `item/completed` with item.type = contextCompaction.
func TestDispatchLineSuppressesChildContextCompactionItem(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"child-provider-1","turnId":"child-turn-1","item":{"type":"contextCompaction","id":"compact-child-1"},"completedAtMs":1781709441420}}`))

	if len(events) != 0 {
		t.Fatalf("expected no events for child context compaction item, got %+v", events)
	}
}

func TestDispatchLineEmitsParentContextCompactionItem(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		},
	}
	s.setRootThreadID("provider-parent")

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"provider-parent","turnId":"parent-turn-1","item":{"type":"contextCompaction","id":"compact-parent-1"},"completedAtMs":1781709692716}}`))

	if len(events) != 1 {
		t.Fatalf("expected 1 parent compaction event, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventCompactBoundary {
		t.Fatalf("event kind = %q, want %q", events[0].Kind, provider.EventCompactBoundary)
	}
	if events[0].ItemID != "compact-parent-1" {
		t.Fatalf("item id = %q, want compact-parent-1", events[0].ItemID)
	}
	if events[0].ThreadID != "parent-thread" {
		t.Fatalf("thread id = %q, want parent-thread", events[0].ThreadID)
	}
}

// TestDispatchLineSuppressesChildNameUpdated ensures child-thread name updates
// don't rename the parent thread.
func TestDispatchLineSuppressesChildNameUpdated(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/name/updated","params":{"threadId":"child-provider-1","threadName":"Subagent Title"}}`))

	if len(events) != 0 {
		t.Fatalf("expected no events for child name update, got %+v", events)
	}
}

// TestDispatchLineParentTokenUsageStillEmits is the positive control for
// the suppression filter: parent-thread token usage must continue to flow.
func TestDispatchLineParentTokenUsageStillEmits(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"provider-parent","tokenUsage":{"last":{"totalTokens":80000},"modelContextWindow":200000}}}`))

	if len(events) != 1 {
		t.Fatalf("expected one parent token usage event, got %+v", events)
	}
	if events[0].Kind != provider.EventTokenUsage {
		t.Fatalf("kind = %q, want %q", events[0].Kind, provider.EventTokenUsage)
	}
	if events[0].ParentToolUseID != "" {
		t.Fatalf("parent token usage should not carry ParentToolUseID, got %q", events[0].ParentToolUseID)
	}
}
