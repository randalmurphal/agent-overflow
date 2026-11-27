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
	"agent-overflow/internal/tracker/trackerapi"
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
	newRows     func(tracker.IssueID, []tracker.BoardID) (tracker.Placement, error)
	cards       func([]tracker.IssueID) (tracker.Cards, error)
	history     func(tracker.HistoryQuery) (tracker.History, error)
	evidence    func(tracker.EvidenceQuery) (tracker.Evidence, error)
	detail      func(tracker.IssueID) (tracker.Detail, error)
	structure   func(tracker.BoardID, time.Time) (tracker.Structure, error)
	fields      func() ([]tracker.Field, error)
	// detailReads are the issues of each Details call.
	detailReads [][]tracker.IssueID
	// caps are the tracker's capabilities; none unless a test sets them.
	caps tracker.Capabilities
	// note is closed and replaced on every call (waitCall).
	note chan struct{}
	// interactive holds, for each call of calls, whether a person waited
	// on it.
	interactive []bool
	// block, when set, runs before each scripted call and fails it with
	// its error: a test holds a call in flight with it.
	block func(ctx context.Context, name string) error
}

// wait runs p's block, if any, for a call of name.
func (p *scripted) wait(ctx context.Context, name string) error {
	p.mu.Lock()
	block := p.block
	p.mu.Unlock()
	if block == nil {
		return nil
	}
	return block(ctx, name)
}

// interactiveCalls is, for each call made, whether it was interactive.
func (p *scripted) interactiveCalls() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.interactive)
}

// waitCall waits for p's next call, or a second, whichever comes first;
// the caller re-checks what it waits for.
func (p *scripted) waitCall() {
	p.mu.Lock()
	if p.note == nil {
		p.note = make(chan struct{})
	}
	ch := p.note
	p.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(time.Second):
	}
}

var errUnscripted = errors.New("test: unscripted provider call")

// called records a call of name and reports whether the test scripted
// it; an unscripted call fails the test.
func (p *scripted) called(ctx context.Context, name string, scripted func() bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, name)
	p.interactive = append(p.interactive, trackerapi.IsInteractive(ctx))
	if p.note != nil {
		close(p.note)
		p.note = nil
	}
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
	if !p.called(ctx, "ChangedRows", func() bool { return h != nil }) {
		var zero tracker.RowPage
		return zero, errUnscripted
	}
	if err := p.wait(ctx, "ChangedRows"); err != nil {
		var zero tracker.RowPage
		return zero, err
	}
	return h(q)
}

func (p *scripted) BoardIssues(ctx context.Context, b tracker.BoardID, window time.Time) (tracker.Listing, error) {
	p.mu.Lock()
	h := p.boardIssues
	p.mu.Unlock()
	if !p.called(ctx, "BoardIssues", func() bool { return h != nil }) {
		var zero tracker.Listing
		return zero, errUnscripted
	}
	if err := p.wait(ctx, "BoardIssues"); err != nil {
		var zero tracker.Listing
		return zero, err
	}
	return h(b, window)
}

func (p *scripted) BoardRows(ctx context.Context, b tracker.BoardID, ids []tracker.IssueID, _ tracker.Eligibility) (tracker.Rows, error) {
	p.mu.Lock()
	h := p.boardRows
	p.mu.Unlock()
	if !p.called(ctx, "BoardRows", func() bool { return h != nil }) {
		var zero tracker.Rows
		return zero, errUnscripted
	}
	if err := p.wait(ctx, "BoardRows"); err != nil {
		var zero tracker.Rows
		return zero, err
	}
	return h(b, ids)
}

func (p *scripted) NewRows(ctx context.Context, issue tracker.IssueID, boards []tracker.BoardID) (tracker.Placement, error) {
	p.mu.Lock()
	h := p.newRows
	p.mu.Unlock()
	if !p.called(ctx, "NewRows", func() bool { return h != nil }) {
		return tracker.Placement{}, errUnscripted
	}
	if err := p.wait(ctx, "NewRows"); err != nil {
		return tracker.Placement{}, err
	}
	if err := tracker.CheckNewRows(issue, boards); err != nil {
		return tracker.Placement{}, err
	}
	return h(issue, boards)
}

func (p *scripted) Cards(ctx context.Context, ids []tracker.IssueID) (tracker.Cards, error) {
	p.mu.Lock()
	h := p.cards
	p.mu.Unlock()
	if !p.called(ctx, "Cards", func() bool { return h != nil }) {
		var zero tracker.Cards
		return zero, errUnscripted
	}
	if err := p.wait(ctx, "Cards"); err != nil {
		var zero tracker.Cards
		return zero, err
	}
	return h(ids)
}

func (p *scripted) History(ctx context.Context, q tracker.HistoryQuery) (tracker.History, error) {
	p.mu.Lock()
	h := p.history
	p.mu.Unlock()
	if !p.called(ctx, "History", func() bool { return h != nil }) {
		var zero tracker.History
		return zero, errUnscripted
	}
	if err := p.wait(ctx, "History"); err != nil {
		var zero tracker.History
		return zero, err
	}
	return h(q)
}

func (p *scripted) Evidence(ctx context.Context, q tracker.EvidenceQuery) (tracker.Evidence, error) {
	p.mu.Lock()
	h := p.evidence
	p.mu.Unlock()
	if !p.called(ctx, "Evidence", func() bool { return h != nil }) {
		return tracker.Evidence{}, errUnscripted
	}
	if err := p.wait(ctx, "Evidence"); err != nil {
		return tracker.Evidence{}, err
	}
	return h(q)
}

// Details reads each issue through p.detail when called, one "Details"
// call for the set, and answers after p's block; an issue whose read fails
// wrapping trackerapi.ErrNotFound is Missing, and any other failure fails
// the call.
func (p *scripted) Details(ctx context.Context, issues []tracker.IssueID) (tracker.Details, error) {
	p.mu.Lock()
	h := p.detail
	p.detailReads = append(p.detailReads, slices.Clone(issues))
	p.mu.Unlock()
	if !p.called(ctx, "Details", func() bool { return h != nil }) {
		return tracker.Details{}, errUnscripted
	}
	// The tracker answers as the issues are when the call is made; a block
	// delays the answer.
	var out tracker.Details
	var failed error
	for _, id := range issues {
		d, err := h(id)
		switch {
		case errors.Is(err, trackerapi.ErrNotFound):
			out.Missing = append(out.Missing, id)
		case err != nil:
			failed = err
		default:
			out.Details = append(out.Details, d)
		}
	}
	if err := p.wait(ctx, "Details"); err != nil {
		return tracker.Details{}, err
	}
	if failed != nil {
		return tracker.Details{}, failed
	}
	return out, nil
}

// detailsRead are the issues of each Details call, in order.
func (p *scripted) detailsRead() [][]tracker.IssueID {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.detailReads)
}

func (p *scripted) Structure(ctx context.Context, b tracker.BoardID, window time.Time) (tracker.Structure, error) {
	p.mu.Lock()
	h := p.structure
	p.mu.Unlock()
	if !p.called(ctx, "Structure", func() bool { return h != nil }) {
		var zero tracker.Structure
		return zero, errUnscripted
	}
	if err := p.wait(ctx, "Structure"); err != nil {
		var zero tracker.Structure
		return zero, err
	}
	return h(b, window)
}

func (p *scripted) Fields(ctx context.Context) ([]tracker.Field, error) {
	p.mu.Lock()
	h := p.fields
	p.mu.Unlock()
	if !p.called(ctx, "Fields", func() bool { return h != nil }) {
		return nil, errUnscripted
	}
	if err := p.wait(ctx, "Fields"); err != nil {
		return nil, err
	}
	return h()
}

// syncTick runs one tick of scope's board as its pump does (syncBoard),
// the units due names, over a pump of its own: a reconcile pass it leaves
// open lasts this tick. A test that goes on with a pass keeps its pump
// (passTick).
func (s *Service) syncTick(ctx context.Context, scope Scope, due BoardDue, u Urgency) (Outcome, error) {
	out, _, err := s.syncBoard(ctx, &boardPump{scope: scope}, u, func(*run) BoardDue { return due })
	return out, err
}

// Ticks of one unit, as most tests drive them.
func (s *Service) tickStructure(ctx context.Context, sc Scope, u Urgency) (Outcome, error) {
	return s.syncTick(ctx, sc, BoardDue{Structure: true}, u)
}

func (s *Service) tickReconcile(ctx context.Context, sc Scope, u Urgency) (Outcome, error) {
	return s.syncTick(ctx, sc, BoardDue{Reconcile: true}, u)
}

func (s *Service) tickDelta(ctx context.Context, sc Scope, u Urgency) (Outcome, error) {
	return s.syncTick(ctx, sc, BoardDue{Delta: true}, u)
}

// tickHistory runs the history unit of a board scope, or of the linked
// scope when sc names no board.
func (s *Service) tickHistory(ctx context.Context, sc Scope, u Urgency) (Outcome, error) {
	if sc.Board == "" {
		return s.SyncLinked(ctx, sc.Connection, LinkedDue{History: true}, u)
	}
	return s.syncTick(ctx, sc, BoardDue{History: true}, u)
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
	// version counts the tracker's states a test set (tracks).
	version int
}

const testBoard tracker.BoardID = "1"

func newMirrorFixture(t *testing.T) *mirrorFixture {
	t.Helper()
	return newMirrorFixtureOn(t, newFixture(t))
}

// newMirrorFixtureOn is a mirror fixture over f.
func newMirrorFixtureOn(t *testing.T, f *fixture) *mirrorFixture {
	t.Helper()
	p := &scripted{t: t}
	f.svc.makeProvider = func(store.TrackerConnection, *clients, *clients) (tracker.Provider, error) { return p, nil }
	m := &mirrorFixture{fixture: f, conn: f.addJira(), p: p}
	f.listBoards(m.conn.ID, testBoards...)
	p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) { return webStructure(b), nil }
	// A ticket round whose ticket shows no time in its status reads the
	// due histories (roundHistory); by default the tracker holds none.
	p.history = func(tracker.HistoryQuery) (tracker.History, error) { return tracker.History{}, nil }
	return m
}

// refuseKeeps makes the store refuse every keep of an open ticket from
// now on (keepRefused).
func (m *mirrorFixture) refuseKeeps() {
	m.svc.store = keepRefused{m.st}
}

// keepRefused is a store whose bind of an open ticket fails, as a failing
// write does: the ticket shows a failure outside the tracker, which its
// next read retries (bindTickets).
type keepRefused struct{ *store.Store }

func (keepRefused) KeepMirrorTicket(int64, string, int64, int64, store.MirrorRead) (store.MirrorBind, bool, error) {
	return store.MirrorBind{}, false, errors.New("test: the store refused the keep")
}

// keepsMade records the issues of the open tickets the store kept, and
// refuses the first keep of the open ticket of issue refuse.
type keepsMade struct {
	*store.Store
	refuse  string
	mu      sync.Mutex
	refused bool
	issues  []string
}

func (k *keepsMade) KeepMirrorTicket(root int64, issue string, session, pump int64, opened store.MirrorRead) (store.MirrorBind, bool, error) {
	k.mu.Lock()
	refuse := issue == k.refuse && !k.refused
	k.refused = k.refused || refuse
	k.mu.Unlock()
	if refuse {
		return store.MirrorBind{}, false, errors.New("test: the store refused the keep")
	}
	bind, shown, err := k.Store.KeepMirrorTicket(root, issue, session, pump, opened)
	if err != nil {
		return store.MirrorBind{}, false, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.issues = append(k.issues, issue)
	return bind, shown, nil
}

// since is the issues kept since the last call, sorted.
func (k *keepsMade) since() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	issues := slices.Sorted(slices.Values(k.issues))
	k.issues = nil
	return issues
}

// loseKeeps makes the store keep no issue for an open ticket from now on
// (keepsLost).
func (m *mirrorFixture) loseKeeps() {
	m.svc.store = keepsLost{m.st}
}

// keepsLost is a store whose keep of an open ticket succeeds and leaves no
// row, and whose bind never becomes the issue's current one though its
// activation reports it did: the ticket's reads store no body, and its
// pump holds the content it shows.
type keepsLost struct{ *store.Store }

func (k keepsLost) KeepMirrorTicket(root int64, issue string, session, pump int64, opened store.MirrorRead) (store.MirrorBind, bool, error) {
	bind, _, err := k.Store.KeepMirrorTicket(root, issue, session, pump, opened)
	if err != nil {
		return store.MirrorBind{}, false, err
	}
	if _, err := k.DropMirrorTicket(session, pump); err != nil {
		return store.MirrorBind{}, false, err
	}
	return bind, false, nil
}

func (keepsLost) ActivateMirrorBind(store.MirrorBind) bool { return true }

// testBoards are the boards a mirror fixture's connection lists.
var testBoards = []tracker.BoardID{testBoard, "2", "7", "9"}

func (m *mirrorFixture) scope() Scope { return Scope{Connection: m.conn.ID, Board: testBoard} }

// now is the fixture's clock.
func (m *mirrorFixture) now() time.Time { return m.svc.now() }

// read begins a read of the connection's root now, as a run does.
func (m *mirrorFixture) read() store.MirrorRead {
	m.t.Helper()
	return beginReadAt(m.t, m.st, m.root(), millis(m.now()))
}

// beginReadAt begins a read of root at now (Unix ms), as a run does
// (store BeginMirrorRead), for a test's own write of the mirror.
func beginReadAt(t testing.TB, st *store.Store, root, now int64) store.MirrorRead {
	t.Helper()
	read, err := st.BeginMirrorRead(root, now)
	if err != nil {
		t.Fatalf("begin a read of root %d: %v", root, err)
	}
	return read
}

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
	st, _, err := m.st.GetMirrorSync(m.root(), string(board), m.svc.steady().UnixMilli())
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
