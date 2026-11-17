//go:build windows

package webview2host

import (
	"strings"
	"testing"
	"unsafe"
)

// The launcher's download bookkeeping against a stand-in DownloadOperation
// and DownloadStarting args: the reports, the retry rule, and the lifetime
// of every operation reference, subscription and handler.

// fakeOp is a Go DownloadOperation: it counts its references, keeps its
// subscriptions as WebView2 does (each holding a reference to its handler),
// and records cancels.
type fakeOp struct {
	vtbl     *iCoreWebView2DownloadOperationVtbl
	refs     int
	subs     map[int64]*iDownloadEventHandler
	next     int64
	st       uint32
	reason   uint32
	received int64
	total    int64
	path     string
	uri      string
	canceled int
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
	AddStateChanged:            newComProc(fakeSubscribe),
	RemoveStateChanged:         newComProc(fakeUnsubscribe),
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
	return &fakeOp{vtbl: &fakeOpVtbl, refs: 1, subs: map[int64]*iDownloadEventHandler{}, path: path, uri: "https://example.test/f", total: 100}
}

func (o *fakeOp) com() *iCoreWebView2DownloadOperation {
	return (*iCoreWebView2DownloadOperation)(unsafe.Pointer(o))
}

// fire raises the operation's events on every subscription, as WebView2
// raises one event on all of an event's handlers.
func (o *fakeOp) fire() {
	for _, handler := range o.subs {
		_, _, _ = downloadEventVtbl.Invoke.Call(uintptr(unsafe.Pointer(handler)), uintptr(unsafe.Pointer(o)), 0)
	}
}

// fakeStartingArgs is DownloadStartingEventArgs for one operation.
type fakeStartingArgs struct {
	vtbl    *iDownloadStartingEventArgsVtbl
	op      *fakeOp
	handled bool
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
}

func newDownloadRig(t *testing.T) *downloadRig {
	r := &downloadRig{base: comRootCount()}
	r.host = &Host{downloadToken: "t", config: Config{Logf: t.Logf}}
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
	if len(r.page.downloads) != 0 {
		t.Fatalf("%d downloads kept", len(r.page.downloads))
	}
	if got := comRootCount(); got != r.base {
		t.Fatalf("%d roots, want %d", got, r.base)
	}
}

// states lists the reports, progress left out unless withProgress.
func states(reports []DownloadReport, withProgress ...bool) string {
	var out []string
	for _, r := range reports {
		if r.State != DownloadProgress || len(withProgress) > 0 {
			out = append(out, r.ID+":"+r.State)
		}
	}
	return strings.Join(out, " ")
}

func TestALauncherDownloadReportsItsLifeAndLetsItsOperationGo(t *testing.T) {
	r := newDownloadRig(t)
	op := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, op)
	op.received = 40
	op.fire()
	op.fire() // within the progress interval: held
	op.received, op.st = 100, downloadStateCompleted
	op.fire()
	if got := states(r.reports, true); got != "dl-t-1:started dl-t-1:progress dl-t-1:completed" {
		t.Fatalf("reports %s", got)
	}
	if last := r.reports[2]; last.Path != `C:\D\f.bin` || last.Received != 100 || last.Total != 100 {
		t.Fatalf("completion %+v", last)
	}
	r.done(t, op)
}

// A retry is a new DownloadStarting and operation for the file of the
// page's running download: the same row, one terminal report, every
// operation released once it ends, and the replaced one silent meanwhile.
func TestARetryIsTheSameDownload(t *testing.T) {
	r := newDownloadRig(t)
	first := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, first)
	retry := newFakeOp(`C:\D\F.bin`)
	start(t, r.page, retry)
	if first.refs != 2 || len(first.subs) != 2 {
		t.Fatalf("the replaced operation was let go while its download runs (%d refs, %d subscriptions)", first.refs, len(first.subs))
	}
	first.st = downloadStateInterrupted
	first.fire()
	other := newFakeOp(`C:\D\f (1).bin`)
	start(t, r.page, other)
	retry.st, retry.reason = downloadStateInterrupted, 12
	retry.fire()
	want := "dl-t-1:started dl-t-2:started dl-t-1:interrupted"
	if got := states(r.reports); got != want {
		t.Fatalf("reports %s, want %s", got, want)
	}
	if reason := r.reports[len(r.reports)-1].Reason; reason != "the network failed" {
		t.Fatalf("reason %q", reason)
	}
	if first.refs != 1 || len(first.subs) != 0 || retry.refs != 1 || len(retry.subs) != 0 {
		t.Fatal("an ended download kept an operation")
	}
	r.done(t, first, retry, other)
}

// The backend's cancel holds for a retry that starts after it.
func TestACancelHoldsForARetry(t *testing.T) {
	r := newDownloadRig(t)
	first := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, first)
	r.host.cancelDownload(r.page, "dl-t-1")
	r.host.cancelDownload(r.page, "dl-t-9")
	retry := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, retry)
	if first.canceled != 1 || retry.canceled != 1 {
		t.Fatalf("cancels: first %d, retry %d", first.canceled, retry.canceled)
	}
	r.done(t, first, retry)
}

// A page that goes cancels its running downloads, reports nothing for them,
// and lets every operation and handler go; its DownloadStarting handler
// takes no later download.
func TestAClosingPageCancelsAndReleasesItsDownloads(t *testing.T) {
	r := newDownloadRig(t)
	op := newFakeOp(`C:\D\f.bin`)
	start(t, r.page, op)
	handler := r.page.downloadStarting
	r.done(t, op)
	if op.canceled != 1 {
		t.Fatalf("the running download was canceled %d times", op.canceled)
	}
	late := newFakeOp(`C:\D\g.bin`)
	args := &fakeStartingArgs{vtbl: &fakeStartingArgsVtbl, op: late}
	_, _, _ = downloadStartingVtbl.Invoke.Call(uintptr(unsafe.Pointer(handler)), 0, uintptr(unsafe.Pointer(args)))
	if args.handled || late.refs != 1 || len(r.reports) != 1 {
		t.Fatal("a closed page's handler took a download")
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
