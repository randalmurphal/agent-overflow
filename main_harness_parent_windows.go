//go:build windows

package main

import "errors"

// watchParentExit is Unix-only: on Windows the launcher's Job Object owns
// the whole suite tree, and a process's parent PID never changes.
func watchParentExit(int) error {
	return errors.New("--exit-with-parent is Unix-only; the launcher's Job Object owns the tree on Windows")
}
