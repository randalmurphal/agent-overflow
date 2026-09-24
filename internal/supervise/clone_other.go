//go:build !linux && !darwin

package supervise

import "os"

// platformCloneFile has no clone to offer here; the caller copies.
func platformCloneFile(*os.File, string) (bool, error) { return false, nil }
