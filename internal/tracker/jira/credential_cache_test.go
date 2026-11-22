package jira

import (
	"net/http"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/tracker/jira/jiratest"
)

// rotatedSite is a site that knows user A's credential until revoke and
// user B's always: myself answers each with its own user, and with 401
// for A once revoked. Every other route a test adds answers both.
type rotatedSite struct {
	*jiratest.Server
	revoked atomic.Bool
}

func newRotatedSite(t *testing.T) *rotatedSite {
	s := &rotatedSite{Server: unnamedSite(t)}
	s.On("GET", myselfURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		user, _, _ := r.BasicAuth()
		switch {
		case user == "user1@example.com" && s.revoked.Load():
			jiratest.Reply(401, `{"errorMessages":["Client must be authenticated"]}`)(w, r, b)
		case user == "user1@example.com":
			jiratest.Reply(200, userOneJSON)(w, r, b)
		default:
			jiratest.Reply(200, userTwoJSON)(w, r, b)
		}
	})
	s.On("GET", issuePath404, jiratest.Reply(404, `{"errorMessages":["Issue does not exist or you do not have permission to see it."]}`))
	return s
}

// TestMeAfterARotationOutsideCall is the reviewer's case: Me holds user
// A; A is revoked; an issue's 404 that names no account runs the sign-in
// check, which the transport answers 401 under A and retries under B,
// all outside Client.call. Me must not hand out A: it asks again and
// answers B.
func TestMeAfterARotationOutsideCall(t *testing.T) {
	f := newRotatedSite(t)
	src := &rotatingCredentials{}
	c := markClient(t, f.Server, src, nil)
	if u, err := c.Me(t.Context()); err != nil || u.AccountID != "acc-user1" {
		t.Fatalf("me before: %+v %v", u, err)
	}
	f.revoked.Store(true)
	if _, err := c.GetIssue(t.Context(), "WEB-404"); err == nil {
		t.Fatal("a 404 read as found")
	}
	if n := src.reads.Load(); n != 2 {
		t.Fatalf("%d credential reads, want 2: the rotation did not happen", n)
	}
	before := f.Count("GET", myselfURL)
	u, err := c.Me(t.Context())
	if err != nil || u.AccountID != "acc-user2" {
		t.Fatalf("me after the rotation: %+v %v, want user 2", u, err)
	}
	if n := f.Count("GET", myselfURL) - before; n != 1 {
		t.Errorf("%d sign-in reads after the rotation, want 1", n)
	}
}

// TestCachesFollowTheCredential proves every per-credential value the
// client holds (the field catalog, a board's configuration and sprints,
// users) is read again once the transport holds another credential read,
// however the rotation happened.
func TestCachesFollowTheCredential(t *testing.T) {
	f := newRotatedSite(t)
	f.On("GET", "/rest/api/3/field", jiratest.Reply(200, catalogJSON))
	f.On("GET", boardConfigURL, jiratest.Reply(200, boardConfigBody))
	f.On("GET", "/rest/agile/1.0/board/3/sprint", jiratest.Reply(200, `{"isLast":true,"values":[]}`))
	f.On("GET", "/rest/api/3/user", jiratest.Reply(200, userTwoJSON))
	c := markClient(t, f.Server, &rotatingCredentials{}, nil)
	reads := map[string]func() error{
		"/rest/api/3/field":              func() error { _, err := c.Fields(t.Context()); return err },
		boardConfigURL:                   func() error { _, err := c.BoardConfig(t.Context(), 3); return err },
		"/rest/agile/1.0/board/3/sprint": func() error { _, err := c.BoardSprints(t.Context(), 3); return err },
		"/rest/api/3/user":               func() error { _, err := c.User(t.Context(), "acc-user2"); return err },
	}
	for range 2 {
		for path, read := range reads {
			if err := read(); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
		}
	}
	f.revoked.Store(true)
	_, _ = c.GetIssue(t.Context(), "WEB-404") // the rotation, in the sign-in check
	for path, read := range reads {
		if err := read(); err != nil {
			t.Fatalf("%s after the rotation: %v", path, err)
		}
		if n := f.Count("GET", path); n != 2 {
			t.Errorf("%s read %d times, want once under each credential", path, n)
		}
	}
}
