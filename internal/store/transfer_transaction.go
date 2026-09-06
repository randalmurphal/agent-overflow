package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log"
	"sync"
	"time"
)

// beginDurableTx temporarily strengthens the exclusively held writer
// connection for execution-ownership changes and remote command acceptance. WAL+NORMAL protects the history
// cache against corruption, but a power loss may rewind acknowledged commits.
// A retirement/activation/cancellation acknowledgment must not rewind: another
// computer can act on it. EXTRA equals FULL in WAL and also covers DELETE mode.
// fullfsync requests the stronger filesystem flush on platforms supporting it.
//
// The caller defers release BEFORE deferring tx.Rollback, so the transaction
// has ended when connection policy is restored. No pool-wide state is changed,
// and ordinary streaming writes keep their normal durability/performance tradeoff.
func (s *Store) beginDurableTx(ctx context.Context) (*sql.Tx, func(), error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	var synchronous, fullfsync int
	if err = conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err == nil {
		err = conn.QueryRowContext(ctx, "PRAGMA fullfsync").Scan(&fullfsync)
	}
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := conn.ExecContext(cleanup, fmt.Sprintf("PRAGMA synchronous=%d", synchronous))
			if err == nil {
				_, err = conn.ExecContext(cleanup, fmt.Sprintf("PRAGMA fullfsync=%d", fullfsync))
			}
			if err != nil {
				// Never return a connection with unknown policy to the stream writer.
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				log.Printf("store: retire durable writer after policy restore failure: %v", err)
			}
			_ = conn.Close()
		})
	}
	if _, err = conn.ExecContext(ctx, "PRAGMA synchronous=EXTRA"); err == nil {
		_, err = conn.ExecContext(ctx, "PRAGMA fullfsync=ON")
	}
	if err != nil {
		release()
		return nil, nil, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		release()
		return nil, nil, err
	}
	return tx, release, nil
}
