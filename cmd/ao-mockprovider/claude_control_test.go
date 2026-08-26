package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider/claude"
)

// claudeControlResponse mirrors the envelope
// internal/provider/claude/session_control.go's read loop decodes, so
// these tests assert on the same fields the app actually reads.
type claudeControlResponse struct {
	Type     string `json:"type"`
	Response struct {
		Subtype   string          `json:"subtype"`
		RequestID string          `json:"request_id"`
		Error     string          `json:"error"`
		Response  json.RawMessage `json:"response"`
	} `json:"response"`
}

func ackFor(t *testing.T, subtype, request string) claudeControlResponse {
	t.Helper()
	var buf bytes.Buffer
	writeClaudeControlAck(newLineWriter(&buf), "req-1", subtype, json.RawMessage(request))
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatalf("subtype %q produced no control_response at all", subtype)
	}
	var out claudeControlResponse
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("control_response for %q is not decodable: %v (line %s)", subtype, err, line)
	}
	if out.Type != "control_response" || out.Response.RequestID != "req-1" {
		t.Fatalf("control_response envelope for %q = %s", subtype, line)
	}
	return out
}

// TestControlAckIsSubtypeAwareAndStrict pins the two halves of the mock's
// control_request contract, per subtype: a request carrying the CLI's own
// keys gets a REAL-shaped answer, and a request carrying a mis-spelled
// one gets an error NAMING the key.
//
// The strictness is the point. The real CLI destructures the fields it
// wants off `request` and never validates the object, so an unknown key
// is dropped and the field it should have filled reads `undefined` —
// which is how `server_name` shipped for months where the CLI reads
// `serverName`, answering "Server not found: undefined" for every server
// with the round trip, the error path and the status projection all
// working correctly around it. A mock that acks anything cannot catch the
// next one.
//
// Key spellings come from internal/provider/claude's
// TestControlRequestWireKeys, which reads them off the 2.1.237 binary's
// handlers — NOT from what looks consistent, because the CLI mixes
// camelCase and snake_case per handler with no rule.
func TestControlAckIsSubtypeAwareAndStrict(t *testing.T) {
	cases := []struct {
		name string
		// valid is a request the app really sends.
		valid string
		// wrong is the same request with one key mis-spelled, which the
		// mock must refuse by naming the key it wanted.
		wrong    string
		wantKey  string
		wantPass func(t *testing.T, payload json.RawMessage)
	}{
		{
			name:    "mcp_status",
			valid:   `{"subtype":"mcp_status"}`,
			wantKey: "",
			wantPass: func(t *testing.T, payload json.RawMessage) {
				var decoded struct {
					MCPServers []claude.MCPServerStatus `json:"mcpServers"`
				}
				if err := json.Unmarshal(payload, &decoded); err != nil {
					t.Fatalf("mcp_status payload does not decode: %v", err)
				}
				if len(decoded.MCPServers) == 0 {
					t.Fatal("mcp_status answered no servers; the status surface renders empty")
				}
				var sawNeedsAuth bool
				for _, srv := range decoded.MCPServers {
					if srv.Name == "" || srv.Status == "" {
						t.Fatalf("mcp_status entry missing name/status: %+v", srv)
					}
					if srv.Status == "needs-auth" {
						sawNeedsAuth = true
					}
				}
				if !sawNeedsAuth {
					t.Error("no needs-auth server: the sign-in affordance is unreachable under harness")
				}
			},
		},
		{
			name:    "mcp_authenticate",
			valid:   `{"subtype":"mcp_authenticate","serverName":"plugin:mock:oauth"}`,
			wrong:   `{"subtype":"mcp_authenticate","server_name":"plugin:mock:oauth"}`,
			wantKey: "serverName",
			wantPass: func(t *testing.T, payload json.RawMessage) {
				// AuthenticateMCP fails a success response that carries no
				// payload at all ("success response carried no payload"), so
				// the old empty `{}` ack made MCP sign-in fail in the harness
				// no matter what the app did.
				var out claude.MCPAuthResult
				if err := json.Unmarshal(payload, &out); err != nil {
					t.Fatalf("mcp_authenticate payload does not decode into MCPAuthResult: %v", err)
				}
				if out.AuthURL == "" || !out.RequiresUserAction {
					t.Fatalf("mcp_authenticate payload = %+v, want the browser-hop success body", out)
				}
			},
		},
		{
			name:    "mcp_toggle",
			valid:   `{"subtype":"mcp_toggle","serverName":"mock-fs","enabled":false}`,
			wrong:   `{"subtype":"mcp_toggle","serverName":"mock-fs","isEnabled":false}`,
			wantKey: "enabled",
		},
		{
			name:    "mcp_reconnect",
			valid:   `{"subtype":"mcp_reconnect","serverName":"mock-fs"}`,
			wrong:   `{"subtype":"mcp_reconnect","name":"mock-fs"}`,
			wantKey: "serverName",
		},
		{
			name:    "mcp_oauth_callback_url",
			valid:   `{"subtype":"mcp_oauth_callback_url","serverName":"mock-fs","callbackUrl":"http://x/?code=c&state=s"}`,
			wrong:   `{"subtype":"mcp_oauth_callback_url","serverName":"mock-fs","callback_url":"http://x/?code=c&state=s"}`,
			wantKey: "callbackUrl",
		},
		{
			name:    "set_model",
			valid:   `{"subtype":"set_model","model":"opus"}`,
			wrong:   `{"subtype":"set_model","modelId":"opus"}`,
			wantKey: "model",
		},
		{
			name:    "set_permission_mode",
			valid:   `{"subtype":"set_permission_mode","mode":"plan"}`,
			wrong:   `{"subtype":"set_permission_mode","permissionMode":"plan"}`,
			wantKey: "mode",
		},
		{
			name: "stop_task",
			// snake_case here and camelCase two rows up, both from the same
			// binary: the CLI has no rule, which is why every subtype is
			// pinned individually rather than by resemblance.
			valid:   `{"subtype":"stop_task","task_id":"task-1"}`,
			wrong:   `{"subtype":"stop_task","taskId":"task-1"}`,
			wantKey: "task_id",
		},
		{
			name:  "initialize",
			valid: `{"subtype":"initialize"}`,
			wantPass: func(t *testing.T, payload json.RawMessage) {
				var decoded struct {
					Account *struct {
						Email string `json:"email"`
					} `json:"account"`
					Models []json.RawMessage `json:"models"`
				}
				if err := json.Unmarshal(payload, &decoded); err != nil {
					t.Fatalf("initialize payload does not decode: %v", err)
				}
				if decoded.Account == nil || decoded.Account.Email == "" || len(decoded.Models) == 0 {
					t.Fatalf("initialize payload lost its account/models: %s", payload)
				}
			},
		},
		{
			name:  "background_tasks",
			valid: `{"subtype":"background_tasks","tool_use_id":"toolu_1"}`,
			wantPass: func(t *testing.T, payload json.RawMessage) {
				var decoded struct {
					Backgrounded bool `json:"backgrounded"`
				}
				if err := json.Unmarshal(payload, &decoded); err != nil || !decoded.Backgrounded {
					t.Fatalf("background_tasks payload = %s, want {backgrounded:true}", payload)
				}
			},
		},
		{
			name:  "get_settings",
			valid: `{"subtype":"get_settings"}`,
		},
		{
			name:  "interrupt",
			valid: `{"subtype":"interrupt"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ack := ackFor(t, tc.name, tc.valid)
			if ack.Response.Subtype != "success" {
				t.Fatalf("valid %s request was refused: %s", tc.name, ack.Response.Error)
			}
			if tc.wantPass != nil {
				tc.wantPass(t, ack.Response.Response)
			}

			if tc.wrong == "" {
				return
			}
			bad := ackFor(t, tc.name, tc.wrong)
			if bad.Response.Subtype != "error" {
				t.Fatalf("mis-spelled %s request was ACKED as success: %s", tc.name, bad.Response.Response)
			}
			if !strings.Contains(bad.Response.Error, tc.wantKey) {
				t.Fatalf("error for a mis-spelled %s request = %q, want it to name %q",
					tc.name, bad.Response.Error, tc.wantKey)
			}
		})
	}
}

// TestControlAckIsPermissiveForUnknownSubtypes: the app gaining a
// control_request this mock has never heard of must not become a harness
// failure with no diagnosis. Forward compatibility beats strictness for a
// subtype nobody has written an assertion about yet.
func TestControlAckIsPermissiveForUnknownSubtypes(t *testing.T) {
	ack := ackFor(t, "some_future_subtype", `{"subtype":"some_future_subtype","whatever":1}`)
	if ack.Response.Subtype != "success" {
		t.Fatalf("unknown subtype was refused: %s", ack.Response.Error)
	}
	if strings.TrimSpace(string(ack.Response.Response)) != "{}" {
		t.Fatalf("unknown subtype payload = %s, want {}", ack.Response.Response)
	}
}

// TestControlAckRefusesAnUnreadableRequest: a request object that is not
// an object at all cannot carry the keys the CLI destructures, so it is
// refused rather than acked.
func TestControlAckRefusesAnUnreadableRequest(t *testing.T) {
	ack := ackFor(t, "set_model", `"set_model"`)
	if ack.Response.Subtype != "error" {
		t.Fatalf("an unreadable request was acked: %s", ack.Response.Response)
	}
}
