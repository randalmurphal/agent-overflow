package provider

// ProviderKind identifies the provider backend.
type ProviderKind string

const (
	Claude ProviderKind = "claude"
	Codex  ProviderKind = "codex"
	// ClaudeTUI runs the real interactive Claude Code TUI in a PTY and
	// reconstructs the normalized event stream from outside the process
	// (wire gateway + hooks). Full-access only; additive to Claude headless.
	ClaudeTUI ProviderKind = "claude-tui"
)
