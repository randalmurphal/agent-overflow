package discussion

import "testing"

func TestDeliberationRecordPostAlternatesAndConcludes(t *testing.T) {
	d := NewDeliberation("channel-1", 3)

	next, conclude := d.RecordPost("thread-a")
	if next != "" || conclude {
		t.Fatalf("first post = (%q,%v), want (\"\",false)", next, conclude)
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

func TestDeliberationRequiresUnanimousConclusion(t *testing.T) {
	d := NewDeliberation("channel-1", 4)
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
	d := NewDeliberation("channel-2", 0)
	if d.State().MaxTurns != 8 {
		t.Fatalf("MaxTurns = %d, want 8", d.State().MaxTurns)
	}
}
