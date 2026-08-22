package codex

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func newMultiAgentV2RoutingSession(t *testing.T, onEvent func(provider.ProviderEvent)) *Session {
	t.Helper()
	s := &Session{
		threadID:                  "ao-thread",
		onEvent:                   onEvent,
		childParentByThread:       make(map[string]string),
		childParentByAgentPath:    make(map[string]string),
		childThreadByAgentPath:    make(map[string]string),
		childPathOwnerLive:        make(map[string]bool),
		agentPathByThread:         make(map[string]string),
		agentMetaByThread:         make(map[string]collabReceiverMeta),
		subagentNotificationDedup: make(map[subagentNotificationDedupKey]struct{}),
		rawToolCallsByID:          make(map[string]rawToolCall),
		waitReceiverIDsByCall:     make(map[string][]string),
		deferredChildWireEvents:   make(map[string][]deferredChildWireEvent),
		deferredChildDeadlines:    make(map[string]*time.Timer),
		collabHistoryGeneration:   1,
		collabHistoryVisited:      make(map[string]uint64),
		planBuffersByItemID:       make(map[string]*planBuffer),
		planBuffersByTurnID:       make(map[string]*planBuffer),
	}
	s.setRootThreadID("root-provider-thread")
	t.Cleanup(func() {
		s.mu.Lock()
		for _, timer := range s.deferredChildDeadlines {
			timer.Stop()
		}
		s.mu.Unlock()
	})
	return s
}

// v2ActivityParams is the `params` object codex >= 0.146 sends on BOTH legs of
// a subAgentActivity item — emit_sub_agent_activity
// (codex-rs/core/src/tools/handlers/multi_agents_v2.rs) hands the same
// SubAgentActivityItem to emit_turn_item_started and emit_turn_item_completed,
// so the two notifications differ only in method.
func v2ActivityParams(threadID, turnID, itemID, kind, childThreadID, agentPath string) json.RawMessage {
	encoded, _ := json.Marshal(map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
		"item": map[string]any{
			"id":            itemID,
			"type":          "subAgentActivity",
			"kind":          kind,
			"agentThreadId": childThreadID,
			"agentPath":     agentPath,
		},
	})
	return encoded
}

func v2ActivityNotification(method, threadID, turnID, itemID, kind, childThreadID, agentPath string) []byte {
	encoded, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  v2ActivityParams(threadID, turnID, itemID, kind, childThreadID, agentPath),
	})
	return encoded
}

func v2ActivityLine(threadID, turnID, itemID, kind, childThreadID, agentPath string) []byte {
	return v2ActivityNotification("item/completed", threadID, turnID, itemID, kind, childThreadID, agentPath)
}

// TestMultiAgentV2ActivityStartedLegIsDropped pins that the item/started half
// of a subAgentActivity pair is consumed silently. Codex >= 0.146 emits both
// legs for every kind; routing the started leg through the generic tool branch
// minted a raw tool_call row named "subAgentActivity". For started/interacted
// the completed leg upserts the same item id so the row was only transient,
// but interrupted's completed leg is a status event that never settles the
// row — turn-end reconciliation then flipped it to errored, leaving permanent
// junk in the timeline.
func TestMultiAgentV2ActivityStartedLegIsDropped(t *testing.T) {
	for _, kind := range []string{"started", "interacted", "interrupted"} {
		t.Run(kind, func(t *testing.T) {
			params := v2ActivityParams("root-provider-thread", "root-turn", "activity-1", kind, "child-a", "/root/reviewer")
			events, handled := classifyItemNotification("ao-thread", "item/started", params, time.Unix(0, 0).UTC())
			if !handled {
				t.Fatal("item/started subAgentActivity fell through unclaimed")
			}
			if len(events) != 0 {
				t.Fatalf("started leg events = %+v, want none", events)
			}
		})
	}
}

// TestMultiAgentV2ActivityCompletedLegClassification is the counterpart guard:
// dropping the started leg is only safe while the completed leg keeps
// synthesizing the full contract for every kind.
func TestMultiAgentV2ActivityCompletedLegClassification(t *testing.T) {
	type wantEvent struct {
		kind     provider.EventKind
		itemType string
	}
	tests := []struct {
		kind string
		want []wantEvent
	}{
		{
			kind: "started",
			want: []wantEvent{
				{kind: provider.EventToolStart, itemType: "collab_agent"},
				{kind: provider.EventToolComplete, itemType: "collab_agent"},
			},
		},
		{
			kind: "interacted",
			want: []wantEvent{{kind: provider.EventToolComplete, itemType: "send_input"}},
		},
		{
			// A status event, not a tool completion — which is exactly why the
			// started leg must not create a tool row for this kind.
			kind: "interrupted",
			want: []wantEvent{{kind: provider.EventSubagentStatus}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			params := v2ActivityParams("root-provider-thread", "root-turn", "activity-1", tt.kind, "child-a", "/root/reviewer")
			events, handled := classifyItemNotification("ao-thread", "item/completed", params, time.Unix(0, 0).UTC())
			if !handled {
				t.Fatal("item/completed subAgentActivity fell through unclaimed")
			}
			if len(events) != len(tt.want) {
				t.Fatalf("events = %+v, want %d", events, len(tt.want))
			}
			for i, want := range tt.want {
				if events[i].Kind != want.kind || events[i].ItemType != want.itemType {
					t.Fatalf("event %d = (%q, %q), want (%q, %q)",
						i, events[i].Kind, events[i].ItemType, want.kind, want.itemType)
				}
				if events[i].ItemID != "activity-1" || events[i].TurnID != "root-turn" {
					t.Fatalf("event %d identity = (%q, %q)", i, events[i].ItemID, events[i].TurnID)
				}
			}
		})
	}
}

// TestMultiAgentV2InterruptedActivityPairEmitsOnlyStatus drives the full
// started+completed wire pair through the session, which is the sequence that
// left errored `subAgentActivity` tool rows in production thread 4567bd49.
func TestMultiAgentV2InterruptedActivityPairEmitsOnlyStatus(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})
	s.dispatchLine(v2ActivityNotification("item/started", "root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/reviewer"))
	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/reviewer"))
	if len(events) != 2 || events[0].Kind != provider.EventToolStart || events[1].Kind != provider.EventToolComplete {
		t.Fatalf("spawn pair events = %+v, want exactly the synthesized start/complete", events)
	}
	events = nil

	s.dispatchLine(v2ActivityNotification("item/started", "root-provider-thread", "root-turn", "interrupt-call", "interrupted", "child-a", "/root/reviewer"))
	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "interrupt-call", "interrupted", "child-a", "/root/reviewer"))

	if len(events) != 1 {
		t.Fatalf("interrupt events = %+v, want only the scoped status event", events)
	}
	if events[0].Kind != provider.EventSubagentStatus || events[0].ItemID != "spawn-a" || events[0].ParentToolUseID != "spawn-a" {
		t.Fatalf("interrupt event = %+v", events[0])
	}
}

func TestMultiAgentV2StartedNormalizesAndMapsChild(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})
	s.model = "gpt-5.4"
	s.reasoningEffort = "high"
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"child-a","agentNickname":"Socrates","source":{"subAgent":{"threadSpawn":{"parentThreadId":"root-provider-thread","agentPath":"/root/reviewer"}}}}}}`))

	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/reviewer"))

	if got := s.parentToolUseForProviderThread("child-a"); got != "spawn-a" {
		t.Fatalf("child ownership = %q, want spawn-a", got)
	}
	if got := s.parentToolUseForAgentPath("/root/reviewer"); got != "spawn-a" {
		t.Fatalf("agent path ownership = %q, want spawn-a", got)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v, want tool start and completion", events)
	}
	if events[0].Kind != provider.EventToolStart || events[1].Kind != provider.EventToolComplete {
		t.Fatalf("activity kinds = %q, %q", events[0].Kind, events[1].Kind)
	}
	for _, event := range events {
		if event.ItemID != "spawn-a" || event.ItemType != "collab_agent" || event.ParentToolUseID != "" {
			t.Fatalf("unexpected normalized spawn event: %+v", event)
		}
		var meta struct {
			Input struct {
				Tool              string                     `json:"tool"`
				AgentPath         string                     `json:"agentPath"`
				Model             string                     `json:"model"`
				ReasoningEffort   string                     `json:"reasoningEffort"`
				AgentNickname     string                     `json:"newAgentNickname"`
				AgentRole         string                     `json:"newAgentRole"`
				ReceiverThreadIDs []string                   `json:"receiverThreadIds"`
				AgentsStates      map[string]json.RawMessage `json:"agentsStates"`
			} `json:"input"`
		}
		if err := json.Unmarshal(event.Meta, &meta); err != nil {
			t.Fatalf("decode event meta: %v", err)
		}
		if meta.Input.Tool != "spawn_agent" || meta.Input.AgentPath != "/root/reviewer" ||
			meta.Input.Model != "gpt-5.4" || meta.Input.ReasoningEffort != "high" ||
			meta.Input.AgentNickname != "Socrates" || meta.Input.AgentRole != "default" ||
			len(meta.Input.ReceiverThreadIDs) != 1 || meta.Input.ReceiverThreadIDs[0] != "child-a" ||
			len(meta.Input.AgentsStates) != 1 {
			t.Fatalf("normalized meta = %+v", meta.Input)
		}
	}
}

func TestMultiAgentV2NestedSpawnInheritsSourceAgentProfile(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})
	s.model = "gpt-root"
	s.reasoningEffort = "high"
	if !s.registerChildOwnership("root-provider-thread", "child-a", "/root/reviewer", "spawn-a") {
		t.Fatal("register source child ownership")
	}
	s.rememberCollabProfile("child-a", "gpt-child", "low")

	s.dispatchLine(v2ActivityLine("child-a", "child-turn", "spawn-b", "started", "grandchild-b", "/root/reviewer/worker"))

	if len(events) != 2 {
		t.Fatalf("events = %+v, want nested tool start and completion", events)
	}
	for _, event := range events {
		var meta struct {
			Input struct {
				Model           string `json:"model"`
				ReasoningEffort string `json:"reasoningEffort"`
			} `json:"input"`
		}
		if err := json.Unmarshal(event.Meta, &meta); err != nil {
			t.Fatalf("decode nested spawn meta: %v", err)
		}
		if meta.Input.Model != "gpt-child" || meta.Input.ReasoningEffort != "low" {
			t.Fatalf("nested profile = %q/%q, want gpt-child/low", meta.Input.Model, meta.Input.ReasoningEffort)
		}
	}
}

func TestMultiAgentV2NestedChildrenNeverEnterParentLifecycle(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})

	// The child starts and streams before the root's spawn activity arrives.
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"child-a","source":{"subAgent":{"threadSpawn":{"parentThreadId":"root-provider-thread","agentPath":"/root/reviewer"}}}}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"child-a","turn":{"id":"child-turn","status":"inProgress"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"child-a","turnId":"child-turn","itemId":"child-text","delta":"child output"}}`))
	if len(events) != 0 {
		t.Fatalf("unowned child events leaked: %+v", events)
	}

	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/reviewer"))
	if len(events) != 4 || events[2].Kind != provider.EventSubagentStatus || events[3].Kind != provider.EventTextDelta || events[3].ParentToolUseID != "spawn-a" {
		t.Fatalf("root child replay = %+v", events)
	}

	// A grandchild can win the same race. Its ownership activity is emitted
	// on child-a and therefore nests under spawn-a; the grandchild transcript
	// nests under the new spawn-b card, never the AO main scope.
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"grandchild-b","turn":{"id":"grandchild-turn","status":"inProgress"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"grandchild-b","turnId":"grandchild-turn","itemId":"grandchild-text","delta":"grandchild output"}}`))
	if len(events) != 4 {
		t.Fatalf("unowned grandchild events leaked: %+v", events)
	}

	s.dispatchLine(v2ActivityLine("child-a", "child-turn", "spawn-b", "started", "grandchild-b", "/root/reviewer/deep_review"))
	if got := s.parentToolUseForProviderThread("grandchild-b"); got != "spawn-b" {
		t.Fatalf("grandchild ownership = %q, want spawn-b", got)
	}
	if len(events) != 8 {
		t.Fatalf("events after nested activity = %+v", events)
	}
	if events[4].Kind != provider.EventToolStart || events[4].ParentToolUseID != "spawn-a" ||
		events[5].Kind != provider.EventToolComplete || events[5].ParentToolUseID != "spawn-a" {
		t.Fatalf("nested spawn was not scoped to child: %+v / %+v", events[4], events[5])
	}
	if events[6].Kind != provider.EventSubagentStatus || events[6].ParentToolUseID != "spawn-b" ||
		events[7].Kind != provider.EventTextDelta || events[7].ParentToolUseID != "spawn-b" {
		t.Fatalf("grandchild replay was not scoped to nested spawn: %+v / %+v", events[6], events[7])
	}
	for _, event := range events {
		if event.Kind == provider.EventTurnStart || event.Kind == provider.EventTurnComplete {
			t.Fatalf("child lifecycle leaked onto AO parent: %+v", event)
		}
	}
}

func TestMultiAgentV2ChildCompletionAndInterruptAreScopedStatuses(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})
	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/reviewer"))
	events = nil

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"child-a","turn":{"id":"child-turn","status":"completed","error":null}}}`))
	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "interrupt-call", "interrupted", "child-a", "/root/reviewer"))

	if len(events) != 2 {
		t.Fatalf("terminal events = %+v", events)
	}
	for _, event := range events {
		if event.Kind != provider.EventSubagentStatus || event.ItemID != "spawn-a" || event.ParentToolUseID != "spawn-a" {
			t.Fatalf("terminal activity was not scoped to launch: %+v", event)
		}
	}
}

func TestMultiAgentV2InteractionDoesNotExposeEncryptedRawMessage(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})
	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/reviewer"))
	events = nil

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"root-provider-thread","turnId":"root-turn","item":{"type":"function_call","namespace":"collaboration","name":"followup_task","call_id":"followup-a","arguments":"{\"target\":\"/root/reviewer\",\"message\":\"gAAAA-encrypted\"}"}}}`))
	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "followup-a", "interacted", "child-a", "/root/reviewer"))

	if len(events) != 1 || events[0].Kind != provider.EventToolComplete || events[0].ItemType != "send_input" {
		t.Fatalf("interaction events = %+v", events)
	}
	var meta struct {
		Input struct {
			Tool         string `json:"tool"`
			Prompt       string `json:"prompt"`
			Target       string `json:"target"`
			ActivityTool string `json:"activityTool"`
		} `json:"input"`
	}
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("decode interaction meta: %v", err)
	}
	if meta.Input.Tool != "send_input" || meta.Input.Prompt != "" ||
		meta.Input.Target != "/root/reviewer" || meta.Input.ActivityTool != "followup_task" {
		t.Fatalf("interaction meta = %+v", meta.Input)
	}
}

func TestMultiAgentV2SpawnDoesNotExposeEncryptedRawMessage(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"root-provider-thread","turnId":"root-turn","item":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","call_id":"spawn-a","arguments":"{\"task_name\":\"reviewer\",\"message\":\"gAAAA-encrypted\"}"}}}`))
	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/reviewer"))

	if len(events) != 2 {
		t.Fatalf("events = %+v, want tool start and completion", events)
	}
	for _, event := range events {
		var meta struct {
			Input struct {
				Prompt string `json:"prompt"`
			} `json:"input"`
		}
		if err := json.Unmarshal(event.Meta, &meta); err != nil {
			t.Fatalf("decode event meta: %v", err)
		}
		if meta.Input.Prompt != "" {
			t.Fatalf("encrypted prompt leaked into event meta: %q", meta.Input.Prompt)
		}
	}
}

func TestMultiAgentV2ChildThreadWideEventsAreSuppressedAndErrorsAreNonFatal(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})
	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/reviewer"))
	events = nil

	// A child's token usage is no longer dropped: it is re-emitted as a
	// SCOPED progress tick (never EventTokenUsage, never the parent
	// meter). See TestDispatchLineChildTokenUsageBecomesScopedProgress.
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"child-a","tokenUsage":{"last":{"totalTokens":5},"total":{"totalTokens":5},"modelContextWindow":100}}}`))
	if len(events) != 1 || events[0].Kind != provider.EventSubagentProgress || events[0].ItemID != "spawn-a" {
		t.Fatalf("child token usage = %+v, want one scoped progress tick", events)
	}
	events = nil

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"child-a","turnId":"child-turn","item":{"id":"child-user","type":"userMessage","content":[{"type":"text","text":"child prompt"}]}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"model/rerouted","params":{"threadId":"child-a","toModel":"other-model"}}`))
	if len(events) != 0 {
		t.Fatalf("child thread-wide events leaked: %+v", events)
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"error","params":{"threadId":"child-a","willRetry":false,"error":{"message":"child failed"}}}`))
	if len(events) != 2 || events[0].Kind != provider.EventError || events[1].Kind != provider.EventSubagentStatus {
		t.Fatalf("child error events = %+v", events)
	}
	if events[0].ParentToolUseID != "spawn-a" || events[1].ItemID != "spawn-a" {
		t.Fatalf("child error was not scoped: %+v", events)
	}
	var errorMeta struct {
		Fatal bool `json:"fatal"`
	}
	if err := json.Unmarshal(events[0].Meta, &errorMeta); err != nil || errorMeta.Fatal {
		t.Fatalf("child error retained parent-fatal semantics: meta=%s err=%v", events[0].Meta, err)
	}
}

func TestMultiAgentV2DeferredChildApprovalDrainsAfterOwnership(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})
	requestID := json.Number("41")
	params := json.RawMessage(`{"threadId":"child-a","turnId":"child-turn","itemId":"command-a","command":"go test ./..."}`)
	s.dispatchServerRequest("item/commandExecution/requestApproval", &requestID, params, nil)
	if len(events) != 0 {
		t.Fatalf("unowned child approval leaked: %+v", events)
	}

	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/reviewer"))
	if len(events) != 3 || events[2].Kind != provider.EventApprovalRequest || events[2].ParentToolUseID != "spawn-a" {
		t.Fatalf("deferred child approval did not drain under spawn: %+v", events)
	}
}

func TestMultiAgentV2RawExecResultStaysInAOThreadAndChildScope(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})
	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/reviewer"))
	events = nil

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"child-a","turnId":"child-turn","item":{"type":"function_call","name":"exec_command","call_id":"cmd-child","arguments":"{\"cmd\":\"sleep 10\"}"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"child-a","turnId":"child-turn","item":{"type":"function_call_output","call_id":"cmd-child","output":"Process running with session ID 42\nOutput:\n"}}}`))

	if len(events) != 1 {
		t.Fatalf("raw child events = %+v", events)
	}
	if events[0].Kind != provider.EventCodexExecResult || events[0].ThreadID != "ao-thread" || events[0].ParentToolUseID != "spawn-a" {
		t.Fatalf("raw child exec escaped AO scope: %+v", events[0])
	}
}

func TestMultiAgentV2OwnershipRejectsSelfAndConflictingRemap(t *testing.T) {
	s := newMultiAgentV2RoutingSession(t, nil)
	if s.registerChildOwnership("root-provider-thread", "root-provider-thread", "/root", "spawn-self") {
		t.Fatal("registered root provider thread as its own child")
	}
	if !s.registerChildOwnership("root-provider-thread", "child-a", "/root/review", "spawn-a") {
		t.Fatal("initial child ownership was rejected")
	}
	if s.registerChildOwnership("root-provider-thread", "child-a", "/root/other", "spawn-b") {
		t.Fatal("conflicting child ownership was accepted")
	}
	if got := s.parentToolUseForProviderThread("child-a"); got != "spawn-a" {
		t.Fatalf("child ownership changed to %q", got)
	}
}

func TestMultiAgentV2OwnershipAllowsPathReuseByNewChildAfterRestart(t *testing.T) {
	s := newMultiAgentV2RoutingSession(t, nil)
	if !s.registerHistoricalChildOwnership("root-provider-thread", "child-old", "/root/review", "spawn-old") {
		t.Fatal("register historical child ownership")
	}
	if !s.registerChildOwnership("root-provider-thread", "child-new", "/root/review", "spawn-new") {
		t.Fatal("new child could not replace historical path ownership")
	}

	if got := s.parentToolUseForProviderThread("child-old"); got != "spawn-old" {
		t.Fatalf("historical thread ownership = %q, want spawn-old", got)
	}
	if got := s.parentToolUseForProviderThread("child-new"); got != "spawn-new" {
		t.Fatalf("new thread ownership = %q, want spawn-new", got)
	}
	if got := s.parentToolUseForAgentPath("/root/review"); got != "spawn-new" {
		t.Fatalf("current path ownership = %q, want spawn-new", got)
	}
	if got := s.providerThreadForAgentPath("/root/review", "spawn-new"); got != "child-new" {
		t.Fatalf("current provider thread = %q, want child-new", got)
	}
	if got := s.providerThreadForAgentPath("/root/review", "spawn-old"); got != "" {
		t.Fatalf("historical provider thread still owns reused path: %q", got)
	}
	if s.registerChildOwnership("root-provider-thread", "child-old", "/root/review", "spawn-old") {
		t.Fatal("superseded child reclaimed reused path through replay")
	}
	if got := s.providerThreadForAgentPath("/root/review", "spawn-new"); got != "child-new" {
		t.Fatalf("replayed ownership changed current provider thread to %q", got)
	}
	if s.registerChildOwnership("root-provider-thread", "child-malicious", "/root/review", "spawn-malicious") {
		t.Fatal("unseen live child replaced an existing live path owner")
	}
}

func TestMultiAgentV2HistoryCannotReplaceLivePathOwner(t *testing.T) {
	s := newMultiAgentV2RoutingSession(t, nil)
	if !s.registerChildOwnership("root-provider-thread", "child-new", "/root/review", "spawn-new") {
		t.Fatal("register live child ownership")
	}
	if !s.registerHistoricalChildOwnership("root-provider-thread", "child-old", "/root/review", "spawn-old") {
		t.Fatal("register old historical child ownership")
	}
	if !s.registerHistoricalChildOwnership("root-provider-thread", "child-new", "/root/review", "spawn-new") {
		t.Fatal("register current child history")
	}

	if got := s.providerThreadForAgentPath("/root/review", "spawn-new"); got != "child-new" {
		t.Fatalf("history replaced live provider thread with %q", got)
	}
	if !s.childPathOwnerLive["/root/review"] {
		t.Fatal("history downgraded live path ownership provenance")
	}
}

func TestMultiAgentV2PathReuseDrainsBufferedChildCompletion(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})
	if !s.registerHistoricalChildOwnership("root-provider-thread", "child-old", "/root/review", "spawn-old") {
		t.Fatal("register historical child ownership")
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"child-new","turn":{"id":"child-turn","status":"inProgress"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"child-new","turn":{"id":"child-turn","status":"completed","error":null}}}`))
	if len(events) != 0 {
		t.Fatalf("unowned child lifecycle leaked before spawn: %+v", events)
	}

	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "spawn-new", "started", "child-new", "/root/review"))
	if len(events) != 4 {
		t.Fatalf("events after reused-path spawn = %+v", events)
	}
	if events[2].Kind != provider.EventSubagentStatus || events[2].ParentToolUseID != "spawn-new" ||
		events[3].Kind != provider.EventSubagentStatus || events[3].ParentToolUseID != "spawn-new" {
		t.Fatalf("buffered lifecycle was not drained under new spawn: %+v", events)
	}
	var terminalMeta struct {
		AgentPath string `json:"agent_path"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(events[3].Meta, &terminalMeta); err != nil {
		t.Fatalf("decode terminal status: %v", err)
	}
	if terminalMeta.AgentPath != "child-new" || terminalMeta.Status != "completed" {
		t.Fatalf("terminal status = %+v", terminalMeta)
	}
}

func TestMultiAgentV2LegacyConversationApprovalUsesChildScope(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})
	if !s.registerChildOwnership("root-provider-thread", "child-a", "/root/review", "spawn-a") {
		t.Fatal("register child ownership")
	}
	requestID := json.Number("17")
	s.dispatchServerRequest("execCommandApproval", &requestID, json.RawMessage(`{"conversationId":"child-a","callId":"cmd-1","command":["go","test","./..."]}`), nil)
	if len(events) != 1 || events[0].Kind != provider.EventApprovalRequest || events[0].ParentToolUseID != "spawn-a" {
		t.Fatalf("legacy child approval routing = %+v", events)
	}
}

func TestMultiAgentV2QuarantineExpiryDropsUnownedNotifications(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"unknown-child","agentNickname":"Orphan"}}}`))
	if len(s.collabReceiverMetadataForThreads([]string{"unknown-child"})) != 1 {
		t.Fatal("accepted quarantine did not retain child display metadata")
	}
	s.expireDeferredChildWireEvents("unknown-child")
	if s.deferredChildWireCount != 0 || len(s.deferredChildWireEvents) != 0 {
		t.Fatalf("expired quarantine retained data: count=%d queues=%d", s.deferredChildWireCount, len(s.deferredChildWireEvents))
	}
	if len(events) != 1 || events[0].Kind != provider.EventNotification {
		t.Fatalf("expiry warning = %+v", events)
	}
	if len(s.collabReceiverMetadataForThreads([]string{"unknown-child"})) != 0 {
		t.Fatal("expired quarantine retained child display metadata")
	}
}

func TestMultiAgentV2RejectedQuarantineDoesNotCacheMetadata(t *testing.T) {
	s := newMultiAgentV2RoutingSession(t, func(provider.ProviderEvent) {})
	providerThreadID := strings.Repeat("x", maxDeferredChildThreadIDBytes+1)
	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "thread/started",
		"params": map[string]any{
			"thread": map[string]any{"id": providerThreadID, "agentNickname": "Orphan"},
		},
	})
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
	s.dispatchLine(line)
	if len(s.collabReceiverMetadataForThreads([]string{providerThreadID})) != 0 {
		t.Fatal("rejected quarantine retained child display metadata")
	}
}

func TestCollabHistoryOwnershipsSupportV1AndV2(t *testing.T) {
	response := json.RawMessage(`{
		"thread":{"id":"root-provider-thread","turns":[{"items":[
			{"id":"spawn-v1","type":"collabAgentToolCall","tool":"spawnAgent","receiverThreadIds":["child-v1"],"prompt":"inspect"},
			{"id":"spawn-v2","type":"subAgentActivity","kind":"started","agentThreadId":"child-v2","agentPath":"/root/review"}
		]}]}
	}`)
	ownerships, err := collabHistoryOwnerships(response)
	if err != nil {
		t.Fatalf("parse history ownerships: %v", err)
	}
	if len(ownerships) != 2 {
		t.Fatalf("history ownerships = %+v", ownerships)
	}
	if ownerships[0].ParentItemID != "spawn-v1" || ownerships[0].ChildThreadID != "child-v1" || ownerships[0].LaunchMeta.Prompt != "inspect" {
		t.Fatalf("v1 ownership = %+v", ownerships[0])
	}
	if ownerships[1].ParentItemID != "spawn-v2" || ownerships[1].ChildThreadID != "child-v2" || ownerships[1].AgentPath != "/root/review" {
		t.Fatalf("v2 ownership = %+v", ownerships[1])
	}
}

func TestCollabHistoryTerminalReconciliation(t *testing.T) {
	tests := []struct {
		name       string
		response   json.RawMessage
		wantStatus string
	}{
		{
			name:       "completed idle child",
			response:   json.RawMessage(`{"thread":{"id":"child-a","status":{"type":"idle"},"turns":[{"id":"turn-a","status":"completed"}]}}`),
			wantStatus: "completed",
		},
		{
			name:       "failed idle child",
			response:   json.RawMessage(`{"thread":{"id":"child-a","status":{"type":"idle"},"turns":[{"id":"turn-a","status":"failed"}]}}`),
			wantStatus: "errored",
		},
		{
			name:       "interrupted idle child",
			response:   json.RawMessage(`{"thread":{"id":"child-a","status":{"type":"idle"},"turns":[{"id":"turn-a","status":"interrupted"}]}}`),
			wantStatus: "interrupted",
		},
		{
			name:     "active child with previous completed turn",
			response: json.RawMessage(`{"thread":{"id":"child-a","status":{"type":"active"},"turns":[{"id":"turn-a","status":"completed"}]}}`),
		},
		{
			name:     "idle child with in-progress turn",
			response: json.RawMessage(`{"thread":{"id":"child-a","status":{"type":"idle"},"turns":[{"id":"turn-a","status":"inProgress"}]}}`),
		},
		{
			name:       "system error child",
			response:   json.RawMessage(`{"thread":{"id":"child-a","status":{"type":"systemError"},"turns":[{"id":"turn-a","status":"completed"}]}}`),
			wantStatus: "errored",
		},
		{
			name:       "not loaded child",
			response:   json.RawMessage(`{"thread":{"id":"child-a","status":{"type":"notLoaded"},"turns":[{"id":"turn-a","status":"completed"}]}}`),
			wantStatus: "completed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var events []provider.ProviderEvent
			s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
				events = append(events, event)
			})
			_, err := s.reconcileCollabHistoryTerminal(collabHistoryJob{Ownership: collabHistoryOwnership{
				ParentItemID:  "spawn-a",
				ChildThreadID: "child-a",
			}}, tt.response, 0)
			if err != nil {
				t.Fatalf("reconcile terminal history: %v", err)
			}

			if tt.wantStatus == "" {
				if len(events) != 0 {
					t.Fatalf("unexpected recovery event: %+v", events)
				}
				return
			}
			if len(events) != 1 || events[0].Kind != provider.EventSubagentStatus ||
				events[0].ItemID != "spawn-a" || events[0].ParentToolUseID != "spawn-a" {
				t.Fatalf("recovery event = %+v", events)
			}
			var meta struct {
				AgentPath string `json:"agent_path"`
				Status    string `json:"status"`
			}
			if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
				t.Fatalf("decode recovery status: %v", err)
			}
			if meta.AgentPath != "child-a" || meta.Status != tt.wantStatus {
				t.Fatalf("recovery status = %+v, want child-a/%s", meta, tt.wantStatus)
			}
		})
	}
}

func TestCollabHistoryTerminalReconciliationRejectsStaleAndMismatchedSnapshots(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})
	job := collabHistoryJob{Ownership: collabHistoryOwnership{
		ParentItemID:  "spawn-a",
		ChildThreadID: "child-a",
	}}
	completed := json.RawMessage(`{"thread":{"id":"child-a","status":{"type":"idle"},"turns":[{"id":"turn-a","status":"completed"}]}}`)

	revisionBeforeRead := s.childLifecycleRevisionForThread("child-a")
	if !s.registerChildOwnership("root-provider-thread", "child-a", "/root/review", "spawn-a") {
		t.Fatal("register child ownership")
	}
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"child-a","turn":{"id":"turn-b","status":"inProgress"}}}`))
	events = nil
	if _, err := s.reconcileCollabHistoryTerminal(job, completed, revisionBeforeRead); err != nil {
		t.Fatalf("reconcile stale snapshot: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("stale snapshot overwrote live lifecycle: %+v", events)
	}

	mismatched := json.RawMessage(`{"thread":{"id":"child-other","status":{"type":"idle"},"turns":[{"status":"completed"}]}}`)
	if _, err := s.reconcileCollabHistoryTerminal(job, mismatched, s.childLifecycleRevisionForThread("child-a")); err == nil {
		t.Fatal("accepted terminal history for a different child thread")
	}
}
