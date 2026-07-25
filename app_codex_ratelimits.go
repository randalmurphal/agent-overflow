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

	if err := a.reconcileExternalProviderAccount(string(provider.Codex)); err != nil {
		log.Printf("codex: reconcile external account before rate-limit probe: %v", err)
		return
	}
	binary := a.providerBinaryPath(string(provider.Codex))
	selection := a.captureProviderAccountSelection(string(provider.Codex))
	if selection.AccountID != "" {
		if err := a.refreshProviderAccountUsage(
			ctx,
			string(provider.Codex),
			selection.AccountID,
		); err != nil {
			log.Printf("codex: rate-limit probe: %v", err)
		}
		return
	}
	var observedSnapshot *provider.RateLimitsSnapshot
	info, err := codex.ProbeAccount(ctx, codex.ProbeConfig{
		Binary: binary,
		OnSnapshot: func(snapshot provider.RateLimitsSnapshot) {
			// Attribution waits until account/read has been merged into the
			// probe result below. Emitting here would blindly stamp the
			// selected metadata account onto externally replaced credentials.
			observedSnapshot = &snapshot
		},
	})
	if err != nil {
		log.Printf("codex: rate-limit probe: %v", err)
		return
	}
	if observedSnapshot == nil {
		return
	}
	accountID, updated, err := a.accountIDForObservedIdentity(
		string(provider.Codex),
		selection.AccountID,
		info,
	)
	if err != nil {
		log.Printf("codex: rate-limit identity: %v", err)
		return
	}
	if updated != nil {
		a.emitProviderAccountIfCurrent(
			string(provider.Codex),
			*updated,
			providerAccountInfo(*updated),
		)
	}
	observedSnapshot.AccountID = accountID
	a.emitRateLimitsSnapshot(*observedSnapshot)
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
