package jira

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

// rotatingCredentials hands out user A's credential on the first read and
// user B's on every later one, and counts the reads.
type rotatingCredentials struct{ reads atomic.Int32 }

func (s *rotatingCredentials) Credential(context.Context) (*trackerapi.Credential, error) {
	if s.reads.Add(1) == 1 {
		return trackerapi.Basic("user1@example.com", secret.New([]byte("fake-token-A"))), nil
	}
	return trackerapi.Basic("user2@example.com", secret.New([]byte("fake-token-B"))), nil
}

// markSite is a fake site that answers every issue with 404 and the
// sign-in check by user: myself[user] is its status.
func markSite(t *testing.T, myself map[string]int) *fakeJira {
	f := newFake(t)
	f.on("GET", issuePath404, reply(404, `{}`))
	f.on("DELETE", "/rest/api/3/attachment/404", reply(404, `{}`))
	f.on("GET", myselfURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		user, _, _ := r.BasicAuth()
		status, ok := myself[user]
		if !ok {
			t.Errorf("sign-in check under %q", user)
		}
		if status == 200 {
			reply(200, userOneJSON)(w, r, b)
			return
		}
		reply(status, `{"errorMessages":["Client must be authenticated"]}`)(w, r, b)
	})
	return f
}

// markClient is a client on f reading src, whose OnAnswer is onAnswer
// with the client in hand.
func markClient(t *testing.T, f *fakeJira, src trackerapi.CredentialSource, onAnswer func(c *Client, status int)) *Client {
	t.Helper()
	var (
		mu sync.Mutex
		c  *Client
	)
	opts := Options{BaseURL: f.srv.URL, Version: "test", Credentials: src}
	if onAnswer != nil {
		opts.OnAnswer = func(status int, _ http.Header) {
			mu.Lock()
			cl := c
			mu.Unlock()
			onAnswer(cl, status)
		}
	}
	cl, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	c = cl
	mu.Unlock()
	t.Cleanup(cl.Close)
	return cl
}

// wantReplaced requires err to be a 404 left unconfirmed because the
// credential changed between it and the check: not "not found", and a
// write it answered is not applied. A refusal of the new credential stays
// reachable as the cause, since it is true of the current credential.
func wantReplaced(t *testing.T, what string, err error) {
	t.Helper()
	ue, ok := errors.AsType[*UnconfirmedError](err)
	switch {
	case !ok || ue.NotFound == nil || ue.NotFound.Status != 404:
		t.Errorf("%s: got %v, want an unconfirmed 404", what, err)
	case errors.Is(err, trackerapi.ErrNotFound):
		t.Errorf("%s: a check under another credential decided the 404: %v", what, err)
	case !trackerapi.WriteNotApplied(err):
		t.Errorf("%s: the 404 no longer proves the write was not applied: %v", what, err)
	}
}

// TestCheckUnderAReplacedCredential is the 404 drawn under A whose check,
// refused under A, is retried by the transport under B and succeeds: B's
// success says nothing about A's 404.
func TestCheckUnderAReplacedCredential(t *testing.T) {
	f := markSite(t, map[string]int{"user1@example.com": 401, "user2@example.com": 200})
	src := &rotatingCredentials{}
	c := markClient(t, f, src, nil)
	_, err := c.GetIssue(t.Context(), "WEB-404")
	wantReplaced(t, "read", err)
	if !errors.Is(err, errCredentialReplaced) {
		t.Errorf("the cause is not named: %v", err)
	}
	if n := src.reads.Load(); n != 2 {
		t.Errorf("%d credential reads, want 2: the sequence did not run", n)
	}
	// Under B now, a 404 and its check share the credential read.
	if _, err := c.GetIssue(t.Context(), "WEB-404"); !errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("a 404 confirmed under the same credential: %v", err)
	}
	err = c.DeleteAttachment(t.Context(), "404")
	if !errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("a write's 404 confirmed under the same credential: %v", err)
	}
}

// TestCredentialForgottenAsTheNotFoundArrives is ForgetCredential called
// from OnAnswer when the 404's headers arrive: the check reads B, and
// neither its success nor its refusal speaks for A's 404.
func TestCredentialForgottenAsTheNotFoundArrives(t *testing.T) {
	for _, tc := range []struct {
		name   string
		underB int
	}{{"check succeeds", 200}, {"check refused", 401}} {
		t.Run(tc.name, func(t *testing.T) {
			f := markSite(t, map[string]int{"user2@example.com": tc.underB})
			src := &rotatingCredentials{}
			var forgot atomic.Int32
			c := markClient(t, f, src, func(c *Client, status int) {
				if status == 404 && forgot.Add(1) == 1 {
					c.ForgetCredential()
				}
			})
			_, err := c.GetIssue(t.Context(), "WEB-404")
			wantReplaced(t, "read", err)
			if forgot.Load() == 0 || src.reads.Load() < 2 {
				t.Fatalf("the sequence did not run: forgot %d, reads %d", forgot.Load(), src.reads.Load())
			}
		})
	}
	// A write's 404 under the same sequence stays not applied.
	f := markSite(t, map[string]int{"user2@example.com": 200})
	var forgot atomic.Int32
	c := markClient(t, f, &rotatingCredentials{}, func(c *Client, status int) {
		if status == 404 && forgot.Add(1) == 1 {
			c.ForgetCredential()
		}
	})
	wantReplaced(t, "write", c.DeleteAttachment(t.Context(), "404"))
}

// TestAnswersWithoutACredentialAreNeverTheSameRead proves the zero mark,
// which an answer that carried no credential has, vouches for nothing,
// though two zero marks compare equal.
func TestAnswersWithoutACredentialAreNeverTheSameRead(t *testing.T) {
	if sameRead(trackerapi.CredentialMark{}, trackerapi.CredentialMark{}) {
		t.Error("two answers without a credential count as one read")
	}
}
