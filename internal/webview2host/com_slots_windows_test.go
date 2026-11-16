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
