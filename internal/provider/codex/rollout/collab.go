package rollout

import (
	"encoding/json"
	"sort"
	"strings"
)

// Collab-agent records in a PARENT rollout.
//
// A Codex spawn creates a CHILD THREAD with its own rollout file, not a
// nested tool call, and those child threads are excluded from List (they are
// `thread_source = 'subagent'` and appear in `thread_spawn_edges`). What
// reaches the parent's file is the child's lifecycle and its delivered
// result, and both carry the wire's own linkage back to the spawning call:
//
//   - `sub_agent_activity.event_id` IS the spawning tool call's `call_id`
//     (codex-rs SubAgentActivityEvent), which is how an activity row is
//     parented under the `spawn_agent` row.
//   - the FINAL_ANSWER delivery (`response_item/agent_message`, or
//     `inter_agent_communication` on 0.142-era files) names the child by
//     `author` agent path, which the activity records already mapped to that
//     same spawning call.
//
// When neither linkage resolves — an activity for a spawn outside the
// imported range — the row is emitted top-level as a notification rather than
// dropped, and never parented under a guess.

// applySubAgentActivity emits the child-lifecycle row and records the
// agent-path → parent-row mapping the later delivery is parented by.
func (c *converter) applySubAgentActivity(env envelope) {
	var p subAgentActivityPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	agentPath := strings.TrimSpace(p.AgentPath)
	parent := c.toolItemIDs[strings.TrimSpace(p.EventID)]
	if parent != "" && agentPath != "" {
		c.agentParents[agentPath] = parent
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
	c.emitInterAgent(p)
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
	c.emitInterAgent(p)
}

func (c *converter) emitInterAgent(p interAgentPayload) {
	text, present := contentText(p.Content)
	if !present || strings.TrimSpace(text) == "" {
		return
	}
	author := strings.TrimSpace(p.Author)
	meta := map[string]any{"kind": "agent_message"}
	if author != "" {
		meta["agentPath"] = author
	}
	if recipient := strings.TrimSpace(p.Recipient); recipient != "" {
		meta["recipient"] = recipient
	}
	c.emitNotification(text, meta, c.agentParents[author])
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
