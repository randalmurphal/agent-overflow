//go:build windows

package webview2host

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The WebView2 COM interfaces a page's downloads use (download_windows.go):
// ICoreWebView2_4's DownloadStarting, its event args, the download
// operation, and the two handler shapes.

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

// addDownloadStarting subscribes handler for the view's life; its
// registration token is never needed.
func (v *iCoreWebView2_4) addDownloadStarting(handler *iDownloadStartingHandler) error {
	var token eventRegistrationToken
	hr, _, _ := v.vtbl.AddDownloadStarting.Call(
		uintptr(unsafe.Pointer(v)),
		uintptr(unsafe.Pointer(handler)),
		uintptr(unsafe.Pointer(&token)),
	)
	return hresult(hr)
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

// uri is get_Uri, or an error when the runtime does not answer it.
func (o *iCoreWebView2DownloadOperation) uri() (string, error) {
	uri, err := comStringResult(o.vtbl.GetURI, unsafe.Pointer(o))
	if err != nil {
		return "", fmt.Errorf("read its address: %w", err)
	}
	return uri, nil
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

func (o *iCoreWebView2DownloadOperation) state() (uint32, error) {
	var value uint32
	hr, _, _ := o.vtbl.GetState.Call(uintptr(unsafe.Pointer(o)), uintptr(unsafe.Pointer(&value)))
	if err := hresult(hr); err != nil {
		return 0, fmt.Errorf("read its state: %w", err)
	}
	return value, nil
}

func (o *iCoreWebView2DownloadOperation) interruptReason() (uint32, error) {
	var value uint32
	hr, _, _ := o.vtbl.GetInterruptReason.Call(uintptr(unsafe.Pointer(o)), uintptr(unsafe.Pointer(&value)))
	if err := hresult(hr); err != nil {
		return 0, fmt.Errorf("read its interrupt reason: %w", err)
	}
	return value, nil
}

func (o *iCoreWebView2DownloadOperation) cancel() error {
	hr, _, _ := o.vtbl.Cancel.Call(uintptr(unsafe.Pointer(o)))
	return hresult(hr)
}

// comString calls a getter of an LPWSTR the callee allocated and the caller
// frees, answering "" on failure.
func comString(getter comProc, this unsafe.Pointer) string {
	value, _ := comStringResult(getter, this)
	return value
}

// comStringResult is comString with the failure: an HRESULT, or no string.
func comStringResult(getter comProc, this unsafe.Pointer) (string, error) {
	var value *uint16
	hr, _, _ := getter.Call(uintptr(this), uintptr(unsafe.Pointer(&value)))
	if err := hresult(hr); err != nil {
		return "", err
	}
	if value == nil {
		return "", errors.New("the runtime returned no string")
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(value))
	return windows.UTF16PtrToString(value), nil
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
