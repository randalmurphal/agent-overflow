package trackerapp

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// scripted is a tracker.Provider whose reads the test scripts one
// handler per method. A read with no handler fails the test; a method
// sync never calls panics through the nil embedded Provider.
type scripted struct {
	tracker.Provider
	t testing.TB

	mu          sync.Mutex
	calls       []string
	changedRows func(tracker.DeltaQuery) (tracker.RowPage, error)
	boardIssues func(tracker.BoardID, time.Time) (tracker.Listing, error)
	boardRows   func(tracker.BoardID, []tracker.IssueID) (tracker.Rows, error)
	cards       func([]tracker.IssueID) (tracker.Cards, error)
	history     func(tracker.HistoryQuery) (tracker.History, error)
	detail      func(tracker.IssueID) (tracker.Detail, error)
	structure   func(tracker.BoardID, time.Time) (tracker.Structure, error)
	// caps are the tracker's capabilities; none unless a test sets them.
	caps tracker.Capabilities
}

var errUnscripted = errors.New("test: unscripted provider call")

// called records a call of name and reports whether the test scripted
// it; an unscripted call fails the test.
func (p *scripted) called(name string, scripted func() bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, name)
	if !scripted() {
		p.t.Errorf("unexpected provider call %s", name)
		return false
	}
	return true
}

func (p *scripted) set(f func(p *scripted)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
}

// callsInOrder are the calls made, in order.
func (p *scripted) callsInOrder() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// count is how many calls of name were made.
func (p *scripted) count(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if c == name {
			n++
		}
	}
	return n
}

// Capabilities are p.caps; asking makes no call.
func (p *scripted) Capabilities() tracker.Capabilities {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.caps
}

// updatedMovesWithDetail makes p a tracker whose update time moves with
// its detail, as Jira's does, whose card read answers cards.
func (p *scripted) updatedMovesWithDetail(cards func([]tracker.IssueID) (tracker.Cards, error)) {
	p.set(func(p *scripted) {
		p.caps.DetailMovesUpdated = true
		p.cards = cards
	})
}

func (p *scripted) ChangedRows(ctx context.Context, q tracker.DeltaQuery) (tracker.RowPage, error) {
	p.mu.Lock()
	h := p.changedRows
	p.mu.Unlock()
	if !p.called("ChangedRows", func() bool { return h != nil }) {
		var zero tracker.RowPage
		return zero, errUnscripted
	}
	return h(q)
}

func (p *scripted) BoardIssues(ctx context.Context, b tracker.BoardID, window time.Time) (tracker.Listing, error) {
	p.mu.Lock()
	h := p.boardIssues
	p.mu.Unlock()
	if !p.called("BoardIssues", func() bool { return h != nil }) {
		var zero tracker.Listing
		return zero, errUnscripted
	}
	return h(b, window)
}

func (p *scripted) BoardRows(ctx context.Context, b tracker.BoardID, ids []tracker.IssueID) (tracker.Rows, error) {
	p.mu.Lock()
	h := p.boardRows
	p.mu.Unlock()
	if !p.called("BoardRows", func() bool { return h != nil }) {
		var zero tracker.Rows
		return zero, errUnscripted
	}
	return h(b, ids)
}

func (p *scripted) Cards(ctx context.Context, ids []tracker.IssueID) (tracker.Cards, error) {
	p.mu.Lock()
	h := p.cards
	p.mu.Unlock()
	if !p.called("Cards", func() bool { return h != nil }) {
		var zero tracker.Cards
		return zero, errUnscripted
	}
	return h(ids)
}

func (p *scripted) History(ctx context.Context, q tracker.HistoryQuery) (tracker.History, error) {
	p.mu.Lock()
	h := p.history
	p.mu.Unlock()
	if !p.called("History", func() bool { return h != nil }) {
		var zero tracker.History
		return zero, errUnscripted
	}
	return h(q)
}

func (p *scripted) Detail(ctx context.Context, id tracker.IssueID) (tracker.Detail, error) {
	p.mu.Lock()
	h := p.detail
	p.mu.Unlock()
	if !p.called("Detail", func() bool { return h != nil }) {
		var zero tracker.Detail
		return zero, errUnscripted
	}
	return h(id)
}

func (p *scripted) Structure(ctx context.Context, b tracker.BoardID, window time.Time) (tracker.Structure, error) {
	p.mu.Lock()
	h := p.structure
	p.mu.Unlock()
	if !p.called("Structure", func() bool { return h != nil }) {
		var zero tracker.Structure
		return zero, errUnscripted
	}
	return h(b, window)
}

// Ticks of one unit, as most tests drive them.
func (s *Service) tickStructure(ctx context.Context, sc Scope, u Urgency) (Outcome, error) {
	return s.SyncBoard(ctx, sc, BoardDue{Structure: true}, u)
}

func (s *Service) tickReconcile(ctx context.Context, sc Scope, u Urgency) (Outcome, error) {
	return s.SyncBoard(ctx, sc, BoardDue{Reconcile: true}, u)
}

func (s *Service) tickDelta(ctx context.Context, sc Scope, u Urgency) (Outcome, error) {
	return s.SyncBoard(ctx, sc, BoardDue{Delta: true}, u)
}

// tickHistory runs the history unit of a board scope, or of the linked
// scope when sc names no board.
func (s *Service) tickHistory(ctx context.Context, sc Scope, u Urgency) (Outcome, error) {
	if sc.Board == "" {
		return s.SyncLinked(ctx, sc.Connection, LinkedDue{History: true}, u)
	}
	return s.SyncBoard(ctx, sc, BoardDue{History: true}, u)
}

func (s *Service) tickCards(ctx context.Context, id string, u Urgency) (Outcome, error) {
	return s.SyncLinked(ctx, id, LinkedDue{Cards: true}, u)
}

// mirrorFixture is a Service over a cloned store with one Jira
// connection whose provider is scripted.
type mirrorFixture struct {
	*fixture
	conn Connection
	p    *scripted
}

const testBoard tracker.BoardID = "1"

func newMirrorFixture(t *testing.T) *mirrorFixture {
	t.Helper()
	f := newFixture(t)
	p := &scripted{t: t}
	f.svc.makeProvider = func(store.TrackerConnection, *clients, *clients) (tracker.Provider, error) { return p, nil }
	m := &mirrorFixture{fixture: f, conn: f.addJira(), p: p}
	f.listBoards(m.conn.ID, testBoards...)
	p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) { return webStructure(b), nil }
	return m
}

// testBoards are the boards a mirror fixture's connection lists.
var testBoards = []tracker.BoardID{testBoard, "2", "7", "9"}

func (m *mirrorFixture) scope() Scope { return Scope{Connection: m.conn.ID, Board: testBoard} }

// now is the fixture's clock.
func (m *mirrorFixture) now() time.Time { return m.svc.now() }

// root is the connection's mirror root; 0 for none.
func (m *mirrorFixture) root() int64 {
	m.t.Helper()
	root, ok, err := m.st.GetMirrorRoot(m.conn.ID)
	if err != nil {
		m.t.Fatal(err)
	}
	if !ok {
		return 0
	}
	return root
}

// rowIDs are the issues with a row on board, sorted.
func (m *mirrorFixture) rowIDs(board tracker.BoardID) []string {
	m.t.Helper()
	root := m.root()
	if root == 0 {
		return nil
	}
	var ids []string
	for _, id := range allIssues {
		rows, _, err := m.st.ListMirrorIssueRows(root, id, "")
		if err != nil {
			m.t.Fatal(err)
		}
		for _, r := range rows {
			if r.BoardID == string(board) {
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	return ids
}

// allIssues are every issue ID the tests use.
var allIssues = []string{"10001", "10002", "10003", "10004", "10005", "10006", "10007", "10008", "10009"}

// sync is the scope's sync state.
func (m *mirrorFixture) sync(board tracker.BoardID) store.MirrorSync {
	m.t.Helper()
	st, _, err := m.st.GetMirrorSync(m.root(), string(board))
	if err != nil {
		m.t.Fatal(err)
	}
	return st
}

// begun is a run of scope that has read the scope's sync state, as a
// tick's run has before its units; the caller finishes it.
func (m *mirrorFixture) begun(t *testing.T, scope Scope) *run {
	t.Helper()
	r := must[*run](t)(m.svc.start(t.Context(), scope.Connection, Background))
	r.scope = scope
	if err := r.readState(); err != nil {
		r.finish()
		t.Fatal(err)
	}
	return r
}

// problem is the connection's problem kind, "" for none.
func (m *mirrorFixture) problem() FailureKind {
	m.svc.mu.Lock()
	defer m.svc.mu.Unlock()
	return m.svc.problems[m.conn.ID].Kind
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// Test data: board 1 of project WEB with three columns and two sprints.
var (
	statusToDo  = tracker.Status{ID: "s1", Name: "To Do", Group: tracker.GroupToDo}
	statusDoing = tracker.Status{ID: "s2", Name: "In Progress", Group: tracker.GroupInProgress}
	statusDone  = tracker.Status{ID: "s3", Name: "Done", Group: tracker.GroupDone}
	user1       = tracker.User{ID: "acc-user1", Name: "User 1", Email: "user1@example.com", Active: true}
	user2       = tracker.User{ID: "acc-user2", Name: "User 2", Active: true}
)

func webStructure(b tracker.BoardID) tracker.Structure {
	return tracker.Structure{
		Board:    tracker.Board{ID: b, Project: "10000", Name: "WEB board " + string(b), Sprints: true},
		Columns:  []tracker.Column{{Name: "To Do", Statuses: []tracker.StatusID{"s1"}}, {Name: "In Progress", Statuses: []tracker.StatusID{"s2"}}, {Name: "Done", Statuses: []tracker.StatusID{"s3"}}},
		Estimate: tracker.Estimate{Unit: tracker.UnitPoints},
		Sprints: []tracker.Sprint{
			{ID: "1", Board: b, Name: "Sprint 1", State: tracker.SprintActive, Start: time.UnixMilli(1_799_000_000_000).UTC()},
			{ID: "2", Board: b, Name: "Sprint 2", State: tracker.SprintFuture},
		},
		Catalog: tracker.Catalog{Statuses: []tracker.Status{statusToDo, statusDoing, statusDone}, Users: []tracker.User{user1}},
	}
}

// card is issue id updated at ms, in status s1, assigned to User 1.
func card(id string, updated int64) tracker.Card {
	return tracker.Card{
		ID: tracker.IssueID(id), Key: "WEB-" + id[len(id)-1:], Project: "10000", Title: "Issue " + id,
		Status: "s1", Assignee: "acc-user1", Created: time.UnixMilli(1_700_000_000_000).UTC(), Updated: time.UnixMilli(updated).UTC(),
	}
}

func row(id string, updated int64, order string) tracker.Row {
	return tracker.Row{Card: card(id, updated), Order: tracker.OrderKey(order)}
}

// cursorAfter is the scripted provider's cursor after issue id.
func cursorAfter(id string) tracker.Cursor { return tracker.Cursor("after:" + id) }

func ids(s ...string) []tracker.IssueID {
	out := make([]tracker.IssueID, len(s))
	for i, id := range s {
		out[i] = tracker.IssueID(id)
	}
	return out
}
