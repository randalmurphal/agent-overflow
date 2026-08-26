//go:build !linux

package procrss

// Sample refuses everywhere /proc does not exist. The perf run treats the
// refusal as "no RSS series", never as a failed run: macOS and Windows have
// their own process-memory stories (and on Windows the webview is a
// WebView2 process the launcher owns, not our child at all).
func Sample(_ int, _ []string) (Tree, error) { return Tree{}, ErrUnsupported }

// Supported reports whether Sample can answer on this platform.
func Supported() bool { return false }
