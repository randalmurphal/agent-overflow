//go:build !((linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui))

package browser

import (
	"os"
	"testing"
)

// runTests is TestMain's run where the native engines do not build.
func runTests(m *testing.M) { os.Exit(m.Run()) }
