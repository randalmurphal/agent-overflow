package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// leaderArg is argv[1] when this command runs as a job's leader (lead).
const leaderArg = "__ao-gotest-lead"

// forwarded are the signals this command and each leader pass on: those a
// terminal or a test runner sends, SIGQUIT included, which asks a Go
// program for its stacks.
var forwarded = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}

// endGrace is how long the processes a job left have after SIGTERM before
// SIGKILL.
var endGrace = 5 * time.Second

// lead runs argv, a job's `go test`, as the leader of its process tree,
// and exits with its status once nothing of the tree is left. The go
// command dies of SIGTERM without passing it on to its test binary, and a
// test may start processes in groups of their own, so a signal to the `go
// test` alone, or its death, leaves them running.
//
// The `go test` leads a process group of its own, which every signal the
// leader receives goes to. On Linux the leader is also the subreaper of
// the tree, so a process whose parent exits comes to it, whatever its
// group, and is waited for as it exits (reapUntil). The leader reads lifeline, a pipe whose other end only the
// launcher holds, so the read ends when the launcher exits, however it
// exits, and the leader then ends the tree as for SIGTERM. Once the `go
// test` has exited, endTree ends what is left and waits for it; what was
// still running after a `go test` that ended on its own is named on
// stderr, since a test leaked it.
func lead(argv []string, lifeline io.Reader, stderr io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "ao-gotest: a leader needs the command it leads")
		return 2
	}
	signals := make(chan os.Signal, len(forwarded)+1)
	signal.Notify(signals, forwarded...)
	if err := adoptOrphans(); err != nil {
		fmt.Fprintln(stderr, "ao-gotest: lead the processes of a go test:", err)
		return 1
	}
	command := exec.Command(argv[0], argv[1:]...)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		fmt.Fprintf(stderr, "ao-gotest: start %s: %v\n", strings.Join(command.Args, " "), err)
		return 1
	}
	group := command.Process.Pid
	go func() {
		_, _ = io.Copy(io.Discard, lifeline)
		signals <- syscall.SIGTERM
	}()
	type waited struct {
		status syscall.WaitStatus
		err    error
	}
	exited := make(chan waited, 1)
	go func() {
		status, err := reapUntil(group)
		exited <- waited{status, err}
	}()
	stopped := false
	for {
		select {
		case sig := <-signals:
			stopped = true
			if err := syscall.Kill(-group, sig.(syscall.Signal)); err != nil && !errors.Is(err, syscall.ESRCH) {
				fmt.Fprintf(stderr, "ao-gotest: pass %v to the processes of a go test: %v\n", sig, err)
			}
		case w := <-exited:
			status := waitedStatus(w.status)
			if w.err != nil {
				fmt.Fprintln(stderr, "ao-gotest:", w.err)
				status = 1
			}
			left, endErr := endTree(group, endGrace)
			if endErr != nil {
				fmt.Fprintln(stderr, "ao-gotest: end the processes a go test left:", endErr)
				status = max(status, 1)
			}
			if len(left) > 0 && !stopped {
				fmt.Fprintf(stderr, "ao-gotest: the go test left %d processes running, which were ended: %s\n", len(left), strings.Join(left, "; "))
			}
			return status
		}
	}
}

// reapUntil waits for each child of this process as it exits until pid
// does, and returns how pid ended. On Linux the leader is the subreaper of
// its tree, so a process whose parent exits comes to it; until it is
// waited for, it stays a zombie, which kill(pid, 0) and a test's check that
// a process group has ended still find. The go test is waited for here and
// not through exec.Cmd.Wait, which a wait for any child could take its
// status from. endTree waits for the rest once pid has exited.
func reapUntil(pid int) (syscall.WaitStatus, error) {
	for {
		var status syscall.WaitStatus
		got, err := syscall.Wait4(-1, &status, 0, nil)
		switch {
		case errors.Is(err, syscall.EINTR):
		case err != nil:
			return 0, fmt.Errorf("wait for the go test (pid %d): %w", pid, err)
		case got == pid:
			return status, nil
		}
	}
}
