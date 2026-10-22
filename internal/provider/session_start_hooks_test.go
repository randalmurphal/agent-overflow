package provider

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

var testSessionStartHook = SessionStartHook{Label: "Agent Overflow memory", Command: "hook memory", Heading: "## Memory", Ending: "(End of memory)", Stamp: "(Delivered at "}

const testMemoryContext = "## Memory\n\n- a\n\n(End of memory)"

const testMissingAtStart = "Agent Overflow memory did not reach this session after its start: no SessionStart hook output carried it. A hook failed, or hooks are turned off in the provider's own settings."

func newTestDelivery(t *testing.T, hook *SessionStartHook) *SessionStartDelivery {
	t.Helper()
	return newTestDeliveryWithReceipts(t, hook, nil)
}

func newTestDeliveryWithReceipts(t *testing.T, hook *SessionStartHook, receipts *SessionStartReceipts) *SessionStartDelivery {
	t.Helper()
	d, err := NewSessionStartDelivery(hook, receipts)
	if err != nil {
		t.Fatalf("NewSessionStartDelivery: %v", err)
	}
	return d
}

// testHook is a copy of testSessionStartHook, so a test can change it.
func testHook() *SessionStartHook {
	hook := testSessionStartHook
	return &hook
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
		Keep: func(sessionRef, context string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.keepErr != nil {
				return r.keepErr
			}
			r.kept = append(r.kept, sessionRef+" "+context)
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

func TestSessionStartDeliveryWithoutAHookIsNilAndInert(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, nil)
	if d != nil {
		t.Fatalf("delivery without a hook = %+v", d)
	}
	if d.Expect(SessionStartAfterStart) != "" || d.ExpectAfresh(SessionStartAfterCompaction) != "" {
		t.Fatal("a nil delivery warned")
	}
	d.Deliver(testMemoryContext)
	d.Fail("exit code 1")
	d.DropDeliveries()
	if d.Expecting() != "" || d.Pending() || d.Settle("session-1") != "" || d.Keep() != "" || d.Hook() != nil {
		t.Fatal("a nil delivery acted")
	}
}

// A hook a delivery could not tell apart, or name in a warning, is refused,
// not silently counted as delivered: an empty heading opens every context.
func TestSessionStartDeliveryRefusesAnIncompleteHook(t *testing.T) {
	t.Parallel()
	for name, hook := range map[string]SessionStartHook{
		"no heading": {Label: "a", Command: "a", Ending: "[End]", Stamp: "[At "},
		"no command": {Label: "a", Heading: "## A", Ending: "[End]", Stamp: "[At "},
		"no label":   {Command: "a", Heading: "## A", Ending: "[End]", Stamp: "[At "},
		"no ending":  {Label: "a", Command: "a", Heading: "## A", Stamp: "[At "},
		"no stamp":   {Label: "a", Command: "a", Heading: "## A", Ending: "[End]"},
	} {
		if d, err := NewSessionStartDelivery(&hook, nil); err == nil || d != nil {
			t.Errorf("%s: delivery %+v, error %v", name, d, err)
		}
	}
}

// Hook is a copy: changing it changes nothing the delivery follows.
func TestSessionStartDeliveryHookIsACopy(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, testHook())
	d.Hook().Heading = "## Other"
	if got := d.Hook(); !reflect.DeepEqual(*got, testSessionStartHook) {
		t.Fatalf("hook = %+v", got)
	}
}

func TestSessionStartDeliveryWarnsWhenTheHookDidNotDeliver(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, testHook())
	if d.Expecting() != "" {
		t.Fatal("expecting before Expect")
	}
	d.Expect(SessionStartAfterStart)
	if d.Expecting() != SessionStartAfterStart {
		t.Fatalf("expecting %q after Expect", d.Expecting())
	}
	if got := d.Settle(""); got != testMissingAtStart {
		t.Fatalf("warning = %q, want %q", got, testMissingAtStart)
	}
	if d.Expecting() != "" || d.Settle("") != "" {
		t.Fatal("Settle did not end the expectation")
	}

	d.Expect(SessionStartAfterCompaction)
	d.Deliver("## Memo\n\n- a")
	if got, want := d.Settle(""), "Agent Overflow memory did not reach this session after its compaction: no SessionStart hook output carried it. A hook failed, or hooks are turned off in the provider's own settings."; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}

	d.Expect(SessionStartAfterCompaction)
	d.Deliver(testMemoryContext)
	if got := d.Settle(""); got != "" {
		t.Fatalf("warning after the delivery = %q", got)
	}
}

// A failed run's cause replaces the guess, cleaned to one bounded line; the
// first failure since the last Settle is the one quoted.
func TestSessionStartDeliveryQuotesTheFailure(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, testHook())
	d.Expect(SessionStartAfterStart)
	d.Fail("exit code 127:\n\x1b[31msh: /gone/agent-overflow: not found\x1b[0m‮")
	d.Fail("exit code 1: later")
	if got, want := d.Settle(""), "Agent Overflow memory did not reach this session after its start: a SessionStart hook failed (exit code 127: [31msh: /gone/agent-overflow: not found [0m)."; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}

	// Settle forgot it, and a failure when the hook delivered (the user's
	// own hook) warns about nothing.
	d.Expect(SessionStartAfterCompaction)
	d.Fail("exit code 2")
	d.Deliver(testMemoryContext)
	if got := d.Settle(""); got != "" {
		t.Fatalf("warning = %q", got)
	}
	d.Expect(SessionStartAfterCompaction)
	if got := d.Settle(""); strings.Contains(got, "exit code") {
		t.Fatalf("a settled failure was quoted again: %q", got)
	}

	// Cut on a rune boundary: the bound falls inside an "é".
	d.Expect(SessionStartAfterStart)
	d.Fail("a" + strings.Repeat("é", sessionStartFailureMaxBytes))
	got := d.Settle("")
	quoted := got[strings.Index(got, "(")+1 : strings.LastIndex(got, ")")]
	if len(quoted) > sessionStartFailureMaxBytes+len("…") || !strings.HasSuffix(quoted, "…") || strings.ContainsRune(quoted, '�') {
		t.Fatalf("quoted failure = %q (%d bytes)", quoted, len(quoted))
	}
}

// Only context that opens with the hook's heading is its output.
func TestSessionStartDeliveryCountsOnlyContextOpeningWithTheHeading(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, testHook())
	for _, context := range []string{"", "user hook output", " ## Memory", "Quoted: ## Memory", "## Memo"} {
		d.Deliver(context)
	}
	d.Expect(SessionStartAfterStart)
	if got := d.Settle(""); got != testMissingAtStart {
		t.Fatalf("warning = %q", got)
	}
}

// A delivery before Expect counts: Claude Code reports a compaction's hook
// output before its compaction boundary. ExpectAfresh forgets it, and a
// failure: Codex runs a compaction's hooks only after reporting the
// compaction.
func TestSessionStartDeliveryBeforeExpect(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, testHook())
	d.Deliver(testMemoryContext)
	d.Expect(SessionStartAfterCompaction)
	if got := d.Settle(""); got != "" {
		t.Fatalf("Expect after the delivery: %q", got)
	}

	d.Deliver(testMemoryContext)
	d.Fail("exit code 1")
	d.ExpectAfresh(SessionStartAfterCompaction)
	if got := d.Settle(""); !strings.HasPrefix(got, "Agent Overflow memory did not reach this session after its compaction: no SessionStart hook output") {
		t.Fatalf("ExpectAfresh kept the earlier delivery or failure: %q", got)
	}

	// Settle forgets a delivery even when nothing was expected.
	d.Deliver(testMemoryContext)
	if got := d.Settle(""); got != "" {
		t.Fatalf("Settle with nothing expected = %q", got)
	}
	d.Expect(SessionStartAfterStart)
	if got := d.Settle(""); got != testMissingAtStart {
		t.Fatalf("a delivery survived Settle: %q", got)
	}
}

// A resume expects the hook only when its Resume is set, and opens no
// expectation otherwise; a compaction expects it either way.
func TestSessionStartDeliveryAfterAResumeExpectsAResumingHook(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, testHook())
	d.Expect(SessionStartAfterResume)
	if d.Expecting() != "" || d.Pending() {
		t.Fatalf("a resume of a hook that does not resume expects %q, pending %v", d.Expecting(), d.Pending())
	}
	if got := d.Settle(""); got != "" {
		t.Fatalf("warning = %q", got)
	}
	d.Expect(SessionStartAfterCompaction)
	if !d.Pending() {
		t.Fatal("a compaction does not wait for the hook")
	}

	hook := testHook()
	hook.Resume = true
	d = newTestDelivery(t, hook)
	d.Expect(SessionStartAfterResume)
	if d.Expecting() != SessionStartAfterResume || !d.Pending() {
		t.Fatalf("expecting %q, pending %v", d.Expecting(), d.Pending())
	}
	if got, want := d.Settle(""), "Agent Overflow memory did not reach this session after its resume: no SessionStart hook output carried it. A hook failed, or hooks are turned off in the provider's own settings."; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}

	d.Expect(SessionStartAfterResume)
	d.Deliver(testMemoryContext)
	if d.Pending() {
		t.Fatal("pending after the delivery")
	}
	if got := d.Settle(""); got != "" {
		t.Fatalf("warning = %q", got)
	}
}

// Settle holds a receipt of exactly what the hook delivered under the
// provider session that received it, none without a session, and Keep
// writes what Settle holds, once.
func TestSessionStartDeliveryKeepsReceipts(t *testing.T) {
	t.Parallel()
	var r testReceipts
	d := newTestDeliveryWithReceipts(t, testHook(), r.receipts())
	d.Expect(SessionStartAfterStart)
	d.Deliver(testMemoryContext)
	if got := d.Settle("session-1"); got != "" {
		t.Fatalf("warning = %q", got)
	}
	if kept, _ := r.take(); len(kept) != 0 {
		t.Fatalf("Settle kept %q before Keep", kept)
	}
	if warning := d.Keep(); warning != "" {
		t.Fatalf("Keep warning = %q", warning)
	}
	kept, forgotten := r.take()
	if want := []string{"session-1 " + testMemoryContext}; !reflect.DeepEqual(kept, want) || forgotten != 0 {
		t.Fatalf("kept %q, forgot %d times; want %q", kept, forgotten, want)
	}

	// Keep and Settle forgot what they wrote and held.
	d.Keep()
	d.Settle("session-1")
	d.Keep()
	if kept, _ := r.take(); len(kept) != 0 {
		t.Fatalf("kept again: %q", kept)
	}

	// A later Settle's receipt replaces the one held.
	d.Deliver(testMemoryContext + " 1")
	d.Settle("session-1")
	d.Deliver(testMemoryContext + " 2")
	d.Settle("session-2")
	d.Keep()
	if kept, _ := r.take(); !reflect.DeepEqual(kept, []string{"session-2 " + testMemoryContext + " 2"}) {
		t.Fatalf("kept %q", kept)
	}

	d.Expect(SessionStartAfterStart)
	d.Deliver(testMemoryContext)
	d.Settle("")
	d.Keep()
	if kept, _ := r.take(); len(kept) != 0 {
		t.Fatalf("kept without a session: %q", kept)
	}
	d.Settle("session-1")
	d.Keep()
	if kept, _ := r.take(); len(kept) != 0 {
		t.Fatalf("a context outlived the Settle that had no session: %q", kept)
	}
}

// A compaction forgets the receipt, kept or held, Expect's or
// ExpectAfresh's; a start and a resume do not.
func TestSessionStartDeliveryForgetsReceiptsAtACompaction(t *testing.T) {
	t.Parallel()
	var r testReceipts
	d := newTestDeliveryWithReceipts(t, testHook(), r.receipts())
	d.Deliver(testMemoryContext)
	d.Settle("session-1")
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
	d.Settle("session-1")
	if got := d.ExpectAfresh(SessionStartAfterCompaction); got != "" {
		t.Fatalf("warning = %q", got)
	}
	d.Keep()
	if kept, forgotten := r.take(); len(kept) != 0 || forgotten != 1 {
		t.Fatalf("ExpectAfresh kept %q and forgot %d times", kept, forgotten)
	}

	r.forgetErr = errors.New("disk full")
	if got, want := d.Expect(SessionStartAfterCompaction), "Agent Overflow could not record that this compaction replaced what the SessionStart hook delivered, so a later resume may not deliver Agent Overflow memory again: disk full"; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}
}

func TestSessionStartDeliveryWarnsWhenAReceiptIsNotKept(t *testing.T) {
	t.Parallel()
	r := testReceipts{keepErr: errors.New("database is locked")}
	d := newTestDeliveryWithReceipts(t, testHook(), r.receipts())
	d.Expect(SessionStartAfterStart)
	d.Deliver(testMemoryContext)
	if got := d.Settle("session-1"); got != "" {
		t.Fatalf("warning = %q", got)
	}
	if got, want := d.Keep(), "Agent Overflow could not record that Agent Overflow memory reached this session, so its next resume delivers it again: database is locked"; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}
}

// DropDeliveries forgets what was delivered and failed since the last
// Settle, and the receipt held, and keeps the expectation open.
func TestSessionStartDeliveryDropsDeliveries(t *testing.T) {
	t.Parallel()
	var r testReceipts
	d := newTestDeliveryWithReceipts(t, testHook(), r.receipts())
	d.Deliver(testMemoryContext)
	d.Settle("session-1")
	d.Expect(SessionStartAfterStart)
	d.Deliver(testMemoryContext)
	d.Fail("exit code 1")
	d.DropDeliveries()
	if d.Expecting() != SessionStartAfterStart || !d.Pending() {
		t.Fatalf("expecting %q, pending %v", d.Expecting(), d.Pending())
	}
	if got := d.Settle("session-1"); got != testMissingAtStart {
		t.Fatalf("warning = %q", got)
	}
	d.Keep()
	if kept, _ := r.take(); len(kept) != 0 {
		t.Fatalf("kept %q", kept)
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
	d := newTestDeliveryWithReceipts(t, testHook(), r.receipts())
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
