package main

import (
	"log"

	"agent-overflow/internal/provider"
)

// ProviderStatusEvent is the payload for the `provider:status` Wails
// event. It surfaces provider-level health (install / version / auth)
// to the frontend so ProviderStatusBanner can render actionable
// guidance. This is a separate channel from the triage router's
// per-turn `provider:item_event` stream — those describe timeline
// content; these describe the binary itself.
type ProviderStatusEvent struct {
	// Provider is "claude" or "codex".
	Provider string `json:"provider"`

	// Status mirrors ProviderStatus.Status values:
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

// providerActionURL returns the canonical docs / login URL for a
// given provider + status pair. Keeping the list here means the
// frontend can't invent its own URLs — every banner's primary action
// comes from this table.
func providerActionURL(providerName, status string) string {
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

// emitProviderStatus pushes a `provider:status` event through the
// shared a.emit helper so the transport bus stamps the same per-channel
// seq it stamps on every other wire emission. Safe to call even when
// the status is "ready" — the UI treats ready as a clear-banner signal.
func (a *App) emitProviderStatus(evt ProviderStatusEvent) {
	a.emit("provider:status", evt)
}

// providerStatusEventFromDetect converts the pull-only ProviderStatus
// returned by DetectProvider into the push-shape used on the wire.
// The "ready" case short-circuits to an all-default event because the
// UI treats any status=="ready" payload as "clear the banner, no
// action needed".
func providerStatusEventFromDetect(ps provider.ProviderStatus) ProviderStatusEvent {
	if ps.Status == "ready" {
		return ProviderStatusEvent{
			Provider: ps.Provider,
			Status:   "ready",
			Version:  ps.Version,
		}
	}
	return ProviderStatusEvent{
		Provider:   ps.Provider,
		Status:     ps.Status,
		Message:    ps.Message,
		Version:    ps.Version,
		Actionable: true,
		ActionURL:  providerActionURL(ps.Provider, ps.Status),
	}
}

// emitProviderStatusesFromDetect emits a provider:status event per
// non-ready provider status in `statuses`. Callers that also want to
// clear stale banners for providers that are now ready should emit
// the ready event themselves — this helper deliberately skips them
// to keep the startup path quiet on the happy path.
func (a *App) emitProviderStatusesFromDetect(statuses []provider.ProviderStatus) {
	for _, ps := range statuses {
		if ps.Status == "ready" {
			continue
		}
		a.emitProviderStatus(providerStatusEventFromDetect(ps))
	}
}

// probeStartupProviderStatuses runs DetectProvider for every known
// provider and pushes non-ready results through the event channel.
// Called once from ServiceStartup in a goroutine so it never blocks
// app boot. If settings aren't wired yet (tests, early failure) the
// helper is a no-op — DetectProvider needs a binary path.
//
// GetProviderStatuses already emits; we use it as the single entry
// point so every code path (settings refresh, startup probe, manual
// trigger) shares the same emit behavior.
//
// Auto-flip: if defaultProvider points at a not_found provider AND the
// other provider is ready, swap the default to the ready one. Common
// case is "user installed Codex but the saved default is Claude" or
// vice versa — without the auto-flip, every new thread fails until the
// user finds the dropdown in Settings. We surface a status event for
// the now-default provider so the UI can render a toast / banner
// explaining the swap.
func (a *App) probeStartupProviderStatuses() {
	if a.settings == nil {
		return
	}
	statuses, err := a.GetProviderStatuses()
	if err != nil {
		return
	}
	a.maybeAutoFlipDefaultProvider(statuses)
}

// maybeAutoFlipDefaultProvider swaps DefaultProvider when the current
// default is missing and exactly one other provider is ready. The
// helper is exported via test wiring rather than directly so callers
// can validate by passing curated ProviderStatus slices.
//
// We never auto-flip when the current default is already ready, even
// if the other provider is also ready — that path doesn't help the
// user. We never auto-flip back: once swapped to the ready provider,
// a future restart with both unhealthy leaves the user's swapped
// default alone so the auto-flip can't loop between configurations.
func (a *App) maybeAutoFlipDefaultProvider(statuses []provider.ProviderStatus) {
	if a.settings == nil {
		return
	}
	cfg := a.settings.Get()
	current := cfg.DefaultProvider
	if current == "" {
		return
	}
	currentStatus, otherStatus := pickProviderStatuses(statuses, current)
	if currentStatus == nil || otherStatus == nil {
		return
	}
	if currentStatus.Status == "ready" {
		return
	}
	if currentStatus.Status != "not_found" {
		return
	}
	if otherStatus.Status != "ready" {
		return
	}
	patch := map[string]any{"defaultProvider": otherStatus.Provider}
	if _, err := a.settings.Update(patch); err != nil {
		log.Printf("provider auto-flip: update settings failed: %v", err)
		return
	}
	log.Printf("provider auto-flip: defaultProvider %q -> %q (current not_found, other ready)", current, otherStatus.Provider)
	a.emitEvent("provider:default_swapped", map[string]any{
		"from":     current,
		"to":       otherStatus.Provider,
		"reason":   "not_found",
		"otherCli": providerDisplayName(otherStatus.Provider),
		"fromCli":  providerDisplayName(current),
	})
}

// pickProviderStatuses returns the (current, other) pair from the
// detect slice. The "other" is whichever provider isn't `current`. If
// statuses contains zero or one entry the helper returns nil for the
// missing slots.
func pickProviderStatuses(statuses []provider.ProviderStatus, current string) (*provider.ProviderStatus, *provider.ProviderStatus) {
	var cur, other *provider.ProviderStatus
	for i := range statuses {
		s := &statuses[i]
		if s.Provider == current {
			cur = s
		} else {
			other = s
		}
	}
	return cur, other
}

// providerDisplayName returns the user-facing label for a provider
// id. Kept as a tiny lookup so log + event payloads stay consistent
// with the welcome-panel pill copy.
func providerDisplayName(name string) string {
	switch name {
	case string(provider.Claude):
		return "Claude"
	case string(provider.Codex):
		return "Codex"
	default:
		return name
	}
}

// claudeUnauthenticatedStatus reports whether a ProbeAccount result
// indicates the user isn't logged in. forge uses the same signal
// (empty subscription + empty token source) to route users to
// `claude login`.
func claudeUnauthenticatedStatus(info provider.AccountInfo) bool {
	return info.SubscriptionType == "" && info.TokenSource == ""
}

// emitClaudeUnauthenticatedStatus emits a `provider:status` event for
// Claude's unauthenticated case. Used by ProbeClaudeAccount when it
// succeeds with a zero-value AccountInfo — the binary is fine, the
// user just hasn't logged in yet.
func (a *App) emitClaudeUnauthenticatedStatus() {
	const message = "Claude is not authenticated. Run `claude login` to sign in."
	a.emitProviderStatus(ProviderStatusEvent{
		Provider:   string(provider.Claude),
		Status:     "unauthenticated",
		Message:    message,
		Actionable: true,
		ActionURL:  providerActionURL(string(provider.Claude), "unauthenticated"),
	})
}

// emitProviderStatusOnSessionStartError inspects why a session start
// failed and, when the failure looks like a provider-level problem
// (binary missing, version too old, broken install), emits a
// provider:status event so the banner can update before the
// StartSession RPC returns. We re-run DetectProvider rather than
// pattern-matching on the session error text because the detect path
// is the authoritative source of the status string.
func (a *App) emitProviderStatusOnSessionStartError(providerName string) {
	if a.settings == nil {
		return
	}
	status := provider.DetectProvider(providerName, a.providerBinaryPath(providerName))
	if status.Status == "ready" {
		// The binary is fine — the session failed for some other
		// reason (transport, workspace, timeout). No banner.
		return
	}
	a.emitProviderStatus(providerStatusEventFromDetect(status))
}
