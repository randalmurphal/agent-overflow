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
	// Env is the base environment for the spawned claude. Empty means inherit
	// the current process environment.
	Env []string
	// Rows / Cols seed the PTY winsize; the drawer resizes on attach. Zero
	// falls back to defaultRows / defaultCols.
	Rows uint16
	Cols uint16
	// Upstream overrides the gateway's forward target. Empty means the real
	// Anthropic API; tests point it at a stub.
	Upstream string
}

// ConfigFromOptions translates a provider-agnostic SessionOptions bundle into
// the TUI launch Config, reusing the headless provider's model / workdir /
// resume resolution so the two Claude transports never drift on those.
// Full-access is implied — the TUI provider is full-access only.
func ConfigFromOptions(opts provider.SessionOptions) Config {
	base := claude.ConfigFromOptions(opts)
	return Config{
		Model:   base.Model,
		WorkDir: base.WorkDir,
		Resume:  base.Resume,
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
