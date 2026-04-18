package claude

import (
	"strings"

	"agent-overflow/internal/provider"
)

// effortSystemPrefix maps the provider-agnostic reasoning-effort tier onto a
// Claude-recognised trigger word that gets prepended to the system prompt.
// Claude's CLI has no native reasoning_effort flag today; these prefixes are
// the documented way to nudge the model's deliberation depth.
//
//   - Low        → "" (CLI default behaviour)
//   - Medium     → "think"
//   - High       → "think hard"
//   - XHigh      → "think harder"
//   - Max        → "ultrathink"
//
// Anything outside the enum returns empty (caller preserves the prompt
// verbatim), which matches the NormalizeReasoningEffort coercion upstream.
func effortSystemPrefix(effort provider.ReasoningEffort) string {
	switch effort {
	case provider.EffortMedium:
		return "think"
	case provider.EffortHigh:
		return "think hard"
	case provider.EffortXHigh:
		return "think harder"
	case provider.EffortMax:
		return "ultrathink"
	case provider.EffortLow:
		fallthrough
	default:
		return ""
	}
}

// composeSystemPrompt joins the effort prefix with the caller-composed system
// prompt. Either side may be empty; we never end up with stray blank lines
// when one half is missing.
func composeSystemPrompt(prefix, systemPrompt string) string {
	if prefix == "" {
		return systemPrompt
	}
	if systemPrompt == "" {
		return prefix
	}
	return prefix + "\n\n" + systemPrompt
}

// claudeOneMillionContextBeta is the header value Claude recognises to enable
// the 1M-token context window. The flag is opt-in; omitting it leaves Claude
// on its default 200k window.
const claudeOneMillionContextBeta = "context-1m-2025-08-07"

// envForContextWindow returns the environment additions required to enable
// the requested context window, if any. 200000 (and 0, the "not-set"
// sentinel from legacy threads) return nil; 1000000 sets ANTHROPIC_BETAS.
// Unknown sizes return nil so an accidental 500000 doesn't silently drop a
// real beta header we'd otherwise set.
func envForContextWindow(contextWindow int) map[string]string {
	if contextWindow == 1000000 {
		return map[string]string{"ANTHROPIC_BETAS": claudeOneMillionContextBeta}
	}
	return nil
}

// fastModelForClaude swaps a heavyweight Claude model for the Haiku tier when
// Fast Mode is on. The rules:
//
//   - If the current model id contains "opus" (or is empty, meaning the caller
//     never set one and will get the Anthropic default which is opus-class),
//     swap to "claude-haiku-4-5".
//   - If it already contains "sonnet" or "haiku", leave it alone — the user
//     has already picked a cheap model and we shouldn't second-guess them.
//
// The "contains" check is deliberately loose so new minor revisions
// (claude-opus-4-7, claude-opus-5, ...) stay routed to Haiku without a code
// update.
func fastModelForClaude(current string) string {
	lowered := strings.ToLower(current)
	if lowered == "" || strings.Contains(lowered, "opus") {
		return "claude-haiku-4-5"
	}
	// Already cheap — leave it.
	return current
}

// ConfigFromOptions translates a provider-agnostic SessionOptions bundle into
// the Claude-specific launch Config. This is the single place where the
// effort prefix, 1M-context env var, permission flags, and fast-mode model
// swap are applied. Callers in app.go pass the result straight into
// claude.NewSession plus any ancillary wiring (Binary, EventLogger) that
// lives outside the session-options abstraction.
func ConfigFromOptions(opts provider.SessionOptions) Config {
	model := opts.Model
	if opts.FastMode {
		model = fastModelForClaude(model)
	}

	systemPrompt := composeSystemPrompt(effortSystemPrefix(opts.ReasoningEffort), opts.SystemPrompt)

	return Config{
		Model:           model,
		WorkDir:         opts.WorkDir,
		Resume:          opts.Resume,
		ForkSession:     opts.ForkSession,
		SystemPrompt:    systemPrompt,
		PermissionFlags: provider.ClaudePermissionFlags(opts.RuntimeMode),
		Env:             envForContextWindow(opts.ContextWindow),
	}
}
