package trackerapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
)

func TestCloseZeroesTheCredentialAndRefusesCalls(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{"GET /me": reply(http.StatusOK)})
	src := &testSource{secrets: []string{testSecret}}
	opts := testOptions(tr.url())
	opts.Credentials = src
	c, _ := loopbackClient(t, opts)
	if _, err := c.Do(t.Context(), Request{Path: "me"}); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if !src.zeroed(0) {
		t.Fatal("Close left the secret behind")
	}
	c.Close()
	_, doErr := c.Do(t.Context(), Request{Path: "me"})
	_, streamErr := c.Stream(t.Context(), Request{Path: "me"}, &bytes.Buffer{}, 10)
	_, locationErr := c.Location(t.Context(), Request{Path: "me"})
	for _, err := range []error{doErr, streamErr, locationErr} {
		if _, ok := errors.AsType[*NotSentError](err); !ok || !errors.Is(err, errClosed) {
			t.Fatalf("after Close: %v, want a *NotSentError", err)
		}
	}
	if tr.count("GET /me") != 1 || src.readCount() != 1 {
		t.Fatalf("after Close: %d requests, %d reads", tr.count("GET /me"), src.readCount())
	}
}

// Close ends a call in flight; a write it ended is unconfirmed.
func TestCloseEndsCallsInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) { hang(r) }, nil)
		done := make(chan error)
		go func() {
			_, err := c.Do(t.Context(), Request{Method: http.MethodPost, Path: "issue", Body: map[string]string{}})
			done <- err
		}()
		synctest.Wait()
		c.Close()
		err := <-done
		if !errors.Is(err, errClosed) || WriteNotApplied(err) {
			t.Fatalf("Do = %v, want an unconfirmed closed error", err)
		}
	})
}

// Close ends the credential read in flight and waits for it; the call
// waiting on the read fails unsent.
func TestCloseEndsTheCredentialRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &testSource{secrets: []string{testSecret}, gate: make(chan struct{})}
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("a call went out without a credential")
		}, func(o *Options) { o.Credentials = src })
		done := make(chan error)
		go func() {
			_, err := c.Do(t.Context(), Request{Path: "me"})
			done <- err
		}()
		synctest.Wait()
		c.Close()
		err := <-done
		if _, ok := errors.AsType[*NotSentError](err); !ok || !errors.Is(err, errClosed) {
			t.Fatalf("Do = %v, want an unsent closed error", err)
		}
	})
}

// A caller that leaves stops its own wait; the read it shared goes on
// for the others, and a cancelled request in flight is not transient.
func TestCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &testSource{secrets: []string{testSecret}, gate: make(chan struct{})}
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/hang" {
				hang(r)
			}
		}, func(o *Options) { o.Credentials = src })

		leaving, leave := context.WithCancel(t.Context())
		left := make(chan error)
		stayed := make(chan error)
		go func() {
			_, err := c.Do(leaving, Request{Path: "me"})
			left <- err
		}()
		go func() {
			_, err := c.Do(t.Context(), Request{Path: "me"})
			stayed <- err
		}()
		synctest.Wait()
		leave()
		if err := <-left; !errors.Is(err, context.Canceled) || !WriteNotApplied(err) {
			t.Fatalf("leaving the credential wait: %v, want an unsent cancellation", err)
		}
		close(src.gate)
		if err := <-stayed; err != nil || src.readCount() != 1 {
			t.Fatalf("the caller that stayed: %v after %d reads", err, src.readCount())
		}

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() {
			_, err := c.Do(ctx, Request{Method: http.MethodPatch, Path: "hang", Body: map[string]string{}})
			done <- err
		}()
		synctest.Wait()
		cancel()
		err := <-done
		if _, transient := errors.AsType[*TransientError](err); transient || !errors.Is(err, context.Canceled) || WriteNotApplied(err) {
			t.Fatalf("cancelled mid-request: %v, want an unconfirmed cancellation", err)
		}
	})
}
