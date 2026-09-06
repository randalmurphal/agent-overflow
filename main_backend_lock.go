package main

import (
	"errors"
	"os"

	"agent-overflow/internal/appdirs"
)

// All backend entry points reach bootTransport. Keep one OS-held data-root
// lock until process exit, before opening SQLite or starting providers. A
// supervisor and a frontend that adopts a running service own no backend lock.
var heldBackendLock *harnessInstanceLock

func acquireBackendInstanceLock(root string) (*harnessInstanceLock, error) {
	if root == "" {
		return nil, errors.New("cannot determine the backend data directory")
	}
	if err := os.MkdirAll(root, appdirs.PrivateDirPerm); err != nil {
		return nil, err
	}
	return acquireInstanceLock(root, "backend", "backend.lock", "another Agent Overflow backend")
}
