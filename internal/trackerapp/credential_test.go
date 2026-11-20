package trackerapp

import (
	"context"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// reads is how many times connection id's client read its key file.
func (f *fixture) reads(id string) int64 {
	f.t.Helper()
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	c := f.svc.clients[id]
	if c == nil {
		f.t.Fatalf("%s has no client", id)
	}
	return c.source.reads.Load()
}

// Listing the connections while a verification waits for its answer
// leaves the verification alone: it succeeds and is recorded.
func TestAListDuringAVerifyLeavesItAlone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addJira()
	f.writeToken(conn, jiraToken)
	arrived := make(chan struct{})
	release := make(chan struct{})
	f.jira.On("GET", myselfPath, func(w http.ResponseWriter, r *http.Request, body []byte) {
		arrived <- struct{}{}
		<-release
		myself(t, jiraToken)(w, r, body)
	})
	done := make(chan Connection, 1)
	go func() {
		got, err := f.svc.Verify(context.Background(), conn.ID)
		if err != nil {
			t.Error(err)
		}
		done <- got
	}()
	<-arrived
	for range 3 {
		if _, err := f.svc.Connections(); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	got := <-done
	if got.Problem != nil || got.VerifiedAt == 0 || f.row(conn.ID).VerifiedAt == 0 {
		t.Fatalf("a Verify listed under = %+v, problem %v", got.TrackerConnection, got.Problem)
	}
}

// A token the file still holds is never dropped: across calls, lists, key
// file opens and verifications the client reads its key file once.
func TestAnUnchangedTokenIsReadOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addJira()
	f.writeToken(conn, jiraToken)
	f.jira.On("GET", myselfPath, myself(t, jiraToken))
	p := f.provider(conn.ID)
	for range 2 {
		if _, err := p.Identity(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.Connections(); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.PrepareKeyFile(conn.ID); err != nil {
			t.Fatal(err)
		}
		f.verify(conn.ID)
	}
	if got := f.reads(conn.ID); got != 1 {
		t.Fatalf("the key file was read %d times, want once", got)
	}
	if got := len(f.jira.Requests()); got != 4 {
		t.Fatalf("Jira saw %d requests, want 4", got)
	}
}

// A token replaced in the file is used by the next call after a list or
// a verification, and a file emptied or removed stops the client using
// the token it held.
func TestAReplacedTokenIsUsedAfterTheNextLook(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addJira()
	var mu sync.Mutex
	var sent []string
	f.jira.On("GET", myselfPath, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		mu.Lock()
		sent = append(sent, r.Header.Get("Authorization"))
		mu.Unlock()
		jiratest.Reply(http.StatusOK, myselfJSON)(w, r, nil)
	})
	p := f.provider(conn.ID)
	f.writeToken(conn, "token-a")
	if _, err := p.Identity(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.writeToken(conn, "token-b")
	f.verify(conn.ID)
	f.writeToken(conn, "token-c")
	if _, err := f.svc.Connections(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Identity(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, empty := range []func(){
		func() { f.writeToken(conn, "\n") },
		func() {
			if err := os.Remove(conn.KeyFilePath); err != nil {
				t.Fatal(err)
			}
		},
	} {
		f.writeToken(conn, "token-d")
		f.verify(conn.ID)
		empty()
		list, err := f.svc.Connections()
		if err != nil {
			t.Fatal(err)
		}
		if state := list[0].KeyFile.State; state == keyfile.StatePresent {
			t.Fatalf("the list shows the key file %s", state)
		}
		if _, err := p.Identity(t.Context()); err == nil {
			t.Fatal("a call after the key file was emptied succeeded")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{basic("token-a"), basic("token-b"), basic("token-c"), basic("token-d"), basic("token-d")}
	if len(sent) != len(want) {
		t.Fatalf("the requests carried %d credentials, want %d", len(sent), len(want))
	}
	for i := range want {
		if sent[i] != want[i] {
			t.Fatalf("request %d carried another credential than the file held then", i)
		}
	}
}

// An answer that arrives after the token in the key file was replaced
// (a list made the client drop the old one) is refused by the transport
// as trackerapi.ErrCredentialChanged. The verification it ends returns
// with that transient problem in place of the one found before, records
// no verification, and the next verification, with the new token,
// clears it.
func TestAVerifyAnsweredAfterTheTokenChangedIsTransient(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addJira()
	f.writeToken(conn, "token-a")
	arrived := make(chan struct{})
	release := make(chan struct{})
	var n atomic.Int32
	f.jira.On("GET", myselfPath, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch n.Add(1) {
		case 1:
			jiratest.Reply(http.StatusForbidden, `{}`)(w, r, nil)
		case 2:
			arrived <- struct{}{}
			<-release
			jiratest.Reply(http.StatusOK, myselfJSON)(w, r, nil)
		default:
			myself(t, "token-b")(w, r, nil)
		}
	})
	if got := f.verify(conn.ID); got.Problem == nil || got.Problem.Kind != FailureForbidden {
		t.Fatalf("the first Verify = %+v", got.Problem)
	}
	done := make(chan Connection, 1)
	go func() {
		got, err := f.svc.Verify(context.Background(), conn.ID)
		if err != nil {
			t.Error(err)
		}
		done <- got
	}()
	<-arrived
	f.writeToken(conn, "token-b")
	if _, err := f.svc.Connections(); err != nil {
		t.Fatal(err)
	}
	close(release)
	got := <-done
	want := Failure{Kind: FailureTransient, Message: "Jira answered as the token in the key file was being replaced; try again"}
	if got.Problem == nil || *got.Problem != want || got.VerifiedAt != 0 || f.row(conn.ID).VerifiedAt != 0 {
		t.Fatalf("a Verify answered after the token changed = %+v, problem %v", got.TrackerConnection, got.Problem)
	}
	if got := f.verify(conn.ID); got.Problem != nil || got.VerifiedAt == 0 {
		t.Fatalf("the Verify after = %+v, problem %v", got.TrackerConnection, got.Problem)
	}
}
