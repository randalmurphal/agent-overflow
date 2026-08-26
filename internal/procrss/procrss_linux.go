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

// SampleAll reads the real /proc and reports every descendant of pid,
// whatever it is named. It is the shape a health rollup wants: "how much
// memory does this instance's process tree hold", where a provider mock
// counts as much as a renderer does.
func SampleAll(pid int) (Tree, error) { return SampleAllRoot("/proc", pid) }

// Supported reports whether Sample can answer on this platform.
func Supported() bool { return true }
