package trackerapp

import (
	"errors"
	"net/http"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

// settler drives settle directly on one Jira connection in use, as
// overlapping calls on it would.
type settler struct {
	f  *fixture
	id string
	c  *clients
}

func newSettler(t *testing.T) settler {
	f := newFixture(t)
	conn := f.addJira()
	f.provider(conn.ID)
	f.svc.mu.Lock()
	c := f.svc.clients[conn.ID]
	f.svc.mu.Unlock()
	return settler{f: f, id: conn.ID, c: c}
}

func (s settler) begin() stamp { return s.f.svc.begin(s.c) }

// settle applies callErr as the outcome of the call begun at at, and
// reports whether it applied.
func (s settler) settle(at stamp, callErr error) bool {
	s.f.svc.opMu.Lock()
	defer s.f.svc.opMu.Unlock()
	applied, _, err := s.f.svc.settle(s.id, store.TrackerKindJira, at, callErr, nil)
	if err != nil {
		s.f.t.Fatal(err)
	}
	return applied
}

// problem is the connection's problem kind, "" for none.
func (s settler) problem() FailureKind {
	s.f.svc.mu.Lock()
	defer s.f.svc.mu.Unlock()
	return s.f.svc.problems[s.id].Kind
}

var forbidden = &trackerapi.StatusError{Tracker: "jira", Status: 403}

// Three overlapping calls begun 1, 2, 3 and finishing 2, 1, 3: 2 applies,
// 1 is older than an applied outcome and is dropped, 3 applies.
func TestSettleAppliesOverlappingCallsInTurn(t *testing.T) {
	t.Parallel()
	s := newSettler(t)
	one, two, three := s.begin(), s.begin(), s.begin()
	if !s.settle(two, forbidden) || s.problem() != FailureForbidden {
		t.Fatalf("call 2 did not apply: problem %q", s.problem())
	}
	if s.settle(one, nil) || s.problem() != FailureForbidden {
		t.Fatalf("call 1, older than the applied call 2, applied: problem %q", s.problem())
	}
	if !s.settle(three, nil) || s.problem() != "" {
		t.Fatalf("call 3 did not apply: problem %q", s.problem())
	}
}

// A newer call that never settles does not hold back the older one's
// outcome.
func TestSettleAppliesAnOlderCallWhenTheNewerNeverSettles(t *testing.T) {
	t.Parallel()
	s := newSettler(t)
	older := s.begin()
	s.begin() // never settles
	if !s.settle(older, forbidden) || s.problem() != FailureForbidden {
		t.Fatalf("the older call did not apply: problem %q", s.problem())
	}
}

// One stamp's outcome applies once.
func TestSettleRefusesAStampTwice(t *testing.T) {
	t.Parallel()
	s := newSettler(t)
	at := s.begin()
	if !s.settle(at, forbidden) {
		t.Fatal("the first settlement did not apply")
	}
	if s.settle(at, nil) || s.problem() != FailureForbidden {
		t.Fatalf("the stamp settled twice: problem %q", s.problem())
	}
}

// The outcome of a call whose client the connection no longer uses
// changes nothing.
func TestSettleAfterTheClientStoppedChangesNothing(t *testing.T) {
	t.Parallel()
	s := newSettler(t)
	at := s.begin()
	if _, err := s.f.svc.SetEnabled(s.id, false); err != nil {
		t.Fatal(err)
	}
	if s.settle(at, errors.New("late")) || s.problem() != "" {
		t.Fatalf("a call whose client closed applied: problem %q", s.problem())
	}
}

// stopAtPublish makes Stop overtake the next settlement between its
// record and its publication, the gap where Stop takes no lock the
// settlement holds. Stop's wait for calls in flight runs at cleanup.
func stopAtPublish(f *fixture) {
	f.svc.beforePublish = func() {
		for _, c := range f.svc.halt() {
			c.close()
		}
	}
}

// A Verify whose client cannot be built, overtaken by Stop before its
// setup problem is published, publishes nothing and fails with
// ErrStopped.
func TestStopOvertakingANoClientSettlement(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.origin = func(string, string) (string, error) { return "", errors.New("no fake tracker") }
	conn := f.addJira()
	stopAtPublish(f)
	before := f.changes.Load()
	if _, err := f.svc.Verify(t.Context(), conn.ID); !errors.Is(err, ErrStopped) {
		t.Fatalf("Verify overtaken by Stop = %v, want ErrStopped", err)
	}
	f.svc.mu.Lock()
	problems := len(f.svc.problems)
	f.svc.mu.Unlock()
	if problems != 0 || f.changes.Load() != before {
		t.Fatalf("a settlement Stop overtook published %d problems and %d changes", problems, f.changes.Load()-before)
	}
}

// A Verify overtaken by Stop between its record and its publication
// publishes nothing and fails with ErrStopped, whether it failed or
// succeeded. A verification row it wrote before Stop was noticed stays,
// as it is true.
func TestStopOvertakingAVerificationAfterItsRecord(t *testing.T) {
	t.Parallel()
	for name, status := range map[string]int{"failed": http.StatusForbidden, "succeeded": http.StatusOK} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			conn := f.addJira()
			f.writeToken(conn, jiraToken)
			f.jira.On("GET", myselfPath, jiratest.Reply(status, myselfJSON))
			stopAtPublish(f)
			before := f.changes.Load()
			if _, err := f.svc.Verify(t.Context(), conn.ID); !errors.Is(err, ErrStopped) {
				t.Fatalf("Verify overtaken by Stop = %v, want ErrStopped", err)
			}
			f.svc.mu.Lock()
			problems := len(f.svc.problems)
			f.svc.mu.Unlock()
			if problems != 0 || f.changes.Load() != before {
				t.Fatalf("a settlement Stop overtook published %d problems and %d changes", problems, f.changes.Load()-before)
			}
			if verified := f.row(conn.ID).VerifiedAt != 0; verified != (status == http.StatusOK) {
				t.Fatalf("the row's verification is %d after a Verify that %s", f.row(conn.ID).VerifiedAt, name)
			}
		})
	}
}
