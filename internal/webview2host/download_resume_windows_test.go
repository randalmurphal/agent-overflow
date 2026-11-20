//go:build windows

package webview2host

import (
	"strings"
	"testing"
	"unsafe"
)

// A download from a long address (a data URL) keeps and reports at most
// maxDownloadURIBytes of it, in every snapshot and the terminal one it
// keeps, and a resume is matched by the whole address: of two dropped
// downloads whose addresses share that prefix, the resume takes its own.
func TestALongAddressIsKeptBoundedAndResumesByTheWholeAddress(t *testing.T) {
	r := newDownloadRig(t)
	prefix := "data:application/octet-stream;base64," + strings.Repeat("QUJD", 2000)
	a := newFakeOp(`C:\\D\\a.bin`)
	a.uri = prefix + "AAAA"
	start(t, r.page, a)
	b := newFakeOp(`C:\\D\\b.bin`)
	b.uri = prefix + "BBBB"
	start(t, r.page, b)
	drop(a)
	drop(b)
	ofB := newFakeOp(`C:\\D\\b (1).bin`)
	ofB.uri = b.uri
	resume(t, r.page, ofB)
	if got := r.page.downloads["dl-t-2"]; got == nil || got.op != ofB.com() {
		t.Fatal("the resume did not take the download of its whole address")
	}
	ofB.received = 100
	ofB.set(downloadStateCompleted, 0)
	reports := r.take()
	if got := states(reports); got != "dl-t-1:running dl-t-2:running dl-t-2:completed" {
		t.Fatalf("reports %s", got)
	}
	for _, report := range reports {
		if len(report.URI) > maxDownloadURIBytes || !strings.HasPrefix(prefix, report.URI) {
			t.Fatalf("a report carries %d bytes of the address, want at most %d", len(report.URI), maxDownloadURIBytes)
		}
	}
	if kept := r.page.ended.byID["dl-t-2"]; len(kept.URI) > maxDownloadURIBytes {
		t.Fatalf("the kept terminal snapshot holds %d bytes of the address", len(kept.URI))
	}
	r.done(t, a, b, ofB)
}

// resume raises DownloadStarting for op as WebView2 resumes a download: op
// is in progress and carries the interruption's reason.
func resume(t *testing.T, page *hostPage, op *fakeOp) *fakeStartingArgs {
	t.Helper()
	op.reason = 24
	args := &fakeStartingArgs{vtbl: &fakeStartingArgsVtbl, op: op}
	_, _, _ = downloadStartingVtbl.Invoke.Call(uintptr(unsafe.Pointer(page.downloadStarting)), 0, uintptr(unsafe.Pointer(args)))
	if !args.handled {
		t.Fatal("a resume was not taken from WebView2's dialog")
	}
	return args
}

// A start carrying an interruption's reason is WebView2 resuming the
// page's running download from that address: the same row, saving to the
// file it started with, one terminal report, the replaced operation kept
// silent until the end and every operation released then. A fresh
// download of the same address meanwhile is a row of its own.
func TestAResumeIsTheSameDownload(t *testing.T) {
	r := newDownloadRig(t)
	first := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, first)
	drop(first)
	// Continued from its bytes: WebView2 names another file.
	continued := newFakeOp(`C:\D\f (1).bin`)
	if args := resume(t, r.page, continued); args.put != `C:\D\f.bin` || continued.path != `C:\D\f.bin` {
		t.Fatalf("the resume saves to %q (put %q), not the download's file", continued.path, args.put)
	}
	if first.refs != 2 || len(first.subs) != 2 {
		t.Fatalf("the replaced operation was let go while its download runs (%d refs, %d subscriptions)", first.refs, len(first.subs))
	}
	first.set(downloadStateInterrupted, 24)
	// Restarted from nothing: WebView2 names the same file.
	restarted := newFakeOp(`C:\D\f.bin`)
	if args := resume(t, r.page, restarted); args.put != "" {
		t.Fatalf("a resume to the download's own file put %q", args.put)
	}
	fresh := newFakeOp(`C:\D\f (1).bin`)
	start(t, r.page, fresh)
	continued.set(downloadStateInterrupted, 24)
	restarted.received = 100
	restarted.set(downloadStateCompleted, 0)
	reports := r.take()
	if got, want := states(reports), "dl-t-1:running dl-t-2:running dl-t-1:completed"; got != want {
		t.Fatalf("reports %s, want %s", got, want)
	}
	if end := reports[2]; end.Path != `C:\D\f.bin` || end.Name != "f.bin" || end.Received != 100 {
		t.Fatalf("the resumed download ended %+v", end)
	}
	for _, op := range []*fakeOp{first, continued, restarted} {
		if op.refs != 1 || len(op.subs) != 0 {
			t.Fatal("an ended download kept an operation")
		}
	}
	r.done(t, first, continued, restarted, fresh)
}

// drop marks op as WebView2 marks a dropped download's operation when it
// starts the download's resume: still in progress, with the reason.
func drop(op *fakeOp) { op.reason = 24 }

// untouched asserts a running download kept its operation, the operations
// it replaced (stale) and its file.
func untouched(t *testing.T, r *downloadRig, id string, op *fakeOp, stale int, path string) {
	t.Helper()
	d := r.page.downloads[id]
	if d == nil || d.op != op.com() || len(d.stale) != stale || d.path != path || op.path != path || op.canceled != 0 {
		t.Fatalf("%s was touched: %+v", id, d)
	}
}

// Downloads a and b from one address save to f.bin and
// f (1).bin, and only b drops. b alone reads as dropped, so WebView2's
// resume, proposing f (2).bin, is b's: b's row, set back to b's file, and
// a is untouched.
func TestAResumeTakesTheOneDroppedDownload(t *testing.T) {
	r := newDownloadRig(t)
	other := newFakeOp(`C:\D\g.bin`)
	other.uri = "https://example.test/g"
	start(t, r.page, other)
	drop(other)
	a := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, a)
	b := newFakeOp(`C:\D\f (1).bin`)
	start(t, r.page, b)
	drop(b)
	ofB := newFakeOp(`C:\D\f (2).bin`)
	if args := resume(t, r.page, ofB); args.put != `C:\D\f (1).bin` {
		t.Fatalf("the resume put %q, want b's file", args.put)
	}
	if got := r.page.downloads["dl-t-3"]; got.op != ofB.com() || got.path != `C:\D\f (1).bin` {
		t.Fatal("the resume did not take the dropped download")
	}
	untouched(t, r, "dl-t-2", a, 0, `C:\D\f.bin`)
	untouched(t, r, "dl-t-1", other, 0, `C:\D\g.bin`)
	if got := states(r.take()); got != "dl-t-1:running dl-t-2:running dl-t-3:running" {
		t.Fatalf("reports %s", got)
	}
	r.done(t, a, b, other, ofB)
}

// A resume that is not certain is a row of its own saving where WebView2
// chose, and every running download is left as it is: with none reading as
// dropped, with two that dropped together, and with one that dropped beside
// a download that already resumed once, whose operation reads its reason
// for good.
func TestAnUncertainResumeIsANewRowAndTouchesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup starts a and b, from one address, saving to f.bin and
		// f (1).bin, and answers the operations each runs on.
		setup func(t *testing.T, r *downloadRig) (a, b *fakeOp)
		ops   int
	}{
		{"none dropped", func(t *testing.T, r *downloadRig) (*fakeOp, *fakeOp) {
			a, b := newFakeOp(`C:\D\f.bin`), newFakeOp(`C:\D\f (1).bin`)
			start(t, r.page, a)
			start(t, r.page, b)
			return a, b
		}, 0},
		{"two dropped together", func(t *testing.T, r *downloadRig) (*fakeOp, *fakeOp) {
			a, b := newFakeOp(`C:\D\f.bin`), newFakeOp(`C:\D\f (1).bin`)
			start(t, r.page, a)
			start(t, r.page, b)
			drop(a)
			drop(b)
			return a, b
		}, 0},
		{"one dropped beside one resumed", func(t *testing.T, r *downloadRig) (*fakeOp, *fakeOp) {
			first := newFakeOp(`C:\D\f.bin`)
			start(t, r.page, first)
			drop(first)
			a := newFakeOp(`C:\D\f (1).bin`)
			resume(t, r.page, a)
			b := newFakeOp(`C:\D\f (1).bin`)
			start(t, r.page, b)
			drop(b)
			r.extra = append(r.extra, first)
			return a, b
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDownloadRig(t)
			a, b := tc.setup(t, r)
			aPath, bPath := a.path, b.path
			r.take()
			late := newFakeOp(`C:\D\f (2).bin`)
			if args := resume(t, r.page, late); args.put != "" {
				t.Fatalf("an uncertain resume was retargeted to %q", args.put)
			}
			untouched(t, r, "dl-t-1", a, tc.ops, aPath)
			untouched(t, r, "dl-t-2", b, 0, bPath)
			reports := r.take()
			if states(reports) != "dl-t-3:running" || reports[0].Path != `C:\D\f (2).bin` {
				t.Fatalf("the uncertain resume reported %+v", reports)
			}
			r.done(t, append([]*fakeOp{a, b, late}, r.extra...)...)
		})
	}
}

// A resume with no running download from its address is a new row.
func TestAResumeWithNoRunningDownloadIsANewRow(t *testing.T) {
	r := newDownloadRig(t)
	first := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, first)
	first.set(downloadStateCompleted, 0)
	late := newFakeOp(`C:\D\f (1).bin`)
	if args := resume(t, r.page, late); args.put != "" {
		t.Fatalf("a resume with nothing to continue put %q", args.put)
	}
	if got := states(r.take()); got != "dl-t-1:running dl-t-1:completed dl-t-2:running" {
		t.Fatalf("reports %s", got)
	}
	r.done(t, first, late)
}

// A resume whose file cannot be set back keeps WebView2's name, and the
// download's snapshots name the file it really writes.
func TestAResumeThatCannotKeepItsFileNamesTheFileItWrites(t *testing.T) {
	r := newDownloadRig(t)
	first := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, first)
	drop(first)
	continued := newFakeOp(`C:\D\f (1).bin`)
	continued.reason = 24
	args := &fakeStartingArgs{vtbl: &fakeStartingArgsVtbl, op: continued, refusePut: true}
	_, _, _ = downloadStartingVtbl.Invoke.Call(uintptr(unsafe.Pointer(r.page.downloadStarting)), 0, uintptr(unsafe.Pointer(args)))
	continued.received = 100
	continued.set(downloadStateCompleted, 0)
	reports := r.take()
	if states(reports) != "dl-t-1:running dl-t-1:completed" || reports[1].Path != `C:\D\f (1).bin` || reports[1].Name != "f (1).bin" {
		t.Fatalf("reports %+v", reports)
	}
	r.done(t, first, continued)
}

// The backend's cancel holds for a resume that starts after it.
func TestACancelHoldsForAResume(t *testing.T) {
	r := newDownloadRig(t)
	first := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, first)
	r.host.answerDownload(r.page, "dl-t-1", 1)
	drop(first)
	again := newFakeOp(`C:\D\f.bin`)
	resume(t, r.page, again)
	if first.canceled != 1 || again.canceled != 1 {
		t.Fatalf("cancels: first %d, resume %d", first.canceled, again.canceled)
	}
	r.done(t, first, again)
}

// An operation that already reads interrupted, its state change still on
// the way, is not a dropped download a resume continues.
func TestAResumeSkipsAnOperationThatReadsInterrupted(t *testing.T) {
	r := newDownloadRig(t)
	a := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, a)
	a.st, a.reason = downloadStateInterrupted, 24
	b := newFakeOp(`C:\D\f (1).bin`)
	start(t, r.page, b)
	drop(b)
	ofB := newFakeOp(`C:\D\f (2).bin`)
	resume(t, r.page, ofB)
	if got := r.page.downloads["dl-t-2"]; got.op != ofB.com() {
		t.Fatal("the resume did not take the one in-progress dropped download")
	}
	r.take()
	r.done(t, a, b, ofB)
}
