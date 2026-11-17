//go:build windows

package webview2host

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// WebView2's default download dialog is a window of its own over the page.
// The launcher takes every download a page starts (download_windows.go), so
// the dialog has nothing of AO's to show, yet WebView2 opens it on its own
// in one case: a popup whose first navigation is a download asks to close
// (WindowCloseRequested) and WebView2 then opens its opener's dialog for
// that download. Measured on the real runtime (popup_smoke_windows_test.go,
// runtime 157), nothing a host sets prevents it. DownloadStarting is raised
// on the popup's view only, whose handler, like every handler of the view,
// is attached before put_NewWindow and before the deferral completes, and
// reads Handled back as set; the opener raises none for it. Closing the
// popup inside its close request changes nothing, and a margin that would
// place the dialog off its page is clamped onto the screen. So every page
// closes its dialog as soon as WebView2 says it opened: about 35 ms after
// the popup's close request, and closed about 6 ms later, it shows through
// WebView2's own close animation, about 0.22 s, and nothing on it can be
// used.

// iCoreWebView2_9Vtbl is the flattened chain through ICoreWebView2_9 (C
// vtable ICoreWebView2_9Vtbl in WebView2.h, SDK 1.0.3405.78): slots 0-60 are
// iCoreWebView2Vtbl, 61-87 the methods ICoreWebView2_2 through
// ICoreWebView2_8 add (7+5+4+2+1+1+7), then ICoreWebView2_9's own.
type iCoreWebView2_9Vtbl struct {
	iCoreWebView2Vtbl
	_                                        [27]comProc
	AddIsDefaultDownloadDialogOpenChanged    comProc
	RemoveIsDefaultDownloadDialogOpenChanged comProc
	GetIsDefaultDownloadDialogOpen           comProc
	OpenDefaultDownloadDialog                comProc
	CloseDefaultDownloadDialog               comProc
}

type iCoreWebView2_9 struct {
	vtbl *iCoreWebView2_9Vtbl
}

// iidWebView2_9 is IID_ICoreWebView2_9, {4d7b2eab-9fdc-468d-b998-a9260b5ed651}.
var iidWebView2_9 = windows.GUID{
	Data1: 0x4d7b2eab, Data2: 0x9fdc, Data3: 0x468d,
	Data4: [8]byte{0xb9, 0x98, 0xa9, 0x26, 0x0b, 0x5e, 0xd6, 0x51},
}

// queryWebView9 returns nil on a runtime without the default download
// dialog API.
func (v *iCoreWebView2) queryWebView9() *iCoreWebView2_9 {
	var result *iCoreWebView2_9
	_, _, _ = v.vtbl.QueryInterface.Call(
		uintptr(unsafe.Pointer(v)),
		uintptr(unsafe.Pointer(&iidWebView2_9)),
		uintptr(unsafe.Pointer(&result)),
	)
	return result
}

func (v *iCoreWebView2_9) release() {
	_, _, _ = v.vtbl.Release.Call(uintptr(unsafe.Pointer(v)))
}

// addDownloadDialogOpenChanged is add_IsDefaultDownloadDialogOpenChanged.
func (v *iCoreWebView2_9) addDownloadDialogOpenChanged(handler *iViewEventHandler) error {
	var token eventRegistrationToken
	hr, _, _ := v.vtbl.AddIsDefaultDownloadDialogOpenChanged.Call(
		uintptr(unsafe.Pointer(v)),
		uintptr(unsafe.Pointer(handler)),
		uintptr(unsafe.Pointer(&token)),
	)
	return hresult(hr)
}

// downloadDialogOpen is get_IsDefaultDownloadDialogOpen([out, retval] BOOL*).
func (v *iCoreWebView2_9) downloadDialogOpen() (bool, error) {
	var open int32
	hr, _, _ := v.vtbl.GetIsDefaultDownloadDialogOpen.Call(uintptr(unsafe.Pointer(v)), uintptr(unsafe.Pointer(&open)))
	return open != 0, hresult(hr)
}

// closeDownloadDialog is CloseDefaultDownloadDialog().
func (v *iCoreWebView2_9) closeDownloadDialog() error {
	hr, _, _ := v.vtbl.CloseDefaultDownloadDialog.Call(uintptr(unsafe.Pointer(v)))
	return hresult(hr)
}

// watchDownloadDialog closes the page's default download dialog whenever
// WebView2 opens it. A runtime without the API has no way to say so, and
// the dialog stays until the user closes it.
func (h *Host) watchDownloadDialog(page *hostPage, view *iCoreWebView2) {
	view9 := view.queryWebView9()
	if view9 == nil {
		h.config.Logf("browser host: page %s: the runtime has no default download dialog API; a dialog WebView2 opens on its own stays open", page.id)
		return
	}
	defer view9.release()
	handler := newViewEventHandler(func() { h.closeDownloadDialog(page) })
	page.dialogOpened = handler
	if err := view9.addDownloadDialogOpenChanged(handler); err != nil {
		h.config.Logf("browser host: page %s download dialog subscription: %v", page.id, err)
	}
	handler.ref.release()
}

// closeDownloadDialog closes the page's default download dialog if it is
// open. Runs on the UI thread, in the event that says it changed.
func (h *Host) closeDownloadDialog(page *hostPage) {
	if page.view == nil {
		return
	}
	view9 := page.view.queryWebView9()
	if view9 == nil {
		return
	}
	defer view9.release()
	open, err := view9.downloadDialogOpen()
	if err != nil || !open {
		return
	}
	if err := view9.closeDownloadDialog(); err != nil {
		h.config.Logf("browser host: page %s: close WebView2's download dialog: %v", page.id, err)
		return
	}
	h.config.Logf("browser host: page %s: closed the download dialog WebView2 opened", page.id)
}
