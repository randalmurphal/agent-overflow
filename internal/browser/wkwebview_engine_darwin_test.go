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

func TestNativeEngineRefusesProfilesWhileStopped(t *testing.T) {
	engine := newNativeEngine(t.TempDir(), ManagerOptions{
		NativeWindow: func() unsafe.Pointer { return nil },
	}, engineEvents{})
	if engine == nil {
		return
	}
	if _, err := engine.NewProfile(context.Background(), profileOptions{Jar: jarKey(t.TempDir()), Ephemeral: true}); err == nil {
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

// holdPopupAdoption makes the Manager adopt each popup only after hold
// (heldAdoption). Call it before the Manager opens a page.
func holdPopupAdoption(manager *Manager, hold time.Duration) {
	manager.engine.(*wkEngine).events.PopupOpened = heldAdoption(manager, hold)
}
