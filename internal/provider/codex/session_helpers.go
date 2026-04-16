package codex

import (
	"encoding/json"
	"fmt"

	"agent-overflow/internal/provider"
)

func buildThreadParams(cfg Config) map[string]any {
	params := map[string]any{}

	if cfg.WorkDir != "" {
		params["cwd"] = cfg.WorkDir
	}

	if cfg.Model != "" {
		params["model"] = cfg.Model
	}

	if cfg.Sandbox != "" {
		switch cfg.Sandbox {
		case "danger-full-access":
			params["approvalPolicy"] = "never"
			params["sandboxPolicy"] = "none"
		case "workspace-write":
			params["approvalPolicy"] = cfg.ApprovalPolicy
			params["sandboxPolicy"] = "workspace"
		default:
			params["approvalPolicy"] = cfg.ApprovalPolicy
			params["sandboxPolicy"] = "read-only"
		}
	}

	if cfg.SystemPrompt != "" {
		params["baseInstructions"] = cfg.SystemPrompt
	}
	if len(cfg.MCPServers) > 0 {
		params["config"] = map[string]any{
			"mcp_servers": cfg.MCPServers,
		}
	}

	return params
}

func readRouteFields(params json.RawMessage) (string, string) {
	turnID := readTopLevelString(params, "turnId")
	if turnID == "" {
		turnID = readNestedString(params, "turn", "id")
	}

	itemID := readTopLevelString(params, "itemId")
	if itemID == "" {
		itemID = readNestedString(params, "item", "id")
	}

	return turnID, itemID
}

func buildApprovalMeta(threadID, turnID, method string, rpcID int64, params json.RawMessage) json.RawMessage {
	var parsed map[string]json.RawMessage
	_ = json.Unmarshal(params, &parsed)

	toolName := method
	description := method
	var input json.RawMessage
	title := method
	kind := approvalKindForMethod(method)

	if cmd, ok := parsed["command"]; ok {
		var cmdStr string
		if json.Unmarshal(cmd, &cmdStr) == nil {
			toolName = "command"
			description = cmdStr
			title = "Run command"
			input = cmd
		}
	}
	if filePath, ok := parsed["filePath"]; ok {
		var fp string
		if json.Unmarshal(filePath, &fp) == nil {
			if kind == "file-read" {
				toolName = "file_read"
				title = "File read"
			} else {
				toolName = "file_change"
				title = "File change"
			}
			description = fp
			input = params
		}
	}

	approval := provider.ApprovalRequest{
		RequestID:   fmt.Sprintf("%d", rpcID),
		ThreadID:    threadID,
		TurnID:      turnID,
		ToolName:    toolName,
		Description: description,
		Input:       input,
		Title:       title,
		Kind:        kind,
	}

	data, _ := json.Marshal(approval)
	return data
}

// approvalKindForMethod derives the approval kind from the JSON-RPC method name.
func approvalKindForMethod(method string) string {
	switch method {
	case "item/commandExecution/requestApproval", "execCommandApproval":
		return "command"
	case "item/fileRead/requestApproval":
		return "file-read"
	case "item/fileChange/requestApproval", "applyPatchApproval":
		return "file-change"
	default:
		return ""
	}
}

func buildElicitationMeta(threadID, turnID string, rpcID int64, params json.RawMessage) json.RawMessage {
	var parsed struct {
		ServerName string          `json:"serverName"`
		Message    string          `json:"message"`
		Schema     json.RawMessage `json:"requestedSchema"`
	}
	_ = json.Unmarshal(params, &parsed)

	description := "MCP server elicitation"
	if parsed.ServerName != "" {
		description = fmt.Sprintf("MCP server %q requests user consent", parsed.ServerName)
	}
	if parsed.Message != "" {
		description = parsed.Message
	}

	approval := provider.ApprovalRequest{
		RequestID:   fmt.Sprintf("%d", rpcID),
		ThreadID:    threadID,
		TurnID:      turnID,
		ToolName:    "mcp_elicitation",
		Description: description,
		Input:       params,
		Kind:        "mcp-elicitation",
		Title:       "MCP Server Consent",
	}
	data, _ := json.Marshal(approval)
	return data
}

func buildUserInputMeta(threadID, turnID string, rpcID int64, params json.RawMessage) json.RawMessage {
	questions := parseUserInputQuestions(params)

	approval := provider.ApprovalRequest{
		RequestID: fmt.Sprintf("%d", rpcID),
		ThreadID:  threadID,
		TurnID:    turnID,
		ToolName:  "user_input",
		Input:     params,
		Kind:      "user-input",
		Questions: questions,
		Title:     "User Input Required",
	}
	data, _ := json.Marshal(approval)
	return data
}

func buildPermissionMeta(threadID, turnID string, rpcID int64, params json.RawMessage) json.RawMessage {
	reason, perms := parsePermissionRequest(params)

	approval := provider.ApprovalRequest{
		RequestID:   fmt.Sprintf("%d", rpcID),
		ThreadID:    threadID,
		TurnID:      turnID,
		ToolName:    "permissions",
		Kind:        "permission",
		Input:       params,
		Description: reason,
		Permissions: perms,
		Title:       "Permission Required",
	}
	data, _ := json.Marshal(approval)
	return data
}

func parseUserInputQuestions(params json.RawMessage) []provider.UserInputQuestion {
	var payload struct {
		Questions []provider.UserInputQuestion `json:"questions"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return nil
	}
	return payload.Questions
}

func parsePermissionRequest(params json.RawMessage) (string, *provider.PermissionProfile) {
	var payload struct {
		Reason      string                      `json:"reason"`
		Permissions *provider.PermissionProfile `json:"permissions"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return "", nil
	}
	return payload.Reason, payload.Permissions
}
