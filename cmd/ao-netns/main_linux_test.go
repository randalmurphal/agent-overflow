//go:build linux

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
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
