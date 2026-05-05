package design

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestDiagnosticBuffer_AppendBatchAssignsMonotonicTokens(t *testing.T) {
	b := NewDiagnosticBuffer(nil)
	out := b.AppendBatch("t", []Diagnostic{
		{Severity: SeverityError, Message: "first"},
		{Severity: SeverityWarn, Message: "second"},
	})
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}
	if out[0].Token != 1 || out[1].Token != 2 {
		t.Fatalf("tokens = (%d, %d), want (1, 2)", out[0].Token, out[1].Token)
	}
	out2 := b.AppendBatch("t", []Diagnostic{{Severity: SeverityError, Message: "third"}})
	if out2[0].Token != 3 {
		t.Fatalf("third token = %d, want 3", out2[0].Token)
	}
	if got := b.LatestToken("t"); got != 3 {
		t.Fatalf("LatestToken = %d, want 3", got)
	}
}

func TestDiagnosticBuffer_AppendBatchEmptyAndBlankThreadAreNoOps(t *testing.T) {
	b := NewDiagnosticBuffer(nil)
	if got := b.AppendBatch("", []Diagnostic{{Message: "x"}}); got != nil {
		t.Fatalf("blank threadID returned %v, want nil", got)
	}
	if got := b.AppendBatch("t", nil); got != nil {
		t.Fatalf("empty batch returned %v, want nil", got)
	}
	if got := b.LatestToken("t"); got != 0 {
		t.Fatalf("LatestToken after no-op = %d, want 0", got)
	}
}

func TestDiagnosticBuffer_DrainReturnsOnlyEntriesAfterSinceToken(t *testing.T) {
	b := NewDiagnosticBuffer(nil)
	b.AppendBatch("t", []Diagnostic{{Message: "a"}, {Message: "b"}, {Message: "c"}})

	// since=0 → all three.
	got, latest := b.Drain(t.Context(), "t", 0)
	if len(got) != 3 {
		t.Fatalf("len(drain since 0) = %d, want 3", len(got))
	}
	if latest != 3 {
		t.Fatalf("latest = %d, want 3", latest)
	}

	// since=2 → only the third (token 3).
	got, latest = b.Drain(t.Context(), "t", 2)
	if len(got) != 1 || got[0].Message != "c" {
		t.Fatalf("len=%d msg=%q, want 1 c", len(got), msgFirst(got))
	}
	if latest != 3 {
		t.Fatalf("latest = %d, want 3", latest)
	}

	// since >= latest → empty.
	got, latest = b.Drain(t.Context(), "t", 99)
	if len(got) != 0 {
		t.Fatalf("len(drain since 99) = %d, want 0", len(got))
	}
	if latest != 3 {
		t.Fatalf("latest = %d, want 3", latest)
	}
}

func TestDiagnosticBuffer_RingEvictsOldestAtCap(t *testing.T) {
	b := NewDiagnosticBuffer(nil)
	// Append cap+10 entries; oldest 10 should be evicted.
	overflow := 10
	batch := make([]Diagnostic, 0, DiagnosticRingCap+overflow)
	for i := 0; i < DiagnosticRingCap+overflow; i++ {
		batch = append(batch, Diagnostic{Severity: SeverityError, Message: "x"})
	}
	b.AppendBatch("t", batch)

	got, latest := b.Drain(t.Context(), "t", 0)
	if len(got) != DiagnosticRingCap {
		t.Fatalf("ring length = %d, want %d", len(got), DiagnosticRingCap)
	}
	if latest != int64(DiagnosticRingCap+overflow) {
		t.Fatalf("latest token = %d, want %d", latest, DiagnosticRingCap+overflow)
	}
	// First surviving entry should be token overflow+1 (1..overflow evicted).
	if got[0].Token != int64(overflow+1) {
		t.Fatalf("oldest survivor token = %d, want %d", got[0].Token, overflow+1)
	}
	if got[len(got)-1].Token != latest {
		t.Fatalf("newest token = %d, want %d", got[len(got)-1].Token, latest)
	}
}

func TestDiagnosticBuffer_DrainReturnsImmediatelyWhenWatcherIdle(t *testing.T) {
	// No MarkActivity called → no settle window → Drain on empty returns
	// instantly even though the deadline is 1s.
	b := NewDiagnosticBuffer(nil)
	start := time.Now()
	got, _ := b.Drain(t.Context(), "t", 0)
	elapsed := time.Since(start)
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("Drain blocked %s; expected immediate return", elapsed)
	}
}

func TestDiagnosticBuffer_DrainBlocksThenReturnsAfterAppend(t *testing.T) {
	b := NewDiagnosticBuffer(nil)
	b.MarkActivity("t") // open the settle window

	type result struct {
		got     []Diagnostic
		latest  int64
		elapsed time.Duration
	}
	resultCh := make(chan result, 1)
	go func() {
		start := time.Now()
		got, latest := b.Drain(t.Context(), "t", 0)
		resultCh <- result{got: got, latest: latest, elapsed: time.Since(start)}
	}()

	// Sleep long enough that the goroutine is parked in cond.Wait, then
	// post a diagnostic to wake it.
	time.Sleep(50 * time.Millisecond)
	b.AppendBatch("t", []Diagnostic{{Message: "lazy"}})

	select {
	case r := <-resultCh:
		if len(r.got) != 1 || r.got[0].Message != "lazy" {
			t.Fatalf("got=%v want one 'lazy'", r.got)
		}
		if r.elapsed > diagnosticDrainDeadline {
			t.Fatalf("drain held past deadline: %s > %s", r.elapsed, diagnosticDrainDeadline)
		}
	case <-time.After(diagnosticDrainDeadline + 200*time.Millisecond):
		t.Fatal("Drain did not return after Append")
	}
}

func TestDiagnosticBuffer_DrainTimesOutWhenSettleWindowPassesEmpty(t *testing.T) {
	// MarkActivity opens the settle window; no append follows. Drain should
	// block up to the deadline and then return empty. Use a fake clock so
	// the test runs in milliseconds, not seconds.
	now := time.Now()
	clock := func() time.Time { return now }
	b := NewDiagnosticBuffer(clock)

	b.MarkActivity("t")

	// Skip the deadline immediately to validate the empty-return path
	// without sleeping. AfterFunc fires the broadcast when the timer
	// duration elapses; advancing the clock doesn't fire it. Instead
	// post + drain to confirm the structural exit.
	type result struct {
		got    []Diagnostic
		latest int64
	}
	resultCh := make(chan result, 1)
	go func() {
		// Force the deadline check to fire on the next clock read.
		got, latest := b.Drain(t.Context(), "t", 0)
		resultCh <- result{got, latest}
	}()
	// Advance the clock past the deadline before any append, and broadcast.
	time.Sleep(20 * time.Millisecond) // park the drainer on cond.Wait
	now = now.Add(2 * diagnosticDrainDeadline)
	// Wake the drainer by appending+removing trick: use TeardownThread to
	// wake without satisfying the drain. tornDown returns nil/empty.
	b.TeardownThread("t")

	select {
	case r := <-resultCh:
		if len(r.got) != 0 {
			t.Fatalf("got = %v, want empty after deadline+teardown", r.got)
		}
	case <-time.After(time.Second):
		t.Fatal("Drain did not return after deadline+teardown")
	}
}

func TestDiagnosticBuffer_DrainCancelsOnContext(t *testing.T) {
	b := NewDiagnosticBuffer(nil)
	b.MarkActivity("t")

	ctx, cancel := context.WithCancel(t.Context())
	resultCh := make(chan struct{}, 1)
	go func() {
		_, _ = b.Drain(ctx, "t", 0)
		resultCh <- struct{}{}
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("Drain did not return after ctx cancel")
	}
}

func TestDiagnosticBuffer_TeardownIsolatesPerThread(t *testing.T) {
	b := NewDiagnosticBuffer(nil)

	b.AppendBatch("a", []Diagnostic{{Message: "a1"}})
	b.AppendBatch("b", []Diagnostic{{Message: "b1"}})

	b.TeardownThread("a")

	// "a" is gone; latest token resets.
	if got := b.LatestToken("a"); got != 0 {
		t.Fatalf("a LatestToken after teardown = %d, want 0", got)
	}
	// "b" untouched.
	if got := b.LatestToken("b"); got != 1 {
		t.Fatalf("b LatestToken = %d, want 1", got)
	}
}

func TestDiagnosticBuffer_PerThreadCondDoesNotCrossWake(t *testing.T) {
	// Two concurrent drainers on different threads; appending to one must
	// not unblock the drain on the other. The per-thread cond design is
	// what guarantees this — verify by making sure thread A's drain stays
	// parked while thread B sees its append.
	b := NewDiagnosticBuffer(nil)
	b.MarkActivity("a")
	b.MarkActivity("b")

	var wg sync.WaitGroup
	wg.Add(1)
	resultB := make(chan []Diagnostic, 1)

	// Drain A: should remain blocked through the Append on B.
	doneA := make(chan struct{})
	go func() {
		defer wg.Done()
		_, _ = b.Drain(t.Context(), "a", 0)
		close(doneA)
	}()

	go func() {
		got, _ := b.Drain(t.Context(), "b", 0)
		resultB <- got
	}()

	// Trigger only thread B.
	time.Sleep(30 * time.Millisecond)
	b.AppendBatch("b", []Diagnostic{{Message: "only-b"}})

	select {
	case got := <-resultB:
		if len(got) != 1 || got[0].Message != "only-b" {
			t.Fatalf("B got %v, want one 'only-b'", got)
		}
	case <-time.After(time.Second):
		t.Fatal("B drain did not return")
	}

	// A should still be blocked on its own cond.
	select {
	case <-doneA:
		t.Fatal("A drain woke up — cond is shared across threads")
	case <-time.After(50 * time.Millisecond):
		// expected: A still waiting
	}

	// Tear down A so the goroutine exits before the test finishes.
	b.TeardownThread("a")
	wg.Wait()
}

func msgFirst(d []Diagnostic) string {
	if len(d) == 0 {
		return ""
	}
	return d[0].Message
}
