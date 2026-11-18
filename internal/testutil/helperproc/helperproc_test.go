//go:build !windows

package helperproc

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		// Until the test kills this process. A bare select would end it at
		// once: with no other goroutine the runtime reports a deadlock.
		for {
			time.Sleep(time.Hour)
		}
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
		_ = starter.Process.Kill()
		_ = starter.Wait()
		t.Fatalf("the starter never started its helper: %v", scanner.Err())
	}
	t.Cleanup(func() { _ = syscall.Kill(held, syscall.SIGKILL) })
	if !running(held) {
		t.Fatal("the helper ended while its starter was running")
	}
	if err := starter.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = starter.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for running(held) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if running(held) {
		t.Fatal("the helper outlived the process that started it")
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

// running reports whether pid exists and has not exited: a zombie has.
func running(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
