// Command ao-netns runs a program inside the test network namespace from
// internal/netisolate: loopback plus a private LAN interface and no route
// off it. The Make test targets run `go test` and the frontend test scripts
// under it, so no test reaches the host network.
//
//	ao-netns <program> [args...]
//
// It passes stdio, the working directory and the environment through, exits
// with the program's status (128+N when signal N ended it), and forwards the
// signals a test runner sends, including SIGQUIT, which `go test` sends a
// timed-out test binary for a stack dump. A host that refuses unprivileged user namespaces
// fails the run; nothing falls back to the host network. On non-Linux hosts
// the program runs directly.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"agent-overflow/internal/netisolate"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == netisolate.HelperArg {
		os.Exit(netisolate.RunHelper(os.Args[2:], os.Stderr))
	}
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// forwarded are the signals a test runner or terminal sends to the process
// it started, which is this one rather than the program.
var forwarded = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: ao-netns <program> [args...]")
		return 2
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		fmt.Fprintln(stderr, "ao-netns:", err)
		return 127
	}
	command := exec.Command(path, args[1:]...)
	command.Args[0] = args[0]
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	if err := netisolate.Command(command); err != nil {
		fmt.Fprintln(stderr, "ao-netns:", err)
		return 1
	}

	signals := make(chan os.Signal, len(forwarded))
	signal.Notify(signals, forwarded...)
	defer signal.Stop(signals)
	if err := command.Start(); err != nil {
		fmt.Fprintln(stderr, "ao-netns: start in an isolated network namespace (unprivileged user namespaces must be enabled):", err)
		return 1
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-signals:
				if err := command.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
					fmt.Fprintln(stderr, "ao-netns: forward", sig, "to the program:", err)
				}
			case <-done:
				return
			}
		}
	}()
	err = command.Wait()
	close(done)
	return exitStatus(err, stderr)
}

func exitStatus(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		fmt.Fprintln(stderr, "ao-netns:", err)
		return 1
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return exitErr.ExitCode()
}
