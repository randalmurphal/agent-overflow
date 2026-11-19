package browser

import (
	"context"
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
// order, and one that only crashed is a crashed tab. Reported after the
// listing, they act on the tab: a close removes it and a crash keeps it
// crashed. The reports go through the Manager's engine events, as every
// engine sends them, at each stage of the adoption.
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
		// after of those reported after it.
		before, after outcome
	}{
		{"nothing", nil, live, live},
		{"a close", []string{"close"}, none, none},
		{"a crash", []string{"crash"}, crashed, crashed},
		{"a crash, then a close", []string{"crash", "close"}, none, crashed},
		{"a close, then a crash", []string{"close", "crash"}, none, none},
	}
	stages := []string{"before its report", "during its attach", "during its history read", "after its listing"}
	for _, stage := range stages {
		for _, trace := range traces {
			t.Run(stage+"/"+trace.name, func(t *testing.T) {
				manager, engine, access, opener := newAdoptionManager(t)
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
				switch stage {
				case "during its attach":
					release := engine.gate("popup")
					defer release()
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
				case "before its report":
					report()
					manager.popupOpened(engine.popup(opener, "popup"))
				case "after its listing":
					manager.popupOpened(engine.popup(opener, "popup"))
					awaitAdmission(t, manager, "popup")
					if !listed(manager, "popup") {
						t.Fatal("the popup was not listed")
					}
					report()
				}
				want := trace.before
				if stage == "after its listing" {
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
				if got := engine.discards(); len(got) != 0 {
					t.Fatalf("discarded %v: the attached popup's own driver closes it", got)
				}
			})
		}
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
