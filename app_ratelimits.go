package main

import (
	"context"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// rateLimitProbeInterval is how often a provider's periodic rate-limit
// probe fires while at least one matching session is alive.
const rateLimitProbeInterval = 2 * time.Minute

type rateLimitProbeLoop struct {
	probeImmediately bool
	hasActiveSession func() bool
	probe            func(context.Context)
}

func (a *App) rememberRateLimitsEvent(name string, data any) {
	if name != "provider:usage" {
		return
	}
	var usage provider.UsageEvent
	switch value := data.(type) {
	case provider.UsageEvent:
		usage = value
	case *provider.UsageEvent:
		if value == nil {
			return
		}
		usage = *value
	default:
		return
	}
	if usage.Action != "rate_limits" || usage.RateLimits == nil {
		return
	}
	incoming := cloneRateLimitsSnapshot(*usage.RateLimits)
	providerName := strings.TrimSpace(incoming.Provider)
	if providerName == "" || len(incoming.Limits) == 0 {
		return
	}
	incoming.Provider = providerName
	a.rateLimitsMu.Lock()
	defer a.rateLimitsMu.Unlock()
	if a.rateLimitsByProvider == nil {
		a.rateLimitsByProvider = make(map[string]provider.RateLimitsSnapshot)
	}
	merged, changed := mergeRateLimitsSnapshot(a.rateLimitsByProvider[providerName], incoming)
	if changed {
		a.rateLimitsByProvider[providerName] = merged
	}
}

// mergeRateLimitsSnapshot mirrors the frontend store's per-window freshness
// rules. Claude reports its 5h and 7d windows in separate events, while Codex
// normally reports both together; replacing a whole provider snapshot would
// therefore erase Claude's other ring. Delayed events must not regress a reset
// boundary or a same-window usage reading either.
func mergeRateLimitsSnapshot(current, incoming provider.RateLimitsSnapshot) (provider.RateLimitsSnapshot, bool) {
	merged := cloneRateLimitsSnapshot(current)
	if merged.Provider == "" {
		merged.Provider = incoming.Provider
	}
	indexByWindow := make(map[int]int, len(merged.Limits))
	for i, entry := range merged.Limits {
		if entry.WindowMins > 0 {
			indexByWindow[entry.WindowMins] = i
		}
	}

	changed := false
	for _, entry := range incoming.Limits {
		if entry.WindowMins <= 0 {
			continue
		}
		if index, exists := indexByWindow[entry.WindowMins]; exists {
			prior := merged.Limits[index]
			if prior.ResetsAt > entry.ResetsAt ||
				(prior.ResetsAt == entry.ResetsAt && prior.UsedPercent > entry.UsedPercent) {
				continue
			}
			if prior == entry {
				continue
			}
			merged.Limits[index] = entry
			changed = true
			continue
		}
		indexByWindow[entry.WindowMins] = len(merged.Limits)
		merged.Limits = append(merged.Limits, entry)
		changed = true
	}
	if !changed {
		return current, false
	}
	if incoming.UpdatedAt > merged.UpdatedAt {
		merged.UpdatedAt = incoming.UpdatedAt
	}
	sort.Slice(merged.Limits, func(i, j int) bool {
		return merged.Limits[i].WindowMins < merged.Limits[j].WindowMins
	})
	return merged, true
}

func cloneRateLimitsSnapshot(snapshot provider.RateLimitsSnapshot) provider.RateLimitsSnapshot {
	snapshot.Limits = append([]provider.RateLimitEntry(nil), snapshot.Limits...)
	return snapshot
}

// GetRateLimitsSnapshots returns the last known account-scoped quota for each
// provider. The data is already published on the remote-safe provider:usage
// channel, so this read-only RPC intentionally remains available to connected
// clients. It closes the first-connect/reconnect race where the startup probe
// completed before the frontend subscribed to that channel.
func (a *App) GetRateLimitsSnapshots() []provider.RateLimitsSnapshot {
	a.rateLimitsMu.RLock()
	snapshots := make([]provider.RateLimitsSnapshot, 0, len(a.rateLimitsByProvider))
	for _, snapshot := range a.rateLimitsByProvider {
		snapshots = append(snapshots, cloneRateLimitsSnapshot(snapshot))
	}
	a.rateLimitsMu.RUnlock()
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].Provider < snapshots[j].Provider
	})
	return snapshots
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
