package attachedbackends

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"agent-overflow/internal/rpcclient"
)

// Agent calls to one computer come in bursts: paging a transcript or a
// large tool output is one call per window, and every new connection
// spends a ticket from that computer's credential budget. Periodic checks
// come every few seconds: a thread request's status poll and a running
// remote command's watch ask again five seconds after their last answer.
// A finished call leaves its connection here for the next one instead, and
// the idle window outlasts a periodic check's interval with room for a
// late wake.
//
// Each connection is used by one call at a time, as a fresh one was, so a
// call that is cancelled still ends only its own connection. An idle
// connection stays open on the far side: its reader answers that side's
// keepalive pings. A connection the far side closed (a revoked session, a
// listener rebind) is skipped by its reader's Done rather than handed to a
// call. One whose path died without a close (a network change, a NAT
// rebinding) is caught by a ping before reuse, so the call dials again
// instead of waiting out its own deadline on a socket nothing answers.
const (
	peerConnIdle     = 30 * time.Second
	maxIdlePeerConns = 4
	peerPingTimeout  = 2 * time.Second
)

type idlePeerConn struct {
	capability string
	rpc        *rpcclient.Client
	since      time.Time
}

// peerConns is every connection one carrier opened that is still open: the
// idle ones finished calls left here and the ones calls hold. They belong to
// the pairing the carrier serves, so when the carrier leaves (carrier.leave)
// all of them close, a held one in the middle of its call included, and the
// verdict that ended the pairing is what that call reads. The zero value is
// ready.
type peerConns struct {
	mu          sync.Mutex
	invalidated func(error)
	// ended is the verdict once the carrier has left; no connection is
	// admitted after it.
	ended      error
	launch     string
	generation uint64
	// open is every connection admitted and not yet seen to end, idle or
	// held; idle is the subset waiting for a call.
	open   []*rpcclient.Client
	idle   []idlePeerConn
	expiry *time.Timer
}

// admit records a connection the carrier has just opened. Once the carrier
// has left, the connection is closed instead and the verdict returned.
func (p *peerConns) admit(rpc *rpcclient.Client, generation uint64) error {
	p.mu.Lock()
	if verdict := p.ended; verdict != nil {
		p.mu.Unlock()
		rpc.Close()
		return verdict
	}
	var previous []*rpcclient.Client
	restarted := false
	if launch := rpc.LaunchID(); launch != p.launch {
		if generation != p.generation {
			p.mu.Unlock()
			rpc.Close()
			return errors.New("the paired computer restarted during the handshake")
		}
		restarted = p.launch != ""
		previous = p.open
		p.open, p.idle = nil, nil
		p.launch = launch
		p.generation++
	}
	p.open = slices.DeleteFunc(p.open, func(held *rpcclient.Client) bool { return !live(held) })
	p.open = append(p.open, rpc)
	p.mu.Unlock()
	if restarted && p.invalidated != nil {
		p.invalidated(errors.New("the paired computer restarted"))
	}
	closeAll(previous)
	return nil
}

// verdict is why the carrier left, or nil while it serves.
func (p *peerConns) verdict() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ended
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
	if p.ended != nil || p.launch != rpc.LaunchID() || len(p.idle) >= maxIdlePeerConns || !live(rpc) {
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
	if len(p.idle) > 0 && p.ended == nil {
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

// close ends every connection, idle or held, records verdict as the
// reason and admits no other. It does not wait for the calls on them: each
// fails on its own once its connection closes. Idempotent; the first
// verdict stands.
func (p *peerConns) close(verdict error) {
	p.mu.Lock()
	first := p.ended == nil
	if first {
		p.ended = verdict
	}
	ended := p.open
	p.open, p.idle = nil, nil
	if p.expiry != nil {
		p.expiry.Stop()
		p.expiry = nil
	}
	p.mu.Unlock()
	if first && p.invalidated != nil {
		p.invalidated(verdict)
	}
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
