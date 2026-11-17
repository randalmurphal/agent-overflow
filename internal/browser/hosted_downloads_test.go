package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/webview2host"
)

// The hosted engine's downloads are the launcher's reports (download.go in
// internal/webview2host), turned into the Manager's download events here,
// and the Manager's cancel turned into the launcher's directive. Nothing in
// these tests crosses CDP.

// hostedDownloadLog is the Manager's side of the download events.
type hostedDownloadLog struct {
	mu       sync.Mutex
	starts   []downloadStart
	progress []downloadProgress
	refuse   bool
}

func (r *hostedDownloadLog) events() engineEvents {
	return engineEvents{
		DownloadStarted: func(start downloadStart) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.starts = append(r.starts, start)
			return !r.refuse
		},
		DownloadProgress: func(progress downloadProgress) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.progress = append(r.progress, progress)
		},
	}
}

func (r *hostedDownloadLog) take() ([]downloadStart, []downloadProgress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	starts, progress := r.starts, r.progress
	r.starts, r.progress = nil, nil
	return starts, progress
}

// testBackendPath stands in for `wslpath -u` of a drive path.
func testBackendPath(_ context.Context, path string) (string, error) {
	drive, rest, ok := strings.Cut(path, `:\`)
	if !ok || len(drive) != 1 {
		return "", fmt.Errorf("wslpath: %s: not a drive path", path)
	}
	return "/mnt/" + strings.ToLower(drive) + "/" + strings.ReplaceAll(rest, `\`, "/"), nil
}

func newDownloadHostedEngine(t *testing.T) (*hostedEngine, *directiveSink, *hostedDownloadLog) {
	t.Helper()
	recorder := &hostedDownloadLog{}
	engine, sink := newTestHostedEngine(t, stubRelay{}, recorder.events())
	engine.backendPath = testBackendPath
	return engine, sink, recorder
}

// reportDownload has the launcher report one download state of a page.
func reportDownload(t *testing.T, engine *hostedEngine, pageID string, report webview2host.DownloadReport) {
	t.Helper()
	detail, err := report.Encode()
	if err != nil {
		t.Fatal(err)
	}
	engine.Report(pageID, webview2host.ReportDownload, detail)
}

// isCancelOf reports whether directive is the launcher's cancel of download
// id in page pageID.
func isCancelOf(directive webview2host.Directive, pageID, id string) bool {
	return directive.Op == webview2host.OpCancelDownload && directive.PageID == pageID && directive.Download == id
}

const testDownloadPath = `C:\Users\dev\Downloads\report (1).pdf`

// A download's life as the launcher reports it reaches the Manager as its
// start in the page, its progress and its finish at the backend's path of
// the file in the Windows Downloads folder. A failure is a failure with the
// launcher's reason, and an ended download is no longer the engine's.
func TestHostedEngineTurnsLauncherDownloadReportsIntoDownloadEvents(t *testing.T) {
	engine, sink, recorder := newDownloadHostedEngine(t)
	bindTestPage(t, engine, "page1", "TARGET-1")

	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", State: webview2host.DownloadStarted, URI: "https://example.test/report.pdf", Path: testDownloadPath, Total: 100})
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", State: webview2host.DownloadProgress, Path: testDownloadPath, Received: 40, Total: 100})
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", State: webview2host.DownloadCompleted, Path: testDownloadPath, Received: 100, Total: 100})
	starts, progress := recorder.take()
	wantStart := downloadStart{Frame: "page1", ID: "dl-a-1", URL: "https://example.test/report.pdf", SuggestedName: "report (1).pdf"}
	if len(starts) != 1 || starts[0] != wantStart {
		t.Fatalf("starts %+v, want %+v", starts, wantStart)
	}
	wantProgress := []downloadProgress{
		{ID: "dl-a-1", Received: 40, Total: 100, State: downloadInProgress},
		{ID: "dl-a-1", Received: 100, Total: 100, State: downloadCompleted, FilePath: "/mnt/c/Users/dev/Downloads/report (1).pdf"},
	}
	if fmt.Sprint(progress) != fmt.Sprint(wantProgress) {
		t.Fatalf("progress %+v, want %+v", progress, wantProgress)
	}
	if err := engine.cancelDownload("dl-a-1"); !errors.Is(err, errDownloadEnded) {
		t.Fatalf("a cancel after the finish answers %v, want errDownloadEnded", err)
	}

	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-2", State: webview2host.DownloadStarted, Path: `C:\Users\dev\Downloads\big.bin`})
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-2", State: webview2host.DownloadInterrupted, Path: `C:\Users\dev\Downloads\big.bin`, Received: 7, Reason: "the network failed"})
	if _, progress := recorder.take(); len(progress) != 1 || progress[0] != (downloadProgress{ID: "dl-a-2", Received: 7, State: downloadCanceled, Error: "the network failed"}) {
		t.Fatalf("an interrupted download reports %+v, want canceled with the launcher's reason", progress)
	}

	// A finished file the backend cannot reach is a failure naming it.
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-3", State: webview2host.DownloadStarted, Path: `\\server\share\x.bin`})
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-3", State: webview2host.DownloadCompleted, Path: `\\server\share\x.bin`, Received: 1})
	if _, progress := recorder.take(); len(progress) != 1 || progress[0].State != downloadCanceled || !strings.Contains(progress[0].Error, `\\server\share\x.bin`) {
		t.Fatalf("an unreachable finished file reports %+v, want a failure naming it", progress)
	}

	// A malformed report reaches nobody.
	engine.Report("page1", webview2host.ReportDownload, `{"id":"dl-a-4","state":"completed"}`)
	if starts, progress := recorder.take(); len(starts)+len(progress) != 0 {
		t.Fatalf("a malformed report reached the Manager: %+v %+v", starts, progress)
	}
	if ops := sink.ops(); len(ops) != 0 {
		t.Fatalf("directives %v, want none", ops)
	}
}

// A launcher download report names its page by the page id, which the
// Manager finds the page by (pageForFrame); the page's CDP frames stay its
// own too.
func TestHostedPageOwnsItsIDAsAFrame(t *testing.T) {
	page := &hostedPage{pageDriver: &cdpPage{handle: "TARGET-1", frames: newFrameTracker()}, id: "page1"}
	if !page.OwnsFrame("page1") || !page.OwnsFrame("TARGET-1") {
		t.Fatal("the hosted page does not own its page id and its main frame")
	}
	if page.OwnsFrame("page2") {
		t.Fatal("the hosted page owns another page's id")
	}
}

// The Manager's refusal of a download is the launcher's cancel of it; a
// download reported for a page the engine does not hold reaches nobody, and
// its page's close is the launcher's cancel.
func TestHostedEngineCancelsADownloadTheManagerRefuses(t *testing.T) {
	engine, sink, recorder := newDownloadHostedEngine(t)
	recorder.refuse = true
	bindTestPage(t, engine, "page1", "TARGET-1")

	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", State: webview2host.DownloadStarted, Path: `C:\Users\dev\Downloads\x.bin`})
	directive := sink.next(t)
	if !isCancelOf(directive, "page1", "dl-a-1") {
		t.Fatalf("the refusal sent %+v, want the launcher's cancel of the download", directive)
	}
	if err := directive.Validate(); err != nil {
		t.Fatalf("the cancel directive is invalid: %v", err)
	}

	reportDownload(t, engine, "page9", webview2host.DownloadReport{ID: "dl-a-2", State: webview2host.DownloadStarted, Path: `C:\Users\dev\Downloads\y.bin`})
	if starts, _ := recorder.take(); len(starts) != 1 {
		t.Fatalf("starts %+v, want only the held page's", starts)
	}
	sink.expectOps(t, webview2host.OpCancelDownload)
}

// The user's cancel is the launcher's: the engine names the download's page
// and leaves the download to the launcher's terminal report.
func TestHostedEngineCancelAsksTheLauncher(t *testing.T) {
	engine, sink, _ := newDownloadHostedEngine(t)
	bindTestPage(t, engine, "page1", "TARGET-1")
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", State: webview2host.DownloadStarted, Path: `C:\Users\dev\Downloads\x.bin`})
	profile := testProfile(t, engine)
	if err := profile.CancelDownload("dl-a-1"); err != nil {
		t.Fatal(err)
	}
	if directive := sink.next(t); !isCancelOf(directive, "page1", "dl-a-1") {
		t.Fatalf("the cancel sent %+v", directive)
	}
	// Running until the launcher says otherwise: a second cancel asks again.
	if err := profile.CancelDownload("dl-a-1"); err != nil {
		t.Fatalf("a second cancel before the launcher's report answers %v", err)
	}
	if err := profile.CancelDownload("dl-unknown"); !errors.Is(err, errDownloadEnded) {
		t.Fatalf("a cancel of a download the engine does not hold answers %v", err)
	}
}

// A page that goes takes its own downloads from the engine, whichever way it
// goes, and leaves another page's running download alone: that one's
// progress still reaches the Manager and its cancel still reaches the
// launcher.
func TestHostedEnginePageDepartureLeavesAnotherPagesDownload(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leave func(engine *hostedEngine)
	}{
		{"the Manager closes it", func(engine *hostedEngine) { engine.closePage("page1") }},
		{"the launcher reports it closed", func(engine *hostedEngine) { engine.Report("page1", webview2host.ReportClosed, "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, sink, recorder := newDownloadHostedEngine(t)
			engine.events.PageClosed = func(string) {}
			bindTestPage(t, engine, "page1", "TARGET-1")
			bindTestPage(t, engine, "page2", "TARGET-2")
			reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", State: webview2host.DownloadStarted, Path: `C:\Users\dev\Downloads\one.bin`})
			reportDownload(t, engine, "page2", webview2host.DownloadReport{ID: "dl-a-2", State: webview2host.DownloadStarted, Path: `C:\Users\dev\Downloads\two.bin`})
			recorder.take()

			tc.leave(engine)
			if err := engine.cancelDownload("dl-a-1"); !errors.Is(err, errDownloadEnded) {
				t.Fatalf("the gone page's download answers a cancel with %v, want errDownloadEnded", err)
			}
			reportDownload(t, engine, "page2", webview2host.DownloadReport{ID: "dl-a-2", State: webview2host.DownloadProgress, Path: `C:\Users\dev\Downloads\two.bin`, Received: 5})
			if _, progress := recorder.take(); len(progress) != 1 || progress[0].ID != "dl-a-2" || progress[0].Received != 5 {
				t.Fatalf("the other page's progress reports %+v", progress)
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
// crashed page, which takes it from the engine; the launcher's later report
// of its end reaches the Manager once.
func TestHostedEngineConnectionLossMidDownload(t *testing.T) {
	endpoint := startFakeCDPEndpoint(t, nil, nil)
	recorder := &hostedDownloadLog{}
	crashed := make(chan string, 4)
	events := recorder.events()
	events.PageCrashed = func(handle, _ string) { crashed <- handle }
	engine, sink := newTestHostedEngine(t, stubRelay{url: endpoint.wsURL}, events)
	engine.backendPath = testBackendPath
	engine.attachTimeout = 5 * time.Second
	conn, err := engine.ensureBrowser()
	if err != nil {
		t.Fatalf("ensureBrowser: %v", err)
	}
	defer engine.Interrupt()
	if err := engine.bind("page1", "TARGET-1", conn); err != nil {
		t.Fatal(err)
	}
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", State: webview2host.DownloadStarted, Path: `C:\Users\dev\Downloads\x.bin`})

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
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", State: webview2host.DownloadProgress, Path: `C:\Users\dev\Downloads\x.bin`, Received: 3})
	if err := engine.cancelDownload("dl-a-1"); err != nil {
		t.Fatalf("a cancel after the connection loss answers %v", err)
	}
	engine.closePage("page1")
	if err := engine.cancelDownload("dl-a-1"); !errors.Is(err, errDownloadEnded) {
		t.Fatalf("a cancel after the page closed answers %v", err)
	}
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", State: webview2host.DownloadInterrupted, Path: `C:\Users\dev\Downloads\x.bin`, Received: 3, Reason: "the download was canceled"})
	_, progress := recorder.take()
	terminal := 0
	for _, report := range progress {
		if report.State != downloadInProgress {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("progress %+v, want one terminal report", progress)
	}
	ops := sink.ops()
	if len(ops) < 2 || ops[len(ops)-2] != webview2host.OpCancelDownload || ops[len(ops)-1] != webview2host.OpClose {
		t.Fatalf("directives %v, want the cancel then the page's close", ops)
	}
}
