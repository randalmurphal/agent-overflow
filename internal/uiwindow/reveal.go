package uiwindow

import "github.com/wailsapp/wails/v3/pkg/application"

// Reveal brings the window in front of the user without changing its
// size: show it if hidden, un-minimise it if minimised, then focus it.
// A maximized or fullscreen window stays maximized or fullscreen.
//
// It exists because the obvious call, Window.Restore, is the wrong one:
// Wails defines Restore as "undo whichever of minimised / fullscreen /
// maximised applies", so revealing a maximized window through it drops
// the window to its normal size (SW_RESTORE on Windows, the equivalent
// on macOS). Every "bring the app forward" path — an OS notification
// click, a second launch of the binary — goes through here and never
// through Restore; a source test in this package enforces that.
func Reveal(window *application.WebviewWindow) {
	if window == nil {
		return
	}
	window.Show()
	if window.IsMinimised() {
		window.UnMinimise()
	}
	window.Focus()
}
