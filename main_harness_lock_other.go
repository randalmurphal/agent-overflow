//go:build !unix && !windows

package main

import "os"

// lockFileExclusiveNonBlocking has no implementation off unix and
// Windows, so the single-instance guard degrades to "always allowed"
// there. Documented rather than faked: a lock that silently does nothing
// is worse than a gap someone can read about, and no isolated boot runs
// on such a platform today (the harness backend is linux/macOS, with the
// Windows shell hosting a WSL Linux backend).
func lockFileExclusiveNonBlocking(*os.File) (bool, error) { return true, nil }
