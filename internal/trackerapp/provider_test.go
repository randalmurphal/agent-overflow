package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

func (f *fixture) provider(id string) tracker.Provider {
	f.t.Helper()
	p, err := f.svc.Provider(context.Background(), id)
	if err != nil {
		f.t.Fatalf("Provider(%s): %v", id, err)
	}
	return p
}

// timeKind names the time tracker a Jira provider logs through.
func timeKind(p tracker.Provider) string { return fmt.Sprintf("%T", p.Time()) }

func TestProviderFollowsTheTempoConnection(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	jira := f.addJira()
	const jiraTime, tempoTime = "*jiraprovider.jiraTime", "*jiraprovider.tempoTime"
	if got := timeKind(f.provider(jira.ID)); got != jiraTime {
		t.Fatalf("a Jira connection without Tempo logs through %s", got)
	}
	tmp := f.addTempo(jira.ID)
	if got := timeKind(f.provider(jira.ID)); got != tempoTime {
		t.Fatalf("with Tempo added the provider logs through %s", got)
	}
	if _, err := f.svc.SetEnabled(tmp.ID, false); err != nil {
		t.Fatal(err)
	}
	if got := timeKind(f.provider(jira.ID)); got != jiraTime || f.inUse(tmp.ID) {
		t.Fatalf("with Tempo off the provider logs through %s (Tempo in use %v)", got, f.inUse(tmp.ID))
	}
	if _, err := f.svc.SetEnabled(tmp.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := timeKind(f.provider(jira.ID)); got != tempoTime {
		t.Fatalf("with Tempo on again the provider logs through %s", got)
	}
	if err := f.svc.Remove(tmp.ID); err != nil {
		t.Fatal(err)
	}
	if got := timeKind(f.provider(jira.ID)); got != jiraTime {
		t.Fatalf("with Tempo removed the provider logs through %s", got)
	}
	if !f.inUse(jira.ID) {
		t.Fatal("removing the Tempo connection stopped its Jira connection")
	}
}

func TestProviderRefuses(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	jira := f.addJira()
	tmp := f.addTempo(jira.ID)
	if _, err := f.svc.Provider(t.Context(), tmp.ID); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("Provider of a Tempo connection = %v", err)
	}
	if f.inUse(tmp.ID) {
		t.Fatal("a refused Provider built the Tempo client")
	}
	if p := f.provider(f.addLinear().ID); p.Time() != nil {
		t.Fatalf("a Linear provider has time tracking: %T", p.Time())
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.svc.Provider(cancelled, jira.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Provider on an ended context = %v", err)
	}
}

// Asking for the key files' state, and preparing a key file, drop the
// credential a client holds, so a replaced token is used on the next
// call without a restart.
func TestAReplacedTokenIsReadAfterTheStatusIsAsked(t *testing.T) {
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
	identity := func() {
		t.Helper()
		if _, err := p.Identity(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	f.writeToken(conn, token("a"))
	identity()
	f.writeToken(conn, token("b"))
	identity()
	if _, err := f.svc.Connections(); err != nil {
		t.Fatal(err)
	}
	identity()
	f.writeToken(conn, token("c"))
	if _, err := f.svc.PrepareKeyFile(conn.ID); err != nil {
		t.Fatal(err)
	}
	identity()
	mu.Lock()
	defer mu.Unlock()
	want := []string{basic(token("a")), basic(token("a")), basic(token("b")), basic(token("c"))}
	if fmt.Sprint(sent) != fmt.Sprint(want) {
		t.Fatalf("the requests carried %q, want %q", sent, want)
	}
}

// Turning a connection off closes its client under a call in flight; the
// call fails as not sent or ended, and nothing races.
func TestClosingAClientUnderACallInFlight(t *testing.T) {
	t.Parallel()
	t.Run("jira", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		conn := f.addJira()
		f.writeToken(conn, jiraToken)
		arrived := make(chan struct{})
		f.jira.On("GET", myselfPath, blocked(arrived))
		closeUnderCall(t, f, conn.ID, arrived, func() error { _, err := f.provider(conn.ID).Identity(t.Context()); return err })
	})
	t.Run("linear", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		conn := f.addLinear()
		f.writeToken(conn, linearKey)
		arrived := make(chan struct{})
		f.linear.Expect(lineartest.Exchange{Op: "Viewer", Wait: true, Arrived: arrived})
		closeUnderCall(t, f, conn.ID, arrived, func() error { _, err := f.provider(conn.ID).Identity(t.Context()); return err })
	})
	t.Run("tempo", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		jira := f.addJira()
		conn := f.addTempo(jira.ID)
		f.writeToken(conn, tempoToken)
		arrived := make(chan struct{})
		release := make(chan struct{})
		f.tempo.On("GET", settings, nil, "", 200, `{}`).During = func() {
			arrived <- struct{}{}
			<-release
		}
		closeUnderCall(t, f, conn.ID, arrived, func() error {
			defer close(release)
			got, err := f.svc.Verify(t.Context(), conn.ID)
			if err == nil && (got.Problem != nil || got.VerifiedAt != 0) {
				return fmt.Errorf("a Verify the disable ended recorded %+v, problem %v", got.TrackerConnection, got.Problem)
			}
			return errors.New("ended")
		})
	})
}

func closeUnderCall(t *testing.T, f *fixture, id string, arrived <-chan struct{}, call func() error) {
	t.Helper()
	errs := make(chan error, 1)
	go func() { errs <- call() }()
	<-arrived
	if _, err := f.svc.SetEnabled(id, false); err != nil {
		t.Fatal(err)
	}
	err := <-errs
	if err == nil {
		t.Fatal("the call in flight succeeded after its client closed")
	}
	// A call Close ends reports the closed client (trackerapi's errClosed,
	// unexported); one Close refuses before sending is a NotSentError.
	_, notSent := errors.AsType[*trackerapi.NotSentError](err)
	if !notSent && !strings.HasSuffix(err.Error(), "client is closed") && err.Error() != "ended" {
		t.Fatalf("the call in flight ended with %v", err)
	}
}

// A provider made while a Jira connection and its Tempo connection were
// both in use holds both clients. When the Jira connection stops being
// used (off, removed, the service stopped), the Tempo client stops too:
// the held provider's worklog read fails without a request. Turned off,
// the Tempo row stays on, and turning Jira on again gives a provider with
// a new Tempo client.
func TestTheTempoClientStopsWithItsJiraConnection(t *testing.T) {
	t.Parallel()
	day, err := tracker.NewDate(2026, 11, 16)
	if err != nil {
		t.Fatal(err)
	}
	query := tracker.WorklogQuery{Author: "acc-user1", From: day, To: day}
	for name, stop := range map[string]func(f *fixture, jiraID string) error{
		"off":     func(f *fixture, id string) error { _, err := f.svc.SetEnabled(id, false); return err },
		"removed": func(f *fixture, id string) error { return f.svc.Remove(id) },
		"stopped": func(f *fixture, _ string) error { f.svc.Stop(); return nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			jira := f.addJira()
			f.writeToken(jira, jiraToken)
			tmp := f.addTempo(jira.ID)
			f.writeToken(tmp, tempoToken)
			f.tempo.On("GET", "/4/worklogs/user/acc-user1",
				url.Values{"from": {"2026-11-16"}, "to": {"2026-11-16"}, "offset": {"0"}, "limit": {"1000"}}, "", 200,
				`{"self":"x","metadata":{"count":0,"offset":0,"limit":1000},"results":[]}`).Repeat = true
			held := f.provider(jira.ID)
			if _, err := held.Time().Worklogs(t.Context(), query); err != nil {
				t.Fatal(err)
			}
			f.svc.mu.Lock()
			first := f.svc.clients[tmp.ID]
			f.svc.mu.Unlock()
			if err := stop(f, jira.ID); err != nil {
				t.Fatal(err)
			}
			if f.inUse(tmp.ID) {
				t.Fatal("the Tempo connection kept its client")
			}
			if _, err := held.Time().Worklogs(t.Context(), query); err == nil {
				t.Fatal("the held provider read Tempo worklogs after its Jira connection stopped")
			}
			if got := len(f.tempo.Requests()); got != 1 {
				t.Fatalf("Tempo saw %d requests, want the one before the stop", got)
			}
			if name != "off" {
				return
			}
			if !f.row(tmp.ID).Enabled {
				t.Fatal("turning Jira off turned its Tempo connection off")
			}
			if _, err := f.svc.SetEnabled(jira.ID, true); err != nil {
				t.Fatal(err)
			}
			if _, err := f.provider(jira.ID).Time().Worklogs(t.Context(), query); err != nil {
				t.Fatalf("a provider after turning Jira on again: %v", err)
			}
			f.svc.mu.Lock()
			second := f.svc.clients[tmp.ID]
			f.svc.mu.Unlock()
			if second == nil || second == first {
				t.Fatal("turning Jira on again did not give Tempo a new client")
			}
		})
	}
}

// A Tempo client looks issues up through its Jira connection as it is at
// the call: not at all while that connection is off.
func TestTempoLooksIssuesUpThroughItsJiraConnection(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	jira := f.addJira()
	issues := parentIssues{s: f.svc, parentID: jira.ID}
	ids, err := issues.IDs(t.Context(), nil)
	if err != nil || len(ids) != 0 || !f.inUse(jira.ID) {
		t.Fatalf("IDs of nothing = %v, %v", ids, err)
	}
	if _, err := f.svc.SetEnabled(jira.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := issues.IDs(t.Context(), []string{"WEB-1"}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("IDs through a Jira connection turned off = %v", err)
	}
	if _, err := issues.Keys(t.Context(), []int64{1}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Keys through a Jira connection turned off = %v", err)
	}
	if f.inUse(jira.ID) || len(f.jira.Requests()) != 0 {
		t.Fatalf("a lookup through a connection turned off reached %v", f.jira.Requests())
	}
	if _, err := (parentIssues{s: f.svc, parentID: "missing"}).IDs(t.Context(), nil); !isNotFound(err) {
		t.Fatalf("IDs through a missing connection = %v", err)
	}
}

// Changes, verifications and provider calls on several connections at
// once leave the registry consistent: run under -race.
func TestConcurrentUse(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	jira := f.addJira()
	f.writeToken(jira, jiraToken)
	f.jira.On("GET", myselfPath, myself(t, jiraToken))
	tmp := f.addTempo(jira.ID)
	f.writeToken(tmp, tempoToken)
	f.tempo.On("GET", settings, nil, "", 200, `{}`).Repeat = true
	f.verify(tmp.ID)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 5 {
				if _, err := f.svc.Verify(t.Context(), jira.ID); err != nil && !errors.Is(err, ErrDisabled) {
					t.Error(err)
				}
				if _, err := f.svc.Provider(t.Context(), jira.ID); err != nil && !errors.Is(err, ErrDisabled) {
					t.Error(err)
				}
				if _, err := f.svc.SetEnabled([]string{jira.ID, tmp.ID}[i%2], i%3 != 0); err != nil {
					t.Error(err)
				}
				if _, err := f.svc.Connections(); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	for _, id := range []string{jira.ID, tmp.ID} {
		if _, err := f.svc.SetEnabled(id, false); err != nil {
			t.Fatal(err)
		}
		if f.inUse(id) {
			t.Fatalf("%s keeps a client after it was turned off", id)
		}
	}
	if rows, err := f.st.ListTrackerConnections(); err != nil || len(rows) != 2 {
		t.Fatalf("rows %+v, %v", rows, err)
	}
}
