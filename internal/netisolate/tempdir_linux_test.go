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
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRAMTempNeedsTheCapPlusTheReserve(t *testing.T) {
	for _, tc := range []struct {
		name      string
		available uint64
		readErr   error
		want      bool
		reason    string
	}{
		{"plenty", 48 << 30, nil, true, ""},
		{"exactly the threshold", ramTempThreshold, nil, true, ""},
		{"one byte under", ramTempThreshold - 1, nil, false, "under the"},
		{"nearly none", 512 << 20, nil, false, "0.5 GiB of memory available"},
		{"unreadable", 0, errors.New("open /proc/meminfo: no such file or directory"), false, "unknown (open /proc/meminfo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ram, reason := ramTempAffordable(tc.available, tc.readErr)
			if ram != tc.want || !strings.Contains(reason, tc.reason) || (ram && reason != "") {
				t.Fatalf("ramTempAffordable = %v %q, want %v and a reason containing %q", ram, reason, tc.want, tc.reason)
			}
		})
	}
}

func TestParseMemAvailable(t *testing.T) {
	got, err := parseMemAvailable([]byte("MemTotal:       53457840 kB\nMemFree:         8651028 kB\nMemAvailable:   47887028 kB\nBuffers: 1 kB\n"))
	if err != nil || got != 47887028<<10 {
		t.Fatalf("parseMemAvailable = %d, %v; want %d", got, err, uint64(47887028)<<10)
	}
	for _, bad := range []string{"MemTotal: 1 kB\n", "MemAvailable: lots kB\n", "MemAvailable: 12 MB\n"} {
		if _, err := parseMemAvailable([]byte(bad)); err == nil {
			t.Fatalf("parseMemAvailable(%q) accepted", bad)
		}
	}
	// The host's own file parses.
	if _, err := readMemAvailable(); err != nil {
		t.Fatal(err)
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

func withMemAvailable(t *testing.T, available uint64) {
	t.Helper()
	saved := readMemAvailable
	readMemAvailable = func() (uint64, error) { return available, nil }
	t.Cleanup(func() { readMemAvailable = saved })
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
		withMemAvailable(t, ramTempThreshold-1)
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
			t.Fatal("probe could not write to the Go work directory")
		}
		if err := release(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the run's temp directory on disk outlived it: %v", err)
		}
	})
	t.Run("plenty", func(t *testing.T) {
		withMemAvailable(t, ramTempThreshold)
		probe, stderr, release := run(t)
		if err := release(); err != nil {
			t.Fatal(err)
		}
		if !probe.TempFS || probe.TempDir != TempDir() || strings.Contains(stderr, "on disk") {
			t.Fatalf("TMPDIR = %q (tmpfs %v), stderr %q; want a tmpfs at %q and no notice", probe.TempDir, probe.TempFS, stderr, TempDir())
		}
	})
}

// controlDirEnv names a host directory the linger probe and the test talk
// through.
const controlDirEnv = "AO_NETISOLATE_PROBE_CONTROL"

// runLowMemoryLauncher stands in for ao-netns on a host short of memory:
// it runs this binary as the linger probe through Command, with its temp
// directory on disk.
func runLowMemoryLauncher() int {
	runtime.LockOSThread()
	readMemAvailable = func() (uint64, error) { return 0, nil }
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

func processExited(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	text := string(data)
	fields := strings.Fields(text[strings.LastIndexByte(text, ')')+1:])
	return len(fields) > 0 && (fields[0] == "Z" || fields[0] == "X")
}

// TestKilledLaunchersDirectoriesStayUntilItsRunEnds: SIGKILL of a launcher
// skips its release. Its command receives SIGTERM; one that keeps running
// still holds the run's directories on disk, the temp directory and the Go
// work directory, so the next run's sweep leaves them. Once it exits, the
// run after removes them.
func TestKilledLaunchersDirectoriesStayUntilItsRunEnds(t *testing.T) {
	binary := probeBinary(t)
	control := filepath.Dir(binary)
	env := ownRunEnv(t, control)
	launcher := exec.Command(binary, "-test.run=^$")
	launcher.Env = append(append(os.Environ(), env...), isolationProbeEnv+"=launcher", controlDirEnv+"="+control)
	// A file rather than a pipe: Wait would wait for every holder of a pipe
	// to close it, the command that outlives the launcher included.
	stderr, err := os.Create(filepath.Join(control, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	launcher.Stderr = stderr
	readStderr := func() string {
		data, _ := os.ReadFile(stderr.Name())
		return string(data)
	}
	stdout, err := launcher.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.Start(); err != nil {
		t.Fatal(err)
	}
	var report lingerReport
	t.Cleanup(func() {
		_ = launcher.Process.Kill()
		if report.Pid != 0 && !processExited(report.Pid) {
			_ = syscall.Kill(report.Pid, syscall.SIGKILL)
		}
	})
	line, err := bufio.NewReader(stdout).ReadBytes('\n')
	if err != nil {
		_ = launcher.Process.Kill()
		_ = launcher.Wait()
		t.Fatalf("linger probe did not report: %v\n%s", err, readStderr())
	}
	if err := json.Unmarshal(line, &report); err != nil || report.Error != "" {
		t.Fatalf("linger report %q: %v %s\n%s", line, err, report.Error, readStderr())
	}
	if !diskNotice.MatchString(readStderr()) {
		t.Fatalf("launcher stderr = %q, want the notice that the temp directory is on disk", readStderr())
	}
	dirs := map[string]string{report.TempSource: diskTempPrefix, report.GoWorkSource: goWorkPrefix}
	for dir, prefix := range dirs {
		if !strings.HasPrefix(filepath.Base(dir), prefix) {
			t.Fatalf("directory %q, want a %s* one", dir, prefix)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	if _, err := os.Stat(filepath.Join(report.TempSource, filepath.Base(report.Written))); err != nil {
		t.Fatalf("the probe's temp file is not in the run's directory on disk: %v", err)
	}

	if err := launcher.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = launcher.Wait()
	waitFor(t, "the command to receive SIGTERM after its launcher was killed", func() bool {
		_, err := os.Stat(filepath.Join(control, "term"))
		return err == nil
	})
	if processExited(report.Pid) {
		t.Fatal("the command that ignores SIGTERM exited")
	}

	setEnv(t, env)
	runOnce := func() {
		t.Helper()
		command := exec.Command(binary, "-test.run=^$")
		var out bytes.Buffer
		command.Stdout, command.Stderr = &out, &out
		release, err := Command(command, &out)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(command.Run(), release()); err != nil {
			t.Fatalf("second run: %v\n%s", err, out.String())
		}
		if strings.Contains(out.String(), "remove an earlier run") {
			t.Fatalf("sweep: %s", out.String())
		}
	}
	runOnce()
	for dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("a run's sweep removed %s while a process of the killed run still used it: %v", dir, err)
		}
	}

	if err := os.WriteFile(filepath.Join(control, "exit"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the command to exit", func() bool { return processExited(report.Pid) })
	runOnce()
	for dir := range dirs {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the sweep after the killed run ended left %s: %v", dir, err)
		}
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
