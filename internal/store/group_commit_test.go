package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// groupRun is one grouped write's outcome.
type groupRun struct {
	err     error
	applied bool
	panic   any
}

// waitForGroupQueue waits until n writes are queued behind the running
// group.
func waitForGroupQueue(t *testing.T, s *Store, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		s.groups.mu.Lock()
		queued := len(s.groups.queue)
		s.groups.mu.Unlock()
		if queued == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d writes queued, want %d", queued, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// holdGroup runs a grouped write that holds the writer until release is
// called, so the writes that follow queue up and form the next group.
func holdGroup(t *testing.T, s *Store) (release func()) {
	t.Helper()
	started, held := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.groupTx("hold", "hold the writer", func(*sql.Tx) (func(), error) {
			close(started)
			<-held
			return nil, nil
		})
	}()
	<-started
	return func() {
		close(held)
		if err := <-done; err != nil {
			t.Errorf("the holding write: %v", err)
		}
	}
}

// queueGroupWrites runs writes as one group, in order, and returns each
// one's outcome.
func queueGroupWrites(t *testing.T, s *Store, writes ...func(tx *sql.Tx) error) []groupRun {
	t.Helper()
	release := holdGroup(t, s)
	runs := make([]groupRun, len(writes))
	var wg sync.WaitGroup
	for i, write := range writes {
		wg.Go(func() {
			defer func() { runs[i].panic = recover() }()
			runs[i].err = s.groupTx(fmt.Sprintf("t%d", i), fmt.Sprintf("write %d", i), func(tx *sql.Tx) (func(), error) {
				if err := write(tx); err != nil {
					return func() { t.Errorf("write %d failed and applied", i) }, err
				}
				return func() { runs[i].applied = true }, nil
			})
		})
		waitForGroupQueue(t, s, i+1)
	}
	release()
	wg.Wait()
	return runs
}

func groupProbeIDs(t *testing.T, s *Store) string {
	t.Helper()
	rows, err := s.reader().Query(`SELECT id FROM group_probe ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(ids, ",")
}

func insertGroupProbe(id string) func(tx *sql.Tx) error {
	return func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO group_probe (id) VALUES (?)`, id)
		return err
	}
}

func newGroupProbeStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	mustExec(t, s.db, `CREATE TABLE group_probe (id TEXT PRIMARY KEY)`)
	return s
}

func requireGroupApplied(t *testing.T, runs []groupRun, want ...bool) {
	t.Helper()
	for i, run := range runs {
		if run.panic != nil {
			t.Errorf("write %d panicked: %v", i, run.panic)
		}
		if ok := run.err == nil && run.applied; ok != want[i] {
			t.Errorf("write %d: err=%v applied=%v, want applied=%v", i, run.err, run.applied, want[i])
		}
	}
}

// TestGroupCommitQueuedWritesShareOneTransaction pins the grouping: the
// writes that queue behind a group run in the next one, each seeing the
// ones before it, and none is visible to a reader until all commit.
func TestGroupCommitQueuedWritesShareOneTransaction(t *testing.T) {
	s := newGroupProbeStore(t)
	var seen []string
	write := func(id string) func(tx *sql.Tx) error {
		return func(tx *sql.Tx) error {
			var inGroup, committed int
			if err := tx.QueryRow(`SELECT count(*) FROM group_probe`).Scan(&inGroup); err != nil {
				return err
			}
			if err := s.reader().QueryRow(`SELECT count(*) FROM group_probe`).Scan(&committed); err != nil {
				return err
			}
			seen = append(seen, fmt.Sprintf("%s:%d/%d", id, inGroup, committed))
			return insertGroupProbe(id)(tx)
		}
	}
	runs := queueGroupWrites(t, s, write("a"), write("b"), write("c"))
	requireGroupApplied(t, runs, true, true, true)
	if got, want := strings.Join(seen, " "), "a:0/0 b:1/0 c:2/0"; got != want {
		t.Errorf("each write saw %q, want %q", got, want)
	}
	if got := groupProbeIDs(t, s); got != "a,b,c" {
		t.Errorf("committed %q, want a,b,c", got)
	}
}

// TestGroupCommitFailedWriteRollsBackAlone pins the savepoints: a write
// that fails after writing, with its own error or SQLite's, loses what it
// wrote and fails alone.
func TestGroupCommitFailedWriteRollsBackAlone(t *testing.T) {
	s := newGroupProbeStore(t)
	errInjected := errors.New("injected")
	runs := queueGroupWrites(t, s,
		insertGroupProbe("a"),
		func(tx *sql.Tx) error {
			if err := insertGroupProbe("b")(tx); err != nil {
				return err
			}
			return errInjected
		},
		func(tx *sql.Tx) error {
			if err := insertGroupProbe("c")(tx); err != nil {
				return err
			}
			return insertGroupProbe("a")(tx)
		},
		insertGroupProbe("d"),
	)
	requireGroupApplied(t, runs, true, false, false, true)
	if !errors.Is(runs[1].err, errInjected) {
		t.Errorf("write 1: %v, want its own error", runs[1].err)
	}
	if runs[2].err == nil || !strings.Contains(runs[2].err.Error(), "UNIQUE") {
		t.Errorf("write 2: %v, want its constraint error", runs[2].err)
	}
	if got := groupProbeIDs(t, s); got != "a,d" {
		t.Errorf("committed %q, want a,d", got)
	}
}

// TestGroupCommitFailedCommitFailsTheGroup pins a commit failure: every
// write the group ran fails with it and none applies. A write that defers
// foreign keys, which a grouped write must not, is how a commit fails.
func TestGroupCommitFailedCommitFailsTheGroup(t *testing.T) {
	s := newGroupProbeStore(t)
	mustExec(t, s.db, `CREATE TABLE group_probe_child (id TEXT PRIMARY KEY, parent TEXT NOT NULL REFERENCES group_probe(id))`)
	runs := queueGroupWrites(t, s,
		insertGroupProbe("a"),
		func(tx *sql.Tx) error {
			if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT INTO group_probe_child (id, parent) VALUES ('orphan', 'missing')`)
			return err
		},
		insertGroupProbe("c"),
	)
	requireGroupApplied(t, runs, false, false, false)
	for i, run := range runs {
		if run.err == nil || !strings.Contains(run.err.Error(), "store: commit write") || !strings.Contains(run.err.Error(), "FOREIGN KEY") {
			t.Errorf("write %d: %v, want the failed commit", i, run.err)
		}
	}
	if got := groupProbeIDs(t, s); got != "" {
		t.Errorf("committed %q, want nothing", got)
	}
	if err := s.groupTx("t", "after", func(tx *sql.Tx) (func(), error) { return nil, insertGroupProbe("e")(tx) }); err != nil {
		t.Fatalf("a write after the failed commit: %v", err)
	}
	if got := groupProbeIDs(t, s); got != "e" {
		t.Errorf("committed %q, want e", got)
	}
}

// TestGroupCommitWriteThatEndsTheTransaction pins a write that ends the
// group's transaction, as SQLite does itself on some errors: the writes
// before it fail, and the writes after it run in a new transaction.
func TestGroupCommitWriteThatEndsTheTransaction(t *testing.T) {
	errInjected := errors.New("injected")
	for _, tc := range []struct {
		name    string
		failure error
		want    string
	}{
		{"with an error", errInjected, "injected"},
		{"reporting success", nil, "store: release write 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newGroupProbeStore(t)
			runs := queueGroupWrites(t, s,
				insertGroupProbe("a"),
				func(tx *sql.Tx) error {
					if err := insertGroupProbe("b")(tx); err != nil {
						return err
					}
					if _, err := tx.Exec(`ROLLBACK`); err != nil {
						return err
					}
					return tc.failure
				},
				insertGroupProbe("c"),
				insertGroupProbe("d"),
			)
			requireGroupApplied(t, runs, false, false, true, true)
			if runs[0].err == nil || !strings.Contains(runs[0].err.Error(), "rolled back with write 1") {
				t.Errorf("write 0: %v, want its rollback with write 1", runs[0].err)
			}
			if runs[1].err == nil || !strings.Contains(runs[1].err.Error(), tc.want) {
				t.Errorf("write 1: %v, want %q", runs[1].err, tc.want)
			}
			if got := groupProbeIDs(t, s); got != "c,d" {
				t.Errorf("committed %q, want c,d", got)
			}
		})
	}
}

// TestGroupCommitPanicRaisesOnTheWritersGoroutine panics in a follower's
// write, which runs on the leader's goroutine: the panic is raised again
// on the follower's own goroutine with the stack it was raised on, the
// write before it rolls back with it, and the write after it commits.
func TestGroupCommitPanicRaisesOnTheWritersGoroutine(t *testing.T) {
	s := newGroupProbeStore(t)
	runs := queueGroupWrites(t, s,
		insertGroupProbe("a"),
		func(*sql.Tx) error { panic("injected") },
		insertGroupProbe("c"),
	)
	if runs[0].panic != nil || !errors.Is(runs[0].err, errWritePanicked) || runs[0].applied {
		t.Errorf("the leader's write: err=%v applied=%v panic=%v, want rolled back with the panic", runs[0].err, runs[0].applied, runs[0].panic)
	}
	raised, ok := runs[1].panic.(*writePanic)
	if !ok || raised.value != "injected" || !strings.Contains(raised.String(), "group_commit_test.go") {
		t.Errorf("the panicking writer recovered %v, want the injected panic with its stack", runs[1].panic)
	}
	requireGroupApplied(t, runs[2:], true)
	if got := groupProbeIDs(t, s); got != "c" {
		t.Errorf("committed %q, want c", got)
	}
	runs = queueGroupWrites(t, s, insertGroupProbe("d"), insertGroupProbe("e"))
	requireGroupApplied(t, runs, true, true)
	if got := groupProbeIDs(t, s); got != "c,d,e" {
		t.Errorf("committed %q, want c,d,e", got)
	}
}

// TestGroupCommitTakesTheGroupOnceTheWriterIsFree queues writes while the
// leader waits for a writer held by a write with a transaction of its own:
// they commit in the leader's group.
func TestGroupCommitTakesTheGroupOnceTheWriterIsFree(t *testing.T) {
	s := newGroupProbeStore(t)
	own, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := own.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("release the writer: %v", err)
		}
	})
	waits := s.db.Stats().WaitCount
	var committedBefore [3]int
	errs := make(chan error, len(committedBefore))
	for i := range committedBefore {
		go func() {
			errs <- s.groupTx("t", fmt.Sprintf("write %d", i), func(tx *sql.Tx) (func(), error) {
				if err := s.reader().QueryRow(`SELECT count(*) FROM group_probe`).Scan(&committedBefore[i]); err != nil {
					return nil, err
				}
				return nil, insertGroupProbe(fmt.Sprint(i))(tx)
			})
		}()
		if i == 0 {
			for deadline := time.Now().Add(5 * time.Second); s.db.Stats().WaitCount == waits; {
				if time.Now().After(deadline) {
					t.Fatal("the leader never waited for the writer")
				}
				time.Sleep(time.Millisecond)
			}
		}
		waitForGroupQueue(t, s, i+1)
	}
	if err := own.Commit(); err != nil {
		t.Fatal(err)
	}
	for range committedBefore {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if committedBefore != [3]int{} {
		t.Fatalf("writes ran after %v commits, want all in one group", committedBefore)
	}
}

// TestGroupCommitBeginFailureFailsTheGroup closes the writer while writes
// are queued: each fails, and the lead passes on rather than hanging.
func TestGroupCommitBeginFailureFailsTheGroup(t *testing.T) {
	s, err := New(newTestStorePath(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.stopCheckpointer()
		if err := s.read.Close(); err != nil {
			t.Errorf("close read pool: %v", err)
		}
	})
	release := holdGroup(t, s)
	errs := make(chan error, 2)
	for i := range 2 {
		go func() {
			errs <- s.groupTx("t", fmt.Sprintf("write %d", i), func(*sql.Tx) (func(), error) {
				t.Errorf("write %d ran on a closed writer", i)
				return nil, nil
			})
		}()
		waitForGroupQueue(t, s, i+1)
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	release()
	for range 2 {
		if err := <-errs; err == nil || !strings.Contains(err.Error(), "store: begin") {
			t.Errorf("queued write on a closed writer: %v, want a begin error", err)
		}
	}
	if err := s.groupTx("t", "late write", func(*sql.Tx) (func(), error) { return nil, nil }); err == nil {
		t.Error("a write after the close succeeded")
	}
}

// panicBeginConnector opens connections whose first Begin closes entered,
// then panics once begin closes. Later Begins return a transaction with
// nothing to commit.
type panicBeginConnector struct {
	entered, begin chan struct{}
	begun          *atomic.Bool
}

func (c panicBeginConnector) Connect(context.Context) (driver.Conn, error) {
	return panicBeginConn{c}, nil
}

func (panicBeginConnector) Driver() driver.Driver { return nil }

type panicBeginConn struct{ panicBeginConnector }

func (panicBeginConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("panicBeginConn runs no statements")
}

func (panicBeginConn) Close() error { return nil }

func (c panicBeginConn) Begin() (driver.Tx, error) {
	if c.begun.CompareAndSwap(false, true) {
		close(c.entered)
		<-c.begin
		panic("injected begin panic")
	}
	return emptyTx{}, nil
}

type emptyTx struct{}

func (emptyTx) Commit() error   { return nil }
func (emptyTx) Rollback() error { return nil }

// TestGroupCommitBeginPanicFailsTheGroup panics in the leader's Begin
// while writes queue behind it: the panic goes on up the leader's
// goroutine, the queued writes fail, and the lead passes on.
func TestGroupCommitBeginPanicFailsTheGroup(t *testing.T) {
	connector := panicBeginConnector{entered: make(chan struct{}), begin: make(chan struct{}), begun: new(atomic.Bool)}
	db := sql.OpenDB(connector)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	s := &Store{db: db}
	noWrite := func(label string) func(*sql.Tx) (func(), error) {
		return func(*sql.Tx) (func(), error) {
			t.Errorf("%s ran without a transaction", label)
			return nil, nil
		}
	}
	leader := make(chan any, 1)
	go func() {
		defer func() { leader <- recover() }()
		err := s.groupTx("t", "leader", noWrite("the leader"))
		t.Errorf("the leader returned %v, want its Begin's panic", err)
	}()
	<-connector.entered
	errs := make(chan error, 2)
	for i := range 2 {
		label := fmt.Sprintf("write %d", i)
		go func() { errs <- s.groupTx("t", label, noWrite(label)) }()
		waitForGroupQueue(t, s, i+2)
	}
	close(connector.begin)
	select {
	case got := <-leader:
		if got != "injected begin panic" {
			t.Errorf("the leader recovered %v, want its Begin's panic", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the leader did not return")
	}
	for range 2 {
		select {
		case err := <-errs:
			if !errors.Is(err, errWritePanicked) {
				t.Errorf("a write queued behind the panicked Begin: %v, want errWritePanicked", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a write queued behind the panicked Begin hung")
		}
	}
	late := make(chan error, 1)
	go func() {
		late <- s.groupTx("t", "late", func(*sql.Tx) (func(), error) { return nil, nil })
	}()
	select {
	case err := <-late:
		if err != nil {
			t.Fatalf("a write after the panicked Begin: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a write after the panicked Begin hung")
	}
}

// TestGroupCommitCapsAGroup pins groupCommitMax: the writes queued past
// it lead the next group.
func TestGroupCommitCapsAGroup(t *testing.T) {
	s := newGroupProbeStore(t)
	var committedBefore []int
	writes := make([]func(tx *sql.Tx) error, groupCommitMax+2)
	for i := range writes {
		writes[i] = func(tx *sql.Tx) error {
			var committed int
			if err := s.reader().QueryRow(`SELECT count(*) FROM group_probe`).Scan(&committed); err != nil {
				return err
			}
			committedBefore = append(committedBefore, committed)
			return insertGroupProbe(fmt.Sprintf("%03d", i))(tx)
		}
	}
	runs := queueGroupWrites(t, s, writes...)
	for i, run := range runs {
		if run.err != nil || !run.applied {
			t.Fatalf("write %d: %v applied=%v", i, run.err, run.applied)
		}
	}
	for i, committed := range committedBefore {
		want := 0
		if i >= groupCommitMax {
			want = groupCommitMax
		}
		if committed != want {
			t.Errorf("write %d ran after %d commits, want %d", i, committed, want)
		}
	}
}

// TestGroupedItemWritesCannotSplitShownRows pins the guard on the one item
// write path that defers foreign keys.
func TestGroupedItemWritesCannotSplitShownRows(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	err := s.groupWriteItems("t", nil, "split", func(tx *sql.Tx, w *cardWrite) error {
		_, err := splitShownRowsTx(tx, w, "t", forkSplit{})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "splits shown rows in a grouped write") {
		t.Fatalf("a grouped split: %v, want the guard's refusal", err)
	}
}

// TestGroupedItemWritesKeepTheirCards runs live item writes of several
// threads in one group: a card write an agent keeps live, one its
// completed agent flushes, an agent's stop, streaming appends, and writes
// that fail after writing a payload or find no row. The failures change
// nothing, and once the cards close every thread is settled.
func TestGroupedItemWritesKeepTheirCards(t *testing.T) {
	s := newTestStore(t)
	threads := []string{"g-live", "g-done", "g-refused", "g-stop", "g-stream", "g-missing"}
	sessions := make(map[string]*cardSessionForTest, len(threads))
	for _, thread := range threads {
		mustCreateThread(t, s, thread)
		sessions[thread] = newCardSessionForTest(t, s, thread)
	}
	insert := func(thread string, r stampFixtureRow) {
		t.Helper()
		item := r.item(thread)
		item.SubagentCard = sessions[thread].card(r.parent)
		if err := s.InsertItem(item); err != nil {
			t.Fatalf("insert %s/%s: %v", thread, r.id, err)
		}
	}
	for _, thread := range []string{"g-live", "g-refused", "g-stop"} {
		insert(thread, stampFixtureRow{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: live", status: "running", turn: 1})
		insert(thread, stampFixtureRow{id: "L-b1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "L", turn: 1, index: 1})
	}
	insert("g-done", stampFixtureRow{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: done", turn: 1})
	insert("g-done", stampFixtureRow{id: "L-b1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "L", turn: 1, index: 1})
	insert("g-stream", stampFixtureRow{id: "text", kind: "assistant_text", summary: "hello", status: "streaming", turn: 1})
	refusedRev := threadHistoryRevForTest(t, s, "g-refused")

	child := func(thread, id string) Item {
		item := stampFixtureRow{id: id, kind: "tool_call", tool: "Bash", summary: "Bash: " + id, parent: "L", turn: 1, index: 2}.item(thread)
		item.SubagentCard = sessions[thread].card("L")
		return item
	}
	live, done, refused := child("g-live", "L-b2"), child("g-done", "L-b2"), child("g-refused", "L-b2")
	refused.SubagentCard = nil
	refused.PayloadID = "p-refused"
	writes := []struct {
		thread string
		run    func() error
	}{
		{"g-live", func() error { _, err := s.UpsertItem(live, nil); return err }},
		{"g-done", func() error { return s.InsertItem(done) }},
		{"g-refused", func() error {
			_, err := s.UpsertItem(refused, &Payload{ID: "p-refused", Kind: "text", Meta: "{}", Data: []byte("lost"), CreatedAt: 1})
			return err
		}},
		{"g-stop", func() error {
			_, err := s.UpdateItemFields("g-stop", "L", ItemPartialUpdate{Status: new("completed")})
			return err
		}},
		{"g-stream", func() error { _, err := s.AppendItemSummary("g-stream", "text", " world", 5_000); return err }},
		{"g-missing", func() error { _, err := s.AppendItemSummary("g-missing", "absent", "x", 5_000); return err }},
	}

	release := holdGroup(t, s)
	errs := make([]error, len(writes))
	var wg sync.WaitGroup
	for i, write := range writes {
		wg.Go(func() { errs[i] = write.run() })
		waitForGroupQueue(t, s, i+1)
	}
	release()
	wg.Wait()

	for i, err := range errs {
		switch thread := writes[i].thread; thread {
		case "g-refused":
			if !errors.Is(err, ErrSubagentAnchor) {
				t.Errorf("%s: %v, want ErrSubagentAnchor", thread, err)
			}
		case "g-missing":
			if !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("%s: %v, want sql.ErrNoRows", thread, err)
			}
		default:
			if err != nil {
				t.Errorf("%s: %v", thread, err)
			}
		}
	}
	for _, thread := range []string{"g-live", "g-done"} {
		if _, found, err := s.GetThreadItemForWrite(thread, "L-b2"); err != nil || !found {
			t.Errorf("%s did not store its row (found=%v err=%v)", thread, found, err)
		}
	}
	if _, found, err := s.GetThreadItemForWrite("g-refused", "L-b2"); err != nil || found {
		t.Errorf("the refused write stored its row (found=%v err=%v)", found, err)
	}
	if n := countRows(t, s, `SELECT count(*) FROM payloads WHERE thread_id = 'g-refused'`); n != 0 {
		t.Errorf("the refused write stored its payload")
	}
	if rev := threadHistoryRevForTest(t, s, "g-refused"); rev != refusedRev {
		t.Errorf("the refused write moved its thread's history_rev %d -> %d", refusedRev, rev)
	}
	if text, _, err := s.GetThreadItemForWrite("g-stream", "text"); err != nil || text.Summary != "hello world" {
		t.Errorf("the streaming append stored %q (%v)", text.Summary, err)
	}
	if pending := pendingCardsForTest(s, "g-stop"); len(pending) > 0 {
		t.Errorf("the agent's stop left %v pending", pending)
	}

	for _, thread := range threads {
		sessions[thread].closeAll()
		if pending := pendingCardsForTest(s, thread); len(pending) > 0 {
			t.Errorf("%s: closed cards left %v pending", thread, pending)
		}
		assertSubagentStampParity(t, s, thread, thread+" after its grouped write", true)
		assertStampsAreTheRecompute(t, s, thread, thread+" after its grouped write")
	}
}

// TestGroupFailureUndoesAnAgentsStop fails an agent's stop by a later
// write in its group that ends the shared transaction: the stop rolls
// back, and so does what it applied to the thread's cards before the
// commit.
func TestGroupFailureUndoesAnAgentsStop(t *testing.T) {
	s := newTestStore(t)
	const thread = "u-stop"
	mustCreateThread(t, s, thread)
	session := newCardSessionForTest(t, s, thread)
	for _, r := range []stampFixtureRow{
		{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: live", status: "running", turn: 1},
		{id: "L-b1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "L", turn: 1, index: 1},
	} {
		item := r.item(thread)
		item.SubagentCard = session.card(r.parent)
		if err := s.InsertItem(item); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	pendingBefore := pendingCardsForTest(s, thread)
	if len(pendingBefore) == 0 {
		t.Fatal("the live agent holds no pending card")
	}
	stop := func() error {
		_, err := s.UpdateItemFields(thread, "L", ItemPartialUpdate{Status: new("completed")})
		return err
	}

	release := holdGroup(t, s)
	stopErr, endErr := make(chan error, 1), make(chan error, 1)
	go func() { stopErr <- stop() }()
	waitForGroupQueue(t, s, 1)
	go func() {
		endErr <- s.groupTx("other", "end the transaction", func(tx *sql.Tx) (func(), error) {
			_, err := tx.Exec(`ROLLBACK`)
			return nil, err
		})
	}()
	waitForGroupQueue(t, s, 2)
	release()
	if err := <-stopErr; err == nil || !strings.Contains(err.Error(), "rolled back with end the transaction") {
		t.Fatalf("the stop: %v, want its rollback", err)
	}
	if err := <-endErr; err == nil {
		t.Fatal("the write that ended the transaction succeeded")
	}
	if item, _, err := s.GetThreadItemForWrite(thread, "L"); err != nil || item.Status != "running" {
		t.Fatalf("the agent after its stop rolled back: status %q (%v)", item.Status, err)
	}
	if pending := pendingCardsForTest(s, thread); !slices.Equal(pending, pendingBefore) {
		t.Fatalf("cards after the stop rolled back = %v, want %v", pending, pendingBefore)
	}

	if err := stop(); err != nil {
		t.Fatalf("retry the stop: %v", err)
	}
	session.closeAll()
	assertSubagentStampParity(t, s, thread, "after the retried stop", true)
	assertStampsAreTheRecompute(t, s, thread, "after the retried stop")
}

func threadHistoryRevForTest(t *testing.T, s *Store, threadID string) int64 {
	t.Helper()
	var rev int64
	if err := s.reader().QueryRow(`SELECT history_rev FROM threads WHERE id = ?`, threadID).Scan(&rev); err != nil {
		t.Fatalf("read %s history_rev: %v", threadID, err)
	}
	return rev
}
