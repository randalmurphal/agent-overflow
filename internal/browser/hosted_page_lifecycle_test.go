package browser

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/popupadmit"
	"agent-overflow/internal/webview2host"
)

// A hosted page's report is received apart from its processing
// (hosted_pages.go): what is true at receipt is recorded at receipt, and
// the effects run on the page's lane in order. These are the reviewer's
// sequences against the real Manager and the fake launcher.

// blockPath holds the engine's conversion of every Windows path ending in
// name until release, and closes entered as the first one starts.
func blockPath(t *testing.T, engine *hostedEngine, name string) (entered <-chan struct{}, release func()) {
	t.Helper()
	convert := engine.backendPath
	in, out := make(chan struct{}), make(chan struct{})
	var enter, leave sync.Once
	engine.backendPath = func(ctx context.Context, path string) (string, error) {
		if strings.HasSuffix(path, name) {
			enter.Do(func() { close(in) })
			select {
			case <-out:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return convert(ctx, path)
	}
	release = func() { leave.Do(func() { close(out) }) }
	t.Cleanup(release)
	return in, release
}

// engineDownloads answers every listed download of the thread by engine
// id, with its state.
func (l *popupLauncher) engineDownloads(threadID string) map[string]string {
	out := map[string]string{}
	for _, p := range l.tabs(threadID) {
		for _, d := range p.downloads.list() {
			out[d.ID] = d.State
		}
	}
	return out
}

// completedAt is a download's completed first snapshot, saved as name.
func completedAt(id, name string) webview2host.DownloadReport {
	return webview2host.DownloadReport{ID: id, Seq: 1, State: webview2host.DownloadCompleted, URI: "https://example.test/" + name, Name: name, Path: `C:\Users\dev\Downloads\` + name, Received: 10, Total: 10}
}

func awaitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(hostedTestDeadline):
		t.Fatalf("%s never happened", what)
	}
}

// A pending popup's completed download the launcher reported, terminal
// first, before the Manager's discard of the popup is kept: the discard
// runs on the popup's lane after it, so the completion reaches the
// opener's tab with its file, and the discard's caller, whose deadline
// passes while the opener's conversion blocks the lane, is told the
// discard is pending.
func TestADiscardKeepsAPopupsReceivedCompletion(t *testing.T) {
	l := newPopupLauncher(t)
	access := popupTestAccess(t, "thread")
	_, opener := l.open(access)
	releaseAttach := l.gate("PT-1")
	defer releaseAttach()
	popupID, _ := l.popup(opener, "https://example.test/popup")
	l.awaitAttach("PT-1")
	entered, unblock := blockPath(t, l.engine, "opener.bin")

	sendDownload(t, l.engine, opener, completedAt("dl-opener", "opener.bin"))
	awaitClosed(t, entered, "the opener's path conversion")
	sendDownload(t, l.engine, popupID, completedAt("dl-popup", "popup.bin"))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := l.engine.DiscardPage(ctx, popupID); !errors.Is(err, errDiscardPending) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a discard behind a blocked lane answered %v, want it pending", err)
	}
	unblock()
	releaseAttach()
	l.awaitDownloads(access, downloadStateCompleted, downloadStateCompleted)
	if listed := l.engineDownloads(access.ThreadID); listed["dl-opener"] != downloadStateCompleted || listed["dl-popup"] != downloadStateCompleted {
		t.Fatalf("the thread lists %v, want both completions", listed)
	}
	settleReports(t, l.engine, "")
	if _, kept := l.engine.pages.state(hostedPageID(popupID)); kept {
		t.Fatal("the discarded popup's record stayed")
	}
}

// A discard of a page whose completion's path conversion runs returns at
// its caller's deadline, saying the discard is pending, and its lane
// finishes it after the completion: the completion is the download's one
// end, the page's record goes, and a second discard joins the first.
func TestADiscardBehindAConversionReturnsAtItsDeadlineAndFinishes(t *testing.T) {
	engine, sink, recorder := newDownloadHostedEngine(t)
	bindTestPage(t, engine, "page1", "TARGET-1")
	entered, unblock := blockPath(t, engine, "slow.bin")
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-1", Seq: 1, State: webview2host.DownloadRunning, Path: `C:\Users\dev\Downloads\slow.bin`, Received: 1, Total: 10})
	sendDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-1", Seq: 2, State: webview2host.DownloadCompleted, Path: `C:\Users\dev\Downloads\slow.bin`, Received: 10, Total: 10})
	awaitClosed(t, entered, "the completion's path conversion")
	sink.drain()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := engine.DiscardPage(ctx, "page1")
	if !errors.Is(err, errDiscardPending) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the discard answered %v, want it pending past its deadline", err)
	}
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("the discard returned %s after its 50ms deadline", waited)
	}
	second := make(chan error, 1)
	go func() { second <- engine.DiscardPage(context.Background(), "page1") }()
	select {
	case err := <-second:
		t.Fatalf("a second discard returned %v before the first finished", err)
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("the joined discard answered %v", err)
		}
	case <-time.After(hostedTestDeadline):
		t.Fatal("the joined discard never returned")
	}
	_, progress := recorder.take()
	if terminals(progress) != 1 || progress[len(progress)-1].State != downloadCompleted || progress[len(progress)-1].FilePath == "" {
		t.Fatalf("progress %+v, want the completion as the download's one end", progress)
	}
	if _, kept := engine.pages.state("page1"); kept {
		t.Fatal("the discarded page's record stayed")
	}
	closes := 0
	for _, d := range sink.drain() {
		if d.Op == webview2host.OpClose {
			closes++
		}
	}
	if closes == 0 {
		t.Fatal("the launcher was never asked to close the page")
	}
}

// A popup whose close the launcher's report delivered is never listed,
// whether its close comes while its attach waits or while the Manager
// reads its history after it attached, though the close's processing
// waits behind its opener's blocked conversion: the close is in the
// Manager's admission at receipt.
func TestAPopupWhoseCloseWasReceivedIsNeverListed(t *testing.T) {
	for _, gated := range []string{"attach", "history"} {
		t.Run(gated, func(t *testing.T) {
			l := newPopupLauncher(t)
			access := popupTestAccess(t, "thread")
			_, opener := l.open(access)
			var release func()
			if gated == "attach" {
				release = l.gate("PT-1")
			} else {
				release = l.gateHistory("PT-1")
			}
			defer release()
			popupID, _ := l.popup(opener, "https://example.test/popup")
			if gated == "attach" {
				l.awaitAttach("PT-1")
			} else {
				l.awaitAttach("history PT-1")
			}
			entered, unblock := blockPath(t, l.engine, "opener.bin")
			sendDownload(t, l.engine, opener, completedAt("dl-opener", "opener.bin"))
			awaitClosed(t, entered, "the opener's path conversion")

			l.report(popupID, webview2host.ReportClosed, "")
			release()
			deadline := time.Now().Add(300 * time.Millisecond)
			if gated == "history" {
				deadline = time.Now().Add(hostedTestDeadline)
			}
			for {
				if _, listed := l.tabs(access.ThreadID)[popupID]; listed {
					t.Fatal("the Manager listed a popup whose close was received")
				}
				if gated == "history" {
					if d, _ := l.manager.admissions.Disposition(popupadmit.Handle(popupID)); d == popupadmit.Ended {
						break
					}
				}
				if time.Now().After(deadline) {
					if gated == "history" {
						t.Fatal("the popup's listing never committed")
					}
					break
				}
				time.Sleep(time.Millisecond)
			}
			unblock()
			l.awaitDownloads(access, downloadStateCompleted)
			l.awaitTabGone(access.ThreadID, popupID)
			settleReports(t, l.engine, "")
			if _, listed := l.tabs(access.ThreadID)[popupID]; listed {
				t.Fatal("the closed popup was listed")
			}
		})
	}
}

// Sibling popups the Manager listed run their reports on lanes of their
// own: one's slow conversion holds neither the other's progress nor its
// close.
func TestListedPopupsDoNotShareTheirOpenersLane(t *testing.T) {
	l := newPopupLauncher(t)
	access := popupTestAccess(t, "thread")
	_, opener := l.open(access)
	a, _ := l.popup(opener, "https://example.test/a")
	b, _ := l.popup(opener, "https://example.test/b")
	for range 2 {
		select {
		case <-l.adopted:
		case <-time.After(hostedTestDeadline):
			t.Fatal("the popups were never listed")
		}
	}
	entered, unblock := blockPath(t, l.engine, "a.bin")
	sendDownload(t, l.engine, a, completedAt("dl-a", "a.bin"))
	awaitClosed(t, entered, "popup a's path conversion")
	if lane, ok := l.engine.pages.laneOf(hostedPageID(a)); !ok || lane != hostedPageID(a) {
		t.Fatalf("listed popup a runs on lane %q (%v), want its own", lane, ok)
	}

	sendDownload(t, l.engine, b, webview2host.DownloadReport{ID: "dl-b", Seq: 1, State: webview2host.DownloadRunning, Path: `C:\Users\dev\Downloads\b.bin`, Received: 5, Total: 10})
	deadline := time.Now().Add(hostedTestDeadline)
	for !slices.Equal(l.pageDownloads(access.ThreadID, b), []string{downloadStateRunning}) {
		if time.Now().After(deadline) {
			t.Fatalf("popup b's download waited behind popup a's conversion: b lists %v", l.pageDownloads(access.ThreadID, b))
		}
		time.Sleep(time.Millisecond)
	}
	l.report(b, webview2host.ReportClosed, "")
	l.awaitTabGone(access.ThreadID, b)
	unblock()
	settleReports(t, l.engine, "")
	if got := l.pageDownloads(access.ThreadID, a); !slices.Equal(got, []string{downloadStateCompleted}) {
		t.Fatalf("popup a lists %v", got)
	}
}

// A download is its page's: a report naming a running download's id from
// a page that never existed, or from another page, settles nothing and
// starts nothing, and the other page's is logged; the owner's own report
// still applies.
func TestAReportNamingAnotherPagesDownloadIsRefused(t *testing.T) {
	engine, _, recorder := newDownloadHostedEngine(t)
	var logMu sync.Mutex
	var logged []string
	engine.logf = func(format string, args ...any) {
		logMu.Lock()
		logged = append(logged, format)
		logMu.Unlock()
	}
	bindTestPage(t, engine, "page1", "TARGET-1")
	bindTestPage(t, engine, "page2", "TARGET-2")
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", Seq: 1, State: webview2host.DownloadRunning, Path: `C:\Users\dev\Downloads\a.bin`, Received: 1, Total: 10})
	recorder.take()

	for _, page := range []string{"never-was", "page2"} {
		reportDownload(t, engine, page, webview2host.DownloadReport{ID: "dl-a-1", Seq: 2, State: webview2host.DownloadCompleted, Path: `C:\Users\dev\Downloads\elsewhere.bin`, Received: 10, Total: 10})
	}
	if starts, progress := recorder.take(); len(starts)+len(progress) != 0 {
		t.Fatalf("another page's report reached the Manager: starts %+v, progress %+v", starts, progress)
	}
	logMu.Lock()
	refusal := slices.ContainsFunc(logged, func(format string) bool { return strings.Contains(format, "another page owns") })
	logMu.Unlock()
	if !refusal {
		t.Fatal("the report naming page1's download from page2 was not logged")
	}
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", Seq: 2, State: webview2host.DownloadCompleted, Path: `C:\Users\dev\Downloads\a.bin`, Received: 10, Total: 10})
	if _, progress := recorder.take(); len(progress) != 1 || progress[0].State != downloadCompleted || progress[0].FilePath != "/mnt/c/Users/dev/Downloads/a.bin" {
		t.Fatalf("the owner's completion applied as %+v", progress)
	}
}

// After a pending popup's close or discard is received it admits no
// download: one it started before ends once, canceled, a first report
// after the end starts nothing, and no download is left running, whether
// the popup was attached (the Manager drops it when its opener closes
// mid-adoption) or had no target (the launcher's close).
func TestAPopupsFirstDownloadAfterItsEndNeverStarts(t *testing.T) {
	for _, how := range []string{"discarded after its bind", "targetless and closed"} {
		t.Run(how, func(t *testing.T) {
			l := newPopupLauncher(t)
			access := popupTestAccess(t, "thread")
			openerTab, opener := l.open(access)
			var popupID string
			var release func()
			if how == "targetless and closed" {
				popupID = "popup-targetless"
				l.reportPopup(popupID, webview2host.PopupReport{Opener: opener, URL: "https://example.test/popup"})
			} else {
				release = l.gateHistory("PT-1")
				defer release()
				popupID, _ = l.popup(opener, "https://example.test/popup")
				l.awaitAttach("history PT-1")
			}
			reportDownload(t, l.engine, popupID, webview2host.DownloadReport{ID: "dl-a", Seq: 1, State: webview2host.DownloadRunning, Path: `C:\Users\dev\Downloads\a.bin`, Received: 1, Total: 10})

			if how == "targetless and closed" {
				l.report(popupID, webview2host.ReportClosed, "")
			} else {
				if err := l.manager.CloseCompanionPage(t.Context(), access, openerTab); err != nil {
					t.Fatalf("close the opener: %v", err)
				}
				release()
				deadline := time.Now().Add(hostedTestDeadline)
				for l.engine.pages.admits(hostedPageID(popupID)) {
					if time.Now().After(deadline) {
						t.Fatal("the Manager never discarded the popup")
					}
					time.Sleep(time.Millisecond)
				}
			}
			sendDownload(t, l.engine, popupID, webview2host.DownloadReport{ID: "dl-b", Seq: 1, State: webview2host.DownloadRunning, Path: `C:\Users\dev\Downloads\b.bin`, Received: 1, Total: 10})
			settleReports(t, l.engine, "")
			if running := l.engine.pages.running(); running != 0 {
				t.Fatalf("the engine runs %d downloads of the gone popup", running)
			}
			listed := l.engineDownloads(access.ThreadID)
			if _, started := listed["dl-b"]; started {
				t.Fatalf("a download first reported after the popup's end was listed: %v", listed)
			}
			for _, d := range l.manager.CompanionState(access).Downloads {
				if d.State == downloadStateRunning {
					t.Fatalf("download %+v was left running", d)
				}
			}
		})
	}
}
