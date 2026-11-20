package browser

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/webview2host"
)

// The hosted engine's downloads are the launcher's snapshots (download.go
// in internal/webview2host), applied idempotently into the Manager's
// download events here, and the Manager's cancel and the engine's status
// question turned into the launcher's directives. Nothing in these tests
// crosses CDP.

// hostedDownloadLog is the Manager's side of the download events.
type hostedDownloadLog struct {
	mu       sync.Mutex
	starts   []downloadStart
	progress []downloadProgress
	refuse   bool
	// admitting, when set, is called as the Manager admits a download,
	// before it answers: the admission hook.
	admitting func(downloadStart)
	// progressing, when set, is called as the Manager takes a download's
	// progress, before it is recorded.
	progressing func(downloadProgress)
}

func (r *hostedDownloadLog) events() engineEvents {
	return engineEvents{
		DownloadStarted: func(start downloadStart) downloadAdmission {
			r.mu.Lock()
			hook := r.admitting
			r.mu.Unlock()
			if hook != nil {
				hook(start)
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			r.starts = append(r.starts, start)
			if r.refuse {
				return downloadRefused
			}
			return downloadAdmitted
		},
		DownloadProgress: func(progress downloadProgress) {
			r.mu.Lock()
			hook := r.progressing
			r.mu.Unlock()
			if hook != nil {
				hook(progress)
			}
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
	// No status question unless a test asks for one.
	engine.downloadQuiet = time.Hour
	t.Cleanup(engine.Stop)
	return engine, sink, recorder
}

// testSnapshotNumbers numbers the snapshots a test sends without one, per
// engine and download, as the launcher numbers them. The map holds each
// engine, so no later test's engine takes its address and continues its
// numbers.
var testSnapshotNumbers = struct {
	sync.Mutex
	last map[*hostedEngine]map[string]uint64
}{last: map[*hostedEngine]map[string]uint64{}}

// settleReports waits until the engine has processed every report of
// pageID it accepted, or of every page when pageID is empty.
func settleReports(t *testing.T, engine *hostedEngine, pageID string) {
	t.Helper()
	if !reportsProcessed(engine, pageID) {
		t.Fatalf("the reports of page %q were not processed", pageID)
	}
}

// reportsProcessed answers whether the engine processed every report of
// pageID (of every page when empty) it accepted, within hostedTestDeadline.
func reportsProcessed(engine *hostedEngine, pageID string) bool {
	for deadline := time.Now().Add(hostedTestDeadline); ; time.Sleep(time.Millisecond) {
		busy := engine.pages.busy()
		if pageID != "" {
			_, busy = engine.pages.laneOf(ids.PageHandle(pageID))
		}
		if !busy {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// reportDownload has the launcher send one snapshot of a page's download,
// numbered after the download's last one unless it carries its number, and
// waits until the engine processed it.
func reportDownload(t *testing.T, engine *hostedEngine, pageID string, report webview2host.DownloadReport) {
	t.Helper()
	sendDownload(t, engine, pageID, report)
	settleReports(t, engine, pageID)
}

// sendDownload is reportDownload without the wait: it returns once the
// engine acknowledged the snapshot.
func sendDownload(t *testing.T, engine *hostedEngine, pageID string, report webview2host.DownloadReport) {
	t.Helper()
	if report.Seq == 0 && report.State != webview2host.DownloadUnknown {
		testSnapshotNumbers.Lock()
		numbers := testSnapshotNumbers.last[engine]
		if numbers == nil {
			numbers = map[string]uint64{}
			testSnapshotNumbers.last[engine] = numbers
		}
		numbers[report.ID]++
		report.Seq = numbers[report.ID]
		testSnapshotNumbers.Unlock()
	}
	detail, err := report.Encode()
	if err != nil {
		t.Fatal(err)
	}
	engine.Report(ids.PageHandle(pageID), webview2host.ReportDownload, detail)
}

// isCancelOf reports whether directive is the launcher's cancel of download
// id in page pageID.
func isCancelOf(directive webview2host.Directive, pageID, id string) bool {
	return directive.Op == webview2host.OpCancelDownload && directive.PageID == pageID && directive.Download == id
}

const testDownloadPath = `C:\Users\dev\Downloads\report (1).pdf`

// snapshot is a download's snapshot at testDownloadPath.
func snapshot(id, state string, received int64) webview2host.DownloadReport {
	report := webview2host.DownloadReport{ID: id, State: state, URI: "https://example.test/report.pdf", Name: "report (1).pdf", Path: testDownloadPath, Received: received, Total: 100}
	if state == webview2host.DownloadInterrupted {
		report.End, report.Reason = webview2host.DownloadEndCanceled, "the download was canceled"
	}
	return report
}

// terminals counts the terminal events among progress.
func terminals(progress []downloadProgress) int {
	n := 0
	for _, p := range progress {
		if p.State != downloadInProgress {
			n++
		}
	}
	return n
}

// A download's snapshots reach the Manager as its start in the page, its
// progress and its finish at the backend's path of the file in the Windows
// Downloads folder. An interruption is a cancel with the launcher's reason;
// a saved file the backend cannot name or reach completes with the reason
// and no file; an ended download is no longer the engine's.
func TestHostedEngineTurnsLauncherSnapshotsIntoDownloadEvents(t *testing.T) {
	engine, sink, recorder := newDownloadHostedEngine(t)
	bindTestPage(t, engine, "page1", "TARGET-1")

	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadRunning, 0))
	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadRunning, 40))
	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadCompleted, 100))
	starts, progress := recorder.take()
	wantStart := downloadStart{Frame: "page1", ID: "dl-a-1", URL: "https://example.test/report.pdf", SuggestedName: "report (1).pdf"}
	if len(starts) == 1 {
		// Its completion was received by now.
		if starts[0].EndReported == nil || !starts[0].EndReported() {
			t.Fatal("the start does not report the end its engine received")
		}
		starts[0].EndReported = nil
	}
	if len(starts) != 1 || !reflect.DeepEqual(starts[0], wantStart) {
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

	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-2", State: webview2host.DownloadRunning, Path: `C:\Users\dev\Downloads\big.bin`})
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-2", State: webview2host.DownloadInterrupted, End: webview2host.DownloadEndFailed, Path: `C:\Users\dev\Downloads\big.bin`, Received: 7, Reason: "the network failed"})
	if _, progress := recorder.take(); len(progress) != 1 || progress[0] != (downloadProgress{ID: "dl-a-2", Received: 7, State: downloadFailed, Error: "the network failed"}) {
		t.Fatalf("a download the network ended reports %+v, want failed with the launcher's reason", progress)
	}

	// A saved file the backend cannot reach completes naming it, with no
	// file to open.
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-3", State: webview2host.DownloadCompleted, Path: `\\server\share\x.bin`, Received: 1})
	if _, progress := recorder.take(); len(progress) != 1 || progress[0].State != downloadCompleted || progress[0].FilePath != "" || !strings.Contains(progress[0].Error, `\\server\share\x.bin`) {
		t.Fatalf("an unreachable saved file reports %+v, want a completion naming it with no file", progress)
	}
	// So does one whose path did not fit the report.
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-4", State: webview2host.DownloadCompleted, Name: "x.bin", Received: 1, Reason: webview2host.DownloadPathTooLongReason})
	if starts, progress := recorder.take(); len(starts) != 1 || starts[0].SuggestedName != "x.bin" || len(progress) != 1 || progress[0].State != downloadCompleted || progress[0].FilePath != "" || progress[0].Error != webview2host.DownloadPathTooLongReason {
		t.Fatalf("a saved file with no path reports %+v %+v", starts, progress)
	}

	// A malformed report reaches nobody.
	engine.Report("page1", webview2host.ReportDownload, `{"id":"dl-a-5","state":"completed"}`)
	if starts, progress := recorder.take(); len(starts)+len(progress) != 0 {
		t.Fatalf("a malformed report reached the Manager: %+v %+v", starts, progress)
	}
	if ops := sink.ops(); len(ops) != 0 {
		t.Fatalf("directives %v, want none", ops)
	}
}

// A lost snapshot heals on the next: whichever arrives first starts the
// row, and a repeat, or anything after the end, changes nothing.
func TestHostedEngineAppliesSnapshotsIdempotently(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reports   []webview2host.DownloadReport
		wantState []downloadState
	}{
		{"a lost start, then progress",
			[]webview2host.DownloadReport{snapshot("dl-a-1", webview2host.DownloadRunning, 40), snapshot("dl-a-1", webview2host.DownloadCompleted, 100)},
			[]downloadState{downloadInProgress, downloadCompleted}},
		{"a lost start, then the end",
			[]webview2host.DownloadReport{snapshot("dl-a-1", webview2host.DownloadCompleted, 100)},
			[]downloadState{downloadCompleted}},
		// A repeat of a running snapshot is the same progress again, which
		// sets nothing new; it never starts a second row.
		{"a repeated snapshot",
			[]webview2host.DownloadReport{snapshot("dl-a-1", webview2host.DownloadRunning, 0), snapshot("dl-a-1", webview2host.DownloadRunning, 0), snapshot("dl-a-1", webview2host.DownloadRunning, 40), snapshot("dl-a-1", webview2host.DownloadCompleted, 100)},
			[]downloadState{downloadInProgress, downloadInProgress, downloadCompleted}},
		{"a duplicate terminal",
			[]webview2host.DownloadReport{snapshot("dl-a-1", webview2host.DownloadRunning, 0), snapshot("dl-a-1", webview2host.DownloadCompleted, 100), snapshot("dl-a-1", webview2host.DownloadCompleted, 100), snapshot("dl-a-1", webview2host.DownloadInterrupted, 100)},
			[]downloadState{downloadCompleted}},
		{"a late progress after the end",
			[]webview2host.DownloadReport{snapshot("dl-a-1", webview2host.DownloadRunning, 0), {ID: "dl-a-1", State: webview2host.DownloadInterrupted, End: webview2host.DownloadEndFailed, Received: 3, Reason: "the network failed"}, snapshot("dl-a-1", webview2host.DownloadRunning, 50)},
			[]downloadState{downloadFailed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, _, recorder := newDownloadHostedEngine(t)
			bindTestPage(t, engine, "page1", "TARGET-1")
			for _, report := range tc.reports {
				reportDownload(t, engine, "page1", report)
			}
			starts, progress := recorder.take()
			if len(starts) != 1 || starts[0].ID != "dl-a-1" || starts[0].SuggestedName != "report (1).pdf" {
				t.Fatalf("starts %+v, want the one row", starts)
			}
			var got []downloadState
			for _, p := range progress {
				got = append(got, p.State)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.wantState) {
				t.Fatalf("progress %+v, want states %v", progress, tc.wantState)
			}
		})
	}
}

// The start and progress arrive, the end is lost,
// and the download looks running. The user's cancel asks the launcher,
// whose answer is the download's terminal snapshot, which settles the row
// once; the engine asks nothing more of it.
func TestHostedEngineCancelOfADownloadWhoseEndWasLostSettlesIt(t *testing.T) {
	engine, sink, recorder := newDownloadHostedEngine(t)
	bindTestPage(t, engine, "page1", "TARGET-1")
	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadRunning, 0))
	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadRunning, 40))
	// The completion is lost.
	recorder.take()

	if err := testProfile(t, engine).CancelDownload("dl-a-1"); err != nil {
		t.Fatal(err)
	}
	directive := sink.next(t)
	if !isCancelOf(directive, "page1", "dl-a-1") {
		t.Fatalf("the cancel sent %+v", directive)
	}
	if err := directive.Validate(); err != nil {
		t.Fatalf("the cancel directive is invalid: %v", err)
	}
	// The launcher answers with the snapshot it kept of the ended download.
	answer := snapshot("dl-a-1", webview2host.DownloadCompleted, 100)
	answer.Answer = true
	reportDownload(t, engine, "page1", answer)
	_, progress := recorder.take()
	if len(progress) != 1 || progress[0].State != downloadCompleted || progress[0].FilePath != "/mnt/c/Users/dev/Downloads/report (1).pdf" {
		t.Fatalf("the answer settled %+v", progress)
	}
	if err := engine.cancelDownload("dl-a-1"); !errors.Is(err, errDownloadEnded) {
		t.Fatalf("a cancel after the answer answers %v", err)
	}
	sink.expectOps(t, webview2host.OpCancelDownload)
}

// A cancel the launcher could not make is answered with the refusal, and
// every later snapshot carries it: it reaches the Manager once, from the
// newest snapshot, and an older one that arrives after changes nothing.
func TestHostedEngineDeliversARefusedCancel(t *testing.T) {
	engine, sink, recorder := newDownloadHostedEngine(t)
	bindTestPage(t, engine, "page1", "TARGET-1")
	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadRunning, 10))
	recorder.take()
	if err := engine.cancelDownload("dl-a-1"); err != nil {
		t.Fatal(err)
	}
	sink.expectOps(t, webview2host.OpCancelDownload)
	refusal := webview2host.DownloadCancelRefusedReason + ": Unspecified error"
	refused := snapshot("dl-a-1", webview2host.DownloadRunning, 20)
	refused.Seq, refused.Answer, refused.Cancel = 3, true, webview2host.CancelRefused(1, errors.New("Unspecified error"))
	newer := snapshot("dl-a-1", webview2host.DownloadRunning, 30)
	newer.Seq, newer.Cancel = 4, refused.Cancel
	reportDownload(t, engine, "page1", newer)
	reportDownload(t, engine, "page1", refused)
	_, progress := recorder.take()
	want := []downloadProgress{
		{ID: "dl-a-1", State: downloadCancelRefused, Error: refusal},
		{ID: "dl-a-1", Received: 30, Total: 100, State: downloadInProgress},
	}
	if !reflect.DeepEqual(progress, want) {
		t.Fatalf("progress %+v, want %+v", progress, want)
	}
}

// A cancel of a download the engine has seen end, or never saw, sends
// nothing. A download the launcher has no record of (its answer says so)
// ends canceled with that reason.
func TestHostedEngineCancelOfAnEndedOrUnknownDownload(t *testing.T) {
	engine, sink, recorder := newDownloadHostedEngine(t)
	bindTestPage(t, engine, "page1", "TARGET-1")
	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadCompleted, 100))
	for _, id := range []downloadID{"dl-a-1", "dl-unknown"} {
		if err := engine.cancelDownload(id); !errors.Is(err, errDownloadEnded) {
			t.Fatalf("a cancel of %s answers %v, want errDownloadEnded", id, err)
		}
	}
	if ops := sink.ops(); len(ops) != 0 {
		t.Fatalf("directives %v, want none", ops)
	}

	reportDownload(t, engine, "page1", snapshot("dl-a-2", webview2host.DownloadRunning, 0))
	recorder.take()
	if err := engine.cancelDownload("dl-a-2"); err != nil {
		t.Fatal(err)
	}
	reportDownload(t, engine, "page1", unknownAnswer("dl-a-2"))
	if _, progress := recorder.take(); len(progress) != 1 || progress[0].State != downloadUntracked || progress[0].Error != webview2host.DownloadUnknownReason {
		t.Fatalf("the launcher's unknown answer settled %+v", progress)
	}
}

// A running download the launcher has not reported for the quiet interval
// is asked about, so a lost end settles with no one acting. One that keeps
// reporting is not asked, and the question stops once nothing runs.
func TestHostedEngineAsksAboutAQuietDownload(t *testing.T) {
	engine, sink, recorder := newDownloadHostedEngine(t)
	const quiet = 50 * time.Millisecond
	engine.downloadQuiet = quiet
	bindTestPage(t, engine, "page1", "TARGET-1")
	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadRunning, 0))
	reportDownload(t, engine, "page1", snapshot("dl-a-2", webview2host.DownloadRunning, 0))
	// dl-a-2 reports far more often than the interval for three intervals.
	for i := range 30 {
		reportDownload(t, engine, "page1", snapshot("dl-a-2", webview2host.DownloadRunning, int64(i+1)))
		time.Sleep(quiet / 10)
	}
	asked := sink.drain()
	if len(asked) == 0 {
		t.Fatal("the quiet download was never asked about")
	}
	for _, directive := range asked {
		if directive.Op != webview2host.OpDownloadStatus || directive.PageID != "page1" || directive.Download != "dl-a-1" {
			t.Fatalf("while one download reported, the engine sent %+v", directive)
		}
		if err := directive.Validate(); err != nil {
			t.Fatalf("the status directive is invalid: %v", err)
		}
	}
	// The end was lost; the answer is the terminal snapshot.
	answer := snapshot("dl-a-1", webview2host.DownloadCompleted, 100)
	answer.Answer = true
	reportDownload(t, engine, "page1", answer)
	reportDownload(t, engine, "page1", snapshot("dl-a-2", webview2host.DownloadCompleted, 100))
	if _, progress := recorder.take(); terminals(progress) != 2 {
		t.Fatalf("progress %+v, want both ended", progress)
	}
	// Nothing runs: the loop ends, and no more questions come.
	time.Sleep(3 * quiet)
	sink.drain()
	time.Sleep(3 * quiet)
	if extra := sink.drain(); len(extra) != 0 {
		t.Fatalf("questions with nothing running: %+v", extra)
	}
	if engine.pages.watchingNow() {
		t.Fatal("the status loop outlived the last running download")
	}
}

// Stop ends the status loop and forgets every download.
func TestHostedEngineStopEndsTheStatusLoop(t *testing.T) {
	engine, sink, _ := newDownloadHostedEngine(t)
	engine.downloadQuiet = 10 * time.Millisecond
	bindTestPage(t, engine, "page1", "TARGET-1")
	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadRunning, 0))
	sink.next(t)
	engine.Stop()
	sink.drain()
	time.Sleep(60 * time.Millisecond)
	if extra := sink.drain(); len(extra) != 0 {
		t.Fatalf("directives after Stop: %+v", extra)
	}

	// The loop ends at Stop, not at its next tick an hour on.
	engine.downloadQuiet = time.Hour
	before := runtime.NumGoroutine()
	bindTestPage(t, engine, "page1", "TARGET-1")
	reportDownload(t, engine, "page1", snapshot("dl-a-2", webview2host.DownloadRunning, 0))
	if runtime.NumGoroutine() <= before {
		t.Fatal("no status loop started")
	}
	engine.Stop()
	for deadline := time.Now().Add(hostedTestDeadline); runtime.NumGoroutine() > before; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines after Stop, %d before the loop", runtime.NumGoroutine(), before)
		}
	}
}

// unknownAnswer is the launcher's answer for an id it has no record of.
func unknownAnswer(id string) webview2host.DownloadReport {
	return webview2host.DownloadReport{ID: id, State: webview2host.DownloadUnknown, Reason: webview2host.DownloadUnknownReason, Answer: true}
}

// The engine keeps a page's last maxHostedEndedDownloads ended ids: a late
// snapshot of any of them changes nothing; past the bound the oldest is
// forgotten, and still no answer naming it, its kept terminal snapshot or
// the launcher's "unknown", starts a row.
func TestHostedEngineBoundsItsEndedDownloads(t *testing.T) {
	engine, _, recorder := newDownloadHostedEngine(t)
	bindTestPage(t, engine, "page1", "TARGET-1")
	for i := range maxHostedEndedDownloads + 1 {
		reportDownload(t, engine, "page1", snapshot(fmt.Sprintf("dl-a-%d", i), webview2host.DownloadCompleted, 100))
	}
	recorder.take()
	if kept, _ := engine.pages.endedOf("page1", ""); kept != maxHostedEndedDownloads {
		t.Fatalf("kept %d ended ids, want %d", kept, maxHostedEndedDownloads)
	}
	for i := 1; i <= maxHostedEndedDownloads; i++ {
		reportDownload(t, engine, "page1", snapshot(fmt.Sprintf("dl-a-%d", i), webview2host.DownloadRunning, 1))
	}
	if starts, progress := recorder.take(); len(starts)+len(progress) != 0 {
		t.Fatalf("a kept ended download reopened: %+v %+v", starts, progress)
	}
	forgotten := snapshot("dl-a-0", webview2host.DownloadCompleted, 100)
	forgotten.Seq, forgotten.Answer = 3, true
	reportDownload(t, engine, "page1", forgotten)
	reportDownload(t, engine, "page1", unknownAnswer("dl-a-0"))
	if starts, progress := recorder.take(); len(starts)+len(progress) != 0 {
		t.Fatalf("an answer about the forgotten id reached the Manager: %+v %+v", starts, progress)
	}
}

// The engine asks about a quiet download, the
// download's own end arrives first, the page ends maxHostedEndedDownloads
// more, and only then does the launcher's answer arrive, as its running
// snapshot, its kept terminal snapshot or "unknown". None reopens the
// finished download's row.
func TestHostedEngineADelayedAnswerNeverResurrectsAFinishedDownload(t *testing.T) {
	for _, late := range []webview2host.DownloadReport{
		{ID: "dl-a-1", Seq: 2, Answer: true, State: webview2host.DownloadRunning, Path: testDownloadPath, Received: 40},
		{ID: "dl-a-1", Seq: 3, Answer: true, State: webview2host.DownloadCompleted, Path: testDownloadPath, Received: 100},
		unknownAnswer("dl-a-1"),
	} {
		t.Run(late.State, func(t *testing.T) {
			engine, sink, recorder := newDownloadHostedEngine(t)
			engine.downloadQuiet = 20 * time.Millisecond
			bindTestPage(t, engine, "page1", "TARGET-1")
			reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", Seq: 1, State: webview2host.DownloadRunning, Path: testDownloadPath})
			if directive := sink.next(t); directive.Op != webview2host.OpDownloadStatus || directive.Download != "dl-a-1" {
				t.Fatalf("the quiet download was asked about with %+v", directive)
			}
			reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", Seq: 3, State: webview2host.DownloadCompleted, Path: testDownloadPath, Received: 100})
			for i := range maxHostedEndedDownloads {
				reportDownload(t, engine, "page1", snapshot(fmt.Sprintf("dl-b-%d", i), webview2host.DownloadCompleted, 100))
			}
			recorder.take()
			reportDownload(t, engine, "page1", late)
			if starts, progress := recorder.take(); len(starts)+len(progress) != 0 {
				t.Fatalf("the delayed answer reached the Manager: %+v %+v", starts, progress)
			}
		})
	}
}

// heldAdmission makes the Manager's next admission wait: entered closes as
// it begins, and the admission answers once release is closed.
func heldAdmission(recorder *hostedDownloadLog) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	recorder.mu.Lock()
	recorder.admitting = func(downloadStart) {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	recorder.mu.Unlock()
	return entered, release
}

// reportAsync sends a snapshot from a goroutine of its own, as the
// launcher's reports arrive, and answers a channel closed once the page's
// accepted reports are applied.
func reportAsync(t *testing.T, engine *hostedEngine, pageID string, report webview2host.DownloadReport) chan struct{} {
	t.Helper()
	detail, err := report.Encode()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.Report(ids.PageHandle(pageID), webview2host.ReportDownload, detail)
		if !reportsProcessed(engine, pageID) {
			t.Errorf("the reports of page %s were not processed", pageID)
		}
	}()
	return done
}

// A completion that arrives while the download's start is being admitted
// waits for the admission and then completes the row; a running snapshot
// still on its way after it changes nothing.
func TestHostedEngineACompletionWaitsForItsStartsAdmission(t *testing.T) {
	engine, sink, recorder := newDownloadHostedEngine(t)
	engine.downloadQuiet = 10 * time.Millisecond
	bindTestPage(t, engine, "page1", "TARGET-1")
	entered, release := heldAdmission(recorder)
	first := reportAsync(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", Seq: 1, State: webview2host.DownloadRunning, Path: testDownloadPath})
	awaitClosed(t, entered, "the admission")
	end := reportAsync(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", Seq: 3, State: webview2host.DownloadCompleted, Path: testDownloadPath, Received: 100})
	time.Sleep(50 * time.Millisecond)
	if _, progress := recorder.take(); len(progress) != 0 {
		t.Fatalf("progress reached the Manager before the start was admitted: %+v", progress)
	}
	// Nor is it asked about: the Manager does not have it yet.
	if asked := sink.drain(); len(asked) != 0 {
		t.Fatalf("a download whose admission is in flight was asked about: %+v", asked)
	}
	close(release)
	awaitClosed(t, first, "the first snapshot's apply")
	awaitClosed(t, end, "the completion's apply")
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", Seq: 2, State: webview2host.DownloadRunning, Path: testDownloadPath, Received: 40})
	starts, progress := recorder.take()
	if len(starts) != 1 || len(progress) != 1 || progress[0].State != downloadCompleted {
		t.Fatalf("starts %+v and progress %+v, want the row started and completed", starts, progress)
	}
}

// The Manager lists a download's row before its admission answers, so the
// user can cancel it then: the engine takes the cancel and sends it to the
// launcher once the admission is done, and the launcher's answer ends the
// row. Before, the cancel answered that the download had ended, which the
// Manager took as settled, and the file kept downloading.
func TestHostedEngineACancelDuringAdmissionReachesTheLauncher(t *testing.T) {
	engine, sink, recorder := newDownloadHostedEngine(t)
	bindTestPage(t, engine, "page1", "TARGET-1")
	entered, release := heldAdmission(recorder)
	first := reportAsync(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", Seq: 1, State: webview2host.DownloadRunning, Path: testDownloadPath, Received: 10})
	awaitClosed(t, entered, "the admission")
	if err := testProfile(t, engine).CancelDownload("dl-a-1"); err != nil {
		t.Fatalf("a cancel of a download being admitted answers %v", err)
	}
	if sent := sink.drain(); len(sent) != 0 {
		t.Fatalf("the cancel reached the launcher before the Manager had the download: %+v", sent)
	}
	close(release)
	awaitClosed(t, first, "the first snapshot's apply")
	if directive := sink.next(t); !isCancelOf(directive, "page1", "dl-a-1") {
		t.Fatalf("after the admission the engine sent %+v, want the cancel", directive)
	}
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", Seq: 2, State: webview2host.DownloadInterrupted, End: webview2host.DownloadEndCanceled, Reason: "canceled", Answer: true})
	if _, progress := recorder.take(); terminals(progress) != 1 {
		t.Fatalf("progress %+v, want the row ended by the launcher's answer", progress)
	}
}

// Snapshots that arrive out of order settle as the launcher took them: the
// completion first starts and completes the row, and the older running
// snapshots after it change nothing; among running ones, an older one after
// a newer one changes nothing either, and a repeat of one is nothing new.
func TestHostedEngineAppliesOnlyNewerSnapshots(t *testing.T) {
	engine, _, recorder := newDownloadHostedEngine(t)
	bindTestPage(t, engine, "page1", "TARGET-1")
	running := func(id string, seq uint64, received int64) webview2host.DownloadReport {
		return webview2host.DownloadReport{ID: id, Seq: seq, State: webview2host.DownloadRunning, Path: testDownloadPath, Received: received}
	}
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", Seq: 3, State: webview2host.DownloadCompleted, Path: testDownloadPath, Received: 100})
	reportDownload(t, engine, "page1", running("dl-a-1", 1, 0))
	reportDownload(t, engine, "page1", running("dl-a-1", 2, 40))
	starts, progress := recorder.take()
	if len(starts) != 1 || len(progress) != 1 || progress[0].State != downloadCompleted {
		t.Fatalf("reversed, starts %+v and progress %+v, want one row completed", starts, progress)
	}

	reportDownload(t, engine, "page1", running("dl-a-2", 1, 10))
	reportDownload(t, engine, "page1", running("dl-a-2", 3, 30))
	reportDownload(t, engine, "page1", running("dl-a-2", 2, 20))
	reportDownload(t, engine, "page1", running("dl-a-2", 3, 30))
	_, progress = recorder.take()
	var received []float64
	for _, p := range progress {
		received = append(received, p.Received)
	}
	if fmt.Sprint(received) != "[10 30]" {
		t.Fatalf("running snapshots applied %v, want [10 30]", received)
	}
}

// A download the Manager refuses stays refused whatever arrives while it
// was being refused: its terminal snapshot creates no row.
func TestHostedEngineARefusedDownloadsEndCreatesNoRow(t *testing.T) {
	engine, _, recorder := newDownloadHostedEngine(t)
	recorder.refuse = true
	bindTestPage(t, engine, "page1", "TARGET-1")
	entered, release := heldAdmission(recorder)
	first := reportAsync(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", Seq: 1, State: webview2host.DownloadRunning, Path: testDownloadPath})
	awaitClosed(t, entered, "the admission")
	end := reportAsync(t, engine, "page1", webview2host.DownloadReport{ID: "dl-a-1", Seq: 2, State: webview2host.DownloadCompleted, Path: testDownloadPath, Received: 100})
	// The end reaches the engine and waits its turn.
	time.Sleep(50 * time.Millisecond)
	close(release)
	awaitClosed(t, first, "the first snapshot's apply")
	awaitClosed(t, end, "the end's apply")
	if starts, progress := recorder.take(); len(starts) != 1 || len(progress) != 0 {
		t.Fatalf("starts %+v and progress %+v, want the one refused start", starts, progress)
	}
}

// The launcher's resume of a download keeps its id, so its later snapshots,
// naming the file it ends in, are the same row; a resume with nothing to
// continue and a fresh second download of the same address each come with
// an id of their own and are rows of their own.
func TestHostedEngineRowsFollowTheLaunchersIDs(t *testing.T) {
	engine, _, recorder := newDownloadHostedEngine(t)
	bindTestPage(t, engine, "page1", "TARGET-1")
	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadRunning, 64))
	// Resumed after a drop: same id, bytes back to 0 when it restarts.
	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadRunning, 0))
	// A fresh download of the same address.
	second := snapshot("dl-a-2", webview2host.DownloadRunning, 0)
	second.Path, second.Name = `C:\Users\dev\Downloads\report (2).pdf`, "report (2).pdf"
	reportDownload(t, engine, "page1", second)
	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadCompleted, 100))
	// A resume the launcher had no running download for.
	reportDownload(t, engine, "page1", snapshot("dl-a-3", webview2host.DownloadRunning, 64))
	starts, progress := recorder.take()
	var ids []string
	for _, start := range starts {
		ids = append(ids, string(start.ID)+"="+start.SuggestedName)
	}
	if got := strings.Join(ids, " "); got != "dl-a-1=report (1).pdf dl-a-2=report (2).pdf dl-a-3=report (1).pdf" {
		t.Fatalf("rows %s", got)
	}
	if terminals(progress) != 1 {
		t.Fatalf("progress %+v, want the one end", progress)
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

// The Manager's refusal of a download is the launcher's cancel of it, and
// the row never comes back; a terminal first snapshot it refuses needs no
// cancel. A download reported for a page the engine does not hold reaches
// nobody.
func TestHostedEngineCancelsADownloadTheManagerRefuses(t *testing.T) {
	engine, sink, recorder := newDownloadHostedEngine(t)
	recorder.refuse = true
	bindTestPage(t, engine, "page1", "TARGET-1")

	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadRunning, 0))
	directive := sink.next(t)
	if !isCancelOf(directive, "page1", "dl-a-1") {
		t.Fatalf("the refusal sent %+v, want the launcher's cancel of the download", directive)
	}
	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadInterrupted, 0))
	reportDownload(t, engine, "page1", snapshot("dl-a-2", webview2host.DownloadCompleted, 100))
	reportDownload(t, engine, "page9", snapshot("dl-a-3", webview2host.DownloadRunning, 0))
	if starts, progress := recorder.take(); len(starts) != 2 || len(progress) != 0 {
		t.Fatalf("starts %+v and progress %+v, want the two refused starts alone", starts, progress)
	}
	sink.expectOps(t, webview2host.OpCancelDownload)
}

// The user's cancel is the launcher's: the engine names the download's page
// and leaves the download to the launcher's answer.
func TestHostedEngineCancelAsksTheLauncher(t *testing.T) {
	engine, sink, _ := newDownloadHostedEngine(t)
	bindTestPage(t, engine, "page1", "TARGET-1")
	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadRunning, 0))
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
	if len(progress) != 2 || progress[0].Received != 3 || progress[1] != (downloadProgress{ID: "dl-a-1", State: downloadPageGone, Error: popupDiscardedReason}) {
		t.Fatalf("progress %+v, want the one progress and the end of the close, none of the launcher's late report", progress)
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
			recorder.mu.Lock()
			recorder.progressing = func(progress downloadProgress) {
				if progress.ID == "dl-1" && progress.State == downloadCanceled {
					// On the popup's lane, running its close: the report is
					// accepted or refused, never waited for.
					once.Do(func() { sendDownload(t, engine, "popup", snapshot("dl-2", webview2host.DownloadRunning, 10)) })
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
