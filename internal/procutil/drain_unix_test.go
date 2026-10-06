//go:build !windows

package procutil

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// slowWriter stalls every write, the shape of a stream to a slow consumer.
type slowWriter struct {
	buf   bytes.Buffer
	delay time.Duration
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return w.buf.Write(p)
}

// TestRunDrainedDeliversOutputBufferedBehindASlowWriter: output still in the
// pipe when the process exits reaches a writer that stalls longer than
// linger, rather than being cut off and reported as success.
func TestRunDrainedDeliversOutputBufferedBehindASlowWriter(t *testing.T) {
	const size = 256 << 10
	out := &slowWriter{delay: 150 * time.Millisecond}
	cmd := exec.Command("sh", "-c", "head -c 262144 /dev/zero | tr '\\0' x")
	if err := RunDrained(context.Background(), cmd, out, nil, 20*time.Millisecond); err != nil {
		t.Fatalf("RunDrained: %v", err)
	}
	if got := out.buf.Len(); got != size {
		t.Fatalf("received %d bytes, want %d", got, size)
	}
}

// TestRunDrainedReturnsWhenADescendantHoldsThePipe: a process the command
// started keeps stdout open after the command exits; the command's own
// output is delivered and the run ends after linger, not with the
// descendant.
func TestRunDrainedReturnsWhenADescendantHoldsThePipe(t *testing.T) {
	var out bytes.Buffer
	cmd := exec.Command("sh", "-c", "sleep 3 & echo done")
	start := time.Now()
	if err := RunDrained(context.Background(), cmd, &out, nil, 100*time.Millisecond); err != nil {
		t.Fatalf("RunDrained: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("RunDrained waited %s for the descendant", elapsed)
	}
	if got := strings.TrimSpace(out.String()); got != "done" {
		t.Fatalf("stdout = %q, want done", got)
	}
}

type failingWriter struct{}

var errWriterFailed = errors.New("writer failed")

func (failingWriter) Write([]byte) (int, error) { return 0, errWriterFailed }

// TestRunDrainedReportsAFailedWriter: a writer error is the run's error even
// when the process itself succeeds.
func TestRunDrainedReportsAFailedWriter(t *testing.T) {
	cmd := exec.Command("sh", "-c", "echo out")
	if err := RunDrained(context.Background(), cmd, failingWriter{}, nil, 100*time.Millisecond); !errors.Is(err, errWriterFailed) {
		t.Fatalf("RunDrained = %v, want the writer's error", err)
	}
}

// TestRunDrainedReportsTheExitAndKeepsStderr: a failed process is an error,
// and what it wrote to stderr is delivered.
func TestRunDrainedReportsTheExitAndKeepsStderr(t *testing.T) {
	var stderr bytes.Buffer
	cmd := exec.Command("sh", "-c", "echo broken >&2; exit 3")
	err := RunDrained(context.Background(), cmd, nil, &stderr, 100*time.Millisecond)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("RunDrained = %v, want exit status 3", err)
	}
	if got := strings.TrimSpace(stderr.String()); got != "broken" {
		t.Fatalf("stderr = %q, want broken", got)
	}
}

// TestRunDrainedRefusesAssignedOutputs: the helper owns both pipes.
func TestRunDrainedRefusesAssignedOutputs(t *testing.T) {
	cmd := exec.Command("true")
	cmd.Stdout = &bytes.Buffer{}
	if err := RunDrained(context.Background(), cmd, nil, nil, time.Second); err == nil {
		t.Fatal("RunDrained accepted a command whose stdout was already assigned")
	}
}

// TestRunDrainedStopsAtCancellationWhileADescendantKeepsWriting: the
// command exits at once, a descendant keeps the pipe busy more often than
// linger, and the context ends the run. exec no longer watches the context
// once the process has exited, so only RunDrained can.
func TestRunDrainedStopsAtCancellationWhileADescendantKeepsWriting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "(i=0; while [ $i -lt 100 ]; do echo tick; sleep 0.02; i=$((i+1)); done) & echo done")
	start := time.Now()
	err := RunDrained(ctx, cmd, io.Discard, nil, 100*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("RunDrained ran %s, past its context", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunDrained = %v, want the context's error", err)
	}
}

// TestRunDrainedStopsAtCancellationDuringAFlood: a descendant that writes
// without pause gives every read data, so the copy ends only because
// cancellation interrupts it: the past read deadline, or the stop check
// for a copy that re-armed its deadline after it. Closing the pipe then
// ends the descendant with SIGPIPE.
func TestRunDrainedStopsAtCancellationDuringAFlood(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "dd if=/dev/zero bs=65536 count=1000000 2>/dev/null & echo done")
	start := time.Now()
	err := RunDrained(ctx, cmd, io.Discard, nil, 100*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("RunDrained ran %s, past its context", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunDrained = %v, want the context's error", err)
	}
}
