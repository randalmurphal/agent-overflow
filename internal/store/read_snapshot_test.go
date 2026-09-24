package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
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
