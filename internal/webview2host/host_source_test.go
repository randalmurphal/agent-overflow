package webview2host

import (
	"os"
	"strings"
	"testing"
)

// TestWindowsControllerIsCreatedUnderFinalClipParent is a platform-neutral
// tripwire over the Windows-only host. A live WebView2 controller that is
// reparented after creation can present stale or blank strips while scrolling;
// CI on macOS/Linux still needs to enforce the creation order that prevents it.
func TestWindowsControllerIsCreatedUnderFinalClipParent(t *testing.T) {
	source, err := os.ReadFile("host_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	container := strings.Index(text, "container, err := createClipContainer(hwnd)")
	controller := strings.Index(text, "createControllerWithOptions(parent, options, handler)")
	if container < 0 || controller < 0 || container > controller {
		t.Fatalf("Windows host must create the clip container before its WebView2 controller (container=%d controller=%d)", container, controller)
	}
	if strings.Contains(text, "putParentWindow(") {
		t.Fatal("Windows host reparents a live WebView2 controller; pass its final parent at creation instead")
	}
}
