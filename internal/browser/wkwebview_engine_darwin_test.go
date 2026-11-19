//go:build darwin && cgo && !ios && !server && !nogui

package browser

import (
	"context"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// holdDownloadEvents stops the engine's download reports from reaching the
// Manager until the answered release runs, as a busy event queue would.
func holdDownloadEvents(t *testing.T, manager *Manager) (release func()) {
	t.Helper()
	engine, ok := manager.engine.(*wkEngine)
	if !ok {
		t.Fatalf("the manager runs %T, not WKWebView", manager.engine)
	}
	gate := make(chan struct{})
	engine.downloadEvents.Go(func() { <-gate })
	return func() { close(gate) }
}

// Engine selection is the one part of the WKWebView engine that can be proven
// without a window, and it is the part that decides what every OTHER
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
	// created the window. Starting then must fail cleanly — never reach AppKit,
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

// The pane background is the one piece of the present path that is pure Go, and
// its failure mode is silent: a mis-parsed colour paints the wrong background
// behind a page rather than raising anything.
func TestPaneBackgroundCodeParsesOnlyRRGGBB(t *testing.T) {
	for value, want := range map[string]int{
		"#000000": 0x000000,
		"#ffffff": 0xffffff,
		"#1a1B26": 0x1a1b26,
		"#0a0b0c": 0x0a0b0c,
		// Everything that is not exactly #rrggbb is "no colour", never a
		// partially parsed one.
		"":           wkNoBackground,
		"#fff":       wkNoBackground,
		"1a1b26":     wkNoBackground,
		"#1a1b2":     wkNoBackground,
		"#1a1b26 ":   wkNoBackground,
		" #1a1b26":   wkNoBackground,
		"#1a1b2g":    wkNoBackground,
		"#12345678":  wkNoBackground,
		"rgb(0,0,0)": wkNoBackground,
	} {
		if got := wkBackgroundCode(value); got != want {
			t.Fatalf("wkBackgroundCode(%q) = %#x, want %#x", value, got, want)
		}
	}
}

func TestNativeEngineRefusesProfilesWhileStopped(t *testing.T) {
	engine := newNativeEngine(t.TempDir(), ManagerOptions{
		NativeWindow: func() unsafe.Pointer { return nil },
	}, engineEvents{})
	if engine == nil {
		return
	}
	if _, err := engine.NewProfile(context.Background(), profileOptions{Jar: t.TempDir(), Ephemeral: true}); err == nil {
		t.Fatal("a profile on a stopped engine must be an error, not a live session")
	}
}

// A popup loads the moment WebKit creates it, before the Manager adopts it,
// so the glue names its opener's page for its first navigations
// (ao_view_policy_page_id). Every navigation answers with the policy of the
// page named: its own once adopted, its opener's before. A page that is gone,
// or a view with none, refuses.
func TestWKNavigationPolicyIsThePagesOrItsOpeners(t *testing.T) {
	const (
		inside  = "file:///Users/dev/repo/index.html"
		outside = "file:///Users/dev/secret.txt"
	)
	page := &wkPage{id: wkPageSeq.Add(1), hooks: pageHooks{Allow: func(url string) bool { return url == inside }}}
	wkPageByID.Store(page.id, page)
	t.Cleanup(func() { wkPageByID.Delete(page.id) })
	closedPage := wkPageSeq.Add(1)

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
		if got := wkNavigationAllowed(tc.pageID, tc.url); got != tc.want {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// holdPopupAdoption makes the Manager hear of each popup only after hold,
// so the popup decides its first navigations with no page: the window a
// profile's policy covers. The engine's call returns at once, as the
// Manager's does (engineEvents). Call it before the Manager opens a page.
func holdPopupAdoption(manager *Manager, hold time.Duration) {
	engine := manager.engine.(*wkEngine)
	adopt := engine.events.PopupOpened
	engine.events.PopupOpened = func(popup enginePopup) {
		go func() {
			time.Sleep(hold)
			adopt(popup)
		}()
	}
}
