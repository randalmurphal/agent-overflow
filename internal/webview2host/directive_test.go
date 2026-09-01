package webview2host

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/keybindings"
)

func TestDirectiveValidateAcceptsEveryOp(t *testing.T) {
	for _, directive := range []Directive{
		{Op: OpCreate, PageID: "page-1", ProfileID: "ws_abc"},
		{Op: OpCreate, PageID: "page-1", ProfileID: "ws_abc", Ephemeral: true},
		{Op: OpBounds, PageID: "page-1", X: 12, Y: 34, W: 800, H: 600},
		{Op: OpBounds, PageID: "page-1", X: -1200, Y: -50, W: 1, H: 1},
		{Op: OpBounds, PageID: "page-1", X: 12, Y: 34, W: 800, H: 600, CX: 12, CY: 34, CW: 400, CH: 600, Bg: "#1a1d21"},
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
		{"bounds half clip", Directive{Op: OpBounds, PageID: "p", W: 10, H: 10, CW: 5}, "clip must be a positive pair"},
		{"bounds negative clip", Directive{Op: OpBounds, PageID: "p", W: 10, H: 10, CW: 5, CH: -1}, "clip must be a positive pair"},
		{"bounds clip NaN", Directive{Op: OpBounds, PageID: "p", W: 10, H: 10, CX: math.NaN(), CW: 5, CH: 5}, "not a finite number"},
		{"bounds bg not hex", Directive{Op: OpBounds, PageID: "p", W: 10, H: 10, Bg: "red"}, "not #rrggbb"},
		{"bounds bg short", Directive{Op: OpBounds, PageID: "p", W: 10, H: 10, Bg: "#fff"}, "not #rrggbb"},
		{"bounds bg alpha", Directive{Op: OpBounds, PageID: "p", W: 10, H: 10, Bg: "#11223344"}, "not #rrggbb"},
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
	if !reflect.DeepEqual(directive, want) {
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
	if want := (Directive{Op: OpClearData, PageID: "clear-1"}); !reflect.DeepEqual(directive, want) {
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

// The accelerator op and report are spelled on both ends of the wire like
// clear-data is, and drift would be just as silent: the launcher would
// deliver every chord to the page and the SPA would never hear Ctrl+W.
func TestAcceleratorsWireSpelling(t *testing.T) {
	var directive Directive
	raw := `{"op":"accelerators","accelerators":[{"key":"w","ctrl":true},{"key":"r","alt":true,"shift":true}]}`
	if err := json.Unmarshal([]byte(raw), &directive); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	want := Directive{Op: OpAccelerators, Accelerators: []keybindings.Accelerator{
		{Key: "w", Ctrl: true}, {Key: "r", Alt: true, Shift: true},
	}}
	if !reflect.DeepEqual(directive, want) {
		t.Fatalf("decoded = %#v, want %#v", directive, want)
	}
	if err := directive.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := (Directive{Op: OpAccelerators}).Validate(); err != nil {
		t.Fatalf("an empty set is a valid (clearing) directive: %v", err)
	}
	if err := (Directive{Op: OpAccelerators, Accelerators: make([]keybindings.Accelerator, maxAccelerators+1)}).Validate(); err == nil {
		t.Fatal("an oversized set validated")
	}
	if !ValidKind(ReportAccelerator) {
		t.Fatal("the accelerator report is not a valid kind")
	}
}
