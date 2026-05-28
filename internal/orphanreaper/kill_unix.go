//go:build !windows

package orphanreaper

import "syscall"

// killGroup signals an entire process group via the negative-PID
// convention from kill(2). It needs only same-uid permission — no parent
// relationship — which is exactly why the sidecar and the startup sweep
// can reach provider groups they never spawned. pgid<=1 is refused so a
// corrupted value can never fan out to every process (-1) or init (-1's
// neighbour, group 1).
func killGroup(pgid int, sig syscall.Signal) {
	if pgid <= 1 {
		return
	}
	_ = syscall.Kill(-pgid, sig)
}
