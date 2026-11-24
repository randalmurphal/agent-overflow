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
		{"read-only", "read-only", RuntimeReadOnly},
		{"approval-required", "approval-required", RuntimeApprovalRequired},
		{"auto-accept-edits", "auto-accept-edits", RuntimeAutoAcceptEdits},
		{"auto", "auto", RuntimeAuto},
		{"full-access", "full-access", RuntimeFullAccess},
		{"empty falls back to default", "", DefaultRuntimeMode},
		{"unknown falls back to default", "yolo", DefaultRuntimeMode},
		{"case-sensitive (upper case falls back)", "FULL-ACCESS", DefaultRuntimeMode},
		{"case-sensitive read-only falls back", "Read-Only", DefaultRuntimeMode},
		{"auto-accept-edits does not truncate to auto", "auto-accept", DefaultRuntimeMode},
		{"case-sensitive auto falls back", "Auto", DefaultRuntimeMode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeRuntimeMode(tc.in); got != tc.want {
				t.Errorf("NormalizeRuntimeMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// Read-only and auto are reachable only by explicit selection. If an
	// unknown value normalized to read-only, a corrupt row would silently
	// cripple an interactive thread. Auto costs money per reviewed tool call
	// and can refuse an action, so a stale value collapsing into it would bill
	// the user for a mode they never picked.
	if DefaultRuntimeMode == RuntimeReadOnly {
		t.Error("DefaultRuntimeMode must not be read-only — unknown values would collapse into a restricted session")
	}
	if DefaultRuntimeMode == RuntimeAuto {
		t.Error("DefaultRuntimeMode must not be auto — unknown values would silently opt the user into a billed reviewer")
	}
}

// TestIsRuntimeModeTracksAllRuntimeModes proves the membership predicate is
// derived from the canonical list rather than a parallel switch. A parallel
// switch is how a new mode ends up legal in one place and coerced to
// full-access in another.
func TestIsRuntimeModeTracksAllRuntimeModes(t *testing.T) {
	for _, mode := range AllRuntimeModes {
		if !IsRuntimeMode(mode) {
			t.Errorf("IsRuntimeMode(%q) = false, want true for a canonical mode", mode)
		}
	}
	for _, mode := range []RuntimeMode{"", "yolo", "readonly", "Read-Only", "auto-review", "auto_review"} {
		if IsRuntimeMode(mode) {
			t.Errorf("IsRuntimeMode(%q) = true, want false", mode)
		}
	}
}

// TestAllRuntimeModesOrdering pins the canonical order the frontend picker
// mirrors: most- to least-restrictive on mutation, with auto between
// auto-accept-edits and full-access. The order is not decoration — the picker
// renders AllRuntimeModes' shape, so a reshuffle here silently reorders the UI.
func TestAllRuntimeModesOrdering(t *testing.T) {
	want := []RuntimeMode{
		RuntimeReadOnly,
		RuntimeApprovalRequired,
		RuntimeAutoAcceptEdits,
		RuntimeAuto,
		RuntimeFullAccess,
	}
	if len(AllRuntimeModes) != len(want) {
		t.Fatalf("AllRuntimeModes has %d entries, want %d", len(AllRuntimeModes), len(want))
	}
	for i, mode := range want {
		if AllRuntimeModes[i] != mode {
			t.Errorf("AllRuntimeModes[%d] = %q, want %q", i, AllRuntimeModes[i], mode)
		}
	}
}
