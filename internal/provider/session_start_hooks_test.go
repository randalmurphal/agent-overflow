package provider

import (
	"strings"
	"sync"
	"testing"
)

var testSessionStartHooks = []SessionStartHook{
	{Label: "instructions.md", Command: "hook instructions.md", Heading: "## Your standing instructions"},
	{Label: "MEMORY.md", Command: "hook MEMORY.md", Heading: "## Your memory index"},
}

func newTestDelivery(t *testing.T, hooks []SessionStartHook) *SessionStartDelivery {
	t.Helper()
	d, err := NewSessionStartDelivery(hooks)
	if err != nil {
		t.Fatalf("NewSessionStartDelivery: %v", err)
	}
	return d
}

func TestSessionStartDeliveryWithoutHooksIsNilAndInert(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, nil)
	if d != nil {
		t.Fatalf("delivery without hooks = %+v", d)
	}
	d.Expect(SessionStartAfterStart)
	d.ExpectAfresh(SessionStartAfterCompaction)
	d.Deliver("## Your memory index")
	d.DeliverWithin([]byte("## Your memory index"))
	d.Fail("exit code 1")
	if d.Expecting() != "" || d.Settle() != "" {
		t.Fatal("a nil delivery acted")
	}
}

// A hook a delivery could not tell apart is refused, not silently counted
// as delivered: an empty heading opens every context.
func TestSessionStartDeliveryRefusesHooksItCannotTellApart(t *testing.T) {
	t.Parallel()
	for name, hooks := range map[string][]SessionStartHook{
		"no heading": {{Label: "a", Command: "a"}},
		"no command": {{Label: "a", Heading: "## A"}},
		"no label":   {{Command: "a", Heading: "## A"}},
		"same heading": {
			{Label: "a", Command: "a", Heading: "## A"},
			{Label: "b", Command: "b", Heading: "## A"},
		},
	} {
		if d, err := NewSessionStartDelivery(hooks); err == nil || d != nil {
			t.Errorf("%s: delivery %+v, error %v", name, d, err)
		}
	}
}

func TestSessionStartDeliveryWarnsForTheHooksThatDidNotDeliver(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, testSessionStartHooks)
	if d.Expecting() != "" {
		t.Fatal("expecting before Expect")
	}
	d.Expect(SessionStartAfterStart)
	if d.Expecting() != SessionStartAfterStart {
		t.Fatalf("expecting %q after Expect", d.Expecting())
	}
	if got, want := d.Settle(), "instructions.md and MEMORY.md did not reach this session after its start: no SessionStart hook output carried them. A hook failed, or hooks are turned off in the provider's own settings."; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}
	if d.Expecting() != "" || d.Settle() != "" {
		t.Fatal("Settle did not end the expectation")
	}

	d.Expect(SessionStartAfterCompaction)
	d.Deliver("## Your standing instructions (instructions.md in your home)\n\nRole")
	if got, want := d.Settle(), "MEMORY.md did not reach this session after its compaction: no SessionStart hook output carried it. A hook failed, or hooks are turned off in the provider's own settings."; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}

	d.Expect(SessionStartAfterCompaction)
	d.Deliver("## Your standing instructions\n\nRole")
	d.Deliver("## Your memory index\n\n- a")
	if got := d.Settle(); got != "" {
		t.Fatalf("warning after both delivered = %q", got)
	}
}

// A failed run's cause replaces the guess, cleaned to one bounded line; the
// first failure since the last Settle is the one quoted.
func TestSessionStartDeliveryQuotesTheFailure(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, testSessionStartHooks)
	d.Expect(SessionStartAfterStart)
	d.Fail("exit code 127:\n\x1b[31msh: /gone/agent-overflow: not found\x1b[0m‮")
	d.Fail("exit code 1: later")
	d.Deliver("## Your standing instructions\n\nRole")
	if got, want := d.Settle(), "MEMORY.md did not reach this session after its start: a SessionStart hook failed (exit code 127: [31msh: /gone/agent-overflow: not found [0m)."; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}

	// Settle forgot it, and a failure when every hook delivered (the
	// user's own hook) warns about nothing.
	d.Expect(SessionStartAfterCompaction)
	d.Fail("exit code 2")
	d.Deliver("## Your standing instructions\n\nRole")
	d.Deliver("## Your memory index\n\n- a")
	if got := d.Settle(); got != "" {
		t.Fatalf("warning = %q", got)
	}
	d.Expect(SessionStartAfterCompaction)
	if got := d.Settle(); strings.Contains(got, "exit code") {
		t.Fatalf("a settled failure was quoted again: %q", got)
	}

	// Cut on a rune boundary: the bound falls inside an "é".
	d.Expect(SessionStartAfterStart)
	d.Fail("a" + strings.Repeat("é", sessionStartFailureMaxBytes))
	got := d.Settle()
	quoted := got[strings.Index(got, "(")+1 : strings.LastIndex(got, ")")]
	if len(quoted) > sessionStartFailureMaxBytes+len("…") || !strings.HasSuffix(quoted, "…") || strings.ContainsRune(quoted, '�') {
		t.Fatalf("quoted failure = %q (%d bytes)", quoted, len(quoted))
	}
}

// Only context that opens with a hook's heading is that hook's output.
func TestSessionStartDeliveryCountsOnlyContextOpeningWithAHeading(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, testSessionStartHooks)
	for _, context := range []string{"", "user hook output", " ## Your memory index", "Quoted: ## Your memory index", "## Your memory"} {
		d.Deliver(context)
	}
	d.Expect(SessionStartAfterStart)
	if got := d.Settle(); !strings.HasPrefix(got, "instructions.md and MEMORY.md ") {
		t.Fatalf("warning = %q", got)
	}
}

// A delivery before Expect counts: Claude Code reports a compaction's hook
// output before its compaction boundary. ExpectAfresh forgets it, and a
// failure: Codex runs a compaction's hooks only after reporting the
// compaction.
func TestSessionStartDeliveryBeforeExpect(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, testSessionStartHooks)
	d.Deliver("## Your standing instructions\n\nRole")
	d.Deliver("## Your memory index\n\n- a")
	d.Expect(SessionStartAfterCompaction)
	if got := d.Settle(); got != "" {
		t.Fatalf("Expect after the deliveries: %q", got)
	}

	d.Deliver("## Your standing instructions\n\nRole")
	d.Deliver("## Your memory index\n\n- a")
	d.Fail("exit code 1")
	d.ExpectAfresh(SessionStartAfterCompaction)
	if got := d.Settle(); !strings.HasPrefix(got, "instructions.md and MEMORY.md did not reach this session after its compaction: no SessionStart hook output") {
		t.Fatalf("ExpectAfresh kept the earlier deliveries or failure: %q", got)
	}

	// Settle forgets deliveries even when nothing was expected.
	d.Deliver("## Your memory index\n\n- a")
	if got := d.Settle(); got != "" {
		t.Fatalf("Settle with nothing expected = %q", got)
	}
	d.Expect(SessionStartAfterStart)
	if got := d.Settle(); !strings.HasPrefix(got, "instructions.md and MEMORY.md ") {
		t.Fatalf("a delivery survived Settle: %q", got)
	}
}

func TestSessionStartDeliveryWithinARequest(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, testSessionStartHooks)
	d.Expect(SessionStartAfterStart)
	d.DeliverWithin([]byte(`SessionStart:startup hook additional context: ## Your memory index\n\n- a`))
	if got := d.Settle(); !strings.HasPrefix(got, "instructions.md did not reach") {
		t.Fatalf("warning = %q", got)
	}
}

func TestSessionStartDeliveryNamesThreeOrMoreHooks(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, append(append([]SessionStartHook{}, testSessionStartHooks...), SessionStartHook{Label: "notes.md", Command: "hook notes.md", Heading: "## Notes"}))
	d.Expect(SessionStartAfterStart)
	if got := d.Settle(); !strings.HasPrefix(got, "instructions.md, MEMORY.md and notes.md did not reach") {
		t.Fatalf("warning = %q", got)
	}
}

func TestSessionStartDeliveryIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, testSessionStartHooks)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				switch i % 5 {
				case 0:
					d.Expect(SessionStartAfterStart)
				case 1:
					d.Deliver("## Your memory index")
				case 2:
					d.DeliverWithin([]byte("## Your standing instructions"))
				case 3:
					d.Fail("exit code 1")
				default:
					d.Settle()
					d.Expecting()
				}
			}
		}()
	}
	wg.Wait()
}
