package transport

// Per-connection SCREEN PRESENCE: what the person on the other end can
// already see (docs/specs/remote-access.md §9, "Notification semantics").
//
// A client states two facts with a `presence` frame (frame.go, conn.go
// handlePresence): whether its window has focus, and which threads are on
// screen right now. The backend keeps them on the Subscriber and reads them
// from ONE place — the OS-notification gate in `internal/app` — to answer a
// single question: is this screen already looking at the thing I was about
// to interrupt it about?
//
// Four properties, and every one of them is load-bearing:
//
//   - **It changes what is RAISED, never what is SENT.** Nothing on the
//     delivery path consults it: not the channel filter, not the entity
//     filter, not the lease, not gap accounting. Off-view work shedding is
//     a rejected design in this codebase for the reason event_entity.go
//     gives about panes — a surface that stops receiving renders wrongly
//     the moment it is looked at — and this frame is not a way back to it.
//     A notification that is not raised is one less toast; a frame that is
//     not sent is a resync the user waits through.
//   - **Not a latch.** Each frame REPLACES the last, both halves together,
//     because they describe one instant. A connection that never sends one
//     is "not attended" (unfocused, nothing on screen), which is what every
//     client that does not speak this frame is and what makes the frame
//     purely additive: before it existed every notification was raised, and
//     a backend that hears nothing raises every notification still.
//   - **Only the LOCAL screen counts.** LocalScreenPresence ORs over
//     subscribers on a loopback origin, because the screen this backend
//     interrupts is its own machine's: in-process on macOS and Linux,
//     through the Windows launcher on WSL — and the WSL launcher's WebView2
//     reaches the backend over WSL2's localhost forwarding, so it arrives on
//     the loopback interface like every other same-machine client. A phone
//     across the room being focused must never silence the desk, and a
//     remote browser's presence is that browser's own business.
//   - **It dies with the socket.** The state lives on the Subscriber and
//     nothing outlives it, so a client that closes its laptop lid stops
//     being attended by disconnecting; there is no stale "somebody is
//     watching" to strand a notification behind.

// subscriberPresence is one connection's last stated presence: an immutable
// snapshot swapped in whole, so a reader on the notification queue never
// sees a focus bit from one frame beside a thread set from another.
type subscriberPresence struct {
	focused bool
	// threads is the on-screen set. A map because the read is a membership
	// test for one thread id; a screen holds a handful of panes, so the map
	// is small and built once per frame rather than per read.
	threads map[string]struct{}
}
