//go:build unix

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/procutil"
	"agent-overflow/internal/providerupdate"
)

// shellUpdateCommand runs script with /bin/sh in a temporary directory, as
// an updater's command would run.
func shellUpdateCommand(t *testing.T, script string) providerupdate.Command {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "update.sh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	return providerupdate.Command{Binary: "/bin/sh", Args: []string{path}, Dir: dir}
}

// An updater hands the install to a package manager it starts (npm, brew):
// ending the run ends that child too, not just the updater.
func TestProviderUpdateCancellationEndsTheUpdatersChildren(t *testing.T) {
	t.Parallel()
	command := shellUpdateCommand(t, "sleep 300 &\necho $! > child.pid\necho installing\nwait\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		out providerupdate.Output
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := runProviderUpdateGroup(ctx, command)
		done <- result{out, err}
	}()
	pid := readChildPID(t, filepath.Join(command.Dir, "child.pid"))
	t.Cleanup(func() {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("kill leftover child %d: %v", pid, err)
		}
	})
	cancel()
	got := <-done
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", got.err)
	}
	if !strings.Contains(got.out.Text, "installing") {
		t.Errorf("output = %q", got.out.Text)
	}
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(pid, 0) == nil && !procutil.Exited(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("the updater's child %d outlived the cancelled run", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A non-zero exit is the command's answer, carried with its interleaved
// output; a command that cannot start is an error.
func TestProviderUpdateGroupRunnerReportsExitAndStartFailures(t *testing.T) {
	t.Parallel()
	out, err := runProviderUpdateGroup(context.Background(),
		shellUpdateCommand(t, "echo fetching\necho 'EACCES: permission denied' >&2\nexit 3\n"))
	if err != nil || out.ExitCode != 3 || !strings.Contains(out.Text, "fetching") || !strings.Contains(out.Text, "EACCES") {
		t.Fatalf("out = %+v, err = %v; want exit 3 with both streams", out, err)
	}
	out, err = runProviderUpdateGroup(context.Background(), shellUpdateCommand(t, "echo done\n"))
	if err != nil || out.ExitCode != 0 || strings.TrimSpace(out.Text) != "done" {
		t.Fatalf("clean run = %+v, %v", out, err)
	}
	missing := providerupdate.Command{Binary: filepath.Join(t.TempDir(), "absent"), Args: []string{"update"}, Dir: t.TempDir()}
	if _, err := runProviderUpdateGroup(context.Background(), missing); err == nil {
		t.Fatal("a command that cannot start was not an error")
	}
}

func readChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil && strings.HasSuffix(string(raw), "\n") {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if convErr != nil {
				t.Fatal(convErr)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("the script did not report its child: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
