package browser

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/webview2host"
)

// A page that goes takes its own downloads, running and ended, from the
// engine, whichever way it goes: one still running ends there, canceled
// with the page as the launcher cancels it, and a later snapshot of one
// reaches nobody. Another page's running download is left alone: its
// progress still reaches the Manager and its cancel still reaches the
// launcher.
func TestHostedEnginePageDepartureLeavesAnotherPagesDownload(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leave func(t *testing.T, engine *hostedEngine)
		why   string
	}{
		{"the Manager closes it", func(t *testing.T, engine *hostedEngine) { mustRetire(t, engine.closePage("page1")) }, popupDiscardedReason},
		{"the launcher reports it closed", func(_ *testing.T, engine *hostedEngine) {
			engine.Report("page1", webview2host.ReportClosed, "")
			reportsProcessed(engine, "page1")
		}, pageClosedReason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, sink, recorder := newDownloadHostedEngine(t)
			engine.events.PageClosed = func(ids.PageHandle) {}
			bindTestPage(t, engine, "page1", "TARGET-1")
			bindTestPage(t, engine, "page2", "TARGET-2")
			reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", State: webview2host.DownloadRunning, Path: `C:\Users\dev\Downloads\one.bin`})
			reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-0", State: webview2host.DownloadCompleted, Path: `C:\Users\dev\Downloads\zero.bin`})
			reportDownload(t, engine, "page2", webview2host.DownloadReport{ID: "dl-a-2", State: webview2host.DownloadRunning, Path: `C:\Users\dev\Downloads\two.bin`})
			recorder.take()

			tc.leave(t, engine)
			if _, progress := recorder.take(); len(progress) != 1 || progress[0] != (downloadProgress{ID: "dl-a-1", State: downloadPageGone, Error: tc.why}) {
				t.Fatalf("the departure reported %+v, want the running download's end", progress)
			}
			if _, kept := engine.pages.state("page1"); kept {
				t.Fatal("the gone page's record stayed")
			}
			if err := engine.cancelDownload("dl-a-1"); !errors.Is(err, errDownloadEnded) {
				t.Fatalf("the gone page's download answers a cancel with %v, want errDownloadEnded", err)
			}
			reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", State: webview2host.DownloadInterrupted, End: webview2host.DownloadEndCanceled, Reason: "the download was canceled"})
			reportDownload(t, engine, "page2", webview2host.DownloadReport{ID: "dl-a-2", State: webview2host.DownloadRunning, Path: `C:\Users\dev\Downloads\two.bin`, Received: 5})
			if starts, progress := recorder.take(); len(starts) != 0 || len(progress) != 1 || progress[0].ID != "dl-a-2" || progress[0].Received != 5 {
				t.Fatalf("after the departure, starts %+v and progress %+v", starts, progress)
			}
			if err := engine.cancelDownload("dl-a-2"); err != nil {
				t.Fatal(err)
			}
			deadline := time.After(hostedTestDeadline)
			for {
				ops := sink.ops()
				if len(ops) > 0 && ops[len(ops)-1] == webview2host.OpCancelDownload {
					break
				}
				select {
				case <-deadline:
					t.Fatalf("directives %v, want the other page's cancel last", ops)
				case <-time.After(5 * time.Millisecond):
				}
			}
		})
	}
}

// A browser connection lost mid-download reports the page crashed; the
// download is the launcher's and runs on until the Manager closes the
// crashed page, which ends it and takes it from the engine, so the
// launcher's later report of its end reaches nobody.
func TestHostedEngineConnectionLossMidDownload(t *testing.T) {
	endpoint := startFakeCDPEndpoint(t, nil, nil)
	recorder := &hostedDownloadLog{}
	crashed := make(chan string, 4)
	events := recorder.events()
	events.PageCrashed = func(handle ids.PageHandle, _ string) { crashed <- string(handle) }
	engine, sink := newTestHostedEngine(t, stubRelay{url: endpoint.wsURL}, events)
	engine.backendPath = testBackendPath
	engine.attachTimeout = 5 * time.Second
	conn, err := engine.ensureBrowser()
	if err != nil {
		t.Fatalf("ensureBrowser: %v", err)
	}
	defer engine.Interrupt()
	if err := bindForTest(engine, "page1", "TARGET-1", conn); err != nil {
		t.Fatal(err)
	}
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", State: webview2host.DownloadRunning, Path: `C:\Users\dev\Downloads\x.bin`})

	endpoint.hangUp()
	select {
	case handle := <-crashed:
		if handle != "page1" {
			t.Fatalf("PageCrashed for %s", handle)
		}
	case <-time.After(hostedTestDeadline):
		t.Fatal("the lost connection never reported its page")
	}
	// The launcher's download runs on until the page closes.
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", State: webview2host.DownloadRunning, Path: `C:\Users\dev\Downloads\x.bin`, Received: 3})
	if err := engine.cancelDownload("dl-a-1"); err != nil {
		t.Fatalf("a cancel after the connection loss answers %v", err)
	}
	engine.closePage("page1")
	if err := engine.cancelDownload("dl-a-1"); !errors.Is(err, errDownloadEnded) {
		t.Fatalf("a cancel after the page closed answers %v", err)
	}
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", State: webview2host.DownloadInterrupted, End: webview2host.DownloadEndCanceled, Path: `C:\Users\dev\Downloads\x.bin`, Received: 3, Reason: "the download was canceled"})
	_, progress := recorder.take()
	if len(progress) != 2 || progress[0].Received != 3 || progress[1] != (downloadProgress{ID: "dl-a-1", State: downloadPageGone, Error: popupDiscardedReason, Received: 3}) {
		t.Fatalf("progress %+v, want the one progress and the end of the close at its counts, none of the launcher's late report", progress)
	}
	ops := sink.ops()
	if len(ops) < 2 || ops[len(ops)-2] != webview2host.OpCancelDownload || ops[len(ops)-1] != webview2host.OpClose {
		t.Fatalf("directives %v, want the cancel then the page's close", ops)
	}
}

// A popup the Manager never listed takes every download it started with it,
// whether it is discarded or its page closes, and whether or not its attach
// bound it: one whose first report arrives while the popup's running
// downloads are being ended is refused or ended too, never left running in
// its opener's tab.
func TestHostedPopupGoingEndsADownloadReportedMeanwhile(t *testing.T) {
	for _, how := range []string{"discard", "close", "discard after its bind", "close after its bind"} {
		t.Run(how, func(t *testing.T) {
			engine, _, recorder := newDownloadHostedEngine(t)
			engine.events.PageClosed = func(ids.PageHandle) {}
			bindTestPage(t, engine, "opener", "T-opener")
			pendingTestPopup(engine, "popup", "T-popup", "opener")
			if strings.HasSuffix(how, "after its bind") {
				// Bound as its attach binds it, before the Manager lists it.
				bindTestPage(t, engine, "popup", "T-popup")
			}
			reportDownload(t, engine, "popup", snapshot("dl-1", webview2host.DownloadRunning, 10))
			var once sync.Once
			injected := make(chan struct{})
			recorder.mu.Lock()
			recorder.progressing = func(progress downloadProgress) {
				if progress.ID == "dl-1" && progress.State == downloadPageGone {
					// On the popup's lane, running its close: the report is
					// accepted or refused, never waited for.
					once.Do(func() {
						sendDownload(t, engine, "popup", snapshot("dl-2", webview2host.DownloadRunning, 10))
						close(injected)
					})
				}
			}
			recorder.mu.Unlock()
			if strings.HasPrefix(how, "discard") {
				if err := engine.DiscardPage(t.Context(), "popup"); err != nil {
					t.Errorf("discard: %v", err)
				}
			} else {
				engine.Report("popup", webview2host.ReportClosed, "")
			}
			settleReports(t, engine, "")
			select {
			case <-injected:
			default:
				t.Fatal("no download was reported while the popup's downloads were ended")
			}
			starts, progress := recorder.take()
			ended := map[downloadID]bool{}
			for _, p := range progress {
				if p.State != downloadInProgress {
					ended[p.ID] = true
				}
			}
			if !slices.ContainsFunc(starts, func(start downloadStart) bool { return start.ID == "dl-1" }) {
				t.Fatalf("the popup's first download never started: starts %+v", starts)
			}
			for _, start := range starts {
				if !ended[start.ID] {
					t.Errorf("download %s started in the popup and never ended: starts %+v, progress %+v", start.ID, starts, progress)
				}
			}
			if running := engine.pages.running(); running != 0 {
				t.Errorf("the engine still runs %d downloads of the gone popup", running)
			}
		})
	}
}
