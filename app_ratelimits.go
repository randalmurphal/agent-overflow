package main

import (
	"context"
	"time"
)

// rateLimitProbeInterval is how often a provider's periodic rate-limit
// probe fires while at least one matching session is alive.
const rateLimitProbeInterval = 2 * time.Minute

type rateLimitProbeLoop struct {
	probeImmediately bool
	hasActiveSession func() bool
	probe            func(context.Context)
}

// startRateLimitProbeLoop runs the shared app-level probe cadence for
// providers that need explicit account-limit refreshes. The probe itself stays
// provider-specific; this helper only owns startup, ticker, active-session
// gating, and shutdown semantics.
func (a *App) startRateLimitProbeLoop(loop rateLimitProbeLoop) {
	go func() {
		if loop.probeImmediately {
			loop.probe(context.Background())
		}

		ticker := time.NewTicker(rateLimitProbeInterval)
		defer ticker.Stop()
		for range ticker.C {
			if a.shuttingDown.Load() {
				return
			}
			if !loop.hasActiveSession() {
				continue
			}
			loop.probe(context.Background())
		}
	}()
}
