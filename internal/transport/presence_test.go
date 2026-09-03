package transport

import "testing"

// localScreen registers a subscriber on the given origin and states its
// presence, returning it so a test can close it.
func localScreen(t *testing.T, bus *EventBus, loopback, focused bool, threads ...string) *Subscriber {
	t.Helper()
	sub := bus.Subscribe()
	sub.SetOriginLoopback(loopback)
	sub.SetPresence(focused, threads)
	return sub
}

// A subscriber that never stated a presence is NOT attended. That is what
// every client predating the frame is, and it is what keeps the frame
// additive: before it existed every notification was raised.
func TestLocalScreenPresenceIsUnattendedUntilStated(t *testing.T) {
	bus := NewEventBus(0)
	defer bus.Close()
	sub := bus.Subscribe()
	sub.SetOriginLoopback(true)

	focused, visible := bus.LocalScreenPresence("thread-a")
	if focused || visible {
		t.Fatalf("a connection that stated nothing reads as attended (focused=%t visible=%t)", focused, visible)
	}
}

// The whole point of the loopback filter: a remote screen is somebody
// else's, and its focus must never silence the machine this backend
// interrupts.
func TestLocalScreenPresenceIgnoresRemoteAndOriginlessSubscribers(t *testing.T) {
	bus := NewEventBus(0)
	defer bus.Close()
	localScreen(t, bus, false, true, "thread-a")
	// No origin recorded at all: the harness waiter and every other
	// non-connection subscriber. Not a screen.
	originless := bus.Subscribe()
	originless.SetPresence(true, []string{"thread-a"})

	focused, visible := bus.LocalScreenPresence("thread-a")
	if focused || visible {
		t.Fatalf("a remote or originless subscriber counted as this machine's screen (focused=%t visible=%t)",
			focused, visible)
	}
}

// ORed over the machine's own connections: the embedded webview and a
// --connect tab beside it are two screens, and either one being looked at
// is a person looking.
func TestLocalScreenPresenceOrsOverLocalSubscribers(t *testing.T) {
	bus := NewEventBus(0)
	defer bus.Close()
	localScreen(t, bus, true, false, "thread-a")
	localScreen(t, bus, true, true)

	focused, visible := bus.LocalScreenPresence("thread-a")
	if !focused {
		t.Error("one focused local connection must make the screen focused")
	}
	if !visible {
		t.Error("one local connection showing the thread must make it visible")
	}
	if _, otherVisible := bus.LocalScreenPresence("thread-b"); otherVisible {
		t.Error("a thread nobody is showing reads as visible")
	}
}

// An empty thread id asks only about focus, which is what a notification
// with no thread behind it (a signed-out provider, an update notice, a
// workflow item) has to ask.
func TestLocalScreenPresenceWithNoThreadAnswersFocusOnly(t *testing.T) {
	bus := NewEventBus(0)
	defer bus.Close()
	localScreen(t, bus, true, true, "thread-a")

	focused, visible := bus.LocalScreenPresence("")
	if !focused {
		t.Error("focus must be answered without a thread id")
	}
	if visible {
		t.Error("an empty thread id must never report a visible thread")
	}
}

// Not a latch: each frame replaces both halves together, so a client that
// blurs and closes its pane stops being attended on the next frame rather
// than keeping whichever half it stopped restating.
func TestSetPresenceReplacesBothHalves(t *testing.T) {
	bus := NewEventBus(0)
	defer bus.Close()
	sub := localScreen(t, bus, true, true, "thread-a")

	sub.SetPresence(false, nil)
	if focused, visible := bus.LocalScreenPresence("thread-a"); focused || visible {
		t.Fatalf("presence latched (focused=%t visible=%t)", focused, visible)
	}

	sub.SetPresence(false, []string{"thread-b"})
	if focused, visible := bus.LocalScreenPresence("thread-b"); focused || !visible {
		t.Fatalf("restated presence not applied (focused=%t visible=%t)", focused, visible)
	}
}

// The presence dies with the socket, so a closed laptop lid stops being a
// screen rather than stranding notifications behind a client that is gone.
func TestLocalScreenPresenceDropsWithTheSubscriber(t *testing.T) {
	bus := NewEventBus(0)
	defer bus.Close()
	sub := localScreen(t, bus, true, true, "thread-a")

	sub.Close()
	if focused, visible := bus.LocalScreenPresence("thread-a"); focused || visible {
		t.Fatalf("a departed subscriber still reads as a screen (focused=%t visible=%t)", focused, visible)
	}
}
