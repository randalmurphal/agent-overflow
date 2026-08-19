package claude

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"unicode"

	"agent-overflow/internal/provider"
)

// claudeEffortForModel resolves the `--effort` flag value for a session,
// dropping it entirely for a model the catalog says has no reasoning tiers
// (Haiku, per the CLI's own model list). Mirrors claudeFastMode, which gates
// the same way on the same catalog. See
// provider.ModelDeclaresNoReasoningEffort for why absence beats coercion here.
func claudeEffortForModel(model string, effort provider.ReasoningEffort) string {
	if provider.ModelDeclaresNoReasoningEffort(string(provider.Claude), model) {
		return ""
	}
	return claudeEffortFromOption(effort)
}

func claudeEffortFromOption(effort provider.ReasoningEffort) string {
	switch effort {
	case provider.EffortLow:
		return "low"
	case provider.EffortMedium:
		return "medium"
	case provider.EffortHigh:
		return "high"
	case provider.EffortXHigh:
		// 2.1.170's --effort accepts `xhigh` natively (a distinct tier between
		// high and max), so pass it through instead of collapsing to max — the
		// old workaround from when the flag's value set lacked xhigh.
		return "xhigh"
	case provider.EffortMax:
		return "max"
	default:
		return ""
	}
}

// claudeModelForContextWindow appends the `[1m]` context-tier marker the CLI
// reads the extended window from. It is the inverse of
// provider.TrimContextMarker, and trims first so a model id that already
// carries a marker (the CLI's own list bakes them into id strings) yields one
// marker rather than `model[1m][1m]`.
func claudeModelForContextWindow(model string, contextWindow int) string {
	model = provider.TrimContextMarker(model)
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
		Model:                claudeModelForContextWindow(model, contextWindow),
		WorkDir:              opts.WorkDir,
		Resume:               opts.Resume,
		ResumeAt:             opts.ResumeAt,
		ForkSession:          opts.ForkSession,
		SystemPrompt:         opts.SystemPrompt,
		ReasoningEffort:      claudeEffortForModel(model, opts.ReasoningEffort),
		FastMode:             claudeFastMode(model, opts.FastMode),
		PermissionFlags:      claudePermissionFlags(opts.RuntimeMode),
		DisallowedTools:      mergeDisallowedTools(claudeDisallowedTools(opts.RuntimeMode), opts.DisabledTools),
		DisableTodoReminders: opts.DisableTodoReminders,
		BasePermissionMode:   claudeBasePermissionMode(opts.RuntimeMode),
		InteractionMode:      opts.Mode,
		AutoCompactPercent:   autoCompactPercent,
		ContextWindow:        contextWindow,
	}
}

// claudeReadOnlyDisallowedTools is the set of built-in tools removed outright
// from a read-only session. See claudeDisallowedTools for why this is separate
// from (and not redundant with) the `dontAsk` permission mode.
var claudeReadOnlyDisallowedTools = []string{"Write", "Edit", "NotebookEdit"}

// claudePermissionFlags maps a RuntimeMode to the raw CLI flag sequence the
// Claude SDK would send to headless `claude -p`. These are exactly the flags
// whose effect `set_permission_mode` can reproduce on a live session; the
// spawn-only tool removal lives in claudeDisallowedTools.
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

// claudeDisallowedTools returns the tools to strip from the session via
// `--disallowedTools`. Only read-only sessions strip anything.
//
// This is deliberately NOT folded into claudePermissionFlags. `dontAsk` alone
// is not enforcement: it converts an "ask" decision into a "deny" at the end
// of the permission pipeline, so an action that some settings source already
// *allows* never becomes an ask and is permitted. A `permissions.allow` entry
// for Write in the user's `~/.claude/settings.json` would therefore let a
// read-only session write files. `--disallowedTools` removes the tools from
// the session's toolset entirely, which no allow rule can reinstate.
//
// Verified against claude 2.1.219 — see docs/references/claude-wire.md
// §"Permission modes for read-only sessions" for the spike transcript.
//
// The split also carries a lifecycle meaning: tool removal is applied once at
// spawn and there is no control_request that can add or drop a tool mid-session.
// Keeping it on its own Config field is what makes any transition into or out
// of read-only fail the PlanLiveUpdate equality check and demand a restart,
// rather than half-applying as a bare set_permission_mode.
func claudeDisallowedTools(mode provider.RuntimeMode) []string {
	if mode != provider.RuntimeReadOnly {
		return nil
	}
	return append([]string(nil), claudeReadOnlyDisallowedTools...)
}

// mergeDisallowedTools unions the runtime-mode strips with the user's
// settings-level list. Mode entries come first and the user's follow in
// their configured order, deduped — the CLI takes one `--disallowedTools`
// flag per name, and a stable order is what keeps PlanLiveUpdate's
// equality check (and the argv it produces) from flapping between two
// spellings of the same set.
//
// Only the headless transport has a mode strip to union in; claudetui
// runs the settings list alone through SanitizeDisallowedTools, because
// Capabilities.EnforcesRuntimeMode is false there and every tier must stay
// inert by construction.
func mergeDisallowedTools(modeTools, settingsTools []string) []string {
	if len(settingsTools) == 0 {
		return modeTools
	}
	return SanitizeDisallowedTools(append(append([]string(nil), modeTools...), settingsTools...))
}

// SanitizeDisallowedTools is the argv boundary for `--disallowedTools`
// names: it trims, drops anything that is not ONE safe CLI argument (with
// a log line naming the reason), and dedupes while preserving order.
// Returns nil when nothing survives, so callers can compare against a nil
// Config field without a length special-case.
//
// Names are re-validated here rather than trusted. Settings validation is
// the primary gate and the one that reports the problem to the user, but
// this is where the value becomes argv: a name with whitespace becomes two
// flag arguments and a name starting with `-` is parsed as a FLAG, which
// turns a bad tool name into an unpredictable CLI invocation. Doing it here
// makes that structurally impossible for any caller — a future one that
// builds Config directly included — instead of leaving the guarantee in
// caller discipline.
//
// Exported because both Claude transports need exactly this pass and a
// second copy would be one edit away from disagreeing: headless folds it
// into mergeDisallowedTools, and claudetui.ConfigFromOptions calls it
// directly on the settings list.
func SanitizeDisallowedTools(tools []string) []string {
	if len(tools) == 0 {
		return nil
	}
	kept := make([]string, 0, len(tools))
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		trimmed := strings.TrimSpace(tool)
		switch {
		case trimmed == "":
			continue
		case strings.ContainsFunc(trimmed, unicode.IsSpace):
			log.Printf("claude: dropping disallowed-tool name %q — a name containing whitespace is not one CLI argument", tool)
			continue
		case strings.HasPrefix(trimmed, "-"):
			log.Printf("claude: dropping disallowed-tool name %q — a leading dash parses as a CLI flag", tool)
			continue
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		kept = append(kept, trimmed)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// claudeBasePermissionMode maps a RuntimeMode to Claude's permission-mode
// value. Unlike claudePermissionFlags, it returns "default" for supervised
// mode so sessions can restore the base mode via set_permission_mode after a
// plan turn.
//
// Every tier is enumerated; the trailing return is the unknown-value path,
// not a tier's mapping. Callers reach this through
// provider.NormalizeRuntimeMode, which never yields a non-canonical value, so
// the fallback is unreachable in practice — and it deliberately lands on
// supervised prompting rather than on any tier that would widen or seize up a
// session. TestClaudeBasePermissionModeCoversEveryRuntimeMode is what forces a
// new tier to be enumerated here rather than absorbed by that fallback.
func claudeBasePermissionMode(mode provider.RuntimeMode) string {
	switch mode {
	case provider.RuntimeReadOnly:
		// "dontAsk" denies anything that would otherwise prompt, immediately
		// and without emitting a CanUseTool control_request. Reads and
		// non-mutating bash still run; the denial arrives as an is_error
		// tool_result the model reads and continues from, so an unattended
		// turn completes instead of stalling. Verified on claude 2.1.219.
		return "dontAsk"
	case provider.RuntimeApprovalRequired:
		return "default"
	case provider.RuntimeAutoAcceptEdits:
		return "acceptEdits"
	case provider.RuntimeAuto:
		return claudeAutoPermissionMode
	case provider.RuntimeFullAccess:
		return "bypassPermissions"
	}
	return "default"
}

// claudeAutoPermissionMode is the CLI permission mode behind
// provider.RuntimeAuto. Spike-verified against claude 2.1.219 (captures under
// the 2026-08-02 Claude spike in t3-improvements.md §Decision log):
//
//   - `--permission-mode auto` is accepted verbatim and echoed back on
//     `system/init` as `permissionMode:"auto"`. The statsig gates the CLI
//     carries for auto (`tengu_harbor_willow` / `tengu_moss_anchor`) guard
//     only the *implicit* default-to-auto path taken when no mode was
//     requested; an explicit flag never reaches them.
//   - `set_permission_mode` accepts "auto" on the stream-json control channel
//     — the accepted set is
//     {acceptEdits, auto, bypassPermissions, default, dontAsk, plan} and the
//     CLI replied `{"mode":"auto"}`. Only "bypassPermissions" is additionally
//     gated on how the process was spawned, so auto ↔ every other non-read-only
//     tier is a live transition (see PlanLiveUpdate).
//   - The decision path is: "acceptEdits would allow it" → allow; safe-tool
//     allowlist → allow; otherwise a two-stage Haiku classifier that allows or
//     DENIES. It falls closed when the classifier is unavailable, and it falls
//     back to a REAL interactive ask on safety_check / ask_rule /
//     plan_mode_floor / org_ask_ceiling / requires_user_interaction.
//
// That last bullet is why auto does not change any of AO's approval plumbing:
// the fallback ask arrives as an ordinary `can_use_tool` control_request on the
// same channel `--permission-prompt-tool stdio` already installs, and
// parse_control.go answers it like any other tier's approval.
//
// The one failure mode worth naming is the CLI's headless posture. When
// `toolPermissionContext.shouldAvoidPermissionPrompts` is set, auto's
// fallback-to-ask becomes a hard deny and a denial streak throws
// "Agent aborted: too many classifier denials in headless mode" instead of
// prompting. AO is never in that posture: disassembly of 2.1.219 shows the flag
// has exactly two producers, both nested-loop constructors — the `avoid_prompts`
// permission layer pushed when a tool-use context is forked for a subagent that
// does not share the parent's app state, and the subagent context builder keyed
// on `agentType` / `requestDialog`. The top-level stream-json session AO spawns
// gets neither, and it supplies a CanUseTool responder, so the denial-limit
// branch takes the "falling back to prompting" path.
const claudeAutoPermissionMode = "auto"

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
		// Claude Code ≥2.1.201 only runs auto-compact when an explicit
		// auto-compact window resolves (env var, SDK option, statsig
		// experiment, or a hardcoded model table that covers only
		// claude-sonnet-5). Without one of those the pct override above
		// is dead code inside the CLI — the should-compact check bails
		// before reading it and the session runs to the hard blocking
		// limit uncompacted. Sending the thread's context window as
		// CLAUDE_CODE_AUTO_COMPACT_WINDOW opens that gate
		// deterministically; the CLI clamps the value to [100k, 1M] and
		// takes min(model max, value), so passing the resolved window
		// straight through is safe. Verified against claude 2.1.201
		// (spike: pct=1 never compacts without this var, compacts
		// immediately with it, on both 200k and 1m windows).
		if cfg.ContextWindow > 0 {
			settings.Env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] = strconv.Itoa(cfg.ContextWindow)
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

// HTTPMCPServer returns Claude Code's provider-specific streamable-HTTP
// server shape. Codex uses a different header key and has its own renderer.
func HTTPMCPServer(url string, headers map[string]string) map[string]any {
	spec := map[string]any{"url": url}
	if len(headers) > 0 {
		spec["headers"] = headers
	}
	return spec
}
