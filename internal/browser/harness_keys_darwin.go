//go:build darwin && cgo && !ios && !server && !nogui

package browser

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa -framework WebKit
#include <stdlib.h>
#include "wkwebviewglue_darwin.h"
*/
import "C"

import (
	"fmt"
	"unsafe"

	"agent-overflow/internal/keybindings"
)

// PressKey is engineKeyPress for the WKWebView engine (harness only).
func (e *wkEngine) PressKey(handle string, chord keybindings.Accelerator) error {
	var page *wkPage
	wkPageByID.Range(func(_, value any) bool {
		if p := value.(*wkPage); p.handle == handle && p.engine == e {
			page = p
			return false
		}
		return true
	})
	if page == nil {
		return fmt.Errorf("browser: page %s is not live on this engine", handle)
	}
	key := C.CString(chord.Key)
	defer C.free(unsafe.Pointer(key))
	if !wkDo(func() {
		C.ao_wkv_view_press_key(page.view, key, flag(chord.Ctrl), flag(chord.Meta), flag(chord.Alt), flag(chord.Shift))
	}) {
		return errWKUnavailable
	}
	return nil
}

func flag(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

var _ engineKeyPress = (*wkEngine)(nil)
