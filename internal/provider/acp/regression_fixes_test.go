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
	if len(s.armedConfirms) != 1 || s.armedConfirms[0] != c1 {
		t.Fatalf("after c0 end_turn, armedConfirms = %v, want [c1]", s.armedConfirms)
	}
	s.confirmArmedPrompt()

	// Next c1 is cancelled -> must NOT arm c2!
	settle1([]byte(`{"jsonrpc":"2.0","id":2,"result":{"stopReason":"cancelled"}}`))
	if len(s.armedConfirms) != 0 {
		t.Fatalf("after c1 cancelled, armedConfirms = %v, want empty", s.armedConfirms)
	}
	settle2([]byte(`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"cancelled"}}`))

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0].Kind != provider.EventUserText || events[0].Content != "second" {
		t.Fatalf("unexpected confirmation events: %+v", events)
	}
	_ = time.Now()
}

type slowAfterPromptFlavor struct {
	DefaultFlavor
	delay    time.Duration
	override *PromptOutcomeOverride
}

func (f *slowAfterPromptFlavor) AfterPrompt(_ context.Context, _, _ string, _ any) *PromptOutcomeOverride {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.override
}

func TestACPInterruptWaitsForAfterPromptAndTurnComplete(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []provider.ProviderEvent
	fl := &slowAfterPromptFlavor{
		DefaultFlavor: DefaultFlavor{Kind: provider.Agy},
		delay:         60 * time.Millisecond,
	}
	s := newUnitSession(fl, &events, &mu)
	s.sessionID = "sess-1"
	s.turnOpen = true
	s.activeTurnID = "turn-1"
	s.inFlightPrompts = 1
	s.activeFinishers = 1
	s.cancelling = true
	s.inFlightZeroCh = make(chan struct{})
	waitCh := s.inFlightZeroCh

	go func() {
		s.finishPrompt(json.RawMessage(`{"stopReason":"cancelled"}`), nil, nil, nil)
	}()

	select {
	case <-waitCh:
	case <-time.After(2 * time.Second):
		t.Fatal("inFlightZeroCh did not close after finishPrompt")
	}

	mu.Lock()
	defer mu.Unlock()
	var sawComplete bool
	for _, ev := range events {
		if ev.Kind == provider.EventTurnComplete {
			sawComplete = true
		}
	}
	if !sawComplete {
		t.Fatalf("inFlightZeroCh closed BEFORE EventTurnComplete was emitted; events=%+v", events)
	}
}

func TestACPSteerFlushesPromptOneBeforeConfirmingPromptTwo(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(nil, &events, &mu)
	s.turnOpen = true
	s.activeTurnID = "turn-1"

	c0 := &pendingPromptConfirm{uuid: "u0", content: "first"}
	c0.confirmed.Store(true)
	c1 := &pendingPromptConfirm{uuid: "u1", content: "second"}
	s.wireInFlightPrompts = 2
	s.inFlightConfirms = []*pendingPromptConfirm{c0, c1}

	settle0 := s.makeWirePromptSettler(c0)
	// Buffer a chunk for Prompt #1 within the 35ms coalesce window.
	s.bufferChunk(provider.EventTextDelta, "tail of prompt 1")
	// Prompt #1 resolves on wire -> must flush & close block BEFORE arming c1.
	settle0([]byte(`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`))
	// First chunk of Prompt #2 confirms c1 and buffers Prompt #2 text.
	s.confirmArmedPrompt()
	s.bufferChunk(provider.EventTextDelta, "reply to prompt 2")
	s.closeActiveContentBlock()

	mu.Lock()
	defer mu.Unlock()
	var idxTail, idxStop, idxUser2, idxReply2 = -1, -1, -1, -1
	for i, ev := range events {
		switch {
		case ev.Kind == provider.EventTextDelta && ev.Content == "tail of prompt 1":
			idxTail = i
		case ev.Kind == provider.EventContentBlockStop && idxStop < 0:
			idxStop = i
		case ev.Kind == provider.EventUserText && ev.Content == "second":
			idxUser2 = i
		case ev.Kind == provider.EventTextDelta && ev.Content == "reply to prompt 2":
			idxReply2 = i
		}
	}
	if idxTail < 0 || idxStop <= idxTail || idxUser2 <= idxStop || idxReply2 <= idxUser2 {
		t.Fatalf("unexpected ordering tail=%d stop=%d user2=%d reply2=%d events=%+v", idxTail, idxStop, idxUser2, idxReply2, events)
	}
}

func TestACPPermissionCancelEmitsCommandCancelledForUnconfirmedSteer(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(nil, &events, &mu)
	s.sessionID = "sess-1"
	s.turnOpen = true
	s.activeTurnID = "turn-1"
	s.inFlightPrompts = 1
	s.activeFinishers = 1
	s.cancelling = false // Not from Interrupt() — simulates RespondToApproval("cancel").
	confirm := &pendingPromptConfirm{uuid: "uuid-steer-perm-cancel", content: "queued steer"}

	s.finishPrompt(json.RawMessage(`{"stopReason":"cancelled"}`), nil, nil, confirm)

	mu.Lock()
	defer mu.Unlock()
	for _, ev := range events {
		if ev.Kind == provider.EventCommandLifecycle {
			var meta provider.CommandLifecycleMeta
			_ = json.Unmarshal(ev.Meta, &meta)
			if meta.CommandUUID == "uuid-steer-perm-cancel" && meta.State == provider.CommandCancelled {
				return
			}
		}
	}
	t.Fatalf("expected EventCommandLifecycle(cancelled) for unconfirmed steer, got %+v", events)
}

func TestACPSpontaneousWakeClosesWithBackgroundToolOpen(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(nil, &events, &mu)
	s.sessionID = "sess-1"
	s.turnOpen = true
	s.spontaneousTurnOpen = true
	s.activeTurnID = "wake-1"
	s.startedTools["bg-cmd-1"] = NormalizedToolStart{ToolName: "Bash", ItemType: "command_execution", IsBackground: true}

	s.handleUsageUpdate(1200, 100000)

	mu.Lock()
	defer mu.Unlock()
	for _, ev := range events {
		if ev.Kind == provider.EventTurnComplete {
			return
		}
	}
	t.Fatalf("expected spontaneous wake turn to close while only background tool is in startedTools, got %+v", events)
}

func TestACPAPIErrorEmitsAPIErrorEnumAndExpectTurnComplete(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []provider.ProviderEvent
	fl := &slowAfterPromptFlavor{
		DefaultFlavor: DefaultFlavor{Kind: provider.Agy},
		override: &PromptOutcomeOverride{
			ErrorMessage: "RESOURCE_EXHAUSTED: quota exceeded",
			Failure: &provider.FailureMeta{
				Class:    provider.FailureTransient,
				Boundary: provider.FailureBoundaryTurn,
				Reason:   provider.FailureReasonUsageLimit,
				Code:     "rate_limit",
			},
		},
	}
	s := newUnitSession(fl, &events, &mu)
	s.sessionID = "sess-1"
	s.turnOpen = true
	s.activeTurnID = "turn-1"
	s.inFlightPrompts = 1
	s.activeFinishers = 1

	s.finishPrompt(json.RawMessage(`{"stopReason":"end_turn"}`), nil, 1, nil)

	mu.Lock()
	defer mu.Unlock()
	for _, ev := range events {
		if ev.Kind == provider.EventError {
			var m map[string]any
			_ = json.Unmarshal(ev.Meta, &m)
			if m["api_error_enum"] != "rate_limit" || m["expect_turn_complete"] != true {
				t.Fatalf("EventError meta = %v, want api_error_enum=rate_limit and expect_turn_complete=true", m)
			}
			return
		}
	}
	t.Fatalf("expected EventError with api_error_enum, got %+v", events)
}

func TestACPChildInteractionQuestionNotAutoApprovedAndCarriesParentID(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(nil, &events, &mu)
	s.sessionID = "4d5f2b49-97b2-49e7-a214-7aff00575443"
	s.permissionPolicy = PolicyAutoAllowNonQuestion
	childUUID := "8d1d87ec-d2f0-472e-8626-2c83eefc2bc1"
	s.registerChildLaunch(childUUID, "launch-sub-1")

	req := RequestPermissionParams{
		SessionID: s.sessionID,
		ToolCall: ToolCallUpdate{
			ToolCallID: childUUID + ":interaction_42",
			Title:      "Choose an approach",
		},
		Options: []PermissionOption{
			{OptionID: "opt_a", Name: "Option A", Kind: "allow_once"},
			{OptionID: "opt_b", Name: "Option B", Kind: "allow_once"},
		},
	}
	s.handlePermissionRequest(json.RawMessage(`99`), req)

	mu.Lock()
	defer mu.Unlock()
	for _, ev := range events {
		if ev.Kind == provider.EventUserInputRequest {
			if ev.ParentToolUseID != "launch-sub-1" {
				t.Fatalf("ParentToolUseID = %q, want launch-sub-1", ev.ParentToolUseID)
			}
			return
		}
	}
	t.Fatalf("expected EventUserInputRequest (not auto-approved) for child interaction_*, got %+v", events)
}

type compactionStubFlavor struct {
	DefaultFlavor
	checkpointIdx int
}

func (f *compactionStubFlavor) DetectCompaction(_ context.Context, _ string, afterIdx int) (int, *CompactionEvent) {
	if f.checkpointIdx > afterIdx {
		return f.checkpointIdx, &CompactionEvent{ItemID: "agy-compact-stub"}
	}
	return afterIdx, nil
}

func TestACPCompactionEmitsBeforePostCompactionTextOnColdResume(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []provider.ProviderEvent
	fl := &compactionStubFlavor{
		DefaultFlavor: DefaultFlavor{Kind: provider.Agy},
		checkpointIdx: 12,
	}
	s := newUnitSession(fl, &events, &mu)
	s.sessionID = "sess-cold"
	s.turnOpen = true
	s.activeTurnID = "turn-1"
	s.lastUsedTokens = 0 // cold resume
	s.compactionCheckPending = true

	s.handleUpdate(SessionUpdate{
		SessionUpdate: "agent_message_chunk",
		Content:       json.RawMessage(`{"type":"text","text":"After compaction"}`),
	})
	s.closeActiveContentBlock()

	mu.Lock()
	defer mu.Unlock()
	idxCompact, idxText := -1, -1
	for i, ev := range events {
		if ev.Kind == provider.EventCompactBoundary && idxCompact < 0 {
			idxCompact = i
		}
		if ev.Kind == provider.EventTextDelta && ev.Content == "After compaction" && idxText < 0 {
			idxText = i
		}
	}
	if idxCompact < 0 || idxText < 0 || idxCompact > idxText {
		t.Fatalf("expected EventCompactBoundary before post-compaction text; compact=%d text=%d events=%+v", idxCompact, idxText, events)
	}
}

func TestACPReplayEmitsContentBlockStop(t *testing.T) {
	t.Parallel()
	updates := []SessionUpdate{
		{SessionUpdate: "agent_thought_chunk", Content: json.RawMessage(`{"type":"text","text":"think"}`)},
		{SessionUpdate: "agent_message_chunk", Content: json.RawMessage(`{"type":"text","text":"answer"}`)},
		{SessionUpdate: "usage_update", Used: 100, Size: 1000},
	}
	events := ReplayUpdatesToEvents(updates, DefaultFlavor{Kind: provider.Agy})
	var stops int
	for _, ev := range events {
		if ev.Kind == provider.EventContentBlockStop {
			stops++
		}
	}
	if stops != 2 {
		t.Fatalf("expected 2 EventContentBlockStop events in replay (thinking + text), got %d: %+v", stops, events)
	}
}

type bgCommandStubFlavor struct {
	DefaultFlavor
	resolved map[string]string
}

func (f *bgCommandStubFlavor) NormalizeToolStart(u *ToolCallUpdate) NormalizedToolStart {
	if u.Name == "invoke_subagent" {
		return NormalizedToolStart{ToolName: "Agent", ItemType: "Agent", SubagentLaunch: true, SubagentType: "research"}
	}
	return NormalizedToolStart{ToolName: "Bash", ItemType: "command_execution"}
}

func (f *bgCommandStubFlavor) NormalizeToolComplete(u *ToolCallUpdate, started NormalizedToolStart) NormalizedToolComplete {
	if u.Status == "in_progress" {
		return NormalizedToolComplete{
			ToolName:         "Bash",
			ItemType:         "command_execution",
			IsBackground:     true,
			BackgroundTaskID: "sess/task-bg-1",
			OutputFile:       "/tmp/task-bg-1.log",
			CommandOut:       "Tool is running as a background task",
		}
	}
	zero := 0
	return NormalizedToolComplete{
		ToolName:   "Bash",
		ItemType:   "command_execution",
		ItemStatus: "completed",
		ExitCode:   &zero,
		CommandOut: "all tests passed",
	}
}

func (f *bgCommandStubFlavor) CanTrackSubagent(sessionID string) bool { return sessionID != "" }

func (f *bgCommandStubFlavor) ResolveSubagentLaunch(_ context.Context, sessionID, childID string) (string, bool) {
	if id, ok := f.resolved[sessionID+":"+childID]; ok {
		return id, true
	}
	return "", false
}

func TestACPBackgroundCommandEmitsTerminalSiblingInsteadOfInPlaceComplete(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(&bgCommandStubFlavor{DefaultFlavor: DefaultFlavor{Kind: provider.Agy}}, &events, &mu)
	s.sessionID = "sess-1"
	s.turnOpen = true
	s.activeTurnID = "turn-1"

	s.handleToolCallStart(&ToolCallUpdate{ToolCallID: "cmd-bg-1", Name: "run_command", Kind: "execute", Status: "in_progress"}, "")
	s.handleToolCallInProgress(&ToolCallUpdate{ToolCallID: "cmd-bg-1", Name: "run_command", Kind: "execute", Status: "in_progress"}, "")
	s.handleToolCallComplete(&ToolCallUpdate{ToolCallID: "cmd-bg-1", Name: "run_command", Kind: "execute", Status: "completed"}, "")

	mu.Lock()
	defer mu.Unlock()
	var sawTerm bool
	for _, ev := range events {
		if ev.Kind == provider.EventToolComplete {
			t.Fatalf("backgrounded command must emit EventBackgroundTaskTerminal, not in-place EventToolComplete: %+v", events)
		}
		if ev.Kind == provider.EventCommandOutput {
			t.Fatalf("backgrounded command with output_file must not pollute launch row with handoff EventCommandOutput: %+v", events)
		}
		if ev.Kind == provider.EventBackgroundTaskTerminal {
			sawTerm = true
			if ev.ItemID != "cmd-bg-1" || ev.Content != "all tests passed" {
				t.Fatalf("unexpected EventBackgroundTaskTerminal: %+v", ev)
			}
			var m map[string]any
			_ = json.Unmarshal(ev.Meta, &m)
			if m["source"] != "agy_command_complete" || m["task_id"] != "sess/task-bg-1" || m["status"] != "completed" {
				t.Fatalf("unexpected terminal meta: %v", m)
			}
		}
	}
	if !sawTerm {
		t.Fatalf("expected EventBackgroundTaskTerminal for backgrounded command, got %+v", events)
	}
}

func TestACPNestedSubagentRoutesGrandchildToChildLaunch(t *testing.T) {
	t.Parallel()
	parentID := "4d5f2b49-97b2-49e7-a214-7aff00575443"
	childA := "11111111-1111-1111-1111-111111111111"
	childB := "22222222-2222-2222-2222-222222222222"

	fl := &bgCommandStubFlavor{
		DefaultFlavor: DefaultFlavor{Kind: provider.Agy},
		resolved: map[string]string{
			parentID + ":" + childA: "launch-A",
			childA + ":" + childB:   "call-launch-B",
		},
	}
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(fl, &events, &mu)
	s.sessionID = parentID
	s.turnOpen = true
	s.activeTurnID = "turn-1"

	// childA launches childB via invoke_subagent; trackedSubagents records it under childA.
	s.handleUpdate(SessionUpdate{
		SessionUpdate: "tool_call",
		ToolCallID:    childA + ":call-launch-B",
		Name:          "invoke_subagent",
		Status:        "in_progress",
	})
	// Grandchild childB runs a tool call -> must resolve parentToolUseID = childA:call-launch-B.
	s.handleUpdate(SessionUpdate{
		SessionUpdate: "tool_call",
		ToolCallID:    childB + ":call-read-1",
		Name:          "read_file",
		Status:        "in_progress",
	})

	mu.Lock()
	defer mu.Unlock()
	var sawGrandchild bool
	for _, ev := range events {
		if ev.Kind == provider.EventToolStart && ev.ItemID == childB+":call-read-1" {
			if ev.ParentToolUseID != childA+":call-launch-B" {
				t.Fatalf("grandchild ParentToolUseID = %q, want %q", ev.ParentToolUseID, childA+":call-launch-B")
			}
			sawGrandchild = true
		}
	}
	if !sawGrandchild {
		t.Fatalf("expected grandchild tool_call routed to childA:call-launch-B, got %+v", events)
	}
}

type lookupBgFlavor struct {
	DefaultFlavor
	mu                sync.Mutex
	byID              map[string]BackgroundCommandInfo
	liveCalls         int
	includeComplCalls int
}

func (f *lookupBgFlavor) NormalizeToolStart(u *ToolCallUpdate) NormalizedToolStart {
	return NormalizedToolStart{
		ToolName: "Bash",
		ItemType: "command_execution",
		Input:    u.EffectiveInput(),
	}
}

func (f *lookupBgFlavor) NormalizeToolComplete(u *ToolCallUpdate, started NormalizedToolStart) NormalizedToolComplete {
	zero := 0
	return NormalizedToolComplete{
		ToolName:   "Bash",
		ItemType:   "command_execution",
		ItemStatus: "completed",
		ExitCode:   &zero,
		CommandOut: "output for " + u.ToolCallID,
	}
}

func (f *lookupBgFlavor) CanLookupBackgroundCommands(sessionID string) bool { return sessionID != "" }

func (f *lookupBgFlavor) LookupBackgroundCommands(_ context.Context, _ string, toolCallIDs []string, includeCompleted bool, _ int) map[string]BackgroundCommandInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	if includeCompleted {
		f.includeComplCalls++
	} else {
		f.liveCalls++
	}
	out := make(map[string]BackgroundCommandInfo)
	for _, id := range toolCallIDs {
		if info, ok := f.byID[id]; ok {
			out[id] = info
		}
	}
	return out
}

func TestACPConcurrentRunCommandsPromoteOnlyTimedOutCommand(t *testing.T) {
	t.Parallel()
	fl := &lookupBgFlavor{
		DefaultFlavor: DefaultFlavor{Kind: provider.Agy},
		byID:          map[string]BackgroundCommandInfo{},
	}
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(fl, &events, &mu)
	s.sessionID = "sess-concurrent"
	s.turnOpen = true
	s.activeTurnID = "turn-1"
	s.pendingBgCommands = make(map[string]pendingBackgroundCommand)
	s.bgCmdWakeCh = make(chan struct{}, 1)
	defer func() {
		s.closing.Store(true)
		s.sessionCancel()
		s.bgCmdWG.Wait()
	}()

	// Launch two concurrent commands: cmd-fast finishes inline, cmd-slow backgrounds after 50ms.
	s.handleToolCallStart(&ToolCallUpdate{
		ToolCallID: "cmd-fast",
		Name:       "run_command",
		Kind:       "execute",
		Status:     "in_progress",
		RawInput:   json.RawMessage(`{"timeout":5000}`),
	}, "")
	s.handleToolCallStart(&ToolCallUpdate{
		ToolCallID: "cmd-slow",
		Name:       "run_command",
		Kind:       "execute",
		Status:     "in_progress",
		RawInput:   json.RawMessage(`{"timeout":50}`),
	}, "")

	// cmd-fast finishes synchronously before timeout -> must stay inline (EventToolComplete).
	s.handleToolCallComplete(&ToolCallUpdate{
		ToolCallID: "cmd-fast",
		Name:       "run_command",
		Kind:       "execute",
		Status:     "completed",
	}, "")

	// Simulate agy backgrounding cmd-slow (populating task_details on disk, zero in_progress wire output).
	fl.mu.Lock()
	fl.byID["cmd-slow"] = BackgroundCommandInfo{
		TaskID:     "sess-concurrent/task-42",
		OutputFile: "/tmp/task-42.log",
	}
	fl.mu.Unlock()

	// Wait for the per-command watcher to promote cmd-slow after its 50ms timeout + 100ms grace.
	deadline := time.Now().Add(2 * time.Second)
	promoted := false
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, ev := range events {
			if ev.Kind == provider.EventToolStart && ev.ItemID == "cmd-slow" {
				var m map[string]any
				_ = json.Unmarshal(ev.Meta, &m)
				if m["meta_update_only"] == true && m["is_background"] == true && m["task_id"] == "sess-concurrent/task-42" && m["output_file"] == "/tmp/task-42.log" {
					promoted = true
					break
				}
			}
		}
		mu.Unlock()
		if promoted {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !promoted {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("cmd-slow was not promoted to background via LookupBackgroundCommands; events=%+v", events)
	}

	// Now cmd-slow completes -> must emit EventBackgroundTaskTerminal, NOT EventToolComplete.
	s.handleToolCallComplete(&ToolCallUpdate{
		ToolCallID: "cmd-slow",
		Name:       "run_command",
		Kind:       "execute",
		Status:     "completed",
	}, "")

	mu.Lock()
	defer mu.Unlock()
	var sawFastComplete, sawSlowTerminal bool
	for _, ev := range events {
		if ev.ItemID == "cmd-fast" {
			if ev.Kind == provider.EventBackgroundTaskTerminal {
				t.Fatalf("inline cmd-fast must not emit EventBackgroundTaskTerminal: %+v", events)
			}
			if ev.Kind == provider.EventToolComplete {
				sawFastComplete = true
			}
		}
		if ev.ItemID == "cmd-slow" {
			if ev.Kind == provider.EventToolComplete {
				t.Fatalf("backgrounded cmd-slow must emit EventBackgroundTaskTerminal, not EventToolComplete: %+v", events)
			}
			if ev.Kind == provider.EventBackgroundTaskTerminal {
				sawSlowTerminal = true
			}
		}
	}
	if !sawFastComplete || !sawSlowTerminal {
		t.Fatalf("expected fast EventToolComplete (%v) and slow EventBackgroundTaskTerminal (%v); events=%+v", sawFastComplete, sawSlowTerminal, events)
	}
}

func TestACPBgCmdWatcherSkipsPendingApprovalAndCatchesNearTimeoutCompletion(t *testing.T) {
	t.Parallel()
	fl := &lookupBgFlavor{
		DefaultFlavor: DefaultFlavor{Kind: provider.Agy},
		byID: map[string]BackgroundCommandInfo{
			"cmd-gated": {TaskID: "sess-gate/task-1", OutputFile: "/tmp/task-1.log"},
			"cmd-near":  {TaskID: "sess-gate/task-2", OutputFile: "/tmp/task-2.log"},
		},
	}
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(fl, &events, &mu)
	s.sessionID = "sess-gate"
	s.turnOpen = true
	s.activeTurnID = "turn-1"
	s.pendingBgCommands = make(map[string]pendingBackgroundCommand)
	s.bgCmdWakeCh = make(chan struct{}, 1)
	defer func() {
		s.closing.Store(true)
		s.sessionCancel()
		s.bgCmdWG.Wait()
	}()

	// Block cmd-gated on a pending approval before starting it so the watcher
	// must skip lookups even after its short timeout expires.
	s.mu.Lock()
	s.pendingPerms["perm-1"] = pendingPermission{toolCallID: "cmd-gated"}
	s.mu.Unlock()

	s.handleToolCallStart(&ToolCallUpdate{
		ToolCallID: "cmd-gated",
		Name:       "run_command",
		Kind:       "execute",
		Status:     "in_progress",
		RawInput:   json.RawMessage(`{"timeout":20}`),
	}, "")

	// Wait longer than 20ms + 100ms grace; while approval is pending, zero live lookups may run.
	time.Sleep(200 * time.Millisecond)
	fl.mu.Lock()
	callsWhilePending := fl.liveCalls
	fl.mu.Unlock()
	if callsWhilePending != 0 {
		t.Fatalf("expected 0 live lookups while approval is pending, got %d", callsWhilePending)
	}

	// Clearing the approval and resetting the command lets the watcher promote cmd-gated.
	s.mu.Lock()
	delete(s.pendingPerms, "perm-1")
	s.resetBgCommandAfterApprovalLocked("cmd-gated")
	s.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	promoted := false
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, ev := range events {
			if ev.Kind == provider.EventToolStart && ev.ItemID == "cmd-gated" {
				var m map[string]any
				_ = json.Unmarshal(ev.Meta, &m)
				if m["meta_update_only"] == true && m["is_background"] == true {
					promoted = true
					break
				}
			}
		}
		mu.Unlock()
		if promoted {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !promoted {
		t.Fatalf("cmd-gated was not promoted after approval resolved")
	}

	// Verify completion-boundary check catches a command that finished near its
	// timeout (before liveCheckDelay fires) without needing prose heuristics.
	s.handleToolCallStart(&ToolCallUpdate{
		ToolCallID: "cmd-near",
		Name:       "run_command",
		Kind:       "execute",
		Status:     "in_progress",
		RawInput:   json.RawMessage(`{"timeout":5000}`),
	}, "")
	// Simulate elapsed = 4800ms (past completionCheckAfter = 4750ms, before nextCheckAt = 5100ms).
	s.mu.Lock()
	p := s.pendingBgCommands["cmd-near"]
	p.completionCheckAfter = time.Now().Add(-50 * time.Millisecond)
	p.nextCheckAt = time.Now().Add(10 * time.Second)
	s.pendingBgCommands["cmd-near"] = p
	s.mu.Unlock()

	s.handleToolCallComplete(&ToolCallUpdate{
		ToolCallID: "cmd-near",
		Name:       "run_command",
		Kind:       "execute",
		Status:     "completed",
	}, "")

	fl.mu.Lock()
	complCalls := fl.includeComplCalls
	fl.mu.Unlock()
	if complCalls != 1 {
		t.Fatalf("expected 1 completion-boundary lookup for cmd-near, got %d", complCalls)
	}
	mu.Lock()
	defer mu.Unlock()
	var sawNearTerminal bool
	for _, ev := range events {
		if ev.ItemID == "cmd-near" {
			if ev.Kind == provider.EventToolComplete {
				t.Fatalf("cmd-near must emit EventBackgroundTaskTerminal, not EventToolComplete")
			}
			if ev.Kind == provider.EventBackgroundTaskTerminal {
				sawNearTerminal = true
			}
		}
	}
	if !sawNearTerminal {
		t.Fatalf("expected EventBackgroundTaskTerminal for cmd-near, events=%+v", events)
	}
}

type todoTestFlavor struct {
	DefaultFlavor
}

func (todoTestFlavor) ExtractTodoUpdate(u *ToolCallUpdate) ([]PlanEntry, bool) {
	if u == nil {
		return nil, false
	}
	name := strings.TrimSpace(u.Name)
	if name == "" {
		name = strings.TrimSpace(u.Title)
	}
	if name != "update_steps" {
		return nil, false
	}
	raw := u.EffectiveInput()
	if len(raw) == 0 {
		return nil, true
	}
	var in struct {
		Steps []struct {
			Title  string `json:"title"`
			Status string `json:"status"`
		} `json:"steps"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return nil, true
	}
	out := make([]PlanEntry, 0, len(in.Steps))
	for _, s := range in.Steps {
		out = append(out, PlanEntry{Content: s.Title, Status: s.Status})
	}
	return out, true
}

func TestACPTodoToolSuppressedAndRoutedToEventTodoUpdate(t *testing.T) {
	t.Parallel()
	fl := todoTestFlavor{DefaultFlavor: DefaultFlavor{Kind: provider.Agy}}
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := newUnitSession(fl, &events, &mu)
	s.turnOpen = true
	s.activeTurnID = "turn-1"

	// 1. Live parent update_steps with immediate input -> EventTodoUpdate only, zero tool rows.
	s.handleToolCallStart(&ToolCallUpdate{
		ToolCallID: "todo-1",
		Name:       "update_steps",
		Status:     "in_progress",
		RawInput:   json.RawMessage(`{"steps":[{"title":"Step A","status":"done"},{"title":"Step B","status":"in_progress"},{"title":"Step C","status":"pending"}]}`),
	}, "")
	s.handleToolCallInProgress(&ToolCallUpdate{
		ToolCallID: "todo-1",
		Name:       "update_steps",
		Status:     "in_progress",
	}, "")
	s.handleToolCallComplete(&ToolCallUpdate{
		ToolCallID: "todo-1",
		Name:       "update_steps",
		Status:     "completed",
	}, "")

	// 2. Deferred input arriving only on completion -> still emits EventTodoUpdate, zero tool rows.
	s.handleToolCallStart(&ToolCallUpdate{
		ToolCallID: "todo-2",
		Name:       "update_steps",
		Status:     "in_progress",
	}, "")
	s.handleToolCallComplete(&ToolCallUpdate{
		ToolCallID: "todo-2",
		Name:       "update_steps",
		Status:     "completed",
		RawInput:   json.RawMessage(`{"steps":[{"title":"Deferred","status":"skipped"}]}`),
	}, "")

	// 3. Child/subagent update_steps -> suppressed cleanly without overwriting parent live_todo.
	s.handleToolCallStart(&ToolCallUpdate{
		ToolCallID: "child:todo-3",
		Name:       "update_steps",
		Status:     "in_progress",
		RawInput:   json.RawMessage(`{"steps":[{"title":"Child private step","status":"in_progress"}]}`),
	}, "launch-1")
	s.handleToolCallComplete(&ToolCallUpdate{
		ToolCallID: "child:todo-3",
		Name:       "update_steps",
		Status:     "completed",
	}, "launch-1")

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("expected exactly 2 EventTodoUpdate events (and 0 tool rows), got %d: %+v", len(events), events)
	}
	for i, ev := range events {
		if ev.Kind != provider.EventTodoUpdate {
			t.Fatalf("event[%d].Kind = %q, want EventTodoUpdate", i, ev.Kind)
		}
	}
	var m0 struct {
		Kind string `json:"kind"`
		Plan []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(events[0].Meta, &m0); err != nil {
		t.Fatalf("unmarshal todo meta: %v", err)
	}
	if m0.Kind != "todo_update" || len(m0.Plan) != 3 ||
		m0.Plan[0].Status != "completed" || m0.Plan[1].Status != "inProgress" || m0.Plan[2].Status != "pending" {
		t.Fatalf("unexpected todo plan[0]: %+v", m0)
	}
	var m1 struct {
		Plan []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(events[1].Meta, &m1); err != nil {
		t.Fatalf("unmarshal deferred todo meta: %v", err)
	}
	if len(m1.Plan) != 1 || m1.Plan[0].Step != "Deferred" || m1.Plan[0].Status != "completed" {
		t.Fatalf("unexpected deferred todo plan: %+v", m1)
	}

	// 4. ReplayUpdatesToEvents must also suppress update_steps tool rows and emit EventTodoUpdate.
	replayed := ReplayUpdatesToEvents([]SessionUpdate{
		{
			SessionUpdate: "tool_call",
			ToolCallID:    "replay-todo-1",
			Title:         "update_steps",
			Status:        "completed",
			RawInput:      json.RawMessage(`{"steps":[{"title":"Replayed","status":"in_progress"}]}`),
		},
	}, fl)
	var sawReplayTodo bool
	for _, ev := range replayed {
		if ev.Kind == provider.EventToolStart || ev.Kind == provider.EventToolComplete {
			t.Fatalf("replayed update_steps must not emit tool row: %+v", ev)
		}
		if ev.Kind == provider.EventTodoUpdate {
			sawReplayTodo = true
		}
	}
	if !sawReplayTodo {
		t.Fatalf("expected EventTodoUpdate in replayed events, got %+v", replayed)
	}
}
