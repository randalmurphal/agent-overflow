package main

import "sync"

// keyedLockRegistry hands out one mutex per string key and reclaims it once
// nothing references it. Two independent users exist: per-thread action
// serialization (`App.threadLocks`) and per-item workflow workspace
// provisioning (`workflowAppRunner.workspaceLocks`).
type keyedLockRegistry struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu       sync.Mutex
	refs     int
	deleting bool
}

func newKeyedLocks() *keyedLockRegistry {
	return &keyedLockRegistry{
		locks: make(map[string]*keyedLock),
	}
}

func (a *App) threadLocks() *keyedLockRegistry {
	a.threadActionLocksOnce.Do(func() {
		if a.threadActionLocks == nil {
			a.threadActionLocks = newKeyedLocks()
		}
	})
	return a.threadActionLocks
}

// Lock returns an unlock function that must be called once the per-key
// critical section completes.
func (r *keyedLockRegistry) Lock(key string) func() {
	r.mu.Lock()
	lock, ok := r.locks[key]
	if !ok {
		lock = &keyedLock{}
		r.locks[key] = lock
	}
	lock.refs++
	r.mu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()

		r.mu.Lock()
		lock.refs--
		if lock.refs == 0 && lock.deleting && r.locks[key] == lock {
			delete(r.locks, key)
		}
		r.mu.Unlock()
	}
}

// Forget drops the mutex entry for a key that no longer exists (a deleted
// thread, a finished run).
//
// The entry remains visible while any holder or waiter still references it, so
// stale callers cannot split the same key onto a second mutex. Deletion
// physically removes the entry when the final reference unlocks.
func (r *keyedLockRegistry) Forget(key string) {
	r.mu.Lock()
	lock, ok := r.locks[key]
	if !ok {
		r.mu.Unlock()
		return
	}
	lock.deleting = true
	if lock.refs == 0 {
		delete(r.locks, key)
	}
	r.mu.Unlock()
}
