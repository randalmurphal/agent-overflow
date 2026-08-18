package claude

import (
	"bytes"
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// --- Gap 1: CanUseTool parity ---
//
// These tests cover:
//   - buildArgs includes --permission-prompt-tool stdio
//   - parseControlRequest preserves permission_suggestions through
//     ApprovalRequest.PermissionSuggestions
//   - buildApprovalResponse emits updatedInput / updatedPermissions when provided

func TestBuildArgsIncludesPermissionPromptTool(t *testing.T) {
	args := buildArgs(Config{}, "")

	for i, arg := range args {
		if arg == "--permission-prompt-tool" {
			if i+1 >= len(args) || args[i+1] != "stdio" {
				t.Fatalf("expected --permission-prompt-tool stdio, got args=%v", args)
			}
			return
		}
	}
	t.Fatalf("missing --permission-prompt-tool stdio flag; args=%v", args)
}

func TestParseControlRequestPreservesPermissionSuggestions(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"req_1_abc","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf /"},"permission_suggestions":[{"type":"addRules","rules":[{"toolName":"Bash"}],"behavior":"allow","destination":"session"}]}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(evt.Meta, &approval); err != nil {
		t.Fatalf("unmarshal approval: %v", err)
	}
	if len(approval.PermissionSuggestions) == 0 {
		t.Fatalf("PermissionSuggestions missing: %s", evt.Meta)
	}

	// Round-trip: the preserved JSON decodes back into the original shape.
	var suggestions []map[string]any
	if err := json.Unmarshal(approval.PermissionSuggestions, &suggestions); err != nil {
		t.Fatalf("unmarshal suggestions: %v (raw=%s)", err, approval.PermissionSuggestions)
	}
	if len(suggestions) != 1 {
		t.Fatalf("suggestions len: got %d, want 1", len(suggestions))
	}
	if suggestions[0]["type"] != "addRules" {
		t.Errorf("suggestion type: got %v, want addRules", suggestions[0]["type"])
	}
	if suggestions[0]["behavior"] != "allow" {
		t.Errorf("suggestion behavior: got %v, want allow", suggestions[0]["behavior"])
	}
}

func TestParseControlRequestWithoutPermissionSuggestions(t *testing.T) {
	// Backward compat: no permission_suggestions means empty field.
	line := []byte(`{"type":"control_request","request_id":"req_1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(events[0].Meta, &approval); err != nil {
		t.Fatalf("unmarshal approval: %v", err)
	}
	if len(approval.PermissionSuggestions) != 0 {
		t.Errorf("PermissionSuggestions should be empty, got %s",
			approval.PermissionSuggestions)
	}
}

// --- Gap 1: extended response writer ---

func TestBuildApprovalResponseAllowWithUpdatedInput(t *testing.T) {
	updatedInput := json.RawMessage(`{"command":"ls -la"}`)

	data, err := buildApprovalResponse(provider.ApprovalResponse{
		RequestID:    "req-1",
		Decision:     "allow",
		UpdatedInput: updatedInput,
	})
	if err != nil {
		t.Fatalf("buildApprovalResponse: %v", err)
	}

	var msg struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Response  struct {
				Behavior     string          `json:"behavior"`
				UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v (data=%s)", err, data)
	}
	if msg.Type != "control_response" {
		t.Errorf("type: got %q, want control_response", msg.Type)
	}
	if msg.Response.Subtype != "success" {
		t.Errorf("subtype: got %q, want success", msg.Response.Subtype)
	}
	if msg.Response.RequestID != "req-1" {
		t.Errorf("request_id: got %q, want req-1", msg.Response.RequestID)
	}
	if msg.Response.Response.Behavior != "allow" {
		t.Errorf("behavior: got %q, want allow", msg.Response.Response.Behavior)
	}
	if !bytes.Equal(msg.Response.Response.UpdatedInput, updatedInput) {
		t.Errorf("updatedInput: got %s, want %s",
			msg.Response.Response.UpdatedInput, updatedInput)
	}
}

func TestBuildApprovalResponseAllowWithUpdatedPermissions(t *testing.T) {
	updatedPerms := json.RawMessage(`[{"type":"addRules","rules":[{"toolName":"Bash"}],"behavior":"allow","destination":"session"}]`)

	data, err := buildApprovalResponse(provider.ApprovalResponse{
		RequestID:          "req-2",
		Decision:           "acceptForSession",
		UpdatedPermissions: updatedPerms,
	})
	if err != nil {
		t.Fatalf("buildApprovalResponse: %v", err)
	}

	var msg struct {
		Response struct {
			Response struct {
				Behavior           string          `json:"behavior"`
				UpdatedPermissions json.RawMessage `json:"updatedPermissions,omitempty"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Response.Response.Behavior != "allow" {
		t.Errorf("behavior: got %q, want allow", msg.Response.Response.Behavior)
	}
	if !bytes.Equal(msg.Response.Response.UpdatedPermissions, updatedPerms) {
		t.Errorf("updatedPermissions: got %s, want %s",
			msg.Response.Response.UpdatedPermissions, updatedPerms)
	}
}

func TestBuildApprovalResponseAllowWithoutExtraFields(t *testing.T) {
	// Backward compat: omitting UpdatedInput / UpdatedPermissions produces the
	// simple allow payload with no extra fields.
	data, err := buildApprovalResponse(provider.ApprovalResponse{
		RequestID: "req-3",
		Decision:  "allow",
	})
	if err != nil {
		t.Fatalf("buildApprovalResponse: %v", err)
	}

	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	inner := msg["response"].(map[string]any)["response"].(map[string]any)
	if inner["behavior"] != "allow" {
		t.Errorf("behavior: got %v, want allow", inner["behavior"])
	}
	if _, present := inner["updatedInput"]; present {
		t.Errorf("updatedInput should be absent, got %v", inner["updatedInput"])
	}
	if _, present := inner["updatedPermissions"]; present {
		t.Errorf("updatedPermissions should be absent, got %v",
			inner["updatedPermissions"])
	}
}

func TestBuildApprovalResponseDenyStripsExtras(t *testing.T) {
	// Backward compat: deny decisions never include updatedInput /
	// updatedPermissions even if callers accidentally set them.
	data, err := buildApprovalResponse(provider.ApprovalResponse{
		RequestID:          "req-4",
		Decision:           "deny",
		UpdatedInput:       json.RawMessage(`{"command":"ignored"}`),
		UpdatedPermissions: json.RawMessage(`[]`),
	})
	if err != nil {
		t.Fatalf("buildApprovalResponse: %v", err)
	}

	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	inner := msg["response"].(map[string]any)["response"].(map[string]any)
	if inner["behavior"] != "deny" {
		t.Errorf("behavior: got %v, want deny", inner["behavior"])
	}
	if _, present := inner["updatedInput"]; present {
		t.Errorf("updatedInput should be absent on deny, got %v",
			inner["updatedInput"])
	}
	if _, present := inner["updatedPermissions"]; present {
		t.Errorf("updatedPermissions should be absent on deny, got %v",
			inner["updatedPermissions"])
	}
	if inner["message"] != "User denied" {
		t.Errorf("message: got %v, want 'User denied'", inner["message"])
	}
}

func TestBuildApprovalResponseAcceptAliasedToAllow(t *testing.T) {
	// The Codex-native "accept" decision is backward-compatible with
	// the legacy "allow" decision — both map to {"behavior":"allow"}.
	for _, decision := range []string{"accept", "acceptForSession", "allow", "allow_session"} {
		data, err := buildApprovalResponse(provider.ApprovalResponse{
			RequestID: "req-alias",
			Decision:  decision,
		})
		if err != nil {
			t.Fatalf("%s: buildApprovalResponse: %v", decision, err)
		}

		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("%s: unmarshal: %v", decision, err)
		}
		inner := msg["response"].(map[string]any)["response"].(map[string]any)
		if inner["behavior"] != "allow" {
			t.Errorf("%s: behavior: got %v, want allow", decision, inner["behavior"])
		}
	}
}
