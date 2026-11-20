package browser

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/webview2host"
)

// The Manager's cancel of a hosted download against lost reports and
// directives: every snapshot names the launcher's last cancel and its
// refusal, and every status question the engine's last cancel, so either
// side's loss heals on the next exchange.

const testRefusal = webview2host.DownloadCancelRefusedReason + ": Unspecified error"

// refusedSnapshot is a snapshot of id carrying the refusal of cancel
// attempt n.
func refusedSnapshot(id string, n webview2host.CancelAttempt, received int64) webview2host.DownloadReport {
	r := snapshot(id, webview2host.DownloadRunning, received)
	r.Cancel = webview2host.CancelRefused(n, errors.New("Unspecified error"))
	return r
}

// A refusal reaches the Manager on whichever snapshot first carries the
// refusal of the last cancel sent, once: a later one repeating it, or one
// naming an earlier cancel's refusal, withdraws nothing again.
func TestHostedEngineTakesARefusalFromAnySnapshot(t *testing.T) {
	engine, sink, recorder := newDownloadHostedEngine(t)
	bindTestPage(t, engine, "page1", "TARGET-1")
	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadRunning, 10))
	recorder.take()
	if err := engine.cancelDownload("dl-a-1"); err != nil {
		t.Fatal(err)
	}
	if directive := sink.next(t); !isCancelOf(directive, "page1", "dl-a-1") || directive.Cancel != 1 {
		t.Fatalf("the cancel sent %+v", directive)
	}
	// The refusal's answer was lost; the next progress carries it.
	reportDownload(t, engine, "page1", refusedSnapshot("dl-a-1", 1, 20))
	reportDownload(t, engine, "page1", refusedSnapshot("dl-a-1", 1, 30))
	refused := downloadProgress{ID: "dl-a-1", State: downloadCancelRefused, Error: testRefusal}
	_, progress := recorder.take()
	want := []downloadProgress{
		refused,
		{ID: "dl-a-1", Received: 20, Total: 100, State: downloadInProgress},
		{ID: "dl-a-1", Received: 30, Total: 100, State: downloadInProgress},
	}
	if !reflect.DeepEqual(progress, want) {
		t.Fatalf("progress %+v, want %+v", progress, want)
	}
	if err := engine.cancelDownload("dl-a-1"); err != nil {
		t.Fatal(err)
	}
	if directive := sink.next(t); directive.Cancel != 2 {
		t.Fatalf("the second cancel sent %+v", directive)
	}
	reportDownload(t, engine, "page1", refusedSnapshot("dl-a-1", 1, 40))
	reportDownload(t, engine, "page1", refusedSnapshot("dl-a-1", 2, 50))
	_, progress = recorder.take()
	want = []downloadProgress{
		{ID: "dl-a-1", Received: 40, Total: 100, State: downloadInProgress},
		refused,
		{ID: "dl-a-1", Received: 50, Total: 100, State: downloadInProgress},
	}
	if !reflect.DeepEqual(progress, want) {
		t.Fatalf("after the second cancel, progress %+v, want %+v", progress, want)
	}

	// Two cancels sent before any answer: the first's refusal, arriving
	// late, withdraws nothing, since the second stands.
	reportDownload(t, engine, "page1", snapshot("dl-a-2", webview2host.DownloadRunning, 10))
	for range 2 {
		if err := engine.cancelDownload("dl-a-2"); err != nil {
			t.Fatal(err)
		}
	}
	sink.drain()
	recorder.take()
	reportDownload(t, engine, "page1", refusedSnapshot("dl-a-2", 1, 20))
	if _, progress := recorder.take(); len(progress) != 1 || progress[0].State == downloadCancelRefused {
		t.Fatalf("an earlier cancel's refusal reached the Manager: %+v", progress)
	}

	// A cancel that could not be sent takes no number, so no status
	// question makes it later.
	reportDownload(t, engine, "page1", snapshot("dl-a-3", webview2host.DownloadRunning, 10))
	send := engine.send
	engine.send = nil
	err := engine.cancelDownload("dl-a-3")
	engine.send = send
	if err == nil {
		t.Fatal("a cancel with no pane host answered nil")
	}
	if cancels := engine.pages.cancelsOf("page1", "dl-a-3"); cancels != 0 {
		t.Fatalf("a cancel never sent kept number %d", cancels)
	}
}

// A cancel no snapshot has named yet is sent again with each status
// question, whether or not the download reports meanwhile, so one lost on
// the way (the bridge was down) is made; once a snapshot names it, a
// download that reports is asked nothing.
func TestHostedEngineAsksAgainForACancelTheLauncherNeverNamed(t *testing.T) {
	engine, sink, _ := newDownloadHostedEngine(t)
	const quiet = 30 * time.Millisecond
	engine.downloadQuiet = quiet
	bindTestPage(t, engine, "page1", "TARGET-1")
	reportDownload(t, engine, "page1", snapshot("dl-a-1", webview2host.DownloadRunning, 1))
	if err := engine.cancelDownload("dl-a-1"); err != nil {
		t.Fatal(err)
	}
	if directive := sink.next(t); directive.Op != webview2host.OpCancelDownload || directive.Cancel != 1 {
		t.Fatalf("the cancel sent %+v", directive)
	}
	keepReporting := func(cancel webview2host.CancelOutcome) []webview2host.Directive {
		sink.drain()
		for i := range 20 {
			r := snapshot("dl-a-1", webview2host.DownloadRunning, int64(i+2))
			r.Cancel = cancel
			reportDownload(t, engine, "page1", r)
			time.Sleep(quiet / 5)
		}
		return sink.drain()
	}
	asked := keepReporting(webview2host.CancelOutcome{})
	if len(asked) == 0 {
		t.Fatal("a cancel the launcher never named was not asked about again")
	}
	for _, directive := range asked {
		if directive.Op != webview2host.OpDownloadStatus || directive.Cancel != 1 || directive.Validate() != nil {
			t.Fatalf("the engine sent %+v", directive)
		}
	}
	// A question the status loop made before the first snapshot naming the
	// cancel was applied can be sent after the next drain, so the round
	// checked starts once a round of named snapshots has passed.
	keepReporting(webview2host.CancelTaken(1))
	if asked := keepReporting(webview2host.CancelTaken(1)); len(asked) != 0 {
		t.Fatalf("a download whose cancel was named was asked %+v", asked)
	}
}

// A download whose cancel the user made ends as the launcher saw it end,
// whatever was pending: canceled only for the cancel WebView2 took, failed
// with the reason for the network's end, the launcher no longer following
// it, or its losing the download. The row never reads a cancel into an
// end that was not one.
func TestADownloadRowListsTheCauseTheLauncherSaw(t *testing.T) {
	for _, tc := range []struct {
		name      string
		end       func(webview2host.DownloadReport) webview2host.DownloadReport
		wantState string
		wantError string
	}{
		{"the cancel taken", func(r webview2host.DownloadReport) webview2host.DownloadReport {
			r.State, r.End, r.Reason = webview2host.DownloadInterrupted, webview2host.DownloadEndCanceled, "the download was canceled"
			return r
		}, downloadStateCanceled, ""},
		{"the network before the cancel", func(r webview2host.DownloadReport) webview2host.DownloadReport {
			r.State, r.End, r.Reason = webview2host.DownloadInterrupted, webview2host.DownloadEndFailed, "the network failed"
			return r
		}, downloadStateFailed, "the network failed"},
		{"no longer followed", func(r webview2host.DownloadReport) webview2host.DownloadReport {
			r.State, r.End, r.Reason = webview2host.DownloadInterrupted, webview2host.DownloadEndUntracked, "the browser did not end the download after its cancel, so it is no longer followed"
			return r
		}, downloadStateFailed, "the browser did not end the download after its cancel, so it is no longer followed"},
		{"lost by the launcher", func(webview2host.DownloadReport) webview2host.DownloadReport {
			return webview2host.DownloadReport{ID: "dl-1", State: webview2host.DownloadUnknown, Reason: webview2host.DownloadUnknownReason, Answer: true}
		}, downloadStateFailed, webview2host.DownloadUnknownReason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newPopupLauncher(t)
			access := l.access("thread")
			_, page := l.open(access)
			reportDownload(t, l.engine, page, snapshot("dl-1", webview2host.DownloadRunning, 10))
			l.awaitDownloads(access, downloadStateRunning)
			row := l.manager.CompanionState(access.Caller()).Downloads[0]
			if err := l.manager.CancelThreadDownload(access, DownloadKey(row.ID)); err != nil {
				t.Fatal(err)
			}
			end := snapshot("dl-1", webview2host.DownloadRunning, 20)
			end.Cancel = webview2host.CancelTaken(1)
			reportDownload(t, l.engine, page, tc.end(end))
			if got := l.manager.CompanionState(access.Caller()).Downloads[0]; got.State != tc.wantState || got.Error != tc.wantError {
				t.Fatalf("the row is %s %q, want %s %q", got.State, got.Error, tc.wantState, tc.wantError)
			}
		})
	}
}

// Through the Manager: the launcher's refusal of a
// cancel is lost, a status answer carries it, and the user's next cancel
// reaches the launcher instead of being taken for the first still
// standing. A failure after a lost refusal lists as a failure, not as the
// user's cancel.
func TestAManagerCancelAfterALostRefusalReachesTheLauncher(t *testing.T) {
	l := newPopupLauncher(t)
	var mu sync.Mutex
	var cancels []webview2host.Directive
	send := l.engine.send
	l.engine.send = func(directive webview2host.Directive) {
		if directive.Op == webview2host.OpCancelDownload {
			mu.Lock()
			cancels = append(cancels, directive)
			mu.Unlock()
		}
		send(directive)
	}
	sent := func() []webview2host.CancelAttempt {
		mu.Lock()
		defer mu.Unlock()
		var out []webview2host.CancelAttempt
		for _, directive := range cancels {
			out = append(out, directive.Cancel)
		}
		return out
	}
	access := l.access("thread")
	_, page := l.open(access)
	reportDownload(t, l.engine, page, snapshot("dl-1", webview2host.DownloadRunning, 10))
	l.awaitDownloads(access, downloadStateRunning)
	row := l.manager.CompanionState(access.Caller()).Downloads[0]
	if err := l.manager.CancelThreadDownload(access, DownloadKey(row.ID)); err != nil {
		t.Fatal(err)
	}
	// The refusal's answer is lost; the next status answer carries it.
	status := refusedSnapshot("dl-1", 1, 20)
	status.Answer = true
	reportDownload(t, l.engine, page, status)
	if got := l.manager.CompanionState(access.Caller()).Downloads[0]; got.State != downloadStateRunning || got.Error != testRefusal {
		t.Fatalf("after the status the row is %+v", got)
	}
	if err := l.manager.CancelThreadDownload(access, DownloadKey(row.ID)); err != nil {
		t.Fatal(err)
	}
	if got := sent(); !reflect.DeepEqual(got, []webview2host.CancelAttempt{1, 2}) {
		t.Fatalf("cancels sent %v, want the second to reach the launcher", got)
	}
	// A late snapshot still carrying the first cancel's refusal leaves the
	// second standing.
	reportDownload(t, l.engine, page, refusedSnapshot("dl-1", 1, 25))
	if got := l.manager.CompanionState(access.Caller()).Downloads[0]; got.State != downloadStateRunning || got.Error != "" {
		t.Fatalf("the first cancel's refusal after the second made the row %+v", got)
	}
	if err := l.manager.CancelThreadDownload(access, DownloadKey(row.ID)); err != nil {
		t.Fatal(err)
	}
	if got := sent(); len(got) != 2 {
		t.Fatalf("the first cancel's refusal withdrew the second: cancels sent %v", got)
	}

	// The second's refusal is lost too, and the download fails.
	failed := refusedSnapshot("dl-1", 2, 30)
	failed.State, failed.End, failed.Reason = webview2host.DownloadInterrupted, webview2host.DownloadEndFailed, "the network failed"
	reportDownload(t, l.engine, page, failed)
	if got := l.manager.CompanionState(access.Caller()).Downloads[0]; got.State != downloadStateFailed || got.Error != "the network failed" {
		t.Fatalf("a failure after a lost refusal lists as %+v", got)
	}
}
