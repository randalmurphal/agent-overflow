//go:build !linux

package orphanreaper

// bootID is empty off Linux: a macOS start time is absolute.
func bootID() (string, error) { return "", nil }
