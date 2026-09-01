package transport

import (
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
)

// entityFilteredChannel and wildcardChannel are the two shapes every test
// below needs: one channel the registry narrows and one it does not. Read
// from the table rather than spelled, so a reclassification moves these
// tests with it instead of leaving them silently testing nothing.
func entityFilteredChannel(t *testing.T) string {
	t.Helper()
	names := EntityFilteredChannels()
	if len(names) == 0 {
		t.Fatal("no EntityFiltered channels in the registry; these tests would pass vacuously")
	}
	return names[0]
}

const wildcardChannel = "provider:item_event"

// TestSubscriberWatchesWildcardUntilFirstFrame pins the property every
// non-SPA client in the tree depends on: a subscriber that never had a
// watch set receives entity-keyed frames exactly as it always did. The WSL
// launcher's notification client, ao-harness and the workflow waiter all
// live on this branch and none of them speaks the frame.
func TestSubscriberWatchesWildcardUntilFirstFrame(t *testing.T) {
	filtered := entityFilteredChannel(t)
	bus := NewEventBus(10)
	defer bus.Close()
	sub := bus.Subscribe()
	defer sub.Close()

	if _, err := bus.EmitEntity(eventchan.Channel(filtered), "thread-A", "x"); err != nil {
		t.Fatalf("emit: %v", err)
	}
	got := drainEvents(t, sub, 1, time.Second)
	if len(got) != 1 || got[0].EntityKey != "thread-A" {
		t.Fatalf("wildcard subscriber did not receive the keyed frame: %+v", got)
	}
}

// TestSubscriberWatchNarrowsOnlyEntityFilteredChannels is the core of the
// column: the SAME foreign entity key is withheld on a filtered channel and
// delivered on every other one. That second half is what keeps the sidebar,
// the tray and the thread-status projections reading threads no pane shows.
func TestSubscriberWatchNarrowsOnlyEntityFilteredChannels(t *testing.T) {
	filtered := entityFilteredChannel(t)
	bus := NewEventBus(10)
	defer bus.Close()
	sub := bus.Subscribe()
	defer sub.Close()
	sub.SetWatchedThreads([]string{"thread-A"})

	if _, err := bus.EmitEntity(eventchan.Channel(filtered), "thread-B", "withheld"); err != nil {
		t.Fatalf("emit filtered foreign: %v", err)
	}
	if _, err := bus.EmitEntity(wildcardChannel, "thread-B", "delivered"); err != nil {
		t.Fatalf("emit wildcard foreign: %v", err)
	}
	if _, err := bus.EmitEntity(eventchan.Channel(filtered), "thread-A", "watched"); err != nil {
		t.Fatalf("emit filtered watched: %v", err)
	}

	got := drainEvents(t, sub, 2, time.Second)
	if len(got) != 2 {
		t.Fatalf("want 2 delivered frames, got %d: %+v", len(got), got)
	}
	if got[0].Channel != wildcardChannel {
		t.Errorf("first delivered frame was %s; a wildcard channel must not be narrowed", got[0].Channel)
	}
	if got[1].Channel != filtered || got[1].EntityKey != "thread-A" {
		t.Errorf("second delivered frame was %+v; want the watched entity on %s", got[1], filtered)
	}
}

// TestSubscriberWatchDeliversUnattributedFrames pins the fail-open
// direction. The key comes from a best-effort payload extractor, so an
// empty one means "could not attribute", never "belongs to nobody" — and a
// payload-shape change must degrade to wasted bytes rather than to frames
// that silently stop rendering.
func TestSubscriberWatchDeliversUnattributedFrames(t *testing.T) {
	filtered := entityFilteredChannel(t)
	bus := NewEventBus(10)
	defer bus.Close()
	sub := bus.Subscribe()
	defer sub.Close()
	sub.SetWatchedThreads([]string{"thread-A"})

	if _, err := bus.EmitEntity(eventchan.Channel(filtered), "", "unattributed"); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got := drainEvents(t, sub, 1, time.Second); len(got) != 1 {
		t.Fatalf("an unattributed frame on a filtered channel must be delivered, got %+v", got)
	}
}

// TestSubscriberWatchAcceptsTheEmptySet pins that "no panes open" is a
// state a client can express. An empty set is NOT the same as never having
// sent a frame, and conflating them would make a client with everything
// closed silently receive the whole stream.
func TestSubscriberWatchAcceptsTheEmptySet(t *testing.T) {
	filtered := entityFilteredChannel(t)
	bus := NewEventBus(10)
	defer bus.Close()
	sub := bus.Subscribe()
	defer sub.Close()
	sub.SetWatchedThreads(nil)

	if _, err := bus.EmitEntity(eventchan.Channel(filtered), "thread-A", "withheld"); err != nil {
		t.Fatalf("emit filtered: %v", err)
	}
	if _, err := bus.EmitEntity(wildcardChannel, "thread-A", "delivered"); err != nil {
		t.Fatalf("emit wildcard: %v", err)
	}
	got := drainEvents(t, sub, 1, time.Second)
	if len(got) != 1 || got[0].Channel != wildcardChannel {
		t.Fatalf("empty watch set must withhold the filtered channel and nothing else, got %+v", got)
	}
}

// TestSubscriberWatchIsAbsoluteAndIdempotent: each frame REPLACES the set
// (not merges into it), and re-sending one changes nothing.
func TestSubscriberWatchIsAbsoluteAndIdempotent(t *testing.T) {
	filtered := entityFilteredChannel(t)
	bus := NewEventBus(10)
	defer bus.Close()
	sub := bus.Subscribe()
	defer sub.Close()

	sub.SetWatchedThreads([]string{"thread-A"})
	sub.SetWatchedThreads([]string{"thread-B"})
	sub.SetWatchedThreads([]string{"thread-B"})

	if _, err := bus.EmitEntity(eventchan.Channel(filtered), "thread-A", "gone"); err != nil {
		t.Fatalf("emit A: %v", err)
	}
	if _, err := bus.EmitEntity(eventchan.Channel(filtered), "thread-B", "kept"); err != nil {
		t.Fatalf("emit B: %v", err)
	}
	got := drainEvents(t, sub, 1, time.Second)
	if len(got) != 1 || got[0].EntityKey != "thread-B" {
		t.Fatalf("the newest set must be the whole answer, got %+v", got)
	}
}

// TestSubscriberWatchWithheldFramesNeverMarkGapped is the property that
// makes server-side withholding safe at all. A withheld frame is not a
// dropped one: flagging the channel would make the NEXT watched frame
// arrive Gap:true, and the client answers a gap with a full resync — so
// every foreign frame would become a resync on a busy backend.
func TestSubscriberWatchWithheldFramesNeverMarkGapped(t *testing.T) {
	filtered := entityFilteredChannel(t)
	bus := NewEventBus(10)
	defer bus.Close()
	// A buffer of one: without the withhold running ahead of the drop
	// path, the foreign frames below would fill it and flag the channel.
	bus.subBuf = 1
	sub := bus.Subscribe()
	defer sub.Close()
	sub.SetWatchedThreads([]string{"thread-A"})

	for range 5 {
		if _, err := bus.EmitEntity(eventchan.Channel(filtered), "thread-B", "foreign"); err != nil {
			t.Fatalf("emit foreign: %v", err)
		}
	}
	if len(sub.gapped) != 0 {
		t.Fatalf("withheld frames marked the channel gapped: %v", sub.gapped)
	}
	if _, err := bus.EmitEntity(eventchan.Channel(filtered), "thread-A", "watched"); err != nil {
		t.Fatalf("emit watched: %v", err)
	}
	got := drainEvents(t, sub, 1, time.Second)
	if len(got) != 1 {
		t.Fatalf("want the watched frame, got %+v", got)
	}
	if got[0].Gap {
		t.Fatalf("the watched frame arrived gap-stamped; withholding must not announce a loss")
	}
}

// TestSubscriberWatchComposesWithOriginAndScopeFilters: the watch set is a
// third narrowing, not a replacement for the other two. A frame the origin
// or grant filter refuses stays refused however the watch set reads.
func TestSubscriberWatchComposesWithOriginAndScopeFilters(t *testing.T) {
	// highlight:seed is AudienceRemoteOnly and ScopeFilesRead, so a
	// loopback origin and a grantless session each refuse it on their own.
	const filtered = "highlight:seed"
	if !channelEntityFiltered(filtered) {
		t.Skipf("%s is no longer entity-filtered; pick another composed case", filtered)
	}

	t.Run("originStillRefuses", func(t *testing.T) {
		bus := NewEventBus(10)
		defer bus.Close()
		sub := bus.Subscribe()
		defer sub.Close()
		sub.SetOriginLoopback(true)
		sub.SetWatchedThreads([]string{"thread-A"})
		if _, err := bus.EmitEntity(filtered, "thread-A", "x"); err != nil {
			t.Fatalf("emit: %v", err)
		}
		if got := drainEvents(t, sub, 1, 150*time.Millisecond); len(got) != 0 {
			t.Fatalf("a watched entity must not defeat the origin filter, got %+v", got)
		}
	})

	t.Run("scopeStillRefuses", func(t *testing.T) {
		bus := NewEventBus(10)
		defer bus.Close()
		sub := bus.Subscribe()
		defer sub.Close()
		sub.SetScopeFilter(sessionScopeFilter(nil, false))
		sub.SetWatchedThreads([]string{"thread-A"})
		if _, err := bus.EmitEntity(filtered, "thread-A", "x"); err != nil {
			t.Fatalf("emit: %v", err)
		}
		if got := drainEvents(t, sub, 1, 150*time.Millisecond); len(got) != 0 {
			t.Fatalf("a watched entity must not defeat the scope filter, got %+v", got)
		}
	})
}

// TestWatchingConnectionIsNotAChannelSubscriber pins the invariant that
// keeps app_notifications.go's "no connected launcher subscriber"
// diagnostic sound: watching entities must not register a connection as a
// channel subscriber. The two frames are separate for exactly this reason.
func TestWatchingConnectionIsNotAChannelSubscriber(t *testing.T) {
	filtered := entityFilteredChannel(t)
	bus := NewEventBus(10)
	defer bus.Close()
	sub := bus.Subscribe()
	defer sub.Close()
	sub.SetWatchedThreads([]string{"thread-A"})

	if got := bus.ChannelSubscriberCount(filtered); got != 0 {
		t.Fatalf("a watching connection counted as %d channel subscribers, want 0", got)
	}
	if got := bus.ChannelSubscriberCount("notification:send"); got != 0 {
		t.Fatalf("a watching connection counted as %d notification subscribers, want 0", got)
	}
}

// TestEntityFilteredChannelsMatchTheTable pins the exported list against
// the authored column, since internal/app and the frontend drift guard both
// read it rather than the table.
func TestEntityFilteredChannelsMatchTheTable(t *testing.T) {
	listed := EntityFilteredChannels()
	seen := make(map[string]bool, len(listed))
	for _, name := range listed {
		if !channelEntityFiltered(name) {
			t.Errorf("%s is listed but the registry does not filter it", name)
		}
		seen[name] = true
	}
	for _, policy := range channelPolicies {
		if policy.EntityFiltered && !seen[string(policy.Channel)] {
			t.Errorf("%s is EntityFiltered but missing from EntityFilteredChannels()", policy.Channel)
		}
	}
}

// TestEntityFilteredChannelsAreThreadKeyed pins the one payload assumption
// the whole mechanism rests on: every filtered channel's events must carry
// a `threadId` the emit-side extractor can find, or the fail-open branch
// swallows the narrowing and the column becomes a no-op nobody notices.
// The wire event structs live in internal/app, so this checks the
// registry's half — that each member is a channel internal/app emits
// through the keyed funnel — by name.
func TestEntityFilteredChannelsAreThreadKeyed(t *testing.T) {
	for _, name := range EntityFilteredChannels() {
		policy, registered := policyForChannel(name)
		if !registered {
			t.Errorf("%s is filtered but unregistered; an unregistered channel must never be narrowed", name)
			continue
		}
		if policy.Why == "" {
			t.Errorf("%s carries no Why; an EntityFiltered row is a claim about its consumers and has to argue it", name)
		}
	}
}
