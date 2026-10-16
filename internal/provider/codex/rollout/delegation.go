package rollout

import (
	"encoding/json"
	"strings"

	"agent-overflow/internal/provider"
)

// Delegated input: Codex's own app and TUI let one thread start or message
// another through the `create_thread` / `send_message_to_thread` tools
// (namespaces `codex_app` and `codex_tui`). The receiving thread gets the
// text as a function-call output submitted as turn input, not as a user
// message, so a thread opened this way has no user message at all.
//
// Codex writes that input twice (core/src/hook_runtime.rs,
// record_pending_input): first a `response_item/function_call_output` with
// `id`, `name` and `namespace` but no `call_id`, then an
// `event_msg/item_completed` carrying `TurnItem::FunctionCallOutput` with the
// same id. Both are persisted in legacy and paginated files
// (rollout/src/policy.rs). The response item is the source, as it is for
// every other message (see items.go); the item only stands in when the
// mirror is missing.
//
// The row is a user row stamped with DelegatedInputOrigin: it is the prompt
// the thread answers, written by another thread.

// DelegatedInputOrigin is the `items.meta.origin` of a user row another Codex
// thread wrote through Codex's delegation tools.
const DelegatedInputOrigin = "codex-delegation"

// MetaDelegationSourceThreadKey names the Codex thread a delegated input came
// from, when the wrapper carries it.
const MetaDelegationSourceThreadKey = "codex_source_thread_id"

// isDelegatedInput mirrors the tool identity check in Codex's
// delegated_output_preview (codex-rs/state/src/delegated_preview.rs).
func isDelegatedInput(namespace, name string) bool {
	switch namespace {
	case "codex_app", "codex_tui":
	default:
		return false
	}
	return name == "create_thread" || name == "send_message_to_thread"
}

// delegatedInputText unwraps the `<codex_delegation>` envelope exactly as
// delegated_output_preview does, so the opening message reads the same as
// the `preview` Codex lists the thread under. Text that is not a complete
// envelope is returned as it is, again as upstream does.
func delegatedInputText(text string) (input, sourceThreadID string) {
	const (
		head      = "<codex_delegation>\n  <source_thread_id>"
		separator = "</source_thread_id>\n  <input>"
		tail      = "</input>\n</codex_delegation>"
	)
	rest, ok := strings.CutPrefix(text, head)
	if !ok {
		return text, ""
	}
	source, body, ok := strings.Cut(rest, separator)
	if !ok {
		return text, ""
	}
	body, ok = strings.CutSuffix(body, tail)
	if !ok {
		return text, ""
	}
	return unescapeDelegation(body), unescapeDelegation(source)
}

// unescapeDelegation reverses the envelope's escaping in upstream's order
// (tui/src/dynamic_tools.rs, parse_delegated_prompt).
func unescapeDelegation(s string) string {
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return strings.ReplaceAll(s, "&amp;", "&")
}

// emitDelegatedInput writes the delegated prompt as a user row. id is the
// Codex item id both records share; it settles the twin.
func (c *converter) emitDelegatedInput(id string, output json.RawMessage) {
	if id != "" {
		key := "delegation:" + id
		if _, done := c.itemRows[key]; done {
			return
		}
		c.itemRows[key] = struct{}{}
	}
	text, _ := contentText(output)
	input, source := delegatedInputText(text)
	if strings.TrimSpace(input) == "" {
		return
	}
	meta := map[string]any{"origin": DelegatedInputOrigin}
	if source = strings.TrimSpace(source); source != "" {
		meta[MetaDelegationSourceThreadKey] = source
	}
	c.ensureTurn()
	c.emit(provider.ProviderEvent{
		Kind:           provider.EventUserText,
		Role:           "user",
		ItemID:         id,
		Content:        input,
		ContentPresent: true,
		Meta:           metaJSON(meta),
	})
}

// delegatedOutputItem is the slice of TurnItem::FunctionCallOutput the
// delegation check reads.
type delegatedOutputItem struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Output    json.RawMessage `json:"output"`
}
