package browser

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// Loading reports apply newest first by their order, whatever order they are
// handled in, and a load that stops reads the page's favicon on an engine
// with a pane.
func TestLoadingKeepsTheNewestReportAndReadsTheFavicon(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page := mustOpen(t, manager, access, "https://example.test/")
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The open's own load has read no icon; reads after it find this one.
	icon := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nicon"))
	engine.mu.Lock()
	engine.favicon = icon
	engine.mu.Unlock()
	loading := func() bool { tab, _ := findTab(manager, access, page.ID); return tab.Loading }

	manager.pageLoading(p, true, 1000)
	if !loading() {
		t.Fatal("a load that started does not show")
	}
	manager.pageLoading(p, false, 999)
	if !loading() {
		t.Fatal("an older stop overrode a newer start")
	}
	manager.pageLoading(p, false, 1001)
	if loading() {
		t.Fatal("a newer stop left the page loading")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if tab, _ := findTab(manager, access, page.ID); tab.Favicon == icon {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stopped load published no favicon")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An engine with no pane reads no favicon.
func TestAnEngineWithNoPaneReadsNoFavicon(t *testing.T) {
	manager, _ := newDisposeCountingManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page := mustOpen(t, manager, access, "https://example.test/")
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	manager.pageLoading(p, true, 1000)
	manager.pageLoading(p, false, 1001)
	p.metaMu.RLock()
	reads := p.load.faviconGen
	p.metaMu.RUnlock()
	if reads != 0 {
		t.Fatalf("%d favicon reads on an engine with no pane", reads)
	}
}

func TestBoundedFaviconAcceptsOnlySmallImageDataURLs(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("icon"))
	big := base64.StdEncoding.EncodeToString(make([]byte, maxFaviconBytes+1))
	full := base64.StdEncoding.EncodeToString(make([]byte, maxFaviconBytes))
	for _, tc := range []struct {
		raw  string
		keep bool
	}{
		{"data:image/png;base64," + png, true},
		{"data:image/svg+xml;base64," + png, true},
		{"data:image/x-icon;base64," + full, true},
		{"data:image/png;base64," + big, false},
		{"data:text/html;base64," + png, false},
		{"data:image/png," + png, false},
		{"data:image/png;base64,!!", false},
		{"data:image/png;base64,", false},
		{"https://example.test/favicon.ico", false},
		{"", false},
	} {
		got := boundedFavicon(tc.raw)
		if (got != "") != tc.keep || (tc.keep && got != tc.raw) {
			t.Errorf("boundedFavicon(%.40q) = %.40q, keep %v", tc.raw, got, tc.keep)
		}
	}
}

// The CDP tracker settles on the right state under every order its two
// subscriptions deliver a page's events in, when the Manager keeps the
// newest answer.
func TestCDPLoadTrackerSettlesUnderAnyInterleaving(t *testing.T) {
	// Two loads, then a third still running: start/stop pairs alternate per
	// frame; the subscriptions may hand them over in any order.
	for _, events := range []string{"SsSsS", "sSSsS", "SSssS", "ssSSS", "SsSSs", "sSsSS", "SSSss"} {
		var tracker cdpLoadTracker
		var kept uint64
		var loading bool
		for _, event := range events {
			var now bool
			var order uint64
			if event == 'S' {
				now, order = tracker.started()
			} else {
				now, order = tracker.stopped()
			}
			if order > kept {
				kept, loading = order, now
			}
		}
		want := strings.Count(events, "S") > strings.Count(events, "s")
		if loading != want {
			t.Errorf("events %s settled loading=%v, want %v", events, loading, want)
		}
	}
}
