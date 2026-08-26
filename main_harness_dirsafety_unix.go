//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// refuseUnsafeHarnessDir rejects a directory that someone else could
// write into.
//
// The threat is specific: `make harness` derives its data root from the
// checkout, which puts a PREDICTABLE path under /tmp on a shared host.
// An attacker who creates that directory first, mode 0777, hands the
// boot a $HOME on a tree they still control — and a $HOME they can write
// is a .gitconfig they can write, which is `core.pager` / `core.hooksPath`
// / an alias running their command as us the next time the harness runs
// git. refuseSymlink catches the link they could have planted INSTEAD;
// this catches the directory they planted instead of the link.
//
// Two conditions, both fatal to the boot rather than repaired: a
// directory owned by someone else is not ours to chmod, and one that is
// group- or other-writable may already have been tampered with, so
// tightening the mode now would only hide it.
func refuseUnsafeHarnessDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		// No ownership facts available: nothing to check against, and
		// refusing every boot on such a filesystem would be worse than
		// the risk on it.
		return nil
	}
	if uid := os.Getuid(); int(stat.Uid) != uid {
		return fmt.Errorf("%s is owned by uid %d, not the uid %d running this boot; an isolated boot (--harness / --soak) refuses a data directory it does not own (it becomes $HOME, and a planted .gitconfig there runs as you)",
			path, stat.Uid, uid)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("%s is mode %04o — group- or world-writable; an isolated boot (--harness / --soak) refuses it (it becomes $HOME, and a planted .gitconfig there runs as you). Fix it with `chmod 700 %s`, or pick a data dir under your own home",
			path, perm, path)
	}
	return nil
}
