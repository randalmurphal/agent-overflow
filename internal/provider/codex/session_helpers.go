package codex

import (
	"encoding/json"
	"fmt"

	"agent-overflow/internal/provider"
)

// buildThreadParams shapes the `thread/start` / `thread/resume` params.
//
// codexVersion is the build of the app-server this session is talking to
// (Session.AppServerVersion, off the `initialize` handshake — see
// app_server_version.go). It is a parameter rather than a Config field
// because Config is built before the process exists; see
// approvalPolicyForCodexVersion in options.go for what it changes and why an
// empty value must keep the pre-0.149 wire value.
func buildThreadParams(cfg Config, codexVersion string) map[string]any {
	params := map[string]any{}

	if cfg.WorkDir != "" {
		params["cwd"] = cfg.WorkDir
	}

	if cfg.Model != "" {
		params["model"] = cfg.Model
	}
	if cfg.ServiceTier != "" {
		params["serviceTier"] = cfg.ServiceTier
	}

	if cfg.Sandbox != "" {
		sandbox := normalizeThreadSandbox(cfg.Sandbox)
		params["approvalPolicy"] = defaultApprovalPolicy(cfg.ApprovalPolicy, sandbox, codexVersion)
		params["sandbox"] = sandbox
	}

	// Unconditional, unlike every other key here. `approvalsReviewer` is
	// thread state Codex keeps across a resume, so omitting it on a resumed
	// thread would silently inherit the reviewer the thread was last started
	// with — a thread moved out of the auto runtime mode would keep answering
	// its own approvals (t3-improvements.md §3.2). Sending the resolved value
	// every time makes the wire state a function of the thread row alone.
	params["approvalsReviewer"] = threadApprovalsReviewer(cfg)

	if cfg.SystemPrompt != "" {
		params["baseInstructions"] = cfg.SystemPrompt
	}
	if cfg.AdditionalInstructions != "" {
		params["developerInstructions"] = cfg.AdditionalInstructions
	}

	// `config` is the free-form override bag on ThreadStartParams. We set
	// mcp_servers (app-owned per-thread wiring) and model_reasoning_effort (the
	// thread-level reasoning_effort default, mirrored by the per-turn
	// `effort` override on turn/start). Handling in codex-source (0.147):
	// app-server/src/request_processors/thread_processor.rs and
	// app-server/src/config_manager.rs — request overrides merge into the
	// CLI-override layer with dotted keys expanded into nested TOML. See
	// docs/references/codex-instructions-tools.md.
	configOverrides := map[string]any{}
	if len(cfg.MCPServers) > 0 {
		configOverrides["mcp_servers"] = cfg.MCPServers
	}
	if cfg.ReasoningEffort != "" {
		configOverrides["model_reasoning_effort"] = cfg.ReasoningEffort
	}
	if cfg.ContextWindow > 0 {
		configOverrides["model_context_window"] = cfg.ContextWindow
	}
	if cfg.AutoCompactTokenLimit > 0 {
		configOverrides["model_auto_compact_token_limit"] = cfg.AutoCompactTokenLimit
	}
	// Tool disabling rides the same bag. It lands here rather than on
	// thread/start alone because this map is shared with thread/resume: a
	// cold resume that omitted the keys would rebuild the thread with the
	// tools back in the request.
	for key, value := range DisabledToolConfigOverrides(cfg.DisabledTools) {
		configOverrides[key] = value
	}
	if len(configOverrides) > 0 {
		params["config"] = configOverrides
	}

	return params
}

// threadApprovalsReviewer resolves the reviewer that actually reaches the
// wire. An empty Config field means "unspecified", which on the wire is
// Codex's protocol default (`ApprovalsReviewer::User`, `#[default]` in
// codex-rs/protocol/src/config_types.rs) — so resolving it here rather than
// omitting the key keeps the always-explicit rule true for every Config,
// including ones built outside ConfigFromOptions.
func threadApprovalsReviewer(cfg Config) string {
	if cfg.ApprovalsReviewer == "" {
		return approvalsReviewerUser
	}
	return cfg.ApprovalsReviewer
}

// verifyApprovalsReviewerEcho fails the handshake when Codex is not running
// the reviewer AO asked for.
//
// This check is not paranoia about a well-behaved server; it is the ONLY
// available detection for a specific silent failure. `ThreadStartParams` has
// no `deny_unknown_fields`, so a codex older than 0.115 accepts a
// `approvalsReviewer` it does not understand, drops it, and starts an ordinary
// user-reviewer thread — the user would sit in the auto runtime mode watching
// approval prompts that mode promises never to raise. `initialize` carries no
// version or capability list to gate on, and `thread/started` does not carry
// the reviewer, so the start/resume RESPONSE is the sole source of truth at
// handshake time. (Later drift is reconciled from `thread/settings/updated`
// in thread_settings.go.) AO's provider floor is codex 0.143, well above the
// 0.124 where the field became reliable, so on every supported binary the echo
// is present and this reduces to a byte comparison.
//
// An absent echo is read as the protocol default rather than as "unknown":
// the field is non-Option upstream on both ThreadStartResponse and
// ThreadResumeResponse, and every codex predating it had only the user
// reviewer. That makes the rule a single comparison with no special cases —
// asking for `user` and getting silence is a match; asking for `auto_review`
// and getting silence is the drop this exists to catch.
//
// The failure is an error, never a downgrade. Starting anyway would run the
// session under a permission posture the thread row does not describe.
func verifyApprovalsReviewerEcho(method, requested string, resp json.RawMessage) error {
	echoed := readTopLevelString(resp, "approvalsReviewer")
	if echoed == "" {
		echoed = approvalsReviewerUser
	}
	if echoed == requested {
		return nil
	}
	return fmt.Errorf(
		"codex: %s: requested approvals reviewer %q but the app-server is running %q — "+
			"this codex build does not support the requested reviewer; upgrade codex or "+
			"switch the thread out of the auto runtime mode",
		method, requested, echoed,
	)
}

func normalizeThreadSandbox(sandbox string) string {
	switch sandbox {
	case "read-only", "workspace-write", "danger-full-access":
		return sandbox
	default:
		return "read-only"
	}
}

// defaultApprovalPolicy resolves the approval policy that reaches the wire:
// the caller's explicit choice when it has one, otherwise the sandbox's own
// most-supervised pairing. Both halves run through the version remap, so a
// Config that never set the axis lands on the same wire value a Config that
// set it to "untrusted" would.
func defaultApprovalPolicy(policy string, sandbox string, codexVersion string) string {
	if policy == "" {
		switch sandbox {
		case "danger-full-access":
			policy = "never"
		case "workspace-write":
			policy = codexApprovalPolicyOnRequest
		default:
			policy = codexApprovalPolicyUnlessTrusted
		}
	}
	return approvalPolicyForCodexVersion(policy, codexVersion)
}

func turnSandboxPolicy(sandbox string) (map[string]any, error) {
	switch sandbox {
	case "read-only":
		return map[string]any{"type": "readOnly"}, nil
	case "workspace-write":
		return map[string]any{"type": "workspaceWrite"}, nil
	case "danger-full-access":
		return map[string]any{"type": "dangerFullAccess"}, nil
	default:
		return nil, fmt.Errorf("codex: unsupported sandbox %q", sandbox)
	}
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
	approval := buildApprovalRequest(threadID, turnID, method, rpcID, params)
	data, _ := json.Marshal(approval)
	return data
}

func buildApprovalRequest(threadID, turnID, method string, rpcID int64, params json.RawMessage) provider.ApprovalRequest {
	var parsed map[string]json.RawMessage
	_ = json.Unmarshal(params, &parsed)

	toolName := method
	description := method
	var input json.RawMessage
	title := method
	kind := approvalKindForMethod(method)
	providerApprovalID := readRawString(parsed, "approvalId")
	itemID := readRawString(parsed, "itemId")
	availableDecisions := readAvailableApprovalDecisions(parsed)
	providerKind := readRawString(parsed, "kind")
	if method == "item/commandExecution/requestApproval" && providerKind == "writeStdin" {
		kind = "write-stdin"
		toolName = "write_stdin"
		title = "Send input to terminal"
		description = readRawString(parsed, "reason")
		if description == "" {
			description = "Send input to the running terminal"
		}
		input = append(json.RawMessage(nil), params...)
	}

	if cmd, ok := parsed["command"]; ok && kind != "write-stdin" {
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

	return provider.ApprovalRequest{
		RequestID:          fmt.Sprintf("%d", rpcID),
		ThreadID:           threadID,
		TurnID:             turnID,
		ToolUseID:          itemID,
		ToolName:           toolName,
		Description:        description,
		Input:              input,
		Title:              title,
		Kind:               kind,
		ProviderApprovalID: providerApprovalID,
		AvailableDecisions: availableDecisions,
	}
}

func readAvailableApprovalDecisions(parsed map[string]json.RawMessage) *[]json.RawMessage {
	raw, present := parsed["availableDecisions"]
	if !present || string(raw) == "null" {
		return nil
	}
	var decisions []json.RawMessage
	if json.Unmarshal(raw, &decisions) != nil {
		return nil
	}
	return &decisions
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
// /home/rmurphy/repos/codex/codex-rs/app-server-protocol/schema/typescript/v2/McpServerElicitationRequestParams.ts
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

func buildUserInputMetaFromQuestions(threadID, turnID, toolUseID string, rpcID int64, questions []provider.UserInputQuestion) json.RawMessage {
	request := provider.UserInputRequest{
		RequestID: fmt.Sprintf("%d", rpcID),
		ThreadID:  threadID,
		TurnID:    turnID,
		ToolUseID: toolUseID,
		ToolName:  "user_input",
		Questions: questions,
		Title:     "User Input Required",
	}
	data, _ := json.Marshal(request)
	return data
}

func buildUserInputToolStartMeta(questions []provider.UserInputQuestion) json.RawMessage {
	input, _ := json.Marshal(map[string]any{
		"questions": questions,
	})
	data, _ := json.Marshal(map[string]any{
		"toolName": "request_user_input",
		"input":    json.RawMessage(input),
	})
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
		Questions *[]provider.UserInputQuestion `json:"questions"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return nil
	}
	if payload.Questions == nil {
		return nil
	}
	return provider.NormalizeUserInputQuestions(*payload.Questions)
}

// parseUserInputIsBlocking decodes `item/tool/requestUserInput`'s `isBlocking`
// field (Codex 0.147, `ToolRequestUserInputParams` in
// codex-rs/app-server-protocol/src/protocol/v2/item.rs).
//
// The default is TRUE and upstream applies it in its own custom Deserialize
// (`is_blocking: wire.is_blocking.unwrap_or(true)`), precisely so a request
// from a client that predates the field keeps its blocking meaning. AO mirrors
// that: an absent or malformed key reads as blocking.
//
// `false` means the turn continues while the question is outstanding — the
// model is not parked on the answer. It replaces the deprecated
// `autoResolutionMs`, which AO never read. No UX change hangs off it yet;
// it is decoded and logged so a non-blocking request is visible in the log
// rather than silently rendered as a turn-blocking prompt.
func parseUserInputIsBlocking(params json.RawMessage) bool {
	var payload struct {
		IsBlocking *bool `json:"isBlocking"`
	}
	if err := json.Unmarshal(params, &payload); err != nil || payload.IsBlocking == nil {
		return true
	}
	return *payload.IsBlocking
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
