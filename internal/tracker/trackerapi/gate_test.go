package trackerapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A closed gate fails a background call at once, unsent. An interactive
// call waits when the gate opens soon and fails the same way otherwise.
func TestRateGate(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{
		"GET /limited": reply(http.StatusTooManyRequests, "Retry-After", "8"),
		"GET /me":      reply(http.StatusOK),
	})
	c, clock := loopbackClient(t, testOptions(tr.url()))
	_, err := c.Do(t.Context(), Request{Path: "limited"})
	opensAt := clock.now().Add(8 * time.Second)
	if _, ok := errors.AsType[*RateLimitedError](err); !ok {
		t.Fatalf("429: %v", err)
	}

	_, err = c.Do(t.Context(), Request{Path: "me"})
	rate, ok := errors.AsType[*RateLimitedError](err)
	if _, notSent := errors.AsType[*NotSentError](err); !ok || !notSent || rate.Status != 0 || !rate.Until.Equal(opensAt) || !WriteNotApplied(err) {
		t.Fatalf("background behind a closed gate: %v", err)
	}
	if tr.count("GET /me") != 0 {
		t.Fatal("a background call went out through a closed gate")
	}

	if _, err := c.Do(WithInteractive(t.Context()), Request{Path: "me"}); err != nil {
		t.Fatalf("interactive behind a short closure: %v", err)
	}
	if !slices.Equal(clock.slept(), seconds(8)) || tr.count("GET /me") != 1 {
		t.Fatalf("interactive waited %v and sent %d", clock.slept(), tr.count("GET /me"))
	}

	c.gate.close(clock.now().Add(interactiveGateWait + time.Second))
	_, err = c.Do(WithInteractive(t.Context()), Request{Path: "me"})
	if _, ok := errors.AsType[*NotSentError](err); !ok || tr.count("GET /me") != 1 {
		t.Fatalf("interactive behind a long closure: %v", err)
	}
}

// A call waiting for its pacing turn checks the gate again before it is
// sent: a rate limit another call met meanwhile stops it.
func TestPacedCallChecksTheGateAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		}, func(o *Options) { o.MaxPerSecond = 1 })
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for range 2 {
			wg.Go(func() {
				_, err := c.Do(t.Context(), Request{Path: "a"})
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		notSent := 0
		for err := range errs {
			if _, ok := errors.AsType[*RateLimitedError](err); !ok {
				t.Fatalf("Do = %v, want a rate limit", err)
			}
			if _, ok := errors.AsType[*NotSentError](err); ok {
				notSent++
			}
		}
		if requests.Load() != 1 || notSent != 1 {
			t.Fatalf("%d requests and %d unsent calls, want the paced call stopped by the gate", requests.Load(), notSent)
		}
	})
}

// A closure never shortens an open one.
func TestGateKeepsTheLaterClosure(t *testing.T) {
	t.Parallel()
	var g gate
	now := time.Unix(1_800_000_000, 0)
	g.close(now.Add(30 * time.Second))
	if until := g.close(now.Add(5 * time.Second)); !until.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("a shorter closure moved the gate to %v", until)
	}
	if _, closed := g.closedUntil(now.Add(30 * time.Second)); closed {
		t.Fatal("the gate is closed at its opening time")
	}
}

// An interactive call waiting for the gate stops when its context ends,
// unsent.
func TestCancelWhileWaitingForTheGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("a call went out through a closed gate")
		}, nil)
		c.gate.close(time.Now().Add(5 * time.Second))
		ctx, cancel := context.WithCancel(WithInteractive(t.Context()))
		done := make(chan error)
		go func() {
			_, err := c.Do(ctx, Request{Method: http.MethodPost, Path: "issue", Body: map[string]string{}})
			done <- err
		}()
		synctest.Wait()
		cancel()
		err := <-done
		if _, ok := errors.AsType[*NotSentError](err); !ok || !errors.Is(err, context.Canceled) || !WriteNotApplied(err) {
			t.Fatalf("Do = %v, want an unsent cancellation", err)
		}
	})
}

// MaxPerSecond spaces every request, concurrent ones included.
func TestPacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var sent []time.Time
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			sent = append(sent, time.Now())
			mu.Unlock()
		}, func(o *Options) { o.MaxPerSecond = 5 })
		var wg sync.WaitGroup
		for range 5 {
			wg.Go(func() {
				if _, err := c.Do(t.Context(), Request{Path: "worklogs"}); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		slices.SortFunc(sent, func(a, b time.Time) int { return a.Compare(b) })
		for i := 1; i < len(sent); i++ {
			if gap := sent[i].Sub(sent[i-1]); gap < 200*time.Millisecond {
				t.Fatalf("requests %d and %d were %s apart, want at least 200ms", i-1, i, gap)
			}
		}
		if total := sent[len(sent)-1].Sub(sent[0]); total != 800*time.Millisecond {
			t.Fatalf("five requests took %s, want 800ms", total)
		}
	})
}

// A call that leaves while waiting for its turn takes no slot: the next
// call goes one interval after the last request sent.
func TestPacingWaiterThatLeavesTakesNoSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var sent []time.Time
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			sent = append(sent, time.Now())
			mu.Unlock()
		}, func(o *Options) { o.MaxPerSecond = 1 })
		start := time.Now()
		if _, err := c.Do(t.Context(), Request{Path: "a"}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() {
			_, err := c.Do(ctx, Request{Path: "a"})
			done <- err
		}()
		synctest.Wait()
		time.Sleep(500 * time.Millisecond)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter: %v", err)
		}
		if _, err := c.Do(t.Context(), Request{Path: "a"}); err != nil {
			t.Fatal(err)
		}
		if len(sent) != 2 || sent[1].Sub(start) != time.Second {
			t.Fatalf("sent at %v, want the second one second after the first", sent)
		}
	})
}
