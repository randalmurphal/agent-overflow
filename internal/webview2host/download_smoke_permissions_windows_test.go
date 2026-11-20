//go:build windows

package webview2host

import (
	"testing"
	"unsafe"
)

func (s *downloadSmoke) permissions(state uint32) {
	s.ui.do(func() { s.perm.answer = state })
}

func (s *downloadSmoke) permissionKinds() []uint32 {
	var kinds []uint32
	s.ui.do(func() { kinds = append(kinds, s.perm.kinds...) })
	return kinds
}

// ---------------------------------------------------------------------
// PermissionRequested, for the smoke alone
// ---------------------------------------------------------------------

// COREWEBVIEW2_PERMISSION_KIND_MULTIPLE_AUTOMATIC_DOWNLOADS and
// COREWEBVIEW2_PERMISSION_STATE.
const (
	permissionMultipleDownloads = 7
	permissionDefault           = 0
	permissionAllow             = 1
)

type smokePermissionArgsVtbl struct {
	iUnknownVtbl
	GetURI             comProc
	GetPermissionKind  comProc
	GetIsUserInitiated comProc
	GetState           comProc
	PutState           comProc
}

type smokePermissionArgs struct {
	vtbl *smokePermissionArgsVtbl
}

// smokePermissions records the page's permission requests and answers each
// with answer; permissionDefault leaves WebView2's own prompt to the user.
// UI thread only.
type smokePermissions struct {
	vtbl   *handlerVtbl
	ref    comRef
	answer uint32
	kinds  []uint32
}

var smokePermissionsVtbl = handlerVtbl{
	iUnknownVtbl: handlerIUnknown,
	Invoke: newComProc(func(this *smokePermissions, _ *iCoreWebView2, args *smokePermissionArgs) uintptr {
		var kind uint32
		_, _, _ = args.vtbl.GetPermissionKind.Call(uintptr(unsafe.Pointer(args)), uintptr(unsafe.Pointer(&kind)))
		this.kinds = append(this.kinds, kind)
		if this.answer != permissionDefault {
			_, _, _ = args.vtbl.PutState.Call(uintptr(unsafe.Pointer(args)), uintptr(this.answer))
		}
		return 0
	}),
}

func watchSmokePermissions(t *testing.T, ui *smokeUIThread, page *hostPage) *smokePermissions {
	t.Helper()
	handler := &smokePermissions{vtbl: &smokePermissionsVtbl}
	handler.ref.own()
	var err error
	ui.do(func() {
		var token eventRegistrationToken
		hr, _, _ := page.view.vtbl.AddPermissionRequested.Call(uintptr(unsafe.Pointer(page.view)), uintptr(unsafe.Pointer(handler)), uintptr(unsafe.Pointer(&token)))
		err = hresult(hr)
	})
	// The view holds its own reference from a registration that succeeded.
	handler.ref.release()
	if err != nil {
		t.Fatalf("add_PermissionRequested: %v", err)
	}
	return handler
}
