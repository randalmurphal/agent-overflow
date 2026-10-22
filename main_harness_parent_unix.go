//go:build unix

package main

import (
	"log"
	"os"
	"syscall"
	"time"
)

// parentPoll is how often a harness backend started with --exit-with-parent
// checks that its parent is still the process that started it.
const parentPoll = 250 * time.Millisecond

// watchParentExit stops this backend once parent is no longer its parent,
// through the SIGTERM path a deliberate stop takes. A harness backend runs
// in a process group of its own, so the group teardown that stops a killed
// test worker never reaches it; without this the backend would outlive the
// worker. The parent PID is the evidence: the kernel reparents an orphan,
// and a PID cannot be reused while a living child still names it.
func watchParentExit(parent int) error {
	go watchParent(parent, os.Getppid, func() {
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			log.Printf("harness: stop after parent exit: %v", err)
		}
	}, parentPoll)
	return nil
}

func watchParent(parent int, ppid func() int, stop func(), poll time.Duration) {
	for ppid() == parent {
		time.Sleep(poll)
	}
	log.Printf("harness: parent %d exited; shutting down", parent)
	stop()
}
