package transport

import (
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
)

// transcriptScopeFilteredChannel is the channel the scope tests narrow,
// read from the table so a reclassification moves these tests with it.
func transcriptScopeFilteredChannel(t *testing.T) string {
	t.Helper()
	names := TranscriptScopeFilteredChannels()
	if len(names) == 0 {
		t.Fatal("no TranscriptScopeFiltered channels in the registry; these tests would pass vacuously")
	}
	return names[0]
}

// entityOnlyChannel is an EntityFiltered channel the scope column does not
// narrow: the control arm for "a scope attribution elsewhere is ignored".
func entityOnlyChannel(t *testing.T) string {
	t.Helper()
	for _, name := range EntityFilteredChannels() {
		if !channelTranscriptScopeFiltered(name) {
			return name
		}
	}
	t.Fatal("every EntityFiltered channel is TranscriptScopeFiltered; the control arm is gone")
	return ""
}

// scopedFrame is one emitted frame's address. Its label is how the tests
// below name what arrived.
type scopedFrame struct{ thread, scope string }

func (f scopedFrame) label() string { return f.thread + "/" + f.scope }

var (
	rootA   = scopedFrame{"thread-A", ""}
	agent1A = scopedFrame{"thread-A", "agent-1"}
	agent2A = scopedFrame{"thread-A", "agent-2"}
	rootB   = scopedFrame{"thread-B", ""}
	agent1B = scopedFrame{"thread-B", "agent-1"}
	// unkeyed is a scoped frame whose thread could not be attributed.
	unkeyed = scopedFrame{"", "agent-1"}

	everyScopedFrame = []scopedFrame{rootA, agent1A, agent2A, rootB, agent1B, unkeyed}
)

// deliveredLabels emits each frame on channel, then a sentinel on a
// wildcard channel, and returns the labels of the frames that arrived ahead
// of the sentinel. Delivery is in emit order, so the sentinel turns "was
// withheld" into a positive observation instead of a timeout.
func deliveredLabels(t *testing.T, bus *EventBus, sub *Subscriber, channel string, frames []scopedFrame) []string {
	t.Helper()
	for _, f := range frames {
		if _, err := bus.EmitScoped(eventchan.Channel(channel), f.thread, f.scope, f.label()); err != nil {
			t.Fatalf("emit %s: %v", f.label(), err)
		}
	}
	if _, err := bus.EmitEntity(wildcardChannel, "", "sentinel"); err != nil {
		t.Fatalf("emit sentinel: %v", err)
	}
	var got []string
	deadline := time.After(2 * time.Second)
	for {
		select {
		case e := <-sub.Events():
			if e.Channel == wildcardChannel {
				return got
			}
			got = append(got, scopedFrame{e.EntityKey, e.EntityScope}.label())
		case <-deadline:
			t.Fatalf("sentinel never arrived; received %v", got)
		}
	}
}

func labels(frames ...scopedFrame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = f.label()
	}
	return out
}

// TestSubscriberScopeWatch is the admission table for a TranscriptScopeFiltered
// channel. A parent pane with no agent open receives its thread's root rows
// and no child rows; opening an agent adds exactly that agent's rows; a
// frame that could not be attributed to a thread reaches everyone.
func TestSubscriberScopeWatch(t *testing.T) {
	channel := transcriptScopeFilteredChannel(t)
	for _, tc := range []struct {
		name string
		// watch is applied before emitting; nil means the connection never
		// sent a watch frame.
		watch func(*Subscriber)
		want  []string
	}{
		{
			name:  "wildcardUntilFirstFrame",
			watch: nil,
			want:  labels(everyScopedFrame...),
		},
		{
			name:  "threadWatcherGetsRootRowsOnly",
			watch: func(s *Subscriber) { s.SetWatch([]string{"thread-A"}, []WatchScope{}) },
			want:  labels(rootA, unkeyed),
		},
		{
			name: "scopeWatcherGetsItsAgentAndNotASibling",
			watch: func(s *Subscriber) {
				s.SetWatch([]string{"thread-A"}, []WatchScope{{ThreadID: "thread-A", ScopeRootID: "agent-1"}})
			},
			// agent1B carries the same scope id under another thread: the
			// pair is the address, not the id.
			want: labels(rootA, agent1A, unkeyed),
		},
		{
			name: "scopeSetWithoutItsThreadStillAdmitsThePair",
			watch: func(s *Subscriber) {
				s.SetWatch(nil, []WatchScope{{ThreadID: "thread-B", ScopeRootID: "agent-1"}})
			},
			want: labels(agent1B, unkeyed),
		},
		{
			name:  "absentScopeSetAdmitsEveryScopeOfWatchedThreads",
			watch: func(s *Subscriber) { s.SetWatch([]string{"thread-A"}, nil) },
			want:  labels(rootA, agent1A, agent2A, unkeyed),
		},
		{
			name:  "emptyWatchWithholdsAllAttributedFrames",
			watch: func(s *Subscriber) { s.SetWatch([]string{}, []WatchScope{}) },
			want:  labels(unkeyed),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus := NewEventBus(20)
			defer bus.Close()
			sub := bus.Subscribe()
			defer sub.Close()
			if tc.watch != nil {
				tc.watch(sub)
			}
			got := deliveredLabels(t, bus, sub, channel, everyScopedFrame)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("delivered %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSubscriberScopeWatchIsAbsolute: each watch replaces the scope set
// with the thread set, so closing an agent's view stops its rows.
func TestSubscriberScopeWatchIsAbsolute(t *testing.T) {
	channel := transcriptScopeFilteredChannel(t)
	bus := NewEventBus(20)
	defer bus.Close()
	sub := bus.Subscribe()
	defer sub.Close()

	sub.SetWatch([]string{"thread-A"}, []WatchScope{{ThreadID: "thread-A", ScopeRootID: "agent-1"}})
	sub.SetWatch([]string{"thread-A"}, []WatchScope{{ThreadID: "thread-A", ScopeRootID: "agent-2"}})
	if got, want := deliveredLabels(t, bus, sub, channel, everyScopedFrame), labels(rootA, agent2A, unkeyed); !slices.Equal(got, want) {
		t.Fatalf("after replacing agent-1 with agent-2: delivered %v, want %v", got, want)
	}

	sub.SetWatch([]string{"thread-A"}, []WatchScope{})
	if got, want := deliveredLabels(t, bus, sub, channel, everyScopedFrame), labels(rootA, unkeyed); !slices.Equal(got, want) {
		t.Fatalf("after closing every agent: delivered %v, want %v", got, want)
	}
}

// TestSubscriberScopeWatchIgnoresScopeOnOtherChannels: the scope column
// narrows only its own rows. A scope attribution on another EntityFiltered
// channel takes the thread rule, and a wildcard channel is untouched.
func TestSubscriberScopeWatchIgnoresScopeOnOtherChannels(t *testing.T) {
	bus := NewEventBus(20)
	defer bus.Close()
	sub := bus.Subscribe()
	defer sub.Close()
	sub.SetWatch([]string{"thread-A"}, []WatchScope{})

	if got, want := deliveredLabels(t, bus, sub, entityOnlyChannel(t), everyScopedFrame), labels(rootA, agent1A, agent2A, unkeyed); !slices.Equal(got, want) {
		t.Fatalf("entity-only channel delivered %v, want the thread rule %v", got, want)
	}
	// On the wildcard channel itself every frame arrives. The sentinel
	// helper shares that channel, so count instead of labelling.
	for _, f := range everyScopedFrame {
		if _, err := bus.EmitScoped(wildcardChannel, f.thread, f.scope, f.label()); err != nil {
			t.Fatalf("emit %s: %v", f.label(), err)
		}
	}
	if got := drainEvents(t, sub, len(everyScopedFrame), time.Second); len(got) != len(everyScopedFrame) {
		t.Fatalf("wildcard channel delivered %d of %d scoped frames", len(got), len(everyScopedFrame))
	}
}

// TestSubscriberScopeWithheldFramesNeverMarkGapped: a scoped frame the
// connection is not viewing is withheld ahead of drop accounting, so a busy
// subagent can never make a parent pane's next frame arrive gap-stamped.
func TestSubscriberScopeWithheldFramesNeverMarkGapped(t *testing.T) {
	channel := transcriptScopeFilteredChannel(t)
	bus := NewEventBus(20)
	defer bus.Close()
	bus.subBuf = 1
	sub := bus.Subscribe()
	defer sub.Close()
	sub.SetWatch([]string{"thread-A"}, []WatchScope{})

	for range 5 {
		if _, err := bus.EmitScoped(eventchan.Channel(channel), "thread-A", "agent-1", "child"); err != nil {
			t.Fatalf("emit child: %v", err)
		}
	}
	if len(sub.gapped) != 0 {
		t.Fatalf("withheld scoped frames marked the channel gapped: %v", sub.gapped)
	}
	if _, err := bus.EmitScoped(eventchan.Channel(channel), "thread-A", "", "root"); err != nil {
		t.Fatalf("emit root: %v", err)
	}
	got := drainEvents(t, sub, 1, time.Second)
	if len(got) != 1 || got[0].EntityScope != "" {
		t.Fatalf("want the root frame, got %+v", got)
	}
	if got[0].Gap {
		t.Fatal("the root frame arrived gap-stamped; a scope withhold is not a loss")
	}
}

// TestSubscriberScopeDropNamesOnlyTheThread: a scoped frame that was
// admitted and then dropped is a real loss, announced under its thread. The
// announcement never names a scope; the client recovers every surface of
// the thread.
func TestSubscriberScopeDropNamesOnlyTheThread(t *testing.T) {
	channel := transcriptScopeFilteredChannel(t)
	bus := NewEventBus(20)
	defer bus.Close()
	bus.subBuf = 1
	sub := bus.Subscribe()
	defer sub.Close()
	sub.SetWatch([]string{"thread-A"}, []WatchScope{{ThreadID: "thread-A", ScopeRootID: "agent-1"}})

	emit := func(scope string) {
		t.Helper()
		if _, err := bus.EmitScoped(eventchan.Channel(channel), "thread-A", scope, scope); err != nil {
			t.Fatalf("emit %q: %v", scope, err)
		}
	}
	emit("")        // fills the buffer
	emit("agent-1") // dropped
	if got := drainEvents(t, sub, 1, time.Second); len(got) != 1 || got[0].Gap {
		t.Fatalf("expected the clean first frame, got %+v", got)
	}
	emit("")
	got := drainEvents(t, sub, 1, time.Second)
	if len(got) != 1 || !got[0].Gap {
		t.Fatalf("expected the stamped frame, got %+v", got)
	}
	if want := []string{"thread-A"}; !slices.Equal(got[0].GapThreads, want) {
		t.Fatalf("GapThreads = %v, want %v", got[0].GapThreads, want)
	}
	if wire := decodedGapThreads(t, got[0]); !slices.Equal(wire, []string{"thread-A"}) {
		t.Fatalf("wire gapThreads = %v, want the thread alone", wire)
	}
}

// TestSubscriberScopeWatchKeepsTheEventsAttribution: the delivered Event
// carries the scope it was emitted with, which is what replay and lease
// coalescing read.
func TestSubscriberScopeWatchKeepsTheEventsAttribution(t *testing.T) {
	channel := transcriptScopeFilteredChannel(t)
	bus := NewEventBus(20)
	defer bus.Close()
	if _, err := bus.EmitScoped(eventchan.Channel(channel), "thread-A", "agent-1", "child"); err != nil {
		t.Fatalf("emit: %v", err)
	}
	replayed := bus.Replay(map[string]uint64{channel: 0})
	if len(replayed) != 1 || replayed[0].EntityKey != "thread-A" || replayed[0].EntityScope != "agent-1" {
		t.Fatalf("ring entry = %+v, want thread-A scoped to agent-1", replayed)
	}
}

// TestTranscriptScopeFilteredChannelsAreEntityFiltered: the scope column
// narrows a channel a watch set already applies to. A row that sets it
// alone would be a narrowing no watch frame could open, so the derived set
// leaves it out and this test names it.
func TestTranscriptScopeFilteredChannelsAreEntityFiltered(t *testing.T) {
	for _, policy := range channelPolicies {
		if policy.TranscriptScopeFiltered && !policy.EntityFiltered {
			t.Errorf("%s is TranscriptScopeFiltered without EntityFiltered", policy.Channel)
		}
	}
}

// TestTranscriptScopeFilteredChannelsMatchTheTable pins the exported list,
// which internal/app reads to decide whether to derive a scope at emit.
func TestTranscriptScopeFilteredChannelsMatchTheTable(t *testing.T) {
	listed := TranscriptScopeFilteredChannels()
	seen := make(map[string]bool, len(listed))
	for _, name := range listed {
		if !channelTranscriptScopeFiltered(name) || !channelEntityFiltered(name) {
			t.Errorf("%s is listed but the registry does not scope-filter it", name)
		}
		seen[name] = true
	}
	for _, policy := range channelPolicies {
		if policy.EntityFiltered && policy.TranscriptScopeFiltered && !seen[string(policy.Channel)] {
			t.Errorf("%s is TranscriptScopeFiltered but missing from TranscriptScopeFilteredChannels()", policy.Channel)
		}
	}
}
