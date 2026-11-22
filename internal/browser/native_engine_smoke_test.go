//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// nativeBrowserSmokeEnv gates the tests that drive this platform's real
// in-process engine: WebKitGTK on Linux, WKWebView on macOS. They open a
// window, so they run only when asked:
//
//	AO_NATIVE_BROWSER_SMOKE=1 make go-test GO_TEST_PKGS=./internal/browser GO_TEST_FLAGS='-run TestNativeEngineReal -count=1'
//
// On Linux the run starts its own headless weston and runs inside it. A
// window on the session display, the only place macOS has for one, needs
// AO_NATIVE_BROWSER_SMOKE_SESSION_DISPLAY=1 as well, and the run is refused
// without it (nativeSmokeDisplay). Nothing else runs them.
const nativeBrowserSmokeEnv = "AO_NATIVE_BROWSER_SMOKE"

// nativeSmokeWindow is the window the gated run hosts pages in, nil unless
// the gate is set.
var nativeSmokeWindow atomic.Pointer[application.WebviewWindow]

// smokeWindow answers the window the gated run hosts pages in, and skips t
// in a run without the gate. It reads the gate inside the test, so the gate
// is an input `go test` caches the result by: a run with the gate never
// reuses the skip of one without it.
func smokeWindow(t *testing.T) *application.WebviewWindow {
	t.Helper()
	_ = os.Getenv(nativeBrowserSmokeEnv)
	window := nativeSmokeWindow.Load()
	if window == nil {
		t.Skipf("set %s=1 to drive this platform's browser engine in a real window", nativeBrowserSmokeEnv)
	}
	return window
}

// nativeSmokeMain runs the package's tests for TestMain when the gate is
// set, and answers ran false when it is not.
func nativeSmokeMain(m *testing.M) (code int, ran bool) {
	if os.Getenv(nativeBrowserSmokeEnv) != "1" {
		return 0, false
	}
	weston, _ := exec.LookPath("weston")
	plan, err := nativeSmokeDisplay(os.Getenv, weston, runtime.GOOS)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1, true
	}
	if plan == smokeUnderWeston {
		return runUnderHeadlessWeston(weston), true
	}
	return runInNativeWindow(m), true
}

// runInNativeWindow runs the tests inside an application loop with one
// window, which is what uiThread posts to. The loop owns the main
// thread, so the tests run beside it and the process exits when they end.
func runInNativeWindow(m *testing.M) int {
	// The engine and the window's own webview inherit these, so nothing the
	// run starts writes into the developer's home, and its downloads land
	// in the temporary home's Downloads folder.
	home, err := os.MkdirTemp("", "ao-native-browser-smoke-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "native browser smoke: %v\n", err)
		return 1
	}
	if err := useTestHome(home); err != nil {
		fmt.Fprintf(os.Stderr, "native browser smoke: %v\n", err)
		return 1
	}
	// The gate is this process's alone: a test binary a test starts as its
	// helper runs plain, with no window of its own.
	if err := os.Unsetenv(nativeBrowserSmokeEnv); err != nil {
		fmt.Fprintf(os.Stderr, "native browser smoke: %v\n", err)
		return 1
	}
	// GTK makes two processes of one application name one application: the
	// second only activates the first, which would run the tests again. The
	// pid keeps another smoke run under way from being that process.
	app := application.New(application.Options{Name: fmt.Sprintf("ao-native-browser-smoke-%d", os.Getpid())})
	var started atomic.Bool
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		// GTK reports every activation of the application as a start. Only
		// the first runs the tests.
		if !started.CompareAndSwap(false, true) {
			return
		}
		window := app.Window.NewWithOptions(application.WebviewWindowOptions{
			Title: "AO native browser smoke", Width: 1280, Height: 800,
			// Grey, so a pixel check tells a page's white from the window.
			HTML: `<!doctype html><title>AO native browser smoke</title><body style="background:#808080">`,
		})
		go func() {
			code := 1
			if awaitNativeHandle(window) {
				nativeSmokeWindow.Store(window)
				code = m.Run()
			} else {
				fmt.Fprintln(os.Stderr, "native browser smoke: the window never got a native handle")
			}
			if err := os.RemoveAll(home); err != nil {
				fmt.Fprintf(os.Stderr, "native browser smoke: remove %s: %v\n", home, err)
			}
			// Exit here rather than quit the loop: quitting can end the
			// process with its own status, which would hide a failure.
			os.Exit(code)
		}()
	})
	if err := app.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "native browser smoke: %v\n", err)
	}
	return 1
}

// awaitNativeHandle waits for the window's platform handle, which the loop
// creates after NewWithOptions returns.
func awaitNativeHandle(window *application.WebviewWindow) bool {
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		//nolint:forbidigo // The smoke's own window, read before any engine exists; no engine call.
		if application.InvokeSyncWithResult(window.NativeWindow) != nil {
			return true
		}
	}
	return false
}
