//go:build windows

package webview2host

import (
	"fmt"
	"testing"
)

// What the launcher does with a download it could not read in full, whose
// state change never arrived, or whose report or directive was lost: an
// address the runtime did not answer, a state it did not answer at the
// cancel bound, an end it never raised before a popup goes, and a cancel's
// refusal or the cancel itself lost on the way.

// refused reports whether a snapshot carries a refusal of its cancel.
func refused(r DownloadReport) bool {
	_, ok := r.Cancel.Refusal()
	return ok
}

// refusal is a snapshot's refusal of its cancel, "" for none.
func refusal(r DownloadReport) string {
	why, _ := r.Cancel.Refusal()
	return why
}

// A cancel's refusal rides every later snapshot of the download, progress,
// a status's answer and its end, so one lost answer does not leave the
// backend's cancel standing. A status naming a cancel the launcher never
// got makes it, once; the refusal of the earlier one no longer stands.
func TestACancelRefusalRidesEveryLaterSnapshot(t *testing.T) {
	r := newDownloadRig(t)
	op := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, op)
	op.refuseCancel = true
	r.host.answerDownload(r.page, "dl-t-1", 1)
	r.take() // the refusal's answer, lost on the way
	op.received = 50
	op.fire()
	r.host.answerDownload(r.page, "dl-t-1", 1) // a repeat of cancel 1
	r.host.answerDownload(r.page, "dl-t-1", 0)
	later := r.take()
	if states(later) != "dl-t-1:running dl-t-1:running dl-t-1:running" || op.canceled != 1 {
		t.Fatalf("after the refusal: %+v, canceled %d times", later, op.canceled)
	}
	for _, report := range later {
		if !refused(report) || report.Cancel.Attempt() != 1 || report.Reason != "" {
			t.Fatalf("a snapshot after the refusal does not carry it: %+v", report)
		}
	}
	op.refuseCancel = false
	r.host.answerDownload(r.page, "dl-t-1", 2) // a status after cancel 2 was lost
	r.host.answerDownload(r.page, "dl-t-1", 2)
	got := r.take()
	if states(got) != "dl-t-1:running dl-t-1:running" || op.canceled != 2 || len(r.timers) != 1 {
		t.Fatalf("a status naming a lost cancel answered %+v, canceled %d times, %d bounds", got, op.canceled, len(r.timers))
	}
	if got[0].Cancel.Attempt() != 2 || refused(got[0]) {
		t.Fatalf("the taken cancel answers %+v", got[0])
	}
	op.set(downloadStateInterrupted, downloadUserCanceled)
	if end := r.take(); states(end) != "dl-t-1:interrupted" || end[0].Cancel.Attempt() != 2 || refused(end[0]) {
		t.Fatalf("the cancel ended %+v", end)
	}

	failed := newFakeOp(`C:\D\g.bin`)
	failed.uri = "https://example.test/g"
	failed.refuseCancel = true
	start(t, r.page, failed)
	r.host.answerDownload(r.page, "dl-t-2", 1)
	r.take()
	failed.set(downloadStateInterrupted, 12)
	if end := r.take(); states(end) != "dl-t-2:interrupted" || !refused(end[0]) || end[0].Reason != downloadInterruptReason(12) {
		t.Fatalf("a failure after a refused cancel ended %+v", end)
	}
	r.done(t, op, failed)
}

// A resume whose address cannot be read is never matched to a dropped
// download whose address could not be read either: both read "", and a
// digest of that would join them and set the resume to the dropped
// download's file. It is a row of its own saving where WebView2 chose.
func TestAnUnreadAddressMatchesNoResume(t *testing.T) {
	r := newDownloadRig(t)
	dropped := newFakeOp(`C:\D\a.bin`)
	dropped.uriErr = true
	start(t, r.page, dropped)
	drop(dropped)
	late := newFakeOp(`C:\D\b.bin`)
	late.uriErr = true
	if args := resume(t, r.page, late); args.put != "" {
		t.Fatalf("a resume of an unread address was set to %q", args.put)
	}
	untouched(t, r, "dl-t-1", dropped, 0, `C:\D\a.bin`)
	reports := r.take()
	if states(reports) != "dl-t-1:running dl-t-2:running" || reports[1].Path != `C:\D\b.bin` || reports[1].URI != "" {
		t.Fatalf("reports %+v", reports)
	}
	r.done(t, dropped, late)
}

// A cancel bound that cannot read the download's state does not end it
// canceled: it may have finished with no state change raised. It stays
// running, and the bound waits again. The status read that succeeds ends
// it as it is, and the later bound does nothing; a download still running
// at a later bound ends canceled then.
func TestACancelBoundThatCannotReadTheStateKeepsTheDownload(t *testing.T) {
	r := newDownloadRig(t)
	op := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, op)
	r.host.answerDownload(r.page, "dl-t-1", 1)
	r.take()
	op.received = 100
	op.st = downloadStateCompleted
	op.stateErr = true
	r.timers[0]()
	if got := r.take(); len(got) != 0 || r.page.downloads["dl-t-1"] == nil || len(r.timers) != 2 {
		t.Fatalf("an unread state at the bound reported %+v and armed %d bounds", got, len(r.timers))
	}
	op.stateErr = false
	r.host.answerDownload(r.page, "dl-t-1", 0)
	if got := r.take(); states(got) != "dl-t-1:completed" || got[0].Received != 100 {
		t.Fatalf("the status after the unread bound answered %+v", got)
	}
	r.timers[1]()
	if got := r.take(); len(got) != 0 {
		t.Fatalf("the bound after the end reported %+v", got)
	}

	stuck := newFakeOp(`C:\D\g.bin`)
	start(t, r.page, stuck)
	r.host.answerDownload(r.page, "dl-t-2", 1)
	r.take()
	stuck.stateErr = true
	r.timers[2]()
	stuck.stateErr = false
	r.timers[3]()
	if got := r.take(); states(got) != "dl-t-2:interrupted" || got[0].Reason != downloadCancelBoundReason {
		t.Fatalf("a download still running at the next bound ended %+v", got)
	}
	r.done(t, op, stuck)
}

// An unannounced popup that goes with its opener reads each download's
// state before it ends the rest as gone: one WebView2 finished without its
// state change is reported completed, with its file, and only the one
// still running is ended by the popup's going.
func TestAPopupGoneWithItsOpenerReadsItsDownloadsFirst(t *testing.T) {
	r := newPopupRig(t)
	openerPage, opener := r.createPage("opener", "prof", false)
	popup, controller := r.openPopup(opener, newFakeWindowArgs(popupURL))
	popup.popup.held.findUnavailable = false
	finished, running := newFakeOp(`C:\D\done.bin`), newFakeOp(`C:\D\running.bin`)
	finished.refuseStateSub = true
	popup.downloads = map[string]*hostDownload{}
	for i, op := range []*fakeOp{finished, running} {
		d := &hostDownload{id: fmt.Sprintf("dl-%d", i+1), page: popup, path: op.path, uri: readDownloadAddress(op.uri, nil)}
		op.refs++ // the reference DownloadStarting hands over
		r.host.attachOperation(d, op.com())
		popup.downloads[d.id] = d
		r.host.report(popup, d.snapshot())
	}
	finished.received, finished.st = 100, downloadStateCompleted
	r.host.Apply(Directive{Op: OpClose, PageID: openerPage.id})
	got := r.awaitReports(5)
	sameReports(t, "an opener closing with a popup whose download finished unseen", kinds(t, got),
		popup.id+":popup(opener,)", popup.id+":download(dl-1,completed)", popup.id+":download(dl-2,interrupted)", popup.id+":closed", "opener:closed")
	if done, _ := ParseDownloadReport(got[1].detail); done.Path != `C:\D\done.bin` || done.Received != 100 {
		t.Errorf("the finished download was reported %+v", done)
	}
	if gone, _ := ParseDownloadReport(got[2].detail); gone.Reason != PopupGoneReason {
		t.Errorf("the running download ended with %q", gone.Reason)
	}
	for _, op := range []*fakeOp{finished, running} {
		if op.refs != 1 || len(op.subs) != 0 {
			t.Errorf("an operation kept %d references beyond WebView2's and %d subscriptions", op.refs-1, len(op.subs))
		}
	}
	if finished.canceled != 0 || running.canceled != 1 {
		t.Errorf("cancels: finished %d, running %d", finished.canceled, running.canceled)
	}
	closed(t, controller)
	closed(t, opener)
	r.done()
}
