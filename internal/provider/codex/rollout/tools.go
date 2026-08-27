package rollout

import (
	"encoding/json"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// openTool is a tool call awaiting its output.
//
// Every correlation in this file is by `call_id` — the key the wire itself
// provides on the call, on its output, and on every end event
// (`ExecCommandEndEvent`, `PatchApplyEndEvent`, `McpToolCallEndEvent`,
// `WebSearchEndEvent` all declare it). There is deliberately no matching by
// command string or arrival order anywhere: a weaker key than the one the
// wire hands us would create wrong pairings that look right.
type openTool struct {
	callID          string
	itemID          string
	itemType        string
	toolName        string
	rawToolName     string
	input           json.RawMessage
	command         string
	turnID          string
	turnIndex       int
	parentToolUseID string
	startedAt       time.Time
	// wireStatus is the call record's own `status`, used when the turn ends
	// before an output line arrived.
	wireStatus string
	// selfCompleting marks a call shape that has NO separate `*_output`
	// response item on the wire — `web_search_call` is the one such shape,
	// and treating it like an unfinished tool would mark ~99% of all
	// searches as unresolved.
	selfCompleting bool
	isBackground   bool

	// enrich holds an end event that arrived before the tool's output
	// line, which is the normal order (a rollout writes
	// `exec_command_end` between the call and its `function_call_output`).
	enrich *toolEnrichment
}

type toolReference struct {
	itemID    string
	itemType  string
	turnID    string
	turnIndex int
}

// toolEnrichment is what an end event contributes to the completion.
type toolEnrichment struct {
	command    string
	cwd        string
	exitCode   *int
	isError    bool
	itemStatus string
	output     string
	diffPatch  string
	extra      map[string]any
}

// startToolCall opens a tool call from a `response_item/*_call` line.
func (c *converter) startToolCall(env envelope) {
	var p responseCallPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	kind := payloadType(env.Payload)
	callID := strings.TrimSpace(p.CallID)
	if callID == "" {
		callID = strings.TrimSpace(p.ID)
	}
	if _, projected := c.collabActivityRows[callID]; projected && callID != "" {
		if isCollabMessageToolName(p.Name) {
			return
		}
		// The typed activity claimed this id as a message, but the raw call
		// says it belongs to a different tool. Preserve the raw tool and make
		// the malformed collision visible instead of silently relabelling it.
		c.corrupt++
	}
	if _, done := c.itemRows[callID]; done && callID != "" {
		// A paginated `item_completed` already emitted the complete row
		// for this call one line earlier, with detail this response item
		// does not carry. Opening a second row would duplicate the tool.
		return
	}
	if callID == "" {
		// Nothing to correlate on. Still worth a row: the call happened.
		// It will settle as unresolved at the turn boundary.
		callID = lineUUID(c.lineStart)
	}
	toolName, itemType := toolIdentity(kind, p.Name)
	input, command := c.toolInput(p, toolName)

	c.ensureTurn()
	turnID := c.turn.id
	if p.Passthru != nil && strings.TrimSpace(p.Passthru.TurnID) != "" {
		turnID = strings.TrimSpace(p.Passthru.TurnID)
	}
	tool := &openTool{
		callID:      callID,
		itemID:      callID,
		itemType:    itemType,
		toolName:    toolName,
		rawToolName: strings.TrimSpace(p.Name),
		input:       input,
		command:     command,
		turnID:      turnID,
		turnIndex:   c.turn.index,
		startedAt:   c.lastTimestamp,
		wireStatus:  strings.TrimSpace(p.Status),

		selfCompleting: kind == "web_search_call",
	}
	if _, exists := c.tools[callID]; !exists {
		c.toolOrder = append(c.toolOrder, callID)
	}
	c.tools[callID] = tool
	// Kept for the whole file, unlike c.tools: a spawn_agent call settles
	// long before the child activity that has to be parented under it.
	c.toolItemIDs[callID] = tool.itemID
	ref := toolReference{itemID: tool.itemID, itemType: tool.itemType, turnID: tool.turnID, turnIndex: tool.turnIndex}
	c.toolRefs[callID] = ref
	if ownership, ok := c.subagentStarts[callID]; ok {
		c.applySubagentOwnership(tool, ownership)
		c.agentParents[ownership.agentPath] = ref
	}

	c.emit(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		TurnID:    turnID,
		ItemID:    tool.itemID,
		ItemType:  itemType,
		Role:      "assistant",
		Meta:      c.toolStartMeta(tool),
		Timestamp: c.lastTimestamp,
	})
}

func (c *converter) toolStartMeta(tool *openTool) json.RawMessage {
	meta := map[string]any{"toolName": tool.toolName}
	if len(tool.input) > 0 {
		meta["input"] = json.RawMessage(tool.input)
	}
	if tool.rawToolName != "" && tool.rawToolName != tool.toolName {
		meta["codexToolName"] = tool.rawToolName
	}
	if tool.callID != "" {
		meta["call_id"] = tool.callID
	}
	if tool.parentToolUseID != "" {
		meta["parent_tool_use_id"] = tool.parentToolUseID
	}
	if tool.isBackground {
		meta["is_background"] = true
		meta["live_background_active"] = true
	}
	return metaJSON(meta)
}

// toolIdentity maps a wire call onto the tool name and item type AO's row
// shaping speaks.
//
// `exec` and `exec_command` are the same unified-exec facility (the `exec`
// custom tool's script calls `tools.exec_command`), and a LIVE Codex session
// surfaces both as a `commandExecution` item with toolName "Bash" — so an
// imported thread has to say the same thing or it renders differently from
// the identical live conversation. The wire name is preserved on the row's
// meta as `codexToolName`, so nothing is lost.
func toolIdentity(kind, name string) (toolName, itemType string) {
	name = strings.TrimSpace(name)
	switch kind {
	case "web_search_call":
		return "web_search", "webSearch"
	case "tool_search_call":
		return "tool_search", "toolSearch"
	case "local_shell_call":
		return "Bash", "commandExecution"
	}
	switch name {
	case "exec", "exec_command":
		return "Bash", "commandExecution"
	case "apply_patch":
		return "file_change", "fileChange"
	case "spawn_agent", "spawnAgent":
		return "collab_agent", "collab_agent"
	case "send_message", "followup_task":
		return "send_input", "send_input"
	case "":
		return "tool", "toolCall"
	default:
		return name, "toolCall"
	}
}

func isCollabMessageToolName(name string) bool {
	switch strings.TrimSpace(name) {
	case "send_message", "followup_task", "send_input", "sendInput":
		return true
	default:
		return false
	}
}

// toolInput normalizes a call's arguments into the JSON object AO's summary
// and preview helpers read.
//
// Three shapes arrive: a JSON object (tool_search_call), a JSON string
// holding an object (`function_call.arguments`), and an opaque string
// (`custom_tool_call.input`, which for `exec` is a JavaScript program). The
// opaque form becomes `{"command": …}` so it previews and renders as the code
// that ran.
func (c *converter) toolInput(p responseCallPayload, toolName string) (json.RawMessage, string) {
	raw := p.Arguments
	if len(raw) == 0 {
		raw = p.Input
	}
	if len(raw) == 0 {
		return nil, ""
	}
	obj, text := decodeToolInput(raw)
	if obj == nil {
		if strings.TrimSpace(text) == "" {
			return nil, ""
		}
		encoded, err := json.Marshal(text)
		if err != nil {
			return nil, ""
		}
		obj = map[string]json.RawMessage{"command": encoded}
	}
	normalizeCommandKeys(obj)
	if toolName == "send_input" {
		obj["tool"] = json.RawMessage(`"send_input"`)
		obj["activityKind"] = json.RawMessage(`"interacted"`)
		if activityTool, err := json.Marshal(strings.TrimSpace(p.Name)); err == nil {
			obj["activityTool"] = activityTool
		}
	}
	command, _ := rawString(obj, "command")
	encoded, err := json.Marshal(obj)
	if err != nil {
		return nil, command
	}
	return encoded, command
}

func decodeToolInput(raw json.RawMessage) (map[string]json.RawMessage, string) {
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(asString), &obj) == nil && obj != nil {
			return obj, ""
		}
		return nil, asString
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil && obj != nil {
		return obj, ""
	}
	return nil, string(raw)
}

// normalizeCommandKeys folds the exec tool's own key names onto the ones
// AO's shared preview/summary helpers look for.
func normalizeCommandKeys(obj map[string]json.RawMessage) {
	if _, ok := obj["command"]; !ok {
		if cmd, ok := obj["cmd"]; ok {
			obj["command"] = cmd
		}
	}
	if _, ok := obj["cwd"]; !ok {
		if workdir, ok := obj["workdir"]; ok {
			obj["cwd"] = workdir
		}
	}
}

// completeToolCall settles a tool call from its `response_item/*_output` line.
func (c *converter) completeToolCall(env envelope) {
	var p responseOutputPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	callID := strings.TrimSpace(p.CallID)
	if callID == "" {
		callID = strings.TrimSpace(p.ID)
	}
	tool, ok := c.tools[callID]
	if !ok {
		if _, done := c.itemRows[callID]; done {
			// Settled by the item record; not an orphan.
			return
		}
		c.emitOrphanCompletion(callID, "tool output")
		return
	}
	output, _ := contentText(p.Output)
	if output == "" {
		output, _ = contentText(p.Tools)
	}
	status := strings.TrimSpace(p.Status)
	isError := p.Success != nil && !*p.Success
	c.finishTool(tool, output, status, isError)
}

// finishTool emits the completion (and, for a patch, the diff) and releases
// the correlation entry, stamped with the line currently being read.
func (c *converter) finishTool(tool *openTool, output, itemStatus string, isError bool) {
	c.finishToolAt(tool, output, itemStatus, isError, c.lastTimestamp)
}

func (c *converter) finishToolAt(tool *openTool, output, itemStatus string, isError bool, at time.Time) {
	meta := map[string]any{"toolName": tool.toolName}
	if len(tool.input) > 0 {
		meta["input"] = json.RawMessage(tool.input)
	}
	if tool.command != "" {
		meta["command"] = tool.command
	}
	if tool.rawToolName != "" && tool.rawToolName != tool.toolName {
		meta["codexToolName"] = tool.rawToolName
	}
	var diffPatch string
	if e := tool.enrich; e != nil {
		if e.command != "" {
			meta["command"] = e.command
		}
		if e.cwd != "" {
			meta["cwd"] = e.cwd
		}
		if e.exitCode != nil {
			meta["exit_code"] = *e.exitCode
		}
		if e.isError {
			isError = true
		}
		if e.itemStatus != "" && itemStatus == "" {
			itemStatus = e.itemStatus
		}
		if output == "" {
			output = e.output
		}
		diffPatch = e.diffPatch
		for k, v := range e.extra {
			meta[k] = v
		}
	}
	if isError {
		meta["is_error"] = true
	}
	if itemStatus != "" {
		meta["item_status"] = itemStatus
	}
	if tool.isBackground {
		meta["is_background"] = true
		meta["live_background_active"] = true
	}

	c.emit(provider.ProviderEvent{
		Kind:            provider.EventToolComplete,
		TurnID:          tool.turnID,
		TurnIndex:       tool.turnIndex,
		ItemID:          tool.itemID,
		ItemType:        tool.itemType,
		Content:         output,
		ContentPresent:  true,
		Meta:            metaJSON(meta),
		ParentToolUseID: tool.parentToolUseID,
		Timestamp:       at,
	})
	if diffPatch != "" {
		c.emit(provider.ProviderEvent{
			Kind:            provider.EventDiff,
			TurnID:          tool.turnID,
			TurnIndex:       tool.turnIndex,
			ItemID:          tool.itemID,
			ItemType:        tool.itemType,
			Content:         diffPatch,
			ContentPresent:  true,
			ParentToolUseID: tool.parentToolUseID,
			Timestamp:       at,
		})
	}
	c.releaseTool(tool.callID)
}

func (c *converter) releaseTool(callID string) {
	delete(c.tools, callID)
	for i, id := range c.toolOrder {
		if id == callID {
			c.toolOrder = append(c.toolOrder[:i], c.toolOrder[i+1:]...)
			break
		}
	}
}
