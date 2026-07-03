package discussion

import "testing"

func TestNewDeliberationSeedsRosterAndFirstSpeaker(t *testing.T) {
	d := NewDeliberation("channel-1", 3, []string{"thread-a", "thread-b", "thread-c"})
	state := d.State()
	if state.CurrentSpeaker != "thread-a" {
		t.Fatalf("CurrentSpeaker = %q, want thread-a (first participant)", state.CurrentSpeaker)
	}
	if state.AwaitingResponse {
		t.Fatal("AwaitingResponse should start false — nobody has been prompted yet")
	}
	if got := d.Participants(); len(got) != 3 || got[0] != "thread-a" || got[1] != "thread-b" || got[2] != "thread-c" {
		t.Fatalf("Participants() = %v, want [thread-a thread-b thread-c] in roster order", got)
	}
}

func TestNewDeliberationEmptyRosterHasNoCurrentSpeaker(t *testing.T) {
	d := NewDeliberation("channel-empty", 3, nil)
	if got := d.State().CurrentSpeaker; got != "" {
		t.Fatalf("CurrentSpeaker = %q, want empty for an empty roster", got)
	}
}

func TestDeliberationRecordPostAlternatesAndConcludes(t *testing.T) {
	d := NewDeliberation("channel-1", 3, []string{"thread-a", "thread-b"})

	next, conclude := d.RecordPost("thread-a")
	if next != "thread-b" || conclude {
		t.Fatalf("first post = (%q,%v), want (thread-b,false)", next, conclude)
	}

	next, conclude = d.RecordPost("thread-b")
	if next != "thread-a" || conclude {
		t.Fatalf("second post = (%q,%v), want (thread-a,false)", next, conclude)
	}

	next, conclude = d.RecordPost("thread-a")
	if next != "" || !conclude {
		t.Fatalf("third post = (%q,%v), want (\"\",true)", next, conclude)
	}

	state := d.State()
	if !state.Concluded {
		t.Fatal("expected deliberation state to be concluded at max turns")
	}
	if state.CurrentSpeaker != "" {
		t.Fatalf("CurrentSpeaker = %q, want empty after conclusion", state.CurrentSpeaker)
	}
}

// TestDeliberationTryClaimCurrentSpeakerThenRecordPostClearsAwaiting
// exercises the awaiting-response lifecycle the app layer drives:
// TryClaimCurrentSpeaker arms it atomically right before a dispatch is
// handed to a background goroutine, RecordPost clears it whether or
// not the poster is the one that was prompted.
func TestDeliberationTryClaimCurrentSpeakerThenRecordPostClearsAwaiting(t *testing.T) {
	d := NewDeliberation("channel-1", 5, []string{"thread-a", "thread-b"})

	speaker, ok := d.TryClaimCurrentSpeaker()
	if !ok || speaker != "thread-a" {
		t.Fatalf("TryClaimCurrentSpeaker() = (%q,%v), want (thread-a,true)", speaker, ok)
	}
	state := d.State()
	if !state.AwaitingResponse {
		t.Fatal("AwaitingResponse should be true immediately after a successful claim")
	}
	if state.CurrentSpeaker != "thread-a" {
		t.Fatalf("CurrentSpeaker = %q, want thread-a after claiming", state.CurrentSpeaker)
	}

	next, conclude := d.RecordPost("thread-a")
	if conclude {
		t.Fatal("expected no conclusion at turn 1 of 5")
	}
	if next != "thread-b" {
		t.Fatalf("next speaker = %q, want thread-b", next)
	}
	if d.State().AwaitingResponse {
		t.Fatal("RecordPost must clear AwaitingResponse")
	}
}

// TestDeliberationTryClaimCurrentSpeakerFailsWhileAwaiting guards the
// race TryClaimCurrentSpeaker exists to close: a second claim attempt
// while a turn is already in flight must not succeed — otherwise two
// concurrent triggers (e.g. two rapid human posts) could both dispatch
// a prompt to the same speaker.
func TestDeliberationTryClaimCurrentSpeakerFailsWhileAwaiting(t *testing.T) {
	d := NewDeliberation("channel-1", 5, []string{"thread-a", "thread-b"})

	if _, ok := d.TryClaimCurrentSpeaker(); !ok {
		t.Fatal("expected the first claim to succeed")
	}
	if speaker, ok := d.TryClaimCurrentSpeaker(); ok {
		t.Fatalf("second claim = (%q,true), want ok=false while a turn is in flight", speaker)
	}
}

// TestDeliberationClearAwaitingResponseAllowsRetryClaim documents the
// failed-dispatch recovery path: promptDiscussionSpeaker calls this
// when sendMessage fails, so a later trigger can retry the same
// CurrentSpeaker instead of the deliberation being stuck forever
// awaiting a reply that was never actually sent.
func TestDeliberationClearAwaitingResponseAllowsRetryClaim(t *testing.T) {
	d := NewDeliberation("channel-1", 5, []string{"thread-a", "thread-b"})

	if _, ok := d.TryClaimCurrentSpeaker(); !ok {
		t.Fatal("expected the first claim to succeed")
	}
	d.ClearAwaitingResponse()

	speaker, ok := d.TryClaimCurrentSpeaker()
	if !ok || speaker != "thread-a" {
		t.Fatalf("retry claim after ClearAwaitingResponse = (%q,%v), want (thread-a,true)", speaker, ok)
	}
}

// TestDeliberationTryClaimCurrentSpeakerFailsWhenNoCurrentSpeaker
// covers the single-participant no-self-loop case: after the sole
// participant posts, CurrentSpeaker is "" and claiming must fail
// rather than handing back an empty thread ID to prompt.
func TestDeliberationTryClaimCurrentSpeakerFailsWhenNoCurrentSpeaker(t *testing.T) {
	d := NewDeliberation("channel-1", 5, []string{"thread-solo"})
	d.RecordPost("thread-solo")

	if speaker, ok := d.TryClaimCurrentSpeaker(); ok {
		t.Fatalf("claim = (%q,true), want ok=false with no current speaker", speaker)
	}
}

// TestDeliberationTryClaimCurrentSpeakerFailsWhenConcluded covers the
// terminal state: a concluded deliberation must never hand back a
// speaker to prompt, even if some caller still holds a stale reference
// to it.
func TestDeliberationTryClaimCurrentSpeakerFailsWhenConcluded(t *testing.T) {
	d := NewDeliberation("channel-1", 1, []string{"thread-a", "thread-b"})
	d.RecordPost("thread-a")
	if !d.State().Concluded {
		t.Fatal("expected deliberation to be concluded at max turns")
	}

	if speaker, ok := d.TryClaimCurrentSpeaker(); ok {
		t.Fatalf("claim = (%q,true), want ok=false once concluded", speaker)
	}
}

// TestDeliberationRecordPostFromNonPromptedParticipantStillAdvances
// documents the "any participant" semantics: a post from a participant
// the FSM wasn't currently awaiting (e.g. a human manually drove a
// child thread's pane) still counts as that participant's turn and
// advances round-robin from them.
func TestDeliberationRecordPostFromNonPromptedParticipantStillAdvances(t *testing.T) {
	d := NewDeliberation("channel-1", 5, []string{"thread-a", "thread-b", "thread-c"})
	if _, ok := d.TryClaimCurrentSpeaker(); !ok {
		t.Fatal("expected the claim on thread-a to succeed")
	}

	// thread-c posts even though thread-a was the one prompted.
	next, conclude := d.RecordPost("thread-c")
	if conclude {
		t.Fatal("expected no conclusion")
	}
	if next != "thread-a" {
		t.Fatalf("next speaker after thread-c = %q, want thread-a (round robin from thread-c)", next)
	}
	if d.State().AwaitingResponse {
		t.Fatal("AwaitingResponse should clear regardless of who posted")
	}
}

// TestDeliberationSingleParticipantDoesNotSelfLoop guards the explicit
// invariant: a discussion with fewer than two participants must never
// hand the turn back to the same speaker.
func TestDeliberationSingleParticipantDoesNotSelfLoop(t *testing.T) {
	d := NewDeliberation("channel-1", 5, []string{"thread-solo"})
	if got := d.State().CurrentSpeaker; got != "thread-solo" {
		t.Fatalf("CurrentSpeaker = %q, want thread-solo as the seeded first speaker", got)
	}

	next, conclude := d.RecordPost("thread-solo")
	if conclude {
		t.Fatal("expected no conclusion at turn 1 of 5")
	}
	if next != "" {
		t.Fatalf("next speaker = %q, want empty — a single participant must not self-loop", next)
	}
	if d.State().CurrentSpeaker != "" {
		t.Fatalf("CurrentSpeaker = %q, want empty after a single-participant post", d.State().CurrentSpeaker)
	}
}

func TestNextSpeakerAfterGuardsAndFallsBack(t *testing.T) {
	if got := NextSpeakerAfter([]string{"a"}, "a"); got != "" {
		t.Fatalf("NextSpeakerAfter(single) = %q, want empty", got)
	}
	if got := NextSpeakerAfter(nil, "a"); got != "" {
		t.Fatalf("NextSpeakerAfter(nil) = %q, want empty", got)
	}
	if got := NextSpeakerAfter([]string{"a", "b", "c"}, "c"); got != "a" {
		t.Fatalf("NextSpeakerAfter(wrap) = %q, want a", got)
	}
	// threadID not in the roster falls back to the first participant.
	if got := NextSpeakerAfter([]string{"a", "b"}, "unknown"); got != "a" {
		t.Fatalf("NextSpeakerAfter(unknown poster) = %q, want a (fallback to first)", got)
	}
}

func TestDeliberationRequiresUnanimousConclusion(t *testing.T) {
	d := NewDeliberation("channel-1", 4, []string{"thread-a", "thread-b"})
	d.RecordPost("thread-a")
	d.RecordPost("thread-b")

	if agreed := d.ProposeConclusionFrom("thread-a", "done"); agreed {
		t.Fatal("expected first proposal to be non-unanimous")
	}
	if agreed := d.ProposeConclusionFrom("thread-b", "agreed"); !agreed {
		t.Fatal("expected unanimous conclusion after both participants propose")
	}

	state := d.State()
	if !state.Concluded {
		t.Fatal("expected deliberation to be concluded")
	}
	if len(state.ConclusionProposals) != 2 {
		t.Fatalf("len(ConclusionProposals) = %d, want 2", len(state.ConclusionProposals))
	}
}

func TestNewDeliberationDefaultsMaxTurns(t *testing.T) {
	d := NewDeliberation("channel-2", 0, []string{"thread-a", "thread-b"})
	if d.State().MaxTurns != DefaultMaxTurns {
		t.Fatalf("MaxTurns = %d, want %d", d.State().MaxTurns, DefaultMaxTurns)
	}
}

// --- RestoreDeliberation (restart-rebuild path) ---

func TestRestoreDeliberationReconstructsInProgressState(t *testing.T) {
	d := RestoreDeliberation("channel-1", 6, []string{"thread-a", "thread-b", "thread-c"}, 4, "thread-b")
	state := d.State()
	if state.TurnCount != 4 {
		t.Fatalf("TurnCount = %d, want 4", state.TurnCount)
	}
	if state.CurrentSpeaker != "thread-b" {
		t.Fatalf("CurrentSpeaker = %q, want thread-b", state.CurrentSpeaker)
	}
	if state.MaxTurns != 6 {
		t.Fatalf("MaxTurns = %d, want 6", state.MaxTurns)
	}
	if state.AwaitingResponse {
		t.Fatal("AwaitingResponse must come back false across a restart — no live provider turn is in flight")
	}
	if state.Concluded {
		t.Fatal("4 of 6 turns must not be concluded")
	}

	// The rebuilt FSM keeps coordinating normally from here.
	next, conclude := d.RecordPost("thread-b")
	if conclude {
		t.Fatal("expected no conclusion at turn 5 of 6")
	}
	if next != "thread-c" {
		t.Fatalf("next speaker = %q, want thread-c", next)
	}
}

func TestRestoreDeliberationAtOrPastMaxTurnsComesBackConcluded(t *testing.T) {
	d := RestoreDeliberation("channel-1", 4, []string{"thread-a", "thread-b"}, 4, "thread-a")
	state := d.State()
	if !state.Concluded {
		t.Fatal("expected a restored deliberation at turnCount==maxTurns to come back concluded")
	}
	if state.CurrentSpeaker != "" {
		t.Fatalf("CurrentSpeaker = %q, want empty for a concluded restore", state.CurrentSpeaker)
	}
}

func TestRestoreDeliberationNormalizesNonPositiveMaxTurns(t *testing.T) {
	d := RestoreDeliberation("channel-1", 0, []string{"thread-a", "thread-b"}, 0, "thread-a")
	if got := d.State().MaxTurns; got != DefaultMaxTurns {
		t.Fatalf("MaxTurns = %d, want normalized default %d", got, DefaultMaxTurns)
	}
}
