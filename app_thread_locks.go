package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// keyedLockRegistry hands out one lock per string key and reclaims it once
// nothing references it. Two independent users exist: per-thread action
// serialization (`App.threadLocks`) and per-item workflow workspace
// provisioning (`workflowAppRunner.workspaceLocks`).
type keyedLockRegistry struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

// keyedLock is a cap-1 channel semaphore rather than a sync.Mutex so a
// waiter can abandon the wait: a mutex acquisition is not selectable, and an
// unbounded, uncancellable wait is how one wedged holder turns into a
// permanently wedged thread (see LockCtx).
//
// `refs` counts holders AND waiters. The entry lives exactly as long as one
// of them does: `dropRef` deletes it when the last reference goes away, so
// the registry cannot leak an entry per key for the life of the process and
// no lifecycle site has to remember to release one. A fresh mint after that
// is indistinguishable from the deleted entry — mutual exclusion never spans
// a moment with zero holders and zero waiters, so entry identity across that
// gap carries nothing.
type keyedLock struct {
	sem  chan struct{}
	refs int
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

// configApplyLocks serializes liveApplySessionConfig per thread: the apply
// is a read-modify-write over session.launchOpts (snapshot → plan → send →
// commit), and two concurrent reconciles would both plan against the same
// snapshot and both send the same change. A separate registry rather than
// the thread action lock because reconcile callers arrive both with and
// without that lock held (applyRuntimeMode and the deferred watcher hold
// it; the model-selection bindings do not).
//
// Lock-order rules: thread action lock → config-apply lock → a.mu, never
// any other order. Session-START paths must never acquire a config-apply
// lock — the serialized section blocks on waitForStartingSession, so a
// start that waited on this lock would deadlock against a holder waiting
// on the start.
func (a *App) configApplyLocks() *keyedLockRegistry {
	a.sessionConfigApplyLocksOnce.Do(func() {
		if a.sessionConfigApplyLocks == nil {
			a.sessionConfigApplyLocks = newKeyedLocks()
		}
	})
	return a.sessionConfigApplyLocks
}

// Lock returns an unlock function that must be called once the per-key
// critical section completes. It waits as long as it takes; callers that
// must be able to give up use LockCtx.
func (r *keyedLockRegistry) Lock(key string) func() {
	unlock, err := r.LockCtx(context.Background(), key)
	if err != nil {
		// Structurally unreachable: context.Background() is never done, so
		// the acquisition arm is LockCtx's only exit. Loud rather than a
		// nil unlock func the caller would defer straight into a panic
		// with no explanation.
		panic(fmt.Sprintf("keyedLockRegistry: uncancellable Lock(%q) failed: %v", key, err))
	}
	return unlock
}

// LockCtx is Lock with an escape hatch: it returns ctx.Err() instead of the
// unlock function when ctx is done before the key is acquired. It is safe to
// abandon precisely because a waiter has performed no side effects yet — the
// caller's critical section has not started. Provider IO already in flight
// must never be wired to a cancellation like this: "cancelled" there would be
// indistinguishable from "delivered".
//
// On the error path nothing is held and the returned func is nil.
func (r *keyedLockRegistry) LockCtx(ctx context.Context, key string) (func(), error) {
	// Answered before touching the registry so an already-dead ctx can't win
	// a random select against a free lock and leave the caller holding a
	// critical section it is about to abandon anyway.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	lock := r.reference(key)
	select {
	case lock.sem <- struct{}{}:
		// Both arms can be ready at once, and `select` then chooses at random: a
		// caller whose context died WHILE it waited can still win the acquisition
		// and walk into a critical section it has already given up on. Rechecking
		// here makes the cancellation decisive whichever arm fired, and the token
		// goes straight back so the next waiter is not held up by a caller that
		// never intended to run.
		if err := ctx.Err(); err != nil {
			<-lock.sem
			r.dropRef(key, lock)
			return nil, err
		}
		return r.releaser(key, lock), nil
	case <-ctx.Done():
		// A cancelled waiter owes the registry the same accounting an
		// unlocker does — without it, refs never reaches zero and a
		// Forget-marked entry is never reclaimed.
		r.dropRef(key, lock)
		return nil, ctx.Err()
	}
}

// reference returns the lock for key, creating it if needed, with one
// reference taken on the caller's behalf.
func (r *keyedLockRegistry) reference(key string) *keyedLock {
	r.mu.Lock()
	defer r.mu.Unlock()
	lock, ok := r.locks[key]
	if !ok {
		lock = &keyedLock{sem: make(chan struct{}, 1)}
		r.locks[key] = lock
	}
	lock.refs++
	return lock
}

// dropRef gives back one reference and reclaims the entry when the last one
// goes away. Both an unlocker and a cancelled LockCtx waiter route through
// here: the self-cleaning invariant on `keyedLock.refs` holds only if every
// reference taken by `reference` is given back exactly once, whichever way
// its holder left.
func (r *keyedLockRegistry) dropRef(key string, lock *keyedLock) {
	r.mu.Lock()
	lock.refs--
	if lock.refs == 0 && r.locks[key] == lock {
		delete(r.locks, key)
	}
	r.mu.Unlock()
}

// releaser builds the unlock function handed to a successful acquirer.
//
// A second call is a caller bug and panics rather than draining the token a
// DIFFERENT holder has since taken (which would silently let two goroutines
// into the critical section) or blocking forever on an empty semaphore. That
// keeps the failure mode of a double-unlock at least as loud as sync.Mutex's.
func (r *keyedLockRegistry) releaser(key string, lock *keyedLock) func() {
	var released atomic.Bool
	return func() {
		if !released.CompareAndSwap(false, true) {
			panic(fmt.Sprintf("keyedLockRegistry: unlock called twice for key %q", key))
		}
		<-lock.sem
		r.dropRef(key, lock)
	}
}
