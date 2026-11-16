//go:build !windows

package orphanreaper

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// recordSidecar runs sidecarScript on input with kill and sleep replaced by
// shell functions that record their arguments, so pgids the script must
// refuse (1 would signal every process the user owns) are never signalled.
// It returns the recorded calls in order and the script's stderr.
func recordSidecar(t *testing.T, input string) (calls []string, stderr string) {
	t.Helper()
	control, feed, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	recorded, record, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := sidecarCommand(control, 7*time.Second)
	cmd.Stdin = strings.NewReader(`kill() { echo "kill $*" >&4; }; sleep() { echo "sleep $*" >&4; }` + "\n" + sidecarScript)
	cmd.ExtraFiles = append(cmd.ExtraFiles, record) // fd 4
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = control.Close()
	_ = record.Close()
	if _, err := io.WriteString(feed, input); err != nil {
		t.Fatal(err)
	}
	_ = feed.Close()
	out, err := io.ReadAll(recorded)
	if err != nil {
		t.Fatal(err)
	}
	_ = recorded.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("sidecar exit: %v (stderr %q)", err, errOut.String())
	}
	for line := range strings.Lines(string(out)) {
		calls = append(calls, strings.TrimSuffix(line, "\n"))
	}
	return calls, errOut.String()
}

func TestSidecarReapsOnlyUnreleasedOnEOF(t *testing.T) {
	calls, _ := recordSidecar(t, formatWatch(100)+formatWatch(200)+formatRelease(100))
	want := []string{"kill -TERM -200", "sleep 7", "kill -KILL -200"}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}

func TestSidecarNoWatchedNoKills(t *testing.T) {
	// A group watched twice is one group: one release ends its watch.
	calls, _ := recordSidecar(t, "watch 100\nwatch 100\nrelease 100\nrelease 300\n")
	if len(calls) != 0 {
		t.Errorf("calls = %q, want none when everything was released", calls)
	}
}

// Garbage and unsafe pgids must be logged and skipped, not end the loop or
// enter the watched set. Padding is tolerated, as the client never sends it.
func TestSidecarSkipsMalformedLines(t *testing.T) {
	rejected := []string{
		"garbage",
		"watch",
		"watch 1 2",
		"watch abc",
		"watch 0",
		"watch 1",
		"watch -5",
		"watch +5",
		"watch 012",
		"watch 12345678901",
		"kill 100",
		"release 1",
	}
	input := strings.Join(rejected, "\n") + "\n\n  watch \t300  \n"
	calls, stderr := recordSidecar(t, input)
	want := []string{"kill -TERM -300", "sleep 7", "kill -KILL -300"}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	if got := strings.Count(stderr, "orphanreaper: malformed") + strings.Count(stderr, "orphanreaper: refusing"); got != len(rejected) {
		t.Errorf("logged %d rejections, want %d:\n%s", got, len(rejected), stderr)
	}
}

// The end-to-end check: the sidecar Spawn runs, real process groups, and a
// real pipe close standing in for parent death. A group that ignores SIGTERM,
// with a child of its own, ends at the SIGKILL.
func TestSidecarKillsRealGroupsOnParentEOF(t *testing.T) {
	plain := startGroup(t, "echo ready; exec sleep 120")
	stubborn := startGroup(t, `trap '' TERM; sleep 120 & echo ready; wait`)

	control, feed, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	sidecar := sidecarCommand(control, time.Second)
	if err := sidecar.Start(); err != nil {
		t.Fatalf("start sidecar: %v", err)
	}
	_ = control.Close()
	exited := make(chan error, 1)
	go func() { exited <- sidecar.Wait() }()

	for _, victim := range []*exec.Cmd{plain, stubborn} {
		if _, err := io.WriteString(feed, formatWatch(victim.Process.Pid)); err != nil {
			t.Fatalf("write watch: %v", err)
		}
	}
	_ = feed.Close()

	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("sidecar exit: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("sidecar did not exit after its parent's EOF")
	}
	for _, victim := range []*exec.Cmd{plain, stubborn} {
		pgid := victim.Process.Pid
		reaped := make(chan struct{})
		go func() {
			_ = victim.Wait()
			close(reaped)
		}()
		select {
		case <-reaped:
		case <-time.After(5 * time.Second):
			t.Fatalf("leader of group %d survived the sidecar", pgid)
		}
		// The group outlives its leader while any member lives; signal 0
		// probes it without touching it.
		deadline := time.Now().Add(5 * time.Second)
		for syscall.Kill(-pgid, 0) == nil {
			if time.Now().After(deadline) {
				t.Fatalf("group %d survived the sidecar", pgid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// startGroup starts script under sh as the leader of its own process group,
// as the app starts the groups it watches, and returns once the script has
// printed its first line.
func startGroup(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatalf("group %d never became ready: %v", pgid, err)
	}
	return cmd
}
