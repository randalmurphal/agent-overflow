//go:build !windows

package orphanreaper

import (
	"bufio"
	"bytes"
	"errors"
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
	calls, stderr, err := recordScript(t, sidecarScript, input)
	if err != nil {
		t.Fatalf("sidecar exit: %v (stderr %q)", err, stderr)
	}
	return calls, stderr
}

// recordScript is recordSidecar for any script, and returns how the shell
// exited.
func recordScript(t *testing.T, script, input string) (calls []string, stderr string, exit error) {
	t.Helper()
	control, feed, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	recorded, record, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// The control lines wait in the pipe, ended, before the shell starts: a
	// shell that exits without reading them cannot fail the write.
	if _, err := io.WriteString(feed, input); err != nil {
		t.Fatal(err)
	}
	_ = feed.Close()
	cmd := sidecarCommand(control, 7*time.Second)
	cmd.ExtraFiles = append(cmd.ExtraFiles, record) // fd 4
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := startSidecar(cmd, `kill() { echo "kill $*" >&4; }; sleep() { echo "sleep $*" >&4; }`+"\n"+script); err != nil {
		t.Fatal(err)
	}
	_ = control.Close()
	_ = record.Close()
	out, err := io.ReadAll(recorded)
	if err != nil {
		t.Fatal(err)
	}
	_ = recorded.Close()
	exit = cmd.Wait()
	for line := range strings.Lines(string(out)) {
		calls = append(calls, strings.TrimSuffix(line, "\n"))
	}
	return calls, errOut.String(), exit
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

// A shell that cannot fork sleep exits at that line, which the recorded sleep
// stands in for by exiting as bash does. The SIGKILL must still be sent.
func TestSidecarKillsWhenTheShellExitsAtTheGrace(t *testing.T) {
	control, feed, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := sidecarCommand(control, 7*time.Second)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := startSidecar(cmd, `kill() { echo "kill $*"; }; sleep() { exit 254; }`+"\n"+sidecarScript); err != nil {
		t.Fatal(err)
	}
	_ = control.Close()
	if _, err := io.WriteString(feed, formatWatch(200)); err != nil {
		t.Fatal(err)
	}
	_ = feed.Close()
	if exit, ok := errors.AsType[*exec.ExitError](cmd.Wait()); !ok || exit.ExitCode() != 254 {
		t.Fatalf("sidecar exit = %v, want the shell's own failure (stderr %q)", exit, errOut.String())
	}
	if got, want := strings.Fields(out.String()), strings.Fields("kill -TERM -200 kill -KILL -200"); !slices.Equal(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
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
	if err := startSidecar(sidecar, sidecarScript); err != nil {
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

// A function the user's environment exports must not stand in for the
// script's kill: bash, which is /bin/sh on macOS and several Linux
// distributions, imports exported functions at startup.
func TestSidecarIgnoresFunctionsExportedInTheEnvironment(t *testing.T) {
	t.Setenv("BASH_FUNC_kill%%", "() { :; }")
	victim := startGroup(t, "echo ready; exec sleep 120")
	control, feed, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	sidecar := sidecarCommand(control, time.Second)
	if err := startSidecar(sidecar, sidecarScript); err != nil {
		t.Fatalf("start sidecar: %v", err)
	}
	_ = control.Close()
	if _, err := io.WriteString(feed, formatWatch(victim.Process.Pid)); err != nil {
		t.Fatalf("write watch: %v", err)
	}
	_ = feed.Close()
	if err := sidecar.Wait(); err != nil {
		t.Fatalf("sidecar exit: %v", err)
	}
	reaped := make(chan struct{})
	go func() {
		_ = victim.Wait()
		close(reaped)
	}()
	select {
	case <-reaped:
	case <-time.After(5 * time.Second):
		t.Fatal("an exported kill function replaced the sidecar's kill")
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
