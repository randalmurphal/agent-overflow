//go:build !((linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui))

package browser

import "testing"

// nativeSmokeMain has no native engine to drive in this build.
func nativeSmokeMain(*testing.M) (int, bool) { return 0, false }
