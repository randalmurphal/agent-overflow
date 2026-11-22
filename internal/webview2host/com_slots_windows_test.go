//go:build windows

package webview2host

import (
	"testing"
	"unsafe"
)

// MoveFocus sits at slot 12 of ICoreWebView2ControllerVtbl in the official
// SDK header (com_windows.go, moveFocus). A handwritten vtable that drifted
// would call another method with the reason as its argument.
func TestControllerMoveFocusSlot(t *testing.T) {
	var vtbl iCoreWebView2ControllerVtbl
	slot := unsafe.Offsetof(vtbl.MoveFocus) / unsafe.Sizeof(comProc(0))
	if slot != 12 {
		t.Fatalf("MoveFocus is at slot %d, want 12", slot)
	}
	if moveFocusProgrammatic != 0 {
		t.Fatalf("COREWEBVIEW2_MOVE_FOCUS_REASON_PROGRAMMATIC is %d, want 0", moveFocusProgrammatic)
	}
}

// The slots a page's popups and the download dialog call, against the C
// vtables in WebView2.h of the Microsoft.Web.WebView2 1.0.3405.78 NuGet
// package.
func TestPopupSlots(t *testing.T) {
	slot := func(offset uintptr) uintptr { return offset / unsafe.Sizeof(comProc(0)) }
	var view iCoreWebView2_9Vtbl
	var controller iCoreWebView2ControllerVtbl
	var args iNewWindowRequestedEventArgsVtbl
	var deferral iCoreWebView2DeferralVtbl
	for name, got := range map[string][2]uintptr{
		"ICoreWebView2::add_NewWindowRequested":                   {slot(unsafe.Offsetof(view.AddNewWindowRequested)), 44},
		"ICoreWebView2::add_WindowCloseRequested":                 {slot(unsafe.Offsetof(view.AddWindowCloseRequested)), 59},
		"ICoreWebView2_9::add_IsDefaultDownloadDialogOpenChanged": {slot(unsafe.Offsetof(view.AddIsDefaultDownloadDialogOpenChanged)), 88},
		"ICoreWebView2_9::get_IsDefaultDownloadDialogOpen":        {slot(unsafe.Offsetof(view.GetIsDefaultDownloadDialogOpen)), 90},
		"ICoreWebView2_9::CloseDefaultDownloadDialog":             {slot(unsafe.Offsetof(view.CloseDefaultDownloadDialog)), 92},
		"ICoreWebView2Controller::get_Bounds":                     {slot(unsafe.Offsetof(controller.GetBounds)), 5},
		"NewWindowRequestedEventArgs::get_Uri":                    {slot(unsafe.Offsetof(args.GetURI)), 3},
		"NewWindowRequestedEventArgs::put_NewWindow":              {slot(unsafe.Offsetof(args.PutNewWindow)), 4},
		"NewWindowRequestedEventArgs::put_Handled":                {slot(unsafe.Offsetof(args.PutHandled)), 6},
		"NewWindowRequestedEventArgs::get_IsUserInitiated":        {slot(unsafe.Offsetof(args.GetIsUserInitiated)), 8},
		"NewWindowRequestedEventArgs::GetDeferral":                {slot(unsafe.Offsetof(args.GetDeferral)), 9},
		"ICoreWebView2Deferral::Complete":                         {slot(unsafe.Offsetof(deferral.Complete)), 3},
	} {
		if got[0] != got[1] {
			t.Errorf("%s is at slot %d, want %d", name, got[0], got[1])
		}
	}
}
