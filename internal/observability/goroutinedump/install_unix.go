//go:build !windows

package goroutinedump

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Signal is the signal Install listens for. SIGUSR1 is unused by the Go
// runtime and by every subsystem in this app, and `kill -USR1 <pid>`
// needs nothing installed on the machine.
const Signal = syscall.SIGUSR1

// MinInterval is the shortest gap between two dumps. Anyone able to
// signal this process can ask for a dump, and a dump is not cheap: it
// stops the world briefly and writes a file whose size scales with the
// goroutine count, which on a wedged process is exactly when it is
// largest. Unthrottled, a signal loop fills the log directory and
// starves the process it is meant to diagnose.
//
// 10s is far below any human's repeat rate — an operator sending a
// second SIGUSR1 by hand always gets a second dump — and far above the
// rate a loop needs to do damage.
const MinInterval = 10 * time.Second

// Install arms the dump handler for the life of the process: every
// SIGUSR1 writes one dump into dir and reports the path through logf.
// Returns a stop func that unregisters the handler and lets the
// listener goroutine exit.
//
// Unlike the pprof listener this is NOT opt-in. A wedge is exactly the
// situation where nobody set the env var beforehand, and an idle
// signal handler costs one parked goroutine.
//
// Errors are reported through logf, never swallowed: an operator who
// sent the signal and got no file has to learn why.
//
// Signals arriving inside MinInterval of the last dump are COALESCED,
// not queued: the dump they would produce would describe the same wedge
// the last one already does. The suppression is logged rather than
// silent, so an operator who sent a signal and sees no new file is told
// which one is theirs to read.
func Install(dir string, logf func(format string, args ...any)) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, Signal)
	done := make(chan struct{})
	go func() {
		var last time.Time
		for {
			select {
			case <-ch:
				if since := time.Since(last); !last.IsZero() && since < MinInterval {
					logf("goroutine dump: ignored (last dump was %s ago; minimum interval is %s)",
						since.Round(time.Millisecond), MinInterval)
					continue
				}
				last = time.Now()
				path, err := Write(dir)
				if err != nil {
					logf("goroutine dump: %v", err)
					continue
				}
				logf("goroutine dump: wrote %s", path)
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
