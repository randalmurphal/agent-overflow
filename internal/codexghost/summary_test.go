package codexghost

import "testing"

func TestGhostSummary(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"empty falls back to standalone", "", "Session ended"},
		{"whitespace falls back to standalone", "   ", "Session ended"},
		{"appends suffix", "Editing src/foo.go", "Editing src/foo.go" + SessionEndedSuffix},
		{"trims leading and trailing whitespace", "  Editing src/foo.go  ", "Editing src/foo.go" + SessionEndedSuffix},
		// A second pass over its own result changes nothing.
		{"is idempotent", "Editing src/foo.go" + SessionEndedSuffix, "Editing src/foo.go" + SessionEndedSuffix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GhostSummary(tc.in); got != tc.want {
				t.Errorf("GhostSummary(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
