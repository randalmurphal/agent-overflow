//go:build windows

package orphanreaper

import "syscall"

// killGroup is a no-op on Windows: the desktop binary never spawns
// provider children there (the WSL-side Linux backend owns them, and the
// launcher uses a Win32 Job Object for lifetime management). This stub
// exists so the package compiles in the GOOS=windows cross-build.
func killGroup(pgid int, sig syscall.Signal) {
	_ = pgid
	_ = sig
}
