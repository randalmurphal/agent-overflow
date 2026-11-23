//go:build linux

package netisolate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/ownedtemp"

	"golang.org/x/sys/unix"
)

// budgetFor is a host whose RAM temp budget fits exactly n runs, with
// plenty of memory available.
func budgetFor(n int) memoryLimits {
	return memoryLimits{available: 64 << 30, total: uint64(float64(n*tempDirBytes) / ramTempBudgetFraction)}
}

func TestRAMTempAdmission(t *testing.T) {
	const gb = 1 << 30
	for _, tc := range []struct {
		name    string
		limits  memoryLimits
		readErr error
		live    int
		want    bool
		reason  string
	}{
		{name: "first run on a large host", limits: memoryLimits{available: 48 * gb, total: 50 * gb}, want: true},
		{name: "under the budget", limits: budgetFor(3), live: 1, want: true},
		{name: "exactly at the budget", limits: budgetFor(3), live: 2, want: true},
		{name: "over the budget", limits: budgetFor(3), live: 3, reason: "the 12.0 GiB budget for RAM temp directories (50% of the host's 24.0 GiB) has no room for another 4.0 GiB one beside the 3 other runs holding one"},
		{name: "one byte short of the budget", limits: memoryLimits{available: 64 * gb, total: budgetFor(2).total - 2}, live: 1, reason: "has no room"},
		{name: "cgroup limit below MemTotal", limits: memoryLimits{available: 48 * gb, total: 64 * gb, cgroupLimit: 16 * gb}, live: 2, reason: "the 8.0 GiB budget for RAM temp directories (50% of the 16.0 GiB cgroup memory limit)"},
		{name: "cgroup limit admits what it fits", limits: memoryLimits{available: 48 * gb, total: 64 * gb, cgroupLimit: 16 * gb}, live: 1, want: true},
		{name: "cgroup limit above MemTotal", limits: memoryLimits{available: 48 * gb, total: 16 * gb, cgroupLimit: 1 << 62}, live: 2, reason: "(50% of the host's 16.0 GiB)"},
		{name: "available exactly the threshold", limits: memoryLimits{available: ramTempThreshold, total: 64 * gb}, want: true},
		{name: "available one byte under", limits: memoryLimits{available: ramTempThreshold - 1, total: 64 * gb}, reason: "under the"},
		{name: "nearly none available", limits: memoryLimits{available: 512 << 20, total: 64 * gb}, reason: "0.5 GiB of memory available"},
		{name: "unreadable", readErr: errors.New("open /proc/meminfo: no such file or directory"), reason: "unknown (open /proc/meminfo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ram, reason := ramTempAdmit(tc.limits, tc.readErr, tc.live)
			if ram != tc.want || !strings.Contains(reason, tc.reason) || (ram && reason != "") || (!ram && reason == "") {
				t.Fatalf("ramTempAdmit = %v %q, want %v and a reason containing %q", ram, reason, tc.want, tc.reason)
			}
		})
	}
}

func TestParseMeminfo(t *testing.T) {
	meminfo := []byte("MemTotal:       53457840 kB\nMemFree:         8651028 kB\nMemAvailable:   47887028 kB\nBuffers: 1 kB\n")
	for key, want := range map[string]uint64{"MemAvailable": 47887028 << 10, "MemTotal": 53457840 << 10} {
		if got, err := parseMeminfo(meminfo, key); err != nil || got != want {
			t.Fatalf("parseMeminfo(%s) = %d, %v; want %d", key, got, err, want)
		}
	}
	for _, bad := range []string{"MemTotal: 1 kB\n", "MemAvailable: lots kB\n", "MemAvailable: 12 MB\n"} {
		if _, err := parseMeminfo([]byte(bad), "MemAvailable"); err == nil {
			t.Fatalf("parseMeminfo(%q) accepted", bad)
		}
	}
	// The host's own files parse.
	if m, err := readMemoryLimits(); err != nil || m.total == 0 || m.available == 0 {
		t.Fatalf("readMemoryLimits = %+v, %v", m, err)
	}
}

// TestCgroupMemoryLimit: the lowest limit on the cgroup's path counts, v1
// when its memory controller is mounted, else v2; "max", a missing file
// and no cgroup at all set none, and an unreadable value is an error.
func TestCgroupMemoryLimit(t *testing.T) {
	root := t.TempDir()
	write := func(rel, value string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("user.slice/memory.max", "17179869184")
	write("user.slice/session/memory.max", "max")
	write("user.slice/session/app/memory.max", "34359738368")
	write("memory/docker/abc/memory.limit_in_bytes", "8589934592")
	write("memory/docker/memory.limit_in_bytes", "9223372036854771712")
	write("broken/memory.max", "lots")
	for _, tc := range []struct {
		name, cgroup string
		want         uint64
		fails        bool
	}{
		{name: "v2 ancestor lowest", cgroup: "0::/user.slice/session/app\n", want: 16 << 30},
		{name: "v2 none set", cgroup: "0::/other/leaf\n"},
		{name: "v2 root", cgroup: "0::/\n"},
		{name: "v1 memory controller", cgroup: "12:pids:/docker/abc\n4:cpu,memory:/docker/abc\n0::/\n", want: 8 << 30},
		{name: "v1 unlimited", cgroup: "4:memory:/docker\n", want: 9223372036854771712},
		{name: "no cgroup", cgroup: ""},
		{name: "unreadable value", cgroup: "0::/broken\n", fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cgroupMemoryLimit([]byte(tc.cgroup), root)
			if (err != nil) != tc.fails || got != tc.want {
				t.Fatalf("cgroupMemoryLimit = %d, %v; want %d, failure %v", got, err, tc.want, tc.fails)
			}
		})
	}
}

func isTmpfs(t *testing.T, dir string) bool {
	t.Helper()
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		t.Fatal(err)
	}
	return stat.Type == unix.TMPFS_MAGIC
}

// ownRunEnv makes Command build a namespace of its own, as from a shell,
// with a Go work directory: inside one it shares that one's directories.
func ownRunEnv(t *testing.T, dir string) (env []string) {
	t.Helper()
	goEnv := filepath.Join(dir, "go-env")
	if err := os.WriteFile(goEnv, []byte("GOPROXY=off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{EnvNamespace + "=", "GOTMPDIR=", "GOENV=" + goEnv}
}

func setEnv(t *testing.T, env []string) {
	t.Helper()
	for _, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		t.Setenv(key, value)
	}
}

func withMemoryLimits(t *testing.T, limits memoryLimits) {
	t.Helper()
	saved := readMemoryLimits
	readMemoryLimits = func() (memoryLimits, error) { return limits, nil }
	t.Cleanup(func() { readMemoryLimits = saved })
}

// withReservations gives the test an empty reservations directory, in
// this process and the launchers it starts.
func withReservations(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "netisolate-reservations-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv(reservationsEnv, dir)
	return dir
}

var diskNotice = regexp.MustCompile(`netisolate: this run's temp directory is on disk \((\S+)\): (.*)\n`)

// TestLowMemoryRunGetsItsTempDirOnDisk: below the threshold the run's
// TMPDIR is still the fixed TempDir, but a directory on disk; the launcher
// says so once; the files are gone after the run. With plenty of memory
// the same launch gets a tmpfs and says nothing.
func TestLowMemoryRunGetsItsTempDirOnDisk(t *testing.T) {
	binary := probeBinary(t)
	setEnv(t, ownRunEnv(t, filepath.Dir(binary)))
	run := func(t *testing.T) (tempProbe, string, func() error) {
		t.Helper()
		command := exec.Command(binary, "-test.run=^$")
		command.Env = append(os.Environ(), isolationProbeEnv+"=temp")
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		release, err := Command(command, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		if err := command.Run(); err != nil {
			t.Fatalf("isolated probe: %v\n%s", err, stderr.String())
		}
		var probe tempProbe
		if err := json.Unmarshal(stdout.Bytes(), &probe); err != nil {
			t.Fatalf("decode probe %q: %v", stdout.String(), err)
		}
		return probe, stderr.String(), release
	}

	t.Run("below the threshold", func(t *testing.T) {
		withMemoryLimits(t, memoryLimits{available: ramTempThreshold - 1, total: 64 << 30})
		probe, stderr, release := run(t)
		notices := diskNotice.FindAllStringSubmatch(stderr, -1)
		if len(notices) != 1 || !strings.Contains(notices[0][2], "under the") {
			t.Fatalf("stderr = %q, want one notice that the temp directory is on disk and why", stderr)
		}
		dir := notices[0][1]
		if probe.TempDir != TempDir() || probe.TempFS {
			t.Fatalf("TMPDIR = %q (tmpfs %v), want the fixed %q on disk", probe.TempDir, probe.TempFS, TempDir())
		}
		if filepath.Dir(dir) != diskParent() || !strings.HasPrefix(filepath.Base(dir), diskTempPrefix) {
			t.Fatalf("temp directory on disk %q, want %s* in %s", dir, diskTempPrefix, diskParent())
		}
		if !strings.HasPrefix(probe.Written, TempDir()+"/") {
			t.Fatalf("probe wrote %q (%s), want a file under %s", probe.Written, probe.WriteError, TempDir())
		}
		written := filepath.Join(dir, filepath.Base(probe.Written))
		if _, err := os.Stat(written); err != nil {
			t.Fatalf("the probe's temp file is not in the run's directory on disk: %v", err)
		}
		if probe.GoWorkFile == "" {
			t.Fatalf("probe could not write to the Go work directory: %s", probe.GoWorkError)
		}
		if err := release(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the run's temp directory on disk outlived it: %v", err)
		}
	})
	t.Run("plenty", func(t *testing.T) {
		withMemoryLimits(t, memoryLimits{available: ramTempThreshold, total: 64 << 30})
		probe, stderr, release := run(t)
		// The run's disk directory is on disk however its temp directory
		// is, at the fixed DiskDir, and goes with the run.
		if probe.DiskFile == "" || !probe.DiskFS || filepath.Dir(probe.DiskFile) != DiskDir() {
			t.Fatalf("probe wrote %q in the disk directory (on disk %v; %s), want a file in %s on disk", probe.DiskFile, probe.DiskFS, probe.DiskError, DiskDir())
		}
		written, err := filepath.Glob(filepath.Join(diskParent(), diskDirPrefix+"*", filepath.Base(probe.DiskFile)))
		if err != nil || len(written) != 1 {
			t.Fatalf("the probe's disk file in %s: %v, %v; want it in the run's directory there", diskParent(), written, err)
		}
		if err := release(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Dir(written[0])); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the run's disk directory outlived it: %v", err)
		}
		if !probe.TempFS || probe.TempDir != TempDir() || strings.Contains(stderr, "on disk") {
			t.Fatalf("TMPDIR = %q (tmpfs %v), stderr %q; want a tmpfs at %q and no notice", probe.TempDir, probe.TempFS, stderr, TempDir())
		}
	})
}

// controlDirEnv names a host directory the linger probe and the test talk
// through.
const controlDirEnv = "AO_NETISOLATE_PROBE_CONTROL"

// runLauncher stands in for ao-netns on a host with limits: it runs this
// binary as the linger probe through Command.
func runLauncher(limits memoryLimits) int {
	runtime.LockOSThread()
	readMemoryLimits = func() (memoryLimits, error) { return limits, nil }
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	command := exec.Command(self, "-test.run=^$")
	command.Env = append(os.Environ(), isolationProbeEnv+"=linger")
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	release, err := Command(command, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	runErr := command.Run()
	if err := errors.Join(runErr, release()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

type lingerReport struct {
	Pid          int
	TempSource   string
	GoWorkSource string
	Written      string
	Error        string
}

// runLinger reports where its directories come from, leaves a file in the
// temp directory, notes SIGTERM in the control directory without exiting,
// and exits once the test creates "exit" there.
func runLinger(out io.Writer) int {
	control := os.Getenv(controlDirEnv)
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	report := lingerReport{Pid: os.Getpid()}
	var errs []error
	var err error
	report.TempSource, err = mountSource(TempDir())
	errs = append(errs, err)
	report.GoWorkSource, err = mountSource(goWorkPath())
	errs = append(errs, err)
	if file, err := os.CreateTemp("", "linger-"); err != nil {
		errs = append(errs, err)
	} else {
		report.Written = file.Name()
		errs = append(errs, file.Close())
	}
	if err := errors.Join(errs...); err != nil {
		report.Error = err.Error()
	}
	if encodeProbe(out, report) != 0 {
		return 1
	}
	deadline := time.After(2 * time.Minute)
	for {
		select {
		case <-terms:
			if err := os.WriteFile(filepath.Join(control, "term"), nil, 0o600); err != nil {
				return 1
			}
		case <-time.After(20 * time.Millisecond):
			if _, err := os.Stat(filepath.Join(control, "exit")); err == nil {
				return 0
			}
		case <-deadline:
			return 3
		}
	}
}

// mountSource names the host directory mounted at path, from the last mount
// on it in /proc/self/mountinfo.
func mountSource(path string) (string, error) {
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	source := ""
	for _, line := range strings.Split(string(info), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 4 && fields[4] == path {
			source = filepath.Join(diskParent(), filepath.Base(fields[3]))
		}
	}
	if source == "" {
		return "", fmt.Errorf("%s is not a mount point", path)
	}
	return source, nil
}

// processExited reports whether every thread of pid has exited. The
// leader of a Go program turns zombie while its other threads are still
// exiting, and those hold the process's open files, the run's locks among
// them, until they are gone.
func processExited(pid int) bool {
	tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return true
	}
	for _, task := range tasks {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/stat", pid, task.Name()))
		if err != nil {
			continue
		}
		text := string(data)
		fields := strings.Fields(text[strings.LastIndexByte(text, ')')+1:])
		if len(fields) == 0 || (fields[0] != "Z" && fields[0] != "X") {
			return false
		}
	}
	return true
}

// lingeringRun is a launcher process (role) running the linger probe,
// whose control files are in control.
type lingeringRun struct {
	launcher *exec.Cmd
	report   lingerReport
	control  string
	stderr   func() string
}

// startLingeringRun starts binary as the launcher role with env and waits
// for the linger probe's report.
func startLingeringRun(t *testing.T, binary, role string, env []string) *lingeringRun {
	t.Helper()
	run := &lingeringRun{control: filepath.Dir(binary)}
	run.launcher = exec.Command(binary, "-test.run=^$")
	run.launcher.Env = append(append(os.Environ(), env...), isolationProbeEnv+"="+role, controlDirEnv+"="+run.control)
	// A file rather than a pipe: Wait would wait for every holder of a pipe
	// to close it, the command that outlives the launcher included.
	stderr, err := os.Create(filepath.Join(run.control, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stderr.Close() })
	run.launcher.Stderr = stderr
	run.stderr = func() string {
		data, _ := os.ReadFile(stderr.Name())
		return string(data)
	}
	stdout, err := run.launcher.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := run.launcher.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = run.launcher.Process.Kill()
		if run.report.Pid != 0 && !processExited(run.report.Pid) {
			_ = syscall.Kill(run.report.Pid, syscall.SIGKILL)
		}
	})
	line, err := bufio.NewReader(stdout).ReadBytes('\n')
	if err != nil {
		_ = run.launcher.Process.Kill()
		_ = run.launcher.Wait()
		t.Fatalf("linger probe did not report: %v\n%s", err, run.stderr())
	}
	if err := json.Unmarshal(line, &run.report); err != nil || run.report.Error != "" {
		t.Fatalf("linger report %q: %v %s\n%s", line, err, run.report.Error, run.stderr())
	}
	return run
}

// kill sends the launcher SIGKILL and waits until its command, which
// ignores the SIGTERM that follows, has received it.
func (r *lingeringRun) kill(t *testing.T) {
	t.Helper()
	if err := r.launcher.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = r.launcher.Wait()
	waitFor(t, "the command to receive SIGTERM after its launcher was killed", func() bool {
		_, err := os.Stat(filepath.Join(r.control, "term"))
		return err == nil
	})
	if processExited(r.report.Pid) {
		t.Fatal("the command that ignores SIGTERM exited")
	}
}

// end lets the command exit and waits until it has.
func (r *lingeringRun) end(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.control, "exit"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the command to exit", func() bool { return processExited(r.report.Pid) })
}

// runTemp runs binary as the temp probe through Command and returns its
// report and what Command said. It fails the test on a sweep error.
func runTemp(t *testing.T, binary string) (tempProbe, string) {
	t.Helper()
	command := exec.Command(binary, "-test.run=^$")
	command.Env = append(os.Environ(), isolationProbeEnv+"=temp")
	var stdout, notices bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &notices
	release, err := Command(command, &notices)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(command.Run(), release()); err != nil {
		t.Fatalf("run: %v\n%s", err, notices.String())
	}
	if strings.Contains(notices.String(), "remove an earlier run") {
		t.Fatalf("sweep: %s", notices.String())
	}
	var probe tempProbe
	if err := json.Unmarshal(stdout.Bytes(), &probe); err != nil {
		t.Fatalf("decode probe %q: %v", stdout.String(), err)
	}
	return probe, notices.String()
}

// TestKilledLaunchersDirectoriesStayUntilItsRunEnds: SIGKILL of a launcher
// skips its release. Its command receives SIGTERM; one that keeps running
// still holds the run's directories on disk, the temp directory and the Go
// work directory, so the next run's sweep leaves them. Once it exits, the
// run after removes them.
func TestKilledLaunchersDirectoriesStayUntilItsRunEnds(t *testing.T) {
	binary := probeBinary(t)
	env := ownRunEnv(t, filepath.Dir(binary))
	run := startLingeringRun(t, binary, "launcher", env)
	if !diskNotice.MatchString(run.stderr()) {
		t.Fatalf("launcher stderr = %q, want the notice that the temp directory is on disk", run.stderr())
	}
	dirs := map[string]string{run.report.TempSource: diskTempPrefix, run.report.GoWorkSource: goWorkPrefix}
	for dir, prefix := range dirs {
		if !strings.HasPrefix(filepath.Base(dir), prefix) {
			t.Fatalf("directory %q, want a %s* one", dir, prefix)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	if _, err := os.Stat(filepath.Join(run.report.TempSource, filepath.Base(run.report.Written))); err != nil {
		t.Fatalf("the probe's temp file is not in the run's directory on disk: %v", err)
	}
	run.kill(t)

	setEnv(t, env)
	runTemp(t, binary)
	for dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("a run's sweep removed %s while a process of the killed run still used it: %v", dir, err)
		}
	}
	run.end(t)
	runTemp(t, binary)
	for dir := range dirs {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the sweep after the killed run ended left %s: %v", dir, err)
		}
	}
}

// TestKilledLaunchersReservationCountsUntilItsRunEnds: the RAM reservation
// of a launcher killed with SIGKILL is held by its command, so while that
// lives a host with room for one RAM run puts the next run on disk; once
// it exits, the next run gets RAM and the stale reservation is gone.
func TestKilledLaunchersReservationCountsUntilItsRunEnds(t *testing.T) {
	binary := probeBinary(t)
	env := ownRunEnv(t, filepath.Dir(binary))
	reservations := withReservations(t)
	run := startLingeringRun(t, binary, "ram-launcher", env)
	if strings.Contains(run.stderr(), "on disk") {
		t.Fatalf("launcher stderr = %q, want a RAM temp directory", run.stderr())
	}
	run.kill(t)

	setEnv(t, env)
	withMemoryLimits(t, budgetFor(1))
	probe, notices := runTemp(t, binary)
	if probe.TempFS || !strings.Contains(notices, "beside the 1 other runs holding one") {
		t.Fatalf("tmpfs %v, notices %q; want disk while the killed launcher's command holds the only reservation", probe.TempFS, notices)
	}
	run.end(t)
	probe, notices = runTemp(t, binary)
	if !probe.TempFS || strings.Contains(notices, "on disk") {
		t.Fatalf("tmpfs %v, notices %q; want RAM once the killed run ended", probe.TempFS, notices)
	}
	if n, err := ownedtemp.Held(reservations, reservationPrefix); err != nil || n != 0 {
		t.Fatalf("reservations held after every run ended: %d, %v", n, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(reservations, reservationPrefix+"*")); len(matches) != 0 {
		t.Fatalf("reservations left behind: %v", matches)
	}
}

// heldRun is a temp probe started through Command that waits for its
// stdin to close.
type heldRun struct {
	probe   tempProbe
	notices string
	stdin   io.WriteCloser
	command *exec.Cmd
	release func() error
}

func startHeldRun(binary string) (*heldRun, error) {
	run := &heldRun{command: exec.Command(binary, "-test.run=^$")}
	run.command.Env = append(os.Environ(), isolationProbeEnv+"=temp-hold")
	var notices bytes.Buffer
	run.command.Stderr = os.Stderr
	release, err := Command(run.command, &notices)
	if err != nil {
		return nil, err
	}
	run.release = release
	stdout, err := run.command.StdoutPipe()
	if err == nil {
		run.stdin, err = run.command.StdinPipe()
	}
	if err == nil {
		err = run.command.Start()
	}
	if err != nil {
		return nil, errors.Join(err, release())
	}
	line, err := bufio.NewReader(stdout).ReadBytes('\n')
	if err == nil {
		err = json.Unmarshal(line, &run.probe)
	}
	run.notices = notices.String()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("probe report %q: %w", line, err), run.end())
	}
	return run, nil
}

func (r *heldRun) end() error {
	return errors.Join(r.stdin.Close(), r.command.Wait(), r.release())
}

// TestRAMTempAdmissionIsBoundedAcrossRuns: of runs launched at once on a
// host whose budget fits three RAM temp directories, exactly three get
// one and the rest get their temp directory on disk with the notice; once
// a RAM run ends, the next launch gets RAM.
func TestRAMTempAdmissionIsBoundedAcrossRuns(t *testing.T) {
	binary := probeBinary(t)
	setEnv(t, ownRunEnv(t, filepath.Dir(binary)))
	withReservations(t)
	const fit, launched = 3, 5
	withMemoryLimits(t, budgetFor(fit))
	runs := make([]*heldRun, launched)
	errs := make([]error, launched)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			runs[i], errs[i] = startHeldRun(binary)
		}()
	}
	close(start)
	wg.Wait()
	t.Cleanup(func() {
		for _, run := range runs {
			if run != nil {
				_ = run.end()
			}
		}
	})
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	var ram []*heldRun
	for _, run := range runs {
		notices := diskNotice.FindAllStringSubmatch(run.notices, -1)
		switch {
		case run.probe.TempFS && len(notices) == 0:
			ram = append(ram, run)
		case !run.probe.TempFS && len(notices) == 1 && strings.Contains(notices[0][2], "no room for another 4.0 GiB one beside the 3 other runs"):
		default:
			t.Fatalf("run with tmpfs %v said %q; want RAM and no notice, or disk and the budget's notice", run.probe.TempFS, run.notices)
		}
	}
	if len(ram) != fit {
		t.Fatalf("%d of %d runs got a RAM temp directory, want %d", len(ram), launched, fit)
	}

	if err := ram[0].end(); err != nil {
		t.Fatal(err)
	}
	runs[slices.Index(runs, ram[0])] = nil
	next, err := startHeldRun(binary)
	if err != nil {
		t.Fatal(err)
	}
	runs = append(runs, next)
	if !next.probe.TempFS || next.notices != "" {
		t.Fatalf("the launch after a RAM run ended: tmpfs %v, notices %q; want RAM", next.probe.TempFS, next.notices)
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
