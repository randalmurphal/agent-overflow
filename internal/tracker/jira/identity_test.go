package jira

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/tracker/jira/jiratest"
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
		jiratest.Reply(401, `{"errorMessages":["Client must be authenticated"]}`)(w, r, b)
		return
	}
	jiratest.Reply(200, userOneJSON)(w, r, b)
}

// TestSequentialNotFoundsEachCheck proves nothing is reused between
// checks: every 404 is decided by a check that started after it.
func TestSequentialNotFoundsEachCheck(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	tokens := &tokenState{}
	f.On("GET", myselfURL, tokens.myself)
	f.On("GET", issuePath404, jiratest.Reply(404, `{"errorMessages":["Issue does not exist or you do not have permission to see it."]}`))
	for range 3 {
		_, err := c.GetIssue(t.Context(), "WEB-404")
		if !errors.Is(err, trackerapi.ErrNotFound) || isAuth(err) {
			t.Fatalf("got %v, want not found", err)
		}
	}
	if n := f.Count("GET", myselfURL); n != 3 {
		t.Fatalf("three 404s in turn made %d checks, want 3", n)
	}
}

// TestRevokedRightAfterConfirmedNotFound proves a 404 right after a
// confirmed one, once the credential is revoked, is an AuthError.
func TestRevokedRightAfterConfirmedNotFound(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	tokens := &tokenState{}
	f.On("GET", myselfURL, tokens.myself)
	f.On("GET", issuePath404, jiratest.Reply(404, `{}`))
	if _, err := c.GetIssue(t.Context(), "WEB-404"); !errors.Is(err, trackerapi.ErrNotFound) {
		t.Fatal(err)
	}
	tokens.revoked.Store(true)
	_, err := c.GetIssue(t.Context(), "WEB-404")
	if !isAuth(err) || errors.Is(err, trackerapi.ErrNotFound) {
		t.Fatalf("after revocation: %v", err)
	}
	if !trackerapi.WriteNotApplied(err) {
		t.Errorf("a refused credential proves nothing was applied: %v", err)
	}
}

// TestNotFoundCheckCannotLoop proves a sign-in check answered with 404
// fails as unconfirmed, after exactly one check, and is not "not found".
func TestNotFoundCheckCannotLoop(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", myselfURL, jiratest.Reply(404, `{}`))
	f.On("GET", issuePath404, jiratest.Reply(404, `{}`))
	_, err := c.GetIssue(t.Context(), "WEB-404")
	ue, ok := errors.AsType[*UnconfirmedError](err)
	if !ok || errors.Is(err, trackerapi.ErrNotFound) || ue.NotFound == nil || ue.NotFound.Status != 404 {
		t.Fatalf("got %v", err)
	}
	if n := f.Count("GET", myselfURL); n != 1 {
		t.Errorf("%d checks, want 1", n)
	}
}

// TestUnconfirmedWriteWasNotApplied proves a write answered 404 is not
// applied whatever the sign-in check says, while the 404 still does not
// read as "not found" and the check's own failure stays reachable.
func TestUnconfirmedWriteWasNotApplied(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", myselfURL, jiratest.Reply(500, `{"errorMessages":["down"]}`))
	f.On("DELETE", "/rest/api/3/issueLink/77", jiratest.Reply(404, `{}`))
	err := c.UnlinkIssues(t.Context(), "77")
	ue, ok := errors.AsType[*UnconfirmedError](err)
	if !ok || errors.Is(err, trackerapi.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if !trackerapi.WriteNotApplied(err) {
		t.Error("a write answered 404 was not applied")
	}
	if se, ok := errors.AsType[*trackerapi.StatusError](err); !ok || se.Status != 404 {
		t.Errorf("the 404 must stay reachable: %v", err)
	}
	if se, ok := errors.AsType[*trackerapi.StatusError](ue.Err); !ok || se.Status != 500 {
		t.Errorf("the check's own failure must stay reachable: %v", ue.Err)
	}
	var target error
	if (&UnconfirmedError{NotFound: ue.NotFound}).As(&target) {
		t.Error("As must not claim the bare error target")
	}
}

// joinRecorder counts the 404s waiting on sign-in checks and names the
// check each waits on.
type joinRecorder struct {
	mu     sync.Mutex
	checks []*signInCheck
	n      chan struct{}
}

func newJoinRecorder(c *Client) *joinRecorder {
	j := &joinRecorder{n: make(chan struct{}, 64)}
	c.id.joined = func(f *signInCheck) {
		j.mu.Lock()
		j.checks = append(j.checks, f)
		j.mu.Unlock()
		j.n <- struct{}{}
	}
	return j
}

func (j *joinRecorder) wait(n int) {
	for range n {
		<-j.n
	}
}

func (j *joinRecorder) check(i int) *signInCheck {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.checks[i]
}

// distinct is how many checks the 404s joined.
func (j *joinRecorder) distinct() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	seen := map[*signInCheck]bool{}
	for _, f := range j.checks {
		seen[f] = true
	}
	return len(seen)
}

// ended reports whether f has its answer.
func ended(f *signInCheck) bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// newGate is a channel a fake handler waits on and the function that
// opens it. The test's cleanup opens it too, before the server closes,
// so a test that fails while a handler waits ends at once.
func newGate(t *testing.T) (<-chan struct{}, func()) {
	ch := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(open)
	return ch, open
}

// TestNotFoundBurstCostsTwoChecks proves concurrent 404s share checks:
// the first starts one, every 404 that arrives while it runs queues for
// the next, so a burst of N costs two, not N.
func TestNotFoundBurstCostsTwoChecks(t *testing.T) {
	const n = 8
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	joins := newJoinRecorder(c)
	release, open := newGate(t)
	var checks atomic.Int32
	f.On("GET", myselfURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if checks.Add(1) == 1 {
			<-release
		}
		jiratest.Reply(200, userOneJSON)(w, r, b)
	})
	f.On("GET", issuePath404, jiratest.Reply(404, `{}`))
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() { _, errs[i] = c.GetIssue(t.Context(), "WEB-404") })
	}
	joins.wait(n)
	if d := joins.distinct(); d != 2 {
		t.Fatalf("a burst of %d joined %d checks, want 2", n, d)
	}
	open()
	wg.Wait()
	for _, err := range errs {
		if !errors.Is(err, trackerapi.ErrNotFound) {
			t.Errorf("got %v", err)
		}
	}
	if got := checks.Load(); got != 2 {
		t.Errorf("a burst of %d made %d checks, want 2", n, got)
	}
}

// TestCallerThatGivesUpLeavesTheCheck proves a waiter that stops waiting
// does not end the check for the others sharing it.
func TestCallerThatGivesUpLeavesTheCheck(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	joins := newJoinRecorder(c)
	release, open := newGate(t)
	f.On("GET", myselfURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		<-release
		jiratest.Reply(200, userOneJSON)(w, r, b)
	})
	f.On("GET", issuePath404, jiratest.Reply(404, `{}`))
	quitter, quit := context.WithCancel(t.Context())
	quitErr, stayErr := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := c.GetIssue(quitter, "WEB-404")
		quitErr <- err
	}()
	joins.wait(1)
	go func() {
		_, err := c.GetIssue(t.Context(), "WEB-404")
		stayErr <- err
	}()
	joins.wait(1)
	quit()
	if err := <-quitErr; !errors.Is(err, context.Canceled) {
		t.Errorf("the caller that gave up: %v", err)
	}
	open()
	if err := <-stayErr; !errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("the caller that stayed: %v", err)
	}
}

// TestForgetDetachesTheCheckInFlight is the race of a 404 that arrives
// after ForgetCredential while a check made with the old credential
// runs: it must not join that check, and the new credential's refusal
// decides it.
func TestForgetDetachesTheCheckInFlight(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	joins := newJoinRecorder(c)
	release, open := newGate(t)
	oldEntered := make(chan struct{})
	var checks atomic.Int32
	f.On("GET", myselfURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if checks.Add(1) == 1 {
			close(oldEntered)
			<-release
			jiratest.Reply(200, userOneJSON)(w, r, b)
			return
		}
		jiratest.Reply(401, `{}`)(w, r, b)
	})
	f.On("GET", issuePath404, jiratest.Reply(404, `{}`))
	first := make(chan error, 1)
	go func() {
		_, err := c.GetIssue(t.Context(), "WEB-404")
		first <- err
	}()
	// The old check's request reaches the site before the forget, so
	// the check the next 404 starts is the second the site sees.
	<-oldEntered
	joins.wait(1)
	c.ForgetCredential()
	second := make(chan error, 1)
	go func() {
		_, err := c.GetIssue(t.Context(), "WEB-404")
		second <- err
	}()
	joins.wait(1)
	c.id.mu.Lock()
	running := c.id.cur
	c.id.mu.Unlock()
	if joins.check(1) == joins.check(0) || running != joins.check(1) {
		t.Fatal("a 404 after ForgetCredential joined or queued behind the old credential's check")
	}
	if err := <-second; !isAuth(err) {
		t.Errorf("the 404 after the forget: %v", err)
	}
	open()
	if err := <-first; !errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("the 404 the old check answers: %v", err)
	}
}

// TestDetachedCheckLeavesTheNewOne proves a check detached by
// ForgetCredential, ending while the new credential's check runs, does
// not displace that check or start the one queued behind it early.
func TestDetachedCheckLeavesTheNewOne(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	joins := newJoinRecorder(c)
	oldGate, openOld := newGate(t)
	laterGate, openNew := newGate(t)
	entered := make(chan struct{}, 3)
	var checks atomic.Int32
	f.On("GET", myselfURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		n := checks.Add(1)
		entered <- struct{}{}
		switch n {
		case 1:
			<-oldGate
		case 2:
			<-laterGate
		}
		jiratest.Reply(200, userOneJSON)(w, r, b)
	})
	f.On("GET", issuePath404, jiratest.Reply(404, `{}`))
	errs := make(chan error, 3)
	notFound := func() {
		_, err := c.GetIssue(t.Context(), "WEB-404")
		errs <- err
	}
	go notFound()
	joins.wait(1)
	<-entered
	c.ForgetCredential()
	go notFound()
	joins.wait(1)
	c.id.mu.Lock()
	started := c.id.cur == joins.check(1)
	c.id.mu.Unlock()
	if !started {
		t.Fatal("the 404 after ForgetCredential did not start a check of its own")
	}
	<-entered
	go notFound() // queued behind the new credential's check
	joins.wait(1)
	openOld()
	if err := <-errs; !errors.Is(err, trackerapi.ErrNotFound) {
		t.Fatalf("the 404 the old check answers: %v", err)
	}
	// The old check ended under the lock; taking it waits for that.
	c.id.mu.Lock()
	cur, next := c.id.cur, c.id.next
	c.id.mu.Unlock()
	if cur != joins.check(1) || next != joins.check(2) {
		t.Fatal("the detached check displaced the running one or started the queued one")
	}
	openNew()
	for range 2 {
		if err := <-errs; !errors.Is(err, trackerapi.ErrNotFound) {
			t.Errorf("got %v", err)
		}
	}
	if n := checks.Load(); n != 3 {
		t.Errorf("%d checks, want 3", n)
	}
}

// TestForgetEndsQueuedNotFounds proves the 404s queued behind a check end
// when the credential is dropped, since no later check can speak for the
// credential their requests carried.
func TestForgetEndsQueuedNotFounds(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	joins := newJoinRecorder(c)
	release, open := newGate(t)
	// The running check's request is on the wire before the credential
	// is dropped, so it was drawn under the same read as the 404s.
	sent := make(chan struct{}, 1)
	f.On("GET", myselfURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		sent <- struct{}{}
		<-release
		jiratest.Reply(200, userOneJSON)(w, r, b)
	})
	f.On("GET", issuePath404, jiratest.Reply(404, `{}`))
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := c.GetIssue(t.Context(), "WEB-404")
			errs <- err
		}()
		joins.wait(1)
	}
	if joins.distinct() != 2 {
		t.Fatal("the second 404 did not queue for a check of its own")
	}
	<-sent
	c.ForgetCredential()
	if !ended(joins.check(1)) || ended(joins.check(0)) {
		t.Fatal("ForgetCredential must end the queued check and leave the running one")
	}
	queued := <-errs
	if _, ok := errors.AsType[*UnconfirmedError](queued); !ok || !errors.Is(queued, errCheckDropped) {
		t.Errorf("the queued 404: %v", queued)
	}
	open()
	if err := <-errs; !errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("the 404 the running check answers: %v", err)
	}
}

func TestAuthErrorDetachesTheCheck(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	joins := newJoinRecorder(c)
	release, open := newGate(t)
	f.On("GET", myselfURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		<-release
		jiratest.Reply(200, userOneJSON)(w, r, b)
	})
	f.On("GET", issuePath404, jiratest.Reply(404, `{}`))
	f.On("GET", "/rest/api/3/field", jiratest.Reply(401, `{}`))
	first := make(chan error, 1)
	go func() {
		_, err := c.GetIssue(t.Context(), "WEB-404")
		first <- err
	}()
	joins.wait(1)
	if _, err := c.Fields(t.Context()); !isAuth(err) {
		t.Fatalf("field catalog: %v", err)
	}
	c.id.mu.Lock()
	detached := c.id.cur == nil
	c.id.mu.Unlock()
	if !detached {
		t.Error("an AuthError left the check joinable")
	}
	open()
	if err := <-first; !errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("the 404 the detached check answers: %v", err)
	}
}

// TestNotFoundAfterCloseStartsNoCheck covers a 404 that lands as the
// client closes: nothing is sent and the 404 stays unconfirmed.
func TestNotFoundAfterCloseStartsNoCheck(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	joins := newJoinRecorder(c)
	c.Close()
	notFound := &trackerapi.StatusError{Method: "GET", URL: "https://example.atlassian.net/rest/api/3/issue/WEB-404", Status: 404}
	err := c.checkNotFound(t.Context(), notFound)
	if _, ok := errors.AsType[*UnconfirmedError](err); !ok || errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("got %v", err)
	}
	// A check started after Close would race the wait for the checks.
	if joins.distinct() != 0 {
		t.Error("a check started after Close")
	}
	if n := f.Count("GET", myselfURL); n != 0 {
		t.Errorf("%d checks after Close", n)
	}
}
