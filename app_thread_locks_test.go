package main

import (
	"testing"
	"time"
)

func TestThreadActionLocksSerializeOneThread(t *testing.T) {
	locks := newThreadActionLocks()
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
	locks := newThreadActionLocks()
	unlock := locks.Lock("thread-1")
	defer unlock()

	otherUnlock := locks.Lock("thread-2")
	otherUnlock()
}

func TestThreadActionLocksForgetDropsUnusedEntry(t *testing.T) {
	locks := newThreadActionLocks()
	unlock := locks.Lock("thread-1")
	unlock()

	if got := len(locks.locks); got != 1 {
		t.Fatalf("lock count before forget = %d, want 1", got)
	}
	locks.Forget("thread-1")
	if got := len(locks.locks); got != 0 {
		t.Fatalf("lock count after forget = %d, want 0", got)
	}
}

func TestThreadActionLocksForgetWaitsForHolderAndWaiter(t *testing.T) {
	locks := newThreadActionLocks()
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
	locks.Forget("thread-1")
	if got := len(locks.locks); got != 1 {
		t.Fatalf("lock count while references remain = %d, want 1", got)
	}

	unlock()
	<-acquired
	<-done
	if got := len(locks.locks); got != 0 {
		t.Fatalf("lock count after final reference unlock = %d, want 0", got)
	}
}

func waitForThreadLockRefs(t *testing.T, locks *threadActionLockRegistry, threadID string, want int) {
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
