package browser

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/popupadmit"
)

// historyGatePage is an adopted popup's fake page whose history read, the
// adoption's last step before the listing, waits for its gate.
type historyGatePage struct {
	*handledPage
	reached chan<- string
	gate    <-chan struct{}
}

func (p historyGatePage) HistoryState(ctx context.Context) (bool, bool, error) {
	p.reached <- "history " + p.handle
	<-p.gate
	return p.handledPage.HistoryState(ctx)
}

// A close or crash an engine reports of a popup decides its tab at the
// listing, wherever it falls from the popup's report on: a popup that
// closed is no tab, live or crashed, whether it crashed too and in either
// order, and one that only crashed is a crashed tab, also when its attach
// then fails, and is never published live first. Reported after the
// listing, they act on the tab: a close removes it and a crash keeps it
// crashed. The reports go through the Manager's engine events, as every
// engine sends them, at each stage of the adoption, and every state push
// is read.
func TestAPopupsEndsDecideItsTab(t *testing.T) {
	type outcome int
	const (
		live outcome = iota
		crashed
		none
	)
	traces := []struct {
		name string
		ends []string
		// before is the outcome of the ends reported before the listing,
		// unattached of those reported before an attach that fails, and
		// after of those reported after the listing.
		before, unattached, after outcome
	}{
		{"nothing", nil, live, none, live},
		{"a close", []string{"close"}, none, none, none},
		{"a crash", []string{"crash"}, crashed, crashed, crashed},
		{"a crash, then a close", []string{"crash", "close"}, none, none, crashed},
		{"a close, then a crash", []string{"close", "crash"}, none, none, none},
	}
	stages := []string{"during its attach", "during an attach that fails", "during its history read", "after its listing"}
	for _, stage := range stages {
		for _, trace := range traces {
			t.Run(stage+"/"+trace.name, func(t *testing.T) {
				manager, engine, access, opener := newAdoptionManager(t)
				// The thread has the opener's tab and the popup's alone, so a
				// push with two live tabs published the popup live.
				var pushMu sync.Mutex
				publishedLive := false
				manager.SetEventSink(func(event CompanionEvent) {
					if event.Kind != "state" || event.ThreadID != access.ThreadID {
						return
					}
					live := 0
					for _, page := range event.Pages {
						if !page.Suspended {
							live++
						}
					}
					if live > 1 {
						pushMu.Lock()
						publishedLive = true
						pushMu.Unlock()
					}
				})
				report := func() {
					for _, end := range trace.ends {
						if end == "close" {
							manager.pageClosed("popup")
						} else {
							manager.pageCrashedReport("popup", "render-process-exited")
						}
					}
				}
				history := make(chan struct{})
				defer func() {
					select {
					case <-history:
					default:
						close(history)
					}
				}()
				want := trace.before
				switch stage {
				case "during its attach", "during an attach that fails":
					release := engine.gate("popup")
					defer release()
					if stage == "during an attach that fails" {
						engine.mu.Lock()
						engine.failing["popup"] = true
						engine.mu.Unlock()
						want = trace.unattached
					}
					manager.popupOpened(engine.popup(opener, "popup"))
					engine.awaitAttach(t, "popup")
					report()
					release()
				case "during its history read":
					engine.wrap = func(driver pageDriver) pageDriver {
						return historyGatePage{handledPage: driver.(*handledPage), reached: engine.reached, gate: history}
					}
					manager.popupOpened(engine.popup(opener, "popup"))
					engine.awaitAttach(t, "history popup")
					report()
					close(history)
				case "after its listing":
					manager.popupOpened(engine.popup(opener, "popup"))
					awaitAdmission(t, manager, "popup")
					if !listed(manager, "popup") {
						t.Fatal("the popup was not listed")
					}
					report()
					want = trace.after
				}
				awaitAdmission(t, manager, "popup")
				deadline := time.Now().Add(hostedTestDeadline)
				for {
					got, tabs := popupOutcome(manager, access, opener)
					if got == int(want) {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("the thread's tabs are %+v, want the popup's %v", tabs, []string{"live", "crashed", "none"}[want])
					}
					time.Sleep(time.Millisecond)
				}
				pushMu.Lock()
				wasLive := publishedLive
				pushMu.Unlock()
				if want != live && stage != "after its listing" && wasLive {
					t.Fatal("a state push listed the popup live before its tab was decided")
				}
				discards := engine.discards()
				if stage == "during an attach that fails" {
					for len(discards) == 0 && time.Now().Before(deadline) {
						time.Sleep(time.Millisecond)
						discards = engine.discards()
					}
					if !slices.Equal(discards, []string{"popup"}) {
						t.Fatalf("discarded %v: a popup with no driver is the engine's to close", discards)
					}
				} else if len(discards) != 0 {
					t.Fatalf("discarded %v: the attached popup's own driver closes it", discards)
				}
			})
		}
	}
}

// A child popup still being adopted keeps its parent's chain however many
// unrelated popups come and go: its parent closes, the child is never
// listed, and the download it finished meanwhile is listed, finished, in
// the tab that opened its parent, its file kept, though its engine named
// only its parent.
func TestAPendingPopupsChainOutlivesTurnover(t *testing.T) {
	manager, engine, _, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	manager.adoptPopup(engine.popup(opener, "parent"))
	if !listed(manager, "parent") {
		t.Fatal("the parent was not listed")
	}
	release := engine.gate("child")
	defer release()
	childDone := make(chan struct{})
	go func() { manager.adoptPopup(engine.popup("parent", "child")); close(childDone) }()
	engine.awaitAttach(t, "child")
	file := engineArtifact(t, manager, "dl-1")
	if !popupDownload(manager, engine, "child", "dl-1", "parent") {
		t.Fatal("the child's download was refused")
	}
	manager.downloadProgress(progressOf("dl-1", downloadCompleted, 5))
	// Twice the registry's history of popups that came and went.
	for i := range 2*orderSlack + 1 {
		handle := fmt.Sprintf("other-%d", i)
		engine.mu.Lock()
		engine.failing[handle] = true
		engine.mu.Unlock()
		manager.adoptPopup(engine.popup(opener, handle))
	}
	manager.removeClosedPage("parent")
	release()
	<-childDone
	if listed(manager, "child") {
		t.Fatal("a popup whose opener closed was listed")
	}
	wantDownloads(t, manager, opener, "dl-1 completed 5")
	manager.mu.Lock()
	root := manager.listedPageLocked(opener)
	manager.mu.Unlock()
	root.downloadMu.Lock()
	path := root.downloads[0].Path
	root.downloadMu.Unlock()
	if path == "" || path == file {
		t.Fatalf("the finished download's file is %q, want it published from %s", path, file)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the finished download's file is gone: %v", err)
	}
}

// awaitAdmission waits until the admission of popup handle is decided:
// listed, with its tab in the same hold, or ended.
func awaitAdmission(t *testing.T, m *Manager, handle string) {
	t.Helper()
	deadline := time.Now().Add(hostedTestDeadline)
	for {
		if d, ok := m.admissions.Disposition(popupadmit.Handle(handle)); ok && d != popupadmit.Pending {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the admission of %s was never decided", handle)
		}
		time.Sleep(time.Millisecond)
	}
}

// popupOutcome answers what the thread shows of the popup next to its
// opener's tab: 0 a live tab, 1 a crashed tab, 2 none, -1 anything else.
func popupOutcome(m *Manager, access Access, opener string) (int, []PageInfo) {
	pages := m.CompanionState(access).Pages
	m.mu.Lock()
	livePopup := m.listedPageLocked("popup") != nil
	m.mu.Unlock()
	switch {
	case len(pages) == 1:
		return 2, pages
	case len(pages) == 2 && livePopup && !pages[1].Suspended:
		return 0, pages
	case len(pages) == 2 && !livePopup && pages[1].Suspended && pages[1].Error != "":
		return 1, pages
	}
	return -1, pages
}

// A download of a popup whose engine names only the page that opened it
// is listed in the nearest tab still open up the chain of openers the
// popups' admissions keep: a popup of a listed popup that closed puts its
// download in the tab that opened its parent.
func TestANestedPopupsDownloadFindsItsNearestOpenAncestor(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	manager.adoptPopup(engine.popup(opener, "parent"))
	if !listed(manager, "parent") {
		t.Fatal("the parent was not listed")
	}
	release := engine.gate("child")
	defer release()
	childDone := make(chan struct{})
	go func() { manager.adoptPopup(engine.popup("parent", "child")); close(childDone) }()
	engine.awaitAttach(t, "child")
	// As a WebKit engine names it: by its immediate opener alone.
	if !popupDownload(manager, engine, "child", "dl-1", "parent") {
		t.Fatal("the child's download was refused")
	}
	manager.removeClosedPage("parent")
	release()
	<-childDone
	if listed(manager, "child") {
		t.Fatal("a popup whose opener closed was listed")
	}
	wantDownloads(t, manager, opener, "dl-1 in_progress 0")
	if got := engine.cancels(); len(got) != 0 {
		t.Fatalf("canceled %v", got)
	}
}
