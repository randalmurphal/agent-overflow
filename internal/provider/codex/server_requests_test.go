package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestCodexApprovalWaitsForUserResponseWithoutTimeout(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":42,"method":"item/commandExecution/requestApproval","params":{"command":"ls"}}`)
	s.dispatchLine(line)

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}

	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case evt := <-eventCh:
			switch evt.Kind {
			case provider.EventError, provider.EventApprovalResolved:
				t.Fatalf("pending approval resolved without user action: %+v", evt)
			}
		case <-deadline:
			if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
				RequestID: "42",
				Decision:  "allow",
			}); err != nil {
				t.Fatalf("RespondToApproval after waiting: %v", err)
			}
			return
		}
	}
}

func TestCodexUserInputWaitsForUserResponseWithoutTimeout(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":43,"method":"item/tool/requestUserInput","params":{"questions":[{"id":"scope","header":"Scope","question":"Choose","options":[{"label":"turn","description":"This turn"}]}]}}`)
	s.dispatchLine(line)

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventUserInputRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventUserInputRequest)
	}

	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case evt := <-eventCh:
			switch evt.Kind {
			case provider.EventError, provider.EventUserInputResolved, provider.EventApprovalResolved:
				t.Fatalf("pending user input resolved without user action: %+v", evt)
			}
		case <-deadline:
			err := s.RespondToUserInput(context.Background(), provider.UserInputResponse{
				RequestID: "43",
				Decision:  "accept",
				Answers: map[string]provider.UserInputAnswer{
					"scope": provider.SingleUserInputAnswer("turn"),
				},
			})
			if err != nil {
				t.Fatalf("RespondToUserInput after waiting: %v", err)
			}
			return
		}
	}
}

func TestCodexRejectsRequestUserInputWithoutQuestions(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":44,"method":"item/tool/requestUserInput","params":{"questions":[]}}`)
	s.dispatchLine(line)

	select {
	case evt := <-eventCh:
		if evt.Kind == provider.EventUserInputRequest {
			t.Fatalf("empty requestUserInput emitted user-input request: %+v", evt)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCodexHandleServerRequestApproval(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	// Write an approval server request through cat.
	line := []byte(`{"jsonrpc":"2.0","id":1,"method":"item/commandExecution/requestApproval","params":{"command":"rm -rf /"}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(evt.Meta, &approval); err != nil {
		t.Fatalf("unmarshal approval: %v", err)
	}
	if approval.RequestID != "1" {
		t.Errorf("requestID: got %q, want %q", approval.RequestID, "1")
	}
	if approval.ToolName != "command" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "command")
	}
}

func TestCodexHandleServerRequestFileApproval(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":2,"method":"item/fileChange/requestApproval","params":{"filePath":"/tmp/test.go"}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}
}

func TestCodexHandleServerRequestUserInput(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":3,"method":"item/tool/requestUserInput","params":{"turn":{"id":"turn-3"},"item":{"id":"item-8"},"questions":[{"id":"scope","header":"Scope","question":"Choose a scope","options":[{"label":"turn","description":"Apply only to this turn"},{"label":"session","description":"Apply for the whole session"}],"multiSelect":false}]}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	start := codexWaitEvent(t, eventCh)
	if start.Kind != provider.EventToolStart {
		t.Fatalf("start kind: got %q, want %q", start.Kind, provider.EventToolStart)
	}
	if start.ItemType != "request_user_input" {
		t.Fatalf("start itemType: got %q, want request_user_input", start.ItemType)
	}
	if start.ItemID != "item-8" {
		t.Errorf("start itemID: got %q, want %q", start.ItemID, "item-8")
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventUserInputRequest {
		t.Fatalf("request kind: got %q, want %q", evt.Kind, provider.EventUserInputRequest)
	}
	if evt.TurnID != "turn-3" {
		t.Errorf("turnID: got %q, want %q", evt.TurnID, "turn-3")
	}
	if evt.ItemID != "item-8" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "item-8")
	}

	var request provider.UserInputRequest
	if err := json.Unmarshal(evt.Meta, &request); err != nil {
		t.Fatalf("unmarshal user input request: %v", err)
	}
	if len(request.Questions) != 1 {
		t.Fatalf("questions len: got %d, want 1", len(request.Questions))
	}
	if request.ToolUseID != "item-8" {
		t.Errorf("toolUseID: got %q, want item-8", request.ToolUseID)
	}
	if request.Questions[0].ID != "scope" {
		t.Errorf("question id: got %q, want %q", request.Questions[0].ID, "scope")
	}
}

func TestCodexHandleServerRequestUserInputV2TopLevelRouteFields(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      31,
		"method":  "item/tool/requestUserInput",
		"params": map[string]any{
			"threadId": s.rootThreadID(),
			"turnId":   "turn-31",
			"itemId":   "item-31",
			"questions": []map[string]any{{
				"id":       "scope",
				"header":   "Scope",
				"question": "Choose a scope",
				"isOther":  true,
				"isSecret": false,
				"options": []map[string]string{
					{"label": "turn", "description": "Apply only to this turn"},
					{"label": "session", "description": "Apply for the whole session"},
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	start := codexWaitEvent(t, eventCh)
	if start.Kind != provider.EventToolStart {
		t.Fatalf("start kind: got %q, want %q", start.Kind, provider.EventToolStart)
	}
	if start.ItemID != "item-31" {
		t.Errorf("start itemID: got %q, want item-31", start.ItemID)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventUserInputRequest {
		t.Fatalf("request kind: got %q, want %q", evt.Kind, provider.EventUserInputRequest)
	}
	if evt.TurnID != "turn-31" {
		t.Errorf("turnID: got %q, want %q", evt.TurnID, "turn-31")
	}
	if evt.ItemID != "item-31" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "item-31")
	}

	var request provider.UserInputRequest
	if err := json.Unmarshal(evt.Meta, &request); err != nil {
		t.Fatalf("unmarshal user input request: %v", err)
	}
	if request.ThreadID != testThread {
		t.Errorf("threadID: got %q, want %q", request.ThreadID, testThread)
	}
	if request.TurnID != "turn-31" {
		t.Errorf("request turnID: got %q, want %q", request.TurnID, "turn-31")
	}
	if request.ToolUseID != "item-31" {
		t.Errorf("request toolUseID: got %q, want item-31", request.ToolUseID)
	}
	if got := len(request.Questions); got != 1 {
		t.Fatalf("questions len: got %d, want 1", got)
	}
	options := request.Questions[0].Options
	if got := len(options); got != 2 {
		t.Fatalf("options len: got %d, want 2", got)
	}
	if options[1].Label != "session" {
		t.Errorf("second option label: got %q, want session", options[1].Label)
	}
}

func TestCodexHandleServerRequestPermission(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":4,"method":"item/permissions/requestApproval","params":{"turnId":"turn-4","itemId":"item-9","reason":"Need broader write access","permissions":{"network":{"enabled":true},"fileSystem":{"read":["/tmp/project/src"],"write":["/tmp/project/out"]}}}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}
	if evt.TurnID != "turn-4" {
		t.Errorf("turnID: got %q, want %q", evt.TurnID, "turn-4")
	}
	if evt.ItemID != "item-9" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "item-9")
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(evt.Meta, &approval); err != nil {
		t.Fatalf("unmarshal approval: %v", err)
	}
	if approval.Kind != "permission" {
		t.Errorf("kind: got %q, want %q", approval.Kind, "permission")
	}
	if approval.Permissions == nil || approval.Permissions.FileSystem == nil {
		t.Fatal("expected filesystem permissions to be populated")
	}
	if approval.Permissions.FileSystem.Read[0] != "/tmp/project/src" {
		t.Errorf("fileSystem.read[0]: got %q, want %q", approval.Permissions.FileSystem.Read[0], "/tmp/project/src")
	}
}

func TestCodexPermissionApprovalWaitsForUserResponseWithoutTimeout(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":24,"method":"item/permissions/requestApproval","params":{"turnId":"turn-24","itemId":"item-24","reason":"Need broader write access","permissions":{"fileSystem":{"read":["/tmp/project/src"]}}}}`)
	s.dispatchLine(line)

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}

	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case evt := <-eventCh:
			switch evt.Kind {
			case provider.EventError, provider.EventApprovalResolved:
				t.Fatalf("pending permission approval resolved without user action: %+v", evt)
			}
		case <-deadline:
			if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
				RequestID: "24",
				Decision:  "allow",
			}); err != nil {
				t.Fatalf("RespondToApproval after waiting: %v", err)
			}
			if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
				RequestID: "24",
				Decision:  "allow",
			}); !errors.Is(err, provider.ErrStaleInteractiveRequest) {
				t.Fatalf("second RespondToApproval error = %v, want ErrStaleInteractiveRequest", err)
			}
			return
		}
	}
}

func TestCodexHandleServerRequestUnknown(t *testing.T) {
	s, _ := newTestCodexSession(t)

	// Unknown server request — should send error response. With cat,
	// the error response echoes back and readLoop sees it as a stray
	// response (no matching pending id) and logs-and-drops. We just
	// verify no crash. Drive a synchronous observation instead of the
	// former fixed-200ms sleep: write the request, then invoke
	// dispatchLine directly on the same line so its decode path runs
	// in the test goroutine and any panic or error surfaces
	// immediately.
	line := []byte(`{"jsonrpc":"2.0","id":1,"method":"unknown/request","params":{}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.dispatchLine(line)
}

// -- SetDynamicToolHandler / handleDynamicToolCall tests --

func TestSetDynamicToolHandler(t *testing.T) {
	s, _ := newTestCodexSession(t)

	called := false
	handler := func(toolName string, args map[string]any) (string, bool, error) {
		called = true
		return "result", true, nil
	}

	s.SetDynamicToolHandler(handler)

	s.mu.Lock()
	h := s.dynamicToolHandler
	s.mu.Unlock()
	if h == nil {
		t.Fatal("expected non-nil handler after SetDynamicToolHandler")
	}

	// Invoke the handler to confirm it's the right one.
	_, _, _ = h("test", nil)
	if !called {
		t.Error("expected handler to be called")
	}
}

func TestSetDynamicToolHandlerNil(t *testing.T) {
	s, _ := newTestCodexSession(t)

	s.SetDynamicToolHandler(func(string, map[string]any) (string, bool, error) {
		return "", false, nil
	})
	s.SetDynamicToolHandler(nil)

	s.mu.Lock()
	h := s.dynamicToolHandler
	s.mu.Unlock()
	if h != nil {
		t.Error("expected nil handler after SetDynamicToolHandler(nil)")
	}
}

func TestHandleDynamicToolCall(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	resultCh := make(chan string, 1)
	s.SetDynamicToolHandler(func(toolName string, args map[string]any) (string, bool, error) {
		resultCh <- toolName
		return "tool output", true, nil
	})

	line := []byte(`{"jsonrpc":"2.0","id":10,"method":"item/tool/call","params":{"tool":"my_tool","arguments":{"key":"value"}}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The handler runs asynchronously. Wait for it.
	select {
	case name := <-resultCh:
		if name != "my_tool" {
			t.Errorf("toolName: got %q, want %q", name, "my_tool")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for dynamic tool handler invocation")
	}

	// Drain any events from the echo.
	_ = eventCh
}

func TestHandleDynamicToolCallToolNameField(t *testing.T) {
	s, _ := newTestCodexSession(t)

	resultCh := make(chan string, 1)
	s.SetDynamicToolHandler(func(toolName string, args map[string]any) (string, bool, error) {
		resultCh <- toolName
		return "ok", true, nil
	})

	// Use "toolName" field instead of "tool".
	line := []byte(`{"jsonrpc":"2.0","id":11,"method":"dynamicToolCall","params":{"toolName":"alt_tool","arguments":{}}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case name := <-resultCh:
		if name != "alt_tool" {
			t.Errorf("toolName: got %q, want %q", name, "alt_tool")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for dynamic tool handler invocation")
	}
}

func TestHandleDynamicToolCallHandlerError(t *testing.T) {
	s, _ := newTestCodexSession(t)

	doneCh := make(chan struct{}, 1)
	s.SetDynamicToolHandler(func(toolName string, args map[string]any) (string, bool, error) {
		defer func() { doneCh <- struct{}{} }()
		return "", false, fmt.Errorf("simulated tool error")
	})

	line := []byte(`{"jsonrpc":"2.0","id":12,"method":"item/tool/call","params":{"tool":"fail_tool","arguments":{}}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case <-doneCh:
		// Handler ran and returned error -- the session formats it as "Error: ..."
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for handler to complete")
	}
}

func TestHandleDynamicToolCallNilArguments(t *testing.T) {
	s, _ := newTestCodexSession(t)

	resultCh := make(chan map[string]any, 1)
	s.SetDynamicToolHandler(func(toolName string, args map[string]any) (string, bool, error) {
		resultCh <- args
		return "ok", true, nil
	})

	// No "arguments" field in params -- should default to empty map.
	line := []byte(`{"jsonrpc":"2.0","id":13,"method":"item/tool/call","params":{"tool":"noargs_tool"}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case args := <-resultCh:
		if args == nil {
			t.Error("expected non-nil args map")
		}
		if len(args) != 0 {
			t.Errorf("expected empty args, got %v", args)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for handler")
	}
}

func TestHandleDynamicToolCallNoHandler(t *testing.T) {
	s, _ := newTestCodexSession(t)
	// No handler set -- should send error response.

	line := []byte(`{"jsonrpc":"2.0","id":14,"method":"item/tool/call","params":{"tool":"orphan_tool"}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Drive dispatchLine in the test goroutine so the decode path
	// runs synchronously — former 200ms sleep just hid the fact that
	// there is no readLoop assertion this test makes beyond "it did
	// not crash."
	s.dispatchLine(line)
}

// -- handleServerRequest: elicitation branch --

func TestCodexHandleServerRequestElicitation(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":5,"method":"mcpServer/elicitation/request","params":{"serverName":"my-mcp","message":"Please authorize","requestedSchema":{"type":"string"}}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(evt.Meta, &approval); err != nil {
		t.Fatalf("unmarshal approval: %v", err)
	}
	if approval.Kind != "mcp-elicitation" {
		t.Errorf("kind: got %q, want %q", approval.Kind, "mcp-elicitation")
	}
	if approval.RequestID != "5" {
		t.Errorf("requestID: got %q, want %q", approval.RequestID, "5")
	}
	if approval.Description != "Please authorize" {
		t.Errorf("description: got %q, want %q", approval.Description, "Please authorize")
	}
}

func TestCodexMcpElicitationWaitsForUserResponseWithoutTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `printf '%s\n' '{"jsonrpc":"2.0","id":5,"method":"mcpServer/elicitation/request","params":{"serverName":"my-mcp","message":"Authorize"}}'; while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; done`},
		Env: map[string]string{
			"CAPTURE": capturePath,
		},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 10)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel: cancel,
	}
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		proc.Close()
	})

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalRequest {
				goto waitWithoutResponse
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for MCP elicitation request")
		}
	}

waitWithoutResponse:
	time.Sleep(200 * time.Millisecond)
	captured, err := os.ReadFile(capturePath)
	if err == nil && len(captured) > 0 {
		t.Fatalf("MCP elicitation wrote response without user action: %s", captured)
	}

	err = s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "5",
		Elicitation: &provider.ElicitationResolution{
			Action:  "confirm",
			Content: json.RawMessage(`{"accepted":true}`),
		},
	})
	if err != nil {
		t.Fatalf("RespondToApproval after waiting: %v", err)
	}

	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		captured, err = os.ReadFile(capturePath)
		if err == nil && len(captured) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(captured) == 0 {
		t.Fatalf("MCP elicitation response was not written: %v", err)
	}
	var frame struct {
		ID     int64 `json:"id"`
		Result struct {
			Action string `json:"action"`
		} `json:"result"`
	}
	if err := json.Unmarshal(captured, &frame); err != nil {
		t.Fatalf("unmarshal captured response: %v (data=%s)", err, captured)
	}
	if frame.ID != 5 {
		t.Fatalf("id = %d, want 5", frame.ID)
	}
	if frame.Result.Action != "confirm" {
		t.Fatalf("action = %q, want confirm", frame.Result.Action)
	}
}

// -- handleServerRequest: legacy approval methods --

func TestCodexHandleServerRequestApplyPatchApproval(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":6,"method":"applyPatchApproval","params":{"filePath":"/tmp/foo.go"}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(evt.Meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.Kind != "file-change" {
		t.Errorf("kind: got %q, want %q", approval.Kind, "file-change")
	}
}

func TestCodexHandleServerRequestExecCommandApproval(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":7,"method":"execCommandApproval","params":{"command":"pnpm test"}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(evt.Meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.Kind != "command" {
		t.Errorf("kind: got %q, want %q", approval.Kind, "command")
	}
	if approval.ToolName != "command" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "command")
	}
}

// -- handleServerRequest: file read approval --

func TestCodexHandleServerRequestFileReadApproval(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":8,"method":"item/fileRead/requestApproval","params":{"filePath":"/etc/passwd"}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(evt.Meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.Kind != "file-read" {
		t.Errorf("kind: got %q, want %q", approval.Kind, "file-read")
	}
	if approval.ToolName != "file_read" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "file_read")
	}
}
