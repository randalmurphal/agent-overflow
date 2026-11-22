//go:build !unix

package browser

import (
	"errors"
	"os"
)

// errStagingUnsupported refuses download staging where no engine stages:
// only the engines in this process, WebKitGTK and WKWebView, stage
// downloads (downloadsStaged).
var errStagingUnsupported = errors.New("browser: downloads are staged only by the engines in this process, on Linux and macOS")

// lockRun refuses: nothing stages here.
func lockRun(*os.File) (bool, error) { return false, errStagingUnsupported }

// unlockRun refuses: nothing stages here.
func unlockRun(*os.File) error { return errStagingUnsupported }
