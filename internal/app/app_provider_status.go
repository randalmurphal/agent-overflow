package app

import (
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/providerstatus"
)

// emitProviderStatus pushes a `provider:status` event through the
// shared a.emit helper so the transport bus stamps the same per-channel
// seq it stamps on every other wire emission. Safe to call even when
// the status is "ready" — the UI treats ready as a clear-banner signal.
func (a *App) emitProviderStatus(evt providerstatus.Event) {
	a.emit(eventchan.ProviderStatus, evt)
}

// probeStartupProviderStatuses runs DetectProvider for every known
// provider and pushes non-ready results through the event channel.
// Called once by the boot's unattended work; it probes on a goroutine
// startupProbeWG counts, so it never blocks app boot and Shutdown joins
// it. If settings aren't wired yet (tests, early failure) the helper is a
// no-op — DetectProvider needs a binary path.
//
// GetProviderStatuses already emits; we use it as the single entry
// point so every code path (settings refresh, startup probe, manual
// trigger) shares the same emit behavior. It cannot fail: an undetected
// provider is a status, not an error.
func (a *App) probeStartupProviderStatuses() {
	if a.settings == nil {
		return
	}
	a.startupProbeWG.Add(1)
	go func() {
		defer a.startupProbeWG.Done()
		_, _ = a.GetProviderStatuses()
	}()
}

// emitProviderStatusOnSessionStartError inspects why a session start
// failed and, when the failure looks like a provider-level problem
// (binary missing, version too old, broken install), emits a
// provider:status event so the banner can update before the start's
// caller returns. We re-run DetectProvider rather than
// pattern-matching on the session error text because the detect path
// is the authoritative source of the status string.
func (a *App) emitProviderStatusOnSessionStartError(providerName string) {
	if a.settings == nil {
		return
	}
	a.providerDiscoveryService().EmitStatusOnSessionStartError(providerName)
}
