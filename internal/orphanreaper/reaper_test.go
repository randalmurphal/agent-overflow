package orphanreaper

import (
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the test binary re-exec itself as the reaper sidecar so
// the EOF-on-parent-death path can be exercised as a real process.
func TestMain(m *testing.M) {
	if os.Getenv("ORPHANREAPER_TEST_CHILD") == "1" {
		RunChild()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// recordingKiller captures (pgid, signal) pairs in call order.
type recordingKiller struct {
	mu    sync.Mutex
	calls []struct {
		pgid int
		sig  syscall.Signal
	}
}

func (k *recordingKiller) kill(pgid int, sig syscall.Signal) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, struct {
		pgid int
		sig  syscall.Signal
	}{pgid, sig})
}

func (k *recordingKiller) signalsFor(pgid int) []syscall.Signal {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []syscall.Signal
	for _, c := range k.calls {
		if c.pgid == pgid {
			out = append(out, c.sig)
		}
	}
	return out
}

func TestRunReapsOnlyUnreleasedOnEOF(t *testing.T) {
	// 100 and 200 watched, 100 then released → only 200 should be reaped.
	input := strings.NewReader("watch 100\nwatch 200\nrelease 100\n")
	k := &recordingKiller{}

	run(input, k.kill, 0)

	if got := k.signalsFor(200); len(got) != 2 || got[0] != syscall.SIGTERM || got[1] != syscall.SIGKILL {
		t.Errorf("pgid 200 signals = %v, want [SIGTERM SIGKILL]", got)
	}
	if got := k.signalsFor(100); len(got) != 0 {
		t.Errorf("pgid 100 was released but got signals %v", got)
	}
}

func TestRunSkipsMalformedLines(t *testing.T) {
	// Garbage and unsafe pgids must be ignored, not abort the loop or
	// pollute the watched set. The valid watch 300 must still be reaped.
	input := strings.NewReader("garbage\nwatch 0\nwatch 300\nnonsense 1 2 3\n")
	k := &recordingKiller{}

	run(input, k.kill, 0)

	if got := k.signalsFor(300); len(got) != 2 {
		t.Errorf("pgid 300 signals = %v, want SIGTERM+SIGKILL", got)
	}
	if got := k.signalsFor(0); len(got) != 0 {
		t.Errorf("unsafe pgid 0 should never be signalled, got %v", got)
	}
}

func TestRunNoWatchedNoKills(t *testing.T) {
	k := &recordingKiller{}
	run(strings.NewReader("watch 100\nrelease 100\n"), k.kill, 0)
	if len(k.calls) != 0 {
		t.Errorf("expected no kills when everything released, got %v", k.calls)
	}
}

// TestRunChildKillsRealGroupOnParentEOF is the end-to-end check: a real
// sidecar process, a real victim process group, and a real pipe close
// standing in for parent death. The victim must be killed.
func TestRunChildKillsRealGroupOnParentEOF(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX process groups on Windows")
	}

	// Victim: a long sleep in its own process group (mirrors how the app
	// spawns providers with Setpgid).
	victim := exec.Command("sleep", "120")
	victim.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := victim.Start(); err != nil {
		t.Fatalf("start victim: %v", err)
	}
	pgid := victim.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })

	// Sidecar: re-exec this test binary into RunChild with the control
	// pipe's read end on fd 3.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), "ORPHANREAPER_TEST_CHILD=1")
	child.ExtraFiles = []*os.File{r}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatalf("start sidecar: %v", err)
	}
	r.Close()
	t.Cleanup(func() { _ = child.Wait() })

	// Watch the victim, then close the pipe — the kernel delivers EOF to
	// the sidecar, which must tear the group down.
	if _, err := io.WriteString(w, formatWatch(pgid)); err != nil {
		t.Fatalf("write watch: %v", err)
	}
	w.Close()

	done := make(chan struct{})
	go func() {
		_, _ = victim.Process.Wait()
		close(done)
	}()

	select {
	case <-done:
		// victim reaped — success
	case <-time.After(15 * time.Second):
		t.Fatal("victim survived; sidecar did not kill the group on parent EOF")
	}
}
