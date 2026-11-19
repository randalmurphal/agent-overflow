//go:build windows

package webview2host

import "testing"

// What the launcher does with a download it could not read in full: an
// address the runtime did not answer.

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
	r.host.answerDownload(r.page, "dl-t-1", true)
	r.take()
	op.received = 100
	op.st = downloadStateCompleted
	op.stateErr = true
	r.timers[0]()
	if got := r.take(); len(got) != 0 || r.page.downloads["dl-t-1"] == nil || len(r.timers) != 2 {
		t.Fatalf("an unread state at the bound reported %+v and armed %d bounds", got, len(r.timers))
	}
	op.stateErr = false
	r.host.answerDownload(r.page, "dl-t-1", false)
	if got := r.take(); states(got) != "dl-t-1:completed" || got[0].Received != 100 {
		t.Fatalf("the status after the unread bound answered %+v", got)
	}
	r.timers[1]()
	if got := r.take(); len(got) != 0 {
		t.Fatalf("the bound after the end reported %+v", got)
	}

	stuck := newFakeOp(`C:\D\g.bin`)
	start(t, r.page, stuck)
	r.host.answerDownload(r.page, "dl-t-2", true)
	r.take()
	stuck.stateErr = true
	r.timers[2]()
	stuck.stateErr = false
	r.timers[3]()
	if got := r.take(); states(got) != "dl-t-2:interrupted" || got[0].Reason != downloadInterruptReason(downloadUserCanceled) {
		t.Fatalf("a download still running at the next bound ended %+v", got)
	}
	r.done(t, op, stuck)
}
