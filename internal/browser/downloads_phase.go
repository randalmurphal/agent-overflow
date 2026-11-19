package browser

import (
	"errors"
	"fmt"
)

// downloadRowPhase is where a listed download is in its life. A row is
// admitted running; the user's cancel moves it to cancel requested, which
// the engine's refusal or CancelDownload's failure moves back; a staged
// download's finish moves it to publishing while its file moves into the
// Downloads folder; and its end settles it, with State saying how. Only
// advance and settle write it.
type downloadRowPhase int

const (
	// rowRunning: the engine runs it and AO has asked nothing of it.
	rowRunning downloadRowPhase = iota
	// rowCancelRequested: the user's cancel is with the engine, whose end
	// report answers it.
	rowCancelRequested
	// rowPublishing: its finished file moves into the Downloads folder
	// (publishFinishedDownload), which settles it; it takes no report.
	rowPublishing
	// rowSettled: it ended, as State says.
	rowSettled
	// rowUserCanceled: it ended canceled while the user's cancel was with
	// the engine, which is not announced.
	rowUserCanceled
)

func (p downloadRowPhase) String() string {
	switch p {
	case rowRunning:
		return "running"
	case rowCancelRequested:
		return "cancel requested"
	case rowPublishing:
		return "publishing"
	case rowSettled:
		return "settled"
	case rowUserCanceled:
		return "canceled by the user"
	}
	return fmt.Sprintf("downloadRowPhase(%d)", int(p))
}

// errDownloadPhase is a transition the row's phase does not allow.
var errDownloadPhase = errors.New("browser: download row transition not allowed")

// ended reports a settled phase.
func (p downloadRowPhase) ended() bool {
	switch p {
	case rowRunning, rowCancelRequested, rowPublishing:
		return false
	case rowSettled, rowUserCanceled:
		return true
	}
	panic(fmt.Sprintf("browser: %v", p))
}

// takesReports is whether the engine's reports still apply to the row:
// running, or with the user's cancel pending.
func (e *DownloadInfo) takesReports() bool {
	return e.phase == rowRunning || e.phase == rowCancelRequested
}

// advance moves the row to the running phase to, and refuses with
// errDownloadPhase a move its phase does not allow: a cancel is requested
// only of a running row, withdrawn only while requested, and a publish
// begins only from a row that takes reports. A settle goes through
// settle.
func (e *DownloadInfo) advance(to downloadRowPhase) error {
	allowed := false
	switch to {
	case rowCancelRequested:
		allowed = e.phase == rowRunning
	case rowRunning:
		allowed = e.phase == rowCancelRequested
	case rowPublishing:
		allowed = e.takesReports()
	case rowSettled, rowUserCanceled:
	}
	if !allowed {
		return fmt.Errorf("%w: download %s from %v to %v", errDownloadPhase, e.ID, e.phase, to)
	}
	e.phase = to
	return nil
}

// settle ends the row as state, a terminal State, from any phase but a
// settled one. A cancel that ends a row whose cancel the user requested is
// that cancel (rowUserCanceled). Error is the caller's to set.
func (e *DownloadInfo) settle(state string) error {
	switch state {
	case downloadStateCompleted, downloadStateFailed, downloadStateCanceled:
	default:
		return fmt.Errorf("%w: download %s settled as %q", errDownloadPhase, e.ID, state)
	}
	if e.phase.ended() {
		return fmt.Errorf("%w: download %s settled again from %v", errDownloadPhase, e.ID, e.phase)
	}
	to := rowSettled
	if e.phase == rowCancelRequested && state == downloadStateCanceled {
		to = rowUserCanceled
	}
	e.phase, e.State = to, state
	return nil
}
