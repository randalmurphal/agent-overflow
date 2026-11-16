package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// identityTTL is how long a successful sign-in check vouches for later
// 404s. A credential revoked within it reads as "not found" until it
// passes, unless an AuthError on another call drops the check first.
const identityTTL = 10 * time.Second

// myselfPath is the sign-in check: who the credential belongs to. Jira
// answers it 401, never 404, when the credential is refused.
const myselfPath = apiPath + "myself"

// identity is the last sign-in check and the one in flight. Jira answers
// some requests with 404 when the credential is refused, so a 404 counts
// as "not found" only while a check that started at most identityTTL ago
// succeeded. A burst of 404s shares one check; the check itself goes
// straight to the transport, so it never runs a check of its own.
type identity struct {
	mu       sync.Mutex
	ok       bool
	at       time.Time // when the successful check started
	inFlight *flight[struct{}]
	// gen changes when the check is dropped; a check started under an
	// older gen stores nothing.
	gen    uint64
	closed bool
	tasks  sync.WaitGroup
}

// forget drops the last check, so the next 404 checks again.
func (id *identity) forget() {
	id.mu.Lock()
	defer id.mu.Unlock()
	id.ok = false
	id.gen++
}

// close refuses later checks and joins the one in flight, which the
// transport's Close has cancelled.
func (id *identity) close() {
	id.mu.Lock()
	id.closed = true
	id.mu.Unlock()
	id.tasks.Wait()
}

// UnconfirmedError is a 404 the client could not tell from a refused
// credential, because the sign-in check failed. It does not match
// trackerapi.ErrNotFound: nothing is known to be missing. Err is the
// check's failure, which unwraps to the transport's error.
type UnconfirmedError struct {
	NotFound *trackerapi.StatusError
	Err      error
}

func (e *UnconfirmedError) Error() string {
	return fmt.Sprintf("jira: %s %s answered 404 and the sign-in check failed: %v",
		e.NotFound.Method, e.NotFound.URL, e.Err)
}

func (e *UnconfirmedError) Unwrap() error { return e.Err }

// checkNotFound decides notFound, a 404: the same error when the
// credential is good, an error holding the *trackerapi.AuthError when it
// is refused, and an *UnconfirmedError when the check fails otherwise.
func (c *Client) checkNotFound(ctx context.Context, notFound error) error {
	err := c.confirmSignedIn(ctx)
	if err == nil {
		return notFound
	}
	if _, ok := errors.AsType[*trackerapi.AuthError](err); ok {
		return fmt.Errorf("jira: the answer was 404 because the credential was refused: %w", err)
	}
	status, _ := errors.AsType[*trackerapi.StatusError](notFound)
	return &UnconfirmedError{NotFound: status, Err: err}
}

// confirmSignedIn reports nil when a sign-in check that started within
// identityTTL succeeded, running or joining one when none did.
func (c *Client) confirmSignedIn(ctx context.Context) error {
	id := &c.id
	id.mu.Lock()
	now := c.now()
	if id.ok && !now.Before(id.at) && now.Sub(id.at) < identityTTL {
		id.mu.Unlock()
		return nil
	}
	f := id.inFlight
	if f == nil && id.closed {
		id.mu.Unlock()
		return notSent("the client is closed")
	}
	if f == nil {
		f = &flight[struct{}]{done: make(chan struct{})}
		id.inFlight = f
		gen := id.gen
		// The check outlives a caller that stops waiting, for the others
		// sharing it; the transport's timeout and Close bound it.
		checkCtx := context.WithoutCancel(ctx)
		id.tasks.Go(func() { c.runSignInCheck(checkCtx, f, now, gen) })
	}
	id.mu.Unlock()
	_, err := f.await(ctx)
	return err
}

func (c *Client) runSignInCheck(ctx context.Context, f *flight[struct{}], started time.Time, gen uint64) {
	_, err := c.api.JSON(ctx, trackerapi.Request{Path: myselfPath}, nil)
	if errors.Is(err, trackerapi.ErrNotFound) {
		// Not a missing user: the base URL is not a Jira site, or the
		// site's API moved. The error must not read as "not found".
		err = fmt.Errorf("jira: GET %s answered %d, so the sign-in cannot be checked", myselfPath, http.StatusNotFound)
	}
	id := &c.id
	id.mu.Lock()
	if id.inFlight == f {
		id.inFlight = nil
	}
	if err == nil && gen == id.gen {
		id.ok, id.at = true, started
	}
	id.mu.Unlock()
	f.err = err
	close(f.done)
}
