package claude

import (
	"strconv"
	"strings"

	"agent-overflow/internal/provider"
)

type rateLimitDescriptor struct {
	limitID    string
	limitName  string
	windowMins int
}

// rateLimitStatusRejected is the one `rate_limit_info.status` value that means
// the API REFUSED the request rather than reporting headroom. It is emitted
// from the CLI's 429 handler, which has no `utilization` to report, so it is
// the discriminator `parseRateLimitEvent` admits a percentage-less snapshot on.
const rateLimitStatusRejected = "rejected"

var (
	sessionRateLimit = rateLimitDescriptor{
		limitID:    "session",
		limitName:  "Current session",
		windowMins: 300,
	}
	weeklyAllRateLimit = rateLimitDescriptor{
		limitID:    "weekly_all",
		limitName:  "All models",
		windowMins: 10080,
	}
)

// NormalizeRateLimitsSnapshot folds Claude's legacy wire/header identifiers
// onto the canonical identifiers returned by /api/oauth/usage. Unknown and
// scoped identifiers are preserved so newly introduced upstream limits remain
// visible instead of being silently grouped with an unrelated quota.
func NormalizeRateLimitsSnapshot(snapshot provider.RateLimitsSnapshot) (provider.RateLimitsSnapshot, bool) {
	normalized := snapshot
	normalized.Limits = make([]provider.RateLimitEntry, 0, len(snapshot.Limits))
	indexByLimit := make(map[string]int, len(snapshot.Limits))
	changed := false

	for _, entry := range snapshot.Limits {
		canonical, entryChanged := normalizeRateLimitEntry(entry)
		changed = changed || entryChanged

		key := normalizedRateLimitEntryKey(canonical)
		if index, exists := indexByLimit[key]; exists {
			changed = true
			if fresherRateLimitEntry(normalized.Limits[index], canonical) {
				normalized.Limits[index] = canonical
			}
			continue
		}

		indexByLimit[key] = len(normalized.Limits)
		normalized.Limits = append(normalized.Limits, canonical)
	}

	if !changed {
		return snapshot, false
	}
	return normalized, true
}

func normalizeRateLimitEntry(entry provider.RateLimitEntry) (provider.RateLimitEntry, bool) {
	descriptor, known := rateLimitDescriptorForType(entry.LimitID)
	if !known {
		return entry, false
	}
	if entry.LimitID == descriptor.limitID && entry.LimitName == descriptor.limitName {
		return entry, false
	}
	entry.LimitID = descriptor.limitID
	entry.LimitName = descriptor.limitName
	return entry, true
}

func rateLimitDescriptorForType(limitID string) (rateLimitDescriptor, bool) {
	switch strings.ToLower(strings.TrimSpace(limitID)) {
	case "five_hour", "session":
		return sessionRateLimit, true
	case "seven_day", "weekly_all":
		return weeklyAllRateLimit, true
	default:
		return rateLimitDescriptor{}, false
	}
}

func normalizedRateLimitEntryKey(entry provider.RateLimitEntry) string {
	return strings.ToLower(strings.TrimSpace(entry.LimitID)) +
		"\x00" + strconv.Itoa(entry.WindowMins)
}

// fresherRateLimitEntry reports whether candidate should replace prior when
// aliases collapse onto the same quota. A later reset boundary wins; within
// one boundary utilization is monotonic.
func fresherRateLimitEntry(prior, candidate provider.RateLimitEntry) bool {
	if candidate.ResetsAt != prior.ResetsAt {
		return candidate.ResetsAt > prior.ResetsAt
	}
	return candidate.UsedPercent > prior.UsedPercent
}
