package trackerapi

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
)

// walkAccounts is a tracker of accounts a, b and c, the first, second and
// later credentials of its source. GET /first and GET /second answer with
// the account's letter, and GET /missing answers 404. An account that
// used up its budget of answered requests is refused with 401, so the
// transport reads the credential again and asks as the next account.
type walkAccounts struct {
	*tracker
	c      *Client
	mu     sync.Mutex
	budget map[string]int // answered requests left; absent is unlimited
}

func newWalkAccounts(t *testing.T, budget map[string]int) *walkAccounts {
	t.Helper()
	s := &walkAccounts{budget: budget}
	users := map[string]string{"Bearer " + testSecret: "a", "Bearer " + testSecret2: "b", "Bearer fake-token-3": "c"}
	as := func(answer func(w http.ResponseWriter, user string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			user := users[r.Header.Get("Authorization")]
			s.mu.Lock()
			left, limited := s.budget[user]
			if limited {
				s.budget[user] = left - 1
			}
			s.mu.Unlock()
			if user == "" || limited && left <= 0 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			answer(w, user)
		}
	}
	letter := as(func(w http.ResponseWriter, user string) { writeString(w, user) })
	s.tracker = newTracker(t, route{
		"GET /first":   letter,
		"GET /second":  letter,
		"GET /missing": as(func(w http.ResponseWriter, _ string) { w.WriteHeader(http.StatusNotFound) }),
	})
	opts := testOptions(s.url())
	opts.Credentials = &testSource{secrets: []string{testSecret, testSecret2, "fake-token-3"}}
	s.c, _ = loopbackClient(t, opts)
	return s
}

// get is the account letter path answered with, "missing" for a 404 and
// "?" for any other failure: a read that keeps a request's failure in
// what it returns.
func (s *walkAccounts) get(ctx context.Context, path string) string {
	resp, err := s.c.Do(ctx, Request{Path: path})
	switch {
	case errors.Is(err, ErrNotFound):
		return "missing"
	case err != nil:
		return "?"
	}
	return string(resp.Body)
}

// A not-found is a fact a read reports, so one drawn under another
// credential than the walk's first answer starts the walk again: the
// read returns the second account's first answer, never the first
// account's beside the second's not-found.
func TestWalkStartsAgainOnAnErrorAnswerOfAnotherCredential(t *testing.T) {
	t.Parallel()
	s := newWalkAccounts(t, map[string]int{"a": 1})
	got, err := Walk(t.Context(), func(ctx context.Context) (string, error) {
		return s.get(ctx, "first") + "," + s.get(ctx, "missing"), nil
	})
	if err != nil || got != "b,missing" {
		t.Fatalf("got %q, %v; want b's answer and its not-found", got, err)
	}
	if n := s.count("GET /first"); n != 2 {
		t.Fatalf("%d first requests, want 2: the walk did not start again", n)
	}
}

// A second change, seen on an error answer too, fails the walk.
func TestWalkFailsWhenAnErrorAnswerMovesItAgain(t *testing.T) {
	t.Parallel()
	// a answers first; b the not-found and first again; c the not-found.
	s := newWalkAccounts(t, map[string]int{"a": 1, "b": 2})
	got, err := Walk(t.Context(), func(ctx context.Context) (string, error) {
		return s.get(ctx, "first") + "," + s.get(ctx, "missing"), nil
	})
	if !errors.Is(err, ErrCredentialChanged) || got != "" {
		t.Fatalf("got %q, %v; want nothing and ErrCredentialChanged", got, err)
	}
}

// A read that keeps a request's failure in what it returns (a board it
// could not judge) does not hide a credential change: the walk starts
// again whatever the read returned.
func TestWalkStartsAgainWhenTheReadKeptTheMove(t *testing.T) {
	t.Parallel()
	s := newWalkAccounts(t, map[string]int{"a": 1})
	got, err := Walk(t.Context(), func(ctx context.Context) (string, error) {
		return s.get(ctx, "first") + "," + s.get(ctx, "second"), nil
	})
	if err != nil || got != "b,b" {
		t.Fatalf("got %q, %v; want b's two answers", got, err)
	}
}

// A walk that cannot start again fails on a change its read kept.
func TestWalkOnceFailsWhenTheReadKeptTheMove(t *testing.T) {
	t.Parallel()
	s := newWalkAccounts(t, map[string]int{"a": 1})
	var handed string
	err := WalkOnce(t.Context(), func(ctx context.Context) error {
		handed = s.get(ctx, "first") + "," + s.get(ctx, "second")
		return nil
	})
	if !errors.Is(err, ErrCredentialChanged) {
		t.Fatalf("handed over %q with %v, want ErrCredentialChanged", handed, err)
	}
}

// A cached value of another credential read moves the walk even when the
// cache reads it as a miss and nothing is then asked.
func TestWalkStartsAgainOnAJoinedValueOfAnotherCredential(t *testing.T) {
	t.Parallel()
	s := newWalkAccounts(t, map[string]int{"a": 1})
	if s.get(t.Context(), "first") != "a" {
		t.Fatal("a did not answer")
	}
	stale := s.c.CredentialMark()
	if s.get(t.Context(), "second") != "b" { // a is refused, b answers
		t.Fatal("b did not answer")
	}
	runs := 0
	got, err := Walk(t.Context(), func(ctx context.Context) (string, error) {
		runs++
		out := s.get(ctx, "first")
		if runs == 1 && JoinWalk(ctx, stale) == nil {
			t.Error("a value of a's read joined a walk of b's")
		}
		return out, nil
	})
	if err != nil || got != "b" || runs != 2 {
		t.Fatalf("got %q, %v in %d runs; want b in 2", got, err, runs)
	}
}

// A write is refused unsent inside a walk, which may run again.
func TestWalkRefusesAWrite(t *testing.T) {
	t.Parallel()
	s := newWalkAccounts(t, nil)
	_, err := Walk(t.Context(), func(ctx context.Context) (struct{}, error) {
		_, err := s.c.Do(ctx, Request{Method: http.MethodPost, Path: "first"})
		return struct{}{}, err
	})
	if !errors.Is(err, ErrInvalidInput) || !WriteNotApplied(err) || s.count("POST /first") != 0 {
		t.Fatalf("got %v after %d requests, want an unsent refusal", err, s.count("POST /first"))
	}
}
