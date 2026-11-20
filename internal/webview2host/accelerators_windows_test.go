//go:build windows

package webview2host

import (
	"fmt"
	"strings"
	"testing"
	"unsafe"
)

// fakeKeyArgs is AcceleratorKeyPressedEventArgs whose kind and key reads
// can fail.
type fakeKeyArgs struct {
	vtbl             *iAcceleratorKeyPressedEventArgsVtbl
	kindErr, keyErr  bool
	kind, virtualKey uint32
	handled          bool
}

var fakeKeyArgsVtbl = iAcceleratorKeyPressedEventArgsVtbl{
	iUnknownVtbl: iUnknownVtbl{QueryInterface: newComProc(noInterface)},
	GetKeyEventKind: newComProc(func(this *fakeKeyArgs, out *uint32) uintptr {
		if this.kindErr {
			return 0x80004005 // E_FAIL
		}
		*out = this.kind
		return 0
	}),
	GetVirtualKey: newComProc(func(this *fakeKeyArgs, out *uint32) uintptr {
		if this.keyErr {
			return 0x80004005 // E_FAIL
		}
		*out = this.virtualKey
		return 0
	}),
	PutHandled: newComProc(func(this *fakeKeyArgs, value uintptr) uintptr {
		this.handled = value != 0
		return 0
	}),
}

// A key event whose kind or key the runtime does not answer is left to the
// page and logged, never read as a key it is not.
func TestAKeyEventThatCannotBeReadGoesToThePage(t *testing.T) {
	for _, tc := range []struct {
		name            string
		kindErr, keyErr bool
		want            string
	}{
		{"its kind", true, false, "read the key event kind"},
		{"its key", false, true, "read the virtual key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged []string
			h := &Host{config: Config{
				Logf:   func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) },
				Report: func(pageID string, kind ReportKind, detail string) { t.Errorf("reported %s %s", kind, detail) },
			}}
			args := &fakeKeyArgs{vtbl: &fakeKeyArgsVtbl, kindErr: tc.kindErr, keyErr: tc.keyErr, kind: keyEventKindKeyDown, virtualKey: 'A'}
			h.acceleratorPressed(&hostPage{id: "p"}, (*iAcceleratorKeyPressedEventArgs)(unsafe.Pointer(args)))
			if args.handled {
				t.Fatal("an unread key event was taken from the page")
			}
			if len(logged) != 1 || !strings.Contains(logged[0], tc.want) || !strings.Contains(logged[0], "page p") {
				t.Fatalf("logged %q, want the failed read", logged)
			}
		})
	}
}
