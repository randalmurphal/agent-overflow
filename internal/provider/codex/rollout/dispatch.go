package rollout

import (
	"encoding/json"
	"strings"

	"agent-overflow/internal/provider"
)

// Payload dispatch for the two envelope families that carry most of a
// rollout: `event_msg` (the live session's own event stream, replayed) and
// `response_item` (the model-context items). Both switches are open by
// construction — an unrecognised payload type is counted and skipped, never
// fatal — because the rollout enum drifts between Codex releases.

// ---------------------------------------------------------------- event_msg

func (c *converter) convertEventMsg(env envelope) {
	switch payloadType(env.Payload) {
	case "task_started":
		c.startTurn(env)
	case "task_complete":
		c.completeTurn(env)
	case "turn_aborted":
		c.abortTurn(env)
	case "token_count":
		c.applyTokenCount(env)

	case "user_message":
		var p userMessagePayload
		if json.Unmarshal(env.Payload, &p) != nil {
			c.corrupt++
			return
		}
		c.emitUserText(p.Message)
	case "agent_message":
		var p agentMessagePayload
		if json.Unmarshal(env.Payload, &p) != nil {
			c.corrupt++
			return
		}
		c.emitAssistantText(p.Message)
	case "agent_reasoning":
		var p agentReasoningPayload
		if json.Unmarshal(env.Payload, &p) != nil {
			c.corrupt++
			return
		}
		c.emitThinking(p.Text)

	case "exec_command_end":
		c.applyExecCommandEnd(env)
	case "patch_apply_end":
		c.applyPatchApplyEnd(env)
	case "mcp_tool_call_end":
		c.applyMCPToolCallEnd(env)
	case "web_search_end":
		c.applyWebSearchEnd(env)

	case "context_compacted":
		// The durable `compacted` record is the one that carries the
		// summary, and codex always writes it BEFORE this lightweight twin
		// (see converter.sawCompacted). So a twin arriving with the flag
		// already set is a duplicate of a divider we just wrote; one
		// arriving with it clear comes from a file old enough to write no
		// `compacted` records at all, and is the only divider there is.
		if c.sawCompacted {
			return
		}
		c.emitCompactionBoundary("", nil)

	case "sub_agent_activity":
		c.applySubAgentActivity(env)

	case "thread_goal_updated":
		var p threadGoalPayload
		if json.Unmarshal(env.Payload, &p) != nil || p.Goal == nil || p.Goal.Objective == "" {
			return
		}
		c.emitNotification("Thread goal: "+p.Goal.Objective, map[string]any{
			"kind":   "thread_goal",
			"status": p.Goal.Status,
		}, "")
	case "thread_rolled_back":
		c.emitNotification("Thread rolled back", map[string]any{"kind": "thread_rolled_back"}, "")
	case "entered_review_mode":
		var p reviewModePayload
		_ = json.Unmarshal(env.Payload, &p)
		c.emitNotification(reviewSummary("Code review started", p.UserFacingHint), map[string]any{
			"kind":  "review_status",
			"title": "Code review started",
		}, "")
	case "exited_review_mode":
		c.emitNotification("Code review finished", map[string]any{
			"kind":  "review_status",
			"title": "Code review finished",
		}, "")

	case "item_completed":
		c.applyItemCompleted(env)

	case "error":
		// Errors are user-facing state, not log entries: an imported
		// session must show the rate-limit or stream failure that ended a
		// turn, exactly as the live session did.
		var p errorPayload
		if json.Unmarshal(env.Payload, &p) != nil || strings.TrimSpace(p.Message) == "" {
			c.corrupt++
			return
		}
		c.ensureTurn()
		meta := map[string]any{}
		if p.Info != "" {
			meta["codexErrorInfo"] = p.Info
		}
		c.emit(provider.ProviderEvent{
			Kind:           provider.EventError,
			Role:           "system",
			Content:        p.Message,
			ContentPresent: true,
			Meta:           metaJSON(meta),
		})

	case "thread_settings_applied", "thread_name_updated":
		// Config echo and rename, not history. Known and deliberately
		// dropped: the per-turn model/effort AO imports comes from
		// `turn_context`, and the thread title comes from the index row.

	default:
		if kind, ok := collabEndKinds[payloadType(env.Payload)]; ok {
			c.applyCollabEnd(env, kind)
			return
		}
		c.countUnknown(env)
	}
}

func reviewSummary(title, hint string) string {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return title
	}
	if len(hint) > 160 {
		hint = strings.TrimSpace(hint[:160]) + "…"
	}
	return title + ": " + hint
}

// applyItemCompleted handles the only two item kinds Codex persists a
// completion for (Plan and Sleep — see rollout/src/policy.rs). A Plan becomes
// the same proposed-plan event a live session produces rather than a generic
// notification, so an imported plan renders in the plan card.
func (c *converter) applyItemCompleted(env envelope) {
	var p itemCompletedPayload
	if json.Unmarshal(env.Payload, &p) != nil || len(p.Item) == 0 {
		c.countUnknown(env)
		return
	}
	var header turnItemHeader
	if json.Unmarshal(p.Item, &header) != nil {
		c.countUnknown(env)
		return
	}
	turnID := strings.TrimSpace(p.TurnID)
	switch strings.ToLower(header.Type) {
	case "plan":
		if strings.TrimSpace(header.Text) == "" {
			return
		}
		c.ensureTurn()
		c.emit(provider.ProviderEvent{
			Kind:      provider.EventProposedPlan,
			TurnID:    turnID,
			ItemID:    header.ID,
			ItemType:  "plan",
			Content:   header.Text,
			Meta:      p.Item,
			Timestamp: c.lastTimestamp,
		})
	case "sleep":
		c.emitNotification("Agent paused", map[string]any{"kind": "sleep"}, "")
	default:
		c.unknown["event_msg/item_completed/"+header.Type]++
	}
}

// ------------------------------------------------------------ response_item

func (c *converter) convertResponseItem(env envelope) {
	switch payloadType(env.Payload) {
	case "message":
		c.convertMessage(env)
	case "reasoning":
		c.convertReasoning(env)
	case "agent_message":
		c.convertInterAgentMessage(env)

	case "function_call", "custom_tool_call", "local_shell_call", "tool_search_call", "web_search_call":
		c.startToolCall(env)
	case "function_call_output", "custom_tool_call_output", "tool_search_output":
		c.completeToolCall(env)

	default:
		c.countUnknown(env)
	}
}

// convertMessage handles `response_item/message`, the model-context mirror of
// the conversation.
//
// Modern rollouts carry BOTH this mirror and the `event_msg` record for the
// same message, and the mirror is the worse source: it also contains developer
// and system injections the user never typed. So it is used only as a fallback
// for files old enough to carry no `event_msg` messages at all.
func (c *converter) convertMessage(env envelope) {
	if c.pre.hasEventMsgMessage {
		return
	}
	var p messagePayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	text, present := contentText(p.Content)
	if !present {
		return
	}
	switch strings.ToLower(strings.TrimSpace(p.Role)) {
	case "user":
		if isInjectedContext(text) {
			return
		}
		c.emitUserText(text)
	case "assistant":
		c.emitAssistantText(text)
	default:
		// developer / system: harness instructions, never transcript.
	}
}

// isInjectedContext reports whether a `role:"user"` message is really an
// environment injection rather than something the user typed. Codex wraps
// those in a single XML-ish element (`<recommended_plugins>…`,
// `<user_instructions>…`). Requiring the text to BOTH open and close the same
// element keeps a real prompt that merely starts with '<' out of the filter.
func isInjectedContext(text string) bool {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "<") {
		return false
	}
	end := strings.IndexAny(trimmed, " \t\r\n>")
	if end <= 1 {
		return false
	}
	name := trimmed[1:end]
	for _, r := range name {
		isNameRune := r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !isNameRune {
			return false
		}
	}
	return strings.HasSuffix(trimmed, "</"+name+">")
}

// convertReasoning handles `response_item/reasoning`.
//
// An item whose only content is `encrypted_content` is SKIPPED ENTIRELY — no
// item, no placeholder (adjudicated). Modern rollouts carry ~30 of these per
// session and AO does not render Codex thinking anyway, so a wall of empty
// thinking cards would be pure noise. Readable summaries (older files, and
// open-weight models' raw reasoning) import as normal thinking rows, unless
// the file also carries the `event_msg/agent_reasoning` twin — which repeats
// the same text verbatim.
func (c *converter) convertReasoning(env envelope) {
	if c.pre.hasEventMsgReasoning {
		return
	}
	var p reasoningPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	text := reasoningText(p)
	if text == "" {
		return
	}
	c.emitThinking(text)
}

func reasoningText(p reasoningPayload) string {
	var b strings.Builder
	for _, blocks := range [][]json.RawMessage{p.Summary, p.Content} {
		for _, raw := range blocks {
			var block map[string]json.RawMessage
			if json.Unmarshal(raw, &block) != nil {
				continue
			}
			part, _ := rawString(block, "text")
			if strings.TrimSpace(part) == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(part)
		}
	}
	return b.String()
}
