//go:build windows

package webview2host

import (
	"runtime"
	"testing"
	"weak"
)

// WebView2 can call or release a callback object after the page or clear
// that made it dropped its own reference, and the objects' IUnknown counts
// nothing, so every object handed to WebView2 must survive collection with
// no other Go reference.
func TestEveryWebView2CallbackObjectSurvivesCollection(t *testing.T) {
	alive := map[string]func() bool{}
	options, _ := newEnvOptions("--x", "")
	track(alive, "environment options", options)
	track(alive, "environment completed", newEnvironmentCompletedHandler(func(uintptr, *iCoreWebView2Environment) {}))
	track(alive, "controller completed", newControllerCompletedHandler(func(uintptr, *iCoreWebView2Controller) {}))
	track(alive, "devtools completed", newDevToolsCompletedHandler(func(uintptr, string) {}))
	track(alive, "find start completed", newFindStartCompletedHandler(func(uintptr) {}))
	track(alive, "accelerator key pressed", newAcceleratorKeyPressedHandler(func(*iAcceleratorKeyPressedEventArgs) {}))
	track(alive, "process failed", newProcessFailedHandler(func(uint32) {}))

	for range 3 {
		runtime.GC()
	}
	for name, ok := range alive {
		if !ok() {
			t.Errorf("the %s object was collected while WebView2 could still use it", name)
		}
	}
}

func track[T any](alive map[string]func() bool, name string, obj *T) {
	p := weak.Make(obj)
	alive[name] = func() bool { return p.Value() != nil }
}
