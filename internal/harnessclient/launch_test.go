package harnessclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
)

// The launch tests drive the REAL spawn path against this test binary
// re-executed as a stand-in backend. Re-exec rather than a shell script
// so the test is portable, and rather than the real binary because none
// of what Launch does — argv assembly, the bootstrap wait, the stderr
// tail, detaching — depends on which program prints the line.
const (
	fakeBackendEnv      = "HARNESSCLIENT_FAKE_BACKEND"
	fakeBackendArgvEnv  = "HARNESSCLIENT_FAKE_ARGV_FILE"
	fakeBackendChildPID = "HARNESSCLIENT_FAKE_CHILD_PID_FILE"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeBackendEnv); mode != "" {
		fakeBackendMain(mode)
		return
	}
	os.Exit(m.Run())
}

// fakeBackendMain impersonates `agent-overflow --harness`. mode selects
// which boot outcome it acts out.
func fakeBackendMain(mode string) {
	if path := os.Getenv(fakeBackendArgvEnv); path != "" {
		record, _ := json.Marshal(os.Args[1:])
		_ = os.WriteFile(path, record, 0o600)
	}
	fmt.Fprintln(os.Stderr, "fake backend: starting")
	if mode == "child-only" {
		for {
			time.Sleep(time.Minute)
		}
	}
	if mode == "linger-child" {
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), fakeBackendEnv+"=child-only")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "fake backend: child start: %v\n", err)
			os.Exit(4)
		}
		if path := os.Getenv(fakeBackendChildPID); path != "" {
			_ = os.WriteFile(path, []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		}
		defer child.Wait()
	}

	switch mode {
	case "die":
		fmt.Fprintln(os.Stderr, "fake backend: boot refused, --data-dir is the OS config root")
		os.Exit(3)
	case "silent":
		// Exits without ever printing the line: the "hung boot" shape,
		// minus the wait.
		os.Exit(0)
	}

	payload := map[string]any{
		"url": "http://127.0.0.1:59123/?token=fake", "port": 59123, "token": "fake",
		"dataRoot": dataDirFromArgs(), "dataDir": filepath.Join(dataDirFromArgs(), "agent-overflow"),
		"mockProvider": "/bin/mock", "pid": os.Getpid(), "version": "test",
	}
	if mode == "wrong-pid" {
		payload["pid"] = os.Getpid() + 1
	}
	if mode == "startup-error" {
		payload["startupError"] = "open database: disk is full"
	}
	line, _ := json.Marshal(payload)
	fmt.Printf("\n%s %s\n", BootstrapPrefix, line)

	if mode == "linger" {
		// Live until the test terminates us, which is what makes the
		// detached path assertable.
		time.Sleep(2 * time.Minute)
	}
	if mode == "wrong-pid" {
		time.Sleep(2 * time.Minute)
	}
}

func dataDirFromArgs() string {
	for i, arg := range os.Args {
		if arg == "--data-dir" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

func fakeBackendOpts(t *testing.T, mode string, dataRoot string) LaunchOptions {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	return LaunchOptions{
		Binary:   self,
		DataRoot: dataRoot,
		Env:      []string{fakeBackendEnv + "=" + mode},
		Timeout:  20 * time.Second,
	}
}

func TestLaunchReadsTheBootstrapLineAndSpellsTheModeFlags(t *testing.T) {
	dataRoot := t.TempDir()
	argvFile := filepath.Join(t.TempDir(), "argv.json")
	opts := fakeBackendOpts(t, "linger", dataRoot)
	opts.Env = append(opts.Env, fakeBackendArgvEnv+"="+argvFile)
	opts.MockProvider = "/opt/bin/ao-mockprovider"

	launched, err := Launch(context.Background(), opts)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = launched.Terminate(context.Background()) })

	if launched.Bootstrap.Port != 59123 || launched.Bootstrap.Token != "fake" {
		t.Fatalf("bootstrap = %+v", launched.Bootstrap)
	}
	if launched.PID <= 0 {
		t.Fatal("Launch reported no pid")
	}

	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	var argv []string
	if err := json.Unmarshal(raw, &argv); err != nil {
		t.Fatalf("decode argv: %v", err)
	}
	got := strings.Join(argv, " ")
	for _, want := range []string{"--harness", "--data-dir " + instanceinfo.NormalizeSystemPath(dataRoot), "--mock-provider /opt/bin/ao-mockprovider"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "--window") {
		t.Errorf("argv %q spelled --window without being asked", got)
	}
}

func TestLaunchSpellsSoakAndWindow(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv.json")
	opts := fakeBackendOpts(t, "linger", t.TempDir())
	opts.Env = append(opts.Env, fakeBackendArgvEnv+"="+argvFile)
	opts.Soak = true
	opts.Window = true

	launched, err := Launch(context.Background(), opts)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = launched.Terminate(context.Background()) })

	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	if !strings.Contains(string(raw), "--soak") || !strings.Contains(string(raw), "--window") {
		t.Fatalf("argv = %s", raw)
	}
	if strings.Contains(string(raw), "--harness") {
		t.Fatalf("a soak launch also spelled --harness: %s", raw)
	}
}

func TestLaunchFailureQuotesTheBackendsStderr(t *testing.T) {
	_, err := Launch(context.Background(), fakeBackendOpts(t, "die", t.TempDir()))
	if err == nil {
		t.Fatal("Launch succeeded against a backend that exited")
	}
	// The interesting half of a failed boot is always in the child's
	// stderr; an error that omitted it would send the reader hunting.
	if !strings.Contains(err.Error(), "--data-dir is the OS config root") {
		t.Fatalf("error does not carry the backend's stderr: %v", err)
	}
}

func TestLaunchReportsAClosedStdoutWithoutABootstrapLine(t *testing.T) {
	_, err := Launch(context.Background(), fakeBackendOpts(t, "silent", t.TempDir()))
	if err == nil {
		t.Fatal("Launch succeeded against a backend that printed nothing")
	}
	if !strings.Contains(err.Error(), "bootstrap line") {
		t.Fatalf("error does not name the missing bootstrap line: %v", err)
	}
}

func TestLaunchRefusesBootstrapFromAnotherProcess(t *testing.T) {
	launched, err := Launch(context.Background(), fakeBackendOpts(t, "wrong-pid", t.TempDir()))
	if err == nil {
		t.Fatal("Launch accepted a bootstrap whose pid did not match the spawned process")
	}
	if !strings.Contains(err.Error(), "does not match spawned pid") {
		t.Fatalf("error does not name the bootstrap identity mismatch: %v", err)
	}
	if launched == nil {
		t.Fatal("Launch discarded the cleanup handle after a bootstrap identity mismatch")
	}
	if err := launched.Kill(context.Background()); err != nil {
		t.Fatalf("cleanup handle could not terminate failed launch: %v", err)
	}
}

func TestLaunchReturnsCleanupHandleWhenIdentityCaptureFails(t *testing.T) {
	previous := captureProcessIdentity
	captureProcessIdentity = func(int) (instanceinfo.ProcessIdentity, error) {
		return instanceinfo.ProcessIdentity{}, errors.New("identity probe unavailable")
	}
	t.Cleanup(func() { captureProcessIdentity = previous })

	launched, err := Launch(context.Background(), fakeBackendOpts(t, "linger", t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "identity probe unavailable") {
		t.Fatalf("Launch error = %v, want identity capture failure", err)
	}
	if launched == nil {
		t.Fatal("Launch discarded the cleanup handle after identity capture failure")
	}
	if err := launched.Kill(context.Background()); err != nil {
		t.Fatalf("cleanup handle could not be retried: %v", err)
	}
}

func TestLaunchRefusesAStartedBackendWhoseAppFailed(t *testing.T) {
	// The transport serves so logs stay readable, but the instance is not
	// usable; handing it back as a success would produce RPC failures
	// nobody can explain.
	_, err := Launch(context.Background(), fakeBackendOpts(t, "startup-error", t.TempDir()))
	if err == nil {
		t.Fatal("Launch accepted a backend that reported a startup error")
	}
	if !strings.Contains(err.Error(), "disk is full") {
		t.Fatalf("error drops the reported cause: %v", err)
	}
}

func TestDetachedLaunchWritesItsLogsAndOutlivesTheLauncher(t *testing.T) {
	dataRoot := t.TempDir()
	logDir := filepath.Join(dataRoot, "agent-overflow", "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	opts := fakeBackendOpts(t, "linger", dataRoot)
	opts.Detach = true
	opts.StdoutPath = filepath.Join(logDir, "backend-stdout.log")
	opts.StderrPath = filepath.Join(logDir, "backend-stderr.log")

	launched, err := Launch(context.Background(), opts)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = TerminateProcess(context.Background(), launched.PID, 2*time.Second) })

	if !instanceinfo.ProcessAlive(launched.PID) {
		t.Fatal("the detached backend is not running")
	}
	stderr, err := TailFile(opts.StderrPath, 10)
	if err != nil {
		t.Fatalf("tail stderr capture: %v", err)
	}
	if len(stderr) == 0 || !strings.Contains(strings.Join(stderr, "\n"), "starting") {
		t.Fatalf("stderr capture = %v, want the backend's own lines", stderr)
	}

	// TerminateProcess is what `ao-harness down` runs; it must actually
	// end the process rather than report success on a signal nobody read.
	if err := TerminateProcess(context.Background(), launched.PID, 5*time.Second); err != nil {
		t.Fatalf("TerminateProcess: %v", err)
	}
	if instanceinfo.ProcessAlive(launched.PID) {
		t.Fatal("the backend survived TerminateProcess")
	}
}

func TestTerminateKillsOwnedProcessGroupDescendants(t *testing.T) {
	childPIDFile := filepath.Join(t.TempDir(), "child.pid")
	opts := fakeBackendOpts(t, "linger-child", t.TempDir())
	opts.Env = append(opts.Env, fakeBackendChildPID+"="+childPIDFile)
	launched, err := Launch(context.Background(), opts)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	childPIDBytes, err := os.ReadFile(childPIDFile)
	if err != nil {
		_ = launched.Terminate(context.Background())
		t.Fatalf("read child pid: %v", err)
	}
	childPID, err := strconv.Atoi(string(childPIDBytes))
	if err != nil {
		_ = launched.Terminate(context.Background())
		t.Fatalf("parse child pid: %v", err)
	}
	if !instanceinfo.ProcessAlive(childPID) {
		_ = launched.Terminate(context.Background())
		t.Fatalf("child %d exited before teardown", childPID)
	}
	if err := launched.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if instanceinfo.ProcessAlive(childPID) {
		t.Fatalf("child %d survived owned process-group teardown", childPID)
	}
}

func TestLaunchRefusesADetachWithNowhereToWrite(t *testing.T) {
	opts := fakeBackendOpts(t, "linger", t.TempDir())
	opts.Detach = true
	if _, err := Launch(context.Background(), opts); err == nil {
		t.Fatal("a detached launch with no log files was accepted")
	}
}

func TestLaunchValidatesItsInputs(t *testing.T) {
	if _, err := Launch(context.Background(), LaunchOptions{DataRoot: t.TempDir()}); err == nil {
		t.Error("Launch accepted an empty binary")
	}
	if _, err := Launch(context.Background(), LaunchOptions{Binary: "/bin/true"}); err == nil {
		t.Error("Launch accepted an empty data root")
	}
}

func TestLaunchRefusesSymlinkedCapturePath(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.log")
	link := filepath.Join(root, "capture.log")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Launch(context.Background(), LaunchOptions{Binary: "/bin/true", DataRoot: root, StdoutPath: link}); err == nil {
		t.Fatal("Launch accepted a symlinked stdout capture path")
	}
}
