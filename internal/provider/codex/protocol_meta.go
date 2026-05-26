package codex

import "encoding/json"

// enrichItemMeta augments item/started and item/completed params with the
// normalized Meta contract used by triage and the renderer:
//
//   - source, item_status, process_id from the raw Codex item
//   - toolName plus concise input fields for row labels/previews
//   - raw item params for debugging and adjacent consumers
//
// Image generation is the one exception to raw preservation: its result field
// is base64 image bytes, so it is stripped from Meta while revisedPrompt and
// savedPath stay available through input/content.
func enrichItemMeta(params json.RawMessage) json.RawMessage {
	return enrichItemMetaFromItem(params, readNestedObject(params, "item"))
}

func enrichItemMetaFromItem(params json.RawMessage, item map[string]json.RawMessage) json.RawMessage {
	source := ""
	status := ""
	processID := ""
	if item != nil {
		source = readRawString(item, "source")
		status = readRawString(item, "status")
		processID = readRawString(item, "processId")
	} else {
		source = readNestedString(params, "item", "source")
		status = readNestedString(params, "item", "status")
		processID = readNestedString(params, "item", "processId")
	}
	extras := map[string]any{}
	if source != "" {
		extras["source"] = source
	}
	if status != "" {
		extras["item_status"] = status
	}
	if processID != "" {
		extras["process_id"] = processID
	}
	if item != nil {
		mergeMap(extras, codexItemMetaExtras(item))
		if itemID := readRawString(item, "id"); itemID != "" {
			extras["itemId"] = itemID
		}
	}
	if len(extras) == 0 {
		return nil
	}
	result, err := json.Marshal(extras)
	if err != nil {
		return params
	}
	return result
}

func codexItemMetaExtras(item map[string]json.RawMessage) map[string]any {
	itemType := readRawString(item, "type")
	switch itemType {
	case "fileChange", "file_change":
		return fileChangeMetaExtras(item)
	case "collabAgentToolCall":
		return collabAgentMetaExtras(item)
	case "webSearch", "web_search":
		return webSearchMetaExtras(item)
	case "mcpToolCall", "mcp_tool_call":
		return mcpToolCallMetaExtras(item)
	case "dynamicToolCall", "dynamic_tool_call":
		return dynamicToolCallMetaExtras(item)
	case "imageView", "image_view":
		return imageViewMetaExtras(item)
	case "imageGeneration", "image_generation":
		return imageGenerationMetaExtras(item)
	default:
		if isCommandExecutionItemType(itemType) {
			return commandExecutionMetaExtras(item)
		}
		return nil
	}
}

func fileChangeMetaExtras(item map[string]json.RawMessage) map[string]any {
	extras := map[string]any{"toolName": "file_change"}
	changesRaw, ok := item["changes"]
	if !ok {
		return extras
	}
	var changes []struct {
		Path string `json:"path"`
		Kind struct {
			Type     string `json:"type"`
			MovePath string `json:"move_path"`
		} `json:"kind"`
	}
	if json.Unmarshal(changesRaw, &changes) != nil {
		return extras
	}
	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		if change.Kind.Type == "update" && change.Kind.MovePath != "" {
			paths = append(paths, change.Kind.MovePath)
			continue
		}
		if change.Path != "" {
			paths = append(paths, change.Path)
		}
	}
	if len(paths) == 1 {
		extras["input"] = map[string]any{"file_path": paths[0]}
	} else if len(paths) > 1 {
		extras["input"] = map[string]any{"files": paths}
	}
	return extras
}

func commandExecutionMetaExtras(item map[string]json.RawMessage) map[string]any {
	extras := map[string]any{"toolName": "Bash"}
	input := map[string]any{}
	if command := readRawString(item, "command"); command != "" {
		input["command"] = command
		extras["command"] = command
	}
	if cwd := readRawString(item, "cwd"); cwd != "" {
		input["cwd"] = cwd
		extras["cwd"] = cwd
	}
	if exitCode, ok := readRawInt(item, "exitCode"); ok {
		extras["exitCode"] = exitCode
	}
	if durationMs, ok := readRawInt(item, "durationMs"); ok {
		extras["durationMs"] = durationMs
	}
	if actions := firstRaw(item, "commandActions", "command_actions"); len(actions) > 0 {
		extras["commandActions"] = actions
	}
	if len(input) > 0 {
		extras["input"] = input
	}
	return extras
}

func collabAgentMetaExtras(item map[string]json.RawMessage) map[string]any {
	tool := normalizeCollabToolName(readRawString(item, "tool"))
	// toolName mirrors the itemType classification so the frontend can pick
	// a renderer without having to inspect input.tool.
	toolName := "collab_agent"
	switch tool {
	case "send_input":
		toolName = "send_input"
	case "wait_agent":
		toolName = "wait_agent"
	case "close_agent":
		toolName = "close_agent"
	case "resume_agent":
		toolName = "resume_agent"
	}
	input := map[string]any{"tool": tool}
	if prompt := readRawString(item, "prompt"); prompt != "" {
		input["prompt"] = prompt
	}
	if model := readRawString(item, "model"); model != "" {
		input["model"] = model
	}
	if reasoningEffort := readRawString(item, "reasoningEffort"); reasoningEffort != "" {
		input["reasoningEffort"] = reasoningEffort
	}
	if nickname := firstRawString(item, "newAgentNickname", "agentNickname", "nickname"); nickname != "" {
		input["newAgentNickname"] = nickname
	}
	if role := firstRawString(item, "newAgentRole", "agentRole", "agent_type", "agentType"); role != "" {
		input["newAgentRole"] = role
	}
	if receiverThreadIDs := readRawStringArray(item, "receiverThreadIds"); len(receiverThreadIDs) > 0 {
		input["receiverThreadIds"] = receiverThreadIDs
	}
	if agentsStates := readRawJSONObject(item, "agentsStates"); agentsStates != nil {
		input["agentsStates"] = agentsStates
	}
	return map[string]any{"toolName": toolName, "input": input}
}

func imageViewMetaExtras(item map[string]json.RawMessage) map[string]any {
	extras := map[string]any{"toolName": "ViewImage"}
	input := map[string]any{}
	if path := readRawString(item, "path"); path != "" {
		input["path"] = path
	}
	if len(input) > 0 {
		extras["input"] = input
	}
	return extras
}

func imageGenerationMetaExtras(item map[string]json.RawMessage) map[string]any {
	extras := map[string]any{"toolName": "ImageGeneration"}
	input := map[string]any{}
	if path := firstNonEmptyString(readRawString(item, "savedPath"), readRawString(item, "saved_path")); path != "" {
		input["path"] = path
	}
	if prompt := firstNonEmptyString(readRawString(item, "revisedPrompt"), readRawString(item, "revised_prompt")); prompt != "" {
		input["prompt"] = prompt
	}
	if len(input) > 0 {
		extras["input"] = input
	}
	return extras
}

func webSearchMetaExtras(item map[string]json.RawMessage) map[string]any {
	extras := map[string]any{"toolName": "WebSearch"}
	input := map[string]any{}
	if query := readRawString(item, "query"); query != "" {
		input["query"] = query
	}
	if len(input) > 0 {
		extras["input"] = input
	}
	return extras
}

func mcpToolCallMetaExtras(item map[string]json.RawMessage) map[string]any {
	server := readRawString(item, "server")
	tool := readRawString(item, "tool")
	toolName := "MCP"
	if tool != "" {
		toolName = "MCP/" + tool
	}
	extras := map[string]any{"toolName": toolName}
	// Arguments are the raw input the model invoked the tool with. We
	// surface them as `meta.input` (same shape Claude's parser produces
	// for `mcp__<server>__<tool>` blocks) so the renderer can compose
	// `server.tool(args)` from a single source on both providers.
	if args := readRawJSONObject(item, "arguments"); args != nil {
		extras["input"] = args
	}
	// `meta.mcp` carries the {server, tool} pair the toolName alone
	// drops (toolName is `MCP/<tool>` once normalized). Both fields
	// are required for the renderer's body synthesis.
	if server != "" || tool != "" {
		mcp := map[string]string{}
		if server != "" {
			mcp["server"] = server
		}
		if tool != "" {
			mcp["tool"] = tool
		}
		extras["mcp"] = mcp
	}
	return extras
}

func dynamicToolCallMetaExtras(item map[string]json.RawMessage) map[string]any {
	namespace := readRawString(item, "namespace")
	tool := readRawString(item, "tool")
	input := toolDescriptionInput(namespace, tool)
	extras := map[string]any{}
	if tool != "" {
		extras["toolName"] = tool
	}
	if len(input) > 0 {
		extras["input"] = input
	}
	return extras
}

func toolDescriptionInput(scope, tool string) map[string]any {
	input := map[string]any{}
	switch {
	case scope != "" && tool != "":
		input["description"] = scope + "/" + tool
	case tool != "":
		input["description"] = tool
	case scope != "":
		input["description"] = scope
	}
	return input
}
