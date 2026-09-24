//go:build windows

package supervise

import "errors"

// The desktop helper runs on macOS and Linux. On Windows the launcher
// applies updates (wsllauncher.UpdateSequence).
var errDesktopHelperWindows = errors.New("supervise: the desktop update helper does not run on Windows")

func crossDevice(error) bool { return false }

// NativeDesktopFiles refuses every operation on Windows.
func NativeDesktopFiles() DesktopFiles {
	return DesktopFiles{
		Replace: func(string, string, string) error { return errDesktopHelperWindows },
		InUse:   func(string) (bool, error) { return false, errDesktopHelperWindows },
	}
}

// StartDesktopHelper refuses on Windows.
func StartDesktopHelper(string, []string, string, []string) error { return errDesktopHelperWindows }

// StartDesktopApp refuses on Windows.
func StartDesktopApp(string, []string, string, []string) error { return errDesktopHelperWindows }
