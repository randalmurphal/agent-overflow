package main

import (
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	claudeprovider "agent-overflow/internal/provider/claude"
)

// rateLimitProbeInterval is how often a provider's periodic rate-limit
// probe fires while at least one matching session is alive.
const rateLimitProbeInterval = 2 * time.Minute

// rateLimitResetJitterTolerance treats small provider-side timestamp drift as
// the same quota window. Claude's OAuth usage endpoint has been observed
// moving one session boundary by a few seconds between consecutive reads; an
// exact comparison would mistake the slightly later timestamp for a newer
// window and reject every subsequent increase carrying the slightly earlier
// timestamp.
const rateLimitResetJitterTolerance = time.Minute

// rateLimitProbeLoop's probe is a usageProbeGate.Request: the gate owns
// coalescing, cooldown, backoff, and the probe's context (lifeCtx, so
// shutdown still aborts an in-flight HTTP call mid-request).
type rateLimitProbeLoop struct {
	probeImmediately bool
	// turnCompletedSince reports whether the provider finished a turn after
	// mark. The ticker polls only then: quota only moves when turns run, so
	// an idle app — however many threads it has open — costs zero requests,
	// and a burst of parallel sessions still resolves to one poll per tick.
	turnCompletedSince func(mark time.Time) bool
	probe              func()
	// interval overrides rateLimitProbeInterval; zero means production
	// cadence. Test seam — the ticker is otherwise untestable in bounded time.
	interval time.Duration
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
	cacheKey := rateLimitsCacheKey(providerName, incoming.AccountID)
	a.rateLimitsMu.Lock()
	if a.rateLimitsByProvider == nil {
		a.rateLimitsByProvider = make(map[string]provider.RateLimitsSnapshot)
	}
	merged, changed := mergeRateLimitsSnapshot(a.rateLimitsByProvider[cacheKey], incoming)
	if changed {
		a.rateLimitsByProvider[cacheKey] = merged
	}
	a.rateLimitsMu.Unlock()
	if changed && incoming.AccountID != "" && a.providerAccounts != nil {
		if err := a.providerAccounts.RememberRateLimits(providerName, incoming.AccountID, merged); err != nil {
			// Rate-limit snapshots are useful cache state but never worth
			// failing a provider turn. Keep the in-memory value and surface the
			// persistence problem in logs.
			log.Printf("rate limits: persist %s account %s: %v", providerName, incoming.AccountID, err)
		}
	}
}

// mergeRateLimitsSnapshot mirrors the frontend store's per-limit/window
// freshness rules. Additional model-scoped buckets can share a duration with
// the provider default, so limit ID participates in identity. Delayed events
// must not regress a reset boundary or same-window usage reading.
func mergeRateLimitsSnapshot(current, incoming provider.RateLimitsSnapshot) (provider.RateLimitsSnapshot, bool) {
	original := current
	if current.Provider == "" {
		current.Provider = incoming.Provider
	}
	current, normalizedCurrent := normalizeProviderRateLimitsSnapshot(current)
	incoming, _ = normalizeProviderRateLimitsSnapshot(incoming)
	merged := cloneRateLimitsSnapshot(current)
	if merged.Provider == "" {
		merged.Provider = incoming.Provider
	}
	if merged.AccountID == "" {
		merged.AccountID = incoming.AccountID
	}
	indexByWindow := make(map[string]int, len(merged.Limits))
	for i, entry := range merged.Limits {
		indexByWindow[rateLimitEntryKey(entry)] = i
	}

	changed := normalizedCurrent
	for _, entry := range incoming.Limits {
		if strings.TrimSpace(entry.LimitID) == "" {
			continue
		}
		entryKey := rateLimitEntryKey(entry)
		if index, exists := indexByWindow[entryKey]; exists {
			prior := merged.Limits[index]
			resetOrder := compareRateLimitResetBoundaries(prior.ResetsAt, entry.ResetsAt)
			if resetOrder < 0 || (resetOrder == 0 && prior.UsedPercent > entry.UsedPercent) {
				continue
			}
			if resetOrder == 0 {
				// Keep the first observed boundary stable so harmless endpoint
				// jitter does not produce a changed snapshot every probe.
				entry.ResetsAt = prior.ResetsAt
			}
			if prior == entry {
				continue
			}
			merged.Limits[index] = entry
			changed = true
			continue
		}
		indexByWindow[entryKey] = len(merged.Limits)
		merged.Limits = append(merged.Limits, entry)
		changed = true
	}
	if !changed {
		return original, false
	}
	if incoming.UpdatedAt > merged.UpdatedAt {
		merged.UpdatedAt = incoming.UpdatedAt
	}
	sort.Slice(merged.Limits, func(i, j int) bool {
		if merged.Limits[i].LimitID != merged.Limits[j].LimitID {
			return merged.Limits[i].LimitID < merged.Limits[j].LimitID
		}
		return merged.Limits[i].WindowMins < merged.Limits[j].WindowMins
	})
	return merged, true
}

// compareRateLimitResetBoundaries returns -1 when candidate is from an older
// quota window, 1 when it is from a newer window, and 0 when the timestamps
// identify the same window within the provider-jitter tolerance.
func compareRateLimitResetBoundaries(prior, candidate int64) int {
	toleranceSeconds := int64(rateLimitResetJitterTolerance / time.Second)
	switch {
	case candidate < prior-toleranceSeconds:
		return -1
	case candidate > prior+toleranceSeconds:
		return 1
	default:
		return 0
	}
}

func rateLimitsCacheKey(providerName, accountID string) string {
	return providerName + "\x00" + accountID
}

func (a *App) forgetRateLimitsSnapshot(providerName, accountID string) {
	a.rateLimitsMu.Lock()
	delete(a.rateLimitsByProvider, rateLimitsCacheKey(providerName, accountID))
	a.rateLimitsMu.Unlock()
	a.emit("provider:usage", provider.UsageEvent{
		Action: "rate_limits_removed",
		RateLimits: &provider.RateLimitsSnapshot{
			Provider:  providerName,
			AccountID: accountID,
		},
	})
}

func rateLimitEntryKey(entry provider.RateLimitEntry) string {
	return strings.ToLower(strings.TrimSpace(entry.LimitID)) + "\x00" + strconv.Itoa(entry.WindowMins)
}

func cloneRateLimitsSnapshot(snapshot provider.RateLimitsSnapshot) provider.RateLimitsSnapshot {
	snapshot.Limits = append([]provider.RateLimitEntry(nil), snapshot.Limits...)
	return snapshot
}

func normalizeProviderRateLimitsSnapshot(snapshot provider.RateLimitsSnapshot) (provider.RateLimitsSnapshot, bool) {
	if strings.EqualFold(strings.TrimSpace(snapshot.Provider), string(provider.Claude)) {
		return claudeprovider.NormalizeRateLimitsSnapshot(snapshot)
	}
	return snapshot, false
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
		if snapshots[i].Provider != snapshots[j].Provider {
			return snapshots[i].Provider < snapshots[j].Provider
		}
		return snapshots[i].AccountID < snapshots[j].AccountID
	})
	return snapshots
}

func (a *App) hydratePersistedAccountRateLimits() {
	if a.providerAccounts == nil {
		return
	}
	now := time.Now()
	type repair struct {
		providerName string
		accountID    string
		snapshot     provider.RateLimitsSnapshot
	}
	var repairs []repair
	a.rateLimitsMu.Lock()
	if a.rateLimitsByProvider == nil {
		a.rateLimitsByProvider = make(map[string]provider.RateLimitsSnapshot)
	}
	for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
		for _, account := range a.providerAccounts.List(providerName, now) {
			if account.RateLimits == nil {
				continue
			}
			snapshot := cloneRateLimitsSnapshot(*account.RateLimits)
			snapshot.Provider = providerName
			snapshot.AccountID = account.ID
			if normalized, changed := normalizeProviderRateLimitsSnapshot(snapshot); changed {
				snapshot = normalized
				repairs = append(repairs, repair{
					providerName: providerName,
					accountID:    account.ID,
					snapshot:     snapshot,
				})
			}
			a.rateLimitsByProvider[rateLimitsCacheKey(providerName, account.ID)] = snapshot
		}
	}
	a.rateLimitsMu.Unlock()

	for _, repair := range repairs {
		if err := a.providerAccounts.RememberRateLimits(repair.providerName, repair.accountID, repair.snapshot); err != nil {
			log.Printf("rate limits: repair cached %s account %s: %v", repair.providerName, repair.accountID, err)
		}
	}
}

// startRateLimitProbeLoop runs the shared app-level probe cadence for
// providers that need explicit account-limit refreshes. The probe itself stays
// provider-specific; this helper only owns startup, ticker, turn-activity
// gating, and shutdown semantics. The loop exits when appCtx is cancelled
// (Shutdown step 1b) so an in-flight HTTP probe aborts immediately rather
// than running to completion past the drain barrier.
//
// The ticker is the ONLY automatic poll: one request per interval while turns
// are completing, zero while idle. Turn completion itself records activity
// instead of probing (sessionEventHandler) — per-turn probing at the gate's
// floor is what earned server 429s on the Claude usage endpoint, whose
// throttle is shared by every machine logged into the same account.
func (a *App) startRateLimitProbeLoop(loop rateLimitProbeLoop) {
	ctx := a.lifeCtx()
	interval := loop.interval
	if interval <= 0 {
		interval = rateLimitProbeInterval
	}
	go func() {
		if loop.probeImmediately {
			loop.probe()
		}

		// lastPoll marks the moment each poll was decided, not when it
		// finished: a turn completing while a probe is in flight lands after
		// the mark and earns the next tick's poll.
		var lastPoll time.Time
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !loop.turnCompletedSince(lastPoll) {
					continue
				}
				lastPoll = time.Now()
				loop.probe()
			}
		}
	}()
}

// noteProviderTurnActivity records that a turn just completed for the
// provider. Called from the session event chokepoint; the periodic poll reads
// it through providerTurnCompletedSince.
func (a *App) noteProviderTurnActivity(providerName string) {
	a.turnActivityMu.Lock()
	if a.turnActivityByProvider == nil {
		a.turnActivityByProvider = make(map[string]time.Time)
	}
	a.turnActivityByProvider[providerName] = time.Now()
	a.turnActivityMu.Unlock()
}

// providerTurnCompletedSince reports whether the provider completed a turn
// after mark. A zero mark means "ever" — a boot with no turns yet polls
// nothing.
func (a *App) providerTurnCompletedSince(providerName string, mark time.Time) bool {
	a.turnActivityMu.Lock()
	last, ok := a.turnActivityByProvider[providerName]
	a.turnActivityMu.Unlock()
	return ok && last.After(mark)
}
