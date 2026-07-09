package codex

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func newMultiAgentV2RoutingSession(t *testing.T, onEvent func(provider.ProviderEvent)) *Session {
	t.Helper()
	s := &Session{
		threadID:                  "ao-thread",
		codexThreadID:             "root-provider-thread",
		onEvent:                   onEvent,
		childParentByThread:       make(map[string]string),
		childParentByAgentPath:    make(map[string]string),
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
	t.Cleanup(func() {
		s.mu.Lock()
		for _, timer := range s.deferredChildDeadlines {
			timer.Stop()
		}
		s.mu.Unlock()
	})
	return s
}

func v2ActivityLine(threadID, turnID, itemID, kind, childThreadID, agentPath string) []byte {
	encoded, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "item/completed",
		"params": map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item": map[string]any{
				"id":            itemID,
				"type":          "subAgentActivity",
				"kind":          kind,
				"agentThreadId": childThreadID,
				"agentPath":     agentPath,
			},
		},
	})
	return encoded
}

func TestMultiAgentV2StartedNormalizesAndMapsChild(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})

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
				ReceiverThreadIDs []string                   `json:"receiverThreadIds"`
				AgentsStates      map[string]json.RawMessage `json:"agentsStates"`
			} `json:"input"`
		}
		if err := json.Unmarshal(event.Meta, &meta); err != nil {
			t.Fatalf("decode event meta: %v", err)
		}
		if meta.Input.Tool != "spawn_agent" || meta.Input.AgentPath != "/root/reviewer" ||
			len(meta.Input.ReceiverThreadIDs) != 1 || meta.Input.ReceiverThreadIDs[0] != "child-a" ||
			len(meta.Input.AgentsStates) != 1 {
			t.Fatalf("normalized meta = %+v", meta.Input)
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

func TestMultiAgentV2InteractionUsesRawMessageMetadataWhenAvailable(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})
	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/reviewer"))
	events = nil

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"root-provider-thread","turnId":"root-turn","item":{"type":"function_call","name":"followup_task","call_id":"followup-a","arguments":"{\"target\":\"/root/reviewer\",\"message\":\"Check the retry path\"}"}}}`))
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
	if meta.Input.Tool != "send_input" || meta.Input.Prompt != "Check the retry path" ||
		meta.Input.Target != "/root/reviewer" || meta.Input.ActivityTool != "followup_task" {
		t.Fatalf("interaction meta = %+v", meta.Input)
	}
}

func TestMultiAgentV2ChildThreadWideEventsAreSuppressedAndErrorsAreNonFatal(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) {
		events = append(events, event)
	})
	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/reviewer"))
	events = nil

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"child-a","tokenUsage":{"last":{"totalTokens":5},"total":{"totalTokens":5},"modelContextWindow":100}}}`))
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
	if !s.deferChildWireEvent("unknown-child", deferredChildWireEvent{Method: "turn/started", Params: json.RawMessage(`{"threadId":"unknown-child"}`)}) {
		t.Fatal("defer unknown child notification")
	}
	s.expireDeferredChildWireEvents("unknown-child")
	if s.deferredChildWireCount != 0 || len(s.deferredChildWireEvents) != 0 {
		t.Fatalf("expired quarantine retained data: count=%d queues=%d", s.deferredChildWireCount, len(s.deferredChildWireEvents))
	}
	if len(events) != 1 || events[0].Kind != provider.EventNotification {
		t.Fatalf("expiry warning = %+v", events)
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
