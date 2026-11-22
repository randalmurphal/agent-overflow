package trackerapp

import (
	"errors"
	"slices"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// heldDelta scripts ChangedRows to tell arrived it is in and answer with
// what release sends.
func heldDelta(arrived chan<- struct{}, release <-chan func() (tracker.RowPage, error)) func(tracker.DeltaQuery) (tracker.RowPage, error) {
	return func(tracker.DeltaQuery) (tracker.RowPage, error) {
		arrived <- struct{}{}
		return (<-release)()
	}
}

func latePage() (tracker.RowPage, error) {
	return tracker.RowPage{Rows: []tracker.Row{row("10001", 1_799_000_000_000, "a")}, Next: cursorAfter("10001")}, nil
}

// inFlight starts a delta that waits inside ChangedRows and returns its
// result channel and the channel that releases it.
func (m *mirrorFixture) inFlight(t *testing.T) (<-chan Outcome, chan<- func() (tracker.RowPage, error)) {
	t.Helper()
	must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	arrived := make(chan struct{})
	release := make(chan func() (tracker.RowPage, error))
	m.p.set(func(p *scripted) { p.changedRows = heldDelta(arrived, release) })
	done := make(chan Outcome)
	go func() {
		out, err := m.svc.tickDelta(t.Context(), m.scope(), Background)
		if err != nil {
			t.Error(err)
		}
		done <- out
	}()
	<-arrived
	return done, release
}

func TestTurnedOffWhileADeltaIsInFlight(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]func() (tracker.RowPage, error){
		// The late answer arrived before the client closed.
		"late rows": latePage,
		// The closed client failed the call.
		"closed client": func() (tracker.RowPage, error) {
			return tracker.RowPage{}, &trackerapi.NotSentError{Err: errors.New("the client is closed")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			done, release := m.inFlight(t)
			if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
				t.Fatal(err)
			}
			if m.root() != 0 {
				t.Fatal("turning the connection off left its mirror")
			}
			release <- answer
			out := <-done
			if !out.Ended || out.Failure != nil || m.problem() != "" {
				t.Fatalf("outcome %+v, problem %q", out, m.problem())
			}
			if out, err := m.svc.tickDelta(t.Context(), m.scope(), Background); err != nil || !out.Ended {
				t.Fatalf("a delta of a connection turned off: %+v, %v", out, err)
			}
		})
	}
}

// Off then on while a delta is in flight: its late rows do not land in
// the mirror the connection has now.
func TestTurnedOffAndOnWhileADeltaIsInFlight(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	done, release := m.inFlight(t)
	old := m.root()
	if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.svc.SetEnabled(m.conn.ID, true); err != nil {
		t.Fatal(err)
	}
	// Another board's tick makes the new mirror; one of the held scope
	// would wait for the held tick.
	must[Outcome](t)(m.svc.tickStructure(t.Context(), Scope{Connection: m.conn.ID, Board: "2"}, Background))
	if m.root() == old || m.root() == 0 {
		t.Fatalf("turning on again kept root %d", old)
	}
	release <- latePage
	if out := <-done; !out.Ended {
		t.Fatalf("late outcome %+v", out)
	}
	if got := m.rowIDs(testBoard); len(got) != 0 {
		t.Fatalf("the late rows landed in the new mirror: %v", got)
	}
}

func TestRemovedWhileADeltaIsInFlight(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	done, release := m.inFlight(t)
	root := m.root()
	if err := m.svc.Remove(m.conn.ID); err != nil {
		t.Fatal(err)
	}
	release <- latePage
	if out := <-done; !out.Ended {
		t.Fatalf("outcome %+v", out)
	}
	if cards := must[[]store.MirrorCard](t)(m.st.ListMirrorCards(root, []string{"10001"})); len(cards) != 0 {
		t.Fatalf("the removed connection's mirror holds %v", cards)
	}
	if out, err := m.svc.tickDelta(t.Context(), m.scope(), Background); err != nil || !out.Ended {
		t.Fatalf("a delta of a removed connection: %+v, %v", out, err)
	}
}

// Turning a connection off clears its mirror and forgets its problem; on
// again, a unit makes a new one.
func TestTurnOffClearsTheMirror(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	root := m.root()
	if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
		t.Fatal(err)
	}
	if cards := must[[]store.MirrorCard](t)(m.st.ListMirrorCards(root, []string{"10001"})); len(cards) != 0 || m.root() != 0 {
		t.Fatalf("turned off, the mirror holds %v", cards)
	}
	if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
		t.Fatalf("turning off twice: %v", err)
	}
	if _, err := m.svc.SetEnabled(m.conn.ID, true); err != nil {
		t.Fatal(err)
	}
	m.p.set(func(p *scripted) { p.changedRows = pagedDelta(t, []tracker.Row{row("10002", 1_799_000_000_000, "a")}) })
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if got := m.rowIDs(testBoard); !slices.Equal(got, []string{"10002"}) {
		t.Fatalf("on again, rows %v", got)
	}
}

// A unit that starts after Stop is refused, as every call is.
func TestUnitsAfterStop(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.svc.Stop()
	if _, err := m.svc.tickDelta(t.Context(), m.scope(), Background); !errors.Is(err, ErrStopped) {
		t.Fatalf("delta after Stop: %v", err)
	}
	if _, err := m.svc.Board(m.scope(), nil); !errors.Is(err, ErrStopped) {
		t.Fatalf("board after Stop: %v", err)
	}
}

// A Tempo connection has no mirror: its units are refused.
func TestUnitsRefuseATempoConnection(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	tmp := m.addTempo(m.conn.ID)
	if _, err := m.svc.tickDelta(t.Context(), Scope{Connection: tmp.ID, Board: testBoard}, Background); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("delta of a Tempo connection: %v", err)
	}
}
