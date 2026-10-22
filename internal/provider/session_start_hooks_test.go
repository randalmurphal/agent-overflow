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
	return newTestDeliveryMissing(t, hook, nil)
}

func newTestDeliveryMissing(t *testing.T, hook *SessionStartHook, missed func() error) *SessionStartDelivery {
	t.Helper()
	d, err := NewSessionStartDelivery(hook, missed)
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

func TestSessionStartDeliveryWithoutAHookIsNilAndInert(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, nil)
	if d != nil {
		t.Fatalf("delivery without a hook = %+v", d)
	}
	d.Expect(SessionStartAfterStart)
	d.ExpectAfresh(SessionStartAfterCompaction)
	d.Deliver(testMemoryContext)
	d.Fail("exit code 1")
	d.Drop()
	if d.Expecting() != "" || d.Pending() || d.Settle() != "" || d.ForgetWhilePending() != "" || d.Hook() != nil {
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
	if got := d.Settle(); got != testMissingAtStart {
		t.Fatalf("warning = %q, want %q", got, testMissingAtStart)
	}
	if d.Expecting() != "" || d.Settle() != "" {
		t.Fatal("Settle did not end the expectation")
	}

	d.Expect(SessionStartAfterCompaction)
	d.Deliver("## Memo\n\n- a")
	if got, want := d.Settle(), "Agent Overflow memory did not reach this session after its compaction: no SessionStart hook output carried it. A hook failed, or hooks are turned off in the provider's own settings."; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}

	d.Expect(SessionStartAfterCompaction)
	d.Deliver(testMemoryContext)
	if got := d.Settle(); got != "" {
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
	if got, want := d.Settle(), "Agent Overflow memory did not reach this session after its start: a SessionStart hook failed (exit code 127: [31msh: /gone/agent-overflow: not found [0m)."; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}

	// Settle forgot it, and a failure when the hook delivered (the user's
	// own hook) warns about nothing.
	d.Expect(SessionStartAfterCompaction)
	d.Fail("exit code 2")
	d.Deliver(testMemoryContext)
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

// Only context that opens with the hook's heading is its output.
func TestSessionStartDeliveryCountsOnlyContextOpeningWithTheHeading(t *testing.T) {
	t.Parallel()
	d := newTestDelivery(t, testHook())
	for _, context := range []string{"", "user hook output", " ## Memory", "Quoted: ## Memory", "## Memo"} {
		d.Deliver(context)
	}
	d.Expect(SessionStartAfterStart)
	if got := d.Settle(); got != testMissingAtStart {
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
	if got := d.Settle(); got != "" {
		t.Fatalf("Expect after the delivery: %q", got)
	}

	d.Deliver(testMemoryContext)
	d.Fail("exit code 1")
	d.ExpectAfresh(SessionStartAfterCompaction)
	if got := d.Settle(); !strings.HasPrefix(got, "Agent Overflow memory did not reach this session after its compaction: no SessionStart hook output") {
		t.Fatalf("ExpectAfresh kept the earlier delivery or failure: %q", got)
	}

	// Settle forgets a delivery even when nothing was expected.
	d.Deliver(testMemoryContext)
	if got := d.Settle(); got != "" {
		t.Fatalf("Settle with nothing expected = %q", got)
	}
	d.Expect(SessionStartAfterStart)
	if got := d.Settle(); got != testMissingAtStart {
		t.Fatalf("a delivery survived Settle: %q", got)
	}
}

// Drop ends the open expectation without a warning and forgets what was
// delivered and failed, so a later expectation does not count them.
func TestSessionStartDeliveryDrops(t *testing.T) {
	t.Parallel()
	forgot := 0
	d := newTestDeliveryMissing(t, testHook(), func() error { forgot++; return nil })
	d.Expect(SessionStartAfterStart)
	d.Deliver(testMemoryContext)
	d.Fail("exit code 1")
	d.Drop()
	if d.Expecting() != "" || d.Pending() {
		t.Fatalf("expecting %q, pending %v after Drop", d.Expecting(), d.Pending())
	}
	if got := d.Settle(); got != "" || forgot != 0 {
		t.Fatalf("Settle after Drop = %q, forgot %d times", got, forgot)
	}
	d.Expect(SessionStartAfterStart)
	if got := d.Settle(); got != testMissingAtStart+" The next prompt delivers it whole." {
		t.Fatalf("a dropped delivery counted for a later expectation: %q", got)
	}
}

// ForgetWhilePending runs forget only while the open expectation waits for
// the hook's output, keeps it open, and warns only when forget failed.
func TestSessionStartDeliveryForgetsWhilePending(t *testing.T) {
	t.Parallel()
	forgot := 0
	var forgetErr error
	d := newTestDeliveryMissing(t, testHook(), func() error { forgot++; return forgetErr })
	if got := d.ForgetWhilePending(); got != "" || forgot != 0 {
		t.Fatalf("with no expectation: warning %q, forgot %d times", got, forgot)
	}
	d.Expect(SessionStartAfterCompaction)
	if got := d.ForgetWhilePending(); got != "" || forgot != 1 || !d.Pending() {
		t.Fatalf("pending: warning %q, forgot %d times, pending %v", got, forgot, d.Pending())
	}
	forgetErr = errors.New("disk full")
	if got, want := d.ForgetWhilePending(), "Agent Overflow could not record that Agent Overflow memory has not reached this session since its compaction, so a later prompt may deliver only what changed: disk full"; got != want || forgot != 2 {
		t.Fatalf("warning = %q (forgot %d), want %q", got, forgot, want)
	}
	d.Deliver(testMemoryContext)
	if got := d.ForgetWhilePending(); got != "" || forgot != 2 {
		t.Fatalf("delivered: warning %q, forgot %d times", got, forgot)
	}
	if got := newTestDelivery(t, testHook()).ForgetWhilePending(); got != "" {
		t.Fatalf("a delivery without forget warned: %q", got)
	}
}

// A delivery that did not arrive runs forget, which forgets the thread's
// records so the next prompt delivers the memory whole, and the warning
// says so, or that it could not; one that arrived, or none expected, does
// not run it.
func TestSessionStartDeliveryRunsMissedWhenTheHookDidNotDeliver(t *testing.T) {
	t.Parallel()
	missed := 0
	var missedErr error
	d := newTestDeliveryMissing(t, testHook(), func() error { missed++; return missedErr })
	d.Expect(SessionStartAfterStart)
	d.Deliver(testMemoryContext)
	d.Settle()
	d.Settle()
	if missed != 0 {
		t.Fatalf("missed ran %d times for a delivery that arrived", missed)
	}
	d.Expect(SessionStartAfterCompaction)
	if got, want := d.Settle(), strings.Replace(testMissingAtStart, "its start", "its compaction", 1)+" The next prompt delivers it whole."; got != want || missed != 1 {
		t.Fatalf("warning = %q (missed %d), want %q", got, missed, want)
	}
	missedErr = errors.New("disk full")
	d.Expect(SessionStartAfterStart)
	if got, want := d.Settle(), testMissingAtStart+" Agent Overflow could not record that, so the next prompt may deliver only what changed: disk full"; got != want || missed != 2 {
		t.Fatalf("warning = %q (missed %d), want %q", got, missed, want)
	}
}

// A failed prompt run's warning names the hook and its cause on one line.
func TestPromptHookFailure(t *testing.T) {
	t.Parallel()
	if got, want := PromptHookFailure(testSessionStartHook, "exit code 1:\n__memory-context: boom"), "Changes to Agent Overflow memory may not have reached this turn: its UserPromptSubmit hook failed (exit code 1: __memory-context: boom)."; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
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
	d := newTestDeliveryMissing(t, testHook(), func() error { return nil })
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				switch i % 6 {
				case 0:
					d.Expect(SessionStartAfterStart)
				case 1:
					d.Deliver(testMemoryContext)
				case 2:
					d.Drop()
					d.ForgetWhilePending()
				case 3:
					d.Fail("exit code 1")
				case 4:
					d.ExpectAfresh(SessionStartAfterCompaction)
					d.Pending()
				default:
					d.Settle()
					d.Expecting()
				}
			}
		}()
	}
	wg.Wait()
}
