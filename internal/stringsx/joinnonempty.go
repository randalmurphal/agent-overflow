package stringsx

import "strings"

// JoinNonEmpty trims each value and joins the non-empty results with sep.
// Useful for composing multi-section prompts where any section may be
// empty (e.g. a design-mode prompt prepended to the per-thread system
// prompt). Returns "" when every part is blank.
func JoinNonEmpty(sep string, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return strings.Join(kept, sep)
}
