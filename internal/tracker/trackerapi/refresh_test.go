package trackerapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/secret"
)

// authorizations is the Authorization of every request tr received.
func (tr *tracker) authorizations() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out := make([]string, len(tr.reqs))
	for i, r := range tr.reqs {
		out[i] = r.Header.Get("Authorization")
	}
	return out
}

// accepting answers 200 to a request carrying Bearer value and 401 to any
// other.
func accepting(value string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+value {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeString(w, `{}`)
	}
}

// readsSource is a source whose n-th read (from 1) is answer(n).
func readsSource(answer func(ctx context.Context, n int) (*Credential, error)) CredentialSource {
	var n atomic.Int32
	return sourceFunc(func(ctx context.Context) (*Credential, error) {
		return answer(ctx, int(n.Add(1)))
	})
}

var errUnreadable = errors.New("key file unreadable")

// A re-read that fails after a 401 drops the refused credential: the next
// call reads the source again and sends nothing with it, and once the
// source answers, calls flow under what it returns.
func TestFailedRereadDropsTheRefusedCredential(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{"GET /me": accepting(testSecret2)})
	refused := Bearer(secret.New([]byte(testSecret)))
	refusedBytes := refused.secret.Reveal()
	var reads atomic.Int32
	opts := testOptions(tr.url())
	opts.Credentials = readsSource(func(_ context.Context, n int) (*Credential, error) {
		reads.Store(int32(n))
		switch n {
		case 1:
			return refused, nil
		case 2, 3:
			return nil, errUnreadable
		}
		return Bearer(secret.New([]byte(testSecret2))), nil
	})
	c, _ := loopbackClient(t, opts)

	_, err := c.Do(t.Context(), Request{Path: "me"})
	if auth, ok := errors.AsType[*AuthError](err); !ok || !errors.Is(auth.Err, errUnreadable) {
		t.Fatalf("the refused call: %v, want an *AuthError carrying the failed re-read", err)
	}
	if !isZero(refusedBytes) {
		t.Error("the refused credential is still held")
	}
	_, err = c.Do(t.Context(), Request{Path: "me"})
	if _, ok := errors.AsType[*NotSentError](err); !ok || !errors.Is(err, errUnreadable) || reads.Load() != 3 {
		t.Fatalf("the next call: %v after %d reads, want the source read again and nothing sent", err, reads.Load())
	}
	if _, err := c.Do(t.Context(), Request{Path: "me"}); err != nil {
		t.Fatalf("once the source answers: %v", err)
	}
	if got, want := tr.authorizations(), []string{"Bearer " + testSecret, "Bearer " + testSecret2}; !slices.Equal(got, want) {
		t.Errorf("the tracker received %q, want %q", got, want)
	}
}

// A call in flight with the refused credential when the failed re-read
// drops it gets ErrCredentialChanged for its 2xx answer, whose body can no
// longer be checked for the zeroed credential.
func TestCallInFlightWhenTheRefusedCredentialDrops(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	tr := newTracker(t, route{
		"GET /slow": func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-release
			writeString(w, `{"a":1}`)
		},
		"GET /me": accepting(testSecret2),
	})
	opts := testOptions(tr.url())
	opts.Credentials = readsSource(func(_ context.Context, n int) (*Credential, error) {
		if n == 1 {
			return Bearer(secret.New([]byte(testSecret))), nil
		}
		return nil, errUnreadable
	})
	c, _ := loopbackClient(t, opts)
	slow := make(chan error, 1)
	go func() {
		_, err := c.Do(t.Context(), Request{Path: "slow"})
		slow <- err
	}()
	<-entered
	if _, err := c.Do(t.Context(), Request{Path: "me"}); err == nil {
		t.Fatal("the refused call succeeded")
	}
	close(release)
	if err := <-slow; !errors.Is(err, ErrCredentialChanged) {
		t.Fatalf("the call in flight: %v, want ErrCredentialChanged", err)
	}
}

// A caller that stops waiting for the re-read its 401 started drops
// nothing: the read completes and what it stored is used by the next call.
func TestEndedWaiterKeepsTheReread(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{"GET /me": accepting(testSecret2)})
	started, gate := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	opts := testOptions(tr.url())
	opts.Credentials = readsSource(func(_ context.Context, n int) (*Credential, error) {
		reads.Store(int32(n))
		if n == 1 {
			return Bearer(secret.New([]byte(testSecret))), nil
		}
		close(started)
		<-gate
		return Bearer(secret.New([]byte(testSecret2))), nil
	})
	c, _ := loopbackClient(t, opts)
	ctx, cancel := context.WithCancel(t.Context())
	waiter := make(chan error, 1)
	go func() {
		_, err := c.Do(ctx, Request{Path: "me"})
		waiter <- err
	}()
	<-started
	cancel()
	if err := <-waiter; !errors.Is(err, context.Canceled) {
		t.Fatalf("the waiter: %v", err)
	}
	close(gate)
	if _, err := c.Do(t.Context(), Request{Path: "me"}); err != nil || reads.Load() != 2 {
		t.Fatalf("the next call: %v after %d reads, want the re-read's credential", err, reads.Load())
	}
}
