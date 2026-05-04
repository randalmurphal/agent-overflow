package codex

import "agent-overflow/internal/provider"

// codexEffortFromOption translates AO's stored ReasoningEffort onto Codex's
// native app-server enum: none|minimal|low|medium|high|xhigh. Max is
// intentionally absent; Codex does not support it, and app/store validation
// prevents setting it on Codex threads.
func codexEffortFromOption(effort provider.ReasoningEffort) string {
	switch effort {
	case provider.EffortNone:
		return "none"
	case provider.EffortMinimal:
		return "minimal"
	case provider.EffortLow:
		return "low"
	case provider.EffortMedium:
		return "medium"
	case provider.EffortHigh:
		return "high"
	case provider.EffortXHigh:
		return "xhigh"
	default:
		// Empty / unknown — let Codex pick its own default by omitting the
		// parameter upstream.
		return ""
	}
}

// runtimeModeToCodex bundles the ApprovalPolicy + Sandbox pair that Codex
// expects for a given RuntimeMode. Splitting into one helper keeps the
// translation intentional — a RuntimeMode change must touch both fields in
// lockstep, and routing through a single function makes that obvious.
type codexRuntime struct {
	ApprovalPolicy string
	Sandbox        string
}

func runtimeModeToCodex(mode provider.RuntimeMode) codexRuntime {
	return codexRuntime{
		ApprovalPolicy: codexApprovalPolicy(mode),
		Sandbox:        codexSandbox(mode),
	}
}

func codexApprovalPolicy(mode provider.RuntimeMode) string {
	switch mode {
	case provider.RuntimeAutoAcceptEdits:
		return "on-request"
	case provider.RuntimeFullAccess:
		return "never"
	case provider.RuntimeApprovalRequired:
		fallthrough
	default:
		return "untrusted"
	}
}

func codexSandbox(mode provider.RuntimeMode) string {
	switch mode {
	case provider.RuntimeAutoAcceptEdits:
		return "workspace-write"
	case provider.RuntimeFullAccess:
		return "danger-full-access"
	case provider.RuntimeApprovalRequired:
		fallthrough
	default:
		return "read-only"
	}
}

// ConfigFromOptions translates a provider-agnostic SessionOptions bundle into
// the Codex-specific launch Config. Codex has a native `reasoning_effort`
// knob and carries the
// system prompt as `baseInstructions` on the thread/start request, so the
// shape of this translation is materially different from its Claude twin.
//
// The function intentionally does NOT handle:
//   - MCPServers: the caller (app.go) merges design-mode MCP servers in
//     after this returns, because design MCP wiring reaches into app-level
//     state (designMCP.RegisterThread) that the provider package should not
//     depend on.
//   - opts.ForkSession: Codex's fork flow is a separate `thread/fork`
//     app-server call and is NOT triggered by ForkSession. The field is a
//     no-op for Codex and we don't pretend otherwise.
func ConfigFromOptions(opts provider.SessionOptions) Config {
	runtime := runtimeModeToCodex(opts.RuntimeMode)

	model := opts.Model
	contextWindow := provider.ResolveContextWindowForModel(string(provider.Codex), model, opts.ContextWindow)
	autoCompactPercent := provider.AutoCompactPercentForContextTier(
		provider.ContextTierForModelWindow(string(provider.Codex), model, contextWindow),
		opts.AutoCompactStandardPercent,
		opts.AutoCompactExtendedPercent,
	)
	if autoCompactPercent == 0 {
		autoCompactPercent = opts.AutoCompactPercent
	}

	return Config{
		Model:                 model,
		WorkDir:               opts.WorkDir,
		ApprovalPolicy:        runtime.ApprovalPolicy,
		Sandbox:               runtime.Sandbox,
		ResumeThreadID:        opts.Resume,
		SystemPrompt:          opts.SystemPrompt,
		ReasoningEffort:       codexEffortFromOption(opts.ReasoningEffort),
		ServiceTier:           codexServiceTier(model, opts.FastMode),
		ContextWindow:         contextWindow,
		AutoCompactTokenLimit: autoCompactTokenLimit(contextWindow, autoCompactPercent),
	}
}

func codexServiceTier(model string, fastMode bool) string {
	if !fastMode || !provider.ModelSupportsCapability(string(provider.Codex), model, provider.ModelCapabilityFastMode) {
		return ""
	}
	return "fast"
}

func autoCompactTokenLimit(contextWindow, percent int) int {
	if contextWindow <= 0 || percent <= 0 {
		return 0
	}
	if percent > 90 {
		percent = 90
	}
	return contextWindow * percent / 100
}
