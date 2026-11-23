//go:build linux

package netisolate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/ownedtemp"
)

// holdReport is what the hold probe writes to holdReportName in the
// control directory: its pid, the run directories whose lock files its
// descriptors hold, and the run's Go work and disk directories.
type holdReport struct {
	Pid      int
	LockDirs []string
	GoWork   string
	DiskDir  string
	Error    string
}

const holdReportName = "hold-report"

// runSpawn stands in for a test runner between the run's command and the
// process that uses the run's directories: it starts this binary as the
// hold probe with os/exec's defaults, passing nothing on explicitly, and
// exits without waiting for it.
func runSpawn() int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	child := exec.Command(self, "-test.run=^$")
	child.Env = append(os.Environ(), isolationProbeEnv+"=hold")
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// runHold reports which run directories' locks it holds, then waits until
// the test creates "exit" in the control directory.
func runHold() int {
	control := os.Getenv(controlDirEnv)
	report := holdReport{Pid: os.Getpid()}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 3
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if err == nil && filepath.Base(target) == ".ownedtemp-lock" {
			report.LockDirs = append(report.LockDirs, filepath.Dir(target))
		}
	}
	if report.GoWork, err = mountSource(goWorkPath()); err != nil {
		report.Error = err.Error()
	}
	if report.DiskDir, err = mountSource(DiskDir()); err != nil {
		report.Error = err.Error()
	}
	data, err := json.Marshal(report)
	if err != nil {
		return 3
	}
	pending := filepath.Join(control, holdReportName+".pending")
	if os.WriteFile(pending, data, 0o600) != nil || os.Rename(pending, filepath.Join(control, holdReportName)) != nil {
		return 3
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(control, "exit")); err == nil {
			return 0
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 4
}

// testGrandchildKeepsTheRun runs intermediate, a command that starts the
// hold probe and exits, with its temp directory on disk or in RAM. The
// probe, the run's grandchild, keeps the run's directories and its RAM
// reservation owned, so the launcher's release and a later run's sweep
// leave them until it exits, and the sweep after that removes them. It
// returns the run directories whose locks the probe held.
func testGrandchildKeepsTheRun(t *testing.T, binary string, ram bool, intermediate *exec.Cmd) (lockDirs []string) {
	t.Helper()
	control := os.Getenv(controlDirEnv)
	for _, name := range []string{holdReportName, "exit"} {
		if err := os.Remove(filepath.Join(control, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	reservations := withReservations(t)
	if ram {
		withMemoryLimits(t, budgetFor(1))
	} else {
		withMemoryLimits(t, memoryLimits{available: 0, total: 64 << 30})
	}
	// A file rather than a pipe: Wait would wait for every holder of a
	// pipe, the grandchild included.
	output, err := os.Create(filepath.Join(control, "intermediate-output"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	intermediate.Stdout, intermediate.Stderr = output, output
	var notices bytes.Buffer
	release, err := Command(intermediate, &notices)
	if err != nil {
		t.Fatal(err)
	}
	runErr := intermediate.Run()
	var report holdReport
	t.Cleanup(func() {
		if report.Pid != 0 && !processExited(report.Pid) {
			_ = os.WriteFile(filepath.Join(control, "exit"), nil, 0o600)
		}
	})
	if runErr != nil {
		data, _ := os.ReadFile(output.Name())
		t.Fatalf("intermediate: %v\n%s", errors.Join(runErr, release()), data)
	}
	waitFor(t, "the grandchild's report", func() bool {
		data, err := os.ReadFile(filepath.Join(control, holdReportName))
		return err == nil && json.Unmarshal(data, &report) == nil
	})
	if report.Error != "" || !strings.HasPrefix(filepath.Base(report.GoWork), goWorkPrefix) {
		t.Fatalf("grandchild report %+v, want its Go work directory", report)
	}
	dirs := []string{report.GoWork}
	if !strings.HasPrefix(filepath.Base(report.DiskDir), diskDirPrefix) {
		t.Fatalf("grandchild report %+v, want its disk directory", report)
	}
	dirs = append(dirs, report.DiskDir)
	notice := diskNotice.FindStringSubmatch(notices.String())
	switch {
	case ram && notice == nil:
		matches, _ := filepath.Glob(filepath.Join(reservations, reservationPrefix+"*"))
		if len(matches) != 1 {
			t.Fatalf("reservations %v, want this run's", matches)
		}
		dirs = append(dirs, matches[0])
	case !ram && notice != nil:
		dirs = append(dirs, notice[1])
	default:
		t.Fatalf("notices %q, want the temp directory in RAM %v", notices.String(), ram)
	}
	for _, dir := range dirs {
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}

	if err := release(); !errors.Is(err, ownedtemp.ErrInUse) {
		t.Fatalf("release while the grandchild lives = %v, want ErrInUse", err)
	}
	probe, _ := runTemp(t, binary)
	for _, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("a later run's sweep removed %s while the grandchild used it: %v", dir, err)
		}
	}
	if ram && probe.TempFS {
		t.Fatal("a later run got a RAM temp directory while the grandchild kept the only reservation")
	}
	if err := os.WriteFile(filepath.Join(control, "exit"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the grandchild to exit", func() bool { return processExited(report.Pid) })
	probe, _ = runTemp(t, binary)
	for _, dir := range dirs {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the sweep after the grandchild exited left %s: %v", dir, err)
		}
	}
	if ram && !probe.TempFS {
		t.Fatal("the run after the grandchild exited did not get the freed RAM reservation")
	}
	return report.LockDirs
}

// grandchildEnv makes the test binary at binary the run's program, with
// its control directory beside it, in a namespace of its own.
func grandchildEnv(t *testing.T) string {
	t.Helper()
	binary := probeBinary(t)
	control := filepath.Dir(binary)
	setEnv(t, ownRunEnv(t, control))
	t.Setenv(controlDirEnv, control)
	return binary
}

var temps = []struct {
	name string
	ram  bool
}{{"disk temp", false}, {"RAM temp", true}}

// TestGoGrandchildKeepsTheRun: a Go program that starts a child with
// os/exec's defaults and exits leaves that child keeping the run, and
// holding its locks: os/exec passes on the descriptors it inherited.
func TestGoGrandchildKeepsTheRun(t *testing.T) {
	for _, temp := range temps {
		t.Run(temp.name, func(t *testing.T) {
			binary := grandchildEnv(t)
			intermediate := exec.Command(binary, "-test.run=^$")
			intermediate.Env = append(os.Environ(), isolationProbeEnv+"=spawn")
			if locks := testGrandchildKeepsTheRun(t, binary, temp.ram, intermediate); len(locks) != 3 {
				t.Fatalf("the grandchild holds the locks of %v; want all three run directories'", locks)
			}
		})
	}
}

// TestNodeGrandchildKeepsTheRun: the same for Node's child_process.spawn,
// which Vitest and Playwright start their workers with. It closes the
// descriptors its child would inherit, so only the run's mount namespace
// keeps the run.
func TestNodeGrandchildKeepsTheRun(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH, so Node's child_process.spawn cannot be checked here")
	}
	const script = `const child = require("child_process").spawn(process.argv[1], ["-test.run=^$"]);
child.on("spawn", () => process.exit(0));
child.on("error", (err) => { console.error(err); process.exit(3); });`
	for _, temp := range temps {
		t.Run(temp.name, func(t *testing.T) {
			binary := grandchildEnv(t)
			intermediate := exec.Command(node, "-e", script, binary)
			intermediate.Env = append(os.Environ(), isolationProbeEnv+"=hold")
			locks := testGrandchildKeepsTheRun(t, binary, temp.ram, intermediate)
			t.Logf("the Node grandchild holds the locks of %d run directories", len(locks))
		})
	}
}
