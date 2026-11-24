package acp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

type testChildAndCompactionFlavor struct {
	DefaultFlavor
	mu          sync.Mutex
	launchByCID map[string]string
	compactions map[int]*CompactionEvent
}

func (f *testChildAndCompactionFlavor) ResolveSubagentLaunch(_ context.Context, _, childID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.launchByCID[childID]
	return id, ok
}

func (f *testChildAndCompactionFlavor) DetectCompaction(_ context.Context, _ string, afterIdx int) (int, *CompactionEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bestIdx := afterIdx
	var best *CompactionEvent
	for idx, ev := range f.compactions {
		if idx > bestIdx {
			bestIdx = idx
			best = ev
		}
	}
	return bestIdx, best
}

func newUnitSession(flavor Flavor, collect *[]provider.ProviderEvent, mu *sync.Mutex) *Session {
	if flavor == nil {
		flavor = DefaultFlavor{Kind: provider.Agy}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Session{
		threadID:             "t-unit",
		sessionID:            "11111111-1111-4111-8111-111111111111",
		flavor:               flavor,
		startedTools:         make(map[string]NormalizedToolStart),
		childStartedTools:    make(map[string]NormalizedToolStart),
		childLaunchByConvID:  make(map[string]string),
		trackedSubagents:     make(map[string]SubagentLaunchContext),
		pendingPerms:         make(map[string]pendingPermission),
		pendingToolQuestions: make(map[string]pendingToolQuestion),
		lastCompactionIdx:    -1,
		sessionCtx:           ctx,
		sessionCancel:        cancel,
		readDone:             make(chan struct{}),
		onEvent: func(ev provider.ProviderEvent) {
			mu.Lock()
			*collect = append(*collect, ev)
			mu.Unlock()
		},
	}
}

func TestACPContentBlockStopSeparatesConsecutiveModelTurns(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(nil, &events, &mu)
	s.turnOpen = true
	s.activeTurnID = "turn-1"

	sendChunk := func(kind, text string) {
		raw, _ := json.Marshal(ContentBlock{Type: "text", Text: text})
		s.handleUpdate(SessionUpdate{SessionUpdate: kind, Content: raw})
	}

	sendChunk("agent_thought_chunk", "Thinking 1")
	sendChunk("agent_message_chunk", "Reply 1")
	s.handleUpdate(SessionUpdate{SessionUpdate: "usage_update", Used: 1000, Size: 100000})
	sendChunk("agent_thought_chunk", "Thinking 2")
	sendChunk("agent_message_chunk", "Reply 2")
	s.handleUpdate(SessionUpdate{SessionUpdate: "usage_update", Used: 2000, Size: 100000})

	mu.Lock()
	defer mu.Unlock()
	var stops []string
	for _, ev := range events {
		if ev.Kind == provider.EventContentBlockStop {
			var m struct {
				BlockType string `json:"blockType"`
			}
			_ = json.Unmarshal(ev.Meta, &m)
			stops = append(stops, m.BlockType)
		}
	}
	want := []string{"thinking", "text", "thinking", "text"}
	if strings.Join(stops, ",") != strings.Join(want, ",") {
		t.Fatalf("EventContentBlockStop sequence = %v, want %v", stops, want)
	}
}

func TestACPChildToolCallRoutingAndDrop(t *testing.T) {
	t.Parallel()
	knownChild := "8d1d87ec-d2f0-472e-8626-2c83eefc2bc1"
	unknownChild := "989ea20e-190d-4886-b619-52dcb3fcd838"

	flavor := &testChildAndCompactionFlavor{
		DefaultFlavor: DefaultFlavor{Kind: provider.Agy},
		launchByCID:   map[string]string{knownChild: "call_launch_1"},
	}
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(flavor, &events, &mu)
	s.turnOpen = true
	s.activeTurnID = "turn-1"

	// Open parent assistant text block; child tool call must NOT split it.
	rawText, _ := json.Marshal(ContentBlock{Type: "text", Text: "Parent streaming..."})
	s.handleUpdate(SessionUpdate{SessionUpdate: "agent_message_chunk", Content: rawText})

	// Known child tool call must route under ParentToolUseID="call_launch_1".
	s.handleUpdate(SessionUpdate{
		SessionUpdate: "tool_call",
		ToolCallID:    knownChild + ":call_bash_1",
		Name:          "run_command",
		Kind:          "execute",
		Status:        "in_progress",
	})
	// Unknown foreign child tool call must be DROPPED (never leak to main timeline).
	s.handleUpdate(SessionUpdate{
		SessionUpdate: "tool_call",
		ToolCallID:    unknownChild + ":call_bash_2",
		Name:          "run_command",
		Kind:          "execute",
		Status:        "in_progress",
	})

	mu.Lock()
	defer mu.Unlock()
	var sawNested, sawLeaked, sawBlockStop bool
	for _, ev := range events {
		if ev.Kind == provider.EventContentBlockStop {
			sawBlockStop = true
		}
		if ev.Kind == provider.EventToolStart {
			if ev.ItemID == knownChild+":call_bash_1" {
				if ev.ParentToolUseID != "call_launch_1" {
					t.Fatalf("known child ParentToolUseID = %q, want call_launch_1", ev.ParentToolUseID)
				}
				sawNested = true
			}
			if ev.ItemID == unknownChild+":call_bash_2" {
				sawLeaked = true
			}
		}
	}
	if !sawNested {
		t.Fatal("expected known child tool call to be routed under call_launch_1")
	}
	if sawLeaked {
		t.Fatal("unknown foreign child tool call leaked to timeline")
	}
	if sawBlockStop {
		t.Fatal("child tool call must not close parent's active assistant text block")
	}
}

func TestACPCompactionEmitsBoundaryBeforeTokenUsageOnContextDrop(t *testing.T) {
	t.Parallel()
	flavor := &testChildAndCompactionFlavor{
		DefaultFlavor: DefaultFlavor{Kind: provider.Agy},
		compactions: map[int]*CompactionEvent{
			6286: {ItemID: "agy-compact-6286"},
		},
	}
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(flavor, &events, &mu)
	s.turnOpen = true
	s.activeTurnID = "turn-1"

	s.handleUpdate(SessionUpdate{SessionUpdate: "usage_update", Used: 500000, Size: 1048576})
	s.handleUpdate(SessionUpdate{SessionUpdate: "usage_update", Used: 49000, Size: 1048576})
	// Second usage_update at same checkpoint index must not double-emit.
	s.handleUpdate(SessionUpdate{SessionUpdate: "usage_update", Used: 45000, Size: 1048576})

	mu.Lock()
	defer mu.Unlock()
	var boundaries []provider.ProviderEvent
	boundaryPos, postUsagePos := -1, -1
	for i, ev := range events {
		if ev.Kind == provider.EventCompactBoundary {
			boundaries = append(boundaries, ev)
			boundaryPos = i
		}
		if ev.Kind == provider.EventTokenUsage && boundaryPos >= 0 && postUsagePos < 0 {
			postUsagePos = i
		}
	}
	if len(boundaries) != 1 {
		t.Fatalf("EventCompactBoundary count = %d, want 1", len(boundaries))
	}
	if boundaries[0].ItemID != "agy-compact-6286" {
		t.Fatalf("boundary ItemID = %q, want agy-compact-6286", boundaries[0].ItemID)
	}
	if postUsagePos <= boundaryPos {
		t.Fatalf("expected EventCompactBoundary (%d) before post-compaction EventTokenUsage (%d)", boundaryPos, postUsagePos)
	}
}

func TestACPSteerReturnsErrNoActiveTurnWhenIdle(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(nil, &events, &mu)
	err := s.Steer(context.Background(), "follow up", provider.SendOptions{})
	if !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("Steer when idle err = %v, want ErrNoActiveTurn", err)
	}
}

func TestACPFinishPromptNeverConfirmsCancelledOrClosing(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(nil, &events, &mu)
	s.turnOpen = true
	s.activeTurnID = "turn-1"
	s.inFlightPrompts = 1

	confirm := &pendingPromptConfirm{uuid: "uuid-cancel-1", content: "queued text"}
	s.finishPrompt(json.RawMessage(`{"stopReason":"cancelled"}`), nil, nil, confirm)

	mu.Lock()
	defer mu.Unlock()
	for _, ev := range events {
		if ev.Kind == provider.EventUserText {
			t.Fatalf("cancelled prompt must not emit EventUserText confirmation: %+v", ev)
		}
	}
}

func TestACPFinishPromptAsyncRPCErrorIncludesFailedProviderItemID(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(nil, &events, &mu)
	s.turnOpen = true
	s.activeTurnID = "turn-1"
	s.inFlightPrompts = 1

	confirm := &pendingPromptConfirm{uuid: "uuid-err-1", content: "bad prompt"}
	s.finishPrompt(nil, &RPCError{Method: "session/prompt", Code: ErrCodeInvalidParams, Message: "rejected"}, nil, confirm)

	mu.Lock()
	defer mu.Unlock()
	for _, ev := range events {
		if ev.Kind == provider.EventError {
			var m struct {
				FailedProviderItemID string `json:"failed_provider_item_id"`
			}
			if json.Unmarshal(ev.Meta, &m) == nil && m.FailedProviderItemID == "uuid-err-1" {
				return
			}
			t.Fatalf("EventError missing failed_provider_item_id, meta=%s", string(ev.Meta))
		}
	}
	t.Fatal("expected EventError on async RPC prompt failure")
}

func TestACPSpontaneousWakeDoesNotCloseWhileToolActive(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(nil, &events, &mu)
	s.turnOpen = true
	s.spontaneousTurnOpen = true
	s.activeTurnID = "wake-1"
	s.startedTools["call_1"] = NormalizedToolStart{ToolName: "Bash", ItemType: "command_execution"}

	s.handleUpdate(SessionUpdate{SessionUpdate: "usage_update", Used: 1000, Size: 100000})

	mu.Lock()
	for _, ev := range events {
		if ev.Kind == provider.EventTurnComplete {
			mu.Unlock()
			t.Fatal("spontaneous wake closed prematurely while parent tool was still running")
		}
	}
	mu.Unlock()
	if !s.spontaneousTurnOpen {
		t.Fatal("spontaneousTurnOpen should remain true while tool is active")
	}
}

func TestACPAutoAcceptEditsPolicy(t *testing.T) {
	t.Parallel()
	f := DefaultFlavor{Kind: provider.Agy}
	_, policy := f.MapRuntimeMode(provider.RuntimeAutoAcceptEdits, provider.ModeChat, []SessionMode{{ID: "default"}})
	if policy != PolicyAutoAcceptEdits {
		t.Fatalf("policy = %v, want PolicyAutoAcceptEdits", policy)
	}
	opts := []PermissionOption{
		{OptionID: "allow", Name: "Allow", Kind: "allow_once"},
		{OptionID: "deny", Name: "Deny", Kind: "reject_once"},
	}
	editCls := f.ClassifyPermission("t1", "turn-1", "req-1", RequestPermissionParams{
		ToolCall: ToolCallUpdate{ToolCallID: "c1", Name: "edit_file", Kind: "edit"},
		Options:  opts,
	}, policy)
	if editCls.Action != PermissionAutoAllow || editCls.OptionID != "allow" {
		t.Fatalf("edit classification = %+v, want auto-allow", editCls)
	}
	execCls := f.ClassifyPermission("t1", "turn-1", "req-2", RequestPermissionParams{
		ToolCall: ToolCallUpdate{ToolCallID: "c2", Name: "run_command", Kind: "execute"},
		Options:  opts,
	}, policy)
	if execCls.Action != PermissionAskUser {
		t.Fatalf("execute classification = %+v, want PermissionAskUser", execCls)
	}
}

func TestACPReplayLeavesZeroTimestamps(t *testing.T) {
	t.Parallel()
	rawText, _ := json.Marshal(ContentBlock{Type: "text", Text: "hello"})
	events := ReplayUpdatesToEvents([]SessionUpdate{
		{SessionUpdate: "user_message_chunk", Content: rawText},
		{SessionUpdate: "agent_message_chunk", Content: rawText},
	}, nil)
	if len(events) == 0 {
		t.Fatal("expected replayed events")
	}
	for i, ev := range events {
		if !ev.Timestamp.IsZero() {
			t.Fatalf("event[%d] (%s) stamped non-zero timestamp %v; replay must leave timestamps zero for sessionimport", i, ev.Kind, ev.Timestamp)
		}
	}
}

func TestExtractStructuredJSONObjectWithProseBraces(t *testing.T) {
	t.Parallel()
	got := extractStructuredJSONObject(`Use {foo} syntax when calling it. Here is the result: {"ok":true,"count":3}`)
	if string(got) != `{"ok":true,"count":3}` {
		t.Fatalf("extractStructuredJSONObject = %q, want {\"ok\":true,\"count\":3}", string(got))
	}
}

func TestACPOnResponseMultiSteerArmsNextConfirmAndSkipsCancelled(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(nil, &events, &mu)
	s.turnOpen = true
	s.activeTurnID = "turn-1"

	c0 := &pendingPromptConfirm{uuid: "u0", content: "first"}
	c1 := &pendingPromptConfirm{uuid: "u1", content: "second"}
	c2 := &pendingPromptConfirm{uuid: "u2", content: "third"}
	s.wireInFlightPrompts = 3
	s.inFlightConfirms = []*pendingPromptConfirm{c0, c1, c2}

	settle0 := s.makeWirePromptSettler(c0)
	settle1 := s.makeWirePromptSettler(c1)
	settle2 := s.makeWirePromptSettler(c2)

	// Predecessor c0 completes with end_turn while 2 steered prompts remain -> arms c1!
	settle0([]byte(`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`))
	if s.armedConfirm != c1 {
		t.Fatalf("after c0 end_turn, armedConfirm = %v, want c1", s.armedConfirm)
	}
	s.confirmArmedPrompt()

	// Next c1 is cancelled -> must NOT arm c2!
	settle1([]byte(`{"jsonrpc":"2.0","id":2,"result":{"stopReason":"cancelled"}}`))
	if s.armedConfirm != nil {
		t.Fatalf("after c1 cancelled, armedConfirm = %v, want nil", s.armedConfirm)
	}
	settle2([]byte(`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"cancelled"}}`))

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0].Kind != provider.EventUserText || events[0].Content != "second" {
		t.Fatalf("unexpected confirmation events: %+v", events)
	}
	_ = time.Now()
}
