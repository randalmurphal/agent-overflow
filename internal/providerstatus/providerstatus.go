// Package providerstatus carries the wire shape and pure mapping
// helpers behind the `provider:status` event channel.
//
// Provider-level health (install / version / auth) is pushed to the
// frontend so ProviderStatusBanner can render actionable guidance.
// This is a separate channel from the triage router's per-turn
// `provider:item_event` stream — those describe timeline content;
// these describe the binary itself.
package providerstatus

import "agent-overflow/internal/provider"

// Event is the payload for the `provider:status` wire event. The JSON
// tags pin the wire shape; relocating the Go type doesn't change what
// reaches the frontend.
type Event struct {
	// Provider is "claude" or "codex".
	Provider string `json:"provider"`

	// Status mirrors provider.ProviderStatus.Status values:
	//   "ready", "not_found", "version_too_old", "unauthenticated", "error".
	Status string `json:"status"`

	// Message is the human-friendly explanation. Always populated for
	// non-ready statuses; callers should treat it as the text the UI
	// will show if it has no status-specific branch.
	Message string `json:"message,omitempty"`

	// Version is the raw version string (e.g. "codex 0.36.0"). Empty
	// when unknown or irrelevant to the status.
	Version string `json:"version,omitempty"`

	// Actionable means the UI should render a primary action button
	// (e.g. "Install Claude CLI", "Authenticate with Claude"). When
	// false the banner stays informational.
	Actionable bool `json:"actionable"`

	// ActionURL is a deep-link / external URL the UI can open from
	// the primary action button. Empty when no URL makes sense (for
	// version_too_old the guidance is "upgrade + restart the app",
	// which doesn't map to a single URL).
	ActionURL string `json:"actionUrl,omitempty"`
}

// ActionURL returns the canonical docs / login URL for a given
// provider + status pair. Keeping the list here means the frontend
// can't invent its own URLs — every banner's primary action comes
// from this table.
func ActionURL(providerName, status string) string {
	switch providerName {
	case string(provider.Claude):
		switch status {
		case "not_found":
			return "https://docs.claude.com/en/docs/claude-code/setup"
		case "unauthenticated":
			// `claude login` is a shell instruction, not a URL; the
			// installation docs cover sign-in too, so point there.
			return "https://docs.claude.com/en/docs/claude-code/setup"
		}
	case string(provider.Codex):
		switch status {
		case "not_found":
			return "https://github.com/openai/codex#installation"
		}
	}
	return ""
}

// EventFromDetect converts the pull-only ProviderStatus returned by
// DetectProvider into the push-shape used on the wire. The "ready"
// case short-circuits to an all-default event because the UI treats
// any status=="ready" payload as "clear the banner, no action needed".
func EventFromDetect(ps provider.ProviderStatus) Event {
	if ps.Status == "ready" {
		return Event{
			Provider: ps.Provider,
			Status:   "ready",
			Version:  ps.Version,
		}
	}
	return Event{
		Provider:   ps.Provider,
		Status:     ps.Status,
		Message:    ps.Message,
		Version:    ps.Version,
		Actionable: true,
		ActionURL:  ActionURL(ps.Provider, ps.Status),
	}
}

// ClaudeUnauthenticated reports whether a ProbeAccount result
// indicates the user isn't logged in. Empty subscription + empty
// token source is the logged-out signal that routes users to
// `claude login`.
func ClaudeUnauthenticated(info provider.AccountInfo) bool {
	return info.SubscriptionType == "" && info.TokenSource == ""
}
