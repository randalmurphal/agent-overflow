package main

import (
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerstatus"
)

// emitProviderStatus pushes a `provider:status` event through the
// shared a.emit helper so the transport bus stamps the same per-channel
// seq it stamps on every other wire emission. Safe to call even when
// the status is "ready" — the UI treats ready as a clear-banner signal.
func (a *App) emitProviderStatus(evt providerstatus.Event) {
	a.emit(eventchan.ProviderStatus, evt)
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
		a.emitProviderStatus(providerstatus.EventFromDetect(ps))
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
func (a *App) probeStartupProviderStatuses() {
	if a.settings == nil {
		return
	}
	_, _ = a.GetProviderStatuses()
}

// emitClaudeUnauthenticatedStatus emits a `provider:status` event for
// Claude's unauthenticated case. Used by ProbeClaudeAccount when it
// succeeds with a zero-value AccountInfo — the binary is fine, the
// user just hasn't logged in yet.
func (a *App) emitClaudeUnauthenticatedStatus() {
	const message = "Claude is not authenticated. Run `claude login` to sign in."
	a.emitProviderStatus(providerstatus.Event{
		Provider:   string(provider.Claude),
		Status:     "unauthenticated",
		Message:    message,
		Actionable: true,
		ActionURL:  providerstatus.ActionURL(string(provider.Claude), "unauthenticated"),
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
	a.emitProviderStatus(providerstatus.EventFromDetect(status))
}
