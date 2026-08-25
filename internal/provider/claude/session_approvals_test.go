package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestRespondToApprovalAllowFormat(t *testing.T) {
	resp := provider.ApprovalResponse{RequestID: "req-1", Decision: "allow"}

	var behavior map[string]any
	if resp.Decision == "allow" || resp.Decision == "allow_session" {
		behavior = map[string]any{"behavior": "allow"}
	} else {
		behavior = map[string]any{"behavior": "deny", "message": "User denied"}
	}
	msg := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": resp.RequestID,
			"response":   behavior,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]json.RawMessage
	json.Unmarshal(data, &parsed)

	var msgType string
	json.Unmarshal(parsed["type"], &msgType)
	if msgType != "control_response" {
		t.Errorf("type: got %q, want %q", msgType, "control_response")
	}

	var response struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Response  struct {
			Behavior string `json:"behavior"`
		} `json:"response"`
	}
	json.Unmarshal(parsed["response"], &response)
	if response.Subtype != "success" {
		t.Errorf("subtype: got %q, want %q", response.Subtype, "success")
	}
	if response.RequestID != "req-1" {
		t.Errorf("request_id: got %q, want %q", response.RequestID, "req-1")
	}
	if response.Response.Behavior != "allow" {
		t.Errorf("behavior: got %q, want %q", response.Response.Behavior, "allow")
	}
}

func TestRespondToApprovalDenyFormat(t *testing.T) {
	resp := provider.ApprovalResponse{RequestID: "req-2", Decision: "deny"}

	var behavior map[string]any
	if resp.Decision == "allow" || resp.Decision == "allow_session" {
		behavior = map[string]any{"behavior": "allow"}
	} else {
		behavior = map[string]any{"behavior": "deny", "message": "User denied"}
	}
	msg := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": resp.RequestID,
			"response":   behavior,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]json.RawMessage
	json.Unmarshal(data, &parsed)

	var response struct {
		Response struct {
			Behavior string `json:"behavior"`
			Message  string `json:"message"`
		} `json:"response"`
	}
	json.Unmarshal(parsed["response"], &response)
	if response.Response.Behavior != "deny" {
		t.Errorf("behavior: got %q, want %q", response.Response.Behavior, "deny")
	}
	if response.Response.Message != "User denied" {
		t.Errorf("message: got %q, want %q", response.Response.Message, "User denied")
	}
}

func TestFullAccessToolRequestDoesNotAutoApproveInPlanMode(t *testing.T) {
	s := &Session{
		basePermissionMode:    "bypassPermissions",
		currentPermissionMode: "plan",
		interactionMode:       provider.ModePlan,
	}
	handled, err := s.maybeHandleFullAccessToolRequest([]byte(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`))
	if err != nil {
		t.Fatalf("maybeHandleFullAccessToolRequest: %v", err)
	}
	if handled {
		t.Fatal("plan-mode session auto-approved full-access tool request")
	}
}

func TestFullAccessToolRequestAutoApprovesRegularTools(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; done`},
		Env: map[string]string{
			"CAPTURE": capturePath,
		},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	defer cancel()
	t.Cleanup(func() { _ = proc.Close() })

	s := &Session{
		proc:                  proc,
		currentPermissionMode: "bypassPermissions",
	}
	handled, err := s.maybeHandleFullAccessToolRequest([]byte(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`))
	if err != nil {
		t.Fatalf("maybeHandleFullAccessToolRequest: %v", err)
	}
	if !handled {
		t.Fatal("full-access regular tool request was not auto-approved")
	}
	lines := waitCapturedLines(t, capturePath, 1)
	var response struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Response  struct {
				Behavior string `json:"behavior"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &response); err != nil {
		t.Fatalf("unmarshal auto-approval response: %v", err)
	}
	if response.Type != "control_response" ||
		response.Response.Subtype != "success" ||
		response.Response.RequestID != "req-1" ||
		response.Response.Response.Behavior != "allow" {
		t.Fatalf("auto-approval response = %+v, want allow for req-1", response)
	}
}

func TestFullAccessToolRequestLeavesInteractiveExceptionsPending(t *testing.T) {
	for _, toolName := range []string{"AskUserQuestion", "ExitPlanMode"} {
		t.Run(toolName, func(t *testing.T) {
			s := &Session{currentPermissionMode: "bypassPermissions"}
			line := []byte(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"` + toolName + `"}}`)
			handled, err := s.maybeHandleFullAccessToolRequest(line)
			if err != nil {
				t.Fatalf("maybeHandleFullAccessToolRequest: %v", err)
			}
			if handled {
				t.Fatalf("%s should remain interactive in full-access mode", toolName)
			}
		})
	}
}

func TestSessionRespondToApproval(t *testing.T) {
	s, _ := newTestClaudeSession(t)

	// Each sub-case uses a distinct request ID: Bug B9 dedup rejects
	// repeat responses for the same ID, so reusing "req-1" across all
	// three iterations would trip ErrApprovalAlreadyResolved on the
	// second decision. Unique IDs keep the test focused on the decision
	// encoding, which is what it is supposed to cover.
	decisions := []string{"allow", "deny", "allow_session"}
	for _, d := range decisions {
		t.Run(d, func(t *testing.T) {
			s.trackPendingApproval("req-"+d, provider.EventApprovalResolved)
			err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
				RequestID: "req-" + d,
				Decision:  d,
			})
			if err != nil {
				t.Fatalf("RespondToApproval(%s): %v", d, err)
			}
		})
	}
}

func TestSessionRespondToUserInputIncludesQuestionsInUpdatedInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; done`},
		Env: map[string]string{
			"CAPTURE": capturePath,
		},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent:  func(provider.ProviderEvent) {},
		cancel:   cancel,
	}
	t.Cleanup(func() {
		cancel()
		proc.Close()
	})

	questions := []provider.UserInputQuestion{{
		ID:       "framework",
		Header:   "Framework",
		Question: "Pick one",
		Options: []provider.UserInputQuestionOption{{
			Label:       "Svelte",
			Description: "Use Svelte",
		}},
	}, {
		ID:          "extras",
		Header:      "Extras",
		Question:    "Pick extras",
		MultiSelect: true,
		Options: []provider.UserInputQuestionOption{{
			Label:       "lint",
			Description: "Run lint",
		}, {
			Label:       "tests",
			Description: "Run tests",
		}},
	}}
	s.trackPendingApprovalWithQuestions("req-user-input", provider.EventUserInputResolved, questions)

	err = s.RespondToUserInput(context.Background(), provider.UserInputResponse{
		RequestID: "req-user-input",
		Decision:  "accept",
		Answers: map[string]provider.UserInputAnswer{
			"framework": provider.SingleUserInputAnswer("Svelte"),
			"extras":    provider.UserInputAnswer{"lint", "tests"},
		},
	})
	if err != nil {
		t.Fatalf("RespondToUserInput: %v", err)
	}

	var captured []byte
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		captured, err = os.ReadFile(capturePath)
		if err == nil && len(captured) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(captured) == 0 {
		t.Fatalf("capture file was empty: %v", err)
	}

	var msg struct {
		Response struct {
			Response struct {
				Behavior     string `json:"behavior"`
				UpdatedInput struct {
					Answers   map[string]string            `json:"answers"`
					Questions []provider.UserInputQuestion `json:"questions"`
				} `json:"updatedInput"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(captured, &msg); err != nil {
		t.Fatalf("unmarshal captured response: %v (data=%s)", err, captured)
	}
	if msg.Response.Response.Behavior != "allow" {
		t.Fatalf("behavior = %q, want allow", msg.Response.Response.Behavior)
	}
	if msg.Response.Response.UpdatedInput.Answers["Pick one"] != "Svelte" {
		t.Fatalf("answers = %+v, want Pick one=Svelte", msg.Response.Response.UpdatedInput.Answers)
	}
	if msg.Response.Response.UpdatedInput.Answers["Pick extras"] != "lint, tests" {
		t.Fatalf("answers = %+v, want Pick extras=\"lint, tests\"", msg.Response.Response.UpdatedInput.Answers)
	}
	if !reflect.DeepEqual(msg.Response.Response.UpdatedInput.Questions, questions) {
		t.Fatalf("questions = %+v, want %+v", msg.Response.Response.UpdatedInput.Questions, questions)
	}
}

func TestClaudeAskUserQuestionAnswersAvoidsDuplicateQuestionTextCollision(t *testing.T) {
	questions := []provider.UserInputQuestion{{
		ID:       "first",
		Header:   "First choice",
		Question: "Pick one",
	}, {
		ID:       "second",
		Header:   "Second choice",
		Question: "Pick one",
	}}

	got := claudeAskUserQuestionAnswers(questions, map[string]provider.UserInputAnswer{
		"first":  provider.SingleUserInputAnswer("React"),
		"second": provider.SingleUserInputAnswer("Svelte"),
	})

	if got["First choice"] != "React" {
		t.Fatalf("first answer = %+v, want First choice=React", got)
	}
	if got["Second choice"] != "Svelte" {
		t.Fatalf("second answer = %+v, want Second choice=Svelte", got)
	}
	if _, ok := got["Pick one"]; ok {
		t.Fatalf("duplicate question text key was used: %+v", got)
	}
}

func TestClaudeApprovalWaitsForUserResponseWithoutTimeout(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithPendingRequests(t)

	approvalLine := []byte(`{"type":"control_request","request_id":"req-waiting","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	if err := s.proc.WriteLine(approvalLine); err != nil {
		t.Fatalf("write approval: %v", err)
	}

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalRequest {
				goto waitWithoutResolution
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no approval event")
		}
	}

waitWithoutResolution:
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
				RequestID: "req-waiting",
				Decision:  "allow",
			}); err != nil {
				t.Fatalf("RespondToApproval after waiting: %v", err)
			}
			return
		}
	}
}

func TestClaudeUserInputWaitsForUserResponseWithoutTimeout(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithPendingRequests(t)

	uqLine := []byte(`{"type":"control_request","request_id":"uq-waiting","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"id":"scope","header":"Scope","question":"Choose","options":[{"label":"turn","description":"This turn"}]}]}}}`)
	if err := s.proc.WriteLine(uqLine); err != nil {
		t.Fatalf("write user-input request: %v", err)
	}

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventUserInputRequest {
				goto waitWithoutResolution
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no user-input request event")
		}
	}

waitWithoutResolution:
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
				RequestID: "uq-waiting",
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

func TestApprovalResponseResolvesPendingClaude(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithPendingRequests(t)

	approvalLine := []byte(`{"type":"control_request","request_id":"req-normal","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	if err := s.proc.WriteLine(approvalLine); err != nil {
		t.Fatalf("write approval: %v", err)
	}

	// Wait for the approval event to arrive.
	var gotApproval bool
	for !gotApproval {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalRequest {
				gotApproval = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no approval event")
		}
	}

	if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "req-normal",
		Decision:  "allow",
	}); err != nil {
		t.Fatalf("RespondToApproval: %v", err)
	}

	if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "req-normal",
		Decision:  "deny",
	}); !errors.Is(err, provider.ErrStaleInteractiveRequest) {
		t.Fatalf("second RespondToApproval error = %v, want ErrStaleInteractiveRequest", err)
	}
}

func TestClaudeCloseResolvesPendingApprovalAsLost(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithPendingRequests(t)

	approvalLine := []byte(`{"type":"control_request","request_id":"req-close","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	if err := s.proc.WriteLine(approvalLine); err != nil {
		t.Fatalf("write approval: %v", err)
	}

	var gotApproval bool
	for !gotApproval {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalRequest {
				gotApproval = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no approval event")
		}
	}

	if err := s.Close(); err != nil {
		t.Logf("close returned %v (acceptable)", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt, ok := <-eventCh:
			if !ok {
				t.Fatal("event channel closed before approval resolved")
			}
			if evt.Kind != provider.EventApprovalResolved {
				continue
			}
			var meta map[string]any
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal resolved meta: %v", err)
			}
			if meta["decision"] != "lost" {
				t.Fatalf("decision = %v, want lost", meta["decision"])
			}
			return
		case <-deadline:
			t.Fatal("pending approval was not resolved on close")
		}
	}
}

func TestClaudeProviderExitResolvesPendingUserInputAsLost(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithPendingRequests(t)

	uqLine := []byte(`{"type":"control_request","request_id":"uq-exit","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"id":"scope","header":"Scope","question":"Choose","options":[{"label":"turn","description":"This turn"}]}]}}}`)
	if err := s.proc.WriteLine(uqLine); err != nil {
		t.Fatalf("write user-input request: %v", err)
	}

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventUserInputRequest {
				goto closeProvider
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no user-input request event")
		}
	}

closeProvider:
	if err := s.proc.Close(); err != nil {
		t.Fatalf("close provider process: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventUserInputResolved {
				continue
			}
			var meta map[string]any
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal resolved meta: %v", err)
			}
			if meta["decision"] != "lost" {
				t.Fatalf("decision = %v, want lost", meta["decision"])
			}
			if _, ok := meta["answers"].(map[string]any); !ok {
				t.Fatalf("answers missing or wrong type: %v", meta["answers"])
			}
			return
		case <-deadline:
			t.Fatal("pending user input was not resolved after provider exit")
		}
	}
}

func TestClaudeCloseResolvesPendingUserInputAsLost(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithPendingRequests(t)

	uqLine := []byte(`{"type":"control_request","request_id":"uq-close","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"id":"scope","header":"Scope","question":"Choose","options":[{"label":"turn","description":"This turn"}]}]}}}`)
	if err := s.proc.WriteLine(uqLine); err != nil {
		t.Fatalf("write user-input request: %v", err)
	}

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventUserInputRequest {
				goto closeSession
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no user-input request event")
		}
	}

closeSession:
	if err := s.Close(); err != nil {
		t.Logf("close returned %v (acceptable)", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventUserInputResolved {
				continue
			}
			var meta map[string]any
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal resolved meta: %v", err)
			}
			if meta["decision"] != "lost" {
				t.Fatalf("decision = %v, want lost", meta["decision"])
			}
			if _, ok := meta["answers"].(map[string]any); !ok {
				t.Fatalf("answers missing or wrong type: %v", meta["answers"])
			}
			err := s.RespondToUserInput(context.Background(), provider.UserInputResponse{
				RequestID: "uq-close",
				Decision:  "accept",
				Answers: map[string]provider.UserInputAnswer{
					"scope": provider.SingleUserInputAnswer("turn"),
				},
			})
			if !errors.Is(err, provider.ErrStaleInteractiveRequest) {
				t.Fatalf("RespondToUserInput after close error = %v, want ErrStaleInteractiveRequest", err)
			}
			return
		case <-deadline:
			t.Fatal("pending user input was not resolved on close")
		}
	}
}

// TestControlCancelRequestClearsPendingApproval exercises the
// `control_cancel_request` cleanup path. When an interrupt aborts an
// in-flight can_use_tool callback, the CLI emits this envelope to
// abandon the prior request. We must:
//   - clear the pending approval / user-input state (so the panel
//     disappears),
//   - emit the matching resolved event with cancel semantics,
//   - NOT write a control_response (the CLI is no longer waiting).
//
// Mirror tests for both the approval and user-input flavours. Bug-fix
// tracker: agent-overflow merry-wirth plan, step 3.
func TestControlCancelRequestClearsPendingApproval(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithPendingRequests(t)

	// 1. CLI emits an approval request.
	approvalLine := []byte(`{"type":"control_request","request_id":"req-cancel","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	if err := s.proc.WriteLine(approvalLine); err != nil {
		t.Fatalf("write approval: %v", err)
	}

	var gotApproval bool
	for !gotApproval {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalRequest && evt.ItemID == "req-cancel" {
				gotApproval = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no approval event")
		}
	}

	// 2. CLI later abandons it via control_cancel_request (e.g. user
	// interrupt fired, SDK-side AbortSignal).
	cancelLine := []byte(`{"type":"control_cancel_request","request_id":"req-cancel"}`)
	if err := s.proc.WriteLine(cancelLine); err != nil {
		t.Fatalf("write cancel: %v", err)
	}

	// 3. Expect EventApprovalResolved with decision:"cancel".
	deadline := time.After(2 * time.Second)
	var gotResolved bool
	for !gotResolved {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalResolved && evt.ItemID == "req-cancel" {
				var meta map[string]any
				if err := json.Unmarshal(evt.Meta, &meta); err != nil {
					t.Fatalf("unmarshal resolved meta: %v", err)
				}
				if meta["decision"] != "cancel" {
					t.Fatalf("resolved decision: got %v, want cancel", meta["decision"])
				}
				gotResolved = true
			}
		case <-deadline:
			t.Fatal("never saw EventApprovalResolved for cancelled request")
		}
	}

	// 4. A subsequent RespondToApproval for the same id must short-
	// circuit: the request is already resolved.
	respErr := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "req-cancel",
		Decision:  "allow",
	})
	if respErr == nil {
		t.Fatalf("RespondToApproval after cancel: expected error, got nil")
	}
	if !errors.Is(respErr, provider.ErrStaleInteractiveRequest) {
		t.Fatalf("RespondToApproval after cancel: got %v, want ErrStaleInteractiveRequest", respErr)
	}
}

// TestControlCancelRequestClearsPendingUserInput is the AskUserQuestion
// flavour: when the CLI cancels a pending user-input prompt, the
// resolved event must carry empty answers and decision="cancel" so
// the panel above the composer clears.
func TestControlCancelRequestClearsPendingUserInput(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithPendingRequests(t)

	uqLine := []byte(`{"type":"control_request","request_id":"uq-cancel","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"id":"q","header":"Pick","question":"a or b?","options":[{"label":"a","description":"opt a"},{"label":"b","description":"opt b"}]}]}}}`)
	if err := s.proc.WriteLine(uqLine); err != nil {
		t.Fatalf("write user-input request: %v", err)
	}

	var gotRequest bool
	for !gotRequest {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventUserInputRequest && evt.ItemID == "uq-cancel" {
				gotRequest = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no user-input request event")
		}
	}

	cancelLine := []byte(`{"type":"control_cancel_request","request_id":"uq-cancel"}`)
	if err := s.proc.WriteLine(cancelLine); err != nil {
		t.Fatalf("write cancel: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventUserInputResolved && evt.ItemID == "uq-cancel" {
				var meta map[string]any
				if err := json.Unmarshal(evt.Meta, &meta); err != nil {
					t.Fatalf("unmarshal resolved meta: %v", err)
				}
				if meta["decision"] != "cancel" {
					t.Fatalf("resolved decision: got %v, want cancel", meta["decision"])
				}
				answers, ok := meta["answers"].(map[string]any)
				if !ok {
					t.Fatalf("answers missing or wrong type: %v", meta["answers"])
				}
				if len(answers) != 0 {
					t.Fatalf("answers: got %v, want empty map", answers)
				}
				return
			}
		case <-deadline:
			t.Fatal("never saw EventUserInputResolved for cancelled request")
		}
	}
}

// TestExitPlanModeWriteFailureClosesSession exercises Bug B7: when the
// synthetic deny-control_response can't be written (stdin closed, pipe
// broken, subprocess gone), the old readLoop just logged and kept
// going — leaving the subprocess hung waiting for a reply. The fix
// treats the write failure as a session-fatal error: readLoop closes
// the subprocess, emits EventError, and reaches the disconnected
// terminal state.
func TestExitPlanModeWriteFailureClosesSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Close stdin (the read end of our write pipe), then print the plan
	// request while keeping the subprocess alive briefly. The ordering is
	// load-bearing: if the request is printed first, readLoop can race ahead
	// and write the denial before the shell has actually closed fd 0.
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", `exec 0<&-; printf '{"type":"control_request","request_id":"plan-1","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"# plan"}}}\n'; sleep 0.05`},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer proc.Kill()

	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	go s.readLoop()

	// Expect: EventProposedPlan fires (read from subprocess line),
	// then the write fails (subprocess already exited), then an
	// EventError describing the failure, then disconnected.
	var gotPlan, gotWriteErr, gotDisconnected bool
	deadline := time.After(5 * time.Second)
	for !(gotPlan && gotWriteErr && gotDisconnected) {
		select {
		case evt := <-eventCh:
			switch {
			case evt.Kind == provider.EventProposedPlan:
				gotPlan = true
			case evt.Kind == provider.EventError &&
				(strings.Contains(evt.Content, "exit plan mode") || strings.Contains(evt.Content, "plan mode response")):
				gotWriteErr = true
			case evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected":
				gotDisconnected = true
			}
		case <-deadline:
			t.Fatalf("timeout (plan=%v writeErr=%v disc=%v)", gotPlan, gotWriteErr, gotDisconnected)
		}
	}
}

// TestExitPlanModeWritesDenyOnHappyPath verifies the normal path is
// unchanged: a plan arrives, the deny response is written, the
// subprocess continues happily.
func TestExitPlanModeWritesDenyOnHappyPath(t *testing.T) {
	s, eventCh := newTestClaudeSession(t)

	planReq := []byte(`{"type":"control_request","request_id":"plan-ok","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"# hi"}}}`)
	if err := s.proc.WriteLine(planReq); err != nil {
		t.Fatalf("write plan request: %v", err)
	}

	var sawPlan bool
	for !sawPlan {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventProposedPlan {
				sawPlan = true
			}
		case <-time.After(3 * time.Second):
			t.Fatal("never saw EventProposedPlan")
		}
	}

	// The subprocess (cat) echoes the deny response back. readLoop
	// will parse the echoed line — it should be a control_response,
	// which ParseLine returns 0 events for, and readLoop continues.
	// Fire another normal line to confirm readLoop survives.
	if err := s.proc.WriteLine([]byte(`{"type":"system","subtype":"future_feature"}`)); err != nil {
		t.Fatalf("write follow-up: %v", err)
	}

	// Confirm no EventError arrives.
	select {
	case evt := <-eventCh:
		if evt.Kind == provider.EventError {
			t.Fatalf("unexpected error after happy-path plan mode: %v", evt.Content)
		}
	case <-time.After(200 * time.Millisecond):
		// ok
	}
}

// TestAutoModeSurfacesFallbackApprovalRequest is the safety net under the auto
// runtime mode. Claude's auto classifier does NOT answer every request: it
// falls back to a real interactive ask on safety_check, ask_rule,
// plan_mode_floor, org_ask_ceiling and requires_user_interaction, and the
// fallback arrives as an ordinary `can_use_tool` control_request. If AO ever
// swallowed or auto-answered those, an auto-mode turn would hang on a prompt
// the user never sees (or, worse, silently allow what the classifier declined
// to bless).
//
// The one path that could swallow it is handleFullAccessToolRequest, whose
// auto-approval short-circuit is keyed on the literal permission mode
// "bypassPermissions". Auto is a different mode with a different promise — the
// reviewer can DENY — so this asserts the full round trip on an auto session:
// the request surfaces as EventApprovalRequest and RespondToApproval resolves
// it exactly as it would in any other tier.
func TestAutoModeSurfacesFallbackApprovalRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 200)
	s := &Session{
		proc:                  proc,
		threadID:              testThread,
		onEvent:               func(evt provider.ProviderEvent) { eventCh <- evt },
		cancel:                cancel,
		readDone:              make(chan struct{}),
		basePermissionMode:    claudeBasePermissionMode(provider.RuntimeAuto),
		currentPermissionMode: claudeBasePermissionMode(provider.RuntimeAuto),
	}
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		proc.Close()
	})

	// Guard the premise: the mode under test really is auto, not a typo that
	// happens to miss the bypassPermissions branch for the wrong reason.
	if got := s.getCurrentPermissionMode(); got != "auto" {
		t.Fatalf("currentPermissionMode = %q, want auto", got)
	}

	line := []byte(`{"type":"control_request","request_id":"req-auto-fallback","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf /tmp/x"}}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write approval: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventApprovalRequest {
				continue
			}
			var approval provider.ApprovalRequest
			if err := json.Unmarshal(evt.Meta, &approval); err != nil {
				t.Fatalf("unmarshal approval: %v", err)
			}
			if approval.RequestID != "req-auto-fallback" || approval.ToolName != "Bash" {
				t.Fatalf("approval = %+v, want the Bash request that was sent", approval)
			}
			if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
				RequestID: "req-auto-fallback",
				Decision:  "allow",
			}); err != nil {
				t.Fatalf("RespondToApproval: %v", err)
			}
			return
		case <-deadline:
			t.Fatal("auto-mode session never surfaced the fallback approval request")
		}
	}
}

// TestAutoModeDoesNotAutoApproveToolRequests is the unit-level twin of the
// round trip above: the full-access short-circuit must decline to claim an
// auto-mode request. Stated separately because the two failures are different
// bugs — this one would auto-ALLOW a tool the classifier had already refused
// to bless, which no event assertion downstream could detect.
func TestAutoModeDoesNotAutoApproveToolRequests(t *testing.T) {
	s := &Session{currentPermissionMode: claudeBasePermissionMode(provider.RuntimeAuto)}
	handled, err := s.maybeHandleFullAccessToolRequest(
		[]byte(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`))
	if err != nil {
		t.Fatalf("maybeHandleFullAccessToolRequest: %v", err)
	}
	if handled {
		t.Fatal("auto-mode session auto-approved a tool request; only bypassPermissions may do that")
	}
}
