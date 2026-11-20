package browser

import (
	"testing"
	"time"

	"agent-overflow/internal/webview2host"
)

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
	awaitQueued(t, engine, "page1", "dl-a-1", 100)
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
	awaitQueued(t, engine, "page1", "dl-a-1", 100)
	close(release)
	awaitClosed(t, first, "the first snapshot's apply")
	awaitClosed(t, end, "the end's apply")
	if starts, progress := recorder.take(); len(starts) != 1 || len(progress) != 0 {
		t.Fatalf("starts %+v and progress %+v, want the one refused start", starts, progress)
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
