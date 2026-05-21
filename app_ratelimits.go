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
// gating, and shutdown semantics. The loop exits when appCtx is cancelled
// (Shutdown step 1b) so an in-flight HTTP probe aborts immediately rather
// than running to completion past the drain barrier.
func (a *App) startRateLimitProbeLoop(loop rateLimitProbeLoop) {
	ctx := a.lifeCtx()
	go func() {
		if loop.probeImmediately {
			loop.probe(ctx)
		}

		ticker := time.NewTicker(rateLimitProbeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !loop.hasActiveSession() {
					continue
				}
				loop.probe(ctx)
			}
		}
	}()
}
