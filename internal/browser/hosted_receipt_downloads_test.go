package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/browser/popupadmit"
	"agent-overflow/internal/webview2host"
)

// Where a download the engine received lists, against every Manager action
// and every point the engine can process what it received at: a download
// belongs, from its receipt, to the page that will list it, and is listed
// there as its reports say whatever leaves first. Each row puts a page in
// a state, has the launcher report a download of it, holds the processing
// of what was received, lets the state end (the page's listing, or its
// popup's close), acts, and lets the processing go at the row's timing:
// before the listing, between the listing and the action, after the
// action, or, for a popup whose close was received, once its engine page
// retired and before the Manager settled it. It asserts the download's one
// settlement: the thread whose history holds it, its state, its cause, its
// bytes received and total, whether it is openable, and whether a file
// action finds its file.

// receiptDownloadName is the downloaded file's name: a type a tab shows,
// so a finished one is openable.
const receiptDownloadName = "a.txt"

// receiptPage is the page whose download a row reports: page is the engine
// page the reports name; list ends its state once they were received,
// answering the tab the actions name; abandon, set for a page being created
// or restored, closes its tab before its listing instead and lets the
// creation or restore end; cause, set for a popup whose close was
// received, is the reason a running download of it ends with, the close's.
type receiptPage struct {
	page    string
	list    func(t *testing.T) (tab string)
	abandon func(t *testing.T)
	cause   string
}

// receiptState puts a fresh launcher's page in a state; listing is set
// for a page being created or restored, whose listing a close can come
// before; timings are the points its row's processing goes at.
type receiptState struct {
	name    string
	listing bool
	timings []receiptTiming
	open    func(t *testing.T, l *popupLauncher, access Access) *receiptPage
}

// receiptTiming is when a row's held processing goes, against the state's
// end (its listing) and the action.
type receiptTiming int

const (
	// processedBeforeListing: before the page's listing, or before the
	// close of a tab being created or restored.
	processedBeforeListing receiptTiming = iota + 1
	// processedBeforeAction: after the listing, before the action.
	processedBeforeAction
	// processedAfterAction: after the action.
	processedAfterAction
	// processedBeforeSettling: a popup whose close was received retires in
	// its engine before the action, and the Manager settles it after.
	processedBeforeSettling
)

func (timing receiptTiming) name(listing bool) string {
	switch timing {
	case processedBeforeListing:
		return "processed before its listing"
	case processedBeforeAction:
		if listing {
			return "processed between its listing and the action"
		}
		return "processed before the action"
	case processedAfterAction:
		return "processed after the action"
	case processedBeforeSettling:
		return "the popup retired before the action and settled after it"
	}
	panic(fmt.Sprintf("receipt timing %d", int(timing)))
}

var (
	listedTimings  = []receiptTiming{processedBeforeAction, processedAfterAction}
	listingTimings = []receiptTiming{processedBeforeListing, processedBeforeAction, processedAfterAction}
	closedTimings  = []receiptTiming{processedAfterAction, processedBeforeSettling}
)

// awaitRetired waits until the engine has no record of page.
func awaitRetired(t *testing.T, engine *hostedEngine, page string) {
	t.Helper()
	for deadline := time.Now().Add(hostedTestDeadline); ; time.Sleep(time.Millisecond) {
		engine.pages.mu.Lock()
		_, kept := engine.pages.records[ids.PageHandle(page)]
		engine.pages.mu.Unlock()
		if !kept {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the engine never retired page %s", page)
		}
	}
}

// awaitCreated answers the engine page id of the next page created.
func awaitCreated(t *testing.T, l *popupLauncher) <-chan string {
	t.Helper()
	created := make(chan string, 1)
	send := l.engine.send
	l.engine.send = func(directive webview2host.Directive) {
		if directive.Op == webview2host.OpCreate {
			select {
			case created <- directive.PageID:
			default:
			}
		}
		send(directive)
	}
	return created
}

func receiveOn(t *testing.T, ch <-chan string, what string) string {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(hostedTestDeadline):
		t.Fatalf("%s never happened", what)
		return ""
	}
}

// loaded has the fake endpoint report that page's navigation loaded, as
// the navigation's wait reads it.
func loaded(l *popupLauncher, page string) {
	session, frame := "S-T-"+page, "T-"+page
	l.endpoint.send(map[string]any{"method": "Page.lifecycleEvent", "sessionId": session, "params": map[string]any{"frameId": frame, "loaderId": "L-2", "name": "init", "timestamp": 1}})
	l.endpoint.send(map[string]any{"method": "Page.loadEventFired", "sessionId": session, "params": map[string]any{"timestamp": 2}})
}

// awaitAnswer waits for a call run in the background.
func awaitAnswer(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(hostedTestDeadline):
		t.Fatalf("%s never answered", what)
		return nil
	}
}

var receiptStates = []receiptState{
	{"a listed tab", false, listedTimings, func(t *testing.T, l *popupLauncher, access Access) *receiptPage {
		tab, page := l.open(access)
		return &receiptPage{page: page, list: func(*testing.T) string { return tab }}
	}},
	{"a tab being created", true, listingTimings, func(t *testing.T, l *popupLauncher, access Access) *receiptPage {
		release := l.gateMethod("Emulation.setDeviceMetricsOverride")
		t.Cleanup(release)
		created := awaitCreated(t, l)
		opened := make(chan string, 1)
		done := make(chan error, 1)
		go func() {
			info, err := l.manager.NewCompanionPage(context.Background(), access, "")
			opened <- info.ID
			done <- err
		}()
		page := receiveOn(t, created, "the create")
		l.awaitAttach("Emulation.setDeviceMetricsOverride")
		return &receiptPage{
			page: page,
			list: func(t *testing.T) string {
				release()
				tab := receiveOn(t, opened, "the creation")
				if err := awaitAnswer(t, done, "the creation"); err != nil || tab == "" {
					t.Fatalf("the creation answered %q, %v", tab, err)
				}
				return tab
			},
			abandon: func(t *testing.T) {
				var tab string
				for _, p := range l.manager.CompanionState(access.Caller()).Pages {
					if p.Creating {
						tab = p.ID
					}
				}
				if err := l.manager.CloseCompanionPage(t.Context(), access, PageID(tab)); err != nil {
					t.Fatalf("close the tab being created: %v", err)
				}
				release()
				opened := receiveOn(t, opened, "the creation")
				if err := awaitAnswer(t, done, "the creation"); err == nil && opened != "" {
					t.Fatalf("a creation whose tab closed opened tab %s", opened)
				}
			},
		}
	}},
	{"a tab being restored, its navigation in flight", true, listingTimings, func(t *testing.T, l *popupLauncher, access Access) *receiptPage {
		tab, page := l.open(access)
		l.open(access)
		p := l.tabs(string(access.ThreadID()))[ids.PageHandle(page)]
		if suspended, err := l.manager.suspendPage(t.Context(), p); err != nil || !suspended {
			t.Fatalf("suspend the tab: %v, %v", suspended, err)
		}
		l.manager.mu.Lock()
		rec := l.manager.suspended[PageID(tab)]
		rec.url = "https://example.test/restore"
		l.manager.mu.Unlock()
		release := l.gateMethod("Page.navigate")
		t.Cleanup(release)
		created := awaitCreated(t, l)
		done := make(chan error, 1)
		go func() { done <- l.manager.restorePage(context.Background(), access, rec, false) }()
		restoring := receiveOn(t, created, "the restore's create")
		l.awaitAttach("Page.navigate")
		return &receiptPage{
			page: restoring,
			list: func(t *testing.T) string {
				release()
				loaded(l, restoring)
				if err := awaitAnswer(t, done, "the restore"); err != nil {
					t.Fatalf("the restore answered %v", err)
				}
				return tab
			},
			abandon: func(t *testing.T) {
				if err := l.manager.CloseCompanionPage(t.Context(), access, PageID(tab)); err != nil {
					t.Fatalf("close the tab being restored: %v", err)
				}
				release()
				loaded(l, restoring)
				if err := awaitAnswer(t, done, "the restore"); err == nil {
					t.Fatal("a restore whose tab closed succeeded")
				}
			},
		}
	}},
	{"a popup being adopted", false, listingTimings, func(t *testing.T, l *popupLauncher, access Access) *receiptPage {
		_, opener := l.open(access)
		release := l.gateHistory("PT-1")
		t.Cleanup(release)
		popup, _ := l.popup(opener, "https://example.test/popup")
		l.awaitAttach("history PT-1")
		return &receiptPage{page: popup, list: func(t *testing.T) string {
			release()
			return string(l.awaitTab(string(access.ThreadID()), popup, true).id)
		}}
	}},
	{"a popup with no target, its close received while its opener is listed", false, closedTimings, func(t *testing.T, l *popupLauncher, access Access) *receiptPage {
		opener, page := l.open(access)
		const popup = "popup-untargeted"
		l.reportPopup(popup, webview2host.PopupReport{Opener: page, URL: "https://example.test/popup"})
		return &receiptPage{page: popup, cause: pageClosedReason, list: func(t *testing.T) string {
			l.report(popup, webview2host.ReportClosed, "")
			return opener
		}}
	}},
	{"a popup being adopted, its close received while its opener is listed", false, closedTimings, func(t *testing.T, l *popupLauncher, access Access) *receiptPage {
		opener, page := l.open(access)
		release := l.gateHistory("PT-1")
		t.Cleanup(release)
		popup, _ := l.popup(page, "https://example.test/popup")
		l.awaitAttach("history PT-1")
		return &receiptPage{page: popup, cause: pageClosedReason, list: func(t *testing.T) string {
			l.report(popup, webview2host.ReportClosed, "")
			release()
			deadline := time.Now().Add(hostedTestDeadline)
			for {
				if d, _ := l.manager.admissions.Disposition(ids.PageHandle(popup)); d != popupadmit.Pending {
					return opener
				}
				if time.Now().After(deadline) {
					t.Fatal("the popup's adoption never ended")
				}
				time.Sleep(time.Millisecond)
			}
		}}
	}},
}

// receiptKind is what the launcher reported of the download: before, what
// the engine processed before the hold, and held, what it received and
// holds; want is the download's settlement once its holder left for
// reason, by Stop when stopped.
type receiptKind struct {
	name         string
	before, held []webview2host.DownloadReport
	want         func(reason string, stopped bool) settledDownload
}

// settledDownload is a download's one settlement, as its thread shows it.
type settledDownload struct {
	State           string
	Cause           string
	Received, Total int64
	Openable        bool
	// Opens is whether a file action finds its file.
	Opens bool
}

func receiptReport(seq uint64, state string, received int64) webview2host.DownloadReport {
	return webview2host.DownloadReport{ID: "dl-1", Seq: seq, State: state, URI: "https://example.test/" + receiptDownloadName, Name: receiptDownloadName, Path: `C:\Users\dev\Downloads\` + receiptDownloadName, Received: received, Total: 10}
}

var receiptKinds = func() []receiptKind {
	started := receiptReport(1, webview2host.DownloadRunning, 1)
	progressed := receiptReport(2, webview2host.DownloadRunning, 6)
	completed := receiptReport(2, webview2host.DownloadCompleted, 10)
	failed := receiptReport(2, webview2host.DownloadInterrupted, 3)
	failed.End, failed.Reason = webview2host.DownloadEndFailed, "the network failed"
	canceled := func(received int64) func(string, bool) settledDownload {
		return func(reason string, _ bool) settledDownload {
			return settledDownload{State: downloadStateCanceled, Cause: reason, Received: received, Total: 10}
		}
	}
	return []receiptKind{
		{"running", nil, []webview2host.DownloadReport{started}, canceled(1)},
		{"running, newer counts", []webview2host.DownloadReport{started}, []webview2host.DownloadReport{progressed}, canceled(6)},
		{"completed", nil, []webview2host.DownloadReport{started, completed}, func(_ string, stopped bool) settledDownload {
			if stopped {
				return settledDownload{State: downloadStateCompleted, Cause: `the file was saved to C:\Users\dev\Downloads\` + receiptDownloadName + "; the browser closed before this backend located it, so it is not opened from here", Received: 10, Total: 10}
			}
			return settledDownload{State: downloadStateCompleted, Received: 10, Total: 10, Openable: true, Opens: true}
		}},
		{"failed", nil, []webview2host.DownloadReport{started, failed}, func(string, bool) settledDownload {
			return settledDownload{State: downloadStateFailed, Cause: "the network failed", Received: 3, Total: 10}
		}},
		{"lost by the launcher", []webview2host.DownloadReport{started}, []webview2host.DownloadReport{unknownAnswer("dl-1")}, func(string, bool) settledDownload {
			return settledDownload{State: downloadStateFailed, Cause: webview2host.DownloadUnknownReason, Received: 1, Total: 10}
		}},
	}
}()

// receiptAction is the Manager's action on a row's page once its state
// ended: abandons is set for the action that closes a tab being created or
// restored before its listing; tab is the tab list answered.
type receiptAction struct {
	name     string
	reason   string
	stops    bool
	forgets  bool
	abandons bool
	act      func(t *testing.T, l *popupLauncher, access Access, tab string)
}

var receiptActions = []receiptAction{
	{name: "its tab closed", reason: "page closed", act: func(t *testing.T, l *popupLauncher, access Access, tab string) {
		if err := l.manager.CloseCompanionPage(t.Context(), access, PageID(tab)); err != nil {
			t.Fatalf("close the tab: %v", err)
		}
	}},
	{name: "its thread closed", reason: "page closed", forgets: true, act: func(t *testing.T, l *popupLauncher, access Access, _ string) {
		if err := l.manager.CloseThread(t.Context(), access.ThreadID()); err != nil {
			t.Fatalf("close the thread: %v", err)
		}
	}},
	{name: "its thread's jar switched", reason: "page closed", act: func(t *testing.T, l *popupLauncher, access Access, _ string) {
		if err := l.manager.SetJar(t.Context(), access, JarPrivate, JarSwitchConfirmed); err != nil {
			t.Fatalf("switch the jar: %v", err)
		}
	}},
	{name: "the browser closed", reason: "the browser closed", stops: true, act: func(t *testing.T, l *popupLauncher, _ Access, _ string) {
		if err := l.manager.Close(); err != nil {
			t.Fatalf("close the browser: %v", err)
		}
	}},
	{name: "its tab closed before its listing", reason: "page closed", abandons: true},
}

// announcements records the settlements the Manager announces, by name.
type announcements struct {
	mu   sync.Mutex
	seen map[string][]ThreadDownload
}

func (a *announcements) sink(event CompanionEvent) {
	if event.Kind != "download" || event.Download == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen[event.Download.Name] = append(a.seen[event.Download.Name], *event.Download)
}

func (a *announcements) await(t *testing.T, name string) []ThreadDownload {
	t.Helper()
	deadline := time.Now().Add(hostedTestDeadline)
	for {
		a.mu.Lock()
		got := append([]ThreadDownload(nil), a.seen[name]...)
		a.mu.Unlock()
		if len(got) > 0 || time.Now().After(deadline) {
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

// holderRow waits until holder's log lists download id settled, and
// answers it.
func holderRow(t *testing.T, holder *managedPage, id string) DownloadInfo {
	t.Helper()
	deadline := time.Now().Add(hostedTestDeadline)
	for {
		var listed []DownloadInfo
		for _, row := range holder.downloads.list() {
			listed = append(listed, row)
			if row.ID == id && row.State != downloadStateRunning {
				return row
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the holder's log lists %+v, not download %s settled", listed, id)
		}
		time.Sleep(time.Millisecond)
	}
}

// tabPage answers the page of the thread's live tab with id tab.
func (l *popupLauncher) tabPage(access Access, tab string) *managedPage {
	l.t.Helper()
	for _, p := range l.tabs(string(access.ThreadID())) {
		if string(p.id) == tab {
			return p
		}
	}
	l.t.Fatalf("no live tab %s", tab)
	return nil
}

// The cross of the page's state at receipt, what was received, the
// Manager's action and when the engine processes what it received: 6
// states, 5 report kinds, 4 actions, with a fifth, the close before the
// listing, for the 2 states that have one, and each state's timings: a
// listed tab's processing before or after the action; a tab being created
// or restored, or a popup being adopted, also before its listing (only
// before or after a close before the listing); a popup whose close was
// received, its engine page held until after the action, or retired
// before it and settled by the Manager after (320 rows). A popup never
// listed whose close was not received has no row: its fate, not a
// receipt, decides where its downloads list, and every action here takes
// away the opener its fate would name.
func TestEveryReceivedDownloadIsListedByItsHolder(t *testing.T) {
	for _, state := range receiptStates {
		for _, kind := range receiptKinds {
			for _, action := range receiptActions {
				if action.abandons && !state.listing {
					continue
				}
				for _, timing := range state.timings {
					if action.abandons && timing == processedBeforeAction {
						continue
					}
					receiptRow(t, state, kind, action, timing)
				}
			}
		}
	}
}

// receiptRow runs one row of TestEveryReceivedDownloadIsListedByItsHolder.
func receiptRow(t *testing.T, state receiptState, kind receiptKind, action receiptAction, timing receiptTiming) {
	t.Helper()
	t.Run(fmt.Sprintf("%s, %s, %s, %s", state.name, kind.name, action.name, timing.name(slices.Contains(state.timings, processedBeforeListing))), func(t *testing.T) {
		l := newPopupLauncher(t)
		saved := t.TempDir()
		if err := os.WriteFile(filepath.Join(saved, receiptDownloadName), []byte("0123456789"), 0o600); err != nil {
			t.Fatal(err)
		}
		l.engine.backendPath = func(_ context.Context, path string) (string, error) {
			return filepath.Join(saved, windowsBase(path)), nil
		}
		announced := &announcements{seen: map[string][]ThreadDownload{}}
		l.manager.SetEventSink(announced.sink)
		access := l.access("thread")
		p := state.open(t, l, access)
		for _, report := range kind.before {
			reportDownload(t, l.engine, p.page, report)
		}
		reached, release := l.holdLane(p.page)
		for _, report := range kind.held {
			sendDownload(t, l.engine, p.page, report)
		}
		awaitClosed(t, reached, "the held report")
		process := func() {
			release()
			settleReports(t, l.engine, "")
		}
		var holder *managedPage
		if action.abandons {
			l.manager.mu.Lock()
			holder = l.manager.creatingPageByHandleLocked(ids.PageHandle(p.page))
			l.manager.mu.Unlock()
			if timing == processedBeforeListing {
				process()
			}
			p.abandon(t)
		} else {
			if timing == processedBeforeListing {
				process()
			}
			holder = l.tabPage(access, p.list(t))
			settle := func() {}
			switch timing {
			case processedBeforeListing, processedAfterAction:
			case processedBeforeAction:
				process()
			case processedBeforeSettling:
				gate := make(chan struct{})
				l.manager.pageEnds.report(l.manager.goLife, ids.PageHandle(p.page), func() bool { return true }, func() { <-gate })
				settle = sync.OnceFunc(func() { close(gate) })
				t.Cleanup(settle)
				if action.stops {
					// The close joins the page's queued end, which runs
					// once the browser has stopped.
					go func() {
						awaitLifeClosed(l.manager)
						settle()
					}()
				}
				process()
				awaitRetired(t, l.engine, p.page)
			}
			action.act(t, l, access, string(holder.id))
			settle()
		}
		release()
		settleReports(t, l.engine, "")

		want := kind.want(action.reason, action.stops && timing == processedAfterAction)
		if p.cause != "" && want.State == downloadStateCanceled {
			want.Cause = p.cause
		}
		row := holderRow(t, holder, "dl-1")
		got := settledDownload{State: row.State, Cause: row.Error, Received: row.Bytes, Total: row.TotalBytes, Openable: want.Openable, Opens: want.Opens}
		if got != want {
			t.Fatalf("its holder lists the download as %+v, want %+v", got, want)
		}
		if live := l.engineDownloads(string(access.ThreadID())); len(live) != 0 {
			t.Fatalf("a live tab lists %v, which its holder took", live)
		}
		l.manager.mu.Lock()
		pending := len(l.manager.endPending)
		l.manager.mu.Unlock()
		if pending != 0 {
			t.Fatalf("the Manager still waits for %d ends", pending)
		}
		if action.forgets {
			// The thread's history goes with it.
			if history := l.manager.CompanionState(access.Caller()).Downloads; len(history) != 0 {
				t.Fatalf("the closed thread's history kept %+v", history)
			}
			return
		}
		settled := announced.await(t, receiptDownloadName)
		if len(settled) != 1 {
			t.Fatalf("the download was announced settled %d times: %+v", len(settled), settled)
		}
		got = settledDownload{State: settled[0].State, Cause: settled[0].Error, Received: settled[0].ReceivedBytes, Total: settled[0].TotalBytes, Openable: settled[0].Openable}
		_, opened := l.manager.finishedDownloadPath(l.manager.Bind(access.Caller()), DownloadKey(settled[0].ID))
		got.Opens = opened == nil
		if got != want {
			t.Fatalf("the thread's settlement is %+v, want %+v", got, want)
		}
		if history := l.manager.CompanionState(access.Caller()).Downloads; len(history) != 1 || history[0] != settled[0] {
			t.Fatalf("the thread's history is %+v, want the settlement %+v", history, settled[0])
		}
	})
}

// awaitLifeClosed waits, within hostedTestDeadline, until the Manager's
// close has stopped the browser and begun waiting for the work it joins.
func awaitLifeClosed(m *Manager) {
	for deadline := time.Now().Add(hostedTestDeadline); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		m.lifeGate.Lock()
		closed := m.lifeClosed
		m.lifeGate.Unlock()
		if closed {
			return
		}
	}
}
