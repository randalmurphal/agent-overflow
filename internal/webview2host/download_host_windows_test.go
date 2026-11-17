//go:build windows

package webview2host

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The launcher's download bookkeeping against a stand-in DownloadOperation
// and DownloadStarting args: the snapshots, the answers to directives, the
// resume rule, and the lifetime of every operation
// reference, subscription and handler.

// fakeOp is a Go DownloadOperation: it counts its references, keeps its
// subscriptions as WebView2 does (each holding a reference to its handler),
// and records cancels.
type fakeOp struct {
	vtbl *iCoreWebView2DownloadOperationVtbl
	refs int
	subs map[int64]*iDownloadEventHandler
	// stateSubs marks the StateChanged subscriptions among subs.
	stateSubs map[int64]bool
	next      int64
	st        uint32
	reason    uint32
	received  int64
	total     int64
	path      string
	uri       string
	canceled  int
}

func fakeHandlerAddRef(h *iDownloadEventHandler) {
	_, _, _ = h.vtbl.AddRef.Call(uintptr(unsafe.Pointer(h)))
}

func fakeHandlerRelease(h *iDownloadEventHandler) {
	_, _, _ = h.vtbl.Release.Call(uintptr(unsafe.Pointer(h)))
}

func fakeSubscribe(this *fakeOp, handler *iDownloadEventHandler, token *eventRegistrationToken) uintptr {
	fakeHandlerAddRef(handler)
	this.next++
	this.subs[this.next] = handler
	token.value = this.next
	return 0
}

func fakeUnsubscribe(this *fakeOp, token int64) uintptr {
	if handler := this.subs[token]; handler != nil {
		delete(this.subs, token)
		delete(this.stateSubs, token)
		fakeHandlerRelease(handler)
	}
	return 0
}

var fakeOpVtbl = iCoreWebView2DownloadOperationVtbl{
	iUnknownVtbl: iUnknownVtbl{
		QueryInterface: newComProc(noInterface),
		AddRef:         newComProc(func(this *fakeOp) uintptr { this.refs++; return uintptr(this.refs) }),
		Release:        newComProc(func(this *fakeOp) uintptr { this.refs--; return uintptr(this.refs) }),
	},
	AddBytesReceivedChanged:    newComProc(fakeSubscribe),
	RemoveBytesReceivedChanged: newComProc(fakeUnsubscribe),
	AddStateChanged: newComProc(func(this *fakeOp, handler *iDownloadEventHandler, token *eventRegistrationToken) uintptr {
		hr := fakeSubscribe(this, handler, token)
		this.stateSubs[token.value] = true
		return hr
	}),
	RemoveStateChanged: newComProc(fakeUnsubscribe),
	GetURI: newComProc(func(this *fakeOp, out *uintptr) uintptr {
		*out = coTaskMemString(this.uri)
		return 0
	}),
	GetResultFilePath: newComProc(func(this *fakeOp, out *uintptr) uintptr {
		*out = coTaskMemString(this.path)
		return 0
	}),
	GetTotalBytesToReceive: newComProc(func(this *fakeOp, out *int64) uintptr { *out = this.total; return 0 }),
	GetBytesReceived:       newComProc(func(this *fakeOp, out *int64) uintptr { *out = this.received; return 0 }),
	GetState:               newComProc(func(this *fakeOp, out *uint32) uintptr { *out = this.st; return 0 }),
	GetInterruptReason:     newComProc(func(this *fakeOp, out *uint32) uintptr { *out = this.reason; return 0 }),
	Cancel: newComProc(func(this *fakeOp) uintptr {
		this.canceled++
		return 0
	}),
}

func newFakeOp(path string) *fakeOp {
	return &fakeOp{vtbl: &fakeOpVtbl, refs: 1, subs: map[int64]*iDownloadEventHandler{}, stateSubs: map[int64]bool{}, path: path, uri: "https://example.test/f", total: 100}
}

func (o *fakeOp) com() *iCoreWebView2DownloadOperation {
	return (*iCoreWebView2DownloadOperation)(unsafe.Pointer(o))
}

// fire raises both of the operation's events on every subscription, as
// WebView2 raises one event on all of an event's handlers.
func (o *fakeOp) fire() {
	for _, handler := range o.subs {
		_, _, _ = downloadEventVtbl.Invoke.Call(uintptr(unsafe.Pointer(handler)), uintptr(unsafe.Pointer(o)), 0)
	}
}

// set moves the operation to a state and raises StateChanged.
func (o *fakeOp) set(state, reason uint32) {
	o.st, o.reason = state, reason
	for token, handler := range o.subs {
		if o.stateSubs[token] {
			_, _, _ = downloadEventVtbl.Invoke.Call(uintptr(unsafe.Pointer(handler)), uintptr(unsafe.Pointer(o)), 0)
		}
	}
}

// fakeStartingArgs is DownloadStartingEventArgs for one operation.
// A put file path becomes the operation's, as WebView2 saves there.
type fakeStartingArgs struct {
	vtbl      *iDownloadStartingEventArgsVtbl
	op        *fakeOp
	handled   bool
	put       string
	refusePut bool
}

var fakeStartingArgsVtbl = iDownloadStartingEventArgsVtbl{
	iUnknownVtbl: iUnknownVtbl{QueryInterface: newComProc(noInterface)},
	GetDownloadOperation: newComProc(func(this *fakeStartingArgs, out **fakeOp) uintptr {
		this.op.refs++
		*out = this.op
		return 0
	}),
	GetResultFilePath: newComProc(func(this *fakeStartingArgs, out *uintptr) uintptr {
		*out = coTaskMemString(this.op.path)
		return 0
	}),
	PutResultFilePath: newComProc(func(this *fakeStartingArgs, value *uint16) uintptr {
		if this.refusePut {
			return 0x80070005 // E_ACCESSDENIED
		}
		this.put = windows.UTF16PtrToString(value)
		this.op.path = this.put
		return 0
	}),
	PutHandled: newComProc(func(this *fakeStartingArgs, value uintptr) uintptr {
		this.handled = value != 0
		return 0
	}),
}

// start raises DownloadStarting for op on page through its handler.
func start(t *testing.T, page *hostPage, op *fakeOp) {
	t.Helper()
	args := &fakeStartingArgs{vtbl: &fakeStartingArgsVtbl, op: op}
	_, _, _ = downloadStartingVtbl.Invoke.Call(uintptr(unsafe.Pointer(page.downloadStarting)), 0, uintptr(unsafe.Pointer(args)))
	if !args.handled {
		t.Fatal("a download was not taken from WebView2's dialog")
	}
}

type downloadRig struct {
	host    *Host
	page    *hostPage
	reports []DownloadReport
	base    int
	// timers are the cancel bounds started, in order; a test fires one.
	timers []func()
	// extra are operations a test's setup made beyond the ones it answers.
	extra []*fakeOp
}

func newDownloadRig(t *testing.T) *downloadRig {
	r := &downloadRig{base: comRootCount()}
	r.host = &Host{downloadToken: "t", config: Config{Logf: t.Logf, OnMain: func(fn func()) { fn() }, Post: func(func()) {
		t.Error("a page with no close request posted its close")
	}}}
	r.host.config.Report = func(pageID string, kind ReportKind, detail string) {
		if kind != ReportDownload || pageID != "p" {
			t.Fatalf("report %s for %s", kind, pageID)
		}
		parsed, err := ParseDownloadReport(detail)
		if err != nil {
			t.Fatal(err)
		}
		r.reports = append(r.reports, parsed)
	}
	r.page = &hostPage{id: "p"}
	r.page.downloadStarting = newDownloadStartingHandler(func(args *iDownloadStartingEventArgs) { r.host.downloadStarting(r.page, args) })
	r.host.pages = map[string]*hostPage{"p": r.page}
	r.host.afterFunc = func(wait time.Duration, fn func()) {
		if wait != downloadCancelBound {
			t.Errorf("a cancel bound of %v, want %v", wait, downloadCancelBound)
		}
		r.timers = append(r.timers, fn)
	}
	return r
}

// done retires the page and asserts nothing of the downloads is left.
func (r *downloadRig) done(t *testing.T, ops ...*fakeOp) {
	t.Helper()
	r.host.retireHandlers(r.page)
	r.page.downloadStarting.ref.release()
	for _, op := range ops {
		if op.refs != 1 || len(op.subs) != 0 {
			t.Fatalf("an operation kept %d references beyond WebView2's and %d subscriptions", op.refs-1, len(op.subs))
		}
	}
	if len(r.page.downloads) != 0 || len(r.page.ended.byID) != 0 {
		t.Fatalf("%d downloads and %d ended snapshots kept", len(r.page.downloads), len(r.page.ended.byID))
	}
	if got := comRootCount(); got != r.base {
		t.Fatalf("%d roots, want %d", got, r.base)
	}
}

// take answers the reports since the last take.
func (r *downloadRig) take() []DownloadReport {
	out := r.reports
	r.reports = nil
	return out
}

// states lists reports as id:state.
func states(reports []DownloadReport) string {
	var out []string
	for _, r := range reports {
		out = append(out, r.ID+":"+r.State)
	}
	return strings.Join(out, " ")
}

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

	r.host.answerDownload(r.page, "dl-t-1", false)
	if got := r.take(); states(got) != "dl-t-1:running" || op.canceled != 0 || got[0].Seq != 2 || !got[0].Answer {
		t.Fatalf("a status of a running download answers %+v and cancels %d times", got, op.canceled)
	}
	r.host.answerDownload(r.page, "dl-t-1", true)
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
	r.host.answerDownload(r.page, "dl-t-1", true)
	r.host.answerDownload(r.page, "dl-t-1", false)
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
	r.host.answerDownload(r.page, "dl-t-9", true)
	if got := r.take(); len(got) != 1 || got[0] != (DownloadReport{ID: "dl-t-9", State: DownloadUnknown, Reason: DownloadUnknownReason, Answer: true}) {
		t.Fatalf("an unknown id answers %+v", got)
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
	r.host.answerDownload(r.page, "dl-t-1", false)
	r.host.answerDownload(r.page, "dl-t-2", false)
	got := r.take()
	if got[0].Reason != DownloadUnknownReason || got[1].State != DownloadCompleted {
		t.Fatalf("the oldest answers %+v and the next %+v", got[0], got[1])
	}
	r.done(t, ops...)
	r.host.answerDownload(r.page, "dl-t-2", false)
	if got := r.take(); got[0].Reason != DownloadUnknownReason {
		t.Fatalf("after the page went an ended download answers %+v", got[0])
	}
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

// The reviewer's sequence: a and b from one address save to f.bin and
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
	r.host.answerDownload(r.page, "dl-t-1", true)
	drop(first)
	again := newFakeOp(`C:\D\f.bin`)
	resume(t, r.page, again)
	if first.canceled != 1 || again.canceled != 1 {
		t.Fatalf("cancels: first %d, resume %d", first.canceled, again.canceled)
	}
	r.done(t, first, again)
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
// went to a row of its own) ends canceled when the bound passes: one
// terminal snapshot, its operation let go, and a state change after that
// reaches nobody. A repeated cancel starts no second bound.
func TestACanceledDownloadWebView2DoesNotEndSettlesAtTheBound(t *testing.T) {
	r := newDownloadRig(t)
	op := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, op)
	drop(op)
	r.take()
	r.host.answerDownload(r.page, "dl-t-1", true)
	r.host.answerDownload(r.page, "dl-t-1", true)
	if got := r.take(); states(got) != "dl-t-1:running dl-t-1:running" || len(r.timers) != 1 {
		t.Fatalf("two cancels answered %s and started %d bounds", states(got), len(r.timers))
	}
	r.timers[0]()
	end := r.take()
	if states(end) != "dl-t-1:interrupted" || end[0].Reason != downloadInterruptReason(downloadUserCanceled) || end[0].Answer {
		t.Fatalf("at the bound the download ended %+v", end)
	}
	if op.refs != 1 || len(op.subs) != 0 {
		t.Fatalf("the settled download kept its operation: %d refs, %d subscriptions", op.refs, len(op.subs))
	}
	op.received = 100
	op.set(downloadStateCompleted, 0)
	op.fire()
	r.host.answerDownload(r.page, "dl-t-1", false)
	if got := r.take(); len(got) != 1 || !got[0].Answer || got[0].State != DownloadInterrupted {
		t.Fatalf("after the settled end: %+v", got)
	}
	r.done(t, op)
}

// A canceled download WebView2 ends settles by WebView2's own report, once;
// the bound that passes after it does nothing.
func TestACanceledDownloadSettlesByWebView2sReport(t *testing.T) {
	r := newDownloadRig(t)
	op := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, op)
	r.host.answerDownload(r.page, "dl-t-1", true)
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
	r.host.answerDownload(r.page, "dl-t-1", true)
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
