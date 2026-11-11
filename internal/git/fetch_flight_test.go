package git

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A worktree cut's bounded wait starts a fetch the background cadence then
// joins: the cut's bound ends only the cut's wait, and the cadence gets the
// fetch's answer.
func TestFetchFlightOutlivesTheCallerThatStartedIt(t *testing.T) {
	t.Parallel()
	var flights fetchFlights
	started := make(chan struct{})
	release := make(chan struct{})
	killed := make(chan error, 1)
	fetch := func(ctx context.Context) (bool, error) {
		close(started)
		select {
		case <-release:
			killed <- nil
			return true, nil
		case <-ctx.Done():
			killed <- ctx.Err()
			return false, ctx.Err()
		}
	}
	seed, leaveSeed := context.WithCancel(t.Context())
	seedDone := make(chan error, 1)
	go func() {
		_, err := flights.do(seed, "repo", fetch)
		seedDone <- err
	}()
	<-started
	cadenceDone := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		close(joined)
		fetched, err := flights.do(t.Context(), "repo", func(context.Context) (bool, error) {
			return false, errors.New("the cadence started a second fetch")
		})
		if err == nil && !fetched {
			err = errors.New("the cadence was not answered by the shared fetch")
		}
		cadenceDone <- err
	}()
	<-joined
	waitForWaiters(t, &flights, "repo", 2)
	leaveSeed()
	if err := <-seedDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("seed after its bound: %v, want context.Canceled", err)
	}
	select {
	case err := <-killed:
		t.Fatalf("the seed leaving ended the fetch the cadence waits for: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-cadenceDone; err != nil {
		t.Fatal(err)
	}
}

// A fetch nobody waits for any longer is killed, and the next caller
// starts a fresh one rather than joining it.
func TestFetchFlightEndsWithItsLastWaiter(t *testing.T) {
	t.Parallel()
	var flights fetchFlights
	started := make(chan struct{})
	killed := make(chan error, 1)
	caller, leave := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := flights.do(caller, "repo", func(ctx context.Context) (bool, error) {
			close(started)
			<-ctx.Done()
			killed <- ctx.Err()
			return false, ctx.Err()
		})
		done <- err
	}()
	<-started
	leave()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller after leaving: %v", err)
	}
	if err := <-killed; !errors.Is(err, context.Canceled) {
		t.Fatalf("fetch with no waiter: %v, want it cancelled", err)
	}
	fetched, err := flights.do(t.Context(), "repo", func(context.Context) (bool, error) { return true, nil })
	if err != nil || !fetched {
		t.Fatalf("next caller = %v, %v, want a fresh fetch", fetched, err)
	}
}

func waitForWaiters(t *testing.T, flights *fetchFlights, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		flights.mu.Lock()
		n := 0
		if flight := flights.flights[key]; flight != nil {
			n = flight.waiters
		}
		flights.mu.Unlock()
		if n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiters = %d, want %d", n, want)
		}
		time.Sleep(time.Millisecond)
	}
}
