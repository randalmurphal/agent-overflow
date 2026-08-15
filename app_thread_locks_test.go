package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestThreadActionLocksSerializeOneThread(t *testing.T) {
	locks := newKeyedLocks()
	unlock := locks.Lock("thread-1")

	attempting := make(chan struct{})
	acquired := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(attempting)
		secondUnlock := locks.Lock("thread-1")
		close(acquired)
		<-release
		secondUnlock()
		close(done)
	}()

	<-attempting
	waitForThreadLockRefs(t, locks, "thread-1", 2)
	select {
	case <-acquired:
		t.Fatal("second lock acquired while first lock was held")
	default:
	}

	unlock()
	<-acquired
	close(release)
	<-done
}

func TestThreadActionLocksAllowDifferentThreads(t *testing.T) {
	locks := newKeyedLocks()
	unlock := locks.Lock("thread-1")
	defer unlock()

	otherUnlock := locks.Lock("thread-2")
	otherUnlock()
}

// The registry self-cleans: an entry lives exactly as long as a holder or a
// waiter references it, so no lifecycle site anywhere has to remember to drop
// one and the map cannot grow an entry per key for the life of the process.
func TestThreadActionLocksEntrySelfCleansOnLastRelease(t *testing.T) {
	locks := newKeyedLocks()
	unlock := locks.Lock("thread-1")

	if got := len(locks.locks); got != 1 {
		t.Fatalf("lock count while held = %d, want 1", got)
	}
	unlock()
	if got := len(locks.locks); got != 0 {
		t.Fatalf("lock count after release = %d, want 0", got)
	}
}

func TestThreadActionLocksEntrySurvivesWhileAnyReferenceRemains(t *testing.T) {
	locks := newKeyedLocks()
	unlock := locks.Lock("thread-1")

	waiting := make(chan struct{})
	acquired := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(waiting)
		secondUnlock := locks.Lock("thread-1")
		close(acquired)
		secondUnlock()
		close(done)
	}()

	<-waiting
	waitForThreadLockRefs(t, locks, "thread-1", 2)

	// The holder releasing hands the key to the waiter; the entry must not be
	// reclaimed out from under a live reference — a re-mint here would split
	// the key onto a second semaphore and admit two holders at once.
	unlock()
	<-acquired
	<-done
	if got := len(locks.locks); got != 0 {
		t.Fatalf("lock count after final reference unlock = %d, want 0", got)
	}
}

func TestThreadActionLocksLockCtxCancelReleasesReference(t *testing.T) {
	locks := newKeyedLocks()
	unlock := locks.Lock("thread-1")

	ctx, cancel := context.WithCancel(context.Background())
	failed := make(chan error, 1)
	go func() {
		_, err := locks.LockCtx(ctx, "thread-1")
		failed <- err
	}()

	waitForThreadLockRefs(t, locks, "thread-1", 2)
	cancel()

	select {
	case err := <-failed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("LockCtx err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled LockCtx waiter did not return")
	}

	// The cancelled waiter owes the registry its reference back; only the
	// holder's remains.
	waitForThreadLockRefs(t, locks, "thread-1", 1)

	// And the key is still usable: the abandoned wait consumed no token.
	unlock()
	next, err := locks.LockCtx(context.Background(), "thread-1")
	if err != nil {
		t.Fatalf("LockCtx after cancelled waiter: %v", err)
	}
	next()
	waitForThreadLockRefs(t, locks, "thread-1", 0)
}

func TestThreadActionLocksLockCtxCancelSelfCleans(t *testing.T) {
	locks := newKeyedLocks()
	unlock := locks.Lock("thread-1")

	ctx, cancel := context.WithCancel(context.Background())
	failed := make(chan error, 1)
	go func() {
		_, err := locks.LockCtx(ctx, "thread-1")
		failed <- err
	}()
	waitForThreadLockRefs(t, locks, "thread-1", 2)

	// Cancel the waiter while the holder still holds, so its reference is the
	// one that has to come back on its own — nothing else will return it.
	cancel()
	if err := <-failed; !errors.Is(err, context.Canceled) {
		t.Fatalf("LockCtx err = %v, want context.Canceled", err)
	}
	waitForThreadLockRefs(t, locks, "thread-1", 1)
	if got := len(locks.locks); got != 1 {
		t.Fatalf("lock count while the holder still references = %d, want 1", got)
	}

	// Holder out: refs reach zero, so the entry is reclaimed. A waiter that
	// dropped its wait without decrementing would have pinned this entry for
	// the life of the process.
	unlock()
	waitForRegistryEmpty(t, locks)
}

func TestThreadActionLocksLockCtxRefusesDeadContext(t *testing.T) {
	locks := newKeyedLocks()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Free key, dead ctx: the answer must be deterministic, and it must not
	// register a lock entry the caller never gets to release.
	for i := 0; i < 50; i++ {
		if _, err := locks.LockCtx(ctx, "thread-1"); !errors.Is(err, context.Canceled) {
			t.Fatalf("LockCtx err = %v, want context.Canceled", err)
		}
	}
	if got := len(locks.locks); got != 0 {
		t.Fatalf("lock count after refused acquisitions = %d, want 0", got)
	}
}

// TestThreadActionLocksConcurrentStress runs Lock and LockCtx (cancelling
// mid-wait) against a small key space so -race and the final reference audit
// both have something to say. The counter proves mutual exclusion held
// throughout — including across the self-cleaning delete/re-mint boundary a
// released key crosses — and the empty registry proves every reference,
// every cancelled waiter's included, was given back.
func TestThreadActionLocksConcurrentStress(t *testing.T) {
	locks := newKeyedLocks()
	keys := []string{"thread-1", "thread-2", "thread-3"}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		holders = make(map[string]int)
	)
	enter := func(key string) {
		mu.Lock()
		holders[key]++
		if holders[key] != 1 {
			mu.Unlock()
			t.Errorf("key %s held by %d goroutines at once", key, holders[key])
			return
		}
		mu.Unlock()
	}
	leave := func(key string) {
		mu.Lock()
		holders[key]--
		mu.Unlock()
	}

	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(seed), 42))
			for n := 0; n < 200; n++ {
				key := keys[rng.IntN(len(keys))]
				if rng.IntN(2) == 0 {
					unlock := locks.Lock(key)
					enter(key)
					leave(key)
					unlock()
					continue
				}
				ctx, cancel := context.WithTimeout(
					context.Background(), time.Duration(rng.IntN(200))*time.Microsecond,
				)
				unlock, err := locks.LockCtx(ctx, key)
				if err == nil {
					enter(key)
					leave(key)
					unlock()
				} else if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("LockCtx err = %v, want DeadlineExceeded or nil", err)
				}
				cancel()
			}
		}(i)
	}
	wg.Wait()
	waitForRegistryEmpty(t, locks)
}

func waitForRegistryEmpty(t *testing.T, locks *keyedLockRegistry) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		locks.mu.Lock()
		got := len(locks.locks)
		locks.mu.Unlock()
		if got == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("registry still holds %d entries after every key was forgotten", got)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForThreadLockRefs(t *testing.T, locks *keyedLockRegistry, threadID string, want int) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		locks.mu.Lock()
		got := 0
		if lock, ok := locks.locks[threadID]; ok {
			got = lock.refs
		}
		locks.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}

	locks.mu.Lock()
	got := 0
	if lock, ok := locks.locks[threadID]; ok {
		got = lock.refs
	}
	locks.mu.Unlock()
	t.Fatalf("lock refs for %s = %d, want %d", threadID, got, want)
}

// cancelledDuringWaitContext is a context that becomes cancelled exactly once,
// on its SECOND Err() call, and never closes its Done channel.
//
// That models the one ordering a real context cannot be made to hold still in:
// the cancellation landing after LockCtx's entry check and while the
// acquisition arm is ready. With a real context both arms go ready together and
// `select` chooses at random, so the defect reproduces only sometimes; with
// this one the acquisition arm is the only arm, which is precisely the case the
// post-acquisition recheck exists for.
type cancelledDuringWaitContext struct {
	context.Context
	checks atomic.Int32
}

func (c *cancelledDuringWaitContext) Done() <-chan struct{} { return nil }

func (c *cancelledDuringWaitContext) Err() error {
	if c.checks.Add(1) == 1 {
		return nil
	}
	return context.Canceled
}

// A waiter whose context dies WHILE it waits must not be admitted. `select`
// chooses at random when both arms are ready, so the acquisition arm can win for
// a caller that has already given up — and walking into a critical section it
// abandoned is the one thing an abandonable lock must never do.
func TestThreadActionLocksLockCtxRefusesAContextCancelledDuringTheWait(t *testing.T) {
	locks := newKeyedLocks()
	const key = "thread-1"
	ctx := &cancelledDuringWaitContext{Context: context.Background()}

	unlock, err := locks.LockCtx(ctx, key)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("LockCtx err = %v, want context.Canceled", err)
	}
	if unlock != nil {
		t.Fatal("a refused acquisition handed back an unlock function")
	}

	// Every reference taken is given back, and the token with it — the refusal
	// was the entry's only reference, so the registry self-cleans it. A refusal
	// that kept either would wedge the key for the next caller.
	locks.mu.Lock()
	_, remains := locks.locks[key]
	locks.mu.Unlock()
	if remains {
		t.Fatal("a refused acquisition left its entry behind")
	}
	acquired := make(chan struct{})
	go func() {
		locks.Lock(key)()
		close(acquired)
	}()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("the key stayed held by a caller that was refused")
	}
}

// A second unlock is a caller bug, and it has to fail as loudly as sync.Mutex's
// does. The alternatives are both silent corruption: draining the token a
// DIFFERENT holder has since taken lets two goroutines into one critical
// section, and blocking on an empty semaphore wedges the caller with no
// explanation.
func TestThreadActionLocksDoubleUnlockPanics(t *testing.T) {
	locks := newKeyedLocks()
	unlock := locks.Lock("thread-1")
	unlock()

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("a second unlock returned quietly")
		}
		message, ok := recovered.(string)
		if !ok || !strings.Contains(message, "thread-1") {
			t.Fatalf("panic value = %v, want one naming the key", recovered)
		}
	}()
	unlock()
}
