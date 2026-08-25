//go:build windows

// keepawake_windows.go holds the assertion with
// kernel32!SetThreadExecutionState.
//
// # Why a parked, locked OS thread
//
// The execution state is PER OS THREAD, not per process: the flags stay
// in force only while the thread that set them is alive, and only that
// same thread can clear or change them. A plain goroutine is useless
// here — the Go runtime may migrate it between OS threads at any
// preemption point, at which moment the inhibit either evaporates (the
// setting thread was recycled) or a later "clear" lands on a thread that
// never set anything. So one goroutine calls runtime.LockOSThread and
// then stays parked for the process lifetime, servicing mode changes over
// a channel. Clearing is SetThreadExecutionState(ES_CONTINUOUS) on that
// very thread.
//
// The thread is created lazily, on the first genuine hold, so a user who
// never turns the feature on never pays for it.
//
// # Two callers, one holder
//
// This file serves both the native Windows build of the app AND
// cmd/agent-overflow-windows, the WSL launcher. In the shipped
// Windows/WSL split the backend runs inside a Linux distro where this
// call does not exist, so the backend emits the power:keepawake
// directive and the launcher answers it by calling power.Apply — landing
// right back here. One implementation, two entry points; do not grow a
// second holder in the launcher.
//
// golang.org/x/sys/windows does not wrap SetThreadExecutionState at the
// pinned version (v0.47.0), so this is a LazySystemDLL proc, the same
// shape as internal/wsllauncher's ntdll!NtResumeProcess shim.
package power

import (
	"fmt"
	"runtime"
	"sync"

	"golang.org/x/sys/windows"
)

// EXECUTION_STATE flags (winbase.h). ES_CONTINUOUS makes the state stick
// until changed rather than resetting the idle timers once.
const (
	esSystemRequired  uint32 = 0x00000001
	esDisplayRequired uint32 = 0x00000002
	esContinuous      uint32 = 0x80000000
)

var (
	kernel32                    = windows.NewLazySystemDLL("kernel32.dll")
	procSetThreadExecutionState = kernel32.NewProc("SetThreadExecutionState")
)

func newOSBackend() backend {
	return &executionStateBackend{requests: make(chan executionStateRequest)}
}

type executionStateRequest struct {
	mode Mode
	done chan error
}

type executionStateBackend struct {
	start    sync.Once
	requests chan executionStateRequest
}

func (b *executionStateBackend) set(mode Mode) error {
	b.start.Do(func() { go b.hold() })
	done := make(chan error, 1)
	b.requests <- executionStateRequest{mode: mode, done: done}
	return <-done
}

// hold owns the one OS thread whose execution state we mutate. It never
// returns: the thread must outlive every mode change, because the state
// dies with it. UnlockOSThread is deliberately absent — unlocking would
// release the thread back to the runtime's pool with our flags still on
// it.
func (b *executionStateBackend) hold() {
	runtime.LockOSThread()
	for req := range b.requests {
		req.done <- setExecutionState(req.mode)
	}
}

// setExecutionState must only ever run on hold's thread.
func setExecutionState(mode Mode) error {
	flags := esContinuous
	switch mode {
	case ModeSystem:
		flags |= esSystemRequired
	case ModeDisplay:
		flags |= esSystemRequired | esDisplayRequired
	}
	// ES_CONTINUOUS on its own is the documented release: it clears the
	// system/display requirements previously set by this thread.
	if err := procSetThreadExecutionState.Find(); err != nil {
		return fmt.Errorf("power: locate kernel32.SetThreadExecutionState: %w", err)
	}
	r1, _, err := procSetThreadExecutionState.Call(uintptr(flags))
	if r1 == 0 {
		// Returns the PREVIOUS state on success, NULL on failure.
		return fmt.Errorf("power: SetThreadExecutionState(0x%08x): %w", flags, err)
	}
	return nil
}
