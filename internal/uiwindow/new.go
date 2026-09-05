package uiwindow

import "github.com/wailsapp/wails/v3/pkg/application"

// New creates an app-shell window. All frontend events use our HTTP/WS
// transport; Wails' automatic window-event JavaScript has no consumer.
func New(app *application.App, options application.WebviewWindowOptions) *application.WebviewWindow {
	return app.Window.NewWithOptions(shellOptions(options))
}

func shellOptions(options application.WebviewWindowOptions) application.WebviewWindowOptions {
	// A no-op evaluateJavaScript between mousedown and click clears WebKit's
	// transient activation and breaks clipboard writes. Keep native callbacks
	// (geometry, lifecycle, page tickets) while disabling the unused JS path.
	options.DisableWindowEventForwarding = true
	return options
}
