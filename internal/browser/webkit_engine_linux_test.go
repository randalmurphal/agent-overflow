//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"strings"
	"testing"
	"time"
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

// The dedupe compares the whole placement, so a clip that moves while the rect
// holds still, or a scale that changes, still re-presents. Losing that would
// freeze the crop against a scroll.
func TestPaneDedupeSeesAClipThatMovedUnderAStillRect(t *testing.T) {
	rect := PaneRect{X: 10, Y: 20, Width: 300, Height: 400, ClipX: 10, ClipY: 20, ClipWidth: 300, ClipHeight: 400}
	placement := PanePlacement{Rect: rect, PageWidth: 1280, PageHeight: 720, Scale: 0.5}
	st := webkitPaneState{placement: placement, applied: placement, appliedShown: true, shown: true}
	if !webkitPaneApplied(st) {
		t.Fatal("an unchanged placement must stay deduped")
	}
	st.placement.Rect.ClipY, st.placement.Rect.ClipHeight = 60, 360
	if webkitPaneApplied(st) {
		t.Fatal("a moved clip must not be treated as already applied")
	}
	st.placement = placement
	st.placement.Scale = 0.25
	if webkitPaneApplied(st) {
		t.Fatal("a changed scale must not be treated as already applied")
	}
}

func TestNativeEngineRefusesProfilesWhileStopped(t *testing.T) {
	engine := newNativeEngine(t.TempDir(), ManagerOptions{
		NativeWindow: func() unsafe.Pointer { return nil },
	}, engineEvents{})
	if _, err := engine.NewProfile(context.Background(), profileOptions{Jar: t.TempDir(), Ephemeral: true}); err == nil {
		t.Fatal("a profile on a stopped engine must be an error, not a live session")
	}
}

// A popup loads the moment WebKit creates it, before the Manager adopts it,
// so the glue names its opener's page for its first navigations
// (ao_view_policy_page_id). Every navigation answers with the policy of the
// page named: its own once adopted, its opener's before. A page that is gone,
// or a view with none, refuses.
func TestWebKitNavigationPolicyIsThePagesOrItsOpeners(t *testing.T) {
	const (
		inside  = "file:///home/dev/repo/index.html"
		outside = "file:///home/dev/secret.txt"
	)
	page := &webkitPage{id: webkitPageSeq.Add(1), hooks: pageHooks{Allow: func(url string) bool { return url == inside }}}
	webkitPageByID.Store(page.id, page)
	t.Cleanup(func() { webkitPageByID.Delete(page.id) })
	closedPage := webkitPageSeq.Add(1)

	for _, tc := range []struct {
		name   string
		pageID uint64
		url    string
		want   bool
	}{
		{"a page or a popup's opener allows its workspace's file", page.id, inside, true},
		{"a page or a popup's opener refuses a file outside it", page.id, outside, false},
		{"a closed page or opener refuses", closedPage, inside, false},
		{"a view with no page and no opener refuses", 0, inside, false},
	} {
		if got := webkitNavigationAllowed(tc.pageID, tc.url); got != tc.want {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// holdPopupAdoption makes the Manager adopt each popup only after hold, so
// the popup decides its first navigations with no page: the window a
// profile's policy covers. Call it before the Manager opens a page.
func holdPopupAdoption(manager *Manager, hold time.Duration) {
	engine := manager.engine.(*webkitEngine)
	adopt := engine.events.PopupOpened
	engine.events.PopupOpened = func(popup enginePopup) {
		time.Sleep(hold)
		adopt(popup)
	}
}
