package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
)

func TestReadSnapshotContextInterruptsStatement(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, err := readSnapshotContext(ctx, s.reader(), "cancelled history", func(q sqlQueryer) (int, error) {
		var sum int
		err := q.QueryRow(`WITH RECURSIVE n(x) AS (
			VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<1000000
		) SELECT SUM(x) FROM n`).Scan(&sum)
		return sum, err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("statement survived its deadline: %v", err)
	}
	var probe int
	if err := s.reader().QueryRowContext(t.Context(), "SELECT 1").Scan(&probe); err != nil || probe != 1 {
		t.Fatalf("read pool unavailable after cancelled read: probe=%d err=%v", probe, err)
	}
}

// interruptedBeginConnector models a read whose deadline fires while its
// BEGIN runs: the driver interrupts the statement and reports the
// interruption, not the context's error.
type interruptedBeginConnector struct{ driver.Connector }

func (c interruptedBeginConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return interruptedBeginConn{conn}, nil
}

type interruptedBeginConn struct{ driver.Conn }

func (interruptedBeginConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	<-ctx.Done()
	return nil, errors.New("interrupted (9)")
}

func TestReadSnapshotContextReportsDeadlineAtBegin(t *testing.T) {
	s := newTestStore(t)
	base, err := sqlite.NewConnector(poolDSN(s.path, readerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(interruptedBeginConnector{base})
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, err = readSnapshotContext(ctx, db, "interrupted begin", func(sqlQueryer) (int, error) {
		t.Error("the read ran without a snapshot")
		return 0, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("begin interrupted by the deadline reported %v", err)
	}
}

func TestTimelineReadsHonorCallerCancellation(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	reads := []struct {
		name string
		read func() error
	}{
		{"slice", func() error { _, err := s.ListThreadSliceAround(ctx, "t", "", 20, 5, TimelineSelection{}); return err }},
		{"before", func() error {
			_, err := s.ListItemsBeforeCursor(ctx, "t", TimelineCursor{TurnIndex: 0}, 20, 5, TimelineSelection{})
			return err
		}},
		{"after", func() error {
			_, err := s.ListItemsAfterCursor(ctx, "t", TimelineCursor{TurnIndex: 0}, 20, 5, TimelineSelection{})
			return err
		}},
		{"members", func() error {
			_, err := s.ListActivityRunMembers(ctx, "t", ActivityRunMembersRequest{RunFirstItemID: "r", Direction: ActivityRunMembersBefore, Limit: 5})
			return err
		}},
	}
	for _, tc := range reads {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.read(); !errors.Is(err, context.Canceled) {
				t.Fatalf("timeline read ignored caller cancellation: %v", err)
			}
		})
	}
}

func TestReadSnapshotSurvivesConcurrentHistoryWrite(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatal(err)
	}
	seedItem(t, s, "t", "a", 0, 0, "")
	old, err := readSnapshot(s.reader(), "test history", func(q sqlQueryer) (Item, error) {
		first, _, err := s.getThreadItem(q, "t", "a")
		if err != nil {
			return Item{}, err
		}
		if _, err := s.db.Exec(`UPDATE items SET summary = 'new' WHERE thread_id = 't' AND id = 'a'`); err != nil {
			return Item{}, err
		}
		second, _, err := s.getThreadItem(q, "t", "a")
		if err == nil && (second.Rev != first.Rev || second.Summary != first.Summary) {
			t.Error("one read mixed two history snapshots")
		}
		return second, err
	})
	if err != nil {
		t.Fatal(err)
	}
	current, _, err := s.GetThreadItem("t", "a")
	if err != nil {
		t.Fatal(err)
	}
	if current.Rev <= old.Rev || current.Summary != "new" {
		t.Fatal("read snapshot blocked or lost the concurrent write")
	}
}

// holdHistoryReadSlots takes every history read slot until the test ends
// and returns one release per slot. With conns, each slot also holds a
// read-pool connection in a transaction, as a history read in flight does.
func holdHistoryReadSlots(t *testing.T, s *Store, conns bool) []func() {
	t.Helper()
	if s.read == nil {
		t.Fatal("the store has no read pool")
	}
	releases := make([]func(), historyReadSlots)
	for i := range releases {
		release, err := s.historyReads.acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var tx *sql.Tx
		if conns {
			// The holders outlive t.Context, which is cancelled before
			// cleanups run and would roll their transactions back under
			// the cleanup.
			if tx, err = s.read.BeginTx(context.Background(), nil); err != nil {
				release()
				t.Fatal(err)
			}
		}
		releases[i] = sync.OnceFunc(func() {
			if tx != nil {
				if err := tx.Rollback(); err != nil {
					t.Error(err)
				}
			}
			release()
		})
		t.Cleanup(releases[i])
	}
	return releases
}

// requireReadRuns fails the test unless read returns without an error
// within five seconds.
func requireReadRuns(t *testing.T, what string, read func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- read() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s waited for a read connection", what)
	}
}

// A history read waits for a slot before it takes a connection. While
// history reads in flight hold every slot and a connection each, the
// reads waiting for a slot hold none, and a single-statement read still
// runs on the pool's last connection.
func TestHistoryReadsLeaveAConnectionForOtherReads(t *testing.T) {
	s := newTestStore(t)
	seedSyncThread(t, s, "t", 3)
	holdHistoryReadSlots(t, s, true)

	reads := []struct {
		name string
		read func(context.Context) error
	}{
		{"window", func(ctx context.Context) error {
			_, err := s.SyncThreadWindow(ctx, "t", "", 20, 5, HistoryStamp{}, nil, TimelineSelection{})
			return err
		}},
		{"slice", func(ctx context.Context) error {
			_, err := s.ListThreadSliceAround(ctx, "t", "", 20, 5, TimelineSelection{})
			return err
		}},
		{"before", func(ctx context.Context) error {
			_, err := s.ListItemsBeforeCursor(ctx, "t", TimelineCursor{TurnIndex: 9}, 20, 5, TimelineSelection{})
			return err
		}},
		{"after", func(ctx context.Context) error {
			_, err := s.ListItemsAfterCursor(ctx, "t", TimelineCursor{TurnIndex: 0}, 20, 5, TimelineSelection{})
			return err
		}},
		{"members", func(ctx context.Context) error {
			_, err := s.ListActivityRunMembers(ctx, "t", ActivityRunMembersRequest{RunFirstItemID: "r", Direction: ActivityRunMembersBefore, Limit: 5})
			return err
		}},
		{"recompute", func(ctx context.Context) error {
			_, err := s.RecomputeSubagentAggregates(ctx, "t", 10)
			return err
		}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make([]chan error, len(reads))
	for i, tc := range reads {
		errs[i] = make(chan error, 1)
		go func() { errs[i] <- tc.read(ctx) }()
	}
	// Let every read reach its wait.
	time.Sleep(100 * time.Millisecond)
	requireReadRuns(t, "a single-statement read", func() error {
		_, _, err := s.GetThreadItem("t", "t-i0")
		return err
	})
	cancel()
	for i, tc := range reads {
		if err := <-errs[i]; !errors.Is(err, context.Canceled) {
			t.Errorf("%s: %v, want it still waiting for a slot until cancelled", tc.name, err)
		}
	}
}

// A subagent subtree read waits for a history read slot like a page: its
// walk grows with the agent's history. It has no caller context, so it
// waits until a slot frees.
func TestSubagentDescendantsTakeAHistoryReadSlot(t *testing.T) {
	s := newTestStore(t)
	seedTimelineParityThread(t, s)
	releases := holdHistoryReadSlots(t, s, true)
	type result struct {
		items []Item
		err   error
	}
	done := make(chan result, 1)
	go func() {
		items, err := s.ListSubagentDescendants(timelineParityThreadID, "loc-launch-2")
		done <- result{items, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("subagent descendants read ran with every history read slot held: %d rows, %v", len(got.items), got.err)
	case <-time.After(100 * time.Millisecond):
	}
	requireReadRuns(t, "a single-statement read", func() error {
		_, _, err := s.GetThreadItem(timelineParityThreadID, "loc-launch-2")
		return err
	})
	releases[0]()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if ids := itemIDs(got.items); !slices.Equal(ids, []string{"loc-child-2", "loc-grandchild-2"}) {
			t.Errorf("descendants = %v, want loc-child-2 and loc-grandchild-2", ids)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subagent descendants read did not run after a slot freed")
	}
}

// A read of a whole thread, or of every thread, waits for a history read
// slot like a page, and runs once one frees.
func TestWholeHistoryReadsTakeAHistoryReadSlot(t *testing.T) {
	s := newTestStore(t)
	seedTimelineParityThread(t, s)
	const thread = timelineParityThreadID
	for _, tc := range []struct {
		name string
		read func() error
	}{
		{"ListItems", func() error { _, err := s.ListItems(thread); return err }},
		{"ListThreadUserMessageTicks", func() error {
			_, err := s.ListThreadUserMessageTicks(thread, TimelineSelection{})
			return err
		}},
		{"ListThreadUserMessageHistory", func() error { _, err := s.ListThreadUserMessageHistory(thread, 20); return err }},
		{"LatestHumanUserText", func() error { _, _, err := s.LatestHumanUserText(thread); return err }},
		{"ThreadTitleContextItems", func() error { _, _, err := s.ThreadTitleContextItems(thread, 20); return err }},
		{"ListEditDiffItems", func() error { _, err := s.ListEditDiffItems(thread); return err }},
		{"ListTurnUserSummaries", func() error { _, err := s.ListTurnUserSummaries(thread); return err }},
		{"SearchThreadMessages", func() error { _, err := s.SearchThreadMessages("answer", 10); return err }},
		{"SearchThreadItems", func() error { _, err := s.SearchThreadItems(thread, "answer", 10); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			releases := holdHistoryReadSlots(t, s, true)
			done := make(chan error, 1)
			go func() { done <- tc.read() }()
			select {
			case err := <-done:
				t.Fatalf("ran with every history read slot held: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			releases[0]()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("did not run after a slot freed")
			}
		})
	}
}
