package codex

import (
	"encoding/json"
	"log"
	"regexp"
	"strings"
)

// subagentNotification is the parsed shape of a single
// <subagent_notification>{...}</subagent_notification> tag that Codex
// core injects into the NEXT user-message item when a detached child
// agent has reached a terminal state without a parent `wait`
// outstanding. See codex-source's `core/src/contextual_user_message.rs`
// for the tag constants and `core/tests/suite/subagent_notifications.rs`
// for the canonical wire shape (test fixture, lines 274-296).
//
// The tag payload is a JSON object with at least `agent_path` and
// `status`; `status` is serialized from Codex core's AgentStatus. Unit
// variants are strings ("running", "shutdown", ...), while terminal
// variants with payloads can be objects ({"completed":"done"},
// {"errored":"boom"}).
// Additional fields may appear in future Codex versions — we preserve
// them in `extra` so downstream can opt into richer rendering without
// a parser update.
//
// The wire field is `agent_path` (codex-source's
// core/src/session_prefix.rs::format_subagent_notification_message).
// An older field name `agent_id` is accepted as a fallback for
// backward compatibility with any pre-rename builds; `agent_path` is
// the fast path and what we forward on.
type subagentNotification struct {
	AgentPath string         `json:"agent_path"`
	Status    string         `json:"status"`
	Extra     map[string]any `json:"-"`
}

// subagentNotificationPattern matches a single
// <subagent_notification>...</subagent_notification> block with lenient
// whitespace handling. The inner body is captured in group 1 and then
// JSON-decoded. `(?s)` makes `.` cross newlines — Codex's core writes
// the tag on a single line today but the tests pin a single-line shape
// and humans might paste multi-line bodies during reproduction.
var subagentNotificationPattern = regexp.MustCompile(`(?s)<subagent_notification>\s*(.*?)\s*</subagent_notification>`)

// parseSubagentNotifications extracts every
// <subagent_notification>...</subagent_notification> tag in text,
// JSON-decoding each body into a subagentNotification. Malformed tags
// (body isn't JSON, or missing agent_path / status) are silently
// skipped — surfacing a parse error would leak a Codex-core detail
// into our UI and a single broken notification should not block the
// user turn from rendering. Multiple tags per text are supported and
// returned in the order they appear.
//
// The production wire field is `agent_path`; an older `agent_id`
// field is accepted as a fallback so pre-rename Codex builds still
// round-trip correctly.
func parseSubagentNotifications(text string) []subagentNotification {
	if text == "" || !strings.Contains(text, "<subagent_notification>") {
		return nil
	}
	matches := subagentNotificationPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	notifications := make([]subagentNotification, 0, len(matches))
	for _, match := range matches {
		body := strings.TrimSpace(match[1])
		if body == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			continue
		}
		// Production wire: `agent_path`. Legacy fallback: `agent_id`.
		// Check agent_path first so the fast path doesn't pay for the
		// fallback when the field is present.
		agentPath, _ := raw["agent_path"].(string)
		if agentPath == "" {
			agentPath, _ = raw["agent_id"].(string)
		}
		status, message, hasMessage := normalizeSubagentStatus(raw["status"])
		if agentPath == "" || status == "" {
			continue
		}
		delete(raw, "agent_path")
		delete(raw, "agent_id")
		delete(raw, "status")
		if hasMessage {
			if _, exists := raw["message"]; !exists {
				raw["message"] = message
			}
		}
		notifications = append(notifications, subagentNotification{
			AgentPath: agentPath,
			Status:    status,
			Extra:     raw,
		})
	}
	if len(notifications) == 0 {
		return nil
	}
	return notifications
}

func normalizeSubagentStatus(raw any) (status string, message any, hasMessage bool) {
	switch value := raw.(type) {
	case string:
		return value, nil, false
	case map[string]any:
		if status, ok := value["status"].(string); ok {
			message, hasMessage := value["message"]
			return status, message, hasMessage
		}
		for _, variant := range []string{
			"completed",
			"errored",
			"interrupted",
			"shutdown",
			"not_found",
			"notFound",
			"pending_init",
			"pendingInit",
			"running",
		} {
			message, ok := value[variant]
			if !ok {
				continue
			}
			return canonicalSubagentStatus(variant), message, message != nil
		}
	}
	return "", nil, false
}

func canonicalSubagentStatus(status string) string {
	switch status {
	case "not_found":
		return "notFound"
	case "pending_init":
		return "pendingInit"
	default:
		return status
	}
}

// extractSubagentNotificationsFromUserMessage inspects an item/completed
// params envelope and returns any subagent notifications the
// user-message's content carries. Returns nil when the item is not a
// userMessage, the content array is missing, or no tags are present.
// The wire shape is:
//
//	params.item.type == "userMessage"
//	params.item.content: [{type: "text", text: "...<subagent_notification>...</subagent_notification>..."}, ...]
//
// We concatenate every text UserInput entry before running the regex so
// a notification split across multiple entries (shouldn't happen today
// but cheap to tolerate) is still detected.
func extractSubagentNotificationsFromUserMessage(params json.RawMessage) []subagentNotification {
	item := readNestedObject(params, "item")
	if item == nil {
		return nil
	}
	if readRawString(item, "type") != "userMessage" {
		return nil
	}
	contentRaw, ok := item["content"]
	if !ok {
		return nil
	}
	var content []map[string]json.RawMessage
	if err := json.Unmarshal(contentRaw, &content); err != nil {
		return nil
	}
	var builder strings.Builder
	for _, entry := range content {
		// UserInput tagged union: only the "text" variant carries the
		// notification payload; image / localImage / skill / mention
		// variants don't. See codex-source v2/UserInput.ts.
		var entryType string
		if rawType, ok := entry["type"]; ok {
			_ = json.Unmarshal(rawType, &entryType)
		}
		if entryType != "text" {
			continue
		}
		rawText, ok := entry["text"]
		if !ok {
			continue
		}
		var text string
		if err := json.Unmarshal(rawText, &text); err != nil {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(text)
	}
	return parseSubagentNotifications(builder.String())
}

// buildSubagentNotificationMeta serialises a parsed subagentNotification
// into the json.RawMessage the triage handler forwards on
// `provider:subagent_notification`. Any `Extra` fields are merged onto
// the top level so future Codex core versions can add fields without a
// parser update; the load-bearing `agent_path` / `status` keys win on
// collision so a stray Extra entry can't poison the contract.
func buildSubagentNotificationMeta(n subagentNotification) json.RawMessage {
	fields := make(map[string]any, 2+len(n.Extra))
	for k, v := range n.Extra {
		fields[k] = v
	}
	fields["agent_path"] = n.AgentPath
	fields["status"] = n.Status
	meta, err := json.Marshal(fields)
	if err != nil {
		// Fallback to a minimal payload so the frontend still gets the
		// load-bearing fields even if a malformed Extra entry trips
		// Marshal. The parser already validated agent_path/status are
		// strings, so this encode is safe.
		minimal, _ := json.Marshal(map[string]string{
			"agent_path": n.AgentPath,
			"status":     n.Status,
		})
		log.Printf("codex: subagent_notification meta marshal fallback: %v", err)
		return minimal
	}
	return meta
}
