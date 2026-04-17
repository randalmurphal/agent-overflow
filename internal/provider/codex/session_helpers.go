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
	elicitation := parseElicitationRequest(params)

	description := "MCP server elicitation"
	if elicitation.ServerName != "" {
		description = fmt.Sprintf("MCP server %q requests user consent", elicitation.ServerName)
	}
	if elicitation.Message != "" {
		description = elicitation.Message
	}

	approval := provider.ApprovalRequest{
		RequestID:   fmt.Sprintf("%d", rpcID),
		ThreadID:    threadID,
		TurnID:      turnID,
		ToolName:    "mcp_elicitation",
		Description: description,
		// `Input` carries the raw provider payload. If the payload is not
		// valid JSON, substitute null so marshaling the approval itself
		// cannot fail — the adversarial-input test pins this down.
		Input:       safeRawMessage(params),
		Kind:        "mcp-elicitation",
		Title:       "MCP Server Consent",
		Elicitation: elicitation,
	}
	data, _ := json.Marshal(approval)
	return data
}

// safeRawMessage returns raw if it's a valid JSON value; otherwise the JSON
// null literal. Used as a defensive filter before embedding third-party
// payloads in fields typed as json.RawMessage — an invalid RawMessage would
// cause json.Marshal on the parent struct to fail entirely.
func safeRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return json.RawMessage("null")
	}
	return raw
}

// parseElicitationRequest extracts the fields the frontend needs to render an
// MCP elicitation dialog out of the Codex server-request params. The mode
// discriminator is normalized; unknown or missing modes fall back to "form" so
// the dialog always has a renderable shape (empty schema in the worst case).
//
// Wire contract authority lives in codex-source:
// /Users/randy/repos/codex-source/codex-rs/app-server-protocol/schema/typescript/v2/McpServerElicitationRequestParams.ts
//
// The sum-type splits on `mode`:
//   - `"form"` carries `message`, `requestedSchema`, and an opaque `_meta`.
//   - `"url"`  carries `message`, `url`, `elicitationId`, and an opaque `_meta`.
//
// Returns a non-nil pointer for every input so callers never need a nil
// check against adversarial or truncated params — an unparseable payload
// still becomes a valid (but empty) form-mode elicitation the UI can render.
func parseElicitationRequest(params json.RawMessage) *provider.ElicitationRequest {
	var parsed struct {
		Mode            string          `json:"mode"`
		Message         string          `json:"message"`
		ServerName      string          `json:"serverName"`
		RequestedSchema json.RawMessage `json:"requestedSchema"`
		URL             string          `json:"url"`
		ElicitationID   string          `json:"elicitationId"`
	}
	_ = json.Unmarshal(params, &parsed)

	mode := normalizeElicitationMode(parsed.Mode, parsed.URL, parsed.RequestedSchema)

	out := &provider.ElicitationRequest{
		Mode:       mode,
		Message:    parsed.Message,
		ServerName: parsed.ServerName,
	}
	switch mode {
	case "url":
		out.URL = parsed.URL
		out.ElicitationID = parsed.ElicitationID
	default:
		// form mode (including fallback) — preserve schema as-is, including null
		// becoming empty so the UI's schema parser has a deterministic input.
		if len(parsed.RequestedSchema) > 0 && !isJSONNull(parsed.RequestedSchema) {
			out.RequestedSchema = parsed.RequestedSchema
		}
	}
	return out
}

// normalizeElicitationMode picks a usable mode from potentially-malformed
// input. Adversarial inputs (missing mode, empty mode, unknown mode) fall
// back to a best guess using the payload shape, finally landing on "form".
func normalizeElicitationMode(raw, url string, schema json.RawMessage) string {
	switch raw {
	case "form", "url":
		return raw
	}
	// No explicit mode — infer from payload shape.
	if url != "" {
		return "url"
	}
	if len(schema) > 0 && !isJSONNull(schema) {
		return "form"
	}
	// Last resort: form with empty schema so the UI renders a decline/cancel
	// dialog instead of a silent dead-end.
	return "form"
}

// isJSONNull reports whether raw decodes to the JSON literal null. Used to
// distinguish "schema was explicitly null" from "schema field absent".
func isJSONNull(raw json.RawMessage) bool {
	// Trim whitespace before comparison — json.RawMessage preserves input
	// formatting, so "null", " null", and "null " all mean the same thing.
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		return len(raw)-i >= 4 &&
			raw[i] == 'n' && raw[i+1] == 'u' && raw[i+2] == 'l' && raw[i+3] == 'l'
	}
	return false
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
