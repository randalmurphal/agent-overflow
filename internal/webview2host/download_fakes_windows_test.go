//go:build windows

package webview2host

import (
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

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
	// refuseStateSub fails AddStateChanged; stateErr fails GetState;
	// refuseCancel fails Cancel; uriErr fails get_Uri; readErr fails the
	// byte counts and get_ResultFilePath.
	refuseStateSub, stateErr, refuseCancel, uriErr, readErr bool
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
		if this.refuseStateSub {
			return 0x80004005 // E_FAIL
		}
		hr := fakeSubscribe(this, handler, token)
		this.stateSubs[token.value] = true
		return hr
	}),
	RemoveStateChanged: newComProc(fakeUnsubscribe),
	GetURI: newComProc(func(this *fakeOp, out *uintptr) uintptr {
		if this.uriErr {
			return 0x80004005 // E_FAIL
		}
		*out = coTaskMemString(this.uri)
		return 0
	}),
	GetResultFilePath: newComProc(func(this *fakeOp, out *uintptr) uintptr {
		if this.readErr {
			return 0x80004005 // E_FAIL
		}
		*out = coTaskMemString(this.path)
		return 0
	}),
	GetTotalBytesToReceive: newComProc(func(this *fakeOp, out *int64) uintptr {
		if this.readErr {
			return 0x80004005 // E_FAIL
		}
		*out = this.total
		return 0
	}),
	GetBytesReceived: newComProc(func(this *fakeOp, out *int64) uintptr {
		if this.readErr {
			return 0x80004005 // E_FAIL
		}
		*out = this.received
		return 0
	}),
	GetState: newComProc(func(this *fakeOp, out *uint32) uintptr {
		if this.stateErr {
			return 0x80004005 // E_FAIL
		}
		*out = this.st
		return 0
	}),
	GetInterruptReason: newComProc(func(this *fakeOp, out *uint32) uintptr { *out = this.reason; return 0 }),
	Cancel: newComProc(func(this *fakeOp) uintptr {
		this.canceled++
		if this.refuseCancel {
			return 0x80004005 // E_FAIL
		}
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
