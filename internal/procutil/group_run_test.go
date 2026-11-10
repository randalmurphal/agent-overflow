//go:build !windows

package procutil

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func shellCommand(t *testing.T, script string, output *syncBuffer) GroupCommand {
	t.Helper()
	return GroupCommand{
		Dir:    t.TempDir(),
		Env:    []string{"PATH=" + os.Getenv("PATH")},
		Argv:   []string{"/bin/sh", "-c", script},
		Output: output,
	}
}

func waitForOutput(t *testing.T, output *syncBuffer, marker string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), marker) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %q", marker, output.String())
}

func TestRunGroupReportsTheExitCodeAndCombinedOutput(t *testing.T) {
	t.Parallel()
	var output syncBuffer
	outcome, err := RunGroup(context.Background(),
		shellCommand(t, "echo to-stdout; echo to-stderr 1>&2; exit 3", &output))
	if err == nil || outcome.ExitCode != 3 || outcome.Leftovers {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if text := output.String(); !strings.Contains(text, "to-stdout") || !strings.Contains(text, "to-stderr") {
		t.Fatalf("output lost a stream: %q", text)
	}
}

func TestRunGroupRefusesAnEmptyArgv(t *testing.T) {
	t.Parallel()
	if _, err := RunGroup(context.Background(), GroupCommand{Output: &syncBuffer{}}); err == nil {
		t.Fatal("empty argv was accepted")
	}
}

func TestRunGroupReportsAStartFailure(t *testing.T) {
	t.Parallel()
	outcome, err := RunGroup(context.Background(), GroupCommand{
		Dir: t.TempDir(), Argv: []string{"/nonexistent/ao-procutil-binary"}, Output: &syncBuffer{},
	})
	if err == nil || outcome.ExitCode != -1 {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
}

// A command that exits while a child it started keeps running finishes with
// its own exit code, its child's output captured and the child stopped.
func TestRunGroupStopsLeftoverProcessesAndKeepsExitCode(t *testing.T) {
	t.Parallel()
	var output syncBuffer
	started := time.Now()
	outcome, err := RunGroup(context.Background(), shellCommand(t,
		`sh -c 'echo worker-started; while :; do sleep 0.05; echo worker-alive; done' &
sleep 0.3; echo leader-done`, &output))
	if err != nil || outcome.ExitCode != 0 || !outcome.Leftovers {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if elapsed := time.Since(started); elapsed > DefaultTerminateGrace+groupDrainGrace+5*time.Second {
		t.Fatalf("leftover sweep took %s", elapsed)
	}
	text := output.String()
	if !strings.Contains(text, "leader-done") || !strings.Contains(text, "worker-started") {
		t.Fatalf("output lost the leader or its child: %q", text)
	}
}

func TestRunGroupTerminatesBeforeKilling(t *testing.T) {
	t.Parallel()
	var output syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan GroupOutcome, 1)
	go func() {
		outcome, _ := RunGroup(ctx, shellCommand(t,
			`trap 'echo graceful-term; exit 0' TERM; echo graceful-started; while :; do sleep 0.05; done`, &output))
		done <- outcome
	}()
	waitForOutput(t, &output, "graceful-started")
	cancel()
	outcome := <-done
	if outcome.ExitCode != 0 || outcome.Leftovers || !strings.Contains(output.String(), "graceful-term") {
		t.Fatalf("SIGTERM did not reach the command first: outcome=%+v output=%q", outcome, output.String())
	}
}

func TestRunGroupKillsWhatIgnoresTerminate(t *testing.T) {
	t.Parallel()
	const grace = 300 * time.Millisecond
	var output syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan GroupOutcome, 1)
	go func() {
		command := shellCommand(t, `trap '' TERM; echo stubborn-started; sleep 60`, &output)
		command.TerminateGrace = grace
		outcome, _ := RunGroup(ctx, command)
		done <- outcome
	}()
	waitForOutput(t, &output, "stubborn-started")
	canceled := time.Now()
	cancel()
	select {
	case outcome := <-done:
		if outcome.ExitCode == 0 || time.Since(canceled) < grace {
			t.Fatalf("stubborn command was not killed after the grace: %+v after %s", outcome, time.Since(canceled))
		}
	case <-time.After(grace + 10*time.Second):
		t.Fatal("stubborn command outlived the kill")
	}
}
