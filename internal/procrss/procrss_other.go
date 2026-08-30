//go:build !linux && (!darwin || !cgo)

package procrss

// Sample refuses on platforms without a native sampler. The perf run treats
// the refusal as "no RSS series", never as a failed run. On Windows the
// WebView2 process belongs to the launcher rather than the backend tree.
func Sample(_ int, _ []string) (Tree, error) { return Tree{}, ErrUnsupported }

// SampleAll refuses for the same reason Sample does.
func SampleAll(_ int) (Tree, error) { return Tree{}, ErrUnsupported }

// Supported reports whether Sample can answer on this platform.
func Supported() bool { return false }
