package webview2host

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The loader reads each option string from the COM allocation up to its
// NUL, so the copy must carry every code unit and the terminator.
func TestCoTaskMemStringCopiesTheStringAndItsTerminator(t *testing.T) {
	for _, want := range []string{"", "--remote-debugging-port=0 --lang=fr-CA", "café \U0001F600"} {
		ptr := coTaskMemString(want)
		if ptr == 0 {
			t.Fatalf("coTaskMemString(%q): allocation failed", want)
		}
		// The address is COM task memory, read back without converting the
		// integer to a Go pointer.
		got := windows.UTF16PtrToString(*(**uint16)(unsafe.Pointer(&ptr)))
		windows.CoTaskMemFree(*(*unsafe.Pointer)(unsafe.Pointer(&ptr)))
		if got != want {
			t.Fatalf("coTaskMemString(%q) holds %q", want, got)
		}
	}
}
