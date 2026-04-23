package codex

import (
	"log"
	"strings"

	"agent-overflow/internal/provider"
)

// codexEffortFromOption translates our five-tier ReasoningEffort onto the
// ReasoningEffort enum Codex actually accepts at the wire (values verified
// against codex-source
// app-server-protocol/schema/json/v2/ThreadStartParams.json →
// ReasoningEffort: none|minimal|low|medium|high|xhigh).
//
// Our EffortMax has no direct Codex equivalent — Codex tops out at xhigh —
// so we floor Max to xhigh and log the compromise exactly once per session
// (the caller is ConfigFromOptions, which runs once per session start).
//
// The logged tier is returned unchanged so tests can detect the floor without
// re-parsing log output.
func codexEffortFromOption(effort provider.ReasoningEffort) string {
	switch effort {
	case provider.EffortLow:
		return "low"
	case provider.EffortMedium:
		return "medium"
	case provider.EffortHigh:
		return "high"
	case provider.EffortXHigh:
		return "xhigh"
	case provider.EffortMax:
		// Codex doesn't define a tier above xhigh today. Floor and surface
		// the compromise in the logs; callers (UI) already label the
		// thread "max" so users know they asked for more than Codex
		// offers.
		log.Printf("codex: effort %q mapped to xhigh (codex tops at xhigh)", effort)
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
//
// The actual string values are owned by internal/provider/types.go via
// CodexApprovalPolicy / CodexSandbox; we just compose them here so
// ConfigFromOptions doesn't have to call both helpers separately and risk
// drifting.
type codexRuntime struct {
	ApprovalPolicy string
	Sandbox        string
}

func runtimeModeToCodex(mode provider.RuntimeMode) codexRuntime {
	return codexRuntime{
		ApprovalPolicy: provider.CodexApprovalPolicy(mode),
		Sandbox:        provider.CodexSandbox(mode),
	}
}

// fastModelForCodex swaps a heavier Codex model for its mini sibling when Fast
// Mode is on. Codex's canonical "cheap and fast" tier is the `-mini` variant
// of the current default (verified against internal/provider/models.go — the
// current mini id is gpt-5.4-mini; OpenAI's current docs expose GPT-5.5 in
// Codex, but do not expose a gpt-5.5-mini sibling yet).
//
// Rules:
//   - If the current model id starts with "gpt-5" but does NOT contain
//     "mini", swap to "gpt-5.4-mini" (the latest documented mini tier).
//   - If it already contains "mini", leave it unchanged (user has already
//     opted into the cheap tier).
//   - For non-gpt-5 Codex models (o3, o4-mini, etc.) we leave the id alone;
//     the user has explicitly picked a different model family and Fast Mode
//     doesn't imply we should cross families.
//
// The swap mirrors the Claude-side helper: Fast Mode is a real effect, not a
// cosmetic toggle.
func fastModelForCodex(current string) string {
	lowered := strings.ToLower(current)
	if strings.Contains(lowered, "mini") {
		return current
	}
	if strings.HasPrefix(lowered, "gpt-5") {
		return "gpt-5.4-mini"
	}
	return current
}

// ConfigFromOptions translates a provider-agnostic SessionOptions bundle into
// the Codex-specific launch Config. Codex has a native `reasoning_effort`
// knob (unlike Claude which needs a system-prompt prefix) and carries the
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
//   - opts.ContextWindow: Codex has no user-facing context window parameter
//     today. The thread persists the field for UI parity across providers
//     but no Codex launch flag reflects it. Revisit when/if codex-source
//     exposes a knob.
func ConfigFromOptions(opts provider.SessionOptions) Config {
	runtime := runtimeModeToCodex(opts.RuntimeMode)

	model := opts.Model
	if opts.FastMode {
		model = fastModelForCodex(model)
	}

	return Config{
		Model:           model,
		WorkDir:         opts.WorkDir,
		ApprovalPolicy:  runtime.ApprovalPolicy,
		Sandbox:         runtime.Sandbox,
		ResumeThreadID:  opts.Resume,
		SystemPrompt:    opts.SystemPrompt,
		ReasoningEffort: codexEffortFromOption(opts.ReasoningEffort),
	}
}
