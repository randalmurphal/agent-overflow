package webview2host

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestDirectiveValidateAcceptsEveryOp(t *testing.T) {
	for _, directive := range []Directive{
		{Op: OpCreate, PageID: "page-1", ProfileID: "ws_abc"},
		{Op: OpCreate, PageID: "page-1", ProfileID: "ws_abc", Ephemeral: true},
		{Op: OpBounds, PageID: "page-1", X: 12, Y: 34, W: 800, H: 600},
		{Op: OpBounds, PageID: "page-1", X: -1200, Y: -50, W: 1, H: 1},
		{Op: OpShow, PageID: "page-1"},
		{Op: OpHide, PageID: "page-1"},
		{Op: OpClose, PageID: "page-1"},
		{Op: OpDevTools, PageID: "page-1"},
		{Op: OpCloseProfile, ProfileID: "ws_abc"},
		// A clear addresses the whole folder, so it names no profile: its
		// page id is purely the correlation id the report comes back under.
		{Op: OpClearData, PageID: "0123456789abcdef"},
	} {
		if err := directive.Validate(); err != nil {
			t.Errorf("Validate(%#v) = %v, want nil", directive, err)
		}
	}
}

func TestDirectiveValidateRejects(t *testing.T) {
	for _, tc := range []struct {
		name      string
		directive Directive
		wantErr   string
	}{
		{"zero value", Directive{}, "unknown browser host op"},
		{"unknown op", Directive{Op: "creat", PageID: "p"}, "unknown browser host op"},
		{"create without page", Directive{Op: OpCreate, ProfileID: "ws"}, "page id is required"},
		{"create without profile", Directive{Op: OpCreate, PageID: "p"}, "profile id is required"},
		{"show without page", Directive{Op: OpShow}, "page id is required"},
		{"close-profile without profile", Directive{Op: OpCloseProfile}, "profile id is required"},
		{"bounds zero size", Directive{Op: OpBounds, PageID: "p", W: 0, H: 10}, "positive width and height"},
		{"bounds negative size", Directive{Op: OpBounds, PageID: "p", W: 10, H: -1}, "positive width and height"},
		{"bounds NaN", Directive{Op: OpBounds, PageID: "p", X: math.NaN(), W: 1, H: 1}, "not a finite number"},
		{"bounds Inf", Directive{Op: OpBounds, PageID: "p", W: math.Inf(1), H: 1}, "not a finite number"},
		{"bounds absurd", Directive{Op: OpBounds, PageID: "p", X: 1e9, W: 1, H: 1}, "outside"},
		{"page id with path", Directive{Op: OpShow, PageID: "../evil"}, "disallowed byte"},
		{"profile id with separator", Directive{Op: OpCloseProfile, ProfileID: `a\b`}, "disallowed byte"},
		{"profile id with trailing dot", Directive{Op: OpCloseProfile, ProfileID: "ws."}, "disallowed byte"},
		{"profile id too long", Directive{Op: OpCloseProfile, ProfileID: strings.Repeat("a", 65)}, "over the 64 limit"},
		// A clear with no correlation id could never be answered: the
		// backend's waiter is keyed on it, so it would block for the whole
		// clear timeout and then report a failure the launcher never had.
		{"clear-data without correlation id", Directive{Op: OpClearData}, "page id is required"},
		{"clear-data with a path correlation id", Directive{Op: OpClearData, PageID: "../evil"}, "disallowed byte"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.directive.Validate()
			if err == nil {
				t.Fatalf("Validate(%#v) = nil, want %q", tc.directive, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// An unrecognised op must be distinguishable, because the launcher logs
// and drops on it rather than treating it as a malformed frame.
func TestDirectiveUnknownOpIsSentinelWrapped(t *testing.T) {
	err := Directive{Op: "teleport", PageID: "p"}.Validate()
	if !errors.Is(err, ErrUnknownOp) {
		t.Fatalf("Validate error = %v, want it to wrap ErrUnknownOp", err)
	}
}

// The JSON shape is the wire contract with the backend; a field rename
// here silently breaks a directive nobody would see fail until runtime.
func TestDirectiveJSONShape(t *testing.T) {
	var directive Directive
	raw := `{"op":"bounds","pageId":"page-1","profileId":"ws_abc","x":10.5,"y":20,"w":800,"h":600,"ephemeral":true}`
	if err := json.Unmarshal([]byte(raw), &directive); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	want := Directive{Op: OpBounds, PageID: "page-1", ProfileID: "ws_abc", X: 10.5, Y: 20, W: 800, H: 600, Ephemeral: true}
	if directive != want {
		t.Fatalf("decoded = %#v, want %#v", directive, want)
	}
}

// The clear-data op and its two report kinds are spelled in the backend's
// engine and in the launcher's handler, and neither would fail to compile
// if one side drifted: a renamed op reaches the launcher as an unknown
// directive that is logged and dropped, and Settings would spin until its
// own timeout with nothing anywhere naming the cause.
func TestClearDataWireSpelling(t *testing.T) {
	var directive Directive
	raw := `{"op":"clear-data","pageId":"clear-1"}`
	if err := json.Unmarshal([]byte(raw), &directive); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if want := (Directive{Op: OpClearData, PageID: "clear-1"}); directive != want {
		t.Fatalf("decoded = %#v, want %#v", directive, want)
	}
	if err := directive.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if ReportCleared != "cleared" || ReportClearFailed != "clear-failed" {
		t.Fatalf("report kinds are %q / %q", ReportCleared, ReportClearFailed)
	}
}

func TestValidKind(t *testing.T) {
	for _, kind := range []ReportKind{
		ReportCreated, ReportCreateFailed, ReportClosed, ReportProcessFailed,
		ReportCleared, ReportClearFailed,
	} {
		if !ValidKind(kind) {
			t.Errorf("ValidKind(%q) = false, want true", kind)
		}
	}
	for _, kind := range []ReportKind{"", "Created", "created ", "boom", "clear", "cleared "} {
		if ValidKind(kind) {
			t.Errorf("ValidKind(%q) = true, want false", kind)
		}
	}
}

func TestTruncateDetailBoundsOnARuneBoundary(t *testing.T) {
	short := "targetId-abc"
	if got := TruncateDetail(short); got != short {
		t.Fatalf("TruncateDetail(short) = %q, want it unchanged", got)
	}
	long := strings.Repeat("é", MaxReportDetailBytes)
	got := TruncateDetail(long)
	if len(got) > MaxReportDetailBytes {
		t.Fatalf("TruncateDetail returned %d bytes, over the %d limit", len(got), MaxReportDetailBytes)
	}
	if !isValidUTF8(got) {
		t.Fatalf("TruncateDetail split a multi-byte rune: %q", got[len(got)-4:])
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
