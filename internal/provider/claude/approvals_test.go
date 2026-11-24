package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/procutil/procutiltest"
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
	args := buildArgs(Config{Reaper: procutiltest.Reaper()}, "", "")

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

// TestBuildApprovalResponse pins the control_response envelope and its inner
// body for every decision shape. The inner body is compared whole, so an
// extra field (updatedInput / updatedPermissions where none belongs) fails.
func TestBuildApprovalResponse(t *testing.T) {
	tests := []struct {
		name      string
		resp      provider.ApprovalResponse
		wantInner string
	}{
		{
			name:      "allow carries updatedInput",
			resp:      provider.ApprovalResponse{Decision: "allow", UpdatedInput: json.RawMessage(`{"command":"ls -la"}`)},
			wantInner: `{"behavior":"allow","updatedInput":{"command":"ls -la"}}`,
		},
		{
			name: "acceptForSession carries updatedPermissions",
			resp: provider.ApprovalResponse{
				Decision:           "acceptForSession",
				UpdatedPermissions: json.RawMessage(`[{"type":"addRules","rules":[{"toolName":"Bash"}],"behavior":"allow","destination":"session"}]`),
			},
			wantInner: `{"behavior":"allow","updatedPermissions":[{"type":"addRules","rules":[{"toolName":"Bash"}],"behavior":"allow","destination":"session"}]}`,
		},
		// Backward compat: omitting the extras produces the simple allow
		// payload, and the Codex-native "accept" decisions alias the legacy
		// "allow" ones.
		{name: "allow without extras", resp: provider.ApprovalResponse{Decision: "allow"}, wantInner: `{"behavior":"allow"}`},
		{name: "allow_session aliases allow", resp: provider.ApprovalResponse{Decision: "allow_session"}, wantInner: `{"behavior":"allow"}`},
		{name: "accept aliases allow", resp: provider.ApprovalResponse{Decision: "accept"}, wantInner: `{"behavior":"allow"}`},
		{name: "acceptForSession aliases allow", resp: provider.ApprovalResponse{Decision: "acceptForSession"}, wantInner: `{"behavior":"allow"}`},
		{
			// Deny never includes the extras even if a caller sets them.
			name: "deny strips extras",
			resp: provider.ApprovalResponse{
				Decision:           "deny",
				UpdatedInput:       json.RawMessage(`{"command":"ignored"}`),
				UpdatedPermissions: json.RawMessage(`[]`),
			},
			wantInner: `{"behavior":"deny","message":"User denied"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.resp.RequestID = "req-1"
			data, err := buildApprovalResponse(tc.resp)
			if err != nil {
				t.Fatalf("buildApprovalResponse: %v", err)
			}
			var msg struct {
				Type     string `json:"type"`
				Response struct {
					Subtype   string          `json:"subtype"`
					RequestID string          `json:"request_id"`
					Response  json.RawMessage `json:"response"`
				} `json:"response"`
			}
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("unmarshal: %v (data=%s)", err, data)
			}
			if msg.Type != "control_response" || msg.Response.Subtype != "success" || msg.Response.RequestID != "req-1" {
				t.Errorf("envelope = %s, want control_response/success/req-1", data)
			}
			if string(msg.Response.Response) != tc.wantInner {
				t.Errorf("inner response = %s, want %s", msg.Response.Response, tc.wantInner)
			}
		})
	}
}
