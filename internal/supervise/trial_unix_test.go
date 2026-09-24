//go:build !windows

package supervise

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/startupprogress"

	"golang.org/x/sys/unix"
)

// The trial's children are scripted like the supervisor's: shell scripts on
// the real inherited descriptors, with PATH and a temp HOME only.

const fakeTrialScript = `#!/bin/sh
OBS='__OBS__'
DB='__DB__'
note() { printf '%s\n' "$*" >> "$OBS/log"; }
# Before anything is reported, as the real trial handles signals before it
# can say prepared: a stop that lands between prepared and the loop below
# must still be observed.
trap 'note stopped; exit 0' TERM INT
serve_until_stopped() {
	while :; do
		# The sleep must not inherit descriptor 5: the real trial makes it
		# close-on-exec, and a sleeping child would hold the lock.
		sleep 5 5>&- &
		wait $! 2>/dev/null
	done
}
progress() {
	printf '{"type":"progress","progress":{"phase":"%s","detail":"%s","updatedAt":1}%s}\n' "$1" "$2" "$3" >&4
}
IFS= read -r ACTIVATE <&3
printf '%s\n' "$ACTIVATE" >> "$OBS/activate"
printf '%s\n' "${AO_BACKEND_LOCK_FD:-none}" >> "$OBS/lockenv"
if { true >&5; } 2>/dev/null; then note fd5-open; fi
__HELLO__
__BEHAVIOR__
`

const (
	helloProgress = `printf '{"type":"hello","protocolVersion":1,"version":"2.0.0","reportsProgress":true}\n' >&4`
	helloLegacy   = `printf '{"type":"hello","protocolVersion":1,"version":"2.0.0"}\n' >&4`
)

type trialRig struct {
	t      *testing.T
	dir    string
	obs    string
	home   string
	db     string
	logs   []string
	logsMu sync.Mutex
}

func newTrialRig(t *testing.T) *trialRig {
	t.Helper()
	root := t.TempDir()
	r := &trialRig{t: t, dir: root, obs: filepath.Join(root, "obs"), home: filepath.Join(root, "home"), db: filepath.Join(root, "data", "agent-overflow.db")}
	for _, dir := range []string{r.obs, r.home, filepath.Dir(r.db)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("child log:\n%s", r.read("log"))
			r.logsMu.Lock()
			t.Logf("trial log:\n  %s", strings.Join(r.logs, "\n  "))
			r.logsMu.Unlock()
		}
	})
	return r
}

func (r *trialRig) script(hello, behavior string) string {
	r.t.Helper()
	body := strings.NewReplacer("__OBS__", r.obs, "__DB__", r.db, "__HELLO__", hello, "__BEHAVIOR__", behavior).Replace(fakeTrialScript)
	path := filepath.Join(r.dir, "trial.sh")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		r.t.Fatal(err)
	}
	return path
}

func (r *trialRig) read(name string) string {
	data, _ := os.ReadFile(filepath.Join(r.obs, name))
	return string(data)
}

func (r *trialRig) config(binary string) TrialConfig {
	return TrialConfig{
		Binary: binary,
		Env:    []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.home},
		// The rule is shrunk so a stall is observed in a test's time; the
		// behaviors below are scaled to it.
		Rule:          StallRule{Window: 400 * time.Millisecond, Ceiling: 10 * time.Second},
		LegacyBudget:  400 * time.Millisecond,
		StopTimeout:   5 * time.Second,
		UpdateID:      "u1",
		TargetVersion: "2.0.0",
		Log: func(format string, args ...any) {
			r.logsMu.Lock()
			defer r.logsMu.Unlock()
			r.logs = append(r.logs, strings.TrimSpace(format))
			_ = args
		},
	}
}

func trialFailure(t *testing.T, err error) string {
	t.Helper()
	var failed *TrialFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("RunTrial = %v, want a TrialFailedError", err)
	}
	return failed.Reason
}

func TestTrialReportsPreparedAndIsStopped(t *testing.T) {
	r := newTrialRig(t)
	cfg := r.config(r.script(helloProgress, `progress store.migrate "Applying migration 1 of 1"
printf '{"type":"prepared","updateId":"u1"}\n' >&4
serve_until_stopped`))
	var got []startupprogress.Progress
	cfg.OnProgress = func(p startupprogress.Progress, liveness bool) { got = append(got, p) }
	stopping := 0
	cfg.OnStopping = func() { stopping++ }
	if err := RunTrial(context.Background(), cfg); err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if !strings.Contains(r.read("log"), "stopped") {
		t.Fatal("the prepared trial was not stopped")
	}
	if stopping != 1 {
		t.Fatalf("OnStopping ran %d times", stopping)
	}
	if len(got) != 1 || got[0].Detail != "Applying migration 1 of 1" {
		t.Fatalf("progress = %+v", got)
	}
	activate := r.read("activate")
	for _, want := range []string{`"type":"activate"`, `"trial":true`, `"updateId":"u1"`, `"targetVersion":"2.0.0"`, `"ownsDataRoot":true`} {
		if !strings.Contains(activate, want) {
			t.Fatalf("activate %q lacks %s", activate, want)
		}
	}
}

func TestProgressKeepsATrialAlivePastTheWindow(t *testing.T) {
	r := newTrialRig(t)
	// Ten real steps 100 ms apart outlast the 400 ms window twice over.
	cfg := r.config(r.script(helloProgress, `i=0
while [ $i -lt 10 ]; do
	i=$((i+1))
	progress store.migrate "Applying migration $i of 10"
	sleep 0.1
done
printf '{"type":"prepared"}\n' >&4
serve_until_stopped`))
	if err := RunTrial(context.Background(), cfg); err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
}

func TestHeartbeatsAloneDoNotKeepATrialAlive(t *testing.T) {
	r := newTrialRig(t)
	cfg := r.config(r.script(helloProgress, `progress store.migrate "Applying migration 3 of 7 add_index"
while :; do
	progress store.migrate "Applying migration 3 of 7 add_index" ',"liveness":true'
	sleep 0.1
done`))
	var heartbeats int
	cfg.OnProgress = func(p startupprogress.Progress, liveness bool) {
		if liveness {
			heartbeats++
		}
	}
	started := time.Now()
	reason := trialFailure(t, RunTrial(context.Background(), cfg))
	if !strings.Contains(reason, "stopped making progress for 400ms") || !strings.Contains(reason, "Applying migration 3 of 7 add_index") {
		t.Fatalf("reason = %q, want the stall naming the last step", reason)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("the stall took %s to be noticed", elapsed)
	}
	if heartbeats == 0 {
		t.Fatal("no heartbeat reached OnProgress, so the test proved nothing")
	}
}

func TestASilentTrialStallsAtTheWindow(t *testing.T) {
	r := newTrialRig(t)
	cfg := r.config(r.script(helloProgress, `serve_until_stopped`))
	reason := trialFailure(t, RunTrial(context.Background(), cfg))
	if !strings.Contains(reason, "reported no progress within 400ms") {
		t.Fatalf("reason = %q", reason)
	}
	if !strings.Contains(r.read("log"), "stopped") {
		t.Fatal("the stalled trial was not stopped")
	}
}

func TestTheCeilingEndsATrialThatReportsForever(t *testing.T) {
	r := newTrialRig(t)
	cfg := r.config(r.script(helloProgress, `i=0
while :; do
	i=$((i+1))
	progress store.migrate "step $i"
	sleep 0.05
done`))
	cfg.Rule = StallRule{Window: 400 * time.Millisecond, Ceiling: 900 * time.Millisecond}
	reason := trialFailure(t, RunTrial(context.Background(), cfg))
	if !strings.Contains(reason, "did not finish starting within 900ms (last step: step") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestALegacyTrialGetsTheFixedBudget(t *testing.T) {
	r := newTrialRig(t)
	// Progress frames from a child that did not claim the capability do not
	// extend its budget.
	cfg := r.config(r.script(helloLegacy, `while :; do
	progress store.migrate "step"
	sleep 0.05
done`))
	cfg.LegacyBudget = 500 * time.Millisecond
	reason := trialFailure(t, RunTrial(context.Background(), cfg))
	if !strings.Contains(reason, "did not report prepared within 500ms") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestAFailedFrameIsTheRecordedReason(t *testing.T) {
	r := newTrialRig(t)
	cfg := r.config(r.script(helloProgress, `printf '{"type":"failed","reason":"database schema 90 is newer than this build knows (88)"}\n' >&4
exit 1`))
	reason := trialFailure(t, RunTrial(context.Background(), cfg))
	if reason != "database schema 90 is newer than this build knows (88)" {
		t.Fatalf("reason = %q", reason)
	}
}

func TestATrialThatExitsEarlyNamesTheExitStatus(t *testing.T) {
	r := newTrialRig(t)
	cfg := r.config(r.script(helloProgress, `exit 3`))
	reason := trialFailure(t, RunTrial(context.Background(), cfg))
	if !strings.Contains(reason, "exited before it finished starting: exit status 3") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestATrialThatCannotStartFails(t *testing.T) {
	r := newTrialRig(t)
	reason := trialFailure(t, RunTrial(context.Background(), r.config(filepath.Join(r.dir, "missing"))))
	if !strings.Contains(reason, "did not start") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestCancellingATrialStopsItWithoutAVerdict(t *testing.T) {
	r := newTrialRig(t)
	cfg := r.config(r.script(helloProgress, `note ready
serve_until_stopped`))
	cfg.Rule = StallRule{Window: time.Minute, Ceiling: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunTrial(ctx, cfg) }()
	waitForFile(t, filepath.Join(r.obs, "log"), "ready")
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunTrial = %v, want context.Canceled", err)
	}
	if !strings.Contains(r.read("log"), "stopped") {
		t.Fatal("the cancelled trial was not stopped")
	}
}

// The trial inherits the data root's lock, so the root stays locked until
// the trial exits even after the parent lets go of its own descriptor.
func TestTheTrialHoldsTheInheritedLockUntilItExits(t *testing.T) {
	r := newTrialRig(t)
	lockPath := filepath.Join(r.dir, "backend.lock")
	lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	cfg := r.config(r.script(helloProgress, `note ready
serve_until_stopped`))
	cfg.Rule = StallRule{Window: time.Minute, Ceiling: time.Minute}
	cfg.Lock = lock
	started := make(chan struct{}, 1)
	logf := cfg.Log
	cfg.Log = func(format string, args ...any) {
		logf(format, args...)
		if strings.HasPrefix(format, "supervise: started version") {
			select {
			case started <- struct{}{}:
			default:
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunTrial(ctx, cfg) }()
	waitForFile(t, filepath.Join(r.obs, "log"), "ready")
	// Start reads the descriptor; closing it is ordered after that read only
	// through this receive, not through the child's file.
	<-started
	if got := strings.TrimSpace(r.read("lockenv")); got != "5" {
		t.Fatalf("the trial was told its lock is on %q", got)
	}
	if !strings.Contains(r.read("log"), "fd5-open") {
		t.Fatal("descriptor 5 was not open in the trial")
	}
	// The parent lets go; the trial still holds the open file description.
	lock.Close()
	if lockable(t, lockPath) {
		t.Fatal("the data root was unlocked while the trial ran")
	}
	cancel()
	<-done
	if !lockable(t, lockPath) {
		t.Fatal("the data root stayed locked after the trial exited")
	}
}

func lockable(t *testing.T, path string) bool {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false
	}
	t.Fatalf("flock: %v", err)
	return false
}

func waitForFile(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never contained %q", path, want)
}

func TestAdoptInheritedLockMakesTheDescriptorCloseOnExec(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "lock")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fd, err := syscall.Dup(int(file.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	// Dup clears close-on-exec, as exec does for an inherited descriptor.
	env := map[string]string{EnvInheritedLock: itoa(fd)}
	adopted, err := AdoptInheritedLock(func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		func(k string) error { delete(env, k); return nil })
	if err != nil || adopted == nil {
		t.Fatalf("AdoptInheritedLock = %v, %v", adopted, err)
	}
	defer adopted.Close()
	if _, still := env[EnvInheritedLock]; still {
		t.Fatal("the marker was left for children to inherit")
	}
	if !closeOnExec(t, fd) {
		t.Fatal("the adopted lock would leak into every process the trial starts")
	}

	none, err := AdoptInheritedLock(func(string) (string, bool) { return "", false }, nil)
	if none != nil || err != nil {
		t.Fatalf("no marker = %v, %v", none, err)
	}
	if _, err := AdoptInheritedLock(func(string) (string, bool) { return "987", true }, nil); err == nil {
		t.Fatal("a descriptor that is not open was adopted")
	}
}

// A refused descriptor is never wrapped, so it stays with whoever holds it.
// A dropped wrapper would close it when collected, under whatever file had
// reused its number by then.
func TestRefusedInheritedDescriptorsStayWithTheirOwner(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "not-a-pipe")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	pipeFD, fileFD := itoa(int(read.Fd())), itoa(int(file.Fd()))

	// The read end passes and the write end does not.
	if _, err := OpenChildChannel(func(string) (string, bool) { return pipeFD + "," + fileFD, true }, nil); err == nil {
		t.Fatal("a regular file was accepted as the channel's write end")
	}
	if _, err := OpenChildChannel(func(string) (string, bool) { return pipeFD + "," + pipeFD, true }, nil); err == nil {
		t.Fatal("one descriptor was accepted as both ends")
	}
	if _, err := AdoptInheritedLock(func(string) (string, bool) { return pipeFD, true }, nil); err == nil {
		t.Fatal("a pipe was adopted as the lock")
	}
	for range 3 {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := file.Write([]byte("kept")); err != nil {
		t.Fatalf("the refused file was closed: %v", err)
	}
	if _, err := write.Write([]byte("x")); err != nil {
		t.Fatalf("the pipe lost its read end: %v", err)
	}
	if _, err := read.Read(make([]byte, 1)); err != nil {
		t.Fatalf("the refused read end was closed: %v", err)
	}
}

func dupFD(t *testing.T, file *os.File) int {
	t.Helper()
	fd, err := syscall.Dup(int(file.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

func TestOpenChildChannelMakesTheChannelCloseOnExec(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	readFD, err := syscall.Dup(int(read.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	writeFD, err := syscall.Dup(int(write.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := OpenChildChannel(func(string) (string, bool) { return itoa(readFD) + "," + itoa(writeFD), true }, nil)
	if err != nil {
		t.Fatalf("OpenChildChannel: %v", err)
	}
	defer conn.Close()
	if !closeOnExec(t, readFD) || !closeOnExec(t, writeFD) {
		t.Fatal("the channel would leak into every process the backend starts")
	}
}

func closeOnExec(t *testing.T, fd int) bool {
	t.Helper()
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("fcntl: %v", err)
	}
	return flags&unix.FD_CLOEXEC != 0
}

func itoa(n int) string { return strconv.Itoa(n) }
