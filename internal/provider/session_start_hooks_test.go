package provider

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

var testSessionStartHooks = []SessionStartHook{
	{Label: "instructions.md", Command: "hook instructions.md", Heading: "## Your standing instructions", Ending: "[End of instructions.md]", Stamp: "[Delivered at "},
	{Label: "MEMORY.md", Command: "hook MEMORY.md", Heading: "## Your memory index", Ending: "[End of MEMORY.md]", Stamp: "[Delivered at "},
}

const (
	testInstructionsContext = "## Your standing instructions\n\nRole\n\n[End of instructions.md]"
	testMemoryContext       = "## Your memory index\n\n- a\n\n[End of MEMORY.md]"
)

func newTestDelivery(t *testing.T, hooks []SessionStartHook) *SessionStartDelivery {
	t.Helper()
	return newTestDeliveryWithReceipts(t, hooks, nil)
}

func newTestDeliveryWithReceipts(t *testing.T, hooks []SessionStartHook, receipts *SessionStartReceipts) *SessionStartDelivery {
	t.Helper()
	d, err := NewSessionStartDelivery(hooks, receipts)
	if err != nil {
		t.Fatalf("NewSessionStartDelivery: %v", err)
	}
	return d
}

// testReceipts records what a delivery keeps and forgets.
type testReceipts struct {
	mu        sync.Mutex
	kept      []string
	forgotten int
	keepErr   error
	forgetErr error
}

func (r *testReceipts) receipts() *SessionStartReceipts {
	return &SessionStartReceipts{
		Keep: func(sessionRef, label, context string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.keepErr != nil {
				return r.keepErr
			}
			r.kept = append(r.kept, sessionRef+" "+label+" "+context)
			return nil
		},
		Forget: func() error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.forgotten++
			return r.forgetErr
		},
	}
}

func (r *testReceipts) take() ([]string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept, forgotten := r.kept, r.forgotten
	r.kept, r.forgotten = nil, 0
	return kept, forgotten
}

// settled is the one warning Settle returned, "" for none.
func settled(t *testing.T, d *SessionStartDelivery, sessionRef string) string {
	t.Helper()
	warnings := d.Settle(sessionRef)
	if len(warnings) > 1 {
		t.Fatalf("Settle warnings = %q, want at most one", warnings)
	}
	if len(warnings) == 0 {
		return ""
	}
	return warnings[0]
}

func TestSessionStartDeliveryWithoutHooksIsNilAndInert(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, nil)
	if d != nil {
		t.Fatalf("delivery without hooks = %+v", d)
	}
	if d.Expect(SessionStartAfterStart) != "" || d.ExpectAfresh(SessionStartAfterCompaction) != "" {
		t.Fatal("a nil delivery warned")
	}
	d.Deliver("## Your memory index")
	d.Fail("exit code 1")
	d.DropDeliveries()
	if d.Expecting() != "" || d.Pending() != nil || d.Settle("session-1") != nil || d.Keep() != nil {
		t.Fatal("a nil delivery acted")
	}
}

// A hook a delivery could not tell apart, or keep a receipt for, is refused,
// not silently counted as delivered: an empty heading opens every context.
func TestSessionStartDeliveryRefusesHooksItCannotTellApart(t *testing.T) {
	t.Parallel()
	for name, hooks := range map[string][]SessionStartHook{
		"no heading": {{Label: "a", Command: "a", Ending: "[End]", Stamp: "[At "}},
		"no command": {{Label: "a", Heading: "## A", Ending: "[End]", Stamp: "[At "}},
		"no label":   {{Command: "a", Heading: "## A", Ending: "[End]", Stamp: "[At "}},
		"no ending":  {{Label: "a", Command: "a", Heading: "## A", Stamp: "[At "}},
		"no stamp":   {{Label: "a", Command: "a", Heading: "## A", Ending: "[End]"}},
		"same heading": {
			{Label: "a", Command: "a", Heading: "## A", Ending: "[End a]", Stamp: "[At "},
			{Label: "b", Command: "b", Heading: "## A", Ending: "[End b]", Stamp: "[At "},
		},
		"same label": {
			{Label: "a", Command: "a", Heading: "## A", Ending: "[End a]", Stamp: "[At "},
			{Label: "a", Command: "b", Heading: "## B", Ending: "[End b]", Stamp: "[At "},
		},
	} {
		if d, err := NewSessionStartDelivery(hooks, nil); err == nil || d != nil {
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
	if got, want := settled(t, d, ""), "instructions.md and MEMORY.md did not reach this session after its start: no SessionStart hook output carried them. A hook failed, or hooks are turned off in the provider's own settings."; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}
	if d.Expecting() != "" || settled(t, d, "") != "" {
		t.Fatal("Settle did not end the expectation")
	}

	d.Expect(SessionStartAfterCompaction)
	d.Deliver("## Your standing instructions (instructions.md in your home)\n\nRole")
	if got, want := settled(t, d, ""), "MEMORY.md did not reach this session after its compaction: no SessionStart hook output carried it. A hook failed, or hooks are turned off in the provider's own settings."; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}

	d.Expect(SessionStartAfterCompaction)
	d.Deliver(testInstructionsContext)
	d.Deliver(testMemoryContext)
	if got := settled(t, d, ""); got != "" {
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
	d.Deliver(testInstructionsContext)
	if got, want := settled(t, d, ""), "MEMORY.md did not reach this session after its start: a SessionStart hook failed (exit code 127: [31msh: /gone/agent-overflow: not found [0m)."; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}

	// Settle forgot it, and a failure when every hook delivered (the
	// user's own hook) warns about nothing.
	d.Expect(SessionStartAfterCompaction)
	d.Fail("exit code 2")
	d.Deliver(testInstructionsContext)
	d.Deliver(testMemoryContext)
	if got := settled(t, d, ""); got != "" {
		t.Fatalf("warning = %q", got)
	}
	d.Expect(SessionStartAfterCompaction)
	if got := settled(t, d, ""); strings.Contains(got, "exit code") {
		t.Fatalf("a settled failure was quoted again: %q", got)
	}

	// Cut on a rune boundary: the bound falls inside an "é".
	d.Expect(SessionStartAfterStart)
	d.Fail("a" + strings.Repeat("é", sessionStartFailureMaxBytes))
	got := settled(t, d, "")
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
	if got := settled(t, d, ""); !strings.HasPrefix(got, "instructions.md and MEMORY.md ") {
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
	d.Deliver(testInstructionsContext)
	d.Deliver(testMemoryContext)
	d.Expect(SessionStartAfterCompaction)
	if got := settled(t, d, ""); got != "" {
		t.Fatalf("Expect after the deliveries: %q", got)
	}

	d.Deliver(testInstructionsContext)
	d.Deliver(testMemoryContext)
	d.Fail("exit code 1")
	d.ExpectAfresh(SessionStartAfterCompaction)
	if got := settled(t, d, ""); !strings.HasPrefix(got, "instructions.md and MEMORY.md did not reach this session after its compaction: no SessionStart hook output") {
		t.Fatalf("ExpectAfresh kept the earlier deliveries or failure: %q", got)
	}

	// Settle forgets deliveries even when nothing was expected.
	d.Deliver(testMemoryContext)
	if got := settled(t, d, ""); got != "" {
		t.Fatalf("Settle with nothing expected = %q", got)
	}
	d.Expect(SessionStartAfterStart)
	if got := settled(t, d, ""); !strings.HasPrefix(got, "instructions.md and MEMORY.md ") {
		t.Fatalf("a delivery survived Settle: %q", got)
	}
}

// A resume expects only the hooks it runs, those whose Resume is set, and
// opens no expectation when none is.
func TestSessionStartDeliveryAfterAResumeExpectsTheResumingHooks(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, testSessionStartHooks)
	d.Expect(SessionStartAfterResume)
	if d.Expecting() != "" || d.Pending() != nil {
		t.Fatalf("a resume of no resuming hook expects %q, pending %v", d.Expecting(), d.Pending())
	}
	if got := settled(t, d, ""); got != "" {
		t.Fatalf("warning = %q", got)
	}

	hooks := append([]SessionStartHook{}, testSessionStartHooks...)
	hooks[1].Resume = true
	d = newTestDelivery(t, hooks)
	d.Expect(SessionStartAfterResume)
	if d.Expecting() != SessionStartAfterResume {
		t.Fatalf("expecting %q", d.Expecting())
	}
	if got, want := d.Pending(), []bool{false, true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pending = %v, want %v", got, want)
	}
	if got, want := settled(t, d, ""), "MEMORY.md did not reach this session after its resume: no SessionStart hook output carried it. A hook failed, or hooks are turned off in the provider's own settings."; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}

	d.Expect(SessionStartAfterResume)
	d.Deliver(testMemoryContext)
	if got := d.Pending(); !reflect.DeepEqual(got, []bool{false, false}) {
		t.Fatalf("pending after the delivery = %v", got)
	}
	if got := settled(t, d, ""); got != "" {
		t.Fatalf("warning = %q", got)
	}

	// A compaction expects every hook, Resume or not.
	d.Expect(SessionStartAfterCompaction)
	if got := d.Pending(); !reflect.DeepEqual(got, []bool{true, true}) {
		t.Fatalf("pending after a compaction = %v", got)
	}
}

// Settle holds a receipt of exactly what each hook delivered under the
// provider session that received it, none without a session, and Keep
// writes what Settle holds, once.
func TestSessionStartDeliveryKeepsReceipts(t *testing.T) {
	t.Parallel()
	var r testReceipts
	d := newTestDeliveryWithReceipts(t, testSessionStartHooks, r.receipts())
	d.Expect(SessionStartAfterStart)
	d.Deliver(testInstructionsContext)
	d.Deliver(testMemoryContext)
	if got := settled(t, d, "session-1"); got != "" {
		t.Fatalf("warning = %q", got)
	}
	if kept, _ := r.take(); len(kept) != 0 {
		t.Fatalf("Settle kept %q before Keep", kept)
	}
	if warnings := d.Keep(); warnings != nil {
		t.Fatalf("Keep warnings = %q", warnings)
	}
	kept, forgotten := r.take()
	if want := []string{"session-1 instructions.md " + testInstructionsContext, "session-1 MEMORY.md " + testMemoryContext}; !reflect.DeepEqual(kept, want) || forgotten != 0 {
		t.Fatalf("kept %q, forgot %d times; want %q", kept, forgotten, want)
	}

	// Keep and Settle forgot what they wrote and held.
	d.Keep()
	settled(t, d, "session-1")
	d.Keep()
	if kept, _ := r.take(); len(kept) != 0 {
		t.Fatalf("kept again: %q", kept)
	}

	// A later Settle's receipt of a hook replaces the one held.
	d.Deliver(testMemoryContext + " 1")
	settled(t, d, "session-1")
	d.Deliver(testMemoryContext + " 2")
	settled(t, d, "session-2")
	d.Keep()
	if kept, _ := r.take(); !reflect.DeepEqual(kept, []string{"session-2 MEMORY.md " + testMemoryContext + " 2"}) {
		t.Fatalf("kept %q", kept)
	}

	d.Expect(SessionStartAfterStart)
	d.Deliver(testMemoryContext)
	settled(t, d, "")
	d.Keep()
	if kept, _ := r.take(); len(kept) != 0 {
		t.Fatalf("kept without a session: %q", kept)
	}
	settled(t, d, "session-1")
	d.Keep()
	if kept, _ := r.take(); len(kept) != 0 {
		t.Fatalf("a context outlived the Settle that had no session: %q", kept)
	}
}

// A compaction forgets every receipt, kept or held, Expect's or
// ExpectAfresh's; a start and a resume do not.
func TestSessionStartDeliveryForgetsReceiptsAtACompaction(t *testing.T) {
	t.Parallel()
	var r testReceipts
	d := newTestDeliveryWithReceipts(t, testSessionStartHooks, r.receipts())
	d.Deliver(testMemoryContext)
	settled(t, d, "session-1")
	d.Expect(SessionStartAfterStart)
	d.Expect(SessionStartAfterResume)
	if _, forgotten := r.take(); forgotten != 0 {
		t.Fatalf("a start or resume forgot %d times", forgotten)
	}
	if got := d.Expect(SessionStartAfterCompaction); got != "" {
		t.Fatalf("warning = %q", got)
	}
	d.Keep()
	if kept, forgotten := r.take(); len(kept) != 0 || forgotten != 1 {
		t.Fatalf("a compaction kept %q and forgot %d times", kept, forgotten)
	}
	d.Deliver(testMemoryContext)
	settled(t, d, "session-1")
	if got := d.ExpectAfresh(SessionStartAfterCompaction); got != "" {
		t.Fatalf("warning = %q", got)
	}
	d.Keep()
	if kept, forgotten := r.take(); len(kept) != 0 || forgotten != 1 {
		t.Fatalf("ExpectAfresh kept %q and forgot %d times", kept, forgotten)
	}

	r.forgetErr = errors.New("disk full")
	if got, want := d.Expect(SessionStartAfterCompaction), "Agent Overflow could not record that this compaction replaced what the SessionStart hooks delivered, so a later resume may not deliver instructions.md and MEMORY.md again: disk full"; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}
}

func TestSessionStartDeliveryWarnsWhenAReceiptIsNotKept(t *testing.T) {
	t.Parallel()
	r := testReceipts{keepErr: errors.New("database is locked")}
	d := newTestDeliveryWithReceipts(t, testSessionStartHooks, r.receipts())
	d.Expect(SessionStartAfterStart)
	d.Deliver(testInstructionsContext)
	if got, want := settled(t, d, "session-1"), "MEMORY.md did not reach this session after its start: no SessionStart hook output carried it. A hook failed, or hooks are turned off in the provider's own settings."; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}
	if got, want := d.Keep(), []string{"Agent Overflow could not record that instructions.md reached this session, so its next resume delivers it again: database is locked"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("warnings = %q, want %q", got, want)
	}
}

// DropDeliveries forgets what was delivered and failed since the last
// Settle, and the receipts held, and keeps the expectation open.
func TestSessionStartDeliveryDropsDeliveries(t *testing.T) {
	t.Parallel()
	var r testReceipts
	d := newTestDeliveryWithReceipts(t, testSessionStartHooks, r.receipts())
	d.Deliver(testInstructionsContext)
	settled(t, d, "session-1")
	d.Expect(SessionStartAfterStart)
	d.Deliver(testInstructionsContext)
	d.Deliver(testMemoryContext)
	d.Fail("exit code 1")
	d.DropDeliveries()
	if d.Expecting() != SessionStartAfterStart || !reflect.DeepEqual(d.Pending(), []bool{true, true}) {
		t.Fatalf("expecting %q, pending %v", d.Expecting(), d.Pending())
	}
	if got := settled(t, d, "session-1"); !strings.HasPrefix(got, "instructions.md and MEMORY.md did not reach this session after its start: no SessionStart hook output") {
		t.Fatalf("warning = %q", got)
	}
	d.Keep()
	if kept, _ := r.take(); len(kept) != 0 {
		t.Fatalf("kept %q", kept)
	}
}

func TestSessionStartDeliveryNamesThreeOrMoreHooks(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, append(append([]SessionStartHook{}, testSessionStartHooks...), SessionStartHook{Label: "notes.md", Command: "hook notes.md", Heading: "## Notes", Ending: "[End of notes.md]", Stamp: "[Delivered at "}))
	d.Expect(SessionStartAfterStart)
	if got := settled(t, d, ""); !strings.HasPrefix(got, "instructions.md, MEMORY.md and notes.md did not reach") {
		t.Fatalf("warning = %q", got)
	}
}

func TestSessionStartMatcher(t *testing.T) {
	t.Parallel()
	if got := SessionStartMatcher(false); got != "startup|compact|clear" {
		t.Fatalf("matcher = %q", got)
	}
	if got := SessionStartMatcher(true); got != "startup|compact|clear|resume|fork" {
		t.Fatalf("resume matcher = %q", got)
	}
}

func TestSessionStartDeliveryIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	var r testReceipts
	d := newTestDeliveryWithReceipts(t, testSessionStartHooks, r.receipts())
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				switch i % 7 {
				case 0:
					d.Expect(SessionStartAfterStart)
				case 1:
					d.Deliver(testMemoryContext)
				case 2:
					d.DropDeliveries()
				case 5:
					d.Keep()
				case 3:
					d.Fail("exit code 1")
				case 4:
					d.ExpectAfresh(SessionStartAfterCompaction)
					d.Pending()
				default:
					d.Settle("session-1")
					d.Expecting()
				}
			}
		}()
	}
	wg.Wait()
}
