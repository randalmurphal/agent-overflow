package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

const fixtureSendMessageAck = "../../../docs/references/fixtures/claude/send_message_ack_20260904.ndjson"

func TestDecodeSendMessageAck(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want SendMessageAck
		ok   bool
	}{
		{
			name: "refusal prefers the display line and names no agent",
			raw:  `{"success":false,"message":"No agent named 'A' is reachable.\nUse ListAgents to see everyone you can message.","display":"Not sent — no agent named 'A' is reachable."}`,
			want: SendMessageAck{Success: false, Reply: "Not sent — no agent named 'A' is reachable."},
			ok:   true,
		},
		{
			name: "queued to a live agent carries the pin id",
			raw:  `{"success":true,"message":"Message queued for delivery to ab487a02304913d06 at its next tool round.","pin":{"id":"ab487a02304913d06","name":"ab487a02304913d06","ref":"33db0e"}}`,
			want: SendMessageAck{Success: true, Reply: "Message queued for delivery to ab487a02304913d06 at its next tool round.", AgentID: "ab487a02304913d06"},
			ok:   true,
		},
		{
			name: "a §E6 resume ack names the agent through resumedAgentId",
			raw:  `{"success":true,"message":"Agent \"a464e54e96a45cd0c\" had no active task; resumed from transcript in the background with your message.","resumedAgentId":"a464e54e96a45cd0c"}`,
			want: SendMessageAck{Success: true, Reply: `Agent "a464e54e96a45cd0c" had no active task; resumed from transcript in the background with your message.`, AgentID: "a464e54e96a45cd0c"},
			ok:   true,
		},
		{
			name: "a peer-session ack has no agent",
			raw:  `{"success":true,"message":"“ping” → BETA"}`,
			want: SendMessageAck{Success: true, Reply: "“ping” → BETA"},
			ok:   true,
		},
		{name: "absent", raw: ``, ok: false},
		{name: "not an object", raw: `"text"`, ok: false},
		{name: "no success field", raw: `{"message":"x"}`, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DecodeSendMessageAck(json.RawMessage(tc.raw))
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestReplaySendMessageAckKeepsTheWireFlagAndTheSibling pins the wire
// fact the triage verdict depends on: the CLI answers a refused send
// with `is_error:false` on the tool_result block and `success:false`
// only inside `tool_use_result`, and the parser forwards both as-is.
func TestReplaySendMessageAckKeepsTheWireFlagAndTheSibling(t *testing.T) {
	events := replayFixture(t, fixtureSendMessageAck)
	var completes []provider.ProviderEvent
	for _, evt := range events {
		if evt.Kind == provider.EventToolComplete {
			completes = append(completes, evt)
		}
	}
	if len(completes) != 2 {
		t.Fatalf("expected 2 EventToolComplete, got %d", len(completes))
	}
	wantSuccess := []bool{false, true}
	for i, evt := range completes {
		var meta struct {
			IsError       bool            `json:"is_error"`
			IsBackground  bool            `json:"is_background"`
			ToolUseResult json.RawMessage `json:"tool_use_result"`
		}
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			t.Fatalf("complete %d: decode meta: %v", i, err)
		}
		if meta.IsError {
			t.Fatalf("complete %d: the wire never flags a SendMessage refusal is_error; parser must not invent one", i)
		}
		if meta.IsBackground {
			t.Fatalf("complete %d: an ordinary SendMessage ack is not a background carrier", i)
		}
		ack, ok := DecodeSendMessageAck(meta.ToolUseResult)
		if !ok {
			t.Fatalf("complete %d: tool_use_result did not decode: %s", i, meta.ToolUseResult)
		}
		if ack.Success != wantSuccess[i] {
			t.Fatalf("complete %d: success = %v, want %v", i, ack.Success, wantSuccess[i])
		}
	}
}
