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
	// CleanupMemoryRecords deletes the records of what the thread's
	// sessions were shown as memory.
	CleanupMemoryRecords func(threadID string) error
	// Forget drops the conversation's in-memory settings after its row is
	// gone, so a delete that fails earlier keeps them.
	Forget func(threadID string)
	// AssistantHome runs first, for a thread that is an assistant's current
	// thread: deleting it deletes the assistant and its home. It refuses a
	// home it may not remove, so nothing is deleted, and otherwise returns
	// the removal, which runs once the delete is marked. nil remove for any
	// other thread.
	AssistantHome func(threadID string) (remove func() error, err error)
	// AssistantDependents deletes, for an assistant's current thread, the
	// threads that live under the assistant: its past threads and helpers,
	// each through its own delete. It runs once the delete has passed its
	// refusals and stopped the assistant's session, before the mark, so a
	// failure leaves the assistant whole for a retry. The mark is refused
	// while any remain (store.AssistantDependentsError), so one made
	// meanwhile sends the delete back here; once marked, none can be made.
	AssistantDependents func(threadID string) error
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

// maxDependentPasses bounds how often an assistant's delete deletes the
// threads under it before giving up: each pass stops the sessions that
// could make another, so a second one is a thread made during the first.
const maxDependentPasses = 3

// markDeleting takes the thread's mutation lock, stops the remote work
// admitted during cleanup and marks a found row deleting; marked is false
// when there is no row. An assistant's current thread is marked only once
// nothing lives under the assistant: its dependents are deleted first,
// with the lock released, and again when one made meanwhile refuses the
// mark. On success the caller releases the returned lock.
func (s *Service) markDeleting(database *store.Store, threadID string, found, assistant bool, ports DeletePorts) (unlock func(), marked bool, err error) {
	for pass := 1; ; pass++ {
		if assistant && ports.AssistantDependents != nil {
			if err := ports.AssistantDependents(threadID); err != nil {
				return nil, false, err
			}
		}
		// Network uploads stage outside the thread. Serialize only
		// publication and final deletion, after provider/process cleanup
		// has finished.
		unlock := s.mutations.Lock(threadID)
		if ports.StopRemoteWork != nil {
			if err := ports.StopRemoteWork(threadID); err != nil {
				unlock()
				return nil, false, err
			}
		}
		if !found {
			return unlock, false, nil
		}
		// The mark refuses new forks before the attachments go, so the
		// ones the thread's forks show are known and kept
		// (store.ReleasableAttachments).
		err := database.BeginThreadDelete(threadID)
		var dependents *store.AssistantDependentsError
		switch {
		case err == nil:
			return unlock, true, nil
		case errors.Is(err, sql.ErrNoRows):
			return unlock, false, nil
		case errors.As(err, &dependents) && assistant && pass < maxDependentPasses:
			unlock()
		default:
			unlock()
			return nil, false, fmt.Errorf("delete thread %s: mark deleting: %w", threadID, err)
		}
	}
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
	var removeHome func() error
	if ports.AssistantHome != nil {
		if removeHome, err = ports.AssistantHome(threadID); err != nil {
			return err
		}
	}
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
	unlockMutation, marked, err := s.markDeleting(database, threadID, threadFound, threadFound && thread.AgentID != "", ports)
	if err != nil {
		return err
	}
	defer unlockMutation()
	// No client lists a holder: its source's delete reported the thread
	// gone, and a revert's holder was never shown.
	if marked && ports.Deleted != nil && thread.Mode != threadmode.ModeHolder {
		defer ports.Deleted(thread, spawned)
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
	if ports.CleanupMemoryRecords != nil {
		if err := ports.CleanupMemoryRecords(threadID); err != nil {
			errs = append(errs, fmt.Errorf("cleanup memory records: %w", err))
		}
	}
	// Before the row drops, so a failure keeps the record and the next
	// attempt, or the boot that finishes the delete, removes the home.
	if removeHome != nil {
		if err := removeHome(); err != nil {
			errs = append(errs, err)
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
