//go:build linux

package procrss

// Sample reads the real /proc. `prefixes` selects which descendants are
// reported; nil means DefaultWebviewPrefixes.
func Sample(pid int, prefixes []string) (Tree, error) {
	if prefixes == nil {
		prefixes = DefaultWebviewPrefixes
	}
	return SampleRoot("/proc", pid, prefixes)
}

// Supported reports whether Sample can answer on this platform.
func Supported() bool { return true }
