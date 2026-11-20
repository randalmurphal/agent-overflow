package browser

import (
	"context"
	"sync"
)

// pageLock is a page's operation lock (managedPage.mu): a mutex whose
// waiter can stop waiting when its context ends (LockContext), so an
// operation bounded by a deadline is bounded while it waits for the page
// too. Its zero value is unlocked.
type pageLock struct {
	once sync.Once
	sem  chan struct{}
}

func (l *pageLock) slot() chan struct{} {
	l.once.Do(func() { l.sem = make(chan struct{}, 1) })
	return l.sem
}

// Lock waits for the lock however long it is held.
func (l *pageLock) Lock() { l.slot() <- struct{}{} }

// LockContext takes the lock, or answers ctx's error without it once ctx
// ends first. A context already ended answers its error without taking it.
func (l *pageLock) LockContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case l.slot() <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unlock releases the lock. Unlocking a lock not held is a programming
// error, as it is for a sync.Mutex.
func (l *pageLock) Unlock() {
	select {
	case <-l.slot():
	default:
		panic("browser: unlock of an unlocked page lock")
	}
}
