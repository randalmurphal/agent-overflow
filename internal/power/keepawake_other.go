//go:build !linux && !darwin && !windows

// keepawake_other.go keeps the package building on any GOOS without an
// inhibitor of its own. Apply stays a well-defined no-op rather than a
// compile error so callers never need a build tag.
package power

func newOSBackend() backend { return noopBackend{} }
