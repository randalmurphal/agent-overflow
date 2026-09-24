package startupprogress

import (
	"fmt"
	"time"
)

// StallWindow is how long a start may go without observed progress, or
// without any sign of life, before it is judged stalled. The Windows
// launcher's probe, an update's trial and serve mode apply it.
const StallWindow = 30 * time.Second

// Stall is a start judged stalled. Unresponsive means its heartbeat stopped
// as well: neither AliveAt nor UpdatedAt advanced for Quiet.
type Stall struct {
	// Progress is the last report.
	Progress     Progress
	Quiet        time.Duration
	Unresponsive bool
}

// The two ways a start stalls, as every judge words them.
const (
	StallNoProgress   = "no progress"
	StallUnresponsive = "backend stopped responding"
)

func (s *Stall) Error() string {
	what := StallNoProgress
	if s.Unresponsive {
		what = StallUnresponsive
	}
	quiet := s.Quiet.Round(time.Millisecond)
	if s.Quiet >= time.Second {
		quiet = s.Quiet.Round(time.Second)
	}
	return fmt.Sprintf("%s for %s in phase %s (%s)", what, quiet, s.Progress.Phase, s.Progress.Status())
}

// StallWatch judges one start by its reports: a report whose UpdatedAt
// changed is progress, and one whose UpdatedAt or AliveAt changed is a sign
// of life. It compares the fields for change and times their arrival on
// its own clock, so the reporter's clock never has to agree with it.
type StallWatch struct {
	window       time.Duration
	last         Progress
	reported     bool
	lastProgress time.Time
	lastAlive    time.Time
}

// NewStallWatch starts judging at now. A window of zero or less takes
// StallWindow.
func NewStallWatch(window time.Duration, now time.Time) *StallWatch {
	if window <= 0 {
		window = StallWindow
	}
	return &StallWatch{window: window, lastProgress: now, lastAlive: now}
}

// Window is the watch's window.
func (w *StallWatch) Window() time.Duration { return w.window }

// Report records p, received at now, and reports whether it was progress:
// the first report, or one whose UpdatedAt changed.
func (w *StallWatch) Report(p Progress, now time.Time) (progressed bool) {
	first := !w.reported
	progressed = first || p.UpdatedAt != w.last.UpdatedAt
	if progressed {
		w.lastProgress = now
	}
	if progressed || p.AliveAt != w.last.AliveAt {
		w.lastAlive = now
	}
	w.last, w.reported = p, true
	return progressed
}

// Last is the last report, if any.
func (w *StallWatch) Last() (Progress, bool) { return w.last, w.reported }

// Check is the stall at now, or nil. A stopped heartbeat takes precedence:
// it is the same failure and says more. Before the first report it is nil;
// what silence from the start means is the caller's to say.
func (w *StallWatch) Check(now time.Time) *Stall {
	if !w.reported {
		return nil
	}
	if quiet := now.Sub(w.lastAlive); quiet >= w.window {
		return &Stall{Progress: w.last, Quiet: quiet, Unresponsive: true}
	}
	if quiet := now.Sub(w.lastProgress); quiet >= w.window {
		return &Stall{Progress: w.last, Quiet: quiet}
	}
	return nil
}

// Silent reports whether no report arrived within the window of the start.
func (w *StallWatch) Silent(now time.Time) bool {
	return !w.reported && now.Sub(w.lastProgress) >= w.window
}

// Deadline is the earliest time Check or Silent can fail: the window after
// the last progress, which is never after the last sign of life.
func (w *StallWatch) Deadline() time.Time { return w.lastProgress.Add(w.window) }
