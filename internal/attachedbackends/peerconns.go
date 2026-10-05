package attachedbackends

import (
	"context"
	"slices"
	"sync"
	"time"

	"agent-overflow/internal/rpcclient"
)

// Agent calls to one computer come in bursts: paging a transcript or a
// large tool output is one call per window, and every new connection
// spends a ticket from that computer's credential budget. A finished call
// leaves its connection here for the next one instead.
//
// Each connection is used by one call at a time, as a fresh one was, so a
// call that is cancelled still ends only its own connection. The idle
// window stays well inside the far side's keepalive verdict, and a
// connection the far side closed (a revoked session, a listener rebind) is
// skipped by its reader's Done rather than handed to a call. One whose path
// died without a close (a network change, a NAT rebinding) is caught by a
// ping before reuse, so the call dials again instead of waiting out its
// own deadline on a socket nothing answers.
const (
	peerConnIdle     = 5 * time.Second
	maxIdlePeerConns = 4
	peerPingTimeout  = 2 * time.Second
)

type idlePeerConn struct {
	capability string
	rpc        *rpcclient.Client
	since      time.Time
}

// peerConns is one carrier's idle connections. The zero value is ready.
type peerConns struct {
	mu     sync.Mutex
	closed bool
	idle   []idlePeerConn
	expiry *time.Timer
}

// take hands out the most recently used live connection proven for
// capability that answers a ping, or nil.
func (p *peerConns) take(ctx context.Context, capability string) *rpcclient.Client {
	for {
		rpc := p.takeIdle(capability)
		if rpc == nil {
			return nil
		}
		ping, cancel := context.WithTimeout(ctx, peerPingTimeout)
		err := rpc.Ping(ping)
		cancel()
		if err == nil {
			return rpc
		}
		rpc.Close()
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (p *peerConns) takeIdle(capability string) *rpcclient.Client {
	p.mu.Lock()
	ended := p.pruneLocked()
	var found *rpcclient.Client
	for i := len(p.idle) - 1; i >= 0; i-- {
		if p.idle[i].capability == capability {
			found = p.idle[i].rpc
			p.idle = slices.Delete(p.idle, i, i+1)
			break
		}
	}
	p.mu.Unlock()
	closeAll(ended)
	return found
}

// put keeps a connection whose last call ended with an answer. It is
// closed instead once the carrier has left or enough are already idle.
func (p *peerConns) put(capability string, rpc *rpcclient.Client) {
	p.mu.Lock()
	if p.closed || len(p.idle) >= maxIdlePeerConns || !live(rpc) {
		p.mu.Unlock()
		rpc.Close()
		return
	}
	p.idle = append(p.idle, idlePeerConn{capability: capability, rpc: rpc, since: time.Now()})
	if p.expiry == nil {
		p.expiry = time.AfterFunc(peerConnIdle, p.expire)
	}
	p.mu.Unlock()
}

// expire closes the connections whose idle window has passed and rearms
// for the oldest one left.
func (p *peerConns) expire() {
	p.mu.Lock()
	ended := p.pruneLocked()
	p.expiry = nil
	if len(p.idle) > 0 && !p.closed {
		p.expiry = time.AfterFunc(peerConnIdle-time.Since(p.idle[0].since), p.expire)
	}
	p.mu.Unlock()
	closeAll(ended)
}

// pruneLocked removes the connections that ended or sat out their idle
// window and returns them for the caller to close outside the lock. The
// order, oldest first, is kept.
func (p *peerConns) pruneLocked() []*rpcclient.Client {
	var ended []*rpcclient.Client
	p.idle = slices.DeleteFunc(p.idle, func(held idlePeerConn) bool {
		if live(held.rpc) && time.Since(held.since) < peerConnIdle {
			return false
		}
		ended = append(ended, held.rpc)
		return true
	})
	return ended
}

// close ends every idle connection and refuses later ones. Idempotent.
func (p *peerConns) close() {
	p.mu.Lock()
	p.closed = true
	ended := make([]*rpcclient.Client, 0, len(p.idle))
	for _, held := range p.idle {
		ended = append(ended, held.rpc)
	}
	p.idle = nil
	if p.expiry != nil {
		p.expiry.Stop()
		p.expiry = nil
	}
	p.mu.Unlock()
	closeAll(ended)
}

func live(rpc *rpcclient.Client) bool {
	select {
	case <-rpc.Done():
		return false
	default:
		return true
	}
}

func closeAll(clients []*rpcclient.Client) {
	for _, rpc := range clients {
		rpc.Close()
	}
}
