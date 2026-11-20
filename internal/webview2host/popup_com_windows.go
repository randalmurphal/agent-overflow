//go:build windows

package webview2host

import (
	"errors"
	"fmt"
	"unsafe"
)

// The WebView2 COM interfaces a page's popups use (popup_windows.go):
// ICoreWebView2's NewWindowRequested and WindowCloseRequested, the new-window
// event args, the deferral, and the two handler shapes. Vtables follow the C
// vtables in WebView2.h of the Microsoft.Web.WebView2 1.0.3405.78 NuGet
// package.

// addNewWindowRequested is add_NewWindowRequested([in]
// ICoreWebView2NewWindowRequestedEventHandler*, [out]
// EventRegistrationToken*), for the view's life: the token is never
// needed.
func (v *iCoreWebView2) addNewWindowRequested(handler *iNewWindowRequestedHandler) error {
	var token eventRegistrationToken
	hr, _, _ := v.vtbl.AddNewWindowRequested.Call(
		uintptr(unsafe.Pointer(v)),
		uintptr(unsafe.Pointer(handler)),
		uintptr(unsafe.Pointer(&token)),
	)
	return hresult(hr)
}

// addWindowCloseRequested is add_WindowCloseRequested([in]
// ICoreWebView2WindowCloseRequestedEventHandler*, [out]
// EventRegistrationToken*), for the view's life: the token is never
// needed.
func (v *iCoreWebView2) addWindowCloseRequested(handler *iViewEventHandler) error {
	var token eventRegistrationToken
	hr, _, _ := v.vtbl.AddWindowCloseRequested.Call(
		uintptr(unsafe.Pointer(v)),
		uintptr(unsafe.Pointer(handler)),
		uintptr(unsafe.Pointer(&token)),
	)
	return hresult(hr)
}

// ---------------------------------------------------------------------
// ICoreWebView2NewWindowRequestedEventArgs
// ---------------------------------------------------------------------

type iNewWindowRequestedEventArgsVtbl struct {
	iUnknownVtbl
	GetURI             comProc
	PutNewWindow       comProc
	GetNewWindow       comProc
	PutHandled         comProc
	GetHandled         comProc
	GetIsUserInitiated comProc
	GetDeferral        comProc
	GetWindowFeatures  comProc
}

type iNewWindowRequestedEventArgs struct {
	vtbl *iNewWindowRequestedEventArgsVtbl
}

func (a *iNewWindowRequestedEventArgs) addRef() {
	_, _, _ = a.vtbl.AddRef.Call(uintptr(unsafe.Pointer(a)))
}

func (a *iNewWindowRequestedEventArgs) release() {
	_, _, _ = a.vtbl.Release.Call(uintptr(unsafe.Pointer(a)))
}

// uri is get_Uri: the address the new window is asked to open.
func (a *iNewWindowRequestedEventArgs) uri() (string, error) {
	uri, err := comString(a.vtbl.GetURI, unsafe.Pointer(a))
	if err != nil {
		return "", fmt.Errorf("read the new window's address: %w", err)
	}
	return uri, nil
}

// putNewWindow is put_NewWindow([in] ICoreWebView2*): the view the new
// window opens in, instead of a window of WebView2's own.
func (a *iNewWindowRequestedEventArgs) putNewWindow(view *iCoreWebView2) error {
	hr, _, _ := a.vtbl.PutNewWindow.Call(uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(view)))
	return hresult(hr)
}

// putHandled is put_Handled([in] BOOL): with no new window put, true opens
// none.
func (a *iNewWindowRequestedEventArgs) putHandled(value bool) error {
	hr, _, _ := a.vtbl.PutHandled.Call(uintptr(unsafe.Pointer(a)), boolArg(value))
	return hresult(hr)
}

// deferral is GetDeferral([out, retval] ICoreWebView2Deferral**), with the
// reference the caller owns.
func (a *iNewWindowRequestedEventArgs) deferral() (*iCoreWebView2Deferral, error) {
	var deferral *iCoreWebView2Deferral
	hr, _, _ := a.vtbl.GetDeferral.Call(uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(&deferral)))
	if err := hresult(hr); err != nil {
		return nil, err
	}
	if deferral == nil {
		return nil, errors.New("GetDeferral returned no deferral")
	}
	return deferral, nil
}

// ---------------------------------------------------------------------
// ICoreWebView2Deferral
// ---------------------------------------------------------------------

type iCoreWebView2DeferralVtbl struct {
	iUnknownVtbl
	Complete comProc
}

type iCoreWebView2Deferral struct {
	vtbl *iCoreWebView2DeferralVtbl
}

func (d *iCoreWebView2Deferral) complete() error {
	hr, _, _ := d.vtbl.Complete.Call(uintptr(unsafe.Pointer(d)))
	return hresult(hr)
}

func (d *iCoreWebView2Deferral) release() {
	_, _, _ = d.vtbl.Release.Call(uintptr(unsafe.Pointer(d)))
}

// ---------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------

// iNewWindowRequestedHandler is ICoreWebView2NewWindowRequestedEventHandler.
// Detached, it still refuses the window: a request WebView2 raises after
// the page went must not fall through to a window of WebView2's own, and a
// refusal WebView2 does not take goes to refuseFailed.
type iNewWindowRequestedHandler struct {
	vtbl         *handlerVtbl
	ref          comRef
	requested    callback[func(*iNewWindowRequestedEventArgs)]
	refuseFailed func(error)
}

var newWindowRequestedVtbl = handlerVtbl{
	iUnknownVtbl: handlerIUnknown,
	Invoke: newComProc(func(this *iNewWindowRequestedHandler, _ *iCoreWebView2, args *iNewWindowRequestedEventArgs) uintptr {
		if args == nil {
			return 0
		}
		if requested, ok := this.requested.load(); ok {
			requested(args)
		} else if err := args.putHandled(true); err != nil {
			this.refuseFailed(err)
		}
		return 0
	}),
}

func newNewWindowRequestedHandler(requested func(*iNewWindowRequestedEventArgs), refuseFailed func(error)) *iNewWindowRequestedHandler {
	h := &iNewWindowRequestedHandler{vtbl: &newWindowRequestedVtbl, refuseFailed: refuseFailed}
	h.requested.set(requested)
	h.ref.own()
	return h
}

// iViewEventHandler is a view event handler whose args carry nothing the
// host reads: ICoreWebView2WindowCloseRequestedEventHandler and
// ICoreWebView2IsDefaultDownloadDialogOpenChangedEventHandler, each
// Invoke(ICoreWebView2* sender, IUnknown* args).
type iViewEventHandler struct {
	vtbl  *handlerVtbl
	ref   comRef
	event callback[func()]
}

var viewEventVtbl = handlerVtbl{
	iUnknownVtbl: handlerIUnknown,
	Invoke: newComProc(func(this *iViewEventHandler, _ *iCoreWebView2, _ uintptr) uintptr {
		if event, ok := this.event.load(); ok {
			event()
		}
		return 0
	}),
}

func newViewEventHandler(event func()) *iViewEventHandler {
	h := &iViewEventHandler{vtbl: &viewEventVtbl}
	h.event.set(event)
	h.ref.own()
	return h
}

// ---------------------------------------------------------------------
// ICoreWebView2Controller bounds
// ---------------------------------------------------------------------

// bounds is get_Bounds([out, retval] RECT*).
func (c *iCoreWebView2Controller) bounds() (rect, error) {
	var bounds rect
	hr, _, _ := c.vtbl.GetBounds.Call(uintptr(unsafe.Pointer(c)), uintptr(unsafe.Pointer(&bounds)))
	return bounds, hresult(hr)
}
