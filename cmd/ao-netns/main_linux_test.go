//go:build linux

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/netisolate"
)

const (
	roleEnv = "AO_NETNS_TEST_ROLE"
	// launcherEnv makes the test binary ao-netns itself, run as a process
	// of its own so a test can kill it.
	launcherEnv = "AO_NETNS_TEST_LAUNCHER"
	// markerEnv names the file the await-sigterm program creates when
	// SIGTERM arrives.
	markerEnv = "AO_NETNS_TEST_MARKER"
)

// TestMain lets the test binary stand in for both ao-netns, whose helper
// re-executes os.Executable(), and the program it runs.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == netisolate.HelperArg {
		os.Exit(netisolate.RunHelper(os.Args[2:], os.Stderr))
	}
	if os.Getenv(launcherEnv) != "" {
		if err := os.Setenv(launcherEnv, ""); err != nil {
			fmt.Fprintln(os.Stderr, "clear", launcherEnv+":", err)
			os.Exit(1)
		}
		// It runs this binary again as the program its role names.
		os.Exit(run(os.Args, os.Stdin, os.Stdout, os.Stderr))
	}
	switch os.Getenv(roleEnv) {
	case "await-sigterm":
		terms := make(chan os.Signal, 1)
		signal.Notify(terms, syscall.SIGTERM)
		fmt.Println("ready")
		select {
		case <-terms:
			if err := os.WriteFile(os.Getenv(markerEnv), nil, 0o600); err != nil {
				os.Exit(3)
			}
			os.Exit(0)
		case <-time.After(time.Minute):
			os.Exit(5)
		}
	case "interfaces":
		ifaces, err := net.Interfaces()
		if err != nil {
			os.Exit(3)
		}
		for _, iface := range ifaces {
			fmt.Println(iface.Name)
		}
		os.Exit(0)
	case "exit7":
		os.Exit(7)
	case "await-term":
		dir, err := goWorkSource()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		fmt.Println(dir)
		time.Sleep(10 * time.Second)
		os.Exit(5)
	case "await-quit":
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGQUIT)
		fmt.Println("ready")
		select {
		case <-quit:
			os.Exit(42)
		case <-time.After(10 * time.Second):
			os.Exit(5)
		}
	}
	os.Exit(m.Run())
}

func runSelf(t *testing.T, role string, stdout io.Writer) int {
	t.Helper()
	t.Setenv(roleEnv, role)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code := run([]string{self, "-test.run=^$"}, nil, stdout, &stderr)
	if stderr.Len() > 0 {
		t.Logf("stderr: %s", stderr.String())
	}
	return code
}

func TestProgramSeesOnlyTheIsolatedInterfaces(t *testing.T) {
	var stdout bytes.Buffer
	if code := runSelf(t, "interfaces", &stdout); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got, want := strings.Fields(stdout.String()), []string{"lo", netisolate.LANName}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("interfaces = %v, want %v", got, want)
	}
}

// TestGoWorkDirectoryIsRemovedWhenASignalEndsTheRun: the run's Go work
// directory on disk is gone once a forwarded SIGTERM has ended the program.
func TestGoWorkDirectoryIsRemovedWhenASignalEndsTheRun(t *testing.T) {
	// A namespace of its own, as from a shell: inside one, the run shares
	// that one's work directory and makes none.
	t.Setenv(netisolate.EnvNamespace, "")
	t.Setenv("GOTMPDIR", "")
	t.Setenv("GOENV", filepath.Join(t.TempDir(), "absent"))
	t.Setenv(roleEnv, "await-term")
	// Under make go-test this binary sits in the outer namespace's temp
	// directory, which the new namespace's own mount hides.
	binary := copyOutsideTempDir(t)
	reader, writer := io.Pipe()
	code := make(chan int, 1)
	go func() {
		var stderr bytes.Buffer
		code <- run([]string{binary, "-test.run=^$"}, nil, writer, &stderr)
		t.Logf("stderr: %s", stderr.String())
		if err := writer.Close(); err != nil {
			t.Errorf("close the program's output: %v", err)
		}
	}()
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil {
		t.Fatalf("program did not start: %v", err)
	}
	go func() {
		if _, err := io.Copy(io.Discard, reader); err != nil {
			t.Errorf("drain the program's output: %v", err)
		}
	}()
	dir := strings.TrimSpace(line)
	if !strings.HasPrefix(filepath.Base(dir), "ao-gowork-") {
		t.Fatalf("program reports Go work directory %q", dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("Go work directory while the program runs: %v", err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-code:
		if got != 128+int(syscall.SIGTERM) {
			t.Fatalf("exit %d, want the program's SIGTERM status", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("program never exited")
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Go work directory %s outlived the run: %v", dir, err)
	}
}

// goWorkSource names the directory mounted at the run's Go work directory,
// from the mount's root in /proc/self/mountinfo, in the host's temp
// directory.
func goWorkSource() (string, error) {
	data, err := os.ReadFile(filepath.Join(os.Getenv("TMPDIR"), ".ao-go-env"))
	if err != nil {
		return "", err
	}
	_, mount, ok := strings.Cut(strings.TrimSpace(string(data)), "GOTMPDIR=")
	if !ok {
		return "", fmt.Errorf("no GOTMPDIR in %q", data)
	}
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	// The last mount on the path is the visible one; an outer namespace's
	// may be listed under it.
	source := ""
	for _, line := range strings.Split(string(info), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 4 && fields[4] == mount {
			source = filepath.Join(os.Getenv(netisolate.EnvHostTempDir), filepath.Base(fields[3]))
		}
	}
	if source == "" {
		return "", fmt.Errorf("%s is not a mount point", mount)
	}
	return source, nil
}

func copyOutsideTempDir(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "ao-netns-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove test binary copy: %v", err)
		}
	})
	binary := filepath.Join(dir, "ao-netns.test")
	if err := os.WriteFile(binary, data, 0o700); err != nil {
		t.Fatal(err)
	}
	return binary
}

// TestKilledLauncherEndsTheRun: SIGKILL of ao-netns skips everything it
// would do on the way out; the program it started receives SIGTERM, from
// the parent-death signal kept through the helper's exec, and ends.
func TestKilledLauncherEndsTheRun(t *testing.T) {
	t.Setenv(netisolate.EnvNamespace, "")
	t.Setenv("GOTMPDIR", "")
	t.Setenv("GOENV", filepath.Join(t.TempDir(), "absent"))
	binary := copyOutsideTempDir(t)
	dir := filepath.Dir(binary)
	marker := filepath.Join(dir, "terminated")
	launcher := exec.Command(binary, "-test.run=^$")
	launcher.Env = append(os.Environ(), launcherEnv+"=1", roleEnv+"=await-sigterm", markerEnv+"="+marker)
	// Files rather than pipes: Wait would wait for the program, which
	// shares them, to close them too.
	out, err := os.Create(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := out.Close(); err != nil {
			t.Errorf("close the output file: %v", err)
		}
	}()
	launcher.Stdout, launcher.Stderr = out, out
	if err := launcher.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Process.Kill() })
	output := func() string {
		data, _ := os.ReadFile(out.Name())
		return string(data)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("ao-netns output:\n%s", output())
		}
	})
	waitFor(t, "the program to start", func() bool { return strings.Contains(output(), "ready\n") })
	if err := launcher.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = launcher.Wait()
	waitFor(t, "the program to receive SIGTERM after ao-netns was killed", func() bool {
		_, err := os.Stat(marker)
		return err == nil
	})
	// The next run removes the Go work directory the killed one left, so it
	// has nothing to report about it.
	t.Setenv(roleEnv, "exit7")
	var stderr bytes.Buffer
	code := run([]string{binary, "-test.run=^$"}, nil, io.Discard, &stderr)
	if rest := withoutDiskNotice(stderr.String()); code != 7 || rest != "" {
		t.Fatalf("next run: exit %d, want 7 and nothing reported\n%s", code, rest)
	}
}

// withoutDiskNotice is a run's stderr without the notice that its temp
// directory is on disk. A host with RAM for one run's temp directory gives
// it to the run these tests are part of, so a run a test starts gets disk.
func withoutDiskNotice(stderr string) string {
	var rest strings.Builder
	for line := range strings.Lines(stderr) {
		if !strings.HasPrefix(line, "netisolate: this run's temp directory is on disk ") {
			rest.WriteString(line)
		}
	}
	return rest.String()
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

func TestExitStatusPassesThrough(t *testing.T) {
	if code := runSelf(t, "exit7", io.Discard); code != 7 {
		t.Fatalf("exit %d, want 7", code)
	}
}

// go test -timeout sends SIGQUIT to the process it started, which is
// ao-netns; the test binary has to receive it to dump its stacks.
func TestSIGQUITReachesTheProgram(t *testing.T) {
	reader, writer := io.Pipe()
	code := make(chan int, 1)
	go func() {
		code <- runSelf(t, "await-quit", writer)
		if err := writer.Close(); err != nil {
			t.Errorf("close the program's output: %v", err)
		}
	}()
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("program did not start: %q %v", line, err)
	}
	go func() {
		if _, err := io.Copy(io.Discard, reader); err != nil {
			t.Errorf("drain the program's output: %v", err)
		}
	}()
	if err := syscall.Kill(os.Getpid(), syscall.SIGQUIT); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-code:
		if got != 42 {
			t.Fatalf("exit %d, want 42 from the program's SIGQUIT handler", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("program never exited")
	}
}

func TestMissingProgramIsReported(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"/no/such/program-ao-netns"}, nil, io.Discard, &stderr); code != 127 {
		t.Fatalf("exit %d, want 127", code)
	}
}
