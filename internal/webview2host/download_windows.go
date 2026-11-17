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

// downloadCancelBound is how long after the backend's cancel the launcher
// waits for WebView2 to end the download before ending it itself
// (settleCancel). On the real runtime WebView2 ends a running download, or
// a resume, within a few milliseconds of its cancel (4 ms and 2 ms
// measured), and a dropped download's old operation never; the bound is
// three orders of magnitude past the first and still clears a stuck row
// promptly.
const downloadCancelBound = 5 * time.Second

// hostDownload is one download a page started, from its DownloadStarting
// until it ends or its page goes.
type hostDownload struct {
	id    string
	page  *hostPage
	op    *iCoreWebView2DownloadOperation
	bytes *iDownloadEventHandler
	state *iDownloadEventHandler
	// bytesToken and stateToken remove the two subscriptions.
	bytesToken, stateToken eventRegistrationToken
	gate                   progressGate
	// uri and path are the snapshot's address and file; received and total
	// its bytes as last read from an operation.
	uri, path       string
	received, total int64
	// cancelRequested is the backend's cancel, applied to a resume too.
	cancelRequested bool
	// snapshots counts the snapshots taken, numbering each (Seq).
	snapshots uint64
	// stale holds the operations resumes replaced (retireOperation).
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
// operation is kept under a launcher id, and the backend hears its first
// snapshot.
//
// WebView2 resumes a download whose connection dropped by starting it
// again: a new DownloadStarting with a new operation carrying the
// interruption's reason, while the old operation stays in progress, reports
// nothing more and ignores a cancel. Measured on the real runtime, the old
// operation then reads as dropped (in progress with that reason) while a
// healthy download that never resumed reads 0, and the new operation saves
// to "name (N)" whether it continues from the bytes it has or restarts.
// Such a start is the dropped download's resume when exactly one running
// download from its address reads as dropped (resumedDownload): it takes
// the old operation's place under the same id, saving to the file the
// download started with, so one click is one row, one file and one end.
// Otherwise it is a new download saving where WebView2 chose.
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
	path, uri := args.resultFilePath(), op.uri()
	if reason := op.interruptReason(); reason != 0 {
		if d := page.resumedDownload(uri); d != nil {
			h.resumeDownload(d, args, op, path, reason)
			return
		}
	}
	h.downloadSeq++
	d := &hostDownload{id: downloadID(h.downloadToken, h.downloadSeq), page: page, path: path, uri: uri}
	h.attachOperation(d, op)
	if page.downloads == nil {
		page.downloads = make(map[string]*hostDownload)
	}
	page.downloads[d.id] = d
	h.report(page, d.snapshot(DownloadRunning, ""))
	// One that ended before its subscriptions were made reports nothing
	// more on its own.
	h.downloadStateChanged(d)
}

// resumedDownload answers the running download a resume from uri
// continues: the one running download from uri whose operation reads as
// dropped, in progress with a nonzero interrupt reason. A download that
// never resumed reads 0 while healthy, so it is never taken. A resume names
// nothing else of its download, so where none or several read as dropped
// it answers nil and the resume is a download of its own, every running
// download left as it is. Several read so when two downloads from one
// address dropped at once, or when one dropped beside a download that
// already resumed once, whose operation (a resume's) reads its reason for
// good, dropped or not.
func (p *hostPage) resumedDownload(uri string) *hostDownload {
	var dropped *hostDownload
	for _, d := range p.downloads {
		if d.op == nil || d.uri != uri || d.op.state() != downloadStateInProgress || d.op.interruptReason() == 0 {
			continue
		}
		if dropped != nil {
			return nil
		}
		dropped = d
	}
	return dropped
}

// resumeDownload makes op, which WebView2 started to resume d after an
// interruption for reason, d's operation. It saves to d's file: WebView2
// chose path while d's own file name was still taken, and on the real
// runtime a resume set back to it completes there byte for byte, whether
// it continues from the bytes it has or restarts. Should the runtime refuse
// the path, the download names the file it does write.
func (h *Host) resumeDownload(d *hostDownload, args *iDownloadStartingEventArgs, op *iCoreWebView2DownloadOperation, path string, reason uint32) {
	h.config.Logf("browser host: download %s: WebView2 resumes it after %s", d.id, downloadInterruptReason(reason))
	if !strings.EqualFold(path, d.path) {
		if err := args.putResultFilePath(d.path); err != nil {
			h.config.Logf("browser host: download %s: keep its file %s: %v", d.id, d.path, err)
			d.path = path
		}
	}
	h.retireOperation(d)
	h.attachOperation(d, op)
	if d.cancelRequested {
		if err := op.cancel(); err != nil {
			h.config.Logf("browser host: cancel download %s: %v", d.id, err)
		}
	}
	h.downloadStateChanged(d)
}

// snapshot is the download's whole state as a report, numbered after the
// last one taken.
func (d *hostDownload) snapshot(state, reason string) DownloadReport {
	if d.op != nil {
		d.received, d.total = d.op.bytesReceived(), d.op.totalBytes()
	}
	d.snapshots++
	return DownloadReport{
		ID: d.id, Seq: d.snapshots, State: state, URI: d.uri, Name: windowsFileName(d.path), Path: d.path,
		Received: d.received, Total: d.total, Reason: reason,
	}
}

// windowsFileName is the last element of a Windows path.
func windowsFileName(path string) string {
	return path[strings.LastIndexAny(path, `\/`)+1:]
}

// downloadOp is one operation a download had, with its subscriptions.
type downloadOp struct {
	op                     *iCoreWebView2DownloadOperation
	bytes, state           *iDownloadEventHandler
	bytesToken, stateToken eventRegistrationToken
}

// retireOperation stops d's operation from reporting and keeps it until d
// ends: a resume's operation shares the download with the one it replaces,
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

// downloadBytes reports a download's snapshot as its bytes change, at most
// once per DownloadProgressInterval.
func (h *Host) downloadBytes(d *hostDownload) {
	if d.op == nil || !d.gate.due(time.Now()) {
		return
	}
	h.report(d.page, d.snapshot(DownloadRunning, ""))
}

// downloadStateChanged follows the operation's state: completed or
// interrupted ends the download. WebView2 resumes a download as a new
// DownloadStarting (downloadStarting), never as this operation coming back
// to in progress, and raises no interruption before it. Measured on the
// real runtime for a dropped connection, a body shorter than its
// Content-Length and a refused Range: the reasons that arrived were the
// length mismatch (24), at the end of WebView2's own retries, and the
// user's cancel; FILE_TOO_SHORT, FILE_HASH_MISMATCH and SERVER_NO_RANGE,
// which WebView2 documents as resumed by itself, never did.
func (h *Host) downloadStateChanged(d *hostDownload) {
	if d.op == nil {
		return
	}
	switch d.op.state() {
	case downloadStateCompleted:
		if path := d.op.resultFilePath(); path != "" {
			d.path = path
		}
		h.endDownload(d, d.snapshot(DownloadCompleted, ""))
	case downloadStateInterrupted:
		reason := d.op.interruptReason()
		terminal := d.snapshot(DownloadInterrupted, downloadInterruptReason(reason))
		// Nothing WebView2 might continue on this operation runs unobserved
		// once it is let go. On the real runtime the cancel of an
		// interrupted operation succeeds and leaves its state and reason.
		if reason != downloadUserCanceled {
			if err := d.op.cancel(); err != nil {
				h.config.Logf("browser host: cancel interrupted download %s: %v", d.id, err)
			}
		}
		h.endDownload(d, terminal)
	}
}

// report sends one snapshot of a page's download.
func (h *Host) report(page *hostPage, r DownloadReport) {
	detail, err := r.Encode()
	if err != nil {
		h.config.Logf("browser host: download %s: %v", r.ID, err)
		return
	}
	h.config.Report(page.id, ReportDownload, detail)
}

// endDownload ends a download with its terminal snapshot: every operation
// it had is let go, the snapshot is kept for a directive that names the
// download later, and the backend hears it.
func (h *Host) endDownload(d *hostDownload, terminal DownloadReport) {
	h.releaseOperations(d)
	delete(d.page.downloads, d.id)
	d.page.ended.add(terminal)
	h.report(d.page, terminal)
}

// releaseOperations lets go of every operation d had.
func (h *Host) releaseOperations(d *hostDownload) {
	h.detachOperation(d)
	for _, stale := range d.stale {
		d.op, d.bytes, d.state, d.bytesToken, d.stateToken = stale.op, stale.bytes, stale.state, stale.bytesToken, stale.stateToken
		h.detachOperation(d)
	}
	d.stale = nil
}

// answerDownload answers a directive naming one of the page's downloads
// with its snapshot, marked as an answer. A cancel cancels a running
// download first, and its end follows from the operation's state change,
// or from settleCancel when WebView2 reports none. An ended download's
// answer is its terminal snapshot again; an id with no record gets
// DownloadUnknown.
func (h *Host) answerDownload(page *hostPage, id string, cancel bool) {
	d := page.downloads[id]
	if d == nil {
		h.report(page, page.ended.answer(id))
		return
	}
	if cancel && d.op != nil {
		first := !d.cancelRequested
		// Kept for a resume that replaces the operation.
		d.cancelRequested = true
		if err := d.op.cancel(); err != nil {
			h.config.Logf("browser host: cancel download %s: %v", id, err)
		}
		if first {
			h.settleCancelAfter(d)
		}
	}
	answer := d.snapshot(DownloadRunning, "")
	answer.Answer = true
	h.report(page, answer)
}

// settleCancelAfter ends a canceled download as canceled once
// downloadCancelBound passes with no end from WebView2 (settleCancel).
func (h *Host) settleCancelAfter(d *hostDownload) {
	after := h.cancelTimer
	if after == nil {
		after = func(wait time.Duration, fn func()) { time.AfterFunc(wait, fn) }
	}
	after(downloadCancelBound, func() {
		h.mu.Lock()
		live := h.pages[d.page.id] == d.page
		h.mu.Unlock()
		if live {
			h.config.OnMain(func() { h.settleCancel(d) })
		}
	})
}

// settleCancel ends a download the backend canceled that WebView2 has not
// ended: the operation of a dropped download whose resume became a row of
// its own stays in progress and never ends, so its row would run for good.
// It ends canceled, its handlers detached, and its operations go. Measured
// on the real runtime: the old operation and its resume are one download in
// WebView2, so this cancel ends that resume too, at once and by WebView2's
// own report, and once the resume finished the cancel and the release
// leave its file as it is. The cost: a download that really finishes after
// the bound is listed canceled, and its file stays in the Downloads folder,
// listed nowhere; its late state change reaches no handler.
func (h *Host) settleCancel(d *hostDownload) {
	if d.page.downloads[d.id] != d {
		// Ended by WebView2's own report, or its page went.
		return
	}
	h.config.Logf("browser host: download %s: WebView2 did not end it within %v of its cancel; it ends canceled here", d.id, downloadCancelBound)
	h.endDownload(d, d.snapshot(DownloadInterrupted, downloadInterruptReason(downloadUserCanceled)))
}

// retireDownloads cancels and lets go every download of a page that is
// going, with every terminal snapshot it kept. The backend settles them as
// its page leaves; nothing is reported.
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
		h.releaseOperations(d)
	}
	page.downloads = nil
	page.ended = endedDownloads{}
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

// putResultFilePath is put_ResultFilePath([in] LPCWSTR): the file the
// download saves to.
func (a *iDownloadStartingEventArgs) putResultFilePath(path string) error {
	value, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	hr, _, _ := a.vtbl.PutResultFilePath.Call(uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(value)))
	return hresult(hr)
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
