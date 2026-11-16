package testutil

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/logging"
)

// SlowStep is one parsed logging.LogSlowStep line.
type SlowStep struct {
	Component string
	Step      string
	Duration  time.Duration
	Outcome   string
	// Line is the whole log line, for a test to check what it must not carry.
	Line string
}

var slowStepLine = regexp.MustCompile(`(\S+): slow step=(\S+) duration=(\S+) outcome=(\S+)$`)

// SlowSteps parses every slow-step line in captured log text, in order.
// Other lines are skipped; a slow-step line that does not parse is an error.
func SlowSteps(logs string) ([]SlowStep, error) {
	var steps []SlowStep
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, ": slow step=") {
			continue
		}
		match := slowStepLine.FindStringSubmatch(line)
		if match == nil {
			return nil, fmt.Errorf("malformed slow-step line %q", line)
		}
		duration, err := time.ParseDuration(match[3])
		if err != nil {
			return nil, fmt.Errorf("slow-step line %q: %w", line, err)
		}
		steps = append(steps, SlowStep{Component: match[1], Step: match[2], Duration: duration, Outcome: match[4], Line: line})
	}
	return steps, nil
}

// CheckedSlowSteps parses the slow-step lines in logs and fails the test on
// a malformed line, a line from another component, or a line that carries
// any of secrets.
func CheckedSlowSteps(t *testing.T, logs, component string, secrets ...string) []SlowStep {
	t.Helper()
	steps, err := SlowSteps(logs)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.Component != component {
			t.Errorf("slow step %q is not from %s", step.Line, component)
		}
		for _, secret := range secrets {
			if strings.Contains(step.Line, secret) {
				t.Errorf("slow step %q carries %q", step.Line, secret)
			}
		}
	}
	return steps
}

// ExpectOneSlowStep fails unless steps is exactly one line for step with
// outcome, lasting at least atLeast.
func ExpectOneSlowStep(t *testing.T, steps []SlowStep, step, outcome string, atLeast time.Duration) {
	t.Helper()
	if len(steps) != 1 {
		t.Fatalf("slow steps = %+v, want one %s line", steps, step)
	}
	got := steps[0]
	if got.Step != step || got.Outcome != outcome || got.Duration < atLeast {
		t.Fatalf("slow step = %+v, want step=%s outcome=%s duration>=%s", got, step, outcome, atLeast)
	}
}

// LowerSlowStepThreshold sets the slow-step threshold for the rest of the
// test. Not for a parallel test: the threshold is process-wide.
func LowerSlowStepThreshold(t *testing.T, threshold time.Duration) {
	t.Helper()
	t.Cleanup(logging.SetSlowStepThreshold(threshold))
}
