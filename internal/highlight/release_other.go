//go:build !linux && !darwin

package highlight

// releaseFreedMemory does nothing where the C allocator has no release
// call this package uses.
func releaseFreedMemory() {}
