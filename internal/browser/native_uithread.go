//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"time"

	"agent-overflow/internal/browser/uicall"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// nativeCallTimeout bounds each wait for the UI thread. The app loop can
// already be gone during shutdown, and a browser operation must fail
// rather than park a goroutine on a queue nothing will drain again.
const nativeCallTimeout = 10 * time.Second

// uiThread is the native engines' one way onto the UI thread Wails' app
// loop owns, the GTK main thread or the AppKit main thread, where every
// GTK, WebKit and AppKit call runs: Run for an operation, Release for
// giving up what is held (uicall). Wails runs a closure posted on that
// thread inline.
var uiThread = uicall.NewThread(
	application.InvokeAsync, //nolint:forbidigo // uicall's adapter: the one raw post to the UI thread.
	func() bool { return application.Get() != nil },
	nativeCallTimeout,
)
