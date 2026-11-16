package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// The attempts a process remembers are bounded, and it never says an
// attempt it may have received never arrived: an attempt whose outcome
// was dropped reads forgotten, and so does every id it does not hold once
// ids have left its window too. A repeat of a forgotten id writes nothing.
func TestWriteAttemptsForgetTheOldestFinished(t *testing.T) {
	t.Parallel()
	var attempts writeAttempts[int]
	refuse := attemptRefusals{other: errors.New("other"), forgotten: errors.New("forgotten"), panicked: errors.New("panicked")}
	finish := func(i int) {
		if _, err := attempts.run(context.Background(), fmt.Sprintf("a%d", i), "key", refuse, func() (int, error) { return i, nil }); err != nil {
			t.Fatal(err)
		}
	}
	for i := range writeAttemptsKept + 1 {
		finish(i)
	}
	if kept, _ := attempts.find(fmt.Sprintf("a%d", writeAttemptsKept), "key"); kept == nil {
		t.Fatal("forgot the newest attempt")
	}
	if kept, forgotten := attempts.find("a0", "key"); kept != nil || !forgotten {
		t.Fatalf("the evicted attempt = %v, forgotten %v; want forgotten", kept, forgotten)
	}
	if _, forgotten := attempts.find("never-sent", "key"); forgotten {
		t.Fatal("an id never received reads forgotten while every id is kept")
	}
	if len(attempts.byID) != writeAttemptsKept {
		t.Fatalf("%d outcomes kept, want %d", len(attempts.byID), writeAttemptsKept)
	}
	wrote := false
	if _, err := attempts.run(context.Background(), "a0", "key", refuse, func() (int, error) { wrote = true; return 0, nil }); err != refuse.forgotten || wrote {
		t.Fatalf("a repeat of a forgotten id = %v, wrote %t; want the forgotten refusal and no write", err, wrote)
	}

	for i := writeAttemptsKept + 1; i <= writeAttemptsKept+writeAttemptIDsKept+1; i++ {
		finish(i)
	}
	if len(attempts.forgotten) != writeAttemptIDsKept {
		t.Fatalf("%d ids kept, want %d", len(attempts.forgotten), writeAttemptIDsKept)
	}
	if _, forgotten := attempts.find("never-sent", "key"); !forgotten {
		t.Fatal("an id not held reads never received after ids overflowed")
	}
}

// A repeated id writes nothing: it answers with the first arrival's
// outcome, waiting for one still running, and refuses one the first
// arrival used for another key. A write that panics still settles.
func TestWriteAttemptsAnswerARepeatFromTheFirstArrival(t *testing.T) {
	t.Parallel()
	var attempts writeAttempts[int]
	refuse := attemptRefusals{other: errors.New("other"), forgotten: errors.New("forgotten"), panicked: errors.New("panicked")}
	started, release := make(chan struct{}), make(chan struct{})
	first := make(chan int, 1)
	go func() {
		got, _ := attempts.run(context.Background(), "a", "key", refuse, func() (int, error) { close(started); <-release; return 7, nil })
		first <- got
	}()
	<-started
	repeat := make(chan int, 1)
	writes := 0
	go func() {
		got, _ := attempts.run(context.Background(), "a", "key", refuse, func() (int, error) { writes++; return 8, nil })
		repeat <- got
	}()
	close(release)
	if got, again := <-first, <-repeat; got != 7 || again != 7 || writes != 0 {
		t.Fatalf("first %d, repeat %d, repeat writes %d; want 7, 7 and none", got, again, writes)
	}
	if _, err := attempts.run(context.Background(), "a", "elsewhere", refuse, func() (int, error) { writes++; return 0, nil }); err != refuse.other || writes != 0 {
		t.Fatalf("a repeat to another key = %v, writes %d; want the other refusal", err, writes)
	}
	func() {
		defer func() { _ = recover() }()
		_, _ = attempts.run(context.Background(), "p", "key", refuse, func() (int, error) { panic("boom") })
	}()
	attempt, _ := attempts.find("p", "key")
	if _, err := attempt.wait(context.Background()); err != refuse.panicked {
		t.Fatalf("a panicked attempt = %v; want it settled as panicked", err)
	}
}
