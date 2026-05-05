package claude

import (
	"encoding/json"
	"fmt"
	"strconv"

	"agent-overflow/internal/provider"
)

func claudeEffortFromOption(effort provider.ReasoningEffort) string {
	switch effort {
	case provider.EffortLow:
		return "low"
	case provider.EffortMedium:
		return "medium"
	case provider.EffortHigh:
		return "high"
	case provider.EffortXHigh:
		// Claude CLI accepts max for the extra-high tier on models whose SDK
		// descriptor names the option xhigh.
		return "max"
	case provider.EffortMax:
		return "max"
	default:
		return ""
	}
}

func claudeModelForContextWindow(model string, contextWindow int) string {
	if model != "" && contextWindow == provider.ClaudeExtendedContextWindow {
		return model + "[1m]"
	}
	return model
}

func claudeFastMode(model string, fastMode bool) bool {
	return fastMode && provider.ModelSupportsCapability(string(provider.Claude), model, provider.ModelCapabilityFastMode)
}

// ConfigFromOptions translates a provider-agnostic SessionOptions bundle into
// the Claude-specific launch Config. This is the single place where the
// effort flag, 1M-context model suffix, permission flags, and fast-mode
// settings are applied. Callers in app.go pass the result straight into
// claude.NewSession plus any ancillary wiring (Binary, EventLogger) that
// lives outside the session-options abstraction.
func ConfigFromOptions(opts provider.SessionOptions) Config {
	model := opts.Model
	contextWindow := provider.ResolveContextWindowForModel(string(provider.Claude), model, opts.ContextWindow)
	autoCompactPercent := provider.AutoCompactPercentForContextTier(
		provider.ContextTierForModelWindow(string(provider.Claude), model, contextWindow),
		opts.AutoCompactStandardPercent,
		opts.AutoCompactExtendedPercent,
	)
	if autoCompactPercent == 0 {
		autoCompactPercent = opts.AutoCompactPercent
	}

	return Config{
		Model:              claudeModelForContextWindow(model, contextWindow),
		WorkDir:            opts.WorkDir,
		Resume:             opts.Resume,
		ForkSession:        opts.ForkSession,
		SystemPrompt:       opts.SystemPrompt,
		ReasoningEffort:    claudeEffortFromOption(opts.ReasoningEffort),
		FastMode:           claudeFastMode(model, opts.FastMode),
		PermissionFlags:    claudePermissionFlags(opts.RuntimeMode),
		BasePermissionMode: claudeBasePermissionMode(opts.RuntimeMode),
		InteractionMode:    opts.Mode,
		AutoCompactPercent: autoCompactPercent,
	}
}

// claudePermissionFlags maps a RuntimeMode to the raw CLI flag sequence the
// Claude SDK would send to headless `claude -p`.
func claudePermissionFlags(mode provider.RuntimeMode) []string {
	permissionMode := claudeBasePermissionMode(mode)
	if permissionMode == "default" {
		return nil
	}
	flags := []string{"--permission-mode", permissionMode}
	if permissionMode == "bypassPermissions" {
		flags = append(flags, "--allow-dangerously-skip-permissions")
	}
	return flags
}

// claudeBasePermissionMode maps a RuntimeMode to Claude's permission-mode
// value. Unlike claudePermissionFlags, it returns "default" for supervised
// mode so sessions can restore the base mode via set_permission_mode after a
// plan turn.
func claudeBasePermissionMode(mode provider.RuntimeMode) string {
	switch mode {
	case provider.RuntimeAutoAcceptEdits:
		return "acceptEdits"
	case provider.RuntimeFullAccess:
		return "bypassPermissions"
	case provider.RuntimeApprovalRequired:
		fallthrough
	default:
		return "default"
	}
}

// cliInlineSettings is the JSON shape projected into the `--settings`
// CLI flag, which Claude Code applies as the `flagSettings` source. The
// struct field order is the JSON key order; keep `FastMode` first so
// the rendered output remains `{"fastMode":true}` for the fastMode-only
// case (matches existing test fixture and avoids needless churn for
// readers comparing CLI invocations).
type cliInlineSettings struct {
	FastMode bool              `json:"fastMode,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

// inlineSettingsForCLI builds the JSON payload for the `--settings` CLI
// flag, combining every setting we want to project as `flagSettings`.
// Returns ("", false) when there's nothing to set so the flag can be
// omitted entirely.
func inlineSettingsForCLI(cfg Config) (string, bool) {
	settings := cliInlineSettings{
		FastMode: cfg.FastMode,
	}
	if cfg.AutoCompactPercent > 0 {
		// Defense in depth: upstream `provider.normalizeAutoCompactPercent`
		// already clamps to ≤90, but `Config.AutoCompactPercent` is a
		// public field a future caller could populate directly without
		// going through `ConfigFromOptions`. The clamp here keeps the
		// rendered CLI flag honest regardless of how `cfg` was built.
		percent := cfg.AutoCompactPercent
		if percent > 90 {
			percent = 90
		}
		settings.Env = map[string]string{
			"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": strconv.Itoa(percent),
		}
	}
	if !settings.FastMode && len(settings.Env) == 0 {
		return "", false
	}
	data, err := json.Marshal(settings)
	if err != nil {
		// `cliInlineSettings` is a bool + map[string]string with no
		// custom marshalers, so json.Marshal cannot fail in practice.
		// Panic loudly if it ever does — silently dropping the flag
		// would mean the user's autocompact / fastMode setting is
		// quietly ignored, which violates the "errors must never
		// silently fail" rule in CLAUDE.md.
		panic(fmt.Sprintf("claude: cliInlineSettings marshal failed (unreachable): %v", err))
	}
	return string(data), true
}

// mcpConfigForCLI renders cfg.MCPServers into the JSON string the
// Claude CLI's --mcp-config flag accepts. Returns ("", false) when no
// servers are configured so the flag can be omitted entirely.
//
// The CLI's --mcp-config requires each server spec to carry an
// explicit "type" discriminator ("http" | "sse" | "stdio"). The
// design MCP (and any future provider-agnostic MCP server we share
// with Codex) returns the canonical untagged shape `{"url": "..."}`
// because Codex's serde uses `untagged + deny_unknown_fields` and
// rejects an unknown "type" field on the StreamableHttp variant —
// the providers can't share a single tagged shape. Backfill "type":
// "http" here for any server that has a `url` and no explicit type;
// pass through any spec that already carries one. Stdio specs (those
// with a "command") get backfilled to "stdio".
func mcpConfigForCLI(cfg Config) (string, bool) {
	if len(cfg.MCPServers) == 0 {
		return "", false
	}
	servers := make(map[string]any, len(cfg.MCPServers))
	for name, spec := range cfg.MCPServers {
		servers[name] = withClaudeTransportType(spec)
	}
	payload := map[string]any{"mcpServers": servers}
	data, err := json.Marshal(payload)
	if err != nil {
		// Same reasoning as inlineSettingsForCLI: nested maps with no
		// custom marshalers can't fail in practice. Panic loudly to
		// satisfy the "errors must never silently fail" rule.
		panic(fmt.Sprintf("claude: mcpConfigForCLI marshal failed (unreachable): %v", err))
	}
	return string(data), true
}

// withClaudeTransportType returns a server spec with a "type" field
// inferred from the present fields. Specs that already carry a "type"
// pass through untouched. Specs without a recognisable shape
// (neither url nor command) pass through too — the CLI will surface
// a clear error in that case.
func withClaudeTransportType(spec any) any {
	specMap, ok := spec.(map[string]any)
	if !ok {
		return spec
	}
	if _, hasType := specMap["type"]; hasType {
		return specMap
	}
	var inferred string
	switch {
	case specMap["url"] != nil:
		inferred = "http"
	case specMap["command"] != nil:
		inferred = "stdio"
	default:
		return specMap
	}
	out := make(map[string]any, len(specMap)+1)
	for k, v := range specMap {
		out[k] = v
	}
	out["type"] = inferred
	return out
}
