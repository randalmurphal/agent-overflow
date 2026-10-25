//go:build !windows && !linux && !darwin

package harnessclient

import "fmt"

// awaitExitUnreaped has no exit notification that leaves the child
// unreaped here, so no exit custody is recorded and a group whose leader
// exited is proven only by the members captured before its signal.
func awaitExitUnreaped(pid int) error {
	return fmt.Errorf("record backend %d at its exit: unsupported on this platform", pid)
}
