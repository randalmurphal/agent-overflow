package main

import (
	"context"
	"errors"
	"os"
	"time"

	"agent-overflow/internal/appdirs"
)

// All backend entry points reach bootTransport. Keep one OS-held data-root
// lock until process exit, before opening SQLite or starting providers.
// `supervise` and the in-app update commands take it for the children they
// start, which boot with BackendLockHeldBySupervisor; a frontend that adopts
// a running service owns none.
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

// backendLockPoll is how often waitForBackendInstanceLock retries.
const backendLockPoll = 200 * time.Millisecond

// waitForBackendInstanceLock takes the backend lock, retrying for up to wait
// while another process holds it: an update step runs while the previous
// version is still shutting down. The last refusal is returned when the wait
// runs out.
func waitForBackendInstanceLock(ctx context.Context, root string, wait time.Duration) (*harnessInstanceLock, error) {
	deadline := time.Now().Add(wait)
	for {
		lock, err := acquireBackendInstanceLock(root)
		if err == nil {
			return lock, nil
		}
		if !time.Now().Before(deadline) {
			return nil, err
		}
		timer := time.NewTimer(backendLockPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
