package rollout

import (
	"encoding/json"
	"sort"
	"strings"

	"agent-overflow/internal/provider"
)

// Collab-agent records in a PARENT rollout.
//
// A Codex spawn creates a CHILD THREAD with its own rollout file, not a
// nested tool call, and those child threads are excluded from List (they are
// `thread_source = 'subagent'` and appear in `thread_spawn_edges`). What
// reaches the parent's file is the child's lifecycle and its delivered
// result, and both carry the wire's own linkage back to the spawning call:
//
//   - a `started` sub_agent_activity.event_id is the spawning tool call's
//     `call_id`, which records ownership. An `interacted` activity names the
//     message call and settles it as a standalone timeline tool row.
//   - the FINAL_ANSWER delivery (`response_item/agent_message`, or
//     `inter_agent_communication` on 0.142-era files) names the child by
//     `author` agent path, which the activity records already mapped to that
//     same spawning call.
//
// When neither linkage resolves — an activity for a spawn outside the
// imported range — the row is emitted top-level as a notification rather than
// dropped, and never parented under a guess.

type subagentOwnership struct {
	agentPath     string
	agentThreadID string
}

// applySubAgentActivity emits child lifecycle, records the agent-path → spawn
// mapping for later final delivery, and promotes messages to timeline tools.
func (c *converter) applySubAgentActivity(env envelope) {
	var p subAgentActivityPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	agentPath := strings.TrimSpace(p.AgentPath)
	eventID := strings.TrimSpace(p.EventID)
	agentThreadID := strings.TrimSpace(p.AgentThreadID)
	kind := strings.TrimSpace(p.Kind)
	parent := c.toolItemIDs[eventID]
	if kind == "started" {
		parent = c.recordSubagentStart(eventID, agentPath, agentThreadID)
	}
	if kind == "interacted" {
		c.emitCollabInteraction(eventID, agentPath, agentThreadID)
		return
	}
	if kind == "completed" && c.emitSubAgentCompleted(parent, agentPath, agentThreadID, eventID) {
		return
	}
	meta := map[string]any{
		"kind":         "subagent_activity",
		"activityKind": strings.TrimSpace(p.Kind),
	}
	if agentPath != "" {
		meta["agentPath"] = agentPath
	}
	if id := strings.TrimSpace(p.AgentThreadID); id != "" {
		meta["agentThreadId"] = id
	}
	c.emitNotification(subAgentSummary(agentPath, p.Kind), meta, parent)
}

func (c *converter) recordSubagentStart(eventID, agentPath, agentThreadID string) string {
	if eventID == "" || agentPath == "" {
		return c.toolItemIDs[eventID]
	}
	ownership := subagentOwnership{agentPath: agentPath, agentThreadID: agentThreadID}
	if existing, ok := c.subagentStarts[eventID]; ok {
		if existing != ownership {
			c.corrupt++
		}
		return c.toolItemIDs[eventID]
	}
	c.subagentStarts[eventID] = ownership
	ref, found := c.toolRefs[eventID]
	if !found {
		return ""
	}
	parent := ref.itemID
	c.agentParents[agentPath] = ref
	if tool := c.tools[eventID]; tool != nil {
		c.applySubagentOwnership(tool, ownership)
		c.emit(provider.ProviderEvent{
			Kind: provider.EventToolStart, TurnID: tool.turnID, TurnIndex: tool.turnIndex,
			ItemID: tool.itemID, ItemType: tool.itemType, Role: "assistant",
			Meta: c.toolStartMeta(tool), Timestamp: c.lastTimestamp,
		})
		return parent
	}
	input := map[string]any{"tool": "spawn_agent"}
	if agentThreadID != "" {
		input["receiverThreadIds"] = []string{agentThreadID}
	}
	c.emit(provider.ProviderEvent{
		Kind: provider.EventToolStart, TurnID: ref.turnID, TurnIndex: ref.turnIndex,
		ItemID: parent, ItemType: ref.itemType, Role: "assistant",
		Meta: metaJSON(map[string]any{
			"toolName": "collab_agent", "input": input, "meta_update_only": true,
			"is_background": true, "live_background_active": true,
		}),
		Timestamp: c.lastTimestamp,
	})
	return parent
}

func (c *converter) applySubagentOwnership(tool *openTool, ownership subagentOwnership) {
	input := map[string]any{}
	if len(tool.input) > 0 && json.Unmarshal(tool.input, &input) != nil {
		c.corrupt++
		input = map[string]any{}
	}
	input["tool"] = "spawn_agent"
	if ownership.agentThreadID != "" {
		input["receiverThreadIds"] = []string{ownership.agentThreadID}
	}
	if ownership.agentPath != "" {
		input["agentPath"] = ownership.agentPath
	}
	tool.input = metaJSON(input)
	tool.isBackground = true
}

func (c *converter) emitSubAgentCompleted(parent, agentPath, agentThreadID, activityID string) bool {
	ref, hasRef := c.agentParents[agentPath]
	if parent == "" {
		parent = ref.itemID
	}
	if parent == "" || agentThreadID == "" {
		return false
	}
	turnID := ""
	turnIndex := 0
	if hasRef {
		turnID = ref.turnID
		turnIndex = ref.turnIndex
	} else {
		c.ensureTurn()
		turnID = c.turn.id
		turnIndex = c.turn.index
	}
	c.emit(provider.ProviderEvent{
		Kind:            provider.EventSubagentStatus,
		TurnID:          turnID,
		TurnIndex:       turnIndex,
		ItemID:          parent,
		ParentToolUseID: parent,
		Meta: metaJSON(map[string]any{
			"agent_path":       agentThreadID,
			"canonical_path":   agentPath,
			"status":           "completed",
			"activity_call_id": strings.TrimSpace(activityID),
		}),
		Timestamp: c.lastTimestamp,
	})
	return true
}

// emitCollabInteraction projects a parent-to-child message at the point it
// happened. It completes the raw function call when that call is present in
// the imported range; paginated/migrated rollouts may only retain the activity
// item, in which case a self-contained start/complete pair keeps the event
// visible and top-level. Neither path nests it under the historical spawn.
func (c *converter) emitCollabInteraction(eventID, agentPath, agentThreadID string) {
	eventID = strings.TrimSpace(eventID)
	agentPath = strings.TrimSpace(agentPath)
	agentThreadID = strings.TrimSpace(agentThreadID)
	activityTool := ""
	if tool := c.tools[eventID]; tool != nil && eventID != "" {
		if !isCollabMessageToolName(tool.rawToolName) {
			c.corrupt++
			eventID = lineUUID(c.lineStart) + ":collab-interaction"
		} else {
			activityTool = strings.TrimSpace(tool.rawToolName)
			tool.toolName = "send_input"
			tool.itemType = "send_input"
			tool.parentToolUseID = ""
			tool.input = c.collabInteractionInput(tool.input, activityTool, agentPath, agentThreadID)
			c.finishTool(tool, "", "completed", false)
			return
		}
	}

	if eventID == "" {
		eventID = lineUUID(c.lineStart)
	}
	input := c.collabInteractionInput(nil, "", agentPath, agentThreadID)
	c.emitStandaloneCollabActivity(eventID, input)
}

func (c *converter) emitStandaloneCollabActivity(itemID string, input json.RawMessage) {
	c.ensureTurn()
	meta := metaJSON(map[string]any{
		"toolName": "send_input",
		"input":    json.RawMessage(input),
	})
	base := provider.ProviderEvent{
		TurnID:    c.turn.id,
		TurnIndex: c.turn.index,
		ItemID:    itemID,
		ItemType:  "send_input",
		Role:      "assistant",
		Meta:      meta,
		Timestamp: c.lastTimestamp,
	}
	start := base
	start.Kind = provider.EventToolStart
	c.emit(start)
	complete := base
	complete.Kind = provider.EventToolComplete
	complete.ContentPresent = true
	c.emit(complete)
	c.collabActivityRows[itemID] = struct{}{}
}

func (c *converter) collabInteractionInput(existing json.RawMessage, activityTool, agentPath, agentThreadID string) json.RawMessage {
	input := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &input); err != nil {
			// toolInput produces object JSON, so this indicates a malformed
			// imported record. Keep the activity visible with the normalized
			// fields below, while preserving the reader's corruption signal.
			c.corrupt++
			input = map[string]any{}
		}
	}
	input["tool"] = "send_input"
	input["activityKind"] = "interacted"
	if activityTool != "" {
		input["activityTool"] = activityTool
	}
	if agentPath != "" {
		input["target"] = agentPath
	}
	if agentThreadID != "" {
		input["receiverThreadIds"] = []string{agentThreadID}
	}
	return metaJSON(input)
}

func subAgentSummary(agentPath, kind string) string {
	name := strings.TrimSpace(agentPath)
	if name == "" {
		name = "Agent"
	}
	switch strings.TrimSpace(kind) {
	case "started":
		return name + " started"
	case "interacted":
		return name + " received a message"
	case "interrupted":
		return name + " was interrupted"
	case "completed":
		return name + " completed"
	case "":
		return name + " activity"
	default:
		return name + " " + strings.TrimSpace(kind)
	}
}

// convertInterAgentMessage handles `response_item/agent_message`: a message
// delivered between agent threads, of which the FINAL_ANSWER envelope is the
// one that matters — it is the moment a child's result entered the parent's
// context. Its content is a mix of a readable header block and an encrypted
// payload block; only the readable half survives contentText.
func (c *converter) convertInterAgentMessage(env envelope) {
	var p interAgentPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	c.emitInterAgent(p, true)
}

// convertInterAgent handles the standalone envelope forms. The 0.146
// `inter_agent_communication_metadata` line carries only `trigger_turn` and
// no content at all, so it is recognised and dropped rather than counted as
// an unknown type.
func (c *converter) convertInterAgent(env envelope) {
	var p interAgentPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	if strings.TrimSpace(p.Author) == "" && len(p.Content) == 0 {
		return
	}
	c.emitInterAgent(p, false)
}

func (c *converter) emitInterAgent(p interAgentPayload, rawAgentMessage bool) {
	text, present := contentText(p.Content)
	if !present || strings.TrimSpace(text) == "" {
		return
	}
	author := strings.TrimSpace(p.Author)
	messageType, message, mailboxEnvelope := parseInterAgentEnvelope(p.Author, p.Recipient, text)
	progressAuthorized := rawAgentMessage
	if !rawAgentMessage {
		progressAuthorized = p.TriggerTurn != nil && !*p.TriggerTurn && message != ""
	} else if p.TriggerTurn != nil && *p.TriggerTurn {
		progressAuthorized = false
	}
	if mailboxEnvelope && messageType == "MESSAGE" && progressAuthorized {
		input := map[string]any{
			"tool":         "send_input",
			"activityKind": "progress",
		}
		if author != "" {
			input["target"] = author
		}
		if message = boundedRolloutProgress(message); message != "" {
			input["message"] = message
		}
		c.emitStandaloneCollabActivity(lineUUID(c.lineStart)+":collab-progress", metaJSON(input))
		return
	}
	meta := map[string]any{"kind": "agent_message"}
	if author != "" {
		meta["agentPath"] = author
	}
	if recipient := strings.TrimSpace(p.Recipient); recipient != "" {
		meta["recipient"] = recipient
	}
	c.emitNotification(text, meta, c.agentParents[author].itemID)
}

func parseInterAgentEnvelope(author, recipient, text string) (messageType, message string, ok bool) {
	author = strings.TrimSpace(author)
	recipient = strings.TrimSpace(recipient)
	if author == "" || recipient != "/root" || !strings.HasPrefix(text, "Message Type: ") {
		return "", "", false
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) < 5 {
		return "", "", false
	}
	messageType = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[0]), "Message Type:"))
	if messageType != "MESSAGE" && messageType != "FINAL_ANSWER" {
		return "", "", false
	}
	if !strings.HasPrefix(lines[1], "Task name:") ||
		strings.TrimSpace(strings.TrimPrefix(lines[1], "Task name:")) != recipient {
		return "", "", false
	}
	if !strings.HasPrefix(lines[2], "Sender:") ||
		strings.TrimSpace(strings.TrimPrefix(lines[2], "Sender:")) != author {
		return "", "", false
	}
	if strings.TrimSpace(lines[3]) != "Payload:" {
		return "", "", false
	}
	return messageType, strings.TrimSpace(strings.Join(lines[4:], "\n")), true
}

func boundedRolloutProgress(message string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
	line = strings.TrimSpace(line)
	runes := make([]rune, 0, 240)
	truncated := false
	for _, value := range line {
		if len(runes) == 240 {
			truncated = true
			break
		}
		runes = append(runes, value)
	}
	bounded := strings.TrimSpace(string(runes))
	if !truncated {
		return bounded
	}
	return bounded + "\u2026"
}

// collabEndKind names which MultiAgentV1 tool a collab end record finishes.
type collabEndKind struct {
	eventType string
	toolName  string
	verb      string
}

var collabEndKinds = map[string]collabEndKind{
	"collab_agent_spawn_end":       {"collab_agent_spawn_end", "spawn_agent", "Spawned"},
	"collab_agent_interaction_end": {"collab_agent_interaction_end", "send_message", "Messaged"},
	"collab_waiting_end":           {"collab_waiting_end", "wait", "Waited for"},
	"collab_close_end":             {"collab_close_end", "close_agent", "Closed"},
}

// applyCollabEnd folds a MultiAgentV1 collab end record onto the tool call it
// finishes. All four carry the spawning call's `call_id`, so they need no
// special correlation — and the waiting/close variants carry the CHILD'S
// ANSWER, which is the whole point of importing them.
func (c *converter) applyCollabEnd(env envelope, kind collabEndKind) {
	var p collabEndPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	ev := endEvent{
		callID:   p.CallID,
		toolName: kind.toolName,
		itemType: "collab_agent",
		what:     "collab agent result",
		enrich:   toolEnrichment{extra: map[string]any{}},
	}
	agent := firstNonEmpty(p.NewAgentNickname, p.ReceiverAgentNickname)
	if agent != "" {
		ev.enrich.extra["agentNickname"] = agent
	}
	if role := firstNonEmpty(p.NewAgentRole, p.ReceiverAgentRole); role != "" {
		ev.enrich.extra["agentRole"] = role
	}
	if childID := firstNonEmpty(p.NewThreadID, p.ReceiverThreadID); childID != "" {
		ev.enrich.extra["agentThreadId"] = childID
	}
	if p.Prompt != "" {
		ev.input = map[string]any{"prompt": p.Prompt}
	}

	var b strings.Builder
	label, text := collabStatusText(p.Status)
	if label != "" || text != "" {
		writeCollabStatus(&b, kind.verb, agent, label, text)
		if label == "errored" || label == "failed" {
			ev.enrich.isError = true
			ev.enrich.itemStatus = "failed"
		}
	}
	for _, status := range p.AgentStatuses {
		label, text := collabStatusText(status.Status)
		writeCollabStatus(&b, kind.verb, status.AgentNickname, label, text)
	}
	ev.enrich.output = b.String()
	c.applyEndEvent(ev)
}

func writeCollabStatus(b *strings.Builder, verb, agent, label, text string) {
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	header := verb
	if agent != "" {
		header += " " + agent
	}
	if label != "" {
		header += " (" + label + ")"
	}
	b.WriteString(header)
	if text != "" {
		b.WriteString("\n")
		b.WriteString(text)
	}
}

// collabStatusText decodes Codex's AgentStatus: unit variants serialize as a
// bare string ("running"), terminal variants as a single-key object
// ({"completed": "<the child's answer>"}).
func collabStatusText(raw json.RawMessage) (label, text string) {
	if len(raw) == 0 {
		return "", ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return asString, ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return "", ""
	}
	for _, key := range sortedRawKeys(obj) {
		var body string
		_ = json.Unmarshal(obj[key], &body)
		return key, body
	}
	return "", ""
}

func sortedRawKeys(obj map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
