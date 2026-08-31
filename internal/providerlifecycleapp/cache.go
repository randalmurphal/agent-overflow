package providerlifecycleapp

import (
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	claudeprovider "agent-overflow/internal/provider/claude"
)

const rateLimitResetJitterTolerance = time.Minute

// RememberEvent observes the provider:usage channel before root publishes it.
// Persistence remains synchronous and precedes bus emission, matching the
// original event ordering.
func (s *Service) RememberEvent(name eventchan.Channel, data any) {
	if name != eventchan.ProviderUsage {
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
	incoming := CloneSnapshot(*usage.RateLimits)
	providerName := strings.TrimSpace(incoming.Provider)
	if providerName == "" || len(incoming.Limits) == 0 {
		return
	}
	incoming.Provider = providerName
	cacheKey := CacheKey(providerName, incoming.AccountID)
	s.cacheMu.Lock()
	merged, changed := MergeSnapshot(s.cache[cacheKey], incoming)
	if changed {
		s.cache[cacheKey] = merged
	}
	s.cacheMu.Unlock()
	if changed && incoming.AccountID != "" && s.deps.Accounts.RememberRateLimit != nil {
		if err := s.deps.Accounts.RememberRateLimit(providerName, incoming.AccountID, merged); err != nil {
			log.Printf("rate limits: persist %s account %s: %v", providerName, incoming.AccountID, err)
		}
	}
}

// MergeSnapshot applies per-limit/window freshness without regressing quota
// windows or same-window usage.
func MergeSnapshot(current, incoming provider.RateLimitsSnapshot) (provider.RateLimitsSnapshot, bool) {
	original := current
	if current.Provider == "" {
		current.Provider = incoming.Provider
	}
	current, normalizedCurrent := NormalizeSnapshot(current)
	incoming, _ = NormalizeSnapshot(incoming)
	merged := CloneSnapshot(current)
	if merged.Provider == "" {
		merged.Provider = incoming.Provider
	}
	if merged.AccountID == "" {
		merged.AccountID = incoming.AccountID
	}
	indexByWindow := make(map[string]int, len(merged.Limits))
	for i, entry := range merged.Limits {
		indexByWindow[entryKey(entry)] = i
	}
	changed := normalizedCurrent
	for _, entry := range incoming.Limits {
		if strings.TrimSpace(entry.LimitID) == "" {
			continue
		}
		key := entryKey(entry)
		if index, exists := indexByWindow[key]; exists {
			prior := merged.Limits[index]
			resetOrder := compareResetBoundaries(prior.ResetsAt, entry.ResetsAt)
			if resetOrder < 0 || (resetOrder == 0 && prior.UsedPercent > entry.UsedPercent) {
				continue
			}
			if resetOrder == 0 {
				entry.ResetsAt = prior.ResetsAt
			}
			if prior == entry {
				continue
			}
			merged.Limits[index] = entry
			changed = true
			continue
		}
		indexByWindow[key] = len(merged.Limits)
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

func compareResetBoundaries(prior, candidate int64) int {
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

func entryKey(entry provider.RateLimitEntry) string {
	return strings.ToLower(strings.TrimSpace(entry.LimitID)) + "\x00" + strconv.Itoa(entry.WindowMins)
}

func CacheKey(providerName, accountID string) string { return providerName + "\x00" + accountID }

func CloneSnapshot(snapshot provider.RateLimitsSnapshot) provider.RateLimitsSnapshot {
	snapshot.Limits = append([]provider.RateLimitEntry(nil), snapshot.Limits...)
	return snapshot
}

func NormalizeSnapshot(snapshot provider.RateLimitsSnapshot) (provider.RateLimitsSnapshot, bool) {
	if strings.EqualFold(strings.TrimSpace(snapshot.Provider), string(provider.Claude)) {
		return claudeprovider.NormalizeRateLimitsSnapshot(snapshot)
	}
	return snapshot, false
}

// Forget removes one account snapshot and publishes the removal after the
// cache mutation.
func (s *Service) Forget(providerName, accountID string) {
	s.cacheMu.Lock()
	delete(s.cache, CacheKey(providerName, accountID))
	s.cacheMu.Unlock()
	if s.deps.Emit != nil {
		s.deps.Emit(eventchan.ProviderUsage, provider.UsageEvent{
			Action: "rate_limits_removed",
			RateLimits: &provider.RateLimitsSnapshot{
				Provider: providerName, AccountID: accountID,
			},
		})
	}
}

// Snapshots returns a sorted defensive copy for the Wails façade.
func (s *Service) Snapshots() []provider.RateLimitsSnapshot {
	s.cacheMu.RLock()
	snapshots := make([]provider.RateLimitsSnapshot, 0, len(s.cache))
	for _, snapshot := range s.cache {
		snapshots = append(snapshots, CloneSnapshot(snapshot))
	}
	s.cacheMu.RUnlock()
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].Provider != snapshots[j].Provider {
			return snapshots[i].Provider < snapshots[j].Provider
		}
		return snapshots[i].AccountID < snapshots[j].AccountID
	})
	return snapshots
}

// Hydrate loads persisted account snapshots and repairs normalized Claude rows.
func (s *Service) Hydrate() {
	if s.deps.Accounts.List == nil {
		return
	}
	type repair struct {
		providerName string
		accountID    string
		snapshot     provider.RateLimitsSnapshot
	}
	var repairs []repair
	s.cacheMu.Lock()
	for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
		for _, account := range s.deps.Accounts.List(providerName) {
			if account.RateLimits == nil {
				continue
			}
			snapshot := CloneSnapshot(*account.RateLimits)
			snapshot.Provider = providerName
			snapshot.AccountID = account.ID
			if normalized, changed := NormalizeSnapshot(snapshot); changed {
				snapshot = normalized
				repairs = append(repairs, repair{providerName, account.ID, snapshot})
			}
			s.cache[CacheKey(providerName, account.ID)] = snapshot
		}
	}
	s.cacheMu.Unlock()
	for _, repair := range repairs {
		if s.deps.Accounts.RememberRateLimit != nil {
			if err := s.deps.Accounts.RememberRateLimit(repair.providerName, repair.accountID, repair.snapshot); err != nil {
				log.Printf("rate limits: repair cached %s account %s: %v", repair.providerName, repair.accountID, err)
			}
		}
	}
}
