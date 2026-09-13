package remotejobs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/store/storetest"
	"github.com/google/uuid"
)

// The test binary is the only executable these tests run. AO_TEST_REMOTE_HELPER
// selects a role; storetest.Run never sees a helper invocation.
func TestMain(m *testing.M) {
	if role := os.Getenv("AO_TEST_REMOTE_HELPER"); role != "" {
		os.Exit(remoteHelper(role))
	}
	os.Exit(storetest.Run(m))
}

func remoteHelper(role string) int {
	switch role {
	case "leave-child":
		// A command that detaches a worker and exits at once: the shape of
		// `sh -c 'server &'`. The worker keeps writing so the pipe stays open.
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "AO_TEST_REMOTE_HELPER=worker")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 3
		}
		// Give the worker a moment to write before the leader leaves, so the
		// test can prove the pipe stayed AO's after the leader exited.
		time.Sleep(300 * time.Millisecond)
		fmt.Println("leader-done")
		return 0
	case "worker":
		fmt.Println("worker-started")
		for range 600 {
			time.Sleep(50 * time.Millisecond)
			fmt.Println("worker-alive")
		}
		return 0
	case "graceful":
		// Exits cleanly on SIGTERM after writing a marker, proving the polite
		// signal arrived before any kill.
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGTERM)
		fmt.Println("graceful-started")
		<-stop
		fmt.Println("graceful-term")
		return 0
	case "stubborn":
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("stubborn-started")
		time.Sleep(time.Minute)
		return 0
	}
	fmt.Fprintln(os.Stderr, "unknown helper role")
	return 2
}

func helperArgv(t *testing.T, role string) ([]string, func() []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the destination runner never executes on Windows")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	env := func() []string {
		return []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "AO_TEST_REMOTE_HELPER=" + role}
	}
	return []string{binary}, env
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A command that exits while a child it started keeps running finishes with
// its own exit code, its child's output captured, the child stopped, and the
// receipt saying so.
func TestProcessRunnerStopsLeftoverProcessesAndKeepsExitCode(t *testing.T) {
	argv, env := helperArgv(t, "leave-child")
	var output bytes.Buffer
	started := time.Now()
	outcome, err := ProcessRunner(env)(context.Background(), t.TempDir(), argv, &output)
	if err != nil || outcome.ExitCode != 0 || !outcome.Leftovers {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if elapsed := time.Since(started); elapsed > terminateGrace+drainGrace+5*time.Second {
		t.Fatalf("leftover sweep took %s", elapsed)
	}
	text := output.String()
	if !strings.Contains(text, "leader-done") || !strings.Contains(text, "worker-started") {
		t.Fatalf("output lost the leader or its child: %q", text)
	}
}

func TestProcessRunnerTerminatesBeforeKilling(t *testing.T) {
	argv, env := helperArgv(t, "graceful")
	var output syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Outcome, 1)
	go func() {
		outcome, _ := ProcessRunner(env)(ctx, t.TempDir(), argv, &output)
		done <- outcome
	}()
	waitFor(t, "helper start", func() bool { return strings.Contains(output.String(), "graceful-started") })
	cancel()
	outcome := <-done
	if outcome.ExitCode != 0 || outcome.Leftovers || !strings.Contains(output.String(), "graceful-term") {
		t.Fatalf("SIGTERM did not reach the command first: outcome=%+v output=%q", outcome, output.String())
	}
}

func TestProcessRunnerKillsWhatIgnoresTerminate(t *testing.T) {
	argv, env := helperArgv(t, "stubborn")
	var output syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Outcome, 1)
	go func() {
		outcome, _ := ProcessRunner(env)(ctx, t.TempDir(), argv, &output)
		done <- outcome
	}()
	waitFor(t, "helper start", func() bool { return strings.Contains(output.String(), "stubborn-started") })
	canceled := time.Now()
	cancel()
	select {
	case outcome := <-done:
		if outcome.ExitCode == 0 || time.Since(canceled) < terminateGrace {
			t.Fatalf("stubborn command was not killed after the grace: %+v after %s", outcome, time.Since(canceled))
		}
	case <-time.After(terminateGrace + 10*time.Second):
		t.Fatal("stubborn command outlived the kill")
	}
}

// The manager reports the sweep on the receipt without changing the result,
// and the script cleanup still happens after the leftover stop.
func TestManagerRecordsLeftoverWarningOnSuccessfulReceipt(t *testing.T) {
	argv, env := helperArgv(t, "leave-child")
	m, _ := logManager(t, logOptions(t, 1<<20), ProcessRunner(env))
	r := request()
	r.Argv = argv
	r.TimeoutSeconds = 0
	if _, err := m.Start("owner", uuid.NewString(), t.TempDir(), r); err != nil {
		t.Fatal(err)
	}
	got := settled(t, m, r.ID)
	if got.State != "succeeded" || got.ExitCode != 0 || !strings.Contains(got.Warning, "background processes") || !strings.Contains(got.Output, "worker-started") {
		t.Fatalf("receipt: %+v", got)
	}
	entries, err := os.ReadDir(m.logs.options.LogDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "script-") {
			t.Fatalf("script retained: %s", filepath.Join(m.logs.options.LogDir, entry.Name()))
		}
	}
}

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
