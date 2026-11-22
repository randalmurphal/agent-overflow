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

type connDial struct {
	done chan struct{}
	err  error
}

type pooledConn struct {
	key          string
	conn         provider.Connection
	refs         int
	draining     bool
	idleGrace    time.Duration
	idleTimer    *time.Timer
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
				}
				pc.refs++
				p.mu.Unlock()
				return pc.conn, p.makeRelease(pc), nil
			}
			delete(p.active, key)
		}
		if inflight, ok := p.dialing[key]; ok {
			p.mu.Unlock()
			select {
			case <-inflight.done:
				if inflight.err != nil {
					return nil, nil, inflight.err
				}
				continue
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}

		// Lead the dial for this key.
		lead := &connDial{done: make(chan struct{})}
		p.dialing[key] = lead
		p.mu.Unlock()

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
			conn, err = dial(ctx)
			completed = true
		}()

		p.mu.Lock()
		if p.dialing[key] == lead {
			delete(p.dialing, key)
		}
		if err != nil {
			lead.err = err
			p.mu.Unlock()
			close(lead.done)
			return nil, nil, err
		}
		if conn == nil {
			lead.err = errors.New("sessionruntime: connection dialer returned nil connection")
			p.mu.Unlock()
			close(lead.done)
			return nil, nil, lead.err
		}
		if p.closed {
			lead.err = ErrConnPoolClosed
			p.mu.Unlock()
			close(lead.done)
			_ = conn.Close()
			return nil, nil, ErrConnPoolClosed
		}

		pc := &pooledConn{
			key:       key,
			conn:      conn,
			refs:      1,
			idleGrace: idleGrace,
		}
		p.active[key] = pc
		p.mu.Unlock()

		if doneCh := conn.Done(); doneCh != nil {
			go p.watchDead(pc, doneCh)
		}

		close(lead.done)
		return conn, p.makeRelease(pc), nil
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
		}
		pc.refs++
		return pc.conn, p.makeRelease(pc), true
	}
	return nil, nil, false
}

// DrainKey marks the active connection for key as draining so subsequent
// Acquires dial a fresh connection. If it has no active references, it closes
// immediately.
func (p *ConnPool) DrainKey(key string) {
	var toClose *pooledConn
	p.mu.Lock()
	if pc, ok := p.active[key]; ok {
		toClose = p.markDrainingLocked(pc)
	}
	p.mu.Unlock()
	if toClose != nil {
		_ = toClose.closeConn()
	}
}

// DrainPrefix marks every active connection whose pool key starts with prefix
// as draining. Connections with zero active references close immediately.
func (p *ConnPool) DrainPrefix(prefix string) {
	var toClose []*pooledConn
	p.mu.Lock()
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

// Reset closes every active and draining connection and waits for them to
// close, leaving the pool open for subsequent Acquires (used by HarnessReset).
func (p *ConnPool) Reset() error {
	return p.closeAll(false)
}

// CloseAll permanently closes the pool and every active and draining
// connection in parallel (used by App.Shutdown).
func (p *ConnPool) CloseAll() error {
	return p.closeAll(true)
}

func (p *ConnPool) closeAll(markClosed bool) error {
	p.mu.Lock()
	if markClosed {
		p.closed = true
	}
	conns := make([]*pooledConn, 0, len(p.active)+len(p.draining))
	for key, pc := range p.active {
		if pc.idleTimer != nil {
			pc.idleTimer.Stop()
			pc.idleTimer = nil
		}
		pc.draining = true
		conns = append(conns, pc)
		delete(p.active, key)
	}
	for pc := range p.draining {
		if pc.idleTimer != nil {
			pc.idleTimer.Stop()
			pc.idleTimer = nil
		}
		conns = append(conns, pc)
		delete(p.draining, pc)
	}
	p.mu.Unlock()

	if len(conns) == 0 {
		return nil
	}
	errs := make([]error, len(conns))
	var wg sync.WaitGroup
	for i, pc := range conns {
		wg.Add(1)
		go func(idx int, target *pooledConn) {
			defer wg.Done()
			errs[idx] = target.closeConn()
		}(i, pc)
	}
	wg.Wait()
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
			}
			closeNow = true
		} else if pc.idleTimer == nil && p.active[pc.key] == pc {
			pc.idleTimer = time.AfterFunc(pc.idleGrace, func() {
				p.expireIdle(pc)
			})
		}
	}
	p.mu.Unlock()

	if closeNow {
		pc.closeConnIfFirst()
	}
}

func (p *ConnPool) expireIdle(pc *pooledConn) {
	var closeNow bool
	p.mu.Lock()
	if pc.refs == 0 && p.active[pc.key] == pc {
		delete(p.active, pc.key)
		pc.draining = true
		pc.idleTimer = nil
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
	}
	pc.draining = true
	p.mu.Unlock()
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
