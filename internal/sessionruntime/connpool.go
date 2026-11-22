package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/provider"
)

// ErrConnPoolClosed is returned by Acquire when the connection pool has been
// permanently closed for shutdown.
var ErrConnPoolClosed = errors.New("sessionruntime: connection pool is closed")

type dialResult struct {
	conn    provider.Connection
	release func()
	err     error
	retry   bool
}

type connDial struct {
	cancel  context.CancelFunc
	stale   bool
	done    bool
	waiters []chan dialResult
}

type pooledConn struct {
	key          string
	conn         provider.Connection
	refs         int
	draining     bool
	idleGrace    time.Duration
	idleTimer    *time.Timer
	idleGen      uint64
	closeStarted atomic.Bool
	closeOnce    sync.Once
	closeErr     error
}

func (pc *pooledConn) closeConn() error {
	pc.closeStarted.Store(true)
	pc.closeOnce.Do(func() {
		if pc.conn != nil {
			pc.closeErr = pc.conn.Close()
		}
	})
	return pc.closeErr
}

// closeConnIfFirst starts closing the connection only if no other caller
// (such as CloseAll or Drain) has already begun closing it. Avoids re-entrant
// sync.Once deadlock when conn.Close() triggers session teardown that calls
// release() before Close() returns.
func (pc *pooledConn) closeConnIfFirst() {
	if !pc.closeStarted.CompareAndSwap(false, true) {
		return
	}
	_ = pc.closeConn()
}

// ConnPool manages shared provider connections keyed by an opaque pool key.
//
// Invariants:
//   - At most one active (non-draining) connection exists per key.
//   - Dials are single-flighted per key; no provider I/O runs while mu is held.
//   - A dead connection (Done() closed) is evicted automatically and never
//     handed to a caller.
//   - Releasing the last reference arms an idle grace timer (when idleGrace > 0
//     and the connection is not draining); a new Acquire or Borrow cancels it.
//   - Draining connections accept no new references and close immediately once
//     their last reference is released.
type ConnPool struct {
	mu       sync.Mutex
	active   map[string]*pooledConn
	draining map[*pooledConn]struct{}
	dialing  map[string]*connDial
	dialWG   sync.WaitGroup
	closed   bool
}

// NewConnPool creates an empty connection pool.
func NewConnPool() *ConnPool {
	return &ConnPool{
		active:   make(map[string]*pooledConn),
		draining: make(map[*pooledConn]struct{}),
		dialing:  make(map[string]*connDial),
	}
}

// Acquire returns the live shared connection for key, dialing one when none is
// active. Callers MUST invoke the returned release function exactly once when
// the session (or operation) detaches from the connection.
func (p *ConnPool) Acquire(
	ctx context.Context,
	key string,
	idleGrace time.Duration,
	dial func(context.Context) (provider.Connection, error),
) (provider.Connection, func(), error) {
	if strings.TrimSpace(key) == "" {
		return nil, nil, errors.New("sessionruntime: connection pool key is required")
	}
	if dial == nil {
		return nil, nil, errors.New("sessionruntime: connection dialer is required")
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, nil, ErrConnPoolClosed
		}
		if pc, ok := p.active[key]; ok {
			if !isConnDead(pc.conn) {
				if pc.idleTimer != nil {
					pc.idleTimer.Stop()
					pc.idleTimer = nil
					pc.idleGen++
				}
				pc.refs++
				p.mu.Unlock()
				return pc.conn, p.makeRelease(pc), nil
			}
			delete(p.active, key)
		}

		waiter := make(chan dialResult, 1)
		lead, ok := p.dialing[key]
		if ok {
			lead.waiters = append(lead.waiters, waiter)
			p.mu.Unlock()
		} else {
			dialCtx, dialCancel := context.WithCancel(context.Background())
			lead = &connDial{
				cancel:  dialCancel,
				waiters: []chan dialResult{waiter},
			}
			p.dialing[key] = lead
			p.dialWG.Add(1)
			p.mu.Unlock()
			go p.runDial(dialCtx, key, idleGrace, dial, lead)
		}

		select {
		case res := <-waiter:
			if res.retry {
				continue
			}
			return res.conn, res.release, res.err
		case <-ctx.Done():
			p.mu.Lock()
			removed := false
			if !lead.done {
				for i, ch := range lead.waiters {
					if ch == waiter {
						lead.waiters = append(lead.waiters[:i], lead.waiters[i+1:]...)
						removed = true
						break
					}
				}
				if removed && len(lead.waiters) == 0 {
					lead.cancel()
				}
			}
			p.mu.Unlock()
			if !removed {
				// Dial finished right before we acquired mu; release the ref
				// delivered to our buffered channel so it never leaks.
				if res := <-waiter; res.release != nil {
					res.release()
				}
			}
			return nil, nil, ctx.Err()
		}
	}
}

func (p *ConnPool) runDial(
	dialCtx context.Context,
	key string,
	idleGrace time.Duration,
	dial func(context.Context) (provider.Connection, error),
	lead *connDial,
) {
	defer p.dialWG.Done()
	defer lead.cancel()

	var (
		conn provider.Connection
		err  error
	)
	completed := false
	func() {
		defer func() {
			if !completed {
				err = fmt.Errorf("sessionruntime: dial for key %q panicked", key)
			}
		}()
		conn, err = dial(dialCtx)
		completed = true
	}()

	p.mu.Lock()
	if p.dialing[key] == lead {
		delete(p.dialing, key)
	}
	lead.done = true
	waiters := lead.waiters
	lead.waiters = nil

	if err != nil {
		p.mu.Unlock()
		for _, ch := range waiters {
			ch <- dialResult{err: err}
		}
		return
	}
	if conn == nil {
		nilErr := errors.New("sessionruntime: connection dialer returned nil connection")
		p.mu.Unlock()
		for _, ch := range waiters {
			ch <- dialResult{err: nilErr}
		}
		return
	}
	if p.closed {
		p.mu.Unlock()
		for _, ch := range waiters {
			ch <- dialResult{err: ErrConnPoolClosed}
		}
		_ = conn.Close()
		return
	}
	if lead.stale {
		p.mu.Unlock()
		for _, ch := range waiters {
			ch <- dialResult{retry: true}
		}
		_ = conn.Close()
		return
	}
	if len(waiters) == 0 {
		p.mu.Unlock()
		_ = conn.Close()
		return
	}

	pc := &pooledConn{
		key:       key,
		conn:      conn,
		refs:      len(waiters),
		idleGrace: idleGrace,
	}
	p.active[key] = pc
	releases := make([]func(), len(waiters))
	for i := range waiters {
		releases[i] = p.makeRelease(pc)
	}
	p.mu.Unlock()

	if doneCh := conn.Done(); doneCh != nil {
		go p.watchDead(pc, doneCh)
	}
	for i, ch := range waiters {
		ch <- dialResult{conn: conn, release: releases[i]}
	}
}

// Borrow takes a temporary reference on an already-live connection for key
// without dialing a new one. Used by process-wide reads (skills, models,
// account usage) so the connection cannot idle-close mid-request.
func (p *ConnPool) Borrow(key string) (provider.Connection, func(), bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, nil, false
	}
	pc, ok := p.active[key]
	if !ok || isConnDead(pc.conn) {
		if ok {
			delete(p.active, key)
		}
		return nil, nil, false
	}
	if pc.idleTimer != nil {
		pc.idleTimer.Stop()
		pc.idleTimer = nil
		pc.idleGen++
	}
	pc.refs++
	return pc.conn, p.makeRelease(pc), true
}

// BorrowMatching borrows the first live active connection whose key and handle
// satisfy match.
func (p *ConnPool) BorrowMatching(match func(key string, conn provider.Connection) bool) (provider.Connection, func(), bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || match == nil {
		return nil, nil, false
	}
	for key, pc := range p.active {
		if isConnDead(pc.conn) {
			delete(p.active, key)
			continue
		}
		if !match(key, pc.conn) {
			continue
		}
		if pc.idleTimer != nil {
			pc.idleTimer.Stop()
			pc.idleTimer = nil
			pc.idleGen++
		}
		pc.refs++
		return pc.conn, p.makeRelease(pc), true
	}
	return nil, nil, false
}

// DrainKey marks the active connection (and any in-flight dial) for key as
// draining so subsequent Acquires dial a fresh connection. If it has no active
// references, it closes immediately.
func (p *ConnPool) DrainKey(key string) {
	var toClose *pooledConn
	p.mu.Lock()
	if lead, ok := p.dialing[key]; ok {
		lead.stale = true
		delete(p.dialing, key)
	}
	if pc, ok := p.active[key]; ok {
		toClose = p.markDrainingLocked(pc)
	}
	p.mu.Unlock()
	if toClose != nil {
		_ = toClose.closeConn()
	}
}

// DrainPrefix marks every active connection and in-flight dial whose pool key
// starts with prefix as draining. Connections with zero active references close
// immediately.
func (p *ConnPool) DrainPrefix(prefix string) {
	var toClose []*pooledConn
	p.mu.Lock()
	for key, lead := range p.dialing {
		if strings.HasPrefix(key, prefix) {
			lead.stale = true
			delete(p.dialing, key)
		}
	}
	for key, pc := range p.active {
		if strings.HasPrefix(key, prefix) {
			if closedNow := p.markDrainingLocked(pc); closedNow != nil {
				toClose = append(toClose, closedNow)
			}
		}
	}
	p.mu.Unlock()
	for _, pc := range toClose {
		_ = pc.closeConn()
	}
}

// DrainConn marks the specific connection handle as draining.
func (p *ConnPool) DrainConn(conn provider.Connection) {
	if conn == nil {
		return
	}
	var toClose *pooledConn
	p.mu.Lock()
	for _, pc := range p.active {
		if pc.conn == conn {
			toClose = p.markDrainingLocked(pc)
			break
		}
	}
	p.mu.Unlock()
	if toClose != nil {
		_ = toClose.closeConn()
	}
}

// ActiveCount reports the number of currently active (non-draining) connections.
func (p *ConnPool) ActiveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.active)
}

// Reset closes every active and draining connection, cancels in-flight dials,
// and waits for them to finish, leaving the pool open for subsequent Acquires
// (used by HarnessReset).
func (p *ConnPool) Reset() error {
	return p.closeAll(false, 0)
}

// CloseAll permanently closes the pool and every active and draining
// connection in parallel (used by App.Shutdown).
func (p *ConnPool) CloseAll() error {
	return p.closeAll(true, 0)
}

// CloseAllWithTimeout permanently closes the pool and bounds how long shutdown
// waits for hung connection closes or in-flight dials before returning.
func (p *ConnPool) CloseAllWithTimeout(timeout time.Duration) error {
	return p.closeAll(true, timeout)
}

func (p *ConnPool) closeAll(markClosed bool, timeout time.Duration) error {
	p.mu.Lock()
	if markClosed {
		p.closed = true
	}
	for key, lead := range p.dialing {
		lead.stale = true
		lead.cancel()
		delete(p.dialing, key)
	}
	conns := make([]*pooledConn, 0, len(p.active)+len(p.draining))
	for key, pc := range p.active {
		if pc.idleTimer != nil {
			pc.idleTimer.Stop()
			pc.idleTimer = nil
			pc.idleGen++
		}
		pc.draining = true
		conns = append(conns, pc)
		delete(p.active, key)
	}
	for pc := range p.draining {
		if pc.idleTimer != nil {
			pc.idleTimer.Stop()
			pc.idleTimer = nil
			pc.idleGen++
		}
		conns = append(conns, pc)
		delete(p.draining, pc)
	}
	p.mu.Unlock()

	errs := make([]error, len(conns))
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i, pc := range conns {
			wg.Add(1)
			go func(idx int, target *pooledConn) {
				defer wg.Done()
				errs[idx] = target.closeConn()
			}(i, pc)
		}
		wg.Wait()
		p.dialWG.Wait()
		close(done)
	}()

	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-done:
			return errors.Join(errs...)
		case <-timer.C:
			return fmt.Errorf("sessionruntime: timed out closing pooled connections after %s", timeout)
		}
	}
	<-done
	return errors.Join(errs...)
}

func (p *ConnPool) markDrainingLocked(pc *pooledConn) *pooledConn {
	if p.active[pc.key] == pc {
		delete(p.active, pc.key)
	}
	pc.draining = true
	if pc.idleTimer != nil {
		pc.idleTimer.Stop()
		pc.idleTimer = nil
		pc.idleGen++
	}
	if pc.refs <= 0 {
		delete(p.draining, pc)
		return pc
	}
	p.draining[pc] = struct{}{}
	return nil
}

func (p *ConnPool) makeRelease(pc *pooledConn) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.release(pc)
		})
	}
}

func (p *ConnPool) release(pc *pooledConn) {
	var closeNow bool
	p.mu.Lock()
	if pc.refs > 0 {
		pc.refs--
	}
	if pc.refs == 0 {
		if pc.draining || p.closed || pc.idleGrace <= 0 || isConnDead(pc.conn) {
			if p.active[pc.key] == pc {
				delete(p.active, pc.key)
			}
			delete(p.draining, pc)
			if pc.idleTimer != nil {
				pc.idleTimer.Stop()
				pc.idleTimer = nil
				pc.idleGen++
			}
			closeNow = true
		} else if pc.idleTimer == nil && p.active[pc.key] == pc {
			pc.idleGen++
			gen := pc.idleGen
			pc.idleTimer = time.AfterFunc(pc.idleGrace, func() {
				p.expireIdle(pc, gen)
			})
		}
	}
	p.mu.Unlock()

	if closeNow {
		pc.closeConnIfFirst()
	}
}

func (p *ConnPool) expireIdle(pc *pooledConn, gen uint64) {
	var closeNow bool
	p.mu.Lock()
	if pc.idleGen == gen && pc.refs == 0 && p.active[pc.key] == pc {
		delete(p.active, pc.key)
		pc.draining = true
		pc.idleTimer = nil
		pc.idleGen++
		closeNow = true
	}
	p.mu.Unlock()
	if closeNow {
		pc.closeConnIfFirst()
	}
}

func (p *ConnPool) watchDead(pc *pooledConn, doneCh <-chan struct{}) {
	<-doneCh
	p.mu.Lock()
	if p.active[pc.key] == pc {
		delete(p.active, pc.key)
	}
	delete(p.draining, pc)
	if pc.idleTimer != nil {
		pc.idleTimer.Stop()
		pc.idleTimer = nil
		pc.idleGen++
	}
	pc.draining = true
	p.mu.Unlock()
	// Close the dead connection handle so its stdin pipe FD and cancel func
	// are released even when the connection crashed while idle (refs == 0).
	pc.closeConnIfFirst()
}

func isConnDead(conn provider.Connection) bool {
	if conn == nil {
		return true
	}
	doneCh := conn.Done()
	if doneCh == nil {
		return false
	}
	select {
	case <-doneCh:
		return true
	default:
		return false
	}
}
