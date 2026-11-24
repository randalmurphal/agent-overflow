package provider

import "slices"

// ProviderKind identifies the provider backend.
type ProviderKind string

const (
	Claude ProviderKind = "claude"
	Codex  ProviderKind = "codex"
	// ClaudeTUI runs the real interactive Claude Code TUI in a PTY and
	// reconstructs the normalized event stream from outside the process
	// (wire gateway + hooks). Full-access only; additive to Claude headless.
	ClaudeTUI ProviderKind = "claude-tui"
	// Agy runs Google Antigravity (`agy acp`) over multiplexed ACP v1 stdio.
	Agy ProviderKind = "agy"
)

var (
	allProviders = []ProviderKind{
		Claude,
		Codex,
		ClaudeTUI,
		Agy,
	}
	standaloneProviders = []ProviderKind{
		Claude,
		Codex,
		Agy,
	}
)

// AllProviders returns every known provider kind in canonical order.
// Callers must not mutate the returned slice.
func AllProviders() []ProviderKind {
	return allProviders
}

// StandaloneProviders returns every provider kind that owns its own binary,
// status check, and settings section (excluding dependent surfaces like
// claude-tui). Callers must not mutate the returned slice.
func StandaloneProviders() []ProviderKind {
	return standaloneProviders
}

// IsKnownProvider reports whether name is a supported ProviderKind.
func IsKnownProvider(name string) bool {
	return slices.Contains(allProviders, ProviderKind(name))
}
