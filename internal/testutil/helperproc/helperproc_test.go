//go:build !windows

package helperproc

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// helperRoleEnv makes TestHelperRole a process of the test below: "starter"
// stands in for a test binary that starts a tied helper, "held" for the
// helper, and "temp" for a helper that reports its temporary directories.
const helperRoleEnv = "HELPERPROC_TEST_ROLE"

// TestHelperRole is not a test of its own.
func TestHelperRole(t *testing.T) {
	switch os.Getenv(helperRoleEnv) {
	case "starter":
		held := exec.Command(os.Args[0], "-test.run=^TestHelperRole$")
		held.Env = append(os.Environ(), helperRoleEnv+"=held")
		Tie(t, held)
		if err := held.Start(); err != nil {
			t.Fatal(err)
		}
		fmt.Printf("held %d\n", held.Process.Pid)
		// Until the test kills this process, or is gone itself.
		Hold()
	case "held":
		Hold()
	case "temp":
		fmt.Printf("temp %s\n%s\n", os.TempDir(), t.TempDir())
	default:
		t.Skip("runs only as a process of this file's tests")
	}
}

// A test binary killed mid-test runs no cleanup. The helper it tied still
// ends, because the kernel closes the pipe the helper holds on.
func TestAHeldHelperEndsWhenItsStarterIsKilled(t *testing.T) {
	starter := exec.Command(os.Args[0], "-test.run=^TestHelperRole$")
	starter.Env = append(os.Environ(), helperRoleEnv+"=starter")
	stdout, err := starter.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// The starter is killed too, so the directory its own Tie makes goes in
	// this test's.
	Tie(t, starter)
	if err := starter.Start(); err != nil {
		t.Fatal(err)
	}
	held := 0
	scanner := bufio.NewScanner(stdout)
	for held == 0 && scanner.Scan() {
		if rest, ok := strings.CutPrefix(scanner.Text(), "held "); ok {
			held, _ = strconv.Atoi(rest)
		}
	}
	if held == 0 {
		killErr := starter.Process.Kill()
		t.Fatalf("the starter never started its helper: scan %v, kill %v, wait %v", scanner.Err(), killErr, starter.Wait())
	}
	t.Cleanup(func() {
		if err := syscall.Kill(held, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("kill the helper %d: %v", held, err)
		}
	})
	if alive, err := running(held); err != nil || !alive {
		t.Fatalf("the helper ended while its starter was running (probe error %v)", err)
	}
	if err := starter.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	var exit *exec.ExitError
	if err := starter.Wait(); !errors.As(err, &exit) {
		t.Fatalf("the killed starter's wait answered %v, want its exit", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		alive, err := running(held)
		if err != nil {
			t.Fatal(err)
		}
		if !alive {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the helper outlived the process that started it")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A helper whose tie cannot be read can no longer tell whether its starter
// is alive, so Hold exits unsuccessfully and says why instead of reporting a
// clean end.
func TestHoldFailsWhenItsTieCannotBeRead(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reading a directory fails with EISDIR on Linux; other systems may answer it")
	}
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := dir.Close(); err != nil {
			t.Errorf("close the directory: %v", err)
		}
	})
	held := exec.Command(os.Args[0], "-test.run=^TestHelperRole$")
	held.Env = append(os.Environ(), helperRoleEnv+"=held")
	held.Stdin = dir
	var stderr strings.Builder
	held.Stderr = &stderr
	err = held.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("Hold on an unreadable stdin answered %v, want exit status 1", err)
	}
	if !strings.Contains(stderr.String(), "helperproc: read the tie") {
		t.Fatalf("Hold's stderr is %q, want the read failure", stderr.String())
	}
}

// A killed helper removes nothing. What it creates under os.TempDir, and its
// own tests' t.TempDir, land in the directory of the test that tied it,
// wherever the run's own temporary directories point.
func TestATiedHelpersTemporaryFilesGoInItsTestsDirectory(t *testing.T) {
	elsewhere := t.TempDir()
	t.Setenv("TMPDIR", elsewhere)
	t.Setenv("GOTMPDIR", elsewhere)
	helper := exec.Command(os.Args[0], "-test.run=^TestHelperRole$")
	helper.Env = append(os.Environ(), helperRoleEnv+"=temp")
	Tie(t, helper)
	out, err := helper.Output()
	if err != nil {
		t.Fatal(err)
	}
	rest, ok := strings.CutPrefix(string(out), "temp ")
	if !ok {
		t.Fatalf("the helper reported %q", out)
	}
	dirs := strings.Split(rest, "\n")[:2]
	root := filepath.Dir(elsewhere) + string(filepath.Separator)
	for _, dir := range dirs {
		if !strings.HasPrefix(dir, root) || strings.HasPrefix(dir+string(filepath.Separator), elsewhere+string(filepath.Separator)) {
			t.Fatalf("the helper's temporary directories are %q; want them in this test's %s, outside the run's %s", dirs, root, elsewhere)
		}
	}
}

// running reports whether pid exists and has not exited: a zombie has. A
// probe that fails for any reason but the process being gone is an error,
// never an exit.
func running(pid int) (bool, error) {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("signal probe of %d: %w", pid, err)
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		// ps exits 1 for a pid that is gone, which it can be by now.
		if probe := syscall.Kill(pid, 0); errors.Is(probe, syscall.ESRCH) {
			return false, nil
		}
		return false, fmt.Errorf("ps -p %d: %w (output %q)", pid, err, out)
	}
	return !strings.HasPrefix(strings.TrimSpace(string(out)), "Z"), nil
}
