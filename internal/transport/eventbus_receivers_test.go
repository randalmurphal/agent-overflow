package transport

import (
	"testing"

	"agent-overflow/internal/eventchan"
)

// RemoteReceiverCount answers "is anybody off this machine in a position
// to use this channel". Work that costs the backend something is gated
// on it (the dev-server scan), so each half of the answer is pinned
// here: a wrong yes makes a desktop-only install poll forever, and a
// wrong no makes the feature silently never start.

func subscribeAs(t *testing.T, bus *EventBus, loopback bool, granted ...string) *Subscriber {
	t.Helper()
	sub := bus.Subscribe()
	t.Cleanup(sub.Close)
	sub.SetOriginLoopback(loopback)
	sub.SetScopeFilter(sessionScopeFilter(granted, false))
	return sub
}

func TestRemoteReceiverCountCountsOnlyOffMachineGrantedSessions(t *testing.T) {
	channel := eventchan.DevServerList.String()
	bus := NewEventBus(8)
	defer bus.Close()

	// The webview in front of the machine: granted, and not remote.
	subscribeAs(t, bus, true, string(ScopePreviewOpen))
	// A remote device that was never granted the capability.
	subscribeAs(t, bus, false, string(ScopeThreadsRead))
	// A subscriber with no origin and no session at all: the harness
	// waiter and everything else that is not a connection.
	plain := bus.Subscribe()
	defer plain.Close()

	if got := bus.RemoteReceiverCount(channel); got != 0 {
		t.Fatalf("RemoteReceiverCount = %d with nobody off-machine granted, want 0", got)
	}

	remote := subscribeAs(t, bus, false, string(ScopePreviewOpen))
	if got := bus.RemoteReceiverCount(channel); got != 1 {
		t.Fatalf("RemoteReceiverCount = %d with one granted remote session, want 1", got)
	}

	// And it goes back down, which is what lets the work stop.
	remote.Close()
	if got := bus.RemoteReceiverCount(channel); got != 0 {
		t.Fatalf("RemoteReceiverCount = %d after the remote session left, want 0", got)
	}
}

// Channel SUBSCRIPTION is deliberately not the signal: an SPA subscriber
// takes every channel by default, so a client that named its channels
// and left this one out is still a client whose grants open it.
func TestRemoteReceiverCountIgnoresChannelSubscription(t *testing.T) {
	channel := eventchan.DevServerList.String()
	bus := NewEventBus(8)
	defer bus.Close()

	sub := subscribeAs(t, bus, false, string(ScopePreviewOpen))
	sub.SetChannels([]string{"thread:updated"})

	if got := bus.RemoteReceiverCount(channel); got != 1 {
		t.Fatalf("RemoteReceiverCount = %d, want 1: the grant is the signal, not the subscription", got)
	}
	if got := bus.ChannelSubscriberCount(channel); got != 0 {
		t.Fatalf("ChannelSubscriberCount = %d, want 0 — the two questions are different", got)
	}
}
