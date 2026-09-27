package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"modernc.org/sqlite"
)

// runCheckpointLoop starts checkpointLoop in the calling synctest bubble with
// a checkpoint that counts its calls and runs body, and returns the count and
// a stop function that waits for the loop to exit.
func runCheckpointLoop(t *testing.T, commits commitSignal, interval time.Duration, body func(context.Context) error) (*atomic.Int64, func()) {
	t.Helper()
	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		checkpointLoop(ctx, commits, interval, func(ctx context.Context) error {
			calls.Add(1)
			if body != nil {
				return body(ctx)
			}
			return nil
		})
	}()
	return &calls, func() {
		cancel()
		<-done
	}
}

func TestCheckpointLoopIdlesWithoutCommits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls, stop := runCheckpointLoop(t, newCommitSignal(), time.Second, nil)
		defer stop()
		time.Sleep(time.Hour)
		synctest.Wait()
		if got := calls.Load(); got != 0 {
			t.Fatalf("checkpoints with no commits = %d, want 0", got)
		}
	})
}

// A burst of commits within one interval is copied by one checkpoint, and
// one more round follows for a commit the burst signalled that was still
// writing its frames when the checkpoint copied; then the loop idles.
func TestCheckpointLoopCopiesABurstAndItsLastCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		commits := newCommitSignal()
		calls, stop := runCheckpointLoop(t, commits, time.Second, nil)
		defer stop()
		for range 5 {
			commits.hook()
		}
		time.Sleep(300 * time.Millisecond)
		for range 5 {
			commits.hook()
		}
		time.Sleep(700*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		if got := calls.Load(); got != 0 {
			t.Fatalf("checkpoints before the interval elapsed = %d, want 0", got)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if got := calls.Load(); got != 2 {
			t.Fatalf("checkpoints for one burst = %d, want 2", got)
		}
	})
}

// The interval runs from the first commit: a steady stream of commits is
// checkpointed once per interval, not deferred until it stops.
func TestCheckpointLoopCheckpointsASteadyStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		commits := newCommitSignal()
		calls, stop := runCheckpointLoop(t, commits, time.Second, nil)
		defer stop()
		for range 100 {
			commits.hook()
			time.Sleep(100 * time.Millisecond)
		}
		synctest.Wait()
		// Ten seconds of commits; the first round starts at the first
		// commit and each later one at the first commit after a round.
		if got := calls.Load(); got < 9 {
			t.Fatalf("checkpoints over 10 intervals of steady commits = %d, want at least 9", got)
		}
	})
}

// A commit made while a checkpoint runs may not be in the frames it copies,
// so it wakes another round.
func TestCheckpointLoopRunsAgainForACommitDuringACheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		commits := newCommitSignal()
		running := make(chan struct{})
		finish := make(chan struct{})
		first := true
		calls, stop := runCheckpointLoop(t, commits, time.Second, func(context.Context) error {
			if first {
				first = false
				close(running)
				<-finish
			}
			return nil
		})
		defer stop()
		commits.hook()
		<-running
		commits.hook()
		close(finish)
		time.Sleep(time.Hour)
		synctest.Wait()
		if got := calls.Load(); got != 2 {
			t.Fatalf("checkpoints = %d, want 2: the commit during the first must wake a second", got)
		}
	})
}

// Stopping the loop cancels a checkpoint that is waiting and returns.
func TestCheckpointLoopStopCancelsAWaitingCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		commits := newCommitSignal()
		waiting := make(chan struct{})
		calls, stop := runCheckpointLoop(t, commits, time.Second, func(ctx context.Context) error {
			close(waiting)
			<-ctx.Done()
			return ctx.Err()
		})
		commits.hook()
		<-waiting
		stop()
		if got := calls.Load(); got != 1 {
			t.Fatalf("checkpoints = %d, want 1", got)
		}
	})
}

// hookSpyConn is a driver connection that records its commit hook
// registrations.
type hookSpyConn struct {
	sqliteConn
	hooked commitHooker
	calls  *[]bool
}

func (c hookSpyConn) RegisterCommitHook(fn sqlite.CommitHookFn) {
	*c.calls = append(*c.calls, fn != nil)
	c.hooked.RegisterCommitHook(fn)
}

type hookSpyConnector struct {
	driver.Connector
	calls *[]bool
}

func (c hookSpyConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return hookSpyConn{sqliteConn: conn.(sqliteConn), hooked: conn.(commitHooker), calls: c.calls}, nil
}

// The driver keeps a registered commit hook until it is unregistered, so a
// hooked connection unregisters it when it closes.
func TestWriterConnectionRemovesItsCommitHookOnClose(t *testing.T) {
	base, err := sqlite.NewConnector(poolDSN(filepath.Join(t.TempDir(), "hook.sqlite"), writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	var calls []bool
	connector := gatedConnector{Connector: hookSpyConnector{Connector: base, calls: &calls}, commits: newCommitSignal()}
	conn, err := connector.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !calls[0] {
		t.Fatalf("registrations after connect = %v, want one hook", calls)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1] {
		t.Fatalf("registrations after close = %v, want the hook removed", calls)
	}
}

func newCheckpointedStore(t *testing.T, interval time.Duration) *Store {
	t.Helper()
	s, err := NewWithOptions(newTestStorePath(t), Options{checkpointInterval: interval})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	if s.read == nil {
		t.Fatal("the store has no read pool")
	}
	return s
}

// Commits on the writer wake the checkpointer, which copies them into the
// database file. A commit signals before it writes its frames, so the
// 4 MiB commit is followed by a small one whose round finds it written.
func TestStoreCheckpointsCommitsInTheBackground(t *testing.T) {
	s := newCheckpointedStore(t, 10*time.Millisecond)
	before, err := fileSize(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetUIState("client:checkpoint", map[string]string{"k": strings.Repeat("x", 4<<20)}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUIState("client:checkpoint-next", map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	var after int64
	for deadline := time.Now().Add(10 * time.Second); after-before < 4<<20 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
		if after, err = fileSize(s.path); err != nil {
			t.Fatal(err)
		}
	}
	if after-before < 4<<20 {
		t.Fatalf("database file grew %d bytes, want the 4 MiB commit copied into it", after-before)
	}
}

// The writer does not checkpoint at SQLite's default of 1,000 frames: the
// checkpointer copies the WAL off the writer.
func TestWriterLeavesTheWALToTheCheckpointer(t *testing.T) {
	s := newCheckpointedStore(t, time.Hour)
	growWAL(t, s, s.path)
	res, err := s.checkpointWAL(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.WALFrames <= 1000 {
		t.Fatalf("WAL holds %d frames, want more than 1000: the writer checkpointed", res.WALFrames)
	}
}

// Under writes with no gap every commit lands while the checkpointer
// copies, so nothing restarts the WAL until the writer's own checkpoint at
// its bound copies the last frames. The bound here is 64 frames.
func TestWriterBoundKeepsTheWALShortUnderSustainedWrites(t *testing.T) {
	s := newCheckpointedStore(t, time.Millisecond)
	mustExec(t, s.db, "PRAGMA wal_autocheckpoint = 64")
	blob := strings.Repeat("x", 4096)
	for i := range 1500 {
		if err := s.SetUIState("client:wal", map[string]string{"k": blob + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if wal := walSize(t, s.path); wal > 2<<20 {
		t.Fatalf("WAL after 1500 back-to-back commits with a 64-frame writer bound = %d bytes, want at most 2 MiB", wal)
	}
}

// A checkpoint takes a history read slot before a connection: while
// history reads hold every slot and a connection each, a checkpoint waiting
// for a slot holds none, and a single-statement read still runs.
func TestCheckpointWaitsForAHistoryReadSlot(t *testing.T) {
	s := newCheckpointedStore(t, time.Hour)
	if err := s.SetUIState("client:slot", map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	releases := holdHistoryReadSlots(t, s, true)
	done := make(chan error, 1)
	go func() {
		_, err := s.checkpointWAL(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("checkpoint ran while every history slot was held (err %v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	requireReadRuns(t, "a single-statement read", func() error {
		_, err := s.GetUIState("client:slot")
		return err
	})
	releases[0]()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("checkpoint did not run after a slot was released")
	}
}

// A checkpoint checks for quiesced reads after it waits for its slot, so
// one that was waiting when quiesceReads drained the pool skips instead of
// running on it.
func TestCheckpointWaitingForASlotSkipsWhileReadsAreQuiesced(t *testing.T) {
	s := newCheckpointedStore(t, time.Hour)
	if err := s.SetUIState("client:quiesce", map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	releases := holdHistoryReadSlots(t, s, false)
	type result struct {
		res CheckpointResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := s.checkpointWAL(context.Background())
		done <- result{res, err}
	}()
	// Let the checkpoint reach its wait for a slot.
	time.Sleep(100 * time.Millisecond)
	err := s.quiesceReads(func() error {
		releases[0]()
		select {
		case got := <-done:
			if got.res != (CheckpointResult{}) {
				t.Errorf("checkpoint while reads were quiesced = %+v, want it skipped", got.res)
			}
			return got.err
		case <-time.After(10 * time.Second):
			return errors.New("the checkpoint did not return after a slot was released")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.checkpointWAL(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.WALFrames == 0 {
		t.Fatal("the WAL was empty after the skipped checkpoint, so the skip proves nothing")
	}
}

// A checkpoint waiting for a slot returns when its context is cancelled, which
// is how Close stops the loop.
func TestCheckpointWaitingForASlotStopsOnCancel(t *testing.T) {
	s := newCheckpointedStore(t, time.Hour)
	for range historyReadSlots {
		release, err := s.historyReads.acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.checkpointWAL(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("checkpoint with every slot held = %v, want the context's error", err)
	}
}

func TestCloseStopsTheCheckpointer(t *testing.T) {
	s, err := NewWithOptions(newTestStorePath(t), Options{checkpointInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetUIState("client:close", map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.checkpoints.done:
	default:
		t.Fatal("the checkpointer is still running after Close")
	}
}
