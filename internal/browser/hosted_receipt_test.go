package browser

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/browser/popupadmit"
	"agent-overflow/internal/webview2host"
)

// What a launcher report records at receipt (hostedPages.receive), against
// every Manager action that can come between the report's acknowledgment
// and its processing on the page's lane: a received download is listed in
// its tab whatever leaves the tab, with the end received for it; a received
// close or crash decides the listing of a page being created or of a
// pending popup; and an end applied through a lookup that found its tab
// listed still clears what the tab's leave waits for. Each row holds the
// report's processing after its acknowledgment, acts, then lets the
// processing go. A pair missing here has no record to read: a listed
// tab's close or crash and the Manager's own close of it both remove the
// tab, and a popup's or a created page's download has no tab until its
// listing.

// receivedScene is a page whose reports were received and acknowledged,
// their processing held until release.
type receivedScene struct {
	l      *popupLauncher
	access Access
	// tab is the tab's id and page the engine page the reports name.
	tab, page string
	release   func()
	// list lets the held listing of the page commit and answers whether
	// the page was a live tab once it had.
	list func() (live bool)
}

// holdLane holds the processing of every report of page the engine
// received from now until release, and closes reached as the first is
// held. Stop ends the hold, the held report unprocessed, as it ends a path
// conversion under way. Call it while no lane runs.
func (l *popupLauncher) holdLane(page string) (reached <-chan struct{}, release func()) {
	in, out := make(chan struct{}), make(chan struct{})
	var enter, leave sync.Once
	process := l.engine.pages.process
	l.engine.pages.process = func(ctx context.Context, w *acceptedWork) {
		if w.page == ids.PageHandle(page) {
			enter.Do(func() { close(in) })
			select {
			case <-out:
			case <-ctx.Done():
				return
			}
		}
		process(ctx, w)
	}
	release = func() { leave.Do(func() { close(out) }) }
	l.t.Cleanup(release)
	return in, release
}

func runningAt(id, name string, received int64) webview2host.DownloadReport {
	return webview2host.DownloadReport{ID: id, Seq: 1, State: webview2host.DownloadRunning, URI: "https://example.test/" + name, Name: name, Path: `C:\Users\dev\Downloads\` + name, Received: received, Total: 10}
}

func finishedAt(id, name string) webview2host.DownloadReport {
	completed := completedAt(id, name)
	completed.Seq = 2
	return completed
}

// openHeld opens a tab and holds its lane.
func openHeld(t *testing.T) (*receivedScene, <-chan struct{}) {
	l := newPopupLauncher(t)
	access := l.access("thread")
	tab, page := l.open(access)
	reached, release := l.holdLane(page)
	return &receivedScene{l: l, access: access, tab: tab, page: page, release: release}, reached
}

// A running download received and not admitted.
func receivedRunning(t *testing.T) *receivedScene {
	s, reached := openHeld(t)
	sendDownload(t, s.l.engine, s.page, runningAt("dl-1", "a.bin", 1))
	awaitClosed(t, reached, "the held report")
	return s
}

// A download whose running and completed snapshots were both received,
// neither admitted.
func receivedFinished(t *testing.T) *receivedScene {
	s, reached := openHeld(t)
	sendDownload(t, s.l.engine, s.page, runningAt("dl-1", "a.bin", 1))
	sendDownload(t, s.l.engine, s.page, finishedAt("dl-1", "a.bin"))
	awaitClosed(t, reached, "the held report")
	return s
}

// A listed download's completion in its path conversion, and a second
// download's running and completed snapshots received behind it.
func receivedBehindAConversion(t *testing.T) *receivedScene {
	l := newPopupLauncher(t)
	access := l.access("thread")
	tab, page := l.open(access)
	sendDownload(t, l.engine, page, runningAt("dl-1", "a.bin", 1))
	l.awaitDownloads(access, downloadStateRunning)
	entered, unblock := blockPath(t, l.engine, "a.bin")
	sendDownload(t, l.engine, page, finishedAt("dl-1", "a.bin"))
	awaitClosed(t, entered, "the first completion's path conversion")
	sendDownload(t, l.engine, page, runningAt("dl-2", "b.bin", 1))
	sendDownload(t, l.engine, page, finishedAt("dl-2", "b.bin"))
	return &receivedScene{l: l, access: access, tab: tab, page: page, release: unblock}
}

// A download whose running and completed snapshots were both received,
// its admission held in the Manager after it found the tab.
func receivedWhileAdmitting(t *testing.T) *receivedScene {
	l := newPopupLauncher(t)
	access := l.access("thread")
	tab, page := l.open(access)
	admitting, resume := make(chan struct{}), make(chan struct{})
	var enter, leave sync.Once
	l.manager.downloadAdmitting = func() {
		enter.Do(func() { close(admitting) })
		<-resume
	}
	release := func() { leave.Do(func() { close(resume) }) }
	t.Cleanup(release)
	sendDownload(t, l.engine, page, runningAt("dl-1", "a.bin", 1))
	sendDownload(t, l.engine, page, finishedAt("dl-1", "a.bin"))
	awaitClosed(t, admitting, "the admission")
	return &receivedScene{l: l, access: access, tab: tab, page: page, release: release}
}

// A listed download's completion that the Manager's application holds
// between its lookup, which found the tab listed, and its row.
func receivedEndBeingApplied(t *testing.T) *receivedScene {
	l := newPopupLauncher(t)
	access := l.access("thread")
	tab, page := l.open(access)
	sendDownload(t, l.engine, page, runningAt("dl-1", "a.bin", 1))
	l.awaitDownloads(access, downloadStateRunning)
	settleReports(t, l.engine, page)
	looked, resume := make(chan struct{}), make(chan struct{})
	progress := l.engine.events.DownloadProgress
	l.engine.events.DownloadProgress = func(event downloadProgress) {
		if event.State != downloadCompleted {
			progress(event)
			return
		}
		p, scope := l.manager.pageForDownload(event.ID)
		close(looked)
		<-resume
		if p != nil {
			l.manager.applyToRow(p, scope, event)
		}
	}
	sendDownload(t, l.engine, page, finishedAt("dl-1", "a.bin"))
	awaitClosed(t, looked, "the completion's lookup")
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(release)
	return &receivedScene{l: l, access: access, tab: tab, page: page, release: release}
}

// receivedWhileCreated is a page being created whose creation is held after
// the Manager attached it and before its listing, with end received for it
// and its processing held.
func receivedWhileCreated(end func(l *popupLauncher, page string)) func(t *testing.T) *receivedScene {
	return func(t *testing.T) *receivedScene {
		l := newPopupLauncher(t)
		access := l.access("thread")
		releaseCreation := l.gateMethod("Emulation.setDeviceMetricsOverride")
		t.Cleanup(releaseCreation)
		created := make(chan string, 1)
		send := l.engine.send
		l.engine.send = func(directive webview2host.Directive) {
			if directive.Op == webview2host.OpCreate {
				created <- directive.PageID
			}
			send(directive)
		}
		live := make(chan bool, 1)
		go func() {
			info, _ := l.manager.NewCompanionPage(context.Background(), access, "")
			_, listed := l.tabs(string(access.ThreadID()))[ids.PageHandle(paneHandleOrEmpty(l.manager, access, info.ID))]
			live <- info.ID != "" && listed
		}()
		var page string
		select {
		case page = <-created:
		case <-time.After(hostedTestDeadline):
			t.Fatal("no page was created")
		}
		l.awaitAttach("Emulation.setDeviceMetricsOverride")
		reached, release := l.holdLane(page)
		end(l, page)
		awaitClosed(t, reached, "the held end")
		return &receivedScene{l: l, access: access, page: page, release: release, list: func() bool {
			releaseCreation()
			select {
			case got := <-live:
				return got
			case <-time.After(hostedTestDeadline):
				t.Fatal("the creation never answered")
				return false
			}
		}}
	}
}

// receivedWhilePending is a popup the Manager is adopting, held after its
// attach and before its listing, with end received for it and its
// processing held.
func receivedWhilePending(end func(l *popupLauncher, page string)) func(t *testing.T) *receivedScene {
	return func(t *testing.T) *receivedScene {
		l := newPopupLauncher(t)
		access := l.access("thread")
		_, opener := l.open(access)
		releaseListing := l.gateHistory("PT-1")
		t.Cleanup(releaseListing)
		popup, _ := l.popup(opener, "https://example.test/popup")
		l.awaitAttach("history PT-1")
		reached, release := l.holdLane(popup)
		end(l, popup)
		awaitClosed(t, reached, "the held end")
		return &receivedScene{l: l, access: access, page: popup, release: release, list: func() bool {
			releaseListing()
			deadline := time.Now().Add(hostedTestDeadline)
			for {
				if d, _ := l.manager.admissions.Disposition(ids.PageHandle(popup)); d != popupadmit.Pending {
					_, listed := l.tabs(string(access.ThreadID()))[ids.PageHandle(popup)]
					return listed
				}
				if time.Now().After(deadline) {
					t.Fatal("the popup's listing never committed")
				}
				time.Sleep(time.Millisecond)
			}
		}}
	}
}

func reportClose(l *popupLauncher, page string) { l.report(page, webview2host.ReportClosed, "") }

func reportCrash(l *popupLauncher, page string) {
	l.report(page, webview2host.ReportProcessFailed, "render-process-exited")
}

// paneHandleOrEmpty is paneHandle for a tab that may not exist.
func paneHandleOrEmpty(m *Manager, access Access, tab string) string {
	if tab == "" {
		return ""
	}
	for _, p := range m.ownedPages(lifetimeOf(m, string(access.ThreadID()))) {
		if string(p.id) == tab {
			return string(p.driver.Handle())
		}
	}
	return ""
}

func closeHeldTab(t *testing.T, s *receivedScene) {
	if err := s.l.manager.CloseCompanionPage(t.Context(), s.access, PageID(s.tab)); err != nil {
		t.Fatalf("close the tab: %v", err)
	}
}

func suspendHeldTab(t *testing.T, s *receivedScene) {
	p := s.l.tabs(string(s.access.ThreadID()))[ids.PageHandle(s.page)]
	if p == nil {
		t.Fatal("the tab is not live")
	}
	if suspended, err := s.l.manager.suspendPage(t.Context(), p); err != nil || !suspended {
		t.Fatalf("suspend the tab: %v, %v", suspended, err)
	}
}

func closeHeldBrowser(t *testing.T, s *receivedScene) {
	if err := s.l.manager.Close(); err != nil {
		t.Fatalf("close the browser: %v", err)
	}
}

// receiptResult is what a row's thread shows once everything settled:
// its downloads' states by name, and its tabs, each live, suspended or
// crashed.
type receiptResult struct {
	downloads map[string]string
	tabs      []string
	// live is whether the held listing listed the page live.
	live bool
}

func (s *receivedScene) result() receiptResult {
	state := s.l.manager.CompanionState(s.access.Caller())
	got := receiptResult{downloads: map[string]string{}}
	for _, d := range state.Downloads {
		got.downloads[d.Name] = d.State
	}
	for _, p := range state.Pages {
		switch {
		case p.Suspended && p.Error != "":
			got.tabs = append(got.tabs, "crashed")
		case p.Suspended:
			got.tabs = append(got.tabs, "suspended")
		default:
			got.tabs = append(got.tabs, "live")
		}
	}
	return got
}

func TestEveryManagerActionFindsWhatTheEngineReceived(t *testing.T) {
	completedA := map[string]string{"a.bin": downloadStateCompleted}
	canceledA := map[string]string{"a.bin": downloadStateCanceled}
	for _, tc := range []struct {
		name    string
		receive func(t *testing.T) *receivedScene
		// act is the Manager's action meanwhile; nil lists the page.
		act  func(t *testing.T, s *receivedScene)
		want receiptResult
	}{
		{"a running download, its tab closed", receivedRunning, closeHeldTab, receiptResult{downloads: canceledA}},
		{"a running download, its tab suspended", receivedRunning, suspendHeldTab, receiptResult{downloads: canceledA, tabs: []string{"suspended"}}},
		{"a running download, the browser closed", receivedRunning, closeHeldBrowser, receiptResult{downloads: canceledA, tabs: []string{"suspended"}}},
		{"a finished download, its tab closed", receivedFinished, closeHeldTab, receiptResult{downloads: completedA}},
		{"a finished download, its tab suspended", receivedFinished, suspendHeldTab, receiptResult{downloads: completedA, tabs: []string{"suspended"}}},
		{"a finished download, the browser closed", receivedFinished, closeHeldBrowser, receiptResult{downloads: completedA, tabs: []string{"suspended"}}},
		{"a finished download being admitted, its tab closed", receivedWhileAdmitting, closeHeldTab, receiptResult{downloads: completedA}},
		{"a finished download behind another's conversion, its tab closed", receivedBehindAConversion, closeHeldTab,
			receiptResult{downloads: map[string]string{"a.bin": downloadStateCompleted, "b.bin": downloadStateCompleted}}},
		{"a finished download whose end is being applied, its tab closed", receivedEndBeingApplied, closeHeldTab, receiptResult{downloads: completedA}},
		{"a close of a page being created, its creation listing", receivedWhileCreated(reportClose), nil, receiptResult{downloads: map[string]string{}}},
		{"a crash of a page being created, its creation listing", receivedWhileCreated(reportCrash), nil, receiptResult{downloads: map[string]string{}, tabs: []string{"crashed"}}},
		{"a close of a pending popup, its listing", receivedWhilePending(reportClose), nil, receiptResult{downloads: map[string]string{}, tabs: []string{"live"}}},
		{"a crash of a pending popup, its listing", receivedWhilePending(reportCrash), nil, receiptResult{downloads: map[string]string{}, tabs: []string{"live", "crashed"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.receive(t)
			var live bool
			if tc.act != nil {
				tc.act(t, s)
			} else {
				live = s.list()
			}
			s.release()
			settleReports(t, s.l.engine, "")
			got := s.result()
			got.live = live
			if !maps.Equal(got.downloads, tc.want.downloads) || !slices.Equal(got.tabs, tc.want.tabs) || got.live {
				t.Fatalf("the thread shows %+v, want %+v", got, tc.want)
			}
			s.l.manager.mu.Lock()
			pending := len(s.l.manager.endPending)
			s.l.manager.mu.Unlock()
			if pending != 0 {
				t.Fatalf("the Manager still waits for %d ends", pending)
			}
		})
	}
}

// Stop during a discard whose retirement is held in its last callback
// tells the discard's waiter it was interrupted, not that the page is
// gone, and the retirement's completion is its own: it comes only once
// that callback returned.
func TestStopInterruptsADiscardWithoutFinishingIt(t *testing.T) {
	engine, _, _ := newDownloadHostedEngine(t)
	bindTestPage(t, engine, "page1", "TARGET-1")
	reportDownload(t, engine, "page1", snapshot("dl-1", webview2host.DownloadRunning, 10))
	inGone, releaseGone := make(chan struct{}), make(chan struct{})
	progress := engine.events.DownloadProgress
	engine.events.DownloadProgress = func(event downloadProgress) {
		if event.State == downloadPageGone {
			close(inGone)
			<-releaseGone
		}
		progress(event)
	}
	discarded := make(chan error, 1)
	go func() { discarded <- engine.DiscardPage(context.Background(), "page1") }()
	awaitClosed(t, inGone, "the discard's page-gone callback")
	joined := engine.pages.receive("page1", receivedDiscard())
	if joined.outcome != receiptEnding {
		t.Fatalf("a second discard was %v, want it joined to the first", joined.outcome)
	}
	stopped := make(chan struct{})
	go func() {
		engine.Stop()
		close(stopped)
	}()
	select {
	case err := <-discarded:
		if !errors.Is(err, errDiscardInterrupted) {
			t.Fatalf("the discard Stop interrupted answered %v", err)
		}
	case <-time.After(hostedTestDeadline):
		t.Fatal("Stop did not end the discard's wait")
	}
	select {
	case <-joined.retired:
		t.Fatal("the retirement completed while its last callback was held")
	default:
	}
	close(releaseGone)
	awaitClosed(t, joined.retired, "the retirement's completion")
	awaitClosed(t, stopped, "Stop")
}

// A create the engine cannot send leaves no record of its page.
func TestARefusedCreateLeavesNoRecord(t *testing.T) {
	engine, _ := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	engine.send = nil
	for range 3 {
		if _, err := engine.createPage(t.Context(), &hostedProfile{engine: engine, id: "profile"}, pageHooks{}); err == nil {
			t.Fatal("a create through an unwired host succeeded")
		}
	}
	engine.pages.mu.Lock()
	records := len(engine.pages.records)
	engine.pages.mu.Unlock()
	if records != 0 {
		t.Fatalf("the refused creates left %d records", records)
	}
}
