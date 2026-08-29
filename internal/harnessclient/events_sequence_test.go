package harnessclient

import (
	"testing"
)

func TestSequenceObservationsArePerChannel(t *testing.T) {
	c := &Client{sequences: make(map[string]channelSequence)}
	c.dispatch(Event{Channel: "a", Seq: 4})
	c.dispatch(Event{Channel: "b", Seq: 99})
	c.dispatch(Event{Channel: "a", Seq: 5})
	c.dispatch(Event{Channel: "b", Seq: 100})
	if got := c.SequenceGaps(); len(got) != 0 {
		t.Fatalf("interleaved channels fabricated gaps: %+v", got)
	}
}

func TestSequenceObservationsReportForwardGapsAndRewinds(t *testing.T) {
	c := &Client{sequences: make(map[string]channelSequence)}
	c.dispatch(Event{Channel: "stream", Seq: 7})
	c.dispatch(Event{Channel: "stream", Seq: 9})
	c.dispatch(Event{Channel: "stream", Seq: 9})
	c.dispatch(Event{Channel: "stream", Seq: 8})
	gaps := c.SequenceGaps()
	if len(gaps) != 1 || gaps[0].Expected != 8 || gaps[0].Observed != 9 {
		t.Fatalf("gaps = %+v, want 8 -> 9", gaps)
	}
	faults := c.SequenceFaults()
	if len(faults) != 2 || faults[0].Previous != 9 || faults[1].Observed != 8 {
		t.Fatalf("faults = %+v, want duplicate and rewind", faults)
	}
}

func TestReplayGapMarkerEstablishesNewCursor(t *testing.T) {
	c := &Client{sequences: make(map[string]channelSequence)}
	c.dispatch(Event{Channel: "stream", Seq: 2})
	c.dispatch(Event{Channel: "stream", Seq: 10, Gap: true})
	c.dispatch(Event{Channel: "stream", Seq: 11})
	if got := c.SequenceFaults(); len(got) != 0 {
		t.Fatalf("replay gap did not establish the new cursor: %+v", got)
	}
	gaps := c.SequenceGaps()
	if len(gaps) != 1 || !gaps[0].Replay || gaps[0].Expected != 3 {
		t.Fatalf("replay gaps = %+v, want explicit marker expected 3", gaps)
	}
}

func TestReplayAndLiveInterleavingDoesNotCreateRewindFaults(t *testing.T) {
	c := &Client{
		sequences:      make(map[string]channelSequence),
		replayChannels: map[string]map[string]struct{}{"replay-1": {"stream": {}}},
	}
	c.dispatch(Event{Channel: "stream", Seq: 5})
	// A live frame overtakes the replay backlog, then the older replayed
	// frames arrive. The wire permits this ordering.
	c.dispatch(Event{Channel: "stream", Seq: 7})
	c.dispatch(Event{Channel: "stream", Seq: 6})
	c.dispatch(Event{Channel: "stream", Seq: 8})
	if got := c.SequenceGaps(); len(got) != 0 {
		t.Fatalf("replay/live interleaving fabricated gaps: %+v", got)
	}
	if got := c.SequenceFaults(); len(got) != 0 {
		t.Fatalf("replay/live interleaving fabricated faults: %+v", got)
	}
	c.mu.Lock()
	delete(c.replayChannels, "replay-1")
	c.mu.Unlock()
	c.dispatch(Event{Channel: "stream", Seq: 9})
	if got := c.SequenceGaps(); len(got) != 0 {
		t.Fatalf("post-replay contiguous live frame fabricated a gap: %+v", got)
	}
}

func TestReplayCompletionReportsMissingSequence(t *testing.T) {
	done := make(chan struct{})
	c := &Client{
		replays:        map[string]chan struct{}{"replay-1": done},
		replayChannels: map[string]map[string]struct{}{"replay-1": {"stream": {}}},
		replaySequences: map[string]map[string]*replaySequence{
			"replay-1": {"stream": {baseline: 5, observed: make(map[uint64]struct{})}},
		},
		sequences: make(map[string]channelSequence),
	}
	c.dispatch(Event{Channel: "stream", Seq: 6})
	c.dispatch(Event{Channel: "stream", Seq: 8})
	c.completeReplay("replay-1")

	gaps := c.SequenceGaps()
	if len(gaps) != 1 || !gaps[0].Replay || gaps[0].Expected != 7 || gaps[0].Observed != 8 {
		t.Fatalf("replay gaps = %+v, want missing 7 observed at 8", gaps)
	}
	select {
	case <-done:
	default:
		t.Fatal("replay completion did not release its waiter")
	}
}
