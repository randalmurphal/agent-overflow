package trackerapp

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/linear/lineartest"
)

// afterARefusedRefresh makes a list and then each call: a provider read,
// a provider write and a Verify. It returns the Verify's connection and
// the connection as a list after it shows it.
func afterARefusedRefresh(t *testing.T, f *fixture, id string, p tracker.Provider, read func() error) (Connection, Connection) {
	t.Helper()
	var verified Connection
	for i, call := range []func(){
		func() {
			if err := read(); err == nil {
				t.Error("a read after the refused refresh succeeded")
			}
		},
		func() {
			res := p.AddComment(t.Context(), tracker.CommentInput{Issue: "10001", Markdown: "hello"})
			if len(res.Parts) != 1 || res.Parts[0].Outcome.State() != tracker.StateNotSent {
				t.Errorf("a write after the refused refresh = %+v", res)
			}
		},
		func() {
			got, err := f.svc.Verify(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			verified = got
		},
	} {
		if _, err := f.svc.Connections(); err != nil {
			t.Fatal(err)
		}
		call()
		if t.Failed() {
			t.Fatalf("call %d after the refused refresh", i)
		}
	}
	list, err := f.svc.Connections()
	if err != nil {
		t.Fatal(err)
	}
	return verified, list[0]
}

// Key A is accepted for the connection's workspace; the file then holds
// key B of another workspace; a call under A is answered 401, the client
// reads B and the workspace check refuses it. From then on no request
// carries A: a read, a write and a Verify, each after a list, send
// nothing, and the connection shows the wrong-workspace problem. A right
// key C in the file recovers it with no restart.
func TestARefusedLinearRefreshDropsTheOldKey(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addLinear()
	const keyA, keyB, keyC = linearKey, "lin_api_fake1111111111", "lin_api_fake2222222222"
	teams := func(key string, status int) lineartest.Exchange {
		e := lineartest.Exchange{Op: "Teams", Key: key, Vars: `{"first":100}`, Body: lineartest.Data(lineartest.PageJSON(nil, false, ""))}
		if status != 0 {
			e.Status = status
			e.Body = `{"errors":[{"message":"Authentication required","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`
		}
		return e
	}
	f.writeToken(conn, keyA)
	f.linear.Expect(linearCheck(keyA, "example"), teams(keyA, 0))
	p := f.provider(conn.ID)
	read := func() error { _, err := p.Projects(t.Context()); return err }
	if err := read(); err != nil {
		t.Fatal(err)
	}

	f.writeToken(conn, keyB)
	f.linear.Expect(teams(keyA, http.StatusUnauthorized), linearCheck(keyB, "other-co"))
	if err := read(); err == nil {
		t.Fatal("the read answered 401 succeeded")
	}

	var mu sync.Mutex
	var unexpected []string
	f.linear.SetReport(func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		unexpected = append(unexpected, fmt.Sprintf(format, args...))
	})
	verified, listed := afterARefusedRefresh(t, f, conn.ID, p, read)
	mu.Lock()
	for _, u := range unexpected {
		if strings.Contains(u, keyA) {
			t.Errorf("a request after the refused refresh carried key A: %s", u)
		} else {
			t.Errorf("a request was sent after the refused refresh: %s", u)
		}
	}
	mu.Unlock()
	setup := "this Linear API key belongs to the workspace other-co, not example; make a key in example"
	for _, c := range []Connection{verified, listed} {
		if c.Problem == nil || c.Problem.Kind != FailureSetup || c.Problem.Message != setup {
			t.Fatalf("the connection shows %+v, want the wrong-workspace problem", c.Problem)
		}
	}

	f.writeToken(conn, keyC)
	f.linear.Expect(linearCheck(keyC, "example"),
		lineartest.Exchange{Op: "Viewer", Key: keyC, Body: fmt.Sprintf(viewerJSON, "example")}, teams(keyC, 0))
	if got := f.verify(conn.ID); got.Problem != nil || got.VerifiedAt == 0 {
		t.Fatalf("Verify with key C = %+v, problem %v", got.TrackerConnection, got.Problem)
	}
	if err := read(); err != nil {
		t.Fatalf("a read with key C = %v", err)
	}
}

// The same for a Jira connection whose key file becomes unreadable: after
// the 401 under token A and the failed re-read, no request carries A,
// and a readable token C recovers it.
func TestARefusedJiraRefreshDropsTheOldToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addJira()
	var mu sync.Mutex
	var sent []string
	record := func(r *http.Request) string {
		mu.Lock()
		defer mu.Unlock()
		auth := r.Header.Get("Authorization")
		sent = append(sent, auth)
		return auth
	}
	accepted := 0
	f.jira.On("GET", myselfPath, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch auth := record(r); {
		case auth == basic(token("c")), auth == basic(token("a")) && accepted == 0:
			accepted++
			jiratest.Reply(http.StatusOK, myselfJSON)(w, r, nil)
		default:
			jiratest.Reply(http.StatusUnauthorized, `{}`)(w, r, nil)
		}
	})
	f.jira.Otherwise(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		record(r)
		jiratest.Reply(http.StatusNotFound, `{}`)(w, r, nil)
	})
	f.writeToken(conn, token("a"))
	p := f.provider(conn.ID)
	read := func() error { _, err := p.Identity(t.Context()); return err }
	if err := read(); err != nil {
		t.Fatal(err)
	}

	f.writeToken(conn, token("b")+" "+token("x")) // refused: more than one token
	if err := read(); err == nil {
		t.Fatal("the read answered 401 succeeded")
	}
	mu.Lock()
	mark := len(sent)
	mu.Unlock()

	verified, listed := afterARefusedRefresh(t, f, conn.ID, p, read)
	mu.Lock()
	for i, auth := range sent[mark:] {
		if auth == basic(token("a")) {
			t.Errorf("request %d after the refused refresh carried token A", mark+i)
		} else {
			t.Errorf("request %d was sent after the refused refresh", mark+i)
		}
	}
	mu.Unlock()
	for _, c := range []Connection{verified, listed} {
		if c.Problem == nil || c.Problem.Kind != FailureSetup || !strings.Contains(c.Problem.Message, "Jira API token file") {
			t.Fatalf("the connection shows %+v, want the key file problem", c.Problem)
		}
	}

	f.writeToken(conn, token("c"))
	if got := f.verify(conn.ID); got.Problem != nil || got.VerifiedAt == 0 {
		t.Fatalf("Verify with token C = %+v, problem %v", got.TrackerConnection, got.Problem)
	}
	if err := read(); err != nil {
		t.Fatalf("a read with token C = %v", err)
	}
}
