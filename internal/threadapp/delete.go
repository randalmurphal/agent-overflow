package threadapp

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

// DeletePorts is the explicit boundary from store-owned deletion ordering to
// root-owned live processes and filesystem resources. Each callback is one
// capability; there is no App-shaped host hidden behind the service.
type DeletePorts struct {
	// StopRemoteWork cancels commands the conversation still owns on other
	// computers. It runs before provider cleanup and again under the
	// mutation lock, so work admitted during cleanup is stopped too.
	StopRemoteWork          func(threadID string) error
	CleanProviderBackground func(store.Thread) error
	StopSession             func(threadID string) error
	CancelWorktreeSetup     func(threadID string)
	// CloseTerminals runs before the row drops and again after, for a
	// terminal a client opened meanwhile: OpenTerminal needs the row, so
	// none can open after the drop.
	CloseTerminals func(threadID string) error
	// CloseBrowserPages runs before the row drops, so a failure keeps the
	// thread for a retry, and again after, for a page a companion action
	// opened meanwhile (the action leaves it to the delete while the row
	// is present).
	CloseBrowserPages  func(threadID string) error
	ClearAutoReconnect func(threadID string)
	CleanupAttachments func(threadID string) error
	CleanupReplayLog   func(threadID string) error
	// Forget drops the conversation's in-memory settings after its row is
	// gone, so a delete that fails earlier keeps them.
	Forget func(threadID string)
	// Deleted fires once the row's delete mark committed
	// (store.BeginThreadDelete), as DeleteThread returns. From the mark on
	// the row is gone to every read, so a delete that fails after it still
	// reports the row, and the next boot finishes it
	// (store.ListPendingThreadDeletes). Root broadcasts it on
	// `thread:updated` so every attached client drops the row without a
	// refresh. spawned lists the live threads a standing agent spawned,
	// read before the drop took their spawn rows: their rows lost the
	// agent bond and go out again. Empty for any other thread.
	Deleted func(thread store.Thread, spawned []string)
	Logf    func(format string, args ...any)
	// ChunkPause, when set, runs between the bounded item-delete chunks
	// of the store delete so a background sweep yields the write lock
	// between transactions. nil keeps user-initiated deletes at full
	// speed.
	ChunkPause store.ChunkPause
}

// DeleteThread stops everything the thread owns and drops its row. Call it
// under the thread's action lock.
func (s *Service) DeleteThread(threadID string, ports DeletePorts) error {
	retired, err := s.CheckCleanup(threadID)
	if err != nil {
		return err
	}
	database, err := s.database("delete thread")
	if err != nil {
		return err
	}
	if ports.Logf == nil {
		ports.Logf = log.Printf
	}
	thread, threadErr := database.GetThread(threadID)
	if threadErr != nil && !errors.Is(threadErr, sql.ErrNoRows) {
		return fmt.Errorf("delete thread %s: lookup: %w", threadID, threadErr)
	}
	threadFound := threadErr == nil
	var spawned []string
	if threadFound && thread.AgentID != "" {
		if spawned, err = database.ListSpawnedThreadIDs(threadID); err != nil {
			return fmt.Errorf("delete thread %s: %w", threadID, err)
		}
	}

	if ports.StopRemoteWork != nil {
		if err := ports.StopRemoteWork(threadID); err != nil {
			return err
		}
	}
	var errs []error
	if threadFound && !retired && ports.CleanProviderBackground != nil {
		if err := ports.CleanProviderBackground(thread); err != nil {
			ports.Logf("delete thread %s: clean provider background terminals: %v", threadID, err)
		}
	}
	if ports.StopSession != nil {
		if err := ports.StopSession(threadID); err != nil {
			errs = append(errs, fmt.Errorf("stop session: %w", err))
		}
	}
	if ports.CancelWorktreeSetup != nil {
		ports.CancelWorktreeSetup(threadID)
	}
	if ports.CloseTerminals != nil {
		if err := ports.CloseTerminals(threadID); err != nil {
			errs = append(errs, fmt.Errorf("close terminals: %w", err))
		}
	}
	if ports.CloseBrowserPages != nil {
		if err := ports.CloseBrowserPages(threadID); err != nil {
			errs = append(errs, fmt.Errorf("close browser pages: %w", err))
		}
	}
	if ports.ClearAutoReconnect != nil {
		ports.ClearAutoReconnect(threadID)
	}
	// Network uploads stage outside the thread. Serialize only publication
	// and final deletion, after provider/process cleanup has finished.
	unlockMutation := s.mutations.Lock(threadID)
	defer unlockMutation()
	if ports.StopRemoteWork != nil {
		if err := ports.StopRemoteWork(threadID); err != nil {
			return err
		}
	}
	// The mark refuses new forks before the attachments go, so the ones
	// the thread's forks show are known and kept (store.ReleasableAttachments).
	if threadFound {
		err := database.BeginThreadDelete(threadID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("delete thread %s: mark deleting: %w", threadID, err)
		}
		// No client lists a holder: its source's delete reported the
		// thread gone, and a revert's holder was never shown.
		if err == nil && ports.Deleted != nil && thread.Mode != threadmode.ModeHolder {
			defer ports.Deleted(thread, spawned)
		}
	}
	if ports.CleanupAttachments != nil {
		if err := ports.CleanupAttachments(threadID); err != nil {
			errs = append(errs, fmt.Errorf("cleanup attachments: %w", err))
		}
	}
	if ports.CleanupReplayLog != nil {
		if err := ports.CleanupReplayLog(threadID); err != nil {
			errs = append(errs, fmt.Errorf("cleanup replay log: %w", err))
		}
	}
	if unbound, err := database.ClearWorkItemOriginThreads(threadID); err != nil {
		errs = append(errs, fmt.Errorf("clear workflow thread bindings: %w", err))
	} else if unbound > 0 {
		ports.Logf("delete thread %s: unbound %d workflow run(s); their results now surface in the workflows overlay", threadID, unbound)
	}
	if len(errs) > 0 {
		return fmt.Errorf("delete thread %s: %w", threadID, errors.Join(errs...))
	}
	if !threadFound {
		return nil
	}
	if err := database.DeleteThreadPaced(threadID, ports.ChunkPause); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("delete thread %s: drop row: %w", threadID, err)
	}
	var closeErrs []error
	if ports.CloseTerminals != nil {
		if err := ports.CloseTerminals(threadID); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("delete thread %s: close terminals opened during the delete: %w", threadID, err))
		}
	}
	if ports.CloseBrowserPages != nil {
		if err := ports.CloseBrowserPages(threadID); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("delete thread %s: close browser pages opened during the delete: %w", threadID, err))
		}
	}
	if ports.Forget != nil {
		ports.Forget(threadID)
	}
	return errors.Join(closeErrs...)
}
