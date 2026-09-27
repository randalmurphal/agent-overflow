//go:build darwin && cgo && !ios && !server && !nogui

package browser

import (
	"context"
	"strings"
	"testing"
	"unsafe"
)

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
	if _, err := engine.NewProfile(context.Background(), profileOptions{Workspace: t.TempDir()}); err == nil {
		t.Fatal("a profile on a stopped engine must be an error, not a live session")
	}
}

// A popup loads the moment WebKit creates it, before the Manager adopts it,
// so its first navigations reach Go with no page id. What answers them is the
// file boundary: the workspace's policy until adoption, the page's own after,
// and a refusal once either is gone.
func TestWKNavigationPolicyCoversAPopupBeforeAdoption(t *testing.T) {
	const (
		inside   = "file:///Users/dev/repo/index.html"
		outside  = "file:///Users/dev/secret.txt"
		pageOnly = "https://page.test/"
	)
	profile := &wkProfile{id: wkProfileSeq.Add(1), allow: func(url string) bool { return url == inside }}
	wkProfileByID.Store(profile.id, profile)
	t.Cleanup(func() { wkProfileByID.Delete(profile.id) })
	page := &wkPage{id: wkPageSeq.Add(1), hooks: pageHooks{Allow: func(url string) bool { return url == pageOnly }}}
	wkPageByID.Store(page.id, page)
	t.Cleanup(func() { wkPageByID.Delete(page.id) })
	closedPage, disposedProfile := wkPageSeq.Add(1), wkProfileSeq.Add(1)

	for _, tc := range []struct {
		name              string
		pageID, profileID uint64
		url               string
		want              bool
	}{
		{"an unadopted popup loads a workspace file", 0, profile.id, inside, true},
		{"an unadopted popup is refused a file outside the workspace", 0, profile.id, outside, false},
		{"an adopted page answers with its own policy", page.id, profile.id, pageOnly, true},
		{"an adopted page does not fall back to the workspace's policy", page.id, profile.id, inside, false},
		{"a closed page refuses", closedPage, profile.id, inside, false},
		{"a popup of a disposed profile refuses", 0, disposedProfile, inside, false},
	} {
		if got := wkNavigationAllowed(tc.pageID, tc.profileID, tc.url); got != tc.want {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The workspace policy is what answers for a popup before adoption, so a
// profile without one must not exist.
func TestNativeEngineRefusesAProfileWithoutANavigationPolicy(t *testing.T) {
	engine := newNativeEngine(t.TempDir(), ManagerOptions{
		NativeWindow: func() unsafe.Pointer { return nil },
	}, engineEvents{}).(*wkEngine)
	engine.started = true
	_, err := engine.NewProfile(context.Background(), profileOptions{Workspace: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "navigation policy") {
		t.Fatalf("profile error = %v, want a missing navigation policy refusal", err)
	}
}
