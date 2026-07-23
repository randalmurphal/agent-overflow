package main

import (
	"context"
	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

// probeCodexRateLimits runs a fresh Codex app-server account probe and emits
// only the rate-limit snapshot. It deliberately bypasses codexProbeCache:
// scheduled refreshes are about current quota state, not account-plan metadata.
func (a *App) probeCodexRateLimits(ctx context.Context) {
	// Cheap fail-fast: ctx cancellation would also short-circuit the
	// subprocess spawn below, but skipping the binary path resolution
	// and process startup is cheaper than letting them allocate and
	// immediately abort.
	if a.shuttingDown.Load() {
		return
	}
	if !a.codexRateLimitProbeRunning.CompareAndSwap(false, true) {
		return
	}
	defer a.codexRateLimitProbeRunning.Store(false)

	binary := a.providerBinaryPath(string(provider.Codex))
	selection := a.captureProviderAccountSelection(string(provider.Codex))
	_, err := codex.ProbeAccount(ctx, codex.ProbeConfig{
		Binary: binary,
		Env:    selection.Env,
		OnSnapshot: func(snapshot provider.RateLimitsSnapshot) {
			snapshot.AccountID = selection.AccountID
			a.emitRateLimitsSnapshot(snapshot)
		},
	})
	if err != nil {
		log.Printf("codex: rate-limit probe: %v", err)
	}
}

// startCodexRateLimitProbeLoop re-probes Codex limits every
// rateLimitProbeInterval while at least one Codex session is alive. Startup is
// intentionally skipped here because probeStartupAccountInfo already hydrates
// Codex account info and rate-limit data once at boot.
func (a *App) startCodexRateLimitProbeLoop() {
	a.startRateLimitProbeLoop(rateLimitProbeLoop{
		probeImmediately: false,
		hasActiveSession: a.hasActiveCodexSession,
		probe:            a.probeCodexRateLimits,
	})
}

func (a *App) hasActiveCodexSession() bool {
	return a.sessionManager().hasProvider(string(provider.Codex))
}
