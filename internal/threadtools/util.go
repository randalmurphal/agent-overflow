package threadtools

import (
	"strconv"
	"strings"
	"time"
)

func trim(text string) string { return strings.TrimSpace(text) }

// parseTimestamp reads an RFC 3339 timestamp with an offset and returns
// Unix milliseconds. The field name is in the refusal so a model that
// passed the wrong one knows which.
func parseTimestamp(value, field string) (int64, error) {
	parsed, err := time.Parse(time.RFC3339, trim(value))
	if err != nil {
		return 0, invalidf("%s must be an RFC 3339 timestamp with an offset, for example 2026-09-19T14:30:00Z.", field)
	}
	return parsed.UnixMilli(), nil
}

// humanBytes renders a size the way a transcript pointer states it.
func humanBytes(size int64) string {
	switch {
	case size < 1024:
		return strconv.FormatInt(size, 10) + " B"
	case size < 1024*1024:
		return strconv.FormatFloat(float64(size)/1024, 'f', 1, 64) + " KB"
	default:
		return strconv.FormatFloat(float64(size)/(1024*1024), 'f', 1, 64) + " MB"
	}
}

// countSet counts the set selectors of a tool that takes one at a time.
func countSet(set ...bool) int {
	count := 0
	for _, isSet := range set {
		if isSet {
			count++
		}
	}
	return count
}

// dedupe reports the first value that repeats, so a refusal can name it.
func dedupe(values []string) (string, bool) {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			return value, true
		}
		seen[value] = struct{}{}
	}
	return "", false
}
