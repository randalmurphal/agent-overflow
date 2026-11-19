package attachedbackends

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/transport"
)

// TestEveryConnectionCheckedOutOfAPairingEndsWithIt: however a pairing's
// carrier leaves, the connections calls hold close with the idle ones. A
// call made after it fails unsent with the pairing's verdict; one already in
// flight fails unanswered. Nothing is dialed under that pairing afterwards.
func TestEveryConnectionCheckedOutOfAPairingEndsWithIt(t *testing.T) {
	for _, end := range []struct {
		name  string
		leave func(t *testing.T, m *Manager, id string)
		code  string
	}{
		{"forgotten", func(t *testing.T, m *Manager, id string) {
			if err := m.Remove(id); err != nil {
				t.Error(err)
			}
		}, "remote_not_paired"},
		{"ended by the far side", func(t *testing.T, m *Manager, id string) {
			held, err := m.carrier(context.Background(), id)
			if err != nil {
				t.Error(err)
				return
			}
			held.client.Retire()
			if !held.ended(deviceclient.ErrSessionEnded) {
				t.Error("the verdict was not taken")
			}
		}, "remote_pairing_expired"},
		{"replaced on disk", func(t *testing.T, m *Manager, id string) {
			held, err := m.carrier(context.Background(), id)
			if err != nil {
				t.Error(err)
				return
			}
			held.client.Retire()
			if again, err := m.carrier(context.Background(), id); err != nil || again == held {
				t.Errorf("the lookup after a retirement = %v, %v; want the profile read again", again, err)
			}
		}, "remote_pairing_replaced"},
		{"closed", func(_ *testing.T, m *Manager, _ string) { m.Close() }, "remote_pairing_unavailable"},
	} {
		t.Run(end.name, func(t *testing.T) {
			m, p := newThreadPeerManager(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// A call that never gets its answer holds one connection.
			hung := make(chan error, 1)
			go func() { hung <- m.CallThreadPeer(ctx, p.id, "ThreadToolCall", nil) }()
			waitFor(t, "the hung call's connection", func() bool { return p.opened.Load() == 1 })
			err := m.HoldThreadPeer(ctx, p.id, func(call PeerCall) error {
				var held int32
				if err := call(ctx, "ThreadToolQuery", &held); err != nil {
					return err
				}
				end.leave(t, m, p.id)
				closed := map[int32]bool{p.nextClosed(t): true, p.nextClosed(t): true}
				if !closed[1] || !closed[held] {
					return fmt.Errorf("closed connections %v, want the hung call's 1 and the held %d", closed, held)
				}
				return call(ctx, "ThreadToolQuery", nil)
			})
			// The call made after the pairing ended is refused unsent, with
			// its verdict. The one in flight may have reached the computer,
			// so it reads as unanswered, never as a refusal that says it was
			// not sent.
			if code, _, _ := errorsx.PublicDetails(err); code != end.code {
				t.Fatalf("the held call after the pairing was %s = %v, want %s", end.name, err, end.code)
			}
			if err := <-hung; !Unanswered(err) {
				t.Fatalf("the hung call after the pairing was %s = %v, want it unanswered", end.name, err)
			}
			if got := p.opened.Load(); got != 2 {
				t.Fatalf("%d connections opened, want none after the pairing was %s", got, end.name)
			}
		})
	}
}

// TestAConnectionThatOpensAfterItsPairingEndedIsClosed: a dial already past
// its ticket when the carrier leaves finishes after it, and is closed then
// rather than handed to its caller.
func TestAConnectionThatOpensAfterItsPairingEndedIsClosed(t *testing.T) {
	m, p := newThreadPeerManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	held, err := m.carrier(ctx, p.id)
	if err != nil {
		t.Fatal(err)
	}
	// The owner still authorizes, as it does until a replacement retires it.
	held.leave(errPairingReplaced)
	if rpc, err := held.openRPC(ctx, transport.CapabilityThreadTools); !errors.Is(err, errPairingReplaced) {
		t.Fatalf("a connection opened after the carrier left = %v, %v; want the verdict", rpc, err)
	}
	if got := p.nextClosed(t); got != 1 {
		t.Fatalf("closed connection %d, want the one opened after the carrier left", got)
	}
}
