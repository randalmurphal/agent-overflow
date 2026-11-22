package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
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

// signInCheck is one GET of myself and the 404s waiting on it. mark is
// the credential read its answer was drawn under, set when it succeeded.
type signInCheck struct {
	done chan struct{}
	err  error
	mark trackerapi.CredentialMark
}

func newCheck() *signInCheck { return &signInCheck{done: make(chan struct{})} }

// end settles f with err, or with the mark of its successful answer.
func (f *signInCheck) end(mark trackerapi.CredentialMark, err error) {
	f.mark, f.err = mark, err
	close(f.done)
}

// errCredentialReplaced is a sign-in check drawn under another credential
// read than the 404 it was to decide: it says nothing about that 404, and
// the call runs again under the current credential.
var errCredentialReplaced = errors.New("jira: the credential was read again between the 404 and the sign-in check, so the check does not speak for the 404; run the call again")

// sameRead reports two answers drawn under the same read of the
// credential; answers that carried none are never the same.
func sameRead(a, b trackerapi.CredentialMark) bool {
	return a == b && a != (trackerapi.CredentialMark{})
}

// forget detaches the checks, so the next 404 starts a check of its own.
func (id *identity) forget() {
	id.mu.Lock()
	defer id.mu.Unlock()
	id.cur = nil
	if id.next != nil {
		id.next.end(trackerapi.CredentialMark{}, errCheckDropped)
		id.next = nil
	}
}

// close refuses later checks and joins the one in flight, which the
// transport's Close has cancelled.
func (id *identity) close() {
	id.mu.Lock()
	id.closed = true
	if id.next != nil {
		id.next.end(trackerapi.CredentialMark{}, errCheckDropped)
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

// checkNotFound decides notFound, a 404. One that names the account it
// was made for (answeredFor) is the same error, with nothing sent. One
// that names none gets a sign-in check: the same error when a check drawn
// under the same credential read succeeds, an error holding the
// *trackerapi.AuthError when that credential is refused, and an
// *UnconfirmedError otherwise: the check failed, or its answer was drawn
// under another read of the credential than the 404 was.
func (c *Client) checkNotFound(ctx context.Context, notFound error) error {
	status, _ := errors.AsType[*trackerapi.StatusError](notFound)
	if answeredFor(status.Header) != "" {
		return notFound
	}
	refused, err := c.checkRead(ctx, status.CredentialMark())
	switch {
	case err == nil:
		return notFound
	case refused:
		return fmt.Errorf("jira: the answer was 404 because the credential was refused: %w", err)
	}
	return &UnconfirmedError{NotFound: status, Err: err}
}

// checkRead runs a sign-in check for an answer drawn under mark: nil when
// one that starts now succeeds under the same read of the credential; the
// check's *trackerapi.AuthError, refused true, when that credential is
// refused; otherwise why the check does not speak for the answer (it
// failed, or was drawn under another read, errCredentialReplaced).
func (c *Client) checkRead(ctx context.Context, mark trackerapi.CredentialMark) (refused bool, err error) {
	checked, err := c.confirmSignedIn(ctx)
	if err == nil {
		if sameRead(mark, checked) {
			return false, nil
		}
		return false, errCredentialReplaced
	}
	if ae, ok := outcomeOf(err).(*trackerapi.AuthError); ok && sameRead(mark, ae.CredentialMark()) {
		return true, err
	}
	return false, err
}

// unnamed are the credential reads of the answers to one read that name
// no account (answeredFor). What such an answer leaves out (an empty
// search, a bulk read missing issues) may be what an anonymous request
// sees, not what the user's does: Jira answers a search with no
// credential 200 with no issues and no login-reason header (measured).
type unnamed []trackerapi.CredentialMark

// note records resp when it names no account.
func (u *unnamed) note(resp *trackerapi.Response) {
	if resp == nil || answeredFor(resp.Header) != "" {
		return
	}
	if m := resp.CredentialMark(); !slices.Contains(*u, m) {
		*u = append(*u, m)
	}
}

// prove is nil when every answer u noted is shown to be the user's by a
// sign-in check under the same read of the credential (checkRead), so
// what the read left out is missing for the user. It costs one check per
// distinct read noted, none when every answer named its account. A
// refused credential is an error holding the *trackerapi.AuthError, and
// any other failure an *UnprovenError.
func (c *Client) prove(ctx context.Context, what string, u unnamed) error {
	for _, mark := range u {
		refused, err := c.checkRead(ctx, mark)
		switch {
		case err == nil:
			continue
		case refused:
			return fmt.Errorf("jira: %s left out what was asked because the credential was refused: %w", what, err)
		}
		return &UnprovenError{What: what, Err: err}
	}
	return nil
}

// UnprovenError is an answer that left out what was asked, named no
// account it was made for, and could not be shown to be the user's: the
// sign-in check failed or was drawn under another read of the
// credential. Nothing is known to be missing.
type UnprovenError struct {
	What string
	Err  error
}

func (e *UnprovenError) Error() string {
	return fmt.Sprintf("jira: %s left out what was asked, named no account, and the sign-in check failed: %v", e.What, e.Err)
}

func (e *UnprovenError) Unwrap() error { return e.Err }

// confirmSignedIn returns the credential mark of a sign-in check that
// starts after this call and succeeds: a new one when none runs, else
// the one queued behind the one running.
func (c *Client) confirmSignedIn(ctx context.Context) (trackerapi.CredentialMark, error) {
	id := &c.id
	id.mu.Lock()
	if id.closed {
		id.mu.Unlock()
		return trackerapi.CredentialMark{}, &trackerapi.NotSentError{Err: errors.New("jira: the client is closed")}
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
		return f.mark, f.err
	case <-ctx.Done():
		return trackerapi.CredentialMark{}, ctx.Err()
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
	resp, err := c.api.Do(ctx, trackerapi.Request{Path: myselfPath})
	if errors.Is(err, trackerapi.ErrCredentialChanged) {
		// The check reads the status and mark, never the body the
		// transport withheld: the credential was accepted.
		err = nil
	}
	var mark trackerapi.CredentialMark
	if err == nil {
		mark = resp.CredentialMark()
	}
	if errors.Is(err, trackerapi.ErrNotFound) {
		// Not a missing user: the base URL is not a Jira site, or the
		// site's API moved. The error must not read as "not found".
		err = fmt.Errorf("jira: GET %s answered %d, so the sign-in cannot be checked", myselfPath, http.StatusNotFound)
	}
	id := &c.id
	id.mu.Lock()
	defer id.mu.Unlock()
	f.end(mark, err)
	if id.cur != f {
		return // detached
	}
	id.cur = nil
	if id.next != nil && !id.closed {
		id.cur, id.next = id.next, nil
		c.startCheck(ctx, id.cur)
	}
}
