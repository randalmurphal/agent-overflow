package codex

import (
	"encoding/json"
	"sort"
	"time"

	"agent-overflow/internal/provider"
)

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
			continue
		}
		limits = append(limits, entry)
	}
	return limits
}

func extractCodexRateLimitEntries(params json.RawMessage) []provider.RateLimitEntry {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(params, &payload); err != nil {
		return nil
	}

	var limits []provider.RateLimitEntry
	if byID, ok := payload["rateLimitsByLimitId"]; ok {
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(byID, &entries); err == nil {
			for _, entry := range entries {
				limits = append(limits, flattenRateLimitEntry(entry)...)
			}
		}
	}

	if len(limits) == 0 {
		if entry, ok := payload["rateLimits"]; ok {
			limits = append(limits, flattenRateLimitEntry(entry)...)
		}
	}

	return limits
}

func flattenRateLimitEntry(raw json.RawMessage) []provider.RateLimitEntry {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}

	limitID := readTopLevelString(raw, "limitId")
	if limitID == "" {
		return nil
	}
	limitName := readTopLevelString(raw, "limitName")

	var limits []provider.RateLimitEntry
	for _, key := range []string{"primary", "secondary"} {
		window, ok := payload[key]
		if !ok {
			continue
		}

		entry, ok := parseRateLimitWindow(limitID, limitName, window)
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
	if payload.WindowDurationMins == 0 || payload.ResetsAt == 0 {
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
