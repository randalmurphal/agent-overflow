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
//
// # Scopes
//
// A TranscriptScopeFiltered channel (provider:item_event) is narrowed one level
// further. Each frame carries a second attribution, Event.EntityScope: the
// `parentId` of the timeline row it is about, derived once at emit beside
// the entity key. Empty means a root-scope row. A watch frame names, besides
// its threads, the (thread, scope root) pairs the connection is viewing
// (frame.go WatchScope). A root-scope frame is admitted by a connection that
// watches its thread; a scoped frame only by one that watches its pair, or
// that names its thread whole (frame.go ClientFrame.ScopeThreads, the
// compact form of a set past MaxWatchScopes). So a connection showing a
// parent thread's pane receives that thread's top-level rows and none of
// its subagents' rows until it opens a surface for one.
//
// The three properties above hold for scopes, restated:
//
//   - **Wildcard until stated.** A connection that never sent a watch frame
//     receives everything, and one whose watch frame carries no scope set
//     (a client that does not narrow by scope) receives every scope of the
//     threads it watches.
//   - **An empty scope attribution is DELIVERED to the thread's watchers.**
//     It means a root-scope row or a payload the extractor could not
//     attribute, and both read as root scope. An empty ENTITY key still
//     delivers to everyone, whatever the frame's scope.
//   - **Only TranscriptScopeFiltered channels are narrowed by scope.**
//     Every other EntityFiltered channel keeps the thread rule; a scope
//     attribution on one of them is ignored rather than trusted.
//
// Scope withholding runs where thread withholding runs (deliver, and
// handleReplay), ahead of gap accounting, so a withheld scoped frame is
// never a loss. Gap attribution stays keyed by thread: a dropped scoped
// frame names its thread in `gapThreads`, and the client recovers every
// surface of that thread, scoped ones included.

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

// transcriptScopeFilteredEventChannels is the TranscriptScopeFiltered
// column as a set, derived the same way and for the same hot-path reason as
// the entity set above. A row that sets TranscriptScopeFiltered without
// EntityFiltered is left out, so the column cannot narrow a channel no
// watch set applies to (TestTranscriptScopeFilteredChannelsAreEntityFiltered
// fails on such a row).
var transcriptScopeFilteredEventChannels = func() map[string]bool {
	set := make(map[string]bool)
	for _, policy := range channelPolicies {
		if policy.EntityFiltered && policy.TranscriptScopeFiltered {
			set[string(policy.Channel)] = true
		}
	}
	return set
}()

// channelTranscriptScopeFiltered reports whether a watch filter narrows
// this channel by transcript scope as well as by thread.
func channelTranscriptScopeFiltered(channel string) bool {
	return transcriptScopeFilteredEventChannels[channel]
}

// TranscriptScopeFilteredChannels returns the wire names of every
// TranscriptScopeFiltered channel, in table order. internal/app reads it to
// decide at emit time whether a frame's scope attribution is worth deriving.
func TranscriptScopeFilteredChannels() []string {
	out := make([]string, 0, len(transcriptScopeFilteredEventChannels))
	for _, policy := range channelPolicies {
		if transcriptScopeFilteredEventChannels[string(policy.Channel)] {
			out = append(out, string(policy.Channel))
		}
	}
	return out
}

// subscriberWatchFilter is a connection's watch set. Same lifecycle as
// subscriberChannelFilter: replaced wholesale under an atomic pointer, never
// mutated in place, so the hot path reads it without a lock.
type subscriberWatchFilter struct {
	threads map[string]struct{}
	// scopes is nil when the watch frame stated no scope set, which admits
	// every scope of a watched thread; empty when it stated none viewed.
	scopes map[WatchScope]struct{}
	// scopeThreads are the threads a stated set admits every scope of.
	scopeThreads map[string]struct{}
}

// admits applies the set to one frame on an EntityFiltered channel with a
// non-empty entity key. A root-scope frame, and any frame on a channel that
// is not TranscriptScopeFiltered, takes the thread rule; a scoped frame
// needs its (thread, scope) pair or its thread named whole, unless the set
// stated no scopes.
func (f *subscriberWatchFilter) admits(channel, entityKey, entityScope string) bool {
	if entityScope == "" || f.scopes == nil || !channelTranscriptScopeFiltered(channel) {
		_, ok := f.threads[entityKey]
		return ok
	}
	if _, ok := f.scopeThreads[entityKey]; ok {
		return true
	}
	_, ok := f.scopes[WatchScope{ThreadID: entityKey, ScopeRootID: entityScope}]
	return ok
}
