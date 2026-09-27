package highlight

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// releaseLog records when a heapRelease released.
type releaseLog struct {
	mu sync.Mutex
	at []time.Time
}

func (l *releaseLog) release() {
	l.mu.Lock()
	l.at = append(l.at, time.Now())
	l.mu.Unlock()
}

func (l *releaseLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.at)
}

func compute(h *heapRelease, d time.Duration) {
	h.begin()
	time.Sleep(d)
	h.end()
}

func TestHeapReleaseRunsOnceIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var log releaseLog
		h := &heapRelease{release: log.release}
		compute(h, 0)
		time.Sleep(releaseIdle / 2)
		compute(h, 0)
		// The second computation restarts the idle wait.
		time.Sleep(releaseIdle - time.Millisecond)
		synctest.Wait()
		if n := log.count(); n != 0 {
			t.Fatalf("released %d times before the highlighter was idle for %v", n, releaseIdle)
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if n := log.count(); n != 1 {
			t.Fatalf("released %d times after %v idle, want 1", n, releaseIdle)
		}
		// Idle with nothing new computed: no further release.
		time.Sleep(10 * releaseIdle)
		synctest.Wait()
		if n := log.count(); n != 1 {
			t.Fatalf("released %d times with nothing computed since, want 1", n)
		}
		// The next computation arms a new release.
		compute(h, 0)
		time.Sleep(releaseIdle)
		synctest.Wait()
		if n := log.count(); n != 2 {
			t.Fatalf("released %d times after a second computation, want 2", n)
		}
	})
}

func TestHeapReleaseWaitsForComputationsInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var log releaseLog
		h := &heapRelease{release: log.release}
		compute(h, 0)
		// A long computation starts before the idle release fires.
		h.begin()
		time.Sleep(5 * releaseIdle)
		synctest.Wait()
		if n := log.count(); n != 0 {
			t.Fatalf("released %d times while a computation was in flight", n)
		}
		h.end()
		time.Sleep(releaseIdle - time.Millisecond)
		synctest.Wait()
		if n := log.count(); n != 0 {
			t.Fatalf("released %d times less than %v after the computation finished", n, releaseIdle)
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if n := log.count(); n != 1 {
			t.Fatalf("released %d times once idle, want 1", n)
		}
	})
}

func TestHeapReleaseBoundsDelayUnderSustainedWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var log releaseLog
		h := &heapRelease{release: log.release}
		start := time.Now()
		// Computations keep arriving with gaps shorter than releaseIdle.
		for time.Since(start) < 3*releaseMaxDelay {
			compute(h, releaseIdle/4)
			time.Sleep(releaseIdle / 4)
		}
		synctest.Wait()
		log.mu.Lock()
		defer log.mu.Unlock()
		if len(log.at) < 2 {
			t.Fatalf("released %d times over %v of sustained work, want at least 2", len(log.at), 3*releaseMaxDelay)
		}
		prev := start
		for _, at := range log.at {
			if gap := at.Sub(prev); gap > releaseMaxDelay+releaseIdle {
				t.Fatalf("release came %v after the previous one, want at most %v", gap, releaseMaxDelay+releaseIdle)
			}
			prev = at
		}
	})
}
