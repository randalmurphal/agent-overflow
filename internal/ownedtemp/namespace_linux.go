//go:build linux

package ownedtemp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// namespaceName is the file in a directory that names the mount namespace
// whose processes keep it owned (KeepForMountNamespace).
const namespaceName = ".ownedtemp-mntns"

// KeepForMountNamespace keeps dir owned, besides while its lock is held,
// while any process is in the calling process's mount namespace. Every
// process inherits its parent's mount namespace, whatever it does with
// the descriptors it inherits, so a command started in a mount namespace
// of its own keeps dir until all its descendants have exited, including
// those started by a parent that closes inherited descriptors, as Node's
// child_process does. Call it in that namespace before starting anything
// that uses dir; until then the lock alone holds it.
func KeepForMountNamespace(dir string) error {
	ns, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		return fmt.Errorf("read the mount namespace: %w", err)
	}
	pending := filepath.Join(dir, namespaceName+".pending")
	if err := os.WriteFile(pending, []byte(ns), 0o600); err != nil {
		return fmt.Errorf("record the mount namespace of %s: %w", dir, err)
	}
	if err := os.Rename(pending, filepath.Join(dir, namespaceName)); err != nil {
		return fmt.Errorf("record the mount namespace of %s: %w", dir, err)
	}
	return nil
}

// mountNamespaceInUse reports whether a process is in the mount namespace
// dir records. A process this user cannot inspect is not one of a run it
// started.
func mountNamespaceInUse(dir string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, namespaceName))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return threadInMountNamespace("/proc", string(data))
}

// threadInMountNamespace reports whether any thread of a process under
// proc, a procfs root, is in the mount namespace named want. It reads
// every thread, not the process's own entry: a leader that has exited
// names no namespace while its other threads are still in it. A process
// that is gone or unreadable has no thread to read.
func threadInMountNamespace(proc, want string) (bool, error) {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		tasks, err := os.ReadDir(filepath.Join(proc, entry.Name(), "task"))
		if err != nil {
			continue
		}
		for _, task := range tasks {
			link := filepath.Join(proc, entry.Name(), "task", task.Name(), "ns", "mnt")
			if ns, err := os.Readlink(link); err == nil && ns == want {
				return true, nil
			}
		}
	}
	return false, nil
}
