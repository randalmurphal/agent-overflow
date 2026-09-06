package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTransferTransactionDurabilityIsExclusiveAndRestoresStreamPolicy(t *testing.T) {
	s := newTestStore(t)
	for _, commit := range []bool{false, true} {
		tx, release, err := s.beginDurableTx(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer release()
			defer tx.Rollback()
			var synchronous, fullfsync int
			if err := tx.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
				t.Fatal(err)
			}
			if err := tx.QueryRow("PRAGMA fullfsync").Scan(&fullfsync); err != nil {
				t.Fatal(err)
			}
			if synchronous != 3 || fullfsync != 1 {
				t.Fatalf("ownership commit is not durable: synchronous=%d fullfsync=%d", synchronous, fullfsync)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if err := s.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("another writer entered the reserved connection: %v", err)
			}
			if commit {
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
		}()
		var synchronous, fullfsync int
		if err := s.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow("PRAGMA fullfsync").Scan(&fullfsync); err != nil {
			t.Fatal(err)
		}
		if synchronous != 1 || fullfsync != 0 {
			t.Fatalf("ordinary streaming policy changed: synchronous=%d fullfsync=%d", synchronous, fullfsync)
		}
	}
}
