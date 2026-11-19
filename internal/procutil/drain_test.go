package procutil

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestRunDrainedHelperSleeps is the child of the test below, not a test.
func TestRunDrainedHelperSleeps(t *testing.T) {
	if os.Getenv("PROCUTIL_TEST_SLEEP") != "1" {
		t.Skip("helper process")
	}
	time.Sleep(time.Minute)
}

// TestRunDrainedReportsTheContextThatKilledTheProcess: a process the
// context killed before it exited reports the context's error, not only its
// "signal: killed" exit. Once it is dead its pipes are at EOF, so the drain
// can finish before it looks at the context: each run here is a draw of
// that order.
func TestRunDrainedReportsTheContextThatKilledTheProcess(t *testing.T) {
	for range 40 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunDrainedHelperSleeps$")
		cmd.Env = append(os.Environ(), "PROCUTIL_TEST_SLEEP=1")
		err := RunDrained(ctx, cmd, nil, nil, time.Minute)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("RunDrained of a process the context killed = %v, want the context's error", err)
		}
	}
}
