package jira

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/tracker/trackerapi"
)

const (
	issuePath404 = "/rest/api/3/issue/WEB-404"
	myselfURL    = "/rest/api/3/myself"
)

// tokenState is a fake site whose credential can be revoked.
type tokenState struct{ revoked atomic.Bool }

func (s *tokenState) myself(w http.ResponseWriter, r *http.Request, b []byte) {
	if s.revoked.Load() {
		reply(401, `{"errorMessages":["Client must be authenticated"]}`)(w, r, b)
		return
	}
	reply(200, userOneJSON)(w, r, b)
}

func TestNotFoundConfirmedOncePerBurst(t *testing.T) {
	f := newFake(t)
	c, clock := newTestClient(t, f)
	tokens := &tokenState{}
	f.on("GET", myselfURL, tokens.myself)
	f.on("GET", issuePath404, reply(404, `{"errorMessages":["Issue does not exist or you do not have permission to see it."]}`))

	for range 3 {
		_, err := c.GetIssue(t.Context(), "WEB-404")
		if !errors.Is(err, trackerapi.ErrNotFound) || isAuth(err) {
			t.Fatalf("got %v, want not found", err)
		}
	}
	if n := f.count("GET", myselfURL); n != 1 {
		t.Fatalf("a burst of 404s checked the sign-in %d times, want 1", n)
	}
	clock.advance(identityTTL)
	if _, err := c.GetIssue(t.Context(), "WEB-404"); !errors.Is(err, trackerapi.ErrNotFound) {
		t.Fatal(err)
	}
	if n := f.count("GET", myselfURL); n != 2 {
		t.Fatalf("an expired check was not repeated: %d checks", n)
	}
}

func TestNotFoundWithRefusedCredentialIsAuthError(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	tokens := &tokenState{}
	tokens.revoked.Store(true)
	f.on("GET", myselfURL, tokens.myself)
	f.on("GET", issuePath404, reply(404, `{"errorMessages":["Issue does not exist"]}`))
	_, err := c.GetIssue(t.Context(), "WEB-404")
	if !isAuth(err) || errors.Is(err, trackerapi.ErrNotFound) {
		t.Fatalf("got %v, want an AuthError that is not a not found", err)
	}
	// A refused check is not remembered as a success.
	if _, err := c.GetIssue(t.Context(), "WEB-404"); !isAuth(err) {
		t.Fatalf("second 404: %v", err)
	}
}

// TestNotFoundAfterRevocation proves a credential revoked mid-session
// turns 404s into AuthErrors once the check expires, and at once after
// an AuthError on another call drops the check.
func TestNotFoundAfterRevocation(t *testing.T) {
	for _, viaAuthError := range []bool{false, true} {
		f := newFake(t)
		c, clock := newTestClient(t, f)
		tokens := &tokenState{}
		f.on("GET", myselfURL, tokens.myself)
		f.on("GET", issuePath404, reply(404, `{}`))
		f.on("GET", "/rest/api/3/field", func(w http.ResponseWriter, r *http.Request, b []byte) {
			reply(401, `{}`)(w, r, b)
		})
		if _, err := c.GetIssue(t.Context(), "WEB-404"); !errors.Is(err, trackerapi.ErrNotFound) {
			t.Fatal(err)
		}
		tokens.revoked.Store(true)
		if viaAuthError {
			if _, err := c.Fields(t.Context()); !isAuth(err) {
				t.Fatalf("field catalog: %v", err)
			}
		} else {
			clock.advance(identityTTL + 1)
		}
		if _, err := c.GetIssue(t.Context(), "WEB-404"); !isAuth(err) {
			t.Fatalf("after revocation (via AuthError %v): %v", viaAuthError, err)
		}
	}
}

// TestNotFoundCheckCannotLoop proves a sign-in check answered with 404
// fails as unconfirmed, after exactly one check, and is not "not found".
func TestNotFoundCheckCannotLoop(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("GET", myselfURL, reply(404, `{}`))
	f.on("GET", issuePath404, reply(404, `{}`))
	_, err := c.GetIssue(t.Context(), "WEB-404")
	ue, ok := errors.AsType[*UnconfirmedError](err)
	if !ok || errors.Is(err, trackerapi.ErrNotFound) || ue.NotFound == nil || ue.NotFound.Status != 404 {
		t.Fatalf("got %v", err)
	}
	if n := f.count("GET", myselfURL); n != 1 {
		t.Errorf("%d checks, want 1", n)
	}
}

func TestNotFoundCheckFailingIsUnconfirmed(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("GET", myselfURL, reply(500, `{"errorMessages":["down"]}`))
	f.on("DELETE", "/rest/api/3/issueLink/77", reply(404, `{}`))
	err := c.UnlinkIssues(t.Context(), "77")
	if _, ok := errors.AsType[*UnconfirmedError](err); !ok || errors.Is(err, trackerapi.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if se, ok := errors.AsType[*trackerapi.StatusError](err); !ok || se.Status != 500 {
		t.Errorf("the check's own error must stay reachable: %v", err)
	}
	if trackerapi.WriteNotApplied(err) {
		t.Error("an unconfirmed 404 must not claim the write was refused")
	}
}

// TestNotFoundConcurrentBurstSharesOneCheck proves concurrent 404s join
// the check in flight, and a waiter that gives up does not end it for
// the others.
func TestNotFoundConcurrentBurstSharesOneCheck(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	f.on("GET", myselfURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		entered <- struct{}{}
		<-release
		reply(200, userOneJSON)(w, r, b)
	})
	f.on("GET", issuePath404, reply(404, `{}`))

	quitter, quit := context.WithCancel(t.Context())
	quitErr := make(chan error, 1)
	go func() {
		_, err := c.GetIssue(quitter, "WEB-404")
		quitErr <- err
	}()
	<-entered
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { _, errs[i] = c.GetIssue(t.Context(), "WEB-404") })
	}
	quit()
	if err := <-quitErr; !errors.Is(err, context.Canceled) {
		t.Errorf("the caller that gave up: %v", err)
	}
	close(release)
	wg.Wait()
	for _, err := range errs {
		if !errors.Is(err, trackerapi.ErrNotFound) {
			t.Errorf("got %v", err)
		}
	}
	if n := f.count("GET", myselfURL); n != 1 {
		t.Errorf("%d checks, want 1", n)
	}
}

func TestForgetCredentialDropsTheCheck(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("GET", myselfURL, reply(200, userOneJSON))
	f.on("GET", issuePath404, reply(404, `{}`))
	for range 2 {
		if _, err := c.GetIssue(t.Context(), "WEB-404"); !errors.Is(err, trackerapi.ErrNotFound) {
			t.Fatal(err)
		}
		c.ForgetCredential()
	}
	if n := f.count("GET", myselfURL); n != 2 {
		t.Errorf("%d checks, want one per credential", n)
	}
}

// TestForgetDuringCheckIsNotOverwritten proves a check that started
// before ForgetCredential answers its own waiters but does not vouch for
// the credential read after it.
func TestForgetDuringCheckIsNotOverwritten(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	f.on("GET", myselfURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		reply(200, userOneJSON)(w, r, b)
	})
	f.on("GET", issuePath404, reply(404, `{}`))
	first := make(chan error, 1)
	go func() {
		_, err := c.GetIssue(t.Context(), "WEB-404")
		first <- err
	}()
	<-entered
	c.ForgetCredential()
	close(release)
	if err := <-first; !errors.Is(err, trackerapi.ErrNotFound) {
		t.Fatalf("the waiting caller: %v", err)
	}
	if _, err := c.GetIssue(t.Context(), "WEB-404"); !errors.Is(err, trackerapi.ErrNotFound) {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("%d checks, want a new one for the credential read after the forget", n)
	}
}

// TestNotFoundAfterCloseStartsNoCheck covers a 404 that lands as the
// client closes: nothing is sent and the 404 stays unconfirmed.
func TestNotFoundAfterCloseStartsNoCheck(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	c.Close()
	notFound := &trackerapi.StatusError{Method: "GET", URL: "https://example.atlassian.net/rest/api/3/issue/WEB-404", Status: 404}
	err := c.checkNotFound(t.Context(), notFound)
	if _, ok := errors.AsType[*UnconfirmedError](err); !ok || errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("got %v", err)
	}
	if n := f.count("GET", myselfURL); n != 0 {
		t.Errorf("%d checks after Close", n)
	}
}
