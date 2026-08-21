package claudetui

import (
	"agent-overflow/internal/logging"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// Config is the resolved launch configuration for an interactive Claude TUI
// session. The app fills Binary / HookCmd / Env / EventLogger (process-level
// facts the session-options abstraction doesn't carry); the rest derives from
// SessionOptions via ConfigFromOptions.
type Config struct {
	// Binary is the resolved `claude` executable path (app-provided, same
	// source as the headless provider's binary).
	Binary string
	// HookCmd is the absolute path to the AO executable that hosts the
	// __claude-hook subcommand. Empty means "use the running executable".
	HookCmd string
	// EventLogger, when non-nil, enables the debug-only diagnostics in
	// debuglog.go: the reconstructed envelope feed and the per-request
	// /v1/messages classification. The app sets it to the same gated logger the
	// headless provider uses (NewProviderEventLogger, nil unless
	// AGENT_OVERFLOW_DEBUG opts in), so a production session pays nothing.
	EventLogger *logging.Logger
	// Model is the resolved model id (incl. the [1m] suffix when extended
	// context is requested), reusing the headless model resolution.
	Model string
	// WorkDir is the workspace the TUI operates in (must be set).
	WorkDir string
	// Resume, when set, is the provider session ref to resume (--resume).
	Resume string
	// SystemPrompt REPLACES the CLI's default system prompt for this
	// session. It reaches the TUI as `--system-prompt-file <path>`, never as
	// an argv value — same MAX_ARG_STRLEN / /proc reasoning as the headless
	// provider, and the same writer (claude.WriteSystemPromptFile). The
	// interactive TUI honors the flag exactly as headless does: the
	// request's `system` array becomes [billing header, the TUI's fixed
	// identity line, this text] (spike-verified 2.1.234 via PTY + wire
	// capture). Empty means "the CLI keeps its own prompt".
	SystemPrompt string
	// DisallowedTools are the tool names removed from the session outright
	// via one `--disallowedTools <name>` flag each; their schemas never
	// reach the request (spike-verified 2.1.234, same capture).
	//
	// Unlike the headless provider's field this is EXACTLY the settings
	// list, with no runtime-mode strip unioned in: Capabilities.
	// EnforcesRuntimeMode is false for claude-tui because the real TUI owns
	// approvals, so every tier must stay inert here by construction.
	//
	// Contract: the names here have already been through
	// claude.SanitizeDisallowedTools, so the launch appends each one
	// verbatim. Build this through ConfigFromOptions rather than by hand.
	DisallowedTools []string
	// DisableTodoReminders exports CLAUDE_CODE_TODO_REMINDER_MODE=off
	// into the PTY environment (as a default a value already in Env
	// outranks — same posture as the todo-tools opt-in; see buildEnv).
	// Silences the CLI's periodic "track your work with the todo tools"
	// nudge while keeping the tools.
	DisableTodoReminders bool
	// ReasoningEffort is the resolved `--effort` value (low/medium/high/xhigh/
	// max), already mapped from SessionOptions by claude.ConfigFromOptions.
	// Empty means "omit the flag" so the CLI keeps its own default. The
	// interactive TUI honors the same global --effort flag headless does;
	// without passing it the session runs at the model's default tier (xhigh on
	// opus-4-8) regardless of the AO selection.
	ReasoningEffort string
	// Env is the base environment for the spawned claude. Empty means inherit
	// the current process environment.
	Env []string
	// Rows / Cols seed the PTY winsize; the drawer resizes on attach. Zero
	// falls back to defaultRows / defaultCols.
	Rows uint16
	Cols uint16
	// CrossSessionEnabled / CrossSessionInbound are the peer-inbox axes
	// (claude/AGENTS.md §Cross-session messaging). They ride here for the
	// same reason they ride the headless Config: the registry is
	// machine-wide and keyed on the shared CLAUDE_CONFIG_DIR, so a TUI
	// session joins the SAME registry an AO headless thread does — "off
	// means off" cannot be a headless-only property.
	//
	// CrossSessionInbound is already resolved by claude.ConfigFromOptions
	// (it is "refuse" whenever the feature is off, never empty), and
	// launch.go states it in the `--settings` block beside the hooks. The
	// enabled half rides buildEnv, which removes the inherited
	// CLAUDE_CODE_HARBOR_KITE gate and sets it only when the setting asks.
	CrossSessionEnabled bool
	CrossSessionInbound string
	// Upstream overrides the gateway's forward target. Empty means the real
	// Anthropic API; tests point it at a stub.
	Upstream string
}

// ConfigFromOptions translates a provider-agnostic SessionOptions bundle into
// the TUI launch Config, reusing the headless provider's model / workdir /
// resume resolution so the two Claude transports never drift on those.
// Full-access is implied — the TUI provider is full-access only.
//
// The two settings-owned axes (docs/specs/prompt-tool-overrides.md) ride
// along, because the interactive TUI honors both flags:
//
//   - SystemPrompt passes through unchanged; launch.go writes it to the
//     temp file `--system-prompt-file` names.
//   - DisabledTools is taken RAW from the options rather than off
//     base.DisallowedTools, which unions in the read-only mode strip that
//     must never reach this provider. It still goes through the headless
//     provider's argv-safety pass so a name that is not one safe CLI
//     argument cannot reach argv here either, however Config was built.
func ConfigFromOptions(opts provider.SessionOptions) Config {
	base := claude.ConfigFromOptions(opts)
	return Config{
		Model:           base.Model,
		WorkDir:         base.WorkDir,
		Resume:          base.Resume,
		ReasoningEffort: base.ReasoningEffort,
		SystemPrompt:    base.SystemPrompt,
		DisallowedTools: claude.SanitizeDisallowedTools(opts.DisabledTools),
		// Settings-owned like the two axes above; claude-tui shares the
		// Claude answer (one binary, one nudge producer).
		DisableTodoReminders: opts.DisableTodoReminders,
		// Same shared-registry reasoning: one binary, one peer registry, so
		// the TUI surface takes the headless resolution verbatim rather
		// than re-deriving a second answer from the same options.
		CrossSessionEnabled: base.CrossSessionEnabled,
		CrossSessionInbound: base.CrossSessionInbound,
	}
}

const (
	defaultRows uint16 = 40
	defaultCols uint16 = 120
)

func (c Config) rows() uint16 {
	if c.Rows == 0 {
		return defaultRows
	}
	return c.Rows
}

func (c Config) cols() uint16 {
	if c.Cols == 0 {
		return defaultCols
	}
	return c.Cols
}
