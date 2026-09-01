package transport

// Per-thread subscription narrowing (docs/specs/remote-access.md §9).
//
// A connection may name the entities it is looking at with a `watch` frame
// (frame.go, conn.go handleWatch). Frames on an EntityFiltered channel that
// are addressed to some OTHER entity are then withheld from it — before gap
// accounting, so a withheld frame is not a loss and never marks the channel
// gapped.
//
// Three properties make this safe to bolt onto a live stream, and all three
// are load-bearing:
//
//   - **Wildcard until the first frame.** A connection that never sends one
//     keeps today's behavior exactly. That is the embedded webview before
//     its first pane, the WSL launcher's notification client, `ao-harness`,
//     and every Go client in the tree; none of them is asked to learn a new
//     frame to keep working.
//   - **An empty entity key is DELIVERED.** The key is derived from the
//     payload by a best-effort extractor (internal/eventscope), so "no key"
//     means "this frame could not be attributed", not "this frame belongs to
//     nobody". Withholding on a failed extraction would make an unrelated
//     payload-shape change silently delete frames; fail-open keeps that
//     failure a wire-cost regression instead of a rendering bug.
//   - **Only EntityFiltered channels are narrowed**, and that column is a
//     claim about the frontend consumers, argued per row in
//     event_channels.go. Everything else is untouched whatever the watch set
//     says, which is what keeps the sidebar, the tray and the status
//     projections reading threads no pane is showing.
//
// The set is derived from pane EXISTENCE on the client side and from nothing
// else. It is deliberately not a visibility signal: an off-screen pane, a
// hidden document and a backgrounded tab all keep watching, because a pane
// that stops receiving is a pane that renders wrongly when it is looked at
// again.

// entityFilteredEventChannels is the EntityFiltered column as a set, derived
// at init from the one authored table — do not add an entry here, set the
// column on a ChannelPolicy row instead.
//
// A map for the reason event_visibility.go's two sets are maps: this is a
// hot-path probe (per event, per subscriber, and again per replayed event
// per connection) and going through ChannelPolicy would copy two string
// headers per frame. Keyed by the wire spelling, since the caller is handed
// Event.Channel.
var entityFilteredEventChannels = func() map[string]bool {
	set := make(map[string]bool)
	for _, policy := range channelPolicies {
		if policy.EntityFiltered {
			set[string(policy.Channel)] = true
		}
	}
	return set
}()

// channelEntityFiltered reports whether a watch filter narrows this channel.
// A channel with no registry row is not filtered: the fail-closed default
// already withholds it from every remote peer, and narrowing a channel
// nobody classified would be a second silent filter on top of that one.
func channelEntityFiltered(channel string) bool {
	return entityFilteredEventChannels[channel]
}

// EntityFilteredChannels returns the wire names of every entity-filtered
// channel, in table order.
//
// Exported for the two callers that need the same list and cannot read the
// table: internal/app decides at emit time whether an event's entity key is
// worth deriving, and the drift guard pins
// frontend/src/lib/transport/entityFilteredChannels.ts against this one. The
// SPA needs the list because it must exempt exactly these channels from its
// forward-skip loss heuristic while a watch filter is armed — a withheld
// frame advances the channel's seq without arriving, which that heuristic
// would otherwise read as dropped events and answer with a full resync.
func EntityFilteredChannels() []string {
	out := make([]string, 0, len(entityFilteredEventChannels))
	for _, policy := range channelPolicies {
		if policy.EntityFiltered {
			out = append(out, string(policy.Channel))
		}
	}
	return out
}

// subscriberWatchFilter is a connection's watched-entity set. Same shape and
// same lifecycle as subscriberChannelFilter: replaced wholesale under an
// atomic pointer, never mutated in place, so the hot path reads it without a
// lock.
type subscriberWatchFilter map[string]struct{}
