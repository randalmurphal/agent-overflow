package transport

import "testing"

func TestEventVisibleToOrigin(t *testing.T) {
	for _, channel := range []string{
		"git:status",
		"notification:activated",
		"notification:send",
		"provider:approval",
		"provider:status",
		"provider:queue_flushed",
		"provider:queue_restored",
		"provider:queue_state_changed",
		"provider:background_task_state",
		"provider:user_input",
		"provider:account",
		"provider:session_account",
		"provider:account_usage_error",
		"terminal:exit",
		"terminal:output",
		"provider:terminal_output",
	} {
		if eventVisibleToOrigin(channel, false) {
			t.Fatalf("%s visible to non-loopback peer", channel)
		}
		if !eventVisibleToOrigin(channel, true) {
			t.Fatalf("%s hidden from loopback peer", channel)
		}
	}
	for _, channel := range []string{
		"provider:item_event",
		"provider:usage",
		"provider:session_died",
		// highlight:diff_seed goes everywhere: its persist-time seeds can
		// be parse-primed — better than the loopback RPC recompute — so
		// local clients consume them as in-place cache upgrades.
		"highlight:diff_seed",
	} {
		if !eventVisibleToOrigin(channel, false) {
			t.Fatalf("remote-safe event %s hidden from non-loopback peer", channel)
		}
		if !eventVisibleToOrigin(channel, true) {
			t.Fatalf("remote-safe event %s hidden from loopback peer", channel)
		}
	}
	for _, channel := range []string{
		"highlight:seed",
	} {
		if !eventVisibleToOrigin(channel, false) {
			t.Fatalf("remote-only event %s hidden from non-loopback peer", channel)
		}
		if eventVisibleToOrigin(channel, true) {
			t.Fatalf("remote-only event %s visible to loopback peer", channel)
		}
	}
}

// The payload-less refetch signals are latest-only, as a PAIR: both are
// `emit(name, nil)` from a debounced directory watcher, so a default-depth
// ring would replay up to DefaultRingCapacity identical nil frames on
// reconnect and fire one full-listing refetch per frame. Replay must hand
// back exactly one.
func TestRefetchSignalChannelsAreLatestOnly(t *testing.T) {
	for _, channel := range []string{"theme:changed", "workflow:definitions-changed"} {
		if !latestOnlyEventChannels[channel] {
			t.Fatalf("%s is not latest-only: a reconnect would replay a ring's worth of identical refetch signals", channel)
		}
		if ephemeralEventChannels[channel] {
			t.Fatalf("%s is ephemeral: a client that missed the change would never refetch", channel)
		}

		bus := NewEventBus(10)
		for range 5 {
			if _, err := bus.Emit(channel, nil); err != nil {
				bus.Close()
				t.Fatalf("Emit(%s): %v", channel, err)
			}
		}
		replayed := bus.Replay(map[string]uint64{channel: 0})
		bus.Close()
		if len(replayed) != 1 || replayed[0].Seq != 5 {
			t.Fatalf("%s replay = %+v, want only the newest frame (seq 5)", channel, replayed)
		}
	}
}

// Seed channels are ephemeral: emitted frames reach live subscribers
// with monotonic seqs but are never retained for replay — a reconnect
// gets nothing back and no gap marker (they were never history).
func TestEventBusEphemeralChannelsSkipReplayRetention(t *testing.T) {
	bus := NewEventBus(0)
	defer bus.Close()

	sub := bus.Subscribe()
	defer sub.Close()

	for _, channel := range []string{"highlight:seed", "highlight:diff_seed"} {
		first, err := bus.Emit(channel, map[string]string{"n": "1"})
		if err != nil {
			t.Fatalf("Emit(%s): %v", channel, err)
		}
		second, err := bus.Emit(channel, map[string]string{"n": "2"})
		if err != nil {
			t.Fatalf("Emit(%s): %v", channel, err)
		}
		if first.Seq != 1 || second.Seq != 2 {
			t.Fatalf("%s seqs = %d, %d; want 1, 2", channel, first.Seq, second.Seq)
		}
		// Live delivery still happens.
		for i := 0; i < 2; i++ {
			select {
			case <-sub.Events():
			default:
				t.Fatalf("%s live event %d not delivered", channel, i)
			}
		}
		// Replay from before both events returns nothing — no events,
		// no gap marker.
		if got := bus.Replay(map[string]uint64{channel: 0}); len(got) != 0 {
			t.Fatalf("%s replay = %#v, want empty", channel, got)
		}
	}

	// Sanity: a regular channel still replays.
	if _, err := bus.Emit("provider:usage", map[string]string{"n": "1"}); err != nil {
		t.Fatalf("Emit(regular): %v", err)
	}
	if got := bus.Replay(map[string]uint64{"provider:usage": 0}); len(got) != 1 {
		t.Fatalf("regular channel replay = %#v, want 1 event", got)
	}
}
