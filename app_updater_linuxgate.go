package main

import (
	"fmt"
	"os"
	"path/filepath"

	"agent-overflow/internal/appimage"
)

// Native-Linux preflight for the in-app updater. Both refusals below answer
// the same question — can the swap actually replace this executable where it
// lives? — and both are checked before App.updater is wired up, so a build
// that cannot be updated in place reports the feature unsupported instead of
// offering a Download that fails at the last step, after the user has waited
// through a tens-of-MB transfer.
//
// Deliberately tag-free and in its own file: initUpdater lives behind
// `!nogui` and needs a Wails application to exercise, while these two checks
// are pure enough to unit-test on any host. Only initUpdater's
// runtime.GOOS == "linux" branch calls it, so macOS (whose .app bundle swap
// has its own working path, and which has no AppImage equivalent) is
// untouched.

// linuxUpdaterBlocked reports why an in-app update could not be applied to
// this Linux install, or "" when nothing blocks it. The returned string is a
// log fragment, phrased to complete "updater: <reason> — in-app updates
// disabled".
func linuxUpdaterBlocked() string {
	if appimage.Running() {
		// The type-2 AppImage runtime mounts the app's squashfs read-only and
		// unmounts it when we exit. There is no version of the swap that can
		// succeed against that mount, and the .AppImage file the user actually
		// launched is not ours to rewrite from inside it — AppImage updates
		// are the launcher's job (AppImageUpdate / zsync), not ours.
		return "running from an AppImage (read-only mount)"
	}
	exe, err := os.Executable()
	if err != nil {
		// The swap helper is handed this path; without it there is nothing to
		// replace. Refusing loudly beats wiring an updater that would fail
		// only once the user pressed Restart.
		return fmt.Sprintf("cannot resolve the running executable: %v", err)
	}
	dir := filepath.Dir(exe)
	if err := probeDirWritable(dir); err != nil {
		return fmt.Sprintf("install directory %s is not writable: %v", dir, err)
	}
	return ""
}

// probeDirWritable reports whether this process may create and remove files
// in dir, by doing exactly that.
//
// An actual create-then-remove is the only honest test. os.Stat mode bits
// describe an inode's declared permissions, not what THIS process may do to
// it: they miss a read-only mount, an immutable flag and a full filesystem
// (all of which pass the bit check and then fail the write), and they
// over-refuse whenever an ACL or capability grants access the bits don't
// advertise. The swap writes a sibling file next to the executable and
// renames over it, so a sibling file next to the executable is what to test.
//
// It cannot leave litter. os.CreateTemp mints a unique name, so two
// simultaneous launches cannot collide, and the directory entry is unlinked
// while the handle is still open — from that point no path to the probe file
// exists, so even a failing Close cannot strand a file next to the user's
// binary.
func probeDirWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".agent-overflow-update-probe-*")
	if err != nil {
		return err
	}
	removeErr := os.Remove(f.Name())
	closeErr := f.Close()
	if removeErr != nil {
		return removeErr
	}
	return closeErr
}
