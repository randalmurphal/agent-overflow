//go:build windows

package webview2host

import (
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Stand-ins for the WebView2 objects a page's create and its popups touch:
// the environment, controller options, controller, view, new-window args
// and deferral. Each counts its references; the view keeps its event
// subscriptions as WebView2 does, each holding a reference to its handler,
// and lets them go when its controller closes.

func addRefObject(obj *comObject) { _, _, _ = obj.vtbl.AddRef.Call(uintptr(unsafe.Pointer(obj))) }

func releaseObject(obj *comObject) { _, _, _ = obj.vtbl.Release.Call(uintptr(unsafe.Pointer(obj))) }

// fakeEnv10 is ICoreWebView2Environment10. Each controller create waits in
// pending, with a reference to its handler, until the test completes it.
type fakeEnv10 struct {
	vtbl     *iCoreWebView2Environment10Vtbl
	createHR uintptr
	pending  []*iControllerCompletedHandler
	options  []*fakeOptions
	parents  []uintptr
}

var fakeEnv10Vtbl = iCoreWebView2Environment10Vtbl{
	iUnknownVtbl: iUnknownVtbl{
		QueryInterface: newComProc(noInterface),
		AddRef:         newComProc(func(*fakeEnv10) uintptr { return 1 }),
		Release:        newComProc(func(*fakeEnv10) uintptr { return 1 }),
	},
	CreateCoreWebView2ControllerOptions: newComProc(func(this *fakeEnv10, out **fakeOptions) uintptr {
		options := &fakeOptions{vtbl: &fakeOptionsVtbl, refs: 1}
		this.options = append(this.options, options)
		*out = options
		return 0
	}),
	CreateCoreWebView2ControllerWithOptions: newComProc(func(this *fakeEnv10, parent uintptr, _ *fakeOptions, handler *comObject) uintptr {
		if this.createHR != 0 {
			return this.createHR
		}
		addRefObject(handler)
		this.parents = append(this.parents, parent)
		this.pending = append(this.pending, (*iControllerCompletedHandler)(unsafe.Pointer(handler)))
		return 0
	}),
}

func (e *fakeEnv10) com() *iCoreWebView2Environment10 {
	return (*iCoreWebView2Environment10)(unsafe.Pointer(e))
}

// complete answers the oldest pending create with hr and controller, then
// lets the handler go, as WebView2 does.
func (e *fakeEnv10) complete(t *testing.T, hr uintptr, controller *fakeController) {
	t.Helper()
	if len(e.pending) == 0 {
		t.Fatal("no controller create is pending")
	}
	handler := e.pending[0]
	e.pending = e.pending[1:]
	if controller == nil {
		_, _, _ = controllerCompletedVtbl.Invoke.Call(uintptr(unsafe.Pointer(handler)), hr, 0)
	} else {
		// Converted in the call's arguments, so the controller is kept
		// where it is until the call returns.
		_, _, _ = controllerCompletedVtbl.Invoke.Call(uintptr(unsafe.Pointer(handler)), hr, uintptr(unsafe.Pointer(controller)))
		// WebView2's own reference, held across the call.
		controller.refs--
	}
	releaseObject((*comObject)(unsafe.Pointer(handler)))
}

type fakeOptions struct {
	vtbl    *iCoreWebView2ControllerOptionsVtbl
	refs    int
	profile string
	private bool
}

var fakeOptionsVtbl = iCoreWebView2ControllerOptionsVtbl{
	iUnknownVtbl: iUnknownVtbl{
		QueryInterface: newComProc(noInterface),
		AddRef:         newComProc(func(this *fakeOptions) uintptr { this.refs++; return uintptr(this.refs) }),
		Release:        newComProc(func(this *fakeOptions) uintptr { this.refs--; return uintptr(this.refs) }),
	},
	PutProfileName: newComProc(func(this *fakeOptions, name *uint16) uintptr {
		this.profile = windows.UTF16PtrToString(name)
		return 0
	}),
	PutIsInPrivateModeEnabled: newComProc(func(this *fakeOptions, value uintptr) uintptr {
		this.private = value != 0
		return 0
	}),
}

// fakeController is ICoreWebView2Controller over one view. It starts with
// WebView2's reference, which fakeEnv10.complete drops.
type fakeController struct {
	vtbl   *iCoreWebView2ControllerVtbl
	refs   int
	view   *fakeView
	bounds rect
	put    []rect
	// putErr fails PutBounds.
	putErr uintptr
	// visible is the last IsVisible put, which visibleErr fails.
	visible    bool
	visibleErr uintptr
	// focused counts MoveFocus calls.
	focused int
	closed  int
	keys    []*comObject
	// reparented counts PutParentWindow calls.
	reparented int
}

var fakeControllerVtbl = iCoreWebView2ControllerVtbl{
	iUnknownVtbl: iUnknownVtbl{
		QueryInterface: newComProc(noInterface),
		AddRef:         newComProc(func(this *fakeController) uintptr { this.refs++; return uintptr(this.refs) }),
		Release:        newComProc(func(this *fakeController) uintptr { this.refs--; return uintptr(this.refs) }),
	},
	PutIsVisible: newComProc(func(this *fakeController, visible uintptr) uintptr {
		if this.visibleErr != 0 {
			return this.visibleErr
		}
		this.visible = visible != 0
		return 0
	}),
	GetBounds: newComProc(func(this *fakeController, out *rect) uintptr {
		*out = this.bounds
		return 0
	}),
	PutParentWindow: newComProc(func(this *fakeController, _ uintptr) uintptr {
		this.reparented++
		return 0
	}),
	PutBounds: newComProc(func(this *fakeController, bounds *rect) uintptr {
		if this.putErr != 0 {
			return this.putErr
		}
		this.bounds = *bounds
		this.put = append(this.put, *bounds)
		return 0
	}),
	MoveFocus: newComProc(func(this *fakeController, _ uintptr) uintptr {
		this.focused++
		return 0
	}),
	AddAcceleratorKeyPressed: newComProc(func(this *fakeController, handler *comObject, _ *eventRegistrationToken) uintptr {
		addRefObject(handler)
		this.keys = append(this.keys, handler)
		return 0
	}),
	Close: newComProc(func(this *fakeController) uintptr {
		this.closed++
		for _, handler := range this.keys {
			releaseObject(handler)
		}
		this.keys = nil
		this.view.destroy()
		return 0
	}),
	GetCoreWebView2: newComProc(func(this *fakeController, out **fakeView) uintptr {
		this.view.refs++
		*out = this.view
		return 0
	}),
}

func newFakeController() *fakeController {
	return &fakeController{vtbl: &fakeControllerVtbl, refs: 1, view: newFakeView()}
}

// fakeView is ICoreWebView2 through ICoreWebView2_9, the view the host
// queries for downloads and the download dialog.
type fakeView struct {
	vtbl *iCoreWebView2_9Vtbl
	refs int
	// subs are the view's event handlers by event, devtools its pending
	// DevTools calls.
	subs       map[string][]*comObject
	devtools   []*iDevToolsCompletedHandler
	dialogOpen bool
	dialogShut int
	// subErr fails the subscription to an event, and dialogErr the read of
	// whether the download dialog is open.
	subErr    map[string]uintptr
	dialogErr uintptr
	// inspectorOpens counts OpenDevToolsWindow calls, which inspectorErr
	// fails.
	inspectorOpens int
	inspectorErr   uintptr
}

func fakeSubscribeView(event string) comProc {
	return newComProc(func(this *fakeView, handler *comObject, _ *eventRegistrationToken) uintptr {
		if hr := this.subErr[event]; hr != 0 {
			return hr
		}
		addRefObject(handler)
		this.subs[event] = append(this.subs[event], handler)
		return 0
	})
}

var fakeViewVtbl = func() iCoreWebView2_9Vtbl {
	var v iCoreWebView2_9Vtbl
	v.QueryInterface = newComProc(func(this *fakeView, riid *windows.GUID, out *uintptr) uintptr {
		if *riid != iidWebView2_9 && *riid != iidWebView2_4 {
			*out = 0
			return 0x80004002 // E_NOINTERFACE
		}
		this.refs++
		*out = uintptr(unsafe.Pointer(this))
		return 0
	})
	// add_DownloadStarting is slot 75 (iCoreWebView2_4Vtbl), inside the
	// slots iCoreWebView2_9Vtbl leaves unnamed.
	*(*comProc)(unsafe.Add(unsafe.Pointer(&v), unsafe.Offsetof(iCoreWebView2_4Vtbl{}.AddDownloadStarting))) = fakeSubscribeView("download-starting")
	v.AddRef = newComProc(func(this *fakeView) uintptr { this.refs++; return uintptr(this.refs) })
	v.Release = newComProc(func(this *fakeView) uintptr { this.refs--; return uintptr(this.refs) })
	v.AddProcessFailed = fakeSubscribeView("process-failed")
	v.AddNewWindowRequested = fakeSubscribeView("new-window")
	v.AddWindowCloseRequested = fakeSubscribeView("close-requested")
	v.AddIsDefaultDownloadDialogOpenChanged = fakeSubscribeView("dialog")
	v.CallDevToolsProtocolMethod = newComProc(func(this *fakeView, _, _ *uint16, handler *comObject) uintptr {
		addRefObject(handler)
		this.devtools = append(this.devtools, (*iDevToolsCompletedHandler)(unsafe.Pointer(handler)))
		return 0
	})
	v.GetIsDefaultDownloadDialogOpen = newComProc(func(this *fakeView, out *int32) uintptr {
		if this.dialogErr != 0 {
			return this.dialogErr
		}
		*out = 0
		if this.dialogOpen {
			*out = 1
		}
		return 0
	})
	v.CloseDefaultDownloadDialog = newComProc(func(this *fakeView) uintptr {
		this.dialogOpen = false
		this.dialogShut++
		return 0
	})
	v.OpenDevToolsWindow = newComProc(func(this *fakeView) uintptr {
		this.inspectorOpens++
		return this.inspectorErr
	})
	return v
}()

func newFakeView() *fakeView {
	return &fakeView{vtbl: &fakeViewVtbl, subs: map[string][]*comObject{}}
}

// destroy lets every subscription and pending call go, as a closed
// controller's view does.
func (v *fakeView) destroy() {
	for event, handlers := range v.subs {
		for _, handler := range handlers {
			releaseObject(handler)
		}
		delete(v.subs, event)
	}
	for _, handler := range v.devtools {
		releaseObject((*comObject)(unsafe.Pointer(handler)))
	}
	v.devtools = nil
}

// answerDevTools completes the oldest pending DevTools call.
func (v *fakeView) answerDevTools(t *testing.T, hr uintptr, result string) {
	t.Helper()
	if len(v.devtools) == 0 {
		t.Fatal("no DevTools call is pending")
	}
	handler := v.devtools[0]
	v.devtools = v.devtools[1:]
	resultPtr, err := windows.UTF16PtrFromString(result)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = devToolsCompletedVtbl.Invoke.Call(uintptr(unsafe.Pointer(handler)), hr, uintptr(unsafe.Pointer(resultPtr)))
	releaseObject((*comObject)(unsafe.Pointer(handler)))
}

// requestWindow raises NewWindowRequested with args on every handler, then
// drops WebView2's reference to the args, as it does once the handlers
// return.
func (v *fakeView) requestWindow(args *fakeWindowArgs) {
	for _, handler := range v.subs["new-window"] {
		_, _, _ = newWindowRequestedVtbl.Invoke.Call(uintptr(unsafe.Pointer(handler)), uintptr(unsafe.Pointer(v)), uintptr(unsafe.Pointer(args)))
	}
	args.refs--
}

// raise invokes a view event whose args the host does not read.
func (v *fakeView) raise(event string) {
	for _, handler := range v.subs[event] {
		_, _, _ = viewEventVtbl.Invoke.Call(uintptr(unsafe.Pointer(handler)), uintptr(unsafe.Pointer(v)), 0)
	}
}

// processFailed raises ProcessFailed with no args.
func (v *fakeView) processFailed() {
	for _, handler := range v.subs["process-failed"] {
		_, _, _ = processFailedVtbl.Invoke.Call(uintptr(unsafe.Pointer(handler)), uintptr(unsafe.Pointer(v)), 0)
	}
}

// fakeWindowArgs is NewWindowRequestedEventArgs, with WebView2's reference.
type fakeWindowArgs struct {
	vtbl        *iNewWindowRequestedEventArgsVtbl
	refs        int
	uri         string
	handled     bool
	newWindow   *fakeView
	putErr      uintptr
	handledErr  uintptr
	deferralErr uintptr
	deferral    *fakeDeferral
}

var fakeWindowArgsVtbl = iNewWindowRequestedEventArgsVtbl{
	iUnknownVtbl: iUnknownVtbl{
		QueryInterface: newComProc(noInterface),
		AddRef:         newComProc(func(this *fakeWindowArgs) uintptr { this.refs++; return uintptr(this.refs) }),
		Release:        newComProc(func(this *fakeWindowArgs) uintptr { this.refs--; return uintptr(this.refs) }),
	},
	GetURI: newComProc(func(this *fakeWindowArgs, out *uintptr) uintptr {
		*out = coTaskMemString(this.uri)
		return 0
	}),
	PutNewWindow: newComProc(func(this *fakeWindowArgs, view *fakeView) uintptr {
		if this.putErr != 0 {
			return this.putErr
		}
		this.newWindow = view
		return 0
	}),
	PutHandled: newComProc(func(this *fakeWindowArgs, value uintptr) uintptr {
		if this.handledErr != 0 {
			return this.handledErr
		}
		this.handled = value != 0
		return 0
	}),
	GetDeferral: newComProc(func(this *fakeWindowArgs, out **fakeDeferral) uintptr {
		if this.deferralErr != 0 {
			return this.deferralErr
		}
		if this.deferral != nil {
			panic("a second deferral of one window request")
		}
		this.deferral = &fakeDeferral{vtbl: &fakeDeferralVtbl, refs: 1}
		*out = this.deferral
		return 0
	}),
}

func newFakeWindowArgs(uri string) *fakeWindowArgs {
	return &fakeWindowArgs{vtbl: &fakeWindowArgsVtbl, refs: 1, uri: uri}
}

type fakeDeferral struct {
	vtbl      *iCoreWebView2DeferralVtbl
	refs      int
	completed int
}

var fakeDeferralVtbl = iCoreWebView2DeferralVtbl{
	iUnknownVtbl: iUnknownVtbl{
		QueryInterface: newComProc(noInterface),
		AddRef:         newComProc(func(this *fakeDeferral) uintptr { this.refs++; return uintptr(this.refs) }),
		Release:        newComProc(func(this *fakeDeferral) uintptr { this.refs--; return uintptr(this.refs) }),
	},
	Complete: newComProc(func(this *fakeDeferral) uintptr {
		this.completed++
		return 0
	}),
}

type rigReport struct {
	page   string
	kind   ReportKind
	detail string
}

// popupRig is a Host over a fakeEnv10, whose UI thread is the test's: a
// call to OnMain from any other thread waits in queue, with every Post,
// until the test pumps it there.
type popupRig struct {
	t     *testing.T
	host  *Host
	env   *fakeEnv10
	base  int
	owner uint32
	// beforeMain, when set, runs as each OnMain on the test's thread
	// starts, before its function, until it answers true.
	beforeMain func() bool

	mu      sync.Mutex
	reports []rigReport
	queue   []func()
	// sink, when set, sees each report as the launcher's report queue
	// would, inside Report, after it is recorded.
	sink func(rigReport)
}

func newPopupRig(t *testing.T) *popupRig {
	t.Helper()
	// The host's windows are the test thread's.
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	r := &popupRig{t: t, env: &fakeEnv10{vtbl: &fakeEnv10Vtbl}, base: comRootCount(), owner: windows.GetCurrentThreadId()}
	hwnd := messageWindow(t)
	r.host = &Host{
		pages:         map[string]*hostPage{},
		downloadToken: "t",
		foreground:    func(uintptr) bool { return false },
		config: Config{
			Logf:       t.Logf,
			HostWindow: func() uintptr { return hwnd },
			OnMain: func(fn func()) {
				if windows.GetCurrentThreadId() == r.owner {
					if before := r.beforeMain; before != nil && before() {
						r.beforeMain = nil
					}
					fn()
					return
				}
				done := make(chan struct{})
				r.post(func() { defer close(done); fn() })
				<-done
			},
			Post: r.post,
			Report: func(pageID string, kind ReportKind, detail string) {
				r.mu.Lock()
				r.reports = append(r.reports, rigReport{pageID, kind, detail})
				sink := r.sink
				r.mu.Unlock()
				if sink != nil {
					sink(rigReport{pageID, kind, detail})
				}
			},
		},
	}
	r.host.env = newPaneEnvironment(func(gen uint64) {
		r.host.env.completed(gen, paneEnv{env10: r.env.com()}, nil)
	}, func(paneEnv) {}, t.Logf, time.Second)
	return r
}

// messageWindow is a message-only window, never shown, to parent the
// host's clip containers.
func messageWindow(t *testing.T) uintptr {
	t.Helper()
	if err := registerClipClass(); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(clipClassName)
	if err != nil {
		t.Fatal(err)
	}
	const hwndMessage = ^uintptr(2) // HWND_MESSAGE, (HWND)-3
	hwnd, _, callErr := procCreateWindowEx.Call(0, uintptr(unsafe.Pointer(name)), 0, 0, 0, 0, 0, 0, hwndMessage, 0, uintptr(clipModule), 0)
	if hwnd == 0 {
		t.Fatalf("create a message-only window: %v", callErr)
	}
	t.Cleanup(func() {
		if err := destroyWindow(hwnd); err != nil {
			t.Error(err)
		}
	})
	return hwnd
}

// takeReports answers the reports since the last call.
func (r *popupRig) takeReports() []rigReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.reports
	r.reports = nil
	return out
}

// post queues fn for the test's thread.
func (r *popupRig) post(fn func()) {
	r.mu.Lock()
	r.queue = append(r.queue, fn)
	r.mu.Unlock()
}

// pump runs on the test's thread what is queued for it, and what that
// queues, and answers how many it ran.
func (r *popupRig) pump() int {
	ran := 0
	for {
		r.mu.Lock()
		if len(r.queue) == 0 {
			r.mu.Unlock()
			return ran
		}
		fn := r.queue[0]
		r.queue = r.queue[1:]
		r.mu.Unlock()
		fn()
		ran++
	}
}

// awaitQueued waits a moment for work queued for the test's thread.
func (r *popupRig) awaitQueued() {
	r.t.Helper()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		r.mu.Lock()
		n := len(r.queue)
		r.mu.Unlock()
		if n > 0 {
			return
		}
	}
	r.t.Fatal("nothing was queued for the UI thread")
}

// awaitReport pumps until page reports kind, and answers the reports until
// then, that one last.
func (r *popupRig) awaitReport(page string, kind ReportKind) []rigReport {
	r.t.Helper()
	var got []rigReport
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		r.pump()
		r.mu.Lock()
		match := slices.IndexFunc(r.reports, func(report rigReport) bool {
			return report.page == page && report.kind == kind
		})
		if match >= 0 {
			got = append(got, r.reports[:match+1]...)
			r.reports = r.reports[match+1:]
			r.mu.Unlock()
			return got
		}
		got = append(got, r.reports...)
		r.reports = nil
		r.mu.Unlock()
	}
	r.t.Fatalf("no %s report of %s in %+v", kind, page, got)
	return nil
}

// createPage creates a backend page through its directive and answers its
// controller and target.
func (r *popupRig) createPage(id, profile string, ephemeral bool) (*hostPage, *fakeController) {
	r.t.Helper()
	r.host.Apply(Directive{Op: OpCreate, PageID: id, ProfileID: profile, Ephemeral: ephemeral})
	controller := newFakeController()
	controller.bounds = rect{Left: 10, Top: 20, Right: 810, Bottom: 620}
	r.env.complete(r.t, 0, controller)
	controller.view.answerDevTools(r.t, 0, `{"targetInfo":{"targetId":"target-`+id+`"}}`)
	reports := r.takeReports()
	if len(reports) != 2 || reports[0].kind != ReportFindUnavailable || reports[1].kind != ReportCreated || reports[1].detail != "target-"+id {
		r.t.Fatalf("create %s reported %+v, want find-unavailable then created", id, reports)
	}
	page := r.page(id)
	if page == nil {
		r.t.Fatalf("page %s is not registered", id)
	}
	return page, controller
}

// page answers the page registered under id. Runs on the test's thread,
// the host's UI thread.
func (r *popupRig) page(id string) *hostPage {
	return r.host.pages[id]
}

// settled checks that a window request was answered exactly once, and
// that every reference the host took to it was let go.
func settled(t *testing.T, args *fakeWindowArgs, window *fakeView) {
	t.Helper()
	if !args.handled {
		t.Error("the window request was not handled")
	}
	if args.newWindow != window {
		t.Errorf("the request's new window is %p, want %p", args.newWindow, window)
	}
	if args.refs != 0 {
		t.Errorf("the request's args have %d references left", args.refs)
	}
	if args.deferral == nil {
		t.Fatal("the request was not deferred")
	}
	if args.deferral.completed != 1 || args.deferral.refs != 0 {
		t.Errorf("the deferral completed %d times with %d references left, want once and none", args.deferral.completed, args.deferral.refs)
	}
}

// closed checks that a controller was closed once and every reference to
// it and its view let go.
func closed(t *testing.T, controller *fakeController) {
	t.Helper()
	if controller.closed != 1 || controller.refs != 0 || controller.view.refs != 0 {
		t.Errorf("controller closed %d times with %d references and its view %d, want once, none and none",
			controller.closed, controller.refs, controller.view.refs)
	}
	if len(controller.keys) != 0 || len(controller.view.subs) != 0 || len(controller.view.devtools) != 0 {
		t.Error("a closed controller kept a subscription")
	}
}

// done closes the host, fails every controller create still pending, as
// WebView2 does when its environment goes, and checks that every Go-backed
// COM object is let go.
func (r *popupRig) done() {
	r.t.Helper()
	r.host.Close()
	r.pump()
	for len(r.env.pending) > 0 {
		r.env.complete(r.t, 0x80004004, nil) // E_ABORT
	}
	if r.host.pendingPopups != 0 {
		r.t.Errorf("%d native popup creates still counted after all completions", r.host.pendingPopups)
	}
	if n := comRootCount(); n != r.base {
		r.t.Errorf("%d COM objects rooted after the host closed, want %d", n, r.base)
	}
}
