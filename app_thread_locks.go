package main

import "sync"

type threadActionLockRegistry struct {
	mu    sync.Mutex
	locks map[string]*threadActionLock
}

type threadActionLock struct {
	mu       sync.Mutex
	refs     int
	deleting bool
}

func newThreadActionLocks() *threadActionLockRegistry {
	return &threadActionLockRegistry{
		locks: make(map[string]*threadActionLock),
	}
}

func (a *App) threadLocks() *threadActionLockRegistry {
	a.threadActionLocksOnce.Do(func() {
		if a.threadActionLocks == nil {
			a.threadActionLocks = newThreadActionLocks()
		}
	})
	return a.threadActionLocks
}

// Lock returns an unlock function that must be called once the per-thread
// critical section completes.
func (r *threadActionLockRegistry) Lock(threadID string) func() {
	r.mu.Lock()
	lock, ok := r.locks[threadID]
	if !ok {
		lock = &threadActionLock{}
		r.locks[threadID] = lock
	}
	lock.refs++
	r.mu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()

		r.mu.Lock()
		lock.refs--
		if lock.refs == 0 && lock.deleting && r.locks[threadID] == lock {
			delete(r.locks, threadID)
		}
		r.mu.Unlock()
	}
}

// Forget drops the mutex entry for a deleted thread.
//
// The entry remains visible while any holder or waiter still references it, so
// stale callers cannot split the same thread onto a second mutex. Deletion
// physically removes the entry when the final reference unlocks.
func (r *threadActionLockRegistry) Forget(threadID string) {
	r.mu.Lock()
	lock, ok := r.locks[threadID]
	if !ok {
		r.mu.Unlock()
		return
	}
	lock.deleting = true
	if lock.refs == 0 {
		delete(r.locks, threadID)
	}
	r.mu.Unlock()
}
