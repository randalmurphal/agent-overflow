package main

import (
	"context"
	"log"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

// probeCodexRateLimits runs a fresh Codex app-server account probe and emits
// only the rate-limit snapshot. It deliberately bypasses codexProbeCache:
// scheduled refreshes are about current quota state, not account-plan metadata.
//
// Every automatic trigger reaches this through codexUsageGate(), never
// directly — the gate owns coalescing concurrent triggers and bounding the
// subprocess spawn rate. Errors are logged here (rate-limit data is
// non-critical) and returned for the gate.
func (a *App) probeCodexRateLimits(ctx context.Context) error {
	// Cheap fail-fast: ctx cancellation would also short-circuit the
	// subprocess spawn below, but skipping the binary path resolution
	// and process startup is cheaper than letting them allocate and
	// immediately abort.
	if a.shuttingDown.Load() {
		return nil
	}
	if err := a.reconcileExternalProviderAccount(string(provider.Codex)); err != nil {
		log.Printf("codex: reconcile external account before rate-limit probe: %v", err)
		return err
	}
	binary := a.providerBinaryPath(string(provider.Codex))
	selection := a.captureProviderAccountSelection(string(provider.Codex))
	if selection.AccountID != "" {
		err := a.refreshProviderAccountUsage(
			ctx,
			string(provider.Codex),
			selection.AccountID,
		)
		if err != nil {
			log.Printf("codex: rate-limit probe: %v", err)
		}
		return err
	}
	var observedSnapshot *provider.RateLimitsSnapshot
	probeCfg := a.codexProbeConfig(binary, nil)
	probeCfg.OnSnapshot = func(snapshot provider.RateLimitsSnapshot) {
		// Attribution waits until account/read has been merged into the
		// probe result below. Emitting here would blindly stamp the
		// selected metadata account onto externally replaced credentials.
		observedSnapshot = &snapshot
	}
	info, err := codex.ProbeAccount(ctx, probeCfg)
	if err != nil {
		log.Printf("codex: rate-limit probe: %v", err)
		return err
	}
	if observedSnapshot == nil {
		return nil
	}
	accountID, updated, err := a.accountIDForObservedIdentity(
		string(provider.Codex),
		selection.AccountID,
		info,
	)
	if err != nil {
		log.Printf("codex: rate-limit identity: %v", err)
		return err
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
	return nil
}

// startCodexRateLimitProbeLoop re-probes Codex limits every
// rateLimitProbeInterval, and only when a Codex turn completed since the
// previous poll — each Codex probe spawns an app-server subprocess, so an
// idle app shouldn't churn processes any more than it should burn Claude
// usage requests. Startup is intentionally skipped here because
// probeStartupAccountInfo already hydrates Codex account info and
// rate-limit data once at boot.
func (a *App) startCodexRateLimitProbeLoop() {
	a.startRateLimitProbeLoop(rateLimitProbeLoop{
		probeImmediately: false,
		turnCompletedSince: func(mark time.Time) bool {
			return a.providerTurnCompletedSince(string(provider.Codex), mark)
		},
		probe: a.codexUsageGate().Request,
	})
}
