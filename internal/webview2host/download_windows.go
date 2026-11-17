//go:build windows

package webview2host

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The launcher's half of a pane page's downloads (download.go). Every
// function here runs on the UI thread: WebView2 raises DownloadStarting and
// the operation's events there, and directives reach it through OnMain.

// hostDownload is one download a page started, from its DownloadStarting
// until its operation ends or its page goes.
type hostDownload struct {
	id    string
	page  *hostPage
	op    *iCoreWebView2DownloadOperation
	path  string
	bytes *iDownloadEventHandler
	state *iDownloadEventHandler
	// bytesToken and stateToken remove the two subscriptions.
	bytesToken, stateToken eventRegistrationToken
	gate                   progressGate
	// cancelRequested is the backend's cancel, applied to a retry too.
	cancelRequested bool
	// stale holds the operations retries replaced (retireOperation).
	stale []downloadOp
}

// newDownloadToken is the part of a download id unique to one host.
func newDownloadToken() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on Windows; a fixed token only loses
		// uniqueness across launcher runs.
		return "host"
	}
	return hex.EncodeToString(b[:])
}

// downloadID is the launcher's id for its n-th download: host is a token
// unique to the host, so no two launcher runs reuse an id.
func downloadID(host string, n uint64) string {
	return "dl-" + host + "-" + strconv.FormatUint(n, 10)
}

// watchDownloads takes every download the page starts, so WebView2's own
// download dialog never opens for it. A runtime without DownloadStarting
// keeps WebView2's dialog and the pane lists nothing.
func (h *Host) watchDownloads(page *hostPage, view *iCoreWebView2) {
	view4 := view.queryWebView4()
	if view4 == nil {
		h.config.Logf("browser host: page %s: the runtime has no DownloadStarting; its downloads show WebView2's dialog and are not listed", page.id)
		return
	}
	defer view4.release()
	handler := newDownloadStartingHandler(func(args *iDownloadStartingEventArgs) { h.downloadStarting(page, args) })
	page.downloadStarting = handler
	if _, err := view4.addDownloadStarting(handler); err != nil {
		h.config.Logf("browser host: page %s download subscription: %v", page.id, err)
	}
	handler.ref.release()
}

// downloadStarting takes one download: WebView2's dialog stays closed, the
// operation is kept under a launcher id, and the backend hears it began.
//
// WebView2 retries a download whose connection dropped by starting it again:
// a new DownloadStarting with a new operation for the same file, while the
// old operation stays in progress and never reports again. A file name is
// reserved while its download runs, so no other download of the page can
// start with it; a start naming the file of the page's running download is
// that download's retry, and takes its place under the same id.
func (h *Host) downloadStarting(page *hostPage, args *iDownloadStartingEventArgs) {
	if args == nil {
		return
	}
	if err := args.putHandled(true); err != nil {
		h.config.Logf("browser host: page %s: take a download: %v", page.id, err)
	}
	op, err := args.downloadOperation()
	if err != nil {
		h.config.Logf("browser host: page %s: a download has no operation: %v", page.id, err)
		return
	}
	path := args.resultFilePath()
	if d := page.runningDownload(path); d != nil {
		h.retireOperation(d)
		h.attachOperation(d, op)
		if d.cancelRequested {
			h.cancelDownload(page, d.id)
		}
		h.downloadStateChanged(d)
		return
	}
	h.downloadSeq++
	d := &hostDownload{id: downloadID(h.downloadToken, h.downloadSeq), page: page, path: path}
	h.attachOperation(d, op)
	if page.downloads == nil {
		page.downloads = make(map[string]*hostDownload)
	}
	page.downloads[d.id] = d
	h.reportDownload(d, DownloadReport{State: DownloadStarted, URI: op.uri(), Received: op.bytesReceived(), Total: op.totalBytes()})
	// One that ended before its subscriptions were made reports nothing
	// more on its own.
	h.downloadStateChanged(d)
}

// runningDownload answers the page's running download saving to path.
func (p *hostPage) runningDownload(path string) *hostDownload {
	if path == "" {
		return nil
	}
	for _, d := range p.downloads {
		if d.op != nil && strings.EqualFold(d.path, path) {
			return d
		}
	}
	return nil
}

// downloadOp is one operation a download had, with its subscriptions.
type downloadOp struct {
	op                     *iCoreWebView2DownloadOperation
	bytes, state           *iDownloadEventHandler
	bytesToken, stateToken eventRegistrationToken
}

// retireOperation stops d's operation from reporting and keeps it until d
// ends: a retry's operation shares the download with the one it replaces,
// and letting the old one go meanwhile stops the new one's state reports
// (measured on the real runtime).
func (h *Host) retireOperation(d *hostDownload) {
	if d.op == nil {
		return
	}
	d.bytes.changed.detach()
	d.state.changed.detach()
	d.stale = append(d.stale, downloadOp{op: d.op, bytes: d.bytes, state: d.state, bytesToken: d.bytesToken, stateToken: d.stateToken})
	d.op = nil
}

// attachOperation makes op d's operation, subscribed to its bytes and state.
// d takes the caller's reference.
func (h *Host) attachOperation(d *hostDownload, op *iCoreWebView2DownloadOperation) {
	d.op = op
	d.bytes = newDownloadEventHandler(func() { h.downloadBytes(d) })
	var err error
	if d.bytesToken, err = op.addBytesReceivedChanged(d.bytes); err != nil {
		h.config.Logf("browser host: download %s progress subscription: %v", d.id, err)
	}
	d.bytes.ref.release()
	d.state = newDownloadEventHandler(func() { h.downloadStateChanged(d) })
	if d.stateToken, err = op.addStateChanged(d.state); err != nil {
		h.config.Logf("browser host: download %s state subscription: %v", d.id, err)
	}
	d.state.ref.release()
}

// detachOperation lets d's operation go: both subscriptions removed, their
// callbacks detached, the operation released.
func (h *Host) detachOperation(d *hostDownload) {
	if d.op == nil {
		return
	}
	if err := d.op.removeBytesReceivedChanged(d.bytesToken); err != nil {
		h.config.Logf("browser host: download %s: remove progress subscription: %v", d.id, err)
	}
	if err := d.op.removeStateChanged(d.stateToken); err != nil {
		h.config.Logf("browser host: download %s: remove state subscription: %v", d.id, err)
	}
	d.bytes.changed.detach()
	d.state.changed.detach()
	d.op.release()
	d.op = nil
}

// downloadBytes reports a download's bytes, at most once per
// DownloadProgressInterval.
func (h *Host) downloadBytes(d *hostDownload) {
	if d.op == nil || !d.gate.due(time.Now()) {
		return
	}
	h.reportDownload(d, DownloadReport{State: DownloadProgress, Received: d.op.bytesReceived(), Total: d.op.totalBytes()})
}

// downloadStateChanged reports a download's end and lets the operation go.
// An operation back in progress (WebView2 resuming it) reports nothing.
func (h *Host) downloadStateChanged(d *hostDownload) {
	if d.op == nil {
		return
	}
	switch d.op.state() {
	case downloadStateCompleted:
		if path := d.op.resultFilePath(); path != "" {
			d.path = path
		}
		h.reportDownload(d, DownloadReport{State: DownloadCompleted, Received: d.op.bytesReceived(), Total: d.op.totalBytes()})
		h.endDownload(d)
	case downloadStateInterrupted:
		h.reportDownload(d, DownloadReport{State: DownloadInterrupted, Received: d.op.bytesReceived(), Total: d.op.totalBytes(), Reason: downloadInterruptReason(d.op.interruptReason())})
		h.endDownload(d)
	}
}

// reportDownload sends one report of d, with its id and file. A report too
// large for the bridge ends the download as interrupted, so the backend
// still hears an end.
func (h *Host) reportDownload(d *hostDownload, r DownloadReport) {
	r.ID, r.Path = d.id, d.path
	detail, err := r.Encode()
	if err != nil {
		h.config.Logf("browser host: download %s: %v", d.id, err)
		if r.State == DownloadInterrupted {
			r.Path, r.URI = "", ""
		} else {
			r = DownloadReport{ID: d.id, State: DownloadInterrupted, Reason: "the download's file path is too long to report"}
			if d.op != nil {
				_ = d.op.cancel()
				h.endDownload(d)
			}
		}
		if detail, err = r.Encode(); err != nil {
			return
		}
	}
	h.config.Report(d.page.id, ReportDownload, detail)
}

// endDownload lets a download go once it ended or its page went, with
// every operation it had.
func (h *Host) endDownload(d *hostDownload) {
	h.detachOperation(d)
	for _, stale := range d.stale {
		d.op, d.bytes, d.state, d.bytesToken, d.stateToken = stale.op, stale.bytes, stale.state, stale.bytesToken, stale.stateToken
		h.detachOperation(d)
	}
	d.stale = nil
	delete(d.page.downloads, d.id)
}

// cancelDownload cancels one of the page's downloads. Its interrupted
// report follows from the operation's own state change.
func (h *Host) cancelDownload(page *hostPage, id string) {
	d := page.downloads[id]
	if d == nil || d.op == nil {
		return
	}
	// Kept for a retry that replaces the operation.
	d.cancelRequested = true
	if err := d.op.cancel(); err != nil {
		h.config.Logf("browser host: cancel download %s: %v", id, err)
	}
}

// retireDownloads cancels and lets go every download of a page that is
// going. The backend settles them as its page leaves; nothing is reported.
func (h *Host) retireDownloads(page *hostPage) {
	if page.downloadStarting != nil {
		page.downloadStarting.starting.detach()
	}
	for _, d := range page.downloads {
		if d.op != nil {
			if err := d.op.cancel(); err != nil {
				h.config.Logf("browser host: cancel download %s of a closing page: %v", d.id, err)
			}
		}
		h.endDownload(d)
	}
}

// ---------------------------------------------------------------------
// ICoreWebView2_4 (DownloadStarting)
// ---------------------------------------------------------------------

// iCoreWebView2_4Vtbl is the flattened chain through ICoreWebView2_4: slots
// 0-60 are iCoreWebView2Vtbl, 61-72 the methods ICoreWebView2_2 (7) and
// ICoreWebView2_3 (5) add, and 73-76 ICoreWebView2_4's own.
type iCoreWebView2_4Vtbl struct {
	iCoreWebView2Vtbl
	_                      [12]comProc
	AddFrameCreated        comProc
	RemoveFrameCreated     comProc
	AddDownloadStarting    comProc
	RemoveDownloadStarting comProc
}

type iCoreWebView2_4 struct {
	vtbl *iCoreWebView2_4Vtbl
}

// iidWebView2_4 is IID_ICoreWebView2_4, {20d02d59-6df2-42dc-bd06-f98a694b1302}.
var iidWebView2_4 = windows.GUID{
	Data1: 0x20d02d59, Data2: 0x6df2, Data3: 0x42dc,
	Data4: [8]byte{0xbd, 0x06, 0xf9, 0x8a, 0x69, 0x4b, 0x13, 0x02},
}

// queryWebView4 returns nil on a runtime without DownloadStarting.
func (v *iCoreWebView2) queryWebView4() *iCoreWebView2_4 {
	var result *iCoreWebView2_4
	_, _, _ = v.vtbl.QueryInterface.Call(
		uintptr(unsafe.Pointer(v)),
		uintptr(unsafe.Pointer(&iidWebView2_4)),
		uintptr(unsafe.Pointer(&result)),
	)
	return result
}

func (v *iCoreWebView2_4) release() {
	_, _, _ = v.vtbl.Release.Call(uintptr(unsafe.Pointer(v)))
}

func (v *iCoreWebView2_4) addDownloadStarting(handler *iDownloadStartingHandler) (eventRegistrationToken, error) {
	var token eventRegistrationToken
	hr, _, _ := v.vtbl.AddDownloadStarting.Call(
		uintptr(unsafe.Pointer(v)),
		uintptr(unsafe.Pointer(handler)),
		uintptr(unsafe.Pointer(&token)),
	)
	return token, hresult(hr)
}

// ---------------------------------------------------------------------
// ICoreWebView2DownloadStartingEventArgs
// ---------------------------------------------------------------------

type iDownloadStartingEventArgsVtbl struct {
	iUnknownVtbl
	GetDownloadOperation comProc
	GetCancel            comProc
	PutCancel            comProc
	GetResultFilePath    comProc
	PutResultFilePath    comProc
	GetHandled           comProc
	PutHandled           comProc
	GetDeferral          comProc
}

type iDownloadStartingEventArgs struct {
	vtbl *iDownloadStartingEventArgsVtbl
}

// putHandled is put_Handled([in] BOOL): true hides WebView2's download
// dialog for this download.
func (a *iDownloadStartingEventArgs) putHandled(value bool) error {
	hr, _, _ := a.vtbl.PutHandled.Call(uintptr(unsafe.Pointer(a)), boolArg(value))
	return hresult(hr)
}

// downloadOperation is get_DownloadOperation, with the reference the
// caller owns.
func (a *iDownloadStartingEventArgs) downloadOperation() (*iCoreWebView2DownloadOperation, error) {
	var op *iCoreWebView2DownloadOperation
	hr, _, _ := a.vtbl.GetDownloadOperation.Call(uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(&op)))
	if err := hresult(hr); err != nil {
		return nil, err
	}
	if op == nil {
		return nil, errors.New("get_DownloadOperation returned no operation")
	}
	return op, nil
}

func (a *iDownloadStartingEventArgs) resultFilePath() string {
	return comString(a.vtbl.GetResultFilePath, unsafe.Pointer(a))
}

// ---------------------------------------------------------------------
// ICoreWebView2DownloadOperation
// ---------------------------------------------------------------------

type iCoreWebView2DownloadOperationVtbl struct {
	iUnknownVtbl
	AddBytesReceivedChanged       comProc
	RemoveBytesReceivedChanged    comProc
	AddEstimatedEndTimeChanged    comProc
	RemoveEstimatedEndTimeChanged comProc
	AddStateChanged               comProc
	RemoveStateChanged            comProc
	GetURI                        comProc
	GetContentDisposition         comProc
	GetMimeType                   comProc
	GetTotalBytesToReceive        comProc
	GetBytesReceived              comProc
	GetEstimatedEndTime           comProc
	GetResultFilePath             comProc
	GetState                      comProc
	GetInterruptReason            comProc
	Cancel                        comProc
	Pause                         comProc
	Resume                        comProc
	GetCanResume                  comProc
}

type iCoreWebView2DownloadOperation struct {
	vtbl *iCoreWebView2DownloadOperationVtbl
}

// COREWEBVIEW2_DOWNLOAD_STATE.
const (
	downloadStateInProgress  = 0
	downloadStateInterrupted = 1
	downloadStateCompleted   = 2
)

func (o *iCoreWebView2DownloadOperation) release() {
	_, _, _ = o.vtbl.Release.Call(uintptr(unsafe.Pointer(o)))
}

func (o *iCoreWebView2DownloadOperation) addBytesReceivedChanged(handler *iDownloadEventHandler) (eventRegistrationToken, error) {
	return o.subscribe(o.vtbl.AddBytesReceivedChanged, handler)
}

func (o *iCoreWebView2DownloadOperation) addStateChanged(handler *iDownloadEventHandler) (eventRegistrationToken, error) {
	return o.subscribe(o.vtbl.AddStateChanged, handler)
}

func (o *iCoreWebView2DownloadOperation) subscribe(add comProc, handler *iDownloadEventHandler) (eventRegistrationToken, error) {
	var token eventRegistrationToken
	hr, _, _ := add.Call(uintptr(unsafe.Pointer(o)), uintptr(unsafe.Pointer(handler)), uintptr(unsafe.Pointer(&token)))
	return token, hresult(hr)
}

// removeBytesReceivedChanged is remove_BytesReceivedChanged([in]
// EventRegistrationToken): the 8-byte struct passes by value in one
// register.
func (o *iCoreWebView2DownloadOperation) removeBytesReceivedChanged(token eventRegistrationToken) error {
	hr, _, _ := o.vtbl.RemoveBytesReceivedChanged.Call(uintptr(unsafe.Pointer(o)), uintptr(token.value))
	return hresult(hr)
}

func (o *iCoreWebView2DownloadOperation) removeStateChanged(token eventRegistrationToken) error {
	hr, _, _ := o.vtbl.RemoveStateChanged.Call(uintptr(unsafe.Pointer(o)), uintptr(token.value))
	return hresult(hr)
}

func (o *iCoreWebView2DownloadOperation) uri() string {
	return comString(o.vtbl.GetURI, unsafe.Pointer(o))
}

func (o *iCoreWebView2DownloadOperation) resultFilePath() string {
	return comString(o.vtbl.GetResultFilePath, unsafe.Pointer(o))
}

func (o *iCoreWebView2DownloadOperation) totalBytes() int64 {
	var value int64
	_, _, _ = o.vtbl.GetTotalBytesToReceive.Call(uintptr(unsafe.Pointer(o)), uintptr(unsafe.Pointer(&value)))
	// -1 when the size is unknown.
	return max(value, 0)
}

func (o *iCoreWebView2DownloadOperation) bytesReceived() int64 {
	var value int64
	_, _, _ = o.vtbl.GetBytesReceived.Call(uintptr(unsafe.Pointer(o)), uintptr(unsafe.Pointer(&value)))
	return max(value, 0)
}

func (o *iCoreWebView2DownloadOperation) state() uint32 {
	value := uint32(downloadStateInProgress)
	_, _, _ = o.vtbl.GetState.Call(uintptr(unsafe.Pointer(o)), uintptr(unsafe.Pointer(&value)))
	return value
}

func (o *iCoreWebView2DownloadOperation) interruptReason() uint32 {
	var value uint32
	_, _, _ = o.vtbl.GetInterruptReason.Call(uintptr(unsafe.Pointer(o)), uintptr(unsafe.Pointer(&value)))
	return value
}

func (o *iCoreWebView2DownloadOperation) cancel() error {
	hr, _, _ := o.vtbl.Cancel.Call(uintptr(unsafe.Pointer(o)))
	return hresult(hr)
}

// comString calls a getter of an LPWSTR the callee allocated and the caller
// frees, answering "" on failure.
func comString(getter comProc, this unsafe.Pointer) string {
	var value *uint16
	hr, _, _ := getter.Call(uintptr(this), uintptr(unsafe.Pointer(&value)))
	if hresult(hr) != nil || value == nil {
		return ""
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(value))
	return windows.UTF16PtrToString(value)
}

// ---------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------

// iDownloadStartingHandler is ICoreWebView2DownloadStartingEventHandler.
type iDownloadStartingHandler struct {
	vtbl     *handlerVtbl
	ref      comRef
	starting callback[func(*iDownloadStartingEventArgs)]
}

var downloadStartingVtbl = handlerVtbl{
	iUnknownVtbl: handlerIUnknown,
	Invoke: newComProc(func(this *iDownloadStartingHandler, _ *iCoreWebView2, args *iDownloadStartingEventArgs) uintptr {
		if starting, ok := this.starting.load(); ok {
			starting(args)
		}
		return 0
	}),
}

func newDownloadStartingHandler(starting func(*iDownloadStartingEventArgs)) *iDownloadStartingHandler {
	h := &iDownloadStartingHandler{vtbl: &downloadStartingVtbl}
	h.starting.set(starting)
	h.ref.own()
	return h
}

// iDownloadEventHandler is both ICoreWebView2BytesReceivedChangedEventHandler
// and ICoreWebView2StateChangedEventHandler: each is Invoke(operation,
// IUnknown args) and the handler reads the operation it was made for.
type iDownloadEventHandler struct {
	vtbl    *handlerVtbl
	ref     comRef
	changed callback[func()]
}

var downloadEventVtbl = handlerVtbl{
	iUnknownVtbl: handlerIUnknown,
	Invoke: newComProc(func(this *iDownloadEventHandler, _ *iCoreWebView2DownloadOperation, _ uintptr) uintptr {
		if changed, ok := this.changed.load(); ok {
			changed()
		}
		return 0
	}),
}

func newDownloadEventHandler(changed func()) *iDownloadEventHandler {
	h := &iDownloadEventHandler{vtbl: &downloadEventVtbl}
	h.changed.set(changed)
	h.ref.own()
	return h
}
