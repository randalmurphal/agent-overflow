package sessionimport

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"agent-overflow/internal/provider"
)

// parity_fixtures_test.go — the two wire sequences the parity gate
// drives. Both are hand-written in the vocabulary the matching provider
// reader emits; parity_test.go is where what they must prove is
// documented.

// claudeParityEvents is the wire sequence a Claude transcript produces:
// a whole turn with a prompt, reasoning, model text, a file-editing
// tool, a shell tool, a compaction boundary, and an api_error, settled
// by a turn complete carrying per-model usage. No wire turn id — the
// turn is implied, and both writers synthesize `<threadID>:<index>`.
func claudeParityEvents(threadID, workspace string) []provider.ProviderEvent {
	editPath := filepath.Join(workspace, "main.go")
	exitCode := 0
	return []provider.ProviderEvent{
		{
			Kind:      provider.EventTurnStart,
			ThreadID:  threadID,
			Timestamp: at(0),
		},
		{
			Kind:      provider.EventUserText,
			ThreadID:  threadID,
			Content:   "Rename the greeting in main.go.",
			Meta:      json.RawMessage(`{"provider_item_id":"user-uuid-1","parent_uuid":"parent-uuid-0"}`),
			Timestamp: at(1),
		},
		{
			Kind:      provider.EventThinking,
			ThreadID:  threadID,
			ItemID:    "reasoning-1",
			Content:   "The greeting lives in main.go; a single Edit covers it.",
			Meta:      json.RawMessage(`{"signature":"sig-1"}`),
			Timestamp: at(2),
		},
		{
			Kind:      provider.EventTextDelta,
			ThreadID:  threadID,
			ItemID:    "message-1",
			Content:   "Renaming the greeting now.",
			Timestamp: at(3),
		},
		{
			Kind:     provider.EventToolStart,
			ThreadID: threadID,
			ItemID:   "toolu_edit_1",
			ItemType: "Edit",
			Meta: json.RawMessage(fmt.Sprintf(
				`{"toolName":"Edit","input":{"file_path":%q,"old_string":"hello","new_string":"howdy"}}`,
				editPath)),
			Timestamp: at(4),
		},
		{
			Kind:     provider.EventToolComplete,
			ThreadID: threadID,
			ItemID:   "toolu_edit_1",
			Meta: json.RawMessage(fmt.Sprintf(
				`{"tool_use_result":{"filePath":%q,"structuredPatch":[`+
					`{"oldStart":1,"oldLines":1,"newStart":1,"newLines":1,`+
					`"lines":["-\thello","+\thowdy"]}]}}`,
				editPath)),
			Timestamp: at(5),
		},
		{
			Kind:      provider.EventToolStart,
			ThreadID:  threadID,
			ItemID:    "toolu_bash_1",
			ItemType:  "Bash",
			Meta:      json.RawMessage(`{"toolName":"Bash","input":{"command":"go build ./...","description":"build"}}`),
			Timestamp: at(6),
		},
		{
			Kind:      provider.EventToolComplete,
			ThreadID:  threadID,
			ItemID:    "toolu_bash_1",
			Content:   "ok  \tagent-overflow\t0.4s\n",
			Meta:      mustExitCodeMeta(exitCode),
			Timestamp: at(7),
		},
		{
			// A refused SendMessage: the wire says is_error:false, the ack
			// says success:false. Both writers must read the ack
			// (triage/sendmessage_ack.go).
			Kind:      provider.EventToolStart,
			ThreadID:  threadID,
			ItemID:    "toolu_send_1",
			ItemType:  "SendMessage",
			Meta:      json.RawMessage(`{"toolName":"SendMessage","input":{"to":"A","message":"status?"}}`),
			Timestamp: at(7),
		},
		{
			Kind:      provider.EventToolComplete,
			ThreadID:  threadID,
			ItemID:    "toolu_send_1",
			ItemType:  "SendMessage",
			Content:   "No agent named \"A\".",
			Meta:      json.RawMessage(`{"is_error":false,"tool_use_result":{"success":false,"message":"No agent named \"A\".","display":"No agent named \"A\" in this session."}}`),
			Timestamp: at(7),
		},
		{
			Kind:      provider.EventCompactBoundary,
			ThreadID:  threadID,
			ItemID:    "compaction-1",
			Content:   "Context compacted",
			Meta:      json.RawMessage(`{"trigger":"auto","summary":"Renamed the greeting in main.go."}`),
			Timestamp: at(8),
		},
		{
			Kind:      provider.EventError,
			ThreadID:  threadID,
			Content:   "Rate limit reached for this account.",
			Meta:      json.RawMessage(`{"api_error_enum":"rate_limit"}`),
			Timestamp: at(9),
		},
		{
			Kind:      provider.EventTurnComplete,
			ThreadID:  threadID,
			Timestamp: at(10),
			TurnComplete: &provider.WireTurnCompleteMeta{
				StopReason:         "end_turn",
				AssistantMessageID: "msg_parity_1",
				Usage: &provider.TokenUsage{
					InputTokens:          1200,
					OutputTokens:         340,
					CacheReadInputTokens: 800,
				},
				ModelUsage: []provider.ModelTokenUsage{{
					Model: "claude-sonnet-4-5",
					TokenUsage: provider.TokenUsage{
						InputTokens:          1200,
						OutputTokens:         340,
						CacheReadInputTokens: 800,
					},
				}},
			},
		},
	}
}

// codexParityEvents is the wire sequence a Codex rollout produces, in
// the vocabulary `internal/provider/codex/rollout` emits it: an explicit
// `turn_id` on the boundary events, a user message with no wire item id,
// whole content blocks as EventContentBlockStop (never deltas — a
// rollout records the settled item, so there is nothing to stream), a
// proposed plan, and a wait_agent whose completion is a SIBLING row
// rather than an in-place settle.
//
// Two things it deliberately leaves out, because both would drag
// live-only state into a store-pure comparison: the wait completion
// carries no `agentsStates` (which would make triage resolve child
// threads it has no trackers for), and the launch carries no receiver
// ids (which would make triage snapshot a subagent list the import has
// no equivalent of). Both are covered by the Codex golden fixtures.
func codexParityEvents(threadID, workspace string) []provider.ProviderEvent {
	return []provider.ProviderEvent{
		{
			Kind:      provider.EventTurnStart,
			ThreadID:  threadID,
			TurnID:    "turn-1",
			Timestamp: at(0),
		},
		{
			Kind:           provider.EventUserText,
			ThreadID:       threadID,
			Role:           "user",
			Content:        "Run the tests and tell me what broke.",
			ContentPresent: true,
			Timestamp:      at(1),
		},
		{
			Kind:      provider.EventToolStart,
			ThreadID:  threadID,
			TurnID:    "turn-1",
			ItemID:    "call_shell_1",
			ItemType:  "toolCall",
			Role:      "assistant",
			Meta:      json.RawMessage(`{"toolName":"shell","input":{"command":["bash","-lc","go test ./..."]},"call_id":"call_shell_1"}`),
			Timestamp: at(2),
		},
		{
			Kind:           provider.EventToolComplete,
			ThreadID:       threadID,
			TurnID:         "turn-1",
			ItemID:         "call_shell_1",
			ItemType:       "toolCall",
			Content:        "ok  \tagent-overflow\t0.4s\n",
			ContentPresent: true,
			Meta: json.RawMessage(`{"toolName":"shell","input":{"command":["bash","-lc","go test ./..."]},` +
				`"command":"go test ./...","exit_code":0,"item_status":"completed"}`),
			Timestamp: at(3),
		},
		{
			Kind:           provider.EventContentBlockStop,
			ThreadID:       threadID,
			Role:           "assistant",
			ItemType:       "agentMessage",
			Content:        "The suite is green; nothing broke.",
			ContentPresent: true,
			Meta:           json.RawMessage(`{"blockType":"text"}`),
			Timestamp:      at(4),
		},
		{
			Kind:           provider.EventContentBlockStop,
			ThreadID:       threadID,
			ItemType:       "reasoning",
			Content:        "Everything passed, so there is nothing left to chase.",
			ContentPresent: true,
			Meta:           json.RawMessage(`{"blockType":"thinking"}`),
			Timestamp:      at(5),
		},
		{
			Kind:      provider.EventProposedPlan,
			ThreadID:  threadID,
			TurnID:    "turn-1",
			ItemID:    "plan-1",
			ItemType:  "plan",
			Content:   "# Keep it green\n\n- add a regression test\n- run the suite",
			Meta:      json.RawMessage(`{"id":"plan-1","type":"plan","title":"Keep it green"}`),
			Timestamp: at(6),
		},
		{
			Kind:      provider.EventToolStart,
			ThreadID:  threadID,
			TurnID:    "turn-1",
			ItemID:    "call_wait_1",
			ItemType:  "wait_agent",
			Role:      "assistant",
			Meta:      json.RawMessage(`{"toolName":"wait_agent","input":{"timeout_ms":30000},"call_id":"call_wait_1"}`),
			Timestamp: at(7),
		},
		{
			Kind:           provider.EventToolComplete,
			ThreadID:       threadID,
			TurnID:         "turn-1",
			ItemID:         "call_wait_1",
			ItemType:       "wait_agent",
			Content:        "The reviewer agent finished.",
			ContentPresent: true,
			Meta:           json.RawMessage(`{"toolName":"wait_agent","item_status":"completed"}`),
			Timestamp:      at(8),
		},
		{
			Kind:      provider.EventTurnComplete,
			ThreadID:  threadID,
			TurnID:    "turn-1",
			Timestamp: at(9),
			TurnComplete: &provider.WireTurnCompleteMeta{
				StopReason:         "end_turn",
				AssistantMessageID: "msg_parity_codex_1",
				Usage: &provider.TokenUsage{
					InputTokens:          900,
					OutputTokens:         220,
					CacheReadInputTokens: 400,
				},
				ModelUsage: []provider.ModelTokenUsage{{
					Model: "gpt-5.6-sol",
					TokenUsage: provider.TokenUsage{
						InputTokens:          900,
						OutputTokens:         220,
						CacheReadInputTokens: 400,
					},
				}},
			},
		},
	}
}

func mustExitCodeMeta(exitCode int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"exit_code":%d}`, exitCode))
}
