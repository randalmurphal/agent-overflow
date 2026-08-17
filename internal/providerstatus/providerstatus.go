// Package providerstatus carries the wire shape and pure mapping
// helpers behind the `provider:status` event channel.
//
// Provider-level health (install / version / auth) is pushed to the
// frontend so ProviderStatusBanner can render actionable guidance.
// This is a separate channel from the triage router's per-turn
// `provider:item_event` stream — those describe timeline content;
// these describe the binary itself.
package providerstatus

import (
	"strings"

	"agent-overflow/internal/provider"
)

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

// claudeFirstPartyAPIProvider is the one `apiProvider` value for which
// the Claude CLI populates the rest of the account object. Every other
// member of the enum (gateway, bedrock, foundry, anthropicAws,
// anthropicGoogleCloud, mantle, vertex) short-circuits the builder.
const claudeFirstPartyAPIProvider = "firstParty"

// ClaudeUnauthenticated reports whether a Claude `ProbeAccount` result
// carries no evidence of a usable account — the logged-out signal that
// routes users to `claude login`.
//
// The rule is "any identity evidence at all means authenticated",
// because the CLI's account-metadata builder is far stingier than the
// wire shape suggests. Verified against claude 2.1.219: the builder
// returns early unless the resolved API provider is "firstParty", so
// for the other seven enum members (gateway, bedrock, foundry,
// anthropicAws, anthropicGoogleCloud, mantle, vertex) a fully working
// account surfaces `apiProvider` and nothing else. Within firstParty, a
// profile-sourced token populates no `tokenSource` — `email` is the only
// field that comes back. Requiring a token source would therefore report
// every external-credential backend, and every profile login, as logged
// out.
//
// Evidence is `email`, `displayName`, or a real `tokenSource`. Three
// fields that look like evidence are not:
//
// A bare `apiProvider:"firstParty"` is NOT evidence: that is the one
// value the builder emits before deciding it has nothing to add, so it
// is exactly what a signed-out first-party CLI reports.
//
// `tokenSource:"none"` is NOT evidence either — it is the CLI's explicit
// no-token marker, not a token source. Spike-verified on 2.1.219
// (2026-08-03): a startup token refresh that fails with invalid_grant
// blanks the stored tokens in place, and the very same probe then
// answers initialize with `{tokenSource:"none", apiProvider:"firstParty"}`
// and exit 0. Reading that "none" as identity evidence reported a
// just-destroyed login as authenticated, which let the destructive
// refresh failure surface only as downstream 401s.
//
// `subscriptionType` is NOT evidence: it is a storage echo, not identity.
// Spike-verified on 2.1.232 (2026-08-16): the husk the CLI writes on
// invalid_grant blanks the tokens but RETAINS the other credential
// fields, and the zero-turn probe then answers initialize with
// `{subscriptionType:"Claude Max", apiProvider:"firstParty"}` — a plan
// label for a login that no longer exists. Counting it defeated this
// predicate at the one moment it matters: app code read "authenticated",
// committed the husk over the account's credential slot, and destroyed
// it. A real subscription login also reports `email`, so nothing that
// was authenticated by this rule stops being so.
//
// Claude-only by contract. Codex hardcodes `apiProvider:"openai"` even
// when it knows nothing about the account, so a Codex AccountInfo would
// always read as authenticated here — `app_codex_probe.go` deliberately
// leaves its unauthenticated hook nil rather than reusing this.
func ClaudeUnauthenticated(info provider.AccountInfo) bool {
	// Trim so a whitespace-only field (a CLI that pads, a caller that
	// hand-builds an AccountInfo) can't masquerade as identity evidence.
	// The persisted-account path in app_provider_accounts.go already
	// trims; probe results do not.
	tokenSource := strings.TrimSpace(info.TokenSource)
	if strings.EqualFold(tokenSource, "none") {
		tokenSource = ""
	}
	if tokenSource != "" ||
		strings.TrimSpace(info.Email) != "" ||
		strings.TrimSpace(info.DisplayName) != "" {
		return false
	}
	apiProvider := strings.TrimSpace(info.APIProvider)
	return apiProvider == "" || apiProvider == claudeFirstPartyAPIProvider
}
