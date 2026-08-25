package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestDispatchLineCollabSpawnRemembersReceiverThread(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{"child-done": "call-collab-1"},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"provider-parent","item":{"id":"call-collab-1","type":"collabAgentToolCall","tool":"spawnAgent","receiverThreadIds":["child-provider-1"],"prompt":"Refactor auth","status":"completed"}}}`))

	if got := s.parentToolUseForProviderThread("child-provider-1"); got != "call-collab-1" {
		t.Fatalf("child mapping: got %q, want %q", got, "call-collab-1")
	}
	if len(events) != 1 || events[0].ItemType != "collab_agent" {
		t.Fatalf("expected collab_agent event, got %+v", events)
	}
}

func TestDispatchLineThreadStartedRemembersAgentPath(t *testing.T) {
	s := &Session{
		threadID:               "parent-thread",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    map[string]string{"child-provider-1": "call-collab-1"},
		childParentByAgentPath: make(map[string]string),
		agentPathByThread:      make(map[string]string),
		onEvent:                func(provider.ProviderEvent) {},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"child-provider-1","source":{"subAgent":{"thread_spawn":{"parent_thread_id":"provider-parent","depth":1,"agent_path":"/root/researcher"}}}}}}`))

	if got := s.parentToolUseForAgentPath("/root/researcher"); got != "call-collab-1" {
		t.Fatalf("agent path mapping: got %q, want %q", got, "call-collab-1")
	}
}

func TestDispatchLineWaitAgentEnrichesReceiverMetadata(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:               "parent-thread",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    map[string]string{"child-provider-1": "call-collab-1"},
		childParentByAgentPath: make(map[string]string),
		agentPathByThread:      make(map[string]string),
		agentMetaByThread:      make(map[string]collabReceiverMeta),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"child-provider-1","agentNickname":"Galileo","agentRole":"explorer","source":{"subAgent":{"thread_spawn":{"parent_thread_id":"provider-parent","depth":1,"agent_path":"/root/researcher"}}}}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"parent-thread","item":{"id":"wait-1","type":"collabAgentToolCall","tool":"wait","receiverThreadIds":["child-provider-1"],"status":"inProgress"}}}`))

	var waitEvent *provider.ProviderEvent
	for i := range events {
		if events[i].ItemType == "wait_agent" {
			waitEvent = &events[i]
		}
	}
	if waitEvent == nil {
		t.Fatalf("wait_agent event missing: %+v", events)
	}
	var meta struct {
		Input struct {
			ReceiverAgents []collabReceiverMeta `json:"receiverAgents"`
		} `json:"input"`
	}
	if err := json.Unmarshal(waitEvent.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if len(meta.Input.ReceiverAgents) != 1 {
		t.Fatalf("receiverAgents = %+v, want one", meta.Input.ReceiverAgents)
	}
	got := meta.Input.ReceiverAgents[0]
	if got.ThreadID != "child-provider-1" || got.AgentNickname != "Galileo" || got.AgentRole != "explorer" {
		t.Fatalf("receiver metadata = %+v, want child-provider-1/Galileo/explorer", got)
	}
}

func TestDispatchLineRawSpawnOutputMapsAgentIDForSubagentNotification(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:               "parent-thread",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    make(map[string]string),
		childParentByAgentPath: make(map[string]string),
		agentPathByThread:      make(map[string]string),
		agentMetaByThread:      make(map[string]collabReceiverMeta),
		rawToolCallsByID:       make(map[string]rawToolCall),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"spawn_agent","call_id":"spawn-1","arguments":"{\"agent_type\":\"default\",\"message\":\"Run a command, then finish\"}"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call_output","call_id":"spawn-1","output":"{\"agent_id\":\"019ecee6-4686-75e3-91aa-6594ec7aab09\",\"nickname\":\"Pasteur\"}"}}}`))
	if got := s.parentToolUseForProviderThread("019ecee6-4686-75e3-91aa-6594ec7aab09"); got != "spawn-1" {
		t.Fatalf("parent for raw spawn agent id = %q, want spawn-1", got)
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"019ecee6-4686-75e3-91aa-6594ec7aab09\",\"status\":{\"completed\":\"detached child finished after bash command\"}}</subagent_notification>"}]}}}`))

	var notif *provider.ProviderEvent
	for i := range events {
		switch events[i].Kind {
		case provider.EventSubagentNotification:
			notif = &events[i]
		case provider.EventUserText:
			if strings.Contains(events[i].Content, "subagent_notification") {
				t.Fatalf("subagent notification carrier emitted as user text: %+v", events[i])
			}
		}
	}
	if notif == nil {
		t.Fatalf("expected EventSubagentNotification, got %+v", events)
	}
	if notif.ItemID != "spawn-1" {
		t.Fatalf("notification ItemID = %q, want spawn-1", notif.ItemID)
	}
	var meta map[string]any
	if err := json.Unmarshal(notif.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["agent_path"] != "019ecee6-4686-75e3-91aa-6594ec7aab09" {
		t.Fatalf("meta.agent_path = %v, want raw spawned agent id", meta["agent_path"])
	}
	if meta["message"] != "detached child finished after bash command" {
		t.Fatalf("meta.message = %v, want child completion message", meta["message"])
	}
}

func TestDispatchLineRawSpawnOutputMapsAgentIDForRawUserSubagentNotification(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:               "parent-thread",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    make(map[string]string),
		childParentByAgentPath: make(map[string]string),
		agentPathByThread:      make(map[string]string),
		agentMetaByThread:      make(map[string]collabReceiverMeta),
		rawToolCallsByID:       make(map[string]rawToolCall),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}
	s.setRootThreadID("parent-provider-thread")

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-provider-thread","turnId":"turn-1","item":{"type":"function_call","name":"spawn_agent","call_id":"spawn-1","arguments":"{\"agent_type\":\"default\",\"message\":\"Run a command, then finish\"}"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-provider-thread","turnId":"turn-1","item":{"type":"function_call_output","call_id":"spawn-1","output":"{\"agent_id\":\"019ecef1-4a59-7932-8c94-76099197299b\",\"nickname\":\"Bernoulli\"}"}}}`))
	if got := s.parentToolUseForProviderThread("019ecef1-4a59-7932-8c94-76099197299b"); got != "spawn-1" {
		t.Fatalf("parent for raw spawn agent id = %q, want spawn-1", got)
	}

	s.dispatchLine(rawUserSubagentNotificationLineForThread(t, "parent-provider-thread", map[string]any{
		"agent_path": "019ecef1-4a59-7932-8c94-76099197299b",
		"status": map[string]any{
			"completed": "detached child retest finished after bash command",
		},
	}))

	var notif *provider.ProviderEvent
	for i := range events {
		switch events[i].Kind {
		case provider.EventSubagentNotification:
			notif = &events[i]
		case provider.EventUserText:
			if strings.Contains(events[i].Content, "subagent_notification") {
				t.Fatalf("raw user subagent carrier emitted as user text: %+v", events[i])
			}
		}
	}
	if notif == nil {
		t.Fatalf("expected EventSubagentNotification, got %+v", events)
	}
	if notif.ItemID != "spawn-1" {
		t.Fatalf("notification ItemID = %q, want spawn-1", notif.ItemID)
	}
	var meta map[string]any
	if err := json.Unmarshal(notif.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["agent_path"] != "019ecef1-4a59-7932-8c94-76099197299b" {
		t.Fatalf("meta.agent_path = %v, want raw spawned agent id", meta["agent_path"])
	}
	if meta["message"] != "detached child retest finished after bash command" {
		t.Fatalf("meta.message = %v, want child completion message", meta["message"])
	}
}

func TestReadChildThreadMetadataEmitsSpawnMetaUpdate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "cat > /dev/null; sleep 60"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { proc.Close() })

	events := make(chan provider.ProviderEvent, 10)
	s := &Session{
		proc:                   proc,
		ctx:                    ctx,
		threadID:               "parent-thread",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    make(map[string]string),
		childParentByAgentPath: make(map[string]string),
		agentPathByThread:      make(map[string]string),
		agentMetaByThread:      make(map[string]collabReceiverMeta),
		onEvent: func(evt provider.ProviderEvent) {
			events <- evt
		},
		cancel: cancel,
	}

	done := make(chan struct{})
	go func() {
		s.readChildThreadMetadata("child-provider-1", "spawn-1", collabLaunchMeta{
			Prompt:            "Run sleep 3",
			Model:             "gpt-5.5",
			ReasoningEffort:   "low",
			ReceiverThreadIDs: []string{"child-provider-1", "child-provider-2"},
		})
		close(done)
	}()

	pending, rpcID := waitForPending(t, s, 3*time.Second)
	pending <- json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"thread":{"id":"child-provider-1","agentNickname":"Newton","agentRole":"default"}}}`, rpcID))

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("readChildThreadMetadata did not return")
	}

	gotMeta := s.agentMetaByThread["child-provider-1"]
	if gotMeta.ThreadID != "child-provider-1" || gotMeta.AgentNickname != "Newton" || gotMeta.AgentRole != "default" {
		t.Fatalf("agent metadata = %+v, want child-provider-1/Newton/default", gotMeta)
	}

	var evt provider.ProviderEvent
	select {
	case evt = <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for metadata update event")
	}
	if evt.Kind != provider.EventToolStart || evt.ItemID != "spawn-1" || evt.ItemType != "collab_agent" {
		t.Fatalf("event = %+v, want meta update for spawn-1", evt)
	}
	var meta struct {
		MetaUpdateOnly bool `json:"meta_update_only"`
		Input          struct {
			ReceiverThreadIDs []string `json:"receiverThreadIds"`
			NewAgentNickname  string   `json:"newAgentNickname"`
			NewAgentRole      string   `json:"newAgentRole"`
			Prompt            string   `json:"prompt"`
			Model             string   `json:"model"`
			ReasoningEffort   string   `json:"reasoningEffort"`
		} `json:"input"`
	}
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if !meta.MetaUpdateOnly {
		t.Fatal("meta_update_only = false, want true")
	}
	if !reflect.DeepEqual(meta.Input.ReceiverThreadIDs, []string{"child-provider-1", "child-provider-2"}) {
		t.Fatalf("receiverThreadIds = %+v, want full launch receiver list", meta.Input.ReceiverThreadIDs)
	}
	if meta.Input.NewAgentNickname != "Newton" || meta.Input.NewAgentRole != "default" {
		t.Fatalf("agent labels = %q/%q, want Newton/default", meta.Input.NewAgentNickname, meta.Input.NewAgentRole)
	}
	if meta.Input.Prompt != "Run sleep 3" || meta.Input.Model != "gpt-5.5" || meta.Input.ReasoningEffort != "low" {
		t.Fatalf("launch metadata = %q/%q/%q, want Run sleep 3/gpt-5.5/low", meta.Input.Prompt, meta.Input.Model, meta.Input.ReasoningEffort)
	}
}

func TestReadChildThreadMetadataRetriesUntilLabelsArrive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args: []string{"-c", `
			count=0
			while IFS= read -r line; do
				id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
				count=$((count + 1))
				if [ "$count" -eq 1 ]; then
					printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"child-provider-1"}}}\n' "$id"
				else
					printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"child-provider-1","agentNickname":"Curie","agentRole":"default"}}}\n' "$id"
				fi
			done
		`},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	events := make(chan provider.ProviderEvent, 10)
	s := &Session{
		proc:                   proc,
		ctx:                    ctx,
		threadID:               "parent-thread",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    make(map[string]string),
		childParentByAgentPath: make(map[string]string),
		agentPathByThread:      make(map[string]string),
		agentMetaByThread:      make(map[string]collabReceiverMeta),
		collabMetadataReads:    make(chan struct{}, 1),
		onEvent: func(evt provider.ProviderEvent) {
			events <- evt
		},
		cancel: cancel,
	}
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		_ = proc.Close()
	})

	done := make(chan struct{})
	go func() {
		s.readChildThreadMetadata("child-provider-1", "spawn-1", collabLaunchMeta{
			ReceiverThreadIDs: []string{"child-provider-1"},
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("readChildThreadMetadata did not return")
	}

	gotMeta := s.agentMetaByThread["child-provider-1"]
	if gotMeta.ThreadID != "child-provider-1" || gotMeta.AgentNickname != "Curie" || gotMeta.AgentRole != "default" {
		t.Fatalf("agent metadata = %+v, want child-provider-1/Curie/default", gotMeta)
	}
	select {
	case evt := <-events:
		if evt.ItemID != "spawn-1" {
			t.Fatalf("event item id = %q, want spawn-1", evt.ItemID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for metadata update event")
	}
}

func TestReadChildThreadMetadataRequestsNoTurns(t *testing.T) {
	capturePath := t.TempDir() + "/request.json"
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args: []string{"-c", `
			IFS= read -r line || exit 1
			printf '%s\n' "$line" > "$CAPTURE_PATH"
			id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
			printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"child-provider-1","agentNickname":"Noether","agentRole":"default"}}}\n' "$id"
		`},
		Env: map[string]string{"CAPTURE_PATH": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn capture sh: %v", err)
	}
	s := &Session{
		proc:                   proc,
		ctx:                    ctx,
		threadID:               "parent-thread",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    make(map[string]string),
		childParentByAgentPath: make(map[string]string),
		agentPathByThread:      make(map[string]string),
		agentMetaByThread:      make(map[string]collabReceiverMeta),
		collabMetadataReads:    make(chan struct{}, 1),
		onEvent:                func(provider.ProviderEvent) {},
		cancel:                 cancel,
	}
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		_ = proc.Close()
	})

	s.readChildThreadMetadata("child-provider-1", "spawn-1", collabLaunchMeta{
		ReceiverThreadIDs: []string{"child-provider-1"},
	})

	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured request: %v", err)
	}
	var frame struct {
		Method string `json:"method"`
		Params struct {
			ThreadID     string `json:"threadId"`
			IncludeTurns bool   `json:"includeTurns"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal captured request: %v", err)
	}
	var rawFrame map[string]any
	if err := json.Unmarshal(data, &rawFrame); err != nil {
		t.Fatalf("unmarshal raw captured request: %v", err)
	}
	rawParams, ok := rawFrame["params"].(map[string]any)
	if !ok {
		t.Fatalf("params missing from captured request: %s", string(data))
	}
	if _, ok := rawParams["includeTurns"]; !ok {
		t.Fatalf("includeTurns missing from captured request: %s", string(data))
	}
	if frame.Method != "thread/read" {
		t.Fatalf("method = %q, want thread/read", frame.Method)
	}
	if frame.Params.ThreadID != "child-provider-1" {
		t.Fatalf("threadId = %q, want child-provider-1", frame.Params.ThreadID)
	}
	if frame.Params.IncludeTurns {
		t.Fatal("includeTurns = true, want false")
	}
}

func TestDispatchLineTypedWaitCompletionPreservesStartedReceiverTargetsSeparately(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		agentMetaByThread: map[string]collabReceiverMeta{
			"child-provider-1": {ThreadID: "child-provider-1", AgentNickname: "Hypatia", AgentRole: "default"},
			"child-provider-2": {ThreadID: "child-provider-2", AgentNickname: "Parfit", AgentRole: "default"},
			"child-provider-3": {ThreadID: "child-provider-3", AgentNickname: "Ada", AgentRole: "default"},
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"id":"wait-1","type":"collabAgentToolCall","tool":"wait","receiverThreadIds":["child-provider-1","child-provider-2","child-provider-3"],"agentsStates":{},"status":"inProgress"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"id":"wait-1","type":"collabAgentToolCall","tool":"wait","receiverThreadIds":["child-provider-1"],"agentsStates":{"child-provider-1":{"status":"completed","message":"done"}},"status":"completed"}}}`))

	var waitEvent *provider.ProviderEvent
	for i := range events {
		if events[i].ItemType == "wait_agent" && events[i].Kind == provider.EventToolComplete {
			waitEvent = &events[i]
		}
	}
	if waitEvent == nil {
		t.Fatalf("wait_agent completion missing: %+v", events)
	}
	var meta struct {
		Input struct {
			ReceiverThreadIDs          []string             `json:"receiverThreadIds"`
			RequestedReceiverThreadIDs []string             `json:"requestedReceiverThreadIds"`
			ReceiverAgents             []collabReceiverMeta `json:"receiverAgents"`
			RequestedReceiverAgents    []collabReceiverMeta `json:"requestedReceiverAgents"`
			AgentsStates               map[string]struct {
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"agentsStates"`
		} `json:"input"`
	}
	if err := json.Unmarshal(waitEvent.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if want := []string{"child-provider-1"}; !reflect.DeepEqual(meta.Input.ReceiverThreadIDs, want) {
		t.Fatalf("receiverThreadIds = %+v, want completion statuses %+v", meta.Input.ReceiverThreadIDs, want)
	}
	wantRequested := []string{"child-provider-1", "child-provider-2", "child-provider-3"}
	if !reflect.DeepEqual(meta.Input.RequestedReceiverThreadIDs, wantRequested) {
		t.Fatalf("requestedReceiverThreadIds = %+v, want wait-start targets %+v", meta.Input.RequestedReceiverThreadIDs, wantRequested)
	}
	wantAgents := []collabReceiverMeta{
		{ThreadID: "child-provider-1", AgentNickname: "Hypatia", AgentRole: "default"},
	}
	if !reflect.DeepEqual(meta.Input.ReceiverAgents, wantAgents) {
		t.Fatalf("receiverAgents = %+v, want %+v", meta.Input.ReceiverAgents, wantAgents)
	}
	wantRequestedAgents := []collabReceiverMeta{
		{ThreadID: "child-provider-1", AgentNickname: "Hypatia", AgentRole: "default"},
		{ThreadID: "child-provider-2", AgentNickname: "Parfit", AgentRole: "default"},
		{ThreadID: "child-provider-3", AgentNickname: "Ada", AgentRole: "default"},
	}
	if !reflect.DeepEqual(meta.Input.RequestedReceiverAgents, wantRequestedAgents) {
		t.Fatalf("requestedReceiverAgents = %+v, want %+v", meta.Input.RequestedReceiverAgents, wantRequestedAgents)
	}
	if len(meta.Input.AgentsStates) != 1 || meta.Input.AgentsStates["child-provider-1"].Status != "completed" {
		t.Fatalf("agentsStates = %+v, want only completed child state", meta.Input.AgentsStates)
	}
}

func TestDispatchLineCloseAgentKeepsOwnItemID(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"provider-parent","item":{"id":"close-call-1","type":"collabAgentToolCall","tool":"closeAgent","receiverThreadIds":["child-provider-1"],"status":"completed"}}}`))

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].ItemID != "close-call-1" {
		t.Fatalf("ItemID: got %q, want %q", events[0].ItemID, "close-call-1")
	}
}
