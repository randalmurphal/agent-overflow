//go:build windows

package webview2host

import (
	"fmt"
	"strings"
	"testing"
	"unsafe"
)

// The launcher's download bookkeeping against a stand-in DownloadOperation
// and DownloadStarting args: the snapshots, the answers to directives, the
// resume rule, and the lifetime of every operation
// reference, subscription and handler.

// Every report is the download's whole snapshot: its address, name and
// file and bytes, from the first on, numbered from 1 in order.
func TestALauncherDownloadReportsSnapshotsAndLetsItsOperationGo(t *testing.T) {
	r := newDownloadRig(t)
	op := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, op)
	op.received = 40
	op.fire()
	op.fire() // within the progress interval: held
	op.received = 100
	op.set(downloadStateCompleted, 0)
	reports := r.take()
	if got := states(reports); got != "dl-t-1:running dl-t-1:running dl-t-1:completed" {
		t.Fatalf("reports %s", got)
	}
	for i, report := range reports {
		if report.URI != "https://example.test/f" || report.Name != "f.bin" || report.Path != `C:\D\f.bin` || report.Total != 100 {
			t.Fatalf("report %d is not a whole snapshot: %+v", i, report)
		}
		if report.Seq != uint64(i+1) || report.Answer {
			t.Fatalf("report %d is numbered %d (answer %v)", i, report.Seq, report.Answer)
		}
	}
	if last := reports[2]; last.Received != 100 {
		t.Fatalf("completion %+v", last)
	}
	r.done(t, op)
}

// A byte count or file the runtime does not answer keeps the last one
// read, and the failure is logged: a failed read is never reported as no
// bytes or no file.
func TestADownloadReadThatFailsKeepsTheLastValues(t *testing.T) {
	r := newDownloadRig(t)
	var failed []string
	r.host.config.Logf = func(format string, a ...any) {
		if line := fmt.Sprintf(format, a...); strings.Contains(line, "read its") {
			failed = append(failed, line)
		}
	}
	op := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, op)
	op.received = 40
	r.host.answerDownload(r.page, "dl-t-1", 0)
	op.readErr = true
	op.received, op.total, op.path = 90, 200, `C:\D\other.bin`
	op.set(downloadStateCompleted, 0)
	reports := r.take()
	last := reports[len(reports)-1]
	if last.State != DownloadCompleted || last.Received != 40 || last.Total != 100 || last.Path != `C:\D\f.bin` {
		t.Fatalf("a completion whose reads failed reports %+v, want the last values read", last)
	}
	if len(failed) != 3 {
		t.Fatalf("logged %q, want each failed read", failed)
	}
	r.done(t, op)
}

// A directive naming a download is answered with a snapshot marked as an
// answer: a running one's, numbered after the last (and a cancel cancels
// it, its end following from the operation), an ended one's terminal
// snapshot again under its own number, and an unknown id "unknown". A
// repeated terminal state and a late progress event after the end reach
// nobody.
func TestADirectiveIsAnsweredWithTheDownloadsSnapshot(t *testing.T) {
	r := newDownloadRig(t)
	op := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, op)
	r.take()

	r.host.answerDownload(r.page, "dl-t-1", 0)
	if got := r.take(); states(got) != "dl-t-1:running" || op.canceled != 0 || got[0].Seq != 2 || !got[0].Answer {
		t.Fatalf("a status of a running download answers %+v and cancels %d times", got, op.canceled)
	}
	r.host.answerDownload(r.page, "dl-t-1", 1)
	if got := r.take(); states(got) != "dl-t-1:running" || op.canceled != 1 || got[0].Seq != 3 || !got[0].Answer {
		t.Fatalf("a cancel answers %+v and cancels %d times", got, op.canceled)
	}
	op.set(downloadStateInterrupted, downloadUserCanceled)
	terminal := r.take()
	if states(terminal) != "dl-t-1:interrupted" || terminal[0].Reason != "the download was canceled" || terminal[0].Seq != 4 || terminal[0].Answer {
		t.Fatalf("the cancel ended as %+v", terminal)
	}
	// Ended: the same terminal snapshot again, as an answer, and nothing
	// canceled.
	r.host.answerDownload(r.page, "dl-t-1", 1)
	r.host.answerDownload(r.page, "dl-t-1", 0)
	again := terminal[0]
	again.Answer = true
	if got := r.take(); len(got) != 2 || got[0] != again || got[1] != again || op.canceled != 1 {
		t.Fatalf("an ended download answers %+v (canceled %d times)", got, op.canceled)
	}
	// A repeat of the end, and a late progress event, reach nobody.
	op.received = 100
	op.set(downloadStateInterrupted, downloadUserCanceled)
	op.fire()
	if got := r.take(); len(got) != 0 {
		t.Fatalf("after the end the operation reported %+v", got)
	}
	r.host.answerDownload(r.page, "dl-t-9", 1)
	if got := r.take(); len(got) != 1 || got[0] != (DownloadReport{ID: "dl-t-9", State: DownloadUnknown, Reason: DownloadUnknownReason, Answer: true}) {
		t.Fatalf("an unknown id answers %+v", got)
	}
	r.done(t, op)
}

// A download whose state subscription failed still ends: a status
// directive reads its state, and one that completed with no state change
// raised ends completed, its terminal snapshot the answer. A state that
// cannot be read keeps it running.
func TestAStatusReconcilesADownloadWithNoStateCallback(t *testing.T) {
	r := newDownloadRig(t)
	op := newFakeOp(`C:\D\f.bin`)
	op.refuseStateSub = true
	start(t, r.page, op)
	r.take()
	op.stateErr = true
	r.host.answerDownload(r.page, "dl-t-1", 0)
	if got := r.take(); states(got) != "dl-t-1:running" || !got[0].Answer {
		t.Fatalf("an unreadable state answers %+v", got)
	}
	op.stateErr = false
	op.received = 100
	op.st = downloadStateCompleted
	r.host.answerDownload(r.page, "dl-t-1", 0)
	got := r.take()
	if states(got) != "dl-t-1:completed" || got[0].Received != 100 {
		t.Fatalf("a completed download with no callback answers %+v", got)
	}
	if r.page.downloads["dl-t-1"] != nil || op.refs != 1 {
		t.Fatalf("the reconciled download is still tracked (%d refs)", op.refs-1)
	}
	r.host.answerDownload(r.page, "dl-t-1", 0)
	if again := r.take(); len(again) != 1 || !again[0].Answer || again[0].State != DownloadCompleted {
		t.Fatalf("after the reconciled end: %+v", again)
	}
	r.done(t, op)
}

// A page asking to close while its one download completed unseen closes:
// the close reads the download's state.
func TestACloseRequestReconcilesItsDownloads(t *testing.T) {
	r := newDownloadRig(t)
	var posted []func()
	r.host.config.Post = func(fn func()) { posted = append(posted, fn) }
	op := newFakeOp(`C:\D\f.bin`)
	op.refuseStateSub = true
	start(t, r.page, op)
	r.take()
	op.st = downloadStateCompleted
	downloads := r.host.config.Report
	var closed int
	r.host.config.Report = func(pageID string, kind ReportKind, detail string) {
		if kind == ReportClosed && pageID == "p" {
			closed++
			return
		}
		downloads(pageID, kind, detail)
	}
	r.host.closeAsked(r.page)
	if len(posted) != 1 {
		t.Fatalf("%d turns posted", len(posted))
	}
	posted[0]()
	if got := states(r.take()); got != "dl-t-1:completed" {
		t.Fatalf("the close reconciled %q", got)
	}
	if r.page.closeAfterDownloads || r.host.pages["p"] != nil || closed != 1 {
		t.Fatalf("the page waits for a download that ended (closeAfterDownloads %v, closed %d)", r.page.closeAfterDownloads, closed)
	}
	r.done(t, op)
}

// A page keeps at most MaxEndedDownloads terminal snapshots; the oldest
// past that answers as unknown. A page that goes takes them all.
func TestAPageKeepsBoundedEndedSnapshotsUntilItGoes(t *testing.T) {
	r := newDownloadRig(t)
	var ops []*fakeOp
	for i := range MaxEndedDownloads + 1 {
		op := newFakeOp(fmt.Sprintf(`C:\D\f%d.bin`, i))
		start(t, r.page, op)
		op.set(downloadStateCompleted, 0)
		ops = append(ops, op)
	}
	r.take()
	r.host.answerDownload(r.page, "dl-t-1", 0)
	r.host.answerDownload(r.page, "dl-t-2", 0)
	got := r.take()
	if got[0].Reason != DownloadUnknownReason || got[1].State != DownloadCompleted {
		t.Fatalf("the oldest answers %+v and the next %+v", got[0], got[1])
	}
	r.done(t, ops...)
	r.host.answerDownload(r.page, "dl-t-2", 0)
	if got := r.take(); got[0].Reason != DownloadUnknownReason {
		t.Fatalf("after the page went an ended download answers %+v", got[0])
	}
}

// An interruption ends the download at once with its reason, and the
// operation is canceled before it is let go, unless the interruption was
// the cancel itself. A dropped connection ends with the length mismatch
// (24) on the real runtime; FILE_TOO_SHORT, FILE_HASH_MISMATCH and
// SERVER_NO_RANGE, which WebView2 documents as resumed by itself, never
// arrived there for a dropped connection, a short body or a refused Range.
func TestAnInterruptionEndsTheDownload(t *testing.T) {
	r := newDownloadRig(t)
	op := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, op)
	op.set(downloadStateInterrupted, 24)
	reports := r.take()
	if states(reports) != "dl-t-1:running dl-t-1:interrupted" || reports[1].Reason != "the server's response was a different length than it announced" || op.canceled != 1 {
		t.Fatalf("reports %+v, canceled %d times", reports, op.canceled)
	}
	canceled := newFakeOp(`C:\D\g.bin`)
	start(t, r.page, canceled)
	canceled.set(downloadStateInterrupted, downloadUserCanceled)
	if canceled.canceled != 0 {
		t.Fatal("a canceled download was canceled again")
	}
	r.done(t, op, canceled)
}

// A page that goes cancels its running downloads, reports nothing for them,
// and lets every operation and handler go; its DownloadStarting handler
// takes no later download.
func TestAClosingPageCancelsAndReleasesItsDownloads(t *testing.T) {
	r := newDownloadRig(t)
	op := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, op)
	replaced := newFakeOp(`C:\D\g.bin`)
	replaced.uri = "https://example.test/g"
	start(t, r.page, replaced)
	drop(replaced)
	resumed := newFakeOp(`C:\D\g.bin`)
	resumed.uri = replaced.uri
	resume(t, r.page, resumed)
	handler := r.page.downloadStarting
	r.take()
	r.done(t, op, replaced, resumed)
	if op.canceled != 1 || resumed.canceled != 1 {
		t.Fatalf("the running downloads were canceled %d and %d times", op.canceled, resumed.canceled)
	}
	late := newFakeOp(`C:\D\h.bin`)
	args := &fakeStartingArgs{vtbl: &fakeStartingArgsVtbl, op: late}
	_, _, _ = downloadStartingVtbl.Invoke.Call(uintptr(unsafe.Pointer(handler)), 0, uintptr(unsafe.Pointer(args)))
	if args.handled || late.refs != 1 || len(r.take()) != 0 {
		t.Fatal("a closed page reported or took a download")
	}
}

// A canceled download WebView2 does not end (a dropped one, whose resume
// went to a row of its own) ends untracked when the bound passes: one
// terminal snapshot, its operation let go, and a state change after that
// reaches nobody. A repeated cancel starts no second bound.
func TestACanceledDownloadWebView2DoesNotEndSettlesAtTheBound(t *testing.T) {
	r := newDownloadRig(t)
	op := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, op)
	drop(op)
	r.take()
	r.host.answerDownload(r.page, "dl-t-1", 1)
	r.host.answerDownload(r.page, "dl-t-1", 2)
	if got := r.take(); states(got) != "dl-t-1:running dl-t-1:running" || len(r.timers) != 1 {
		t.Fatalf("two cancels answered %s and started %d bounds", states(got), len(r.timers))
	}
	r.timers[0]()
	end := r.take()
	if states(end) != "dl-t-1:interrupted" || end[0].Reason != downloadCancelBoundReason || end[0].End != DownloadEndUntracked || end[0].Answer {
		t.Fatalf("at the bound the download ended %+v", end)
	}
	if op.refs != 1 || len(op.subs) != 0 {
		t.Fatalf("the settled download kept its operation: %d refs, %d subscriptions", op.refs, len(op.subs))
	}
	op.received = 100
	op.set(downloadStateCompleted, 0)
	op.fire()
	r.host.answerDownload(r.page, "dl-t-1", 0)
	if got := r.take(); len(got) != 1 || !got[0].Answer || got[0].State != DownloadInterrupted {
		t.Fatalf("after the settled end: %+v", got)
	}
	r.done(t, op)
}

// A cancel WebView2 refuses is answered with the refusal and starts no
// bound: the download runs on, is not ended as canceled, and its own end
// is reported. A later cancel WebView2 takes starts the bound.
func TestARefusedCancelKeepsTheDownloadRunning(t *testing.T) {
	r := newDownloadRig(t)
	op := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, op)
	r.take()
	op.refuseCancel = true
	r.host.answerDownload(r.page, "dl-t-1", 1)
	got := r.take()
	if states(got) != "dl-t-1:running" || !refused(got[0]) ||
		!strings.HasPrefix(refusal(got[0]), DownloadCancelRefusedReason) || got[0].Cancel.Attempt() != 1 || len(r.timers) != 0 {
		t.Fatalf("a refused cancel answered %+v and started %d bounds", got, len(r.timers))
	}
	if d := r.page.downloads["dl-t-1"]; d == nil || d.cancelRequested {
		t.Fatal("a refused cancel is still requested or the download is gone")
	}
	op.refuseCancel = false
	r.host.answerDownload(r.page, "dl-t-1", 2)
	if got := r.take(); states(got) != "dl-t-1:running" || refused(got[0]) || len(r.timers) != 1 {
		t.Fatalf("a cancel taken answered %+v and started %d bounds", got, len(r.timers))
	}
	op.refuseCancel = true
	// A resume of a canceled download whose cancel is refused withdraws it,
	// and the bound the first cancel started ends nothing.
	drop(op)
	ofOp := newFakeOp(`C:\D\f (1).bin`)
	ofOp.refuseCancel = true
	resume(t, r.page, ofOp)
	if got := r.take(); states(got) != "dl-t-1:running" || !refused(got[0]) {
		t.Fatalf("a resume whose cancel was refused reported %+v", got)
	}
	r.timers[0]()
	if got := r.take(); len(got) != 0 {
		t.Fatalf("the bound of a withdrawn cancel ended the download: %+v", got)
	}
	ofOp.received = 100
	ofOp.set(downloadStateCompleted, 0)
	if got := r.take(); states(got) != "dl-t-1:completed" {
		t.Fatalf("the download that ran on ended %+v", got)
	}
	r.done(t, op, ofOp)
}

// A canceled download that completed with no state change raised ends as
// it did when the bound passes, not as canceled.
func TestACancelBoundReadsTheDownloadsState(t *testing.T) {
	r := newDownloadRig(t)
	op := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, op)
	r.host.answerDownload(r.page, "dl-t-1", 1)
	r.take()
	op.received = 100
	op.st = downloadStateCompleted
	r.timers[0]()
	if got := r.take(); states(got) != "dl-t-1:completed" || got[0].Received != 100 {
		t.Fatalf("at the bound the download ended %+v", got)
	}
	r.done(t, op)
}

// A canceled download WebView2 ends settles by WebView2's own report, once;
// the bound that passes after it does nothing.
func TestACanceledDownloadSettlesByWebView2sReport(t *testing.T) {
	r := newDownloadRig(t)
	op := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, op)
	r.host.answerDownload(r.page, "dl-t-1", 1)
	op.set(downloadStateInterrupted, downloadUserCanceled)
	r.timers[0]()
	if got := states(r.take()); got != "dl-t-1:running dl-t-1:running dl-t-1:interrupted" {
		t.Fatalf("reports %s, want one end", got)
	}
	r.done(t, op)
}

// A bound that passes after the page went does nothing, decided on the UI
// thread.
func TestACancelBoundAfterThePageWentDoesNothing(t *testing.T) {
	r := newDownloadRig(t)
	op := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, op)
	r.host.answerDownload(r.page, "dl-t-1", 1)
	r.take()
	r.done(t, op)
	delete(r.host.pages, "p")
	hops := 0
	r.host.config.OnMain = func(fn func()) { hops++; fn() }
	r.timers[0]()
	if got := r.take(); len(got) != 0 || hops != 1 {
		t.Fatalf("a bound after the page went reported %+v and asked the UI thread %d times, want once", got, hops)
	}
}

// DownloadStarting and the operation's slots are where the SDK header puts
// them.
func TestDownloadSlots(t *testing.T) {
	slot := func(offset uintptr) uintptr { return offset / unsafe.Sizeof(comProc(0)) }
	var view iCoreWebView2_4Vtbl
	if got := slot(unsafe.Offsetof(view.AddDownloadStarting)); got != 75 {
		t.Errorf("add_DownloadStarting is at slot %d, want 75", got)
	}
	var op iCoreWebView2DownloadOperationVtbl
	for name, c := range map[string]struct{ got, want uintptr }{
		"add_BytesReceivedChanged": {slot(unsafe.Offsetof(op.AddBytesReceivedChanged)), 3},
		"add_StateChanged":         {slot(unsafe.Offsetof(op.AddStateChanged)), 7},
		"get_ResultFilePath":       {slot(unsafe.Offsetof(op.GetResultFilePath)), 15},
		"get_InterruptReason":      {slot(unsafe.Offsetof(op.GetInterruptReason)), 17},
		"Cancel":                   {slot(unsafe.Offsetof(op.Cancel)), 18},
		"get_CanResume":            {slot(unsafe.Offsetof(op.GetCanResume)), 21},
	} {
		if c.got != c.want {
			t.Errorf("%s is at slot %d, want %d", name, c.got, c.want)
		}
	}
	var args iDownloadStartingEventArgsVtbl
	if got := slot(unsafe.Offsetof(args.PutHandled)); got != 9 {
		t.Errorf("put_Handled is at slot %d, want 9", got)
	}
}
