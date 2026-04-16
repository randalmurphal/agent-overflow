package claude

import (
	"encoding/json"
	"fmt"

	"agent-overflow/internal/provider"
)

// buildApprovalResponse marshals an ApprovalResponse into the Claude CLI
// control_response wire format.
//
// Wire format (allow):
//
//	{"type":"control_response","response":{
//	  "subtype":"success",
//	  "request_id":"<id>",
//	  "response":{"behavior":"allow","updatedInput":{...},"updatedPermissions":[...]}
//	}}
//
// UpdatedInput and UpdatedPermissions are only emitted for allow decisions
// and only when non-empty, preserving backward compatibility with callers that
// use the simple allow/deny contract.
func buildApprovalResponse(resp provider.ApprovalResponse) ([]byte, error) {
	body := approvalBody(resp)
	msg := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": resp.RequestID,
			"response":   body,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("claude: marshal approval response: %w", err)
	}
	return data, nil
}

// approvalBody constructs the inner "response" payload based on the decision.
// For deny decisions, UpdatedInput and UpdatedPermissions are dropped — they
// are meaningless when the tool call is being rejected.
func approvalBody(resp provider.ApprovalResponse) map[string]any {
	if isAllowDecision(resp.Decision) {
		body := map[string]any{"behavior": "allow"}
		if len(resp.UpdatedInput) > 0 {
			body["updatedInput"] = json.RawMessage(resp.UpdatedInput)
		}
		if len(resp.UpdatedPermissions) > 0 {
			body["updatedPermissions"] = json.RawMessage(resp.UpdatedPermissions)
		}
		return body
	}
	return map[string]any{
		"behavior": "deny",
		"message":  "User denied",
	}
}

// isAllowDecision accepts both Codex-native values (accept,
// acceptForSession) and legacy values (allow, allow_session) for
// backward compatibility. Any other value — including explicit deny and
// the Codex-native decline/cancel — is treated as a deny.
func isAllowDecision(decision string) bool {
	switch decision {
	case "allow", "allow_session", "accept", "acceptForSession":
		return true
	default:
		return false
	}
}
