package provider

import "testing"

// TestNormalizeRuntimeMode covers the three valid inputs and the fallback
// paths: empty string and arbitrary junk both collapse to the default. The
// fallback is the reason the normalizer exists — without it, a stale DB
// column or a typo in a wire payload would silently flow into the provider
// config mapping and produce unpredictable CLI flags.
func TestNormalizeRuntimeMode(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want RuntimeMode
	}{
		{"approval-required", "approval-required", RuntimeApprovalRequired},
		{"auto-accept-edits", "auto-accept-edits", RuntimeAutoAcceptEdits},
		{"full-access", "full-access", RuntimeFullAccess},
		{"empty falls back to default", "", DefaultRuntimeMode},
		{"unknown falls back to default", "yolo", DefaultRuntimeMode},
		{"case-sensitive (upper case falls back)", "FULL-ACCESS", DefaultRuntimeMode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeRuntimeMode(tc.in); got != tc.want {
				t.Errorf("NormalizeRuntimeMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestAllRuntimeModesContainsEveryValue keeps the canonical list in sync
// with the const block. A new mode added without being appended here
// would bypass pickers and CHECK-constraint migrations, so this guard
// forces a coordinated update.
func TestAllRuntimeModesContainsEveryValue(t *testing.T) {
	want := map[RuntimeMode]bool{
		RuntimeApprovalRequired: true,
		RuntimeAutoAcceptEdits:  true,
		RuntimeFullAccess:       true,
	}
	got := make(map[RuntimeMode]bool, len(AllRuntimeModes))
	for _, m := range AllRuntimeModes {
		got[m] = true
	}
	for m := range want {
		if !got[m] {
			t.Errorf("AllRuntimeModes is missing %q", m)
		}
	}
	for m := range got {
		if !want[m] {
			t.Errorf("AllRuntimeModes has unknown %q — add to both lists", m)
		}
	}
}
