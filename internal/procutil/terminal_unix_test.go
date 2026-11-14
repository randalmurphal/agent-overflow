//go:build !windows

package procutil

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openTestTerminal opens a terminal and collects everything the command
// writes to it.
func openTestTerminal(t *testing.T) (*Terminal, *syncBuffer, chan struct{}) {
	t.Helper()
	terminal, err := OpenTerminal()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := terminal.Close(); err != nil {
			t.Errorf("close terminal: %v", err)
		}
	})
	written := &syncBuffer{}
	readEnded := make(chan struct{})
	go func() {
		defer close(readEnded)
		_, _ = io.Copy(written, terminal)
	}()
	return terminal, written, readEnded
}

func TestATerminalIsTheControllingTerminalAndNothingElse(t *testing.T) {
	t.Parallel()
	terminal, written, _ := openTestTerminal(t)
	var output syncBuffer
	command := shellCommand(t, `
[ -t 0 ] && echo stdin-is-a-terminal
[ -t 1 ] && echo stdout-is-a-terminal
[ -t 2 ] && echo stderr-is-a-terminal
if (: >&3) 2>/dev/null; then echo descriptor-3-is-open; fi
cat
printf 'ask> ' > /dev/tty
IFS= read -r answer < /dev/tty
echo "answer=$answer"
`, &output)
	command.Terminal = terminal
	done := make(chan error, 1)
	var outcome GroupOutcome
	go func() {
		var err error
		outcome, err = RunGroup(context.Background(), command)
		done <- err
	}()
	waitForOutput(t, written, "ask> ")
	if _, err := terminal.Write([]byte("the answer\n")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || outcome.ExitCode != 0 || outcome.Leftovers || outcome.PipeErr != nil {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	// Stdin is still the null device (cat ended at once), nothing the command
	// inherited is a terminal, and the answer came through /dev/tty.
	if got := output.String(); got != "answer=the answer\n" {
		t.Fatalf("output = %q", got)
	}
	// The terminal side holds the prompt and the echo of what was typed; the
	// output holds neither.
	waitForOutput(t, written, "the answer")
	if strings.Contains(output.String(), "ask> ") {
		t.Fatalf("the prompt reached the output: %q", output.String())
	}
}

// The kernel hangs up a terminal's foreground group when the session leader
// exits. A command's tree ignores that, so what it left running is still
// there for Wait to count and stop.
func TestATerminalDoesNotHangUpTheCommandsProcesses(t *testing.T) {
	t.Parallel()
	terminal, _, _ := openTestTerminal(t)
	var output syncBuffer
	command := shellCommand(t, `kill -HUP $$; echo survived-hangup; sleep 60 & echo $! > pid`, &output)
	command.Terminal = terminal
	command.TerminateGrace = 200 * time.Millisecond
	outcome, err := RunGroup(context.Background(), command)
	pid := recordedPID(t, filepath.Join(command.Dir, "pid"))
	killAtCleanup(t, pid)
	if err != nil || outcome.ExitCode != 0 {
		t.Fatalf("outcome=%+v err=%v output=%q", outcome, err, output.String())
	}
	if !strings.Contains(output.String(), "survived-hangup") {
		t.Fatalf("output = %q", output.String())
	}
	if !outcome.Leftovers {
		t.Fatal("the process the command left running was not counted")
	}
	if running(pid) {
		t.Fatalf("process %d outlived the sweep", pid)
	}
}

func TestACommandThatDoesNotExistFailsToStartWithATerminal(t *testing.T) {
	t.Parallel()
	terminal, _, _ := openTestTerminal(t)
	var output syncBuffer
	missing := filepath.Join(t.TempDir(), "no-such-shell")
	outcome, err := RunGroup(context.Background(), GroupCommand{
		Dir: t.TempDir(), Argv: []string{missing, "-c", "true"}, Output: &output, Terminal: terminal,
	})
	var exitErr *exec.ExitError
	if err == nil || errors.As(err, &exitErr) || outcome.ExitCode != -1 {
		t.Fatalf("outcome=%+v err=%v, want a start failure", outcome, err)
	}
	if !strings.Contains(err.Error(), "no-such-shell") || output.String() != "" {
		t.Fatalf("err=%v output=%q", err, output.String())
	}
}

func TestARelativeCommandIsFoundInTheCommandsDirectoryWithATerminal(t *testing.T) {
	t.Parallel()
	terminal, _, _ := openTestTerminal(t)
	dir := t.TempDir()
	script := "#!/bin/sh\necho ran-in-dir\n"
	if err := os.WriteFile(filepath.Join(dir, "tool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var output syncBuffer
	outcome, err := RunGroup(context.Background(), GroupCommand{
		Dir: dir, Argv: []string{"./tool"}, Output: &output, Terminal: terminal,
	})
	if err != nil || outcome.ExitCode != 0 || output.String() != "ran-in-dir\n" {
		t.Fatalf("outcome=%+v err=%v output=%q", outcome, err, output.String())
	}
	// The same name where no such file is: a start failure.
	_, err = RunGroup(context.Background(), GroupCommand{
		Dir: t.TempDir(), Argv: []string{"./tool"}, Output: &output, Terminal: terminal,
	})
	var exitErr *exec.ExitError
	if err == nil || errors.As(err, &exitErr) {
		t.Fatalf("err=%v, want a start failure", err)
	}
}

func TestCancellationStopsACommandWaitingOnItsTerminal(t *testing.T) {
	t.Parallel()
	terminal, written, readEnded := openTestTerminal(t)
	var output syncBuffer
	command := shellCommand(t, `printf 'password: ' > /dev/tty; IFS= read -r answer < /dev/tty; echo "answer=$answer"`, &output)
	command.Terminal = terminal
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan GroupOutcome, 1)
	go func() {
		outcome, _ := RunGroup(ctx, command)
		done <- outcome
	}()
	waitForOutput(t, written, "password: ")
	cancel()
	select {
	case outcome := <-done:
		if !outcome.Stopped || output.String() != "" {
			t.Fatalf("outcome=%+v output=%q", outcome, output.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the command waiting on its terminal was not stopped")
	}
	// Once the command is gone, closing the terminal ends its reader.
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readEnded:
	case <-time.After(10 * time.Second):
		t.Fatal("the terminal's reader did not end")
	}
	if _, err := terminal.Write([]byte("late\n")); err == nil {
		t.Fatal("a write to a closed terminal succeeded")
	}
}

// openDescriptors counts this process's open descriptors.
func openDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// Not parallel: other tests open and close descriptors of their own.
func TestClosingATerminalReleasesBothEnds(t *testing.T) {
	before := openDescriptors(t)
	for range 40 {
		terminal, err := OpenTerminal()
		if err != nil {
			t.Fatal(err)
		}
		if err := terminal.Close(); err != nil {
			t.Fatal(err)
		}
		if err := terminal.Close(); err != nil {
			t.Fatalf("a second close: %v", err)
		}
	}
	if after := openDescriptors(t); after > before {
		t.Fatalf("open descriptors grew from %d to %d", before, after)
	}
}
