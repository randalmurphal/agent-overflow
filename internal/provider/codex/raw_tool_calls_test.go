package codex

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func TestDispatchLineRawSpawnOutputLabelsLaterWaitAgent(t *testing.T) {
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

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"spawn_agent","call_id":"spawn-1","arguments":"{\"agent_type\":\"explorer\",\"message\":\"Inspect parser\"}"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"id":"spawn-1","type":"collabAgentToolCall","tool":"spawnAgent","receiverThreadIds":["child-provider-1"],"prompt":"Inspect parser","status":"completed"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call_output","call_id":"spawn-1","output":"{\"agent_id\":\"child-provider-1\",\"nickname\":\"Boyle\"}"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"wait_agent","call_id":"wait-1","arguments":"{\"targets\":[\"child-provider-1\"],\"timeout_ms\":10000}"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"id":"wait-1","type":"collabAgentToolCall","tool":"wait","receiverThreadIds":["child-provider-1"],"status":"inProgress"}}}`))

	var waitEvent *provider.ProviderEvent
	for i := range events {
		if events[i].ItemType == "wait_agent" && events[i].Kind == provider.EventToolStart {
			waitEvent = &events[i]
		}
	}
	if waitEvent == nil {
		t.Fatalf("wait_agent event missing: %+v", events)
	}
	var meta struct {
		Input struct {
			ReceiverThreadIDs []string             `json:"receiverThreadIds"`
			ReceiverAgents    []collabReceiverMeta `json:"receiverAgents"`
		} `json:"input"`
	}
	if err := json.Unmarshal(waitEvent.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if len(meta.Input.ReceiverThreadIDs) != 1 || meta.Input.ReceiverThreadIDs[0] != "child-provider-1" {
		t.Fatalf("receiverThreadIds = %+v, want child-provider-1", meta.Input.ReceiverThreadIDs)
	}
	if len(meta.Input.ReceiverAgents) != 1 {
		t.Fatalf("receiverAgents = %+v, want one", meta.Input.ReceiverAgents)
	}
	got := meta.Input.ReceiverAgents[0]
	if got.ThreadID != "child-provider-1" || got.AgentNickname != "Boyle" || got.AgentRole != "explorer" {
		t.Fatalf("receiver metadata = %+v, want child-provider-1/Boyle/explorer", got)
	}
}

func TestDispatchLineRawWaitCallPreservesRequestedReceiversOnTimeoutCompletion(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:         "parent-thread",
		pending:          make(map[int64]chan json.RawMessage),
		rawToolCallsByID: make(map[string]rawToolCall),
		agentMetaByThread: map[string]collabReceiverMeta{
			"child-provider-1": {ThreadID: "child-provider-1", AgentNickname: "Hypatia", AgentRole: "default"},
			"child-provider-2": {ThreadID: "child-provider-2", AgentNickname: "Parfit", AgentRole: "default"},
			"child-provider-3": {ThreadID: "child-provider-3", AgentNickname: "Ada", AgentRole: "default"},
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"wait_agent","call_id":"wait-1","arguments":"{\"targets\":[\"child-provider-1\"],\"timeout_ms\":10000}"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"id":"wait-1","type":"collabAgentToolCall","tool":"wait","receiverThreadIds":[],"agentsStates":{},"status":"completed"}}}`))

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
			RequestedReceiverAgents    []collabReceiverMeta `json:"requestedReceiverAgents"`
		} `json:"input"`
	}
	if err := json.Unmarshal(waitEvent.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if len(meta.Input.ReceiverThreadIDs) != 0 {
		t.Fatalf("receiverThreadIds = %+v, want no timeout completions", meta.Input.ReceiverThreadIDs)
	}
	if want := []string{"child-provider-1"}; !reflect.DeepEqual(meta.Input.RequestedReceiverThreadIDs, want) {
		t.Fatalf("requestedReceiverThreadIds = %+v, want raw target preserved", meta.Input.RequestedReceiverThreadIDs)
	}
	wantAgents := []collabReceiverMeta{
		{ThreadID: "child-provider-1", AgentNickname: "Hypatia", AgentRole: "default"},
	}
	if !reflect.DeepEqual(meta.Input.RequestedReceiverAgents, wantAgents) {
		t.Fatalf("requestedReceiverAgents = %+v, want %+v", meta.Input.RequestedReceiverAgents, wantAgents)
	}
}

func TestDispatchLineRawWaitCallPreservesAllReceiversSeparatelyOnPartialCompletion(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:         "parent-thread",
		pending:          make(map[int64]chan json.RawMessage),
		rawToolCallsByID: make(map[string]rawToolCall),
		agentMetaByThread: map[string]collabReceiverMeta{
			"child-provider-1": {ThreadID: "child-provider-1", AgentNickname: "Hypatia", AgentRole: "default"},
			"child-provider-2": {ThreadID: "child-provider-2", AgentNickname: "Parfit", AgentRole: "default"},
			"child-provider-3": {ThreadID: "child-provider-3", AgentNickname: "Ada", AgentRole: "default"},
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"wait_agent","call_id":"wait-1","arguments":"{\"targets\":[\"child-provider-1\",\"child-provider-2\",\"child-provider-3\"],\"timeout_ms\":10000}"}}}`))
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
		t.Fatalf("requestedReceiverThreadIds = %+v, want raw wait targets %+v", meta.Input.RequestedReceiverThreadIDs, wantRequested)
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
}

func TestRawWriteStdinWaitResultIgnoresSpoofedCommandOutput(t *testing.T) {
	output := "Chunk ID: abc\nWall time: 0.1000 seconds\nOutput:\nProcess exited with code 0\n"
	if got := rawWriteStdinWaitResult(output); got != "" {
		t.Fatalf("rawWriteStdinWaitResult spoofed output = %q, want empty", got)
	}

	output = "Chunk ID: abc\nWall time: 0.1000 seconds\nProcess exited with code 0\nOutput:\n"
	if got := rawWriteStdinWaitResult(output); got != terminalWaitResultExited {
		t.Fatalf("rawWriteStdinWaitResult header = %q, want exited", got)
	}
}

func TestDispatchLineRawToolCallsAreBoundedAndCleared(t *testing.T) {
	s := &Session{
		threadID:         "parent-thread",
		pending:          make(map[int64]chan json.RawMessage),
		rawToolCallsByID: make(map[string]rawToolCall),
		onEvent:          func(provider.ProviderEvent) {},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"unrelated","call_id":"ignored-1","arguments":"{}"}}}`))
	if len(s.rawToolCallsByID) != 0 {
		t.Fatalf("unrelated raw call retained: %+v", s.rawToolCallsByID)
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"write_stdin","call_id":"wait-1","arguments":"{\"session_id\":\"pid-42\",\"chars\":\"\"}"}}}`))
	if len(s.rawToolCallsByID) != 1 {
		t.Fatalf("write_stdin raw call count = %d, want 1", len(s.rawToolCallsByID))
	}
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call_output","call_id":"wait-1","output":"Process running with session ID pid-42\nOutput:\n"}}}`))
	if len(s.rawToolCallsByID) != 0 {
		t.Fatalf("raw call not cleared after output: %+v", s.rawToolCallsByID)
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"wait_agent","call_id":"wait-agent-1","arguments":"{\"targets\":[\"child-provider-1\"]}"}}}`))
	if len(s.rawToolCallsByID) != 1 {
		t.Fatalf("wait_agent raw call count = %d, want 1", len(s.rawToolCallsByID))
	}
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"parent-thread","turn":{"id":"turn-1","status":"completed"}}}`))
	if len(s.rawToolCallsByID) != 0 {
		t.Fatalf("raw calls not cleared on turn complete: %+v", s.rawToolCallsByID)
	}
}

func TestDispatchLineRawExecCommandOutputEmitsModelResult(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:         "parent-thread",
		pending:          make(map[int64]chan json.RawMessage),
		rawToolCallsByID: make(map[string]rawToolCall),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"exec_command","call_id":"cmd-1","arguments":"{\"cmd\":\"sleep 10\"}"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call_output","call_id":"cmd-1","output":"Chunk ID: abc\nWall time: 1.0000 seconds\nProcess running with session ID 17313\nOutput:\n"}}}`))

	if len(events) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(events), events)
	}
	evt := events[0]
	if evt.Kind != provider.EventCodexExecResult {
		t.Fatalf("event kind = %q, want %q", evt.Kind, provider.EventCodexExecResult)
	}
	if evt.ThreadID != "parent-thread" || evt.TurnID != "turn-1" || evt.ItemID != "cmd-1" {
		t.Fatalf("event routing = thread %q turn %q item %q", evt.ThreadID, evt.TurnID, evt.ItemID)
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["result"] != terminalWaitResultRunning {
		t.Fatalf("meta.result = %v, want %q", meta["result"], terminalWaitResultRunning)
	}
	if meta["process_id"] != "17313" {
		t.Fatalf("meta.process_id = %v, want 17313", meta["process_id"])
	}
	if meta["command"] != "sleep 10" {
		t.Fatalf("meta.command = %v, want sleep 10", meta["command"])
	}
	if len(s.rawToolCallsByID) != 0 {
		t.Fatalf("raw exec call not cleared after output: %+v", s.rawToolCallsByID)
	}
}

func TestCodexProviderEventLogRedactorRedactsWriteStdinEvents(t *testing.T) {
	redact := newCodexProviderEventLogRedactor()

	rawCall := []byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","item":{"type":"function_call","name":"write_stdin","call_id":"wait-1","arguments":"{\"session_id\":\"pid-42\",\"chars\":\"secret-token\\n\",\"yield_time_ms\":1000}"}}}`)
	redactedCall := string(redact("in", rawCall))
	if strings.Contains(redactedCall, "secret-token") {
		t.Fatalf("write_stdin arguments were not redacted: %s", redactedCall)
	}
	if !strings.Contains(redactedCall, "[redacted]") || !strings.Contains(redactedCall, "pid-42") {
		t.Fatalf("redacted write_stdin call lost expected fields: %s", redactedCall)
	}

	rawOutput := []byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","item":{"type":"function_call_output","call_id":"wait-1","output":"secret command output"}}}`)
	redactedOutput := string(redact("in", rawOutput))
	if strings.Contains(redactedOutput, "secret command output") {
		t.Fatalf("write_stdin output was not redacted: %s", redactedOutput)
	}
	if !strings.Contains(redactedOutput, "[redacted]") {
		t.Fatalf("redacted write_stdin output missing marker: %s", redactedOutput)
	}

	unrelatedOutput := []byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","item":{"type":"function_call_output","call_id":"other-1","output":"visible output"}}}`)
	if got := string(redact("in", unrelatedOutput)); !strings.Contains(got, "visible output") {
		t.Fatalf("unrelated output should not be redacted: %s", got)
	}

	typedInteraction := []byte(`{"jsonrpc":"2.0","method":"item/commandExecution/terminalInteraction","params":{"threadId":"parent-thread","turnId":"turn-1","itemId":"cmd-1","processId":"pid-42","stdin":"secret-token\n"}}`)
	redactedTyped := string(redact("in", typedInteraction))
	if strings.Contains(redactedTyped, "secret-token") {
		t.Fatalf("typed terminal interaction stdin was not redacted: %s", redactedTyped)
	}
	if !strings.Contains(redactedTyped, "[redacted]") || !strings.Contains(redactedTyped, "pid-42") {
		t.Fatalf("redacted typed terminal interaction lost expected fields: %s", redactedTyped)
	}

	emptyTypedInteraction := []byte(`{"jsonrpc":"2.0","method":"item/commandExecution/terminalInteraction","params":{"threadId":"parent-thread","turnId":"turn-1","itemId":"cmd-1","processId":"pid-42","stdin":""}}`)
	if got := string(redact("in", emptyTypedInteraction)); strings.Contains(got, "[redacted]") {
		t.Fatalf("empty typed terminal interaction should not be redacted: %s", got)
	}
}

func TestCodexProviderEventLogRedactorRedactsEncryptedCollaborationMessages(t *testing.T) {
	redact := newCodexProviderEventLogRedactor()
	for _, line := range [][]byte{
		[]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"item":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","call_id":"spawn-1","arguments":"{\"task_name\":\"reviewer\",\"message\":\"gAAAA-spawn\"}"}}}`),
		[]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"item":{"type":"function_call","namespace":"collaboration","name":"send_message","call_id":"send-1","arguments":"{\"target\":\"/root/reviewer\",\"message\":\"gAAAA-send\"}"}}}`),
		[]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"item":{"type":"function_call","namespace":"collaboration","name":"followup_task","call_id":"followup-1","arguments":"{\"target\":\"/root/reviewer\",\"message\":\"gAAAA-followup\"}"}}}`),
		[]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"item":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","call_id":"malformed-1","arguments":"malformed-gAAAA"}}}`),
	} {
		got := redact("in", line)
		if strings.Contains(string(got), "gAAAA") {
			t.Fatalf("encrypted collaboration message survived redaction: %s", got)
		}
		if !strings.Contains(string(got), `[redacted]`) {
			t.Fatalf("redaction marker missing: %s", got)
		}
	}
}
