package browser

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/webview2host"
)

// The hosted engine's downloads are the launcher's snapshots (download.go
// in internal/webview2host), applied idempotently into the Manager's
// download events here, and the Manager's cancel and the engine's status
// question turned into the launcher's directives. Nothing in these tests
// crosses CDP.

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
		// So was its newest snapshot.
		if starts[0].NewestCounts == nil {
			t.Fatal("the start reports no counts its engine received")
		}
		if received, total := starts[0].NewestCounts(); received != 100 || total != 100 {
			t.Fatalf("the start reports %d of %d bytes received, want the newest snapshot's 100 of 100", received, total)
		}
		starts[0].NewestCounts = nil
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
