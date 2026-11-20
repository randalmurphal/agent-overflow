//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"agent-overflow/internal/browser/ids"
)

// holdDownloadEvents stops the engine's download reports from reaching the
// Manager until the answered release runs, as a busy event queue would.
func holdDownloadEvents(t *testing.T, manager *Manager) (release func()) {
	t.Helper()
	engine, ok := manager.engine.(*webkitEngine)
	if !ok {
		t.Fatalf("the manager runs %T, not WebKitGTK", manager.engine)
	}
	gate := make(chan struct{})
	engine.downloadEvents.Go(func() { <-gate })
	return func() { close(gate) }
}

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

func TestNativeEngineRefusesProfilesWhileStopped(t *testing.T) {
	engine := newNativeEngine(t.TempDir(), ManagerOptions{
		NativeWindow: func() unsafe.Pointer { return nil },
	}, engineEvents{})
	if _, err := engine.NewProfile(context.Background(), profileOptions{Jar: jarKey(t.TempDir()), Ephemeral: true}); err == nil {
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

// A download names the popup it began in by the handle the Manager adopts
// the popup under: a held popup's by the number its view keeps, with the
// opener it was held with, and an adopted one's by its page. A download of
// any other page names none.
func TestWebKitDownloadsNameTheirPopup(t *testing.T) {
	engine := newNativeEngine(t.TempDir(), ManagerOptions{
		NativeWindow: func() unsafe.Pointer { return nil },
	}, engineEvents{}).(*webkitEngine)
	profile := &webkitProfile{engine: engine, handle: "profile-1"}
	view := unsafe.Pointer(new(byte))
	n, handle := engine.holdPopup(view, "page-1")
	if handle != pageHandle(n) {
		t.Fatalf("held as %s, numbered %d", handle, n)
	}
	want := downloadStart{Popup: handle, Openers: []ids.PageHandle{"page-1"}, Profile: "profile-1"}
	if got := profile.downloadStart(nil, n); !reflect.DeepEqual(got, want) {
		t.Errorf("a held popup's download started as %+v, want %+v", got, want)
	}
	// Taken for adoption: the number still names it.
	engine.takePopup(handle)
	if got := profile.downloadStart(nil, n); got.Popup != handle || got.Profile != "profile-1" {
		t.Errorf("a popup being adopted's download started as %+v", got)
	}
	adopted := &webkitPage{handle: "page-2", popup: handle, opener: "page-1"}
	want = downloadStart{Frame: "page-2", Popup: handle, Openers: []ids.PageHandle{"page-1"}, Profile: "profile-1"}
	if got := profile.downloadStart(adopted, n); !reflect.DeepEqual(got, want) {
		t.Errorf("an adopted popup's download started as %+v, want %+v", got, want)
	}
	if got := profile.downloadStart(&webkitPage{handle: "page-1"}, 0); !reflect.DeepEqual(got, downloadStart{Frame: "page-1"}) {
		t.Errorf("a page's download started as %+v", got)
	}
}

// holdPopupAdoption makes the Manager adopt each popup only after hold
// (heldAdoption). Call it before the Manager opens a page.
func holdPopupAdoption(manager *Manager, hold time.Duration) {
	manager.engine.(*webkitEngine).events.PopupOpened = heldAdoption(manager, hold)
}
