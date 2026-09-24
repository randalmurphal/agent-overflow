package supervise

import (
	"errors"
	"fmt"
)

// PendingUpdateError is an update that is part-way through on this data root
// and is not the caller's to finish. Opening the database now would run a
// backend against a trial's work, or against a restore that has not
// happened.
type PendingUpdateError struct {
	Update UpdateRecord
	// Owner names the process that finishes it.
	Owner string
}

func (e *PendingUpdateError) Error() string {
	return fmt.Sprintf("the update from %s to %s (%s) has not finished; %s finishes or undoes it before the database may be opened",
		e.Update.From, e.Update.To, e.Update.ID, e.Owner)
}

// PrepareOptions tunes PrepareDataRoot.
type PrepareOptions struct {
	// OwnsServeLayout is set by the serve supervisor, which resumes its own
	// restores and runs its own trials.
	OwnsServeLayout bool
	// Progress receives the copy progress of a restore being finished.
	Progress CopyProgress
	// Log receives one line per restore finished. nil is silent.
	Log func(format string, args ...any)
}

// PrepareDataRoot runs after a process takes the data root's backend lock
// and before anything opens the database. It finishes a restore an
// interrupted update marked, in the serve layout and the in-app update
// layout, and refuses while an update the caller does not own is pending.
//
// A pending record in the in-app layout belongs to the helper that applies
// the update; one in the serve layout belongs to `agent-overflow supervise`.
// The Windows launcher keeps its record on the Windows side, so only its
// restore marker is visible here.
func PrepareDataRoot(dataDir string, opts PrepareOptions) error {
	appLayout, err := NewAppUpdateLayout(dataDir)
	if err != nil {
		return err
	}
	layouts := []struct {
		layout Layout
		owner  string
	}{{appLayout, "the Agent Overflow app"}}
	if !opts.OwnsServeLayout {
		serveLayout, err := NewLayout(dataDir)
		if err != nil {
			return err
		}
		layouts = append(layouts, struct {
			layout Layout
			owner  string
		}{serveLayout, "`agent-overflow supervise`"})
	}
	for _, entry := range layouts {
		marker, resumed, err := ResumeRestore(entry.layout, opts.Progress)
		if err != nil {
			return err
		}
		if resumed && opts.Log != nil {
			opts.Log("supervise: finished the interrupted restore of update %s in %s", marker.UpdateID, entry.layout.Root())
		}
		state, found, err := LoadState(entry.layout)
		if err != nil {
			return err
		}
		if found && state.Update != nil && state.Update.State == UpdatePending {
			return &PendingUpdateError{Update: *state.Update, Owner: entry.owner}
		}
	}
	return nil
}

// IsPendingUpdate reports whether err is a PendingUpdateError.
func IsPendingUpdate(err error) bool {
	var pending *PendingUpdateError
	return errors.As(err, &pending)
}
