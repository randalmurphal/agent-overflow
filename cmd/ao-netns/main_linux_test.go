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
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/netisolate"
)

const roleEnv = "AO_NETNS_TEST_ROLE"

// TestMain lets the test binary stand in for both ao-netns, whose helper
// re-executes os.Executable(), and the program it runs.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == netisolate.HelperArg {
		os.Exit(netisolate.RunHelper(os.Args[2:], os.Stderr))
	}
	switch os.Getenv(roleEnv) {
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
		writer.Close()
	}()
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil {
		t.Fatalf("program did not start: %v", err)
	}
	go io.Copy(io.Discard, reader)
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
		writer.Close()
	}()
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("program did not start: %q %v", line, err)
	}
	go io.Copy(io.Discard, reader)
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
