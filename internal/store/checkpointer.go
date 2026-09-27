package store

import (
	"context"
	"fmt"
	"log"
	"time"
)

// walCheckpointBoundPages is the WAL length, in frames, at which the writer
// checkpoints inside a commit (the writer's wal_autocheckpoint).
//
// The checkpointer copies frames into the database file off the writer, but
// SQLite restarts the WAL from its first frame only when a write transaction
// begins after a checkpoint has copied every frame. Under writes with no gap
// a commit lands while the checkpointer copies, so nothing restarts the WAL
// and it keeps growing. At this bound the writer's own checkpoint copies the
// frames the checkpointer has not reached and the next write restarts the
// WAL. That keeps the file near 64 MiB and makes a commit wait on checkpoint
// I/O once per 64 MiB of such writes, instead of once per 4 MiB with SQLite's
// default of 1000 frames. Writes with gaps do not reach it: the WAL restarts
// in a gap after the checkpointer catches up.
const walCheckpointBoundPages = 16384

// checkpointInterval is how long the checkpointer lets commits accumulate
// before it copies them. A burst of commits is checkpointed once per
// interval, so a page rewritten by every commit of the burst (the thread
// row's history stamp) is copied once, not once per commit.
const checkpointInterval = time.Second

// commitSignal is set by every commit on the writer connection and consumed
// by the checkpointer. It holds at most one pending signal: the checkpoint
// that consumes it copies every commit before it.
type commitSignal chan struct{}

func newCommitSignal() commitSignal { return make(commitSignal, 1) }

// hook is the writer connection's commit hook. It runs inside SQLite's
// commit, so it must not block or call SQLite; returning 0 lets the commit
// proceed.
func (c commitSignal) hook() int32 {
	select {
	case c <- struct{}{}:
	default:
	}
	return 0
}

// checkpointer owns WAL checkpoints while the store is open. WAL
// maintenance in docs/architecture/sqlite-store.md names the others.
//
// Checkpoints run on a read-pool connection. A PASSIVE checkpoint takes the
// checkpointer lock, not the write lock, so commits continue while it copies
// and fsyncs; on the writer, every write queued behind the copying commit
// waits for the disk. A checkpoint's length grows with the frames it copies
// and with disk contention, so it takes a history read slot
// (historyReadSlots) before its connection.
type checkpointer struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startCheckpointer runs the checkpoint loop until stopCheckpointer. It
// requires the read pool; a store without one has no WAL to checkpoint.
func (s *Store) startCheckpointer(commits commitSignal, interval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	s.checkpoints.cancel = cancel
	s.checkpoints.done = make(chan struct{})
	go func() {
		defer close(s.checkpoints.done)
		checkpointLoop(ctx, commits, interval, func(ctx context.Context) error {
			_, err := s.checkpointWAL(ctx)
			return err
		})
	}()
}

// stopCheckpointer ends the loop and waits for a running checkpoint to
// return. It is a no-op when the loop never started.
func (s *Store) stopCheckpointer() {
	if s.checkpoints.cancel == nil {
		return
	}
	s.checkpoints.cancel()
	<-s.checkpoints.done
}

// checkpointLoop calls checkpoint once per interval while commits signal,
// until ctx is done.
func checkpointLoop(ctx context.Context, commits commitSignal, interval time.Duration, checkpoint func(context.Context) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-commits:
		}
		// The interval starts at the first commit, not the last: a steady
		// stream of commits is checkpointed once per interval rather than
		// never.
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		// A commit signals before it writes its frames, so a commit
		// signalled during the wait may still be writing when this
		// checkpoint copies. Its signal stays and wakes one more round.
		// Only a single commit that takes longer than interval to write
		// waits in the WAL for the next commit.
		if err := checkpoint(ctx); err != nil && ctx.Err() == nil {
			log.Printf("store: %v", err)
		}
	}
}

// checkpointWAL copies the WAL's committed frames into the database file on
// a read-pool connection. A Busy result means another checkpoint holds the
// lock; the frames it leaves are copied by the next round. A store without a
// read pool has no WAL. A skipped checkpoint returns a zero result.
func (s *Store) checkpointWAL(ctx context.Context) (CheckpointResult, error) {
	release, err := s.historyReads.acquire(ctx)
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("checkpoint: wait for a read slot: %w", err)
	}
	defer release()
	// Reads are quiesced while an operation that needs the database to
	// itself runs (TruncateCheckpoint, the conversion swap). The check
	// follows the wait, as historyReadSnapshot's choice of pool does, which
	// keeps the wait out of the window quiesceReads cannot see. The frames
	// a skipped round leaves wait for the round the next commit wakes.
	if s.read == nil || s.readsQuiesced.Load() {
		return CheckpointResult{}, nil
	}
	// PASSIVE never waits for readers or the writer; it copies what it can.
	var res CheckpointResult
	var busy int64
	if err := s.read.QueryRowContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &res.WALFrames, &res.Checkpointed); err != nil {
		return CheckpointResult{}, fmt.Errorf("checkpoint: %w", err)
	}
	res.Busy = busy != 0
	return res, nil
}
