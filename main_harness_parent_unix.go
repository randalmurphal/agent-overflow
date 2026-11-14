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

// orphanExitDeadline bounds the stop an orphaned backend starts: the
// headless shutdown's own bound plus the transport drain. Past it the
// backend exits without its deferred cleanup, which is acceptable for an
// orphan: nothing reads its output, and a stop blocked writing that output
// would otherwise never end.
var orphanExitDeadline = headlessShutdownTimeout + transportShutdownTimeout

// watchParentExit stops this backend once parent is no longer its parent,
// through the SIGTERM path a deliberate stop takes. A harness backend runs
// in a process group of its own, so the group teardown that stops a killed
// test worker never reaches it; without this the backend would outlive the
// worker. The parent PID is the evidence: the kernel reparents an orphan,
// and a PID cannot be reused while a living child still names it.
func watchParentExit(parent int) error {
	go watchParent(parent, os.Getppid, stopOrphan, parentPoll)
	return nil
}

// stopOrphan arms the exit that ends a stop which does not finish, then
// starts the stop. Neither waits on logging: with the worker gone, stderr
// can be a full pipe nobody reads.
func stopOrphan() {
	time.AfterFunc(orphanExitDeadline, func() { os.Exit(1) })
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		log.Printf("harness: stop after parent exit: %v", err)
	}
}

func watchParent(parent int, ppid func() int, stop func(), poll time.Duration) {
	for ppid() == parent {
		time.Sleep(poll)
	}
	// Stop first: this line can block on a full stderr pipe.
	stop()
	log.Printf("harness: parent %d exited; shutting down", parent)
}
