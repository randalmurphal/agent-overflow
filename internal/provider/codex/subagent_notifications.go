package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"regexp"
	"strings"
)

// subagentNotification is the common child-result signal emitted when Codex
// delivers a result into parent model context. Legacy Codex sends a
// <subagent_notification>{...}</subagent_notification> tag; MultiAgentV2 sends
// an agent_message FINAL_ANSWER envelope. See
// codex-source's `core/src/context/subagent_notification.rs` for the
// tag constants and `core/tests/suite/subagent_notifications.rs` for
// the canonical wire shape.
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
	AgentPath string `json:"agent_path"`
	Status    string `json:"status"`
	Message   string `json:"-"`
	// MessageType is the mailbox envelope's own `Message Type:` header —
	// FINAL_ANSWER for a child's terminal answer, MESSAGE for a mid-run
	// progress note. Empty for the legacy <subagent_notification> carriers.
	MessageType     string         `json:"-"`
	MailboxDelivery bool           `json:"-"`
	DeliveryID      string         `json:"-"`
	Extra           map[string]any `json:"-"`
}

const (
	interAgentMessageTypePrefix = "Message Type: "
	interAgentFinalAnswerType   = "FINAL_ANSWER"
	interAgentProgressType      = "MESSAGE"
)

// extractSubagentCompletionFromRawAgentMessageItem parses the MultiAgentV2
// rollout item Codex records when a child's final answer is drained from the
// parent mailbox into model context. Child turn/completed is intentionally not
// enough: this record is the exact transcript presentation boundary.
func extractSubagentCompletionFromRawAgentMessageItem(item map[string]json.RawMessage) (subagentNotification, bool) {
	if strings.TrimSpace(readRawString(item, "type")) != "agent_message" {
		return subagentNotification{}, false
	}
	header, tailDigest, ok := rawMailboxEnvelopeText(item)
	if !ok {
		return subagentNotification{}, false
	}
	notification, ok := parseInterAgentMailboxEnvelope(
		readRawString(item, "author"),
		readRawString(item, "recipient"),
		header,
	)
	if !ok {
		return subagentNotification{}, false
	}
	notification.DeliveryID = interAgentContentDeliveryID(notification, tailDigest)
	return notification, true
}

// extractSubagentCompletionFromInterAgentCommunication reads the DURABLE
// mailbox record: a plain `content` string and nothing else.
//
// That carrier cannot identify an ENCRYPTED progress beat, and is refused for
// one. A MESSAGE envelope's payload is the only thing that separates two of
// them, and an encrypted envelope's plaintext stops at "Payload:\n" — so every
// encrypted progress note from one child renders here as the SAME
// (agent path, MESSAGE, "", "") tuple. Admitting it would hand one delivery a
// second identity (the raw `agent_message` carrier keys the same beat on its
// ciphertext tail digest, which this record does not have), duplicating the
// standalone progress activity, while every later beat from that child collapsed
// onto the one id this carrier can mint. The raw carrier is the only one that
// can tell these apart, so it is the only one allowed to report them.
//
// FINAL_ANSWER and PLAINTEXT progress are unaffected: their payload is in the
// plaintext header, both carriers see the same bytes, and
// interAgentContentDeliveryID already drops the tail for FINAL_ANSWER so the
// two agree on one id.
func extractSubagentCompletionFromInterAgentCommunication(payload map[string]json.RawMessage) (subagentNotification, bool) {
	var triggerTurn *bool
	if raw, ok := payload["trigger_turn"]; ok {
		_ = json.Unmarshal(raw, &triggerTurn)
	}
	if triggerTurn == nil || *triggerTurn {
		return subagentNotification{}, false
	}
	notification, ok := parseInterAgentMailboxEnvelope(
		readRawString(payload, "author"),
		readRawString(payload, "recipient"),
		readRawString(payload, "content"),
	)
	if !ok {
		return subagentNotification{}, false
	}
	if notification.MessageType == interAgentProgressType && notification.Message == "" {
		return subagentNotification{}, false
	}
	notification.DeliveryID = interAgentContentDeliveryID(notification, "")
	return notification, true
}

// interAgentContentDeliveryID is the delivery identity for one mailbox record.
//
// It is deliberately NOT derived from
// `internal_chat_message_metadata_passthrough.turn_id`: that field is the
// RECEIVING PARENT turn, which is constant across every delivery drained into
// one parent turn (corpus: rollout-2026-08-20T16-16-28-01a020d1-* records 686
// and 763, two distinct FINAL_ANSWERs sharing turn_id
// 01a020d1-a06b-7b71-9791-749c71f19cd7). Using it collapsed a child's second
// answer onto the first row.
//
// Hashing (agent path, envelope type, payload) instead makes every distinct
// delivery distinct, while the two carriers of ONE delivery — the live
// `rawResponseItem/completed` stream and the rollout tail — still agree, which
// is what `claimSubagentNotification` needs to dedupe them.
// tailDigest covers the non-text content blocks (Codex's `encrypted_content`
// half of an encrypted envelope). It is the only thing that distinguishes two
// MESSAGE progress deliveries, whose plaintext header ends at "Payload:\n" and
// whose body never leaves the ciphertext.
//
// It is deliberately NOT mixed in for FINAL_ANSWER. Only ONE of the two
// carriers can ever produce a tail: the raw `agent_message` response item
// carries the ciphertext block, while the durable `inter_agent_communication`
// rollout record carries a plain `content` string and nothing else. Mixing the
// tail in unconditionally therefore gave the SAME encrypted delivery two
// different ids depending on which carrier saw it first, which is precisely
// the duplicate row this content key exists to prevent. A FINAL_ANSWER's
// terminal payload is in the plaintext header, so (agent path, type, payload)
// already separates two distinct answers; MESSAGE keeps the tail because its
// payload is not.
//
// That leaves MESSAGE with the same split the tail was added to fix, one level
// down: an ENCRYPTED progress beat is identifiable ONLY through the tail, so
// the carrier that has no tail cannot name it. That is resolved where the
// carriers are read rather than here — the durable carrier refuses an
// encrypted progress beat outright
// (extractSubagentCompletionFromInterAgentCommunication) instead of minting a
// second, degenerate id for a delivery the raw carrier already identified.
func interAgentContentDeliveryID(notification subagentNotification, tailDigest string) string {
	if notification.MessageType == interAgentFinalAnswerType {
		tailDigest = ""
	}
	digest := sha256.Sum256([]byte(
		notification.AgentPath + "\x00" +
			notification.MessageType + "\x00" +
			notification.Message + "\x00" +
			tailDigest,
	))
	return "content:" + hex.EncodeToString(digest[:])
}

// parseInterAgentMailboxEnvelope validates the strict child -> parent mailbox
// envelope Codex renders into parent model context and classifies it by its own
// `Message Type:` header. Two types reach the root mailbox:
//
//   - FINAL_ANSWER — the child's terminal answer. Transcript completion.
//   - MESSAGE      — a mid-run progress note (`send_message`, QueueOnly). NOT
//     terminal: the child keeps running, and on an encrypted
//     envelope the payload never leaves the ciphertext, so the
//     plaintext header stops at "Payload:\n".
//
// NEW_TASK envelopes are parent -> child and never carry `/root` as recipient,
// so they cannot reach this parser.
func parseInterAgentMailboxEnvelope(author, recipient, text string) (subagentNotification, bool) {
	author = strings.TrimSpace(author)
	recipient = strings.TrimSpace(recipient)
	if author == "" || recipient != "/root" || !strings.HasPrefix(text, interAgentMessageTypePrefix) {
		return subagentNotification{}, false
	}

	typeLine, rest, ok := strings.Cut(text, "\n")
	if !ok {
		return subagentNotification{}, false
	}
	messageType := strings.TrimSpace(strings.TrimPrefix(typeLine, interAgentMessageTypePrefix))
	status := ""
	switch messageType {
	case interAgentFinalAnswerType:
		status = "completed"
	case interAgentProgressType:
		// Progress only. Live-state evidence, never a terminal status —
		// inferring "still running" from a delivery is exactly the kind of
		// heuristic invariant 25 forbids, so this rides the wire-typed
		// envelope header and nothing else.
		status = "running"
	default:
		return subagentNotification{}, false
	}

	taskLine, rest, ok := strings.Cut(rest, "\n")
	if !ok || strings.TrimSpace(strings.TrimPrefix(taskLine, "Task name:")) != recipient ||
		!strings.HasPrefix(taskLine, "Task name:") {
		return subagentNotification{}, false
	}
	senderLine, rest, ok := strings.Cut(rest, "\n")
	if !ok || !strings.HasPrefix(senderLine, "Sender:") ||
		strings.TrimSpace(strings.TrimPrefix(senderLine, "Sender:")) != author {
		return subagentNotification{}, false
	}
	payloadHeader, payload, ok := strings.Cut(rest, "\n")
	if !ok || strings.TrimSpace(payloadHeader) != "Payload:" {
		return subagentNotification{}, false
	}
	return subagentNotification{
		AgentPath:       author,
		Status:          status,
		MessageType:     messageType,
		Message:         strings.TrimSpace(payload),
		MailboxDelivery: true,
	}, true
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
	notifications, _ := parseSubagentNotificationsWithCarrierRemainder(text)
	return notifications
}

func parseSubagentNotificationsWithCarrierRemainder(text string) ([]subagentNotification, string) {
	if text == "" || !strings.Contains(text, "<subagent_notification>") {
		return nil, text
	}
	matches := subagentNotificationPattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil, text
	}
	notifications := make([]subagentNotification, 0, len(matches))
	var remainder strings.Builder
	lastEnd := 0
	for _, match := range matches {
		if len(match) < 4 {
			continue
		}
		start, end := match[0], match[1]
		bodyStart, bodyEnd := match[2], match[3]
		body := strings.TrimSpace(text[bodyStart:bodyEnd])
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
		status, statusMessage, hasStatusMessage := normalizeSubagentStatus(raw["status"])
		if agentPath == "" || status == "" {
			continue
		}
		remainder.WriteString(text[lastEnd:start])
		lastEnd = end
		delete(raw, "agent_path")
		delete(raw, "agent_id")
		delete(raw, "status")
		message, messageIsString := raw["message"].(string)
		if messageIsString {
			delete(raw, "message")
		}
		if message == "" && hasStatusMessage {
			if text, ok := statusMessage.(string); ok {
				message = text
			} else if _, exists := raw["message"]; !exists {
				raw["message"] = statusMessage
			}
		}
		notifications = append(notifications, subagentNotification{
			AgentPath: agentPath,
			Status:    status,
			Message:   message,
			Extra:     raw,
		})
	}
	if len(notifications) == 0 {
		return nil, text
	}
	remainder.WriteString(text[lastEnd:])
	return notifications, remainder.String()
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
	notifications, _ := extractSubagentNotificationsAndRemainderFromUserMessage(params)
	return notifications
}

func extractSubagentNotificationsAndRemainderFromUserMessage(params json.RawMessage) ([]subagentNotification, string) {
	item := readNestedObject(params, "item")
	if item == nil {
		return nil, ""
	}
	if readRawString(item, "type") != "userMessage" {
		return nil, ""
	}
	contentRaw, ok := item["content"]
	if !ok {
		return nil, ""
	}
	var content []map[string]json.RawMessage
	if err := json.Unmarshal(contentRaw, &content); err != nil {
		return nil, ""
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
	return parseSubagentNotificationsWithCarrierRemainder(builder.String())
}

type interAgentCommunicationMessage struct {
	Author          string   `json:"author"`
	Recipient       string   `json:"recipient"`
	OtherRecipients []string `json:"other_recipients"`
	Content         string   `json:"content"`
	TriggerTurn     *bool    `json:"trigger_turn"`
}

// extractSubagentNotificationsAndRemainderFromRawInterAgentMessageItem inspects
// the raw Responses API item Codex emits when it records a mailbox-delivered
// InterAgentCommunication into the parent turn. The raw item is intentionally
// not a public app-server timeline event, so keep this parser narrow:
// message item, assistant role, commentary phase, exactly one
// output_text content block containing the InterAgentCommunication JSON
// wrapper, and notification tags inside the wrapper's content field.
func extractSubagentNotificationsAndRemainderFromRawInterAgentMessageItem(item map[string]json.RawMessage) ([]subagentNotification, string) {
	if strings.TrimSpace(readRawString(item, "role")) != "assistant" {
		return nil, ""
	}
	if strings.TrimSpace(readRawString(item, "phase")) != "commentary" {
		return nil, ""
	}

	text, ok := rawMessageSingleTextOfType(item, "output_text")
	if !ok {
		return nil, ""
	}
	if !strings.Contains(text, "subagent_notification") {
		return nil, ""
	}

	var communication interAgentCommunicationMessage
	if err := json.Unmarshal([]byte(text), &communication); err != nil {
		return nil, ""
	}
	if strings.TrimSpace(communication.Author) == "" ||
		strings.TrimSpace(communication.Recipient) == "" ||
		strings.TrimSpace(communication.Content) == "" ||
		communication.TriggerTurn == nil ||
		*communication.TriggerTurn {
		return nil, ""
	}

	notifications, remainder := parseSubagentNotificationsWithCarrierRemainder(communication.Content)
	if !interAgentNotificationAuthorMatches(communication.Author, notifications) {
		return nil, ""
	}
	return notifications, remainder
}

// extractSubagentNotificationsAndRemainderFromRawUserMessageItem inspects the
// current Codex contextual-user mailbox shape. Core stores these
// model-visible session markers as raw Responses API message items with
// role=user and a single input_text block containing only the rendered
// <subagent_notification> fragment.
func extractSubagentNotificationsAndRemainderFromRawUserMessageItem(item map[string]json.RawMessage) ([]subagentNotification, string) {
	if strings.TrimSpace(readRawString(item, "role")) != "user" {
		return nil, ""
	}
	// Contextual user fragments are not phase-scoped. If upstream starts
	// putting these in a phase, add that exact observed shape rather than
	// widening this parser to arbitrary raw user messages.
	if strings.TrimSpace(readRawString(item, "phase")) != "" {
		return nil, ""
	}

	text, ok := rawMessageSingleTextOfType(item, "input_text")
	if !ok || !strings.Contains(text, "subagent_notification") {
		return nil, ""
	}
	return parseSubagentNotificationsWithCarrierRemainder(text)
}

func interAgentNotificationAuthorMatches(author string, notifications []subagentNotification) bool {
	author = strings.TrimSpace(author)
	if author == "" || len(notifications) == 0 {
		return false
	}
	for _, notification := range notifications {
		if strings.TrimSpace(notification.AgentPath) != author {
			return false
		}
	}
	return true
}

// rawMailboxEnvelopeText reads the mailbox envelope's plaintext header out of a
// raw `agent_message` item and digests whatever content blocks follow it.
//
// The strict single-block form (`rawMessageSingleTextOfType`) is what a
// plaintext FINAL_ANSWER looks like, but an ENCRYPTED envelope ships two blocks
// — the plaintext `input_text` header plus an `encrypted_content` body (corpus:
// child rollout 01a020e0-* records 232 / 340 / 350). Requiring exactly one made
// every encrypted delivery invisible. The header must still lead, and the tail
// is never rendered: it is ciphertext. It is digested only so two progress
// deliveries with identical headers stay distinguishable.
func rawMailboxEnvelopeText(item map[string]json.RawMessage) (header string, tailDigest string, ok bool) {
	contentRaw, present := item["content"]
	if !present || len(contentRaw) == 0 {
		return "", "", false
	}
	// Decoded as raw blocks: only the first one is inspected field-by-field,
	// and the tail is hashed as the bytes the wire actually carried instead of
	// being re-marshalled through a map (which would sort keys and reallocate
	// every ciphertext block just to digest it).
	var blocks []json.RawMessage
	if json.Unmarshal(contentRaw, &blocks) != nil || len(blocks) == 0 {
		return "", "", false
	}
	var head map[string]json.RawMessage
	if json.Unmarshal(blocks[0], &head) != nil {
		return "", "", false
	}
	if strings.TrimSpace(readRawString(head, "type")) != "input_text" {
		return "", "", false
	}
	header, present = readRawStringPresent(head, "text")
	if !present {
		return "", "", false
	}
	if len(blocks) == 1 {
		return header, "", true
	}
	hash := sha256.New()
	for _, block := range blocks[1:] {
		var typed struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(block, &typed) != nil {
			return "", "", false
		}
		// A trailing TEXT block would be envelope content this parser does not
		// model; refuse rather than silently dropping it.
		if strings.TrimSpace(typed.Type) == "input_text" {
			return "", "", false
		}
		_, _ = hash.Write(block)
	}
	return header, hex.EncodeToString(hash.Sum(nil)), true
}

func rawMessageSingleTextOfType(item map[string]json.RawMessage, allowedType string) (string, bool) {
	contentRaw, ok := item["content"]
	if !ok || len(contentRaw) == 0 {
		return "", false
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(contentRaw, &blocks) != nil || len(blocks) != 1 {
		return "", false
	}
	block := blocks[0]
	if strings.TrimSpace(readRawString(block, "type")) != allowedType {
		return "", false
	}
	text, present := readRawStringPresent(block, "text")
	return text, present
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
	if n.Message != "" {
		fields["message"] = n.Message
	}
	if n.MessageType != "" {
		fields["message_type"] = n.MessageType
	}
	if n.MailboxDelivery {
		fields["mailbox_delivery"] = true
	}
	if n.DeliveryID != "" {
		fields["delivery_id"] = n.DeliveryID
	}
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
