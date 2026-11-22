package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

type fakeConn struct {
	pid       int
	done      chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool
	closeErr  error
}

func newFakeConn(pid int) *fakeConn {
	return &fakeConn{
		pid:  pid,
		done: make(chan struct{}),
	}
}

func (c *fakeConn) PID() int              { return c.pid }
func (c *fakeConn) Done() <-chan struct{} { return c.done }
func (c *fakeConn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.done)
	})
	return c.closeErr
}

func (c *fakeConn) crash() {
	c.closeOnce.Do(func() {
		close(c.done)
	})
}

func TestConnPoolSingleFlightsAndReuses(t *testing.T) {
	t.Parallel()
	pool := NewConnPool()

	var dials atomic.Int32
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	dial := func(context.Context) (provider.Connection, error) {
		n := dials.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-gate
		return newFakeConn(int(100 + n)), nil
	}

	var wg sync.WaitGroup
	conns := make([]provider.Connection, 4)
	releases := make([]func(), 4)
	errs := make([]error, 4)
	for i := range 4 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			conns[idx], releases[idx], errs[idx] = pool.Acquire(context.Background(), "codex:key1", time.Minute, dial)
		}(i)
	}
	<-started
	close(gate)
	wg.Wait()

	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want 1", dials.Load())
	}
	for i := range 4 {
		if errs[i] != nil {
			t.Fatalf("acquire %d failed: %v", i, errs[i])
		}
		if conns[i] != conns[0] {
			t.Fatalf("acquire %d got different connection", i)
		}
	}

	// Release all 4 references; idle grace keeps the connection open.
	fc := conns[0].(*fakeConn)
	for _, rel := range releases {
		rel()
	}
	if fc.closed.Load() {
		t.Fatal("connection closed immediately despite positive idle grace")
	}

	// Re-acquire before grace expires reuses the same connection.
	reused, rel2, err := pool.Acquire(context.Background(), "codex:key1", time.Minute, dial)
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if reused != conns[0] {
		t.Fatal("expected cached connection reuse")
	}
	rel2()

	if err := pool.CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if !fc.closed.Load() {
		t.Fatal("CloseAll did not close active connection")
	}
}

func TestConnPoolIdleGraceExpires(t *testing.T) {
	t.Parallel()
	pool := NewConnPool()

	fc := newFakeConn(200)
	_, rel, err := pool.Acquire(context.Background(), "agy:k", 25*time.Millisecond, func(context.Context) (provider.Connection, error) {
		return fc, nil
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	rel()

	select {
	case <-fc.Done():
	case <-time.After(time.Second):
		t.Fatal("idle connection did not close after grace")
	}
	if pool.ActiveCount() != 0 {
		t.Fatalf("ActiveCount = %d, want 0", pool.ActiveCount())
	}
}

func TestConnPoolEvictsCrashedConnection(t *testing.T) {
	t.Parallel()
	pool := NewConnPool()

	first := newFakeConn(301)
	second := newFakeConn(302)
	var dials atomic.Int32
	dial := func(context.Context) (provider.Connection, error) {
		if dials.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	}

	c1, rel1, err := pool.Acquire(context.Background(), "codex:k", time.Minute, dial)
	if err != nil || c1 != first {
		t.Fatalf("first acquire = %v/%v", c1, err)
	}
	first.crash()
	rel1()

	c2, rel2, err := pool.Acquire(context.Background(), "codex:k", time.Minute, dial)
	if err != nil || c2 != second {
		t.Fatalf("second acquire after crash = %v/%v, want second", c2, err)
	}
	rel2()
	_ = pool.CloseAll()
}

func TestConnPoolDrainWhileBusyClosesOnLastRelease(t *testing.T) {
	t.Parallel()
	pool := NewConnPool()

	first := newFakeConn(401)
	second := newFakeConn(402)
	var dials atomic.Int32
	dial := func(context.Context) (provider.Connection, error) {
		if dials.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	}

	_, rel1, err := pool.Acquire(context.Background(), "codex:acct", time.Minute, dial)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	pool.DrainPrefix("codex:")

	// Busy connection stays alive while referenced, but a new acquire dials a replacement.
	if first.closed.Load() {
		t.Fatal("draining connection closed while still referenced")
	}
	c2, rel2, err := pool.Acquire(context.Background(), "codex:acct", time.Minute, dial)
	if err != nil || c2 != second {
		t.Fatalf("acquire after drain = %v/%v, want second", c2, err)
	}

	// Releasing last reference on drained connection closes it immediately despite idle grace.
	rel1()
	if !first.closed.Load() {
		t.Fatal("drained connection did not close on last release")
	}
	rel2()
	_ = pool.CloseAll()
}

func TestConnPoolBorrowPreventsIdleClose(t *testing.T) {
	t.Parallel()
	pool := NewConnPool()

	fc := newFakeConn(501)
	_, rel, err := pool.Acquire(context.Background(), "codex:k", 30*time.Millisecond, func(context.Context) (provider.Connection, error) {
		return fc, nil
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	borrowed, unborrow, ok := pool.Borrow("codex:k")
	if !ok || borrowed != fc {
		t.Fatalf("borrow = %v/%v, want fc/true", borrowed, ok)
	}
	rel()
	time.Sleep(60 * time.Millisecond)
	if fc.closed.Load() {
		t.Fatal("connection closed while borrowed")
	}
	unborrow()
	select {
	case <-fc.Done():
	case <-time.After(time.Second):
		t.Fatal("connection did not close after unborrow and idle grace")
	}
}

func TestConnPoolResetAllowsFutureAcquires(t *testing.T) {
	t.Parallel()
	pool := NewConnPool()

	first := newFakeConn(601)
	_, rel1, err := pool.Acquire(context.Background(), "codex:k", time.Minute, func(context.Context) (provider.Connection, error) {
		return first, nil
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	rel1()

	if err := pool.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if !first.closed.Load() {
		t.Fatal("Reset did not close pooled connection")
	}

	second := newFakeConn(602)
	got, rel2, err := pool.Acquire(context.Background(), "codex:k", 0, func(context.Context) (provider.Connection, error) {
		return second, nil
	})
	if err != nil || got != second {
		t.Fatalf("acquire after Reset = %v/%v", got, err)
	}
	rel2()
	if !second.closed.Load() {
		t.Fatal("zero-grace connection did not close on release")
	}

	if err := pool.CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if _, _, err := pool.Acquire(context.Background(), "codex:k", 0, func(context.Context) (provider.Connection, error) {
		return newFakeConn(603), nil
	}); !errors.Is(err, ErrConnPoolClosed) {
		t.Fatalf("Acquire after CloseAll err = %v, want ErrConnPoolClosed", err)
	}
}

type reentrantCloseConn struct {
	pid     int
	done    chan struct{}
	onClose func()
}

func (c *reentrantCloseConn) PID() int              { return c.pid }
func (c *reentrantCloseConn) Done() <-chan struct{} { return c.done }
func (c *reentrantCloseConn) Close() error {
	if c.onClose != nil {
		c.onClose()
	}
	close(c.done)
	return nil
}

func TestConnPoolCloseAllDoesNotDeadlockOnReentrantRelease(t *testing.T) {
	t.Parallel()
	pool := NewConnPool()

	rc := &reentrantCloseConn{pid: 701, done: make(chan struct{})}
	_, rel, err := pool.Acquire(context.Background(), "codex:reentrant", time.Minute, func(context.Context) (provider.Connection, error) {
		return rc, nil
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Simulate conn.Close triggering session teardown that releases its pool
	// ref synchronously before Close returns.
	rc.onClose = rel

	done := make(chan error, 1)
	go func() { done <- pool.CloseAll() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("CloseAll: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CloseAll deadlocked on re-entrant release")
	}
}
