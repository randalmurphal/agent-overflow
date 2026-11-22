package trackerapp

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// pagedDelta scripts ChangedRows as a walk over pages: a query with no
// cursor gets pages[0], one with cursorAfter(last row of page i) gets
// pages[i+1]. Each page's Next is the cursor after its last row.
func pagedDelta(t testing.TB, pages ...[]tracker.Row) func(tracker.DeltaQuery) (tracker.RowPage, error) {
	return func(q tracker.DeltaQuery) (tracker.RowPage, error) {
		at := 0
		if q.From != nil {
			at = -1
			for i, p := range pages {
				if bytes.Equal(q.From, cursorAfter(string(p[len(p)-1].Card.ID))) {
					at = i + 1
				}
			}
			if at < 0 {
				t.Errorf("delta from an unknown cursor %q", q.From)
				return tracker.RowPage{}, errUnscripted
			}
		}
		if at == len(pages) {
			return tracker.RowPage{Next: q.From}, nil
		}
		p := pages[at]
		return tracker.RowPage{Rows: p, Next: cursorAfter(string(p[len(p)-1].Card.ID)), More: at+1 < len(pages),
			Catalog: tracker.Catalog{Users: []tracker.User{user1}}}, nil
	}
}

func TestDeltaStoresEachPageWithItsCursor(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	var starts []time.Time
	walk := pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")}, []tracker.Row{row("10002", 1_799_000_000_001, "b")})
	m.p.changedRows = func(q tracker.DeltaQuery) (tracker.RowPage, error) {
		starts = append(starts, q.Start)
		return walk(q)
	}
	out := must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if out.Failure != nil || out.Ended || out.More() || !out.Changes().Structure ||
		!slices.Equal(out.Changes().Cards, ids("10001", "10002")) || !slices.Equal(out.Changes().Rows, ids("10001", "10002")) {
		t.Fatalf("first delta %+v", out)
	}
	if want := m.now().Add(-HistoryWindow); !starts[0].Equal(want) {
		t.Fatalf("a walk with no cursor starts at %v, want the window's start %v", starts[0], want)
	}
	st := m.sync(testBoard)
	if !bytes.Equal(st.Cursor, cursorAfter("10002")) || st.ReadAt != m.now().UnixMilli() || st.Failure.Kind != "" {
		t.Fatalf("sync state %+v", st)
	}
	// Called again it resumes from the stored cursor and changes nothing.
	again := must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if !again.Changes().Empty() || m.p.count("Structure") != 1 {
		t.Fatalf("second delta %+v, %d structure reads", again, m.p.count("Structure"))
	}
}

// A restart between two pages of issues sharing one update time resumes
// at the cursor the first page stored: nothing is read twice or lost.
func TestDeltaResumesAfterTheLastStoredPage(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	const same = 1_799_000_000_000
	first := []tracker.Row{row("10001", same, "a"), row("10002", same, "b")}
	second := []tracker.Row{row("10003", same, "c")}
	walk := pagedDelta(t, first, second)
	failSecond := true
	var froms []tracker.Cursor
	m.p.changedRows = func(q tracker.DeltaQuery) (tracker.RowPage, error) {
		froms = append(froms, q.From)
		if q.From != nil && failSecond {
			return tracker.RowPage{}, &trackerapi.TransientError{Err: errors.New("connection reset")}
		}
		return walk(q)
	}
	out := must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if out.Failure == nil || out.Failure.Kind != FailureTransient || !slices.Equal(out.Changes().Cards, ids("10001", "10002")) {
		t.Fatalf("interrupted delta %+v", out)
	}
	if st := m.sync(testBoard); !bytes.Equal(st.Cursor, cursorAfter("10002")) || st.Failure.Kind != store.MirrorFailureTransient {
		t.Fatalf("after the failure the sync state is %+v", st)
	}
	failSecond = false
	out = must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if out.Failure != nil || !slices.Equal(out.Changes().Cards, ids("10003")) {
		t.Fatalf("resumed delta %+v", out)
	}
	if !bytes.Equal(froms[2], cursorAfter("10002")) {
		t.Fatalf("the resumed walk started from %q", froms[2])
	}
	if got := m.rowIDs(testBoard); !slices.Equal(got, []string{"10001", "10002", "10003"}) {
		t.Fatalf("rows %v", got)
	}
	if st := m.sync(testBoard); st.Failure.Kind != "" {
		t.Fatalf("a success left the failure %+v", st.Failure)
	}
}

func TestDeltaStopsAtItsPageBound(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	n := 0
	m.p.changedRows = func(q tracker.DeltaQuery) (tracker.RowPage, error) {
		n++
		return tracker.RowPage{Next: tracker.Cursor{byte(n)}, More: true}, nil
	}
	out := must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if !out.More() || n != deltaPages || !bytes.Equal(m.sync(testBoard).Cursor, []byte{byte(deltaPages)}) {
		t.Fatalf("bounded delta %+v after %d pages, cursor %v", out, n, m.sync(testBoard).Cursor)
	}
}

// A stored cursor the provider refuses starts the walk over at the
// window, once.
func TestDeltaStartsOverWhenTheCursorIsRefused(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	var froms []tracker.Cursor
	m.p.set(func(p *scripted) {
		p.changedRows = func(q tracker.DeltaQuery) (tracker.RowPage, error) {
			froms = append(froms, q.From)
			if q.From != nil {
				return tracker.RowPage{}, &trackerapi.NotSentError{Err: tracker.ErrInvalidInput}
			}
			return tracker.RowPage{Rows: []tracker.Row{row("10002", 1_799_000_000_001, "b")}, Next: cursorAfter("10002")}, nil
		}
	})
	out := must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if out.Failure != nil || len(froms) != 2 || froms[1] != nil || !slices.Equal(out.Changes().Cards, ids("10002")) {
		t.Fatalf("delta over a refused cursor %+v, froms %q", out, froms)
	}
}

// A stored row naming a status the mirror lacks reads the structure after
// the page, even in the tick whose own first structure read came before
// the page: that read could not mark the status unexplained. A row read
// again unchanged does not cascade; a changed row naming a new sprint or
// a new user does.
func TestDeltaCascadesToTheStructure(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	r := row("10001", 1_799_000_000_000, "a")
	r.Card.Status = "s9"
	m.p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
		return tracker.RowPage{Rows: []tracker.Row{r}, Next: cursorAfter("10001")}, nil
	}
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if got := m.p.count("Structure"); got != 2 {
		t.Fatalf("%d structure reads, want the first and one after the page", got)
	}
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if got := m.p.count("Structure"); got != 2 {
		t.Fatalf("an unchanged row cascaded: %d structure reads", got)
	}
	r.Card.Updated = r.Card.Updated.Add(time.Second)
	r.Card.Status, r.Card.Assignee, r.Card.Sprints = "s1", "acc-user1", []tracker.SprintID{"9"}
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if got := m.p.count("Structure"); got != 3 {
		t.Fatalf("a new sprint did not cascade: %d structure reads", got)
	}
	r.Card.Updated = r.Card.Updated.Add(time.Second)
	r.Card.Sprints, r.Card.Assignee = []tracker.SprintID{"1"}, "acc-user9"
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if got := m.p.count("Structure"); got != 4 {
		t.Fatalf("a new user did not cascade: %d structure reads", got)
	}
}

// Each failure kind is the outcome's, the scope's and the connection's
// problem, and a later success clears the last two.
func TestDeltaFailureKinds(t *testing.T) {
	t.Parallel()
	until := time.UnixMilli(1_800_000_060_000).UTC()
	for _, tc := range []struct {
		err  error
		kind FailureKind
	}{
		{&trackerapi.TransientError{Err: errors.New("reset")}, FailureTransient},
		{&trackerapi.RateLimitedError{Tracker: "jira", Until: until, Status: http.StatusTooManyRequests}, FailureRateLimited},
		{&trackerapi.AuthError{Tracker: "jira", Status: http.StatusUnauthorized}, FailureSetup},
		{&trackerapi.StatusError{Tracker: "jira", Status: http.StatusForbidden}, FailureForbidden},
		{&trackerapi.StatusError{Tracker: "jira", Status: http.StatusInternalServerError}, FailureTracker},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			m.p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) { return tracker.RowPage{}, tc.err }
			out := must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
			if out.Failure == nil || out.Failure.Kind != tc.kind || out.Ended {
				t.Fatalf("outcome %+v", out)
			}
			if wantRetry := tc.kind == FailureRateLimited; out.RetryAt.Equal(until) != wantRetry {
				t.Fatalf("retry at %v", out.RetryAt)
			}
			if st := m.sync(testBoard); st.Failure.Kind != string(tc.kind) || st.Failure.Message != out.Failure.Message || st.ReadAt != 0 {
				t.Fatalf("sync state %+v", st)
			}
			if m.problem() != tc.kind {
				t.Fatalf("connection problem %q", m.problem())
			}
			changes := m.changes.Load()
			m.p.set(func(p *scripted) { p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")}) })
			if out := must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background)); out.Failure != nil {
				t.Fatalf("recovery %+v", out)
			}
			if st := m.sync(testBoard); st.Failure.Kind != "" || m.problem() != "" || m.changes.Load() == changes {
				t.Fatalf("a success left the failure %+v, problem %q, or told nobody", st.Failure, m.problem())
			}
		})
	}
}

// A request marked interactive reaches the provider marked; a background
// one does not.
func TestUnitsMarkInteractiveCalls(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	var marked []bool
	m.svc.makeProvider = func(store.TrackerConnection, *clients, *clients) (tracker.Provider, error) {
		return ctxProbe{scripted: m.p, marked: &marked}, nil
	}
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Interactive))
	if !slices.Equal(marked, []bool{false, true}) {
		t.Fatalf("interactive marks %v", marked)
	}
}

// ctxProbe records whether each ChangedRows context is interactive.
type ctxProbe struct {
	*scripted
	marked *[]bool
}

func (p ctxProbe) ChangedRows(ctx context.Context, q tracker.DeltaQuery) (tracker.RowPage, error) {
	*p.marked = append(*p.marked, trackerapi.IsInteractive(ctx))
	return p.scripted.ChangedRows(ctx, q)
}

// A unit whose context ends while it waits on the tracker stops with the
// context's error and records nothing.
func TestDeltaCancelledMidway(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	arrived := make(chan struct{})
	m.p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
		close(arrived)
		return tracker.RowPage{}, &trackerapi.NotSentError{Err: context.Canceled}
	}
	ctx, cancel := context.WithCancel(t.Context())
	m.svc.makeProvider = func(store.TrackerConnection, *clients, *clients) (tracker.Provider, error) {
		return cancelling{scripted: m.p, cancel: cancel}, nil
	}
	out, err := m.svc.tickDelta(ctx, m.scope(), Background)
	<-arrived
	if !errors.Is(err, context.Canceled) || out.Failure != nil {
		t.Fatalf("cancelled delta %+v, %v", out, err)
	}
	if st := m.sync(testBoard); st.Failure.Kind != "" || m.problem() != "" {
		t.Fatalf("a cancelled unit recorded %+v, problem %q", st.Failure, m.problem())
	}
}

// cancelling ends the unit's context as its first read arrives.
type cancelling struct {
	*scripted
	cancel context.CancelFunc
}

func (p cancelling) ChangedRows(ctx context.Context, q tracker.DeltaQuery) (tracker.RowPage, error) {
	p.cancel()
	return p.scripted.ChangedRows(ctx, q)
}

// Two deltas of one board at once both succeed and leave every row.
func TestDeltaConcurrentWithItself(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	walk := pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")}, []tracker.Row{row("10002", 1_799_000_000_001, "b")})
	m.p.changedRows = walk
	must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() {
			out, err := m.svc.tickDelta(t.Context(), m.scope(), Background)
			if err == nil && out.Failure != nil {
				err = errors.New(out.Failure.Message)
			}
			errs[i] = err
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if got := m.rowIDs(testBoard); !slices.Equal(got, []string{"10001", "10002"}) {
		t.Fatalf("rows %v", got)
	}
}

func TestBoardUnitsRefuseAScopeWithoutABoard(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	linked := Scope{Connection: m.conn.ID}
	for name, unit := range map[string]func(context.Context, Scope, Urgency) (Outcome, error){
		"delta": m.svc.tickDelta, "reconcile": m.svc.tickReconcile, "structure": m.svc.tickStructure,
	} {
		if _, err := unit(t.Context(), linked, Background); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s of the linked scope: %v", name, err)
		}
	}
	if _, err := m.svc.tickHistory(t.Context(), Scope{}, Background); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("history of no connection: %v", err)
	}
}
