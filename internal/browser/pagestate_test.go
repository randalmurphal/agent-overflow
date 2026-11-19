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
	waitFavicon(t, manager, access, page.ID, icon)
}

// waitFavicon waits until the tab shows want and no favicon read runs.
func waitFavicon(t *testing.T, m *Manager, access Access, pageID, want string) {
	t.Helper()
	p, _, err := m.lookupOwnedPage(access, pageID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		tab, _ := findTab(m, access, pageID)
		p.metaMu.RLock()
		idle := !p.load.reading && !p.load.want
		p.metaMu.RUnlock()
		if tab.Favicon == want && idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("favicon %.40q (reading: %v), want %.40q", tab.Favicon, !idle, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func iconURL(name string) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n"+name))
}

func setFavicon(engine *lifecycleEngine, icon string) {
	engine.mu.Lock()
	engine.favicon = icon
	engine.mu.Unlock()
}

// A load that starts and stops before the engine page is attached (a popup
// loading while it is adopted) is recorded; its favicon is read once the page
// has its driver, never before.
func TestALoadBeforeAttachmentReadsTheFaviconOnceAttached(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	setFavicon(engine, iconURL("early"))
	engine.onNewPage = func(hooks pageHooks) {
		hooks.Loading(true, 1)
		hooks.Loading(false, 2)
		// A read begun now would find no driver or context.
		time.Sleep(20 * time.Millisecond)
	}
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page, err := manager.NewCompanionPage(t.Context(), access, "")
	if err != nil {
		t.Fatal(err)
	}
	waitFavicon(t, manager, access, page.ID, iconURL("early"))
}

// A stop handled before its load's start still reads the favicon: the start,
// older, is dropped, and no loading edge was seen.
func TestAStopHandledBeforeItsStartReadsTheFavicon(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page := mustOpen(t, manager, access, "https://example.test/")
	waitFavicon(t, manager, access, page.ID, "")
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	setFavicon(engine, iconURL("next"))
	manager.pageLoading(p, false, 1002)
	manager.pageLoading(p, true, 1001)
	waitFavicon(t, manager, access, page.ID, iconURL("next"))
	if tab, _ := findTab(manager, access, page.ID); tab.Loading {
		t.Fatal("the older start left the page loading")
	}
}

// A favicon read that a new load overtakes keeps nothing: its answer is the
// old document's. The new load's stop reads the new one.
func TestAReadANewLoadOvertakesIsDropped(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page := mustOpen(t, manager, access, "https://example.test/")
	waitFavicon(t, manager, access, page.ID, "")
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	release := engine.hold(&engine.evalGate)
	defer release()
	setFavicon(engine, iconURL("old"))
	manager.pageLoading(p, false, 1001)
	receive(t, engine.evaluating, "the old document's favicon read")
	manager.pageLoading(p, true, 1002)
	setFavicon(engine, iconURL("new"))
	release()
	waitFavicon(t, manager, access, page.ID, "")
	manager.pageLoading(p, false, 1003)
	waitFavicon(t, manager, access, page.ID, iconURL("new"))
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
	reads := p.load.reads
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
	at := time.Unix(1000, 0)
	for _, events := range []string{"SsSsS", "sSSsS", "SSssS", "ssSSS", "SsSSs", "sSsSS", "SSSss"} {
		var tracker cdpLoadTracker
		var kept uint64
		var loading bool
		for _, event := range events {
			var now bool
			var order uint64
			if event == 'S' {
				now, order = tracker.started(at)
			} else {
				now, order = tracker.stopped(at)
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

// A page attached mid-load (a popup) sees the stop of a load whose start
// was before it subscribed. Its later loads still read as loading, from
// their start to their stop; a stop handled just before its own start
// still reads as idle.
func TestCDPLoadTrackerRecoversFromAStopWithNoStart(t *testing.T) {
	var tracker cdpLoadTracker
	var kept uint64
	// step handles one event and answers what the Manager keeps.
	step := func(event func(time.Time) (bool, uint64), at time.Time) bool {
		t.Helper()
		loading, order := event(at)
		if order <= kept {
			t.Fatalf("answer numbered %d after %d: the Manager would drop it", order, kept)
		}
		kept = order
		return loading
	}
	at := time.Unix(1000, 0)
	if step(tracker.stopped, at) {
		t.Fatal("the stop of the load running at attach reads as loading")
	}
	for load := range 3 {
		at = at.Add(2 * loadReorderWindow)
		if !step(tracker.started, at) {
			t.Fatalf("load %d: its start reads as idle", load)
		}
		at = at.Add(2 * loadReorderWindow)
		if step(tracker.stopped, at) {
			t.Fatalf("load %d: its stop reads as loading", load)
		}
	}
	// A load whose stop is handled before its start, within the window.
	if step(tracker.stopped, at) {
		t.Fatal("a stop handled first reads as loading")
	}
	if step(tracker.started, at.Add(loadReorderWindow/2)) {
		t.Fatal("the start of a load already stopped reads as loading")
	}
	at = at.Add(2 * loadReorderWindow)
	if !step(tracker.started, at) {
		t.Fatal("the next load reads as idle")
	}
}
