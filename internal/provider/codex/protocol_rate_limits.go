package codex

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// canonicalCodexLimitID mirrors the Codex TUI's hardcoded status-line
// bucket (codex-rs/tui/src/chatwidget.rs:2891 + the status-line readers
// at :6927/:6963). Codex emits one rate-limit notification per bucket
// (`codex`, `codex_other`, `spark`, ...); the frontend store is keyed
// by (provider, windowMins) only, so a non-canonical bucket would
// overwrite the canonical 5h/7d data in the same slot. The TUI avoids
// this by reading only the `"codex"` bucket and defaulting a missing
// `limit_id` to it.
//
// The value is intentionally derived from `provider.Codex` — the two
// strings happen to coincide today, and tying them together means a
// future rename of the `ProviderKind` constant prompts a review of
// this filter (and vice versa).
const canonicalCodexLimitID = string(provider.Codex)

func normalizeRateLimitsMeta(params json.RawMessage, now time.Time) json.RawMessage {
	snapshot, ok := buildRateLimitsSnapshot(params, now.UnixMilli())
	if !ok {
		return params
	}

	meta, err := json.Marshal(snapshot)
	if err != nil {
		return params
	}
	return meta
}

func buildRateLimitsSnapshot(params json.RawMessage, updatedAt int64) (provider.RateLimitsSnapshot, bool) {
	limits := extractFlatRateLimitEntries(params)
	if len(limits) == 0 {
		limits = extractCodexRateLimitEntries(params)
	}
	if len(limits) == 0 {
		return provider.RateLimitsSnapshot{}, false
	}

	sort.Slice(limits, func(i, j int) bool {
		if limits[i].LimitID == limits[j].LimitID {
			return limits[i].WindowMins < limits[j].WindowMins
		}
		return limits[i].LimitID < limits[j].LimitID
	})

	return provider.RateLimitsSnapshot{
		Provider:  string(provider.Codex),
		Limits:    limits,
		UpdatedAt: updatedAt,
	}, true
}

// extractFlatRateLimitEntries handles a self-describing
// `{"limits":[{...RateLimitEntry...}]}` envelope — our own normalized
// shape, used when a previously-emitted snapshot is replayed through
// the parser (e.g. for tests round-tripping the wire). Codex never
// emits this shape itself, but the function must still apply the
// canonical-bucket filter so a future caller can't bypass the same
// invariant that the wire path enforces.
func extractFlatRateLimitEntries(params json.RawMessage) []provider.RateLimitEntry {
	var payload struct {
		Limits []provider.RateLimitEntry `json:"limits"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return nil
	}

	limits := make([]provider.RateLimitEntry, 0, len(payload.Limits))
	for _, entry := range payload.Limits {
		if entry.LimitID == "" {
			entry.LimitID = canonicalCodexLimitID
		}
		if !strings.EqualFold(entry.LimitID, canonicalCodexLimitID) {
			continue
		}
		limits = append(limits, entry)
	}
	return limits
}

// extractCodexRateLimitEntries parses the v2 wire shape. The
// `account/rateLimits/read` response carries BOTH a top-level
// `rateLimits` (the "backward-compatible single-bucket view") AND a
// per-bucket `rateLimitsByLimitId` map; `account/rateLimits/updated`
// notifications carry only `rateLimits`. We mirror the TUI's
// status-line behavior exactly (chatwidget.rs:6927/6963 →
// `rate_limit_snapshots_by_limit_id.get("codex")`): when the
// multi-bucket map is present, take ONLY its `"codex"` entry; never
// iterate the map. That way a future bucket name change upstream
// can't sneak through.
func extractCodexRateLimitEntries(params json.RawMessage) []provider.RateLimitEntry {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(params, &payload); err != nil {
		return nil
	}

	if byID, ok := payload["rateLimitsByLimitId"]; ok {
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(byID, &entries); err == nil {
			if entry, ok := entries[canonicalCodexLimitID]; ok {
				if limits := flattenRateLimitEntry(entry); len(limits) > 0 {
					return limits
				}
			}
		}
	}

	if entry, ok := payload["rateLimits"]; ok {
		return flattenRateLimitEntry(entry)
	}

	return nil
}

// flattenRateLimitEntry expands one RateLimitSnapshot envelope into
// per-window entries. Codex's wire `limit_id` is Option<String>; the
// default-bucket case arrives as `"limitId": null`. The TUI defaults
// missing values to "codex" (chatwidget.rs:2891) — we mirror that AND
// drop any explicit non-canonical bucket, since the frontend store is
// keyed by (provider, windowMins) only and can't distinguish buckets.
func flattenRateLimitEntry(raw json.RawMessage) []provider.RateLimitEntry {
	var payload struct {
		LimitID   string          `json:"limitId"`
		LimitName string          `json:"limitName"`
		Primary   json.RawMessage `json:"primary"`
		Secondary json.RawMessage `json:"secondary"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}

	limitID := payload.LimitID
	if limitID == "" {
		limitID = canonicalCodexLimitID
	}
	if !strings.EqualFold(limitID, canonicalCodexLimitID) {
		return nil
	}

	var limits []provider.RateLimitEntry
	for _, window := range []json.RawMessage{payload.Primary, payload.Secondary} {
		if len(window) == 0 {
			continue
		}
		entry, ok := parseRateLimitWindow(limitID, payload.LimitName, window)
		if !ok {
			continue
		}
		limits = append(limits, entry)
	}

	return limits
}

func parseRateLimitWindow(limitID, limitName string, raw json.RawMessage) (provider.RateLimitEntry, bool) {
	var payload struct {
		UsedPercent        float64 `json:"usedPercent"`
		WindowDurationMins int     `json:"windowDurationMins"`
		ResetsAt           int64   `json:"resetsAt"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return provider.RateLimitEntry{}, false
	}
	// Defense-in-depth: a misbehaving codex subprocess could send
	// negative values that would slip through an `== 0` guard. The
	// stale-event check in the frontend store relies on resetsAt
	// strictly increasing across windows, so a negative resetsAt could
	// suppress legitimate later updates.
	if payload.WindowDurationMins <= 0 || payload.ResetsAt <= 0 {
		return provider.RateLimitEntry{}, false
	}

	return provider.RateLimitEntry{
		LimitID:     limitID,
		LimitName:   limitName,
		UsedPercent: payload.UsedPercent,
		WindowMins:  payload.WindowDurationMins,
		ResetsAt:    payload.ResetsAt,
	}, true
}
