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
