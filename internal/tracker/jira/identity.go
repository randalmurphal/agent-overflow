package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"

	"agent-overflow/internal/tracker/trackerapi"
)

// myselfPath is the sign-in check: who the credential belongs to. Jira
// answers it 401, never 404, when the credential is refused.
const myselfPath = apiPath + "myself"

// errCheckDropped ends the 404s waiting for a check that had not started
// when the credential was dropped: no check can speak for the credential
// their requests carried any more.
var errCheckDropped = errors.New("jira: the credential was dropped before the sign-in could be checked")

// identity decides 404s. Jira answers some requests with 404 when the
// credential is refused, so a 404 counts as "not found" only when a
// sign-in check that started after the 404 arrived, under the same
// credential, succeeded. Nothing is remembered between checks.
//
// One check runs at a time (cur). A 404 that arrives while it runs
// cannot use it, since it started first: it joins the next check, which
// starts when cur ends, so a burst of 404s costs at most two checks.
// ForgetCredential and an AuthError on any call detach both: cur keeps
// answering the 404s already waiting on it, nothing later joins it, and
// the 404s queued for next end with errCheckDropped. The check goes
// straight to the transport, so its own 404 never runs a check.
type identity struct {
	mu     sync.Mutex
	cur    *signInCheck
	next   *signInCheck
	closed bool
	tasks  sync.WaitGroup
	// joined, set only by tests before first use, is told the check each
	// 404 waits on.
	joined func(*signInCheck)
}

// signInCheck is one GET of myself and the 404s waiting on it.
type signInCheck struct {
	done chan struct{}
	err  error
}

func newCheck() *signInCheck { return &signInCheck{done: make(chan struct{})} }

// end settles f with err.
func (f *signInCheck) end(err error) {
	f.err = err
	close(f.done)
}

// forget detaches the checks, so the next 404 starts a check of its own.
func (id *identity) forget() {
	id.mu.Lock()
	defer id.mu.Unlock()
	id.cur = nil
	if id.next != nil {
		id.next.end(errCheckDropped)
		id.next = nil
	}
}

// close refuses later checks and joins the one in flight, which the
// transport's Close has cancelled.
func (id *identity) close() {
	id.mu.Lock()
	id.closed = true
	if id.next != nil {
		id.next.end(errCheckDropped)
		id.next = nil
	}
	id.mu.Unlock()
	id.tasks.Wait()
}

// UnconfirmedError is a 404 the client could not tell from a refused
// credential, because the sign-in check failed. It does not match
// trackerapi.ErrNotFound: nothing is known to be missing. The 404 itself
// stays reachable through errors.As (As below), so
// trackerapi.WriteNotApplied reports a write it answered as not applied.
// Err is the check's failure, which Unwrap returns.
type UnconfirmedError struct {
	NotFound *trackerapi.StatusError
	Err      error
}

func (e *UnconfirmedError) Error() string {
	return fmt.Sprintf("jira: %s %s answered 404 and the sign-in check failed: %v",
		e.NotFound.Method, e.NotFound.URL, e.Err)
}

func (e *UnconfirmedError) Unwrap() error { return e.Err }

// As sets target to the 404 when the 404 fits it, for every target but
// the bare error and empty interfaces. errors.Is never calls As, so the
// 404 does not make the error match trackerapi.ErrNotFound, while
// errors.As finds it and trackerapi.WriteNotApplied reads its status.
func (e *UnconfirmedError) As(target any) bool {
	if e.NotFound == nil {
		return false
	}
	ptr := reflect.ValueOf(target)
	if ptr.Kind() != reflect.Pointer || ptr.IsNil() {
		return false
	}
	dst := ptr.Elem()
	if dst.Kind() == reflect.Interface && (dst.NumMethod() == 0 || dst.Type() == reflect.TypeFor[error]()) {
		return false
	}
	src := reflect.ValueOf(e.NotFound)
	if !src.Type().AssignableTo(dst.Type()) {
		return false
	}
	dst.Set(src)
	return true
}

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

// confirmSignedIn reports nil when a sign-in check that starts after
// this call succeeds: a new one when none runs, else the one queued
// behind the one running.
func (c *Client) confirmSignedIn(ctx context.Context) error {
	id := &c.id
	id.mu.Lock()
	if id.closed {
		id.mu.Unlock()
		return notSent("the client is closed")
	}
	var f *signInCheck
	switch {
	case id.cur == nil:
		f = newCheck()
		id.cur = f
		c.startCheck(ctx, f)
	case id.next != nil:
		f = id.next
	default:
		f = newCheck()
		id.next = f
	}
	if id.joined != nil {
		id.joined(f)
	}
	id.mu.Unlock()
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// startCheck runs f; the caller holds id.mu. The check outlives a caller
// that stops waiting, for the others sharing it; the transport's timeout
// and Close bound it.
func (c *Client) startCheck(ctx context.Context, f *signInCheck) {
	checkCtx := context.WithoutCancel(ctx)
	c.id.tasks.Go(func() { c.runSignInCheck(checkCtx, f) })
}

func (c *Client) runSignInCheck(ctx context.Context, f *signInCheck) {
	_, err := c.api.Do(ctx, trackerapi.Request{Path: myselfPath})
	if errors.Is(err, trackerapi.ErrNotFound) {
		// Not a missing user: the base URL is not a Jira site, or the
		// site's API moved. The error must not read as "not found".
		err = fmt.Errorf("jira: GET %s answered %d, so the sign-in cannot be checked", myselfPath, http.StatusNotFound)
	}
	id := &c.id
	id.mu.Lock()
	defer id.mu.Unlock()
	f.end(err)
	if id.cur != f {
		return // detached
	}
	id.cur = nil
	if id.next != nil && !id.closed {
		id.cur, id.next = id.next, nil
		c.startCheck(ctx, id.cur)
	}
}
