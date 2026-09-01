//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"strings"
	"testing"
	"unsafe"
)

// Engine selection is the one part of the WebKit engine that can be proven
// without a display, and it is the part that decides what every OTHER
// environment gets: no window means NO engine, which is what keeps
// `--connect`, the harness, and `go test` itself off an in-process one.
// The windowless half of that rule is tag-free, in manager_test.go.

func TestNativeEngineNeedsAWindowToExist(t *testing.T) {
	if engine := newNativeEngine(t.TempDir(), ManagerOptions{}, engineEvents{}); engine != nil {
		t.Fatal("without a window provider there must be no native engine at all")
	}
}

func TestNativeEngineRefusesToStartBeforeTheWindowExists(t *testing.T) {
	// The provider exists from boot but answers nil until the app loop has
	// created the window. Starting then must fail cleanly — never reach GTK,
	// and never leave the engine reporting itself as running.
	engine := newNativeEngine(t.TempDir(), ManagerOptions{
		NativeWindow: func() unsafe.Pointer { return nil },
	}, engineEvents{})
	if engine == nil {
		t.Fatal("a window provider must select the native engine")
	}
	err := engine.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("start error = %v", err)
	}
	if engine.Running() {
		t.Fatal("a failed start must not report the engine as running")
	}
	// Stop is the shutdown path and must be safe on an engine that never
	// started, because that is exactly the state a failed boot leaves.
	engine.Stop()
	engine.Interrupt()
}

// The pane background is the one piece of the presentation path that is pure
// arithmetic, and the one whose failure is silent: an unparsed color leaves the
// view white, which reads as a flash rather than as an error.
func TestPaneBackgroundParsesSixDigitHexAndRefusesAnythingElse(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  webkitRGB
	}{
		{"#000000", webkitRGB{0, 0, 0}},
		{"#ffffff", webkitRGB{1, 1, 1}},
		{"#FFFFFF", webkitRGB{1, 1, 1}},
		{"  #ff0000  ", webkitRGB{1, 0, 0}},
		{"#00ff00", webkitRGB{0, 1, 0}},
		{"#0000ff", webkitRGB{0, 0, 1}},
		{"#1e2433", webkitRGB{30.0 / 255, 36.0 / 255, 51.0 / 255}},
	} {
		got, ok := webkitParseBackground(tc.value)
		if !ok || got != tc.want {
			t.Fatalf("webkitParseBackground(%q) = %v, %v; want %v", tc.value, got, ok, tc.want)
		}
	}
	for _, value := range []string{
		"", "   ", "#", "#fff", "#ff00ff00", "1e2433", "#12345g", "#12 456",
		"rgb(0,0,0)", "transparent", "#+00000", "#0x0000",
	} {
		if got, ok := webkitParseBackground(value); ok {
			t.Fatalf("webkitParseBackground(%q) = %v, true; want no answer", value, got)
		}
	}
}

// A background is pushed once per change, never per reported rect: the frontend
// reports a rect every changed frame, and a GTK dispatch per frame is exactly
// what the pane dedupe exists to avoid.
func TestPaneBackgroundIsPushedOnlyWhenItChanges(t *testing.T) {
	var st webkitPaneState
	if _, paint := st.takeBackground(PaneRect{}); paint {
		t.Fatal("a rect with no background must not push a color")
	}
	color, paint := st.takeBackground(PaneRect{Background: "#102030"})
	if !paint || color != (webkitRGB{16.0 / 255, 32.0 / 255, 48.0 / 255}) {
		t.Fatalf("first background = %v, %v", color, paint)
	}
	if _, paint := st.takeBackground(PaneRect{Background: "#102030"}); paint {
		t.Fatal("an unchanged background must not push again")
	}
	if _, paint := st.takeBackground(PaneRect{}); paint {
		t.Fatal("a rect that stopped carrying a background must leave the last color alone")
	}
	if _, paint := st.takeBackground(PaneRect{Background: "#405060"}); !paint {
		t.Fatal("a changed background must push")
	}
}

// The dedupe compares the whole rect, so a clip that moves while the rect holds
// still still re-presents. Losing that would freeze the crop against a scroll.
func TestPaneDedupeSeesAClipThatMovedUnderAStillRect(t *testing.T) {
	rect := PaneRect{X: 10, Y: 20, Width: 300, Height: 400, ClipX: 10, ClipY: 20, ClipWidth: 300, ClipHeight: 400}
	st := webkitPaneState{rect: rect, applied: rect, appliedShown: true, shown: true}
	if !webkitPaneApplied(st) {
		t.Fatal("an unchanged rect must stay deduped")
	}
	st.rect.ClipY, st.rect.ClipHeight = 60, 360
	if webkitPaneApplied(st) {
		t.Fatal("a moved clip must not be treated as already applied")
	}
	st.rect = rect
	st.rect.Background = "#101010"
	if webkitPaneApplied(st) {
		t.Fatal("a changed background must not be treated as already applied")
	}
}

func TestNativeEngineRefusesProfilesWhileStopped(t *testing.T) {
	engine := newNativeEngine(t.TempDir(), ManagerOptions{
		NativeWindow: func() unsafe.Pointer { return nil },
	}, engineEvents{})
	if _, err := engine.NewProfile(context.Background(), profileOptions{Workspace: t.TempDir()}); err == nil {
		t.Fatal("a profile on a stopped engine must be an error, not a live session")
	}
}
