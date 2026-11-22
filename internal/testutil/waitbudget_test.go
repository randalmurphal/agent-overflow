package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The budget is the shared budget, cut short by the run's timeout less the
// reporting margin, whenever it is asked; once that is gone it is an error.
func TestTheBudgetEndsBeforeTheRunsTimeoutWheneverItIsAsked(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name     string
		deadline time.Time
		ok       bool
		want     time.Duration
	}{
		{name: "a run without a timeout", want: waitBudget},
		{name: "early in a long run", deadline: now.Add(time.Hour), ok: true, want: waitBudget},
		{name: "a step that starts 40 s before the timeout", deadline: now.Add(40 * time.Second), ok: true, want: 30 * time.Second},
	} {
		got, err := budgetLeft(c.deadline, c.ok, now)
		if err != nil || got != c.want {
			t.Errorf("%s: budget = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	for _, left := range []time.Duration{waitReportMargin, time.Second, -time.Minute} {
		if got, err := budgetLeft(now.Add(left), true, now); err == nil || !strings.Contains(err.Error(), "no wait budget is left") {
			t.Errorf("with the timeout %v away: budget = %v, %v; want the exhausted error", left, got, err)
		}
	}
}

// A run whose timeout leaves no budget at all: WaitBudget fails its test at
// once with its own message, and StepBudget answers an error.
func TestAnExhaustedBudgetFailsTheTestAtOnce(t *testing.T) {
	t.Parallel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestExhaustedBudgetChild$", "-test.timeout=5s", "-test.v")
	cmd.Env = append(os.Environ(), exhaustedBudgetChild+"=1")
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err == nil {
		t.Fatalf("the child run passed:\n%s", text)
	}
	for _, want := range []string{"step budget: testutil: no wait budget is left", "--- FAIL: TestExhaustedBudgetChild", "no wait budget is left"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the child run's output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "past WaitBudget") || strings.Contains(text, "panic: test timed out") {
		t.Fatalf("the child ran on past an exhausted WaitBudget:\n%s", text)
	}
}

const exhaustedBudgetChild = "AO_TESTUTIL_EXHAUSTED_BUDGET_CHILD"

// TestExhaustedBudgetChild runs only as TestAnExhaustedBudgetFailsTheTestAtOnce's
// child, under a 5 s timeout.
func TestExhaustedBudgetChild(t *testing.T) {
	if os.Getenv(exhaustedBudgetChild) == "" {
		t.Skip("runs only as the child of TestAnExhaustedBudgetFailsTheTestAtOnce")
	}
	if _, err := StepBudget(t)(); err != nil {
		fmt.Println("step budget:", err)
	}
	WaitBudget(t)
	fmt.Println("past WaitBudget")
}
