package threadapp

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"agent-overflow/internal/store"
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
	// CancelShellCommand stops a composer shell command still running in the
	// thread's workspace; its result is discarded.
	CancelShellCommand func(threadID string)
	// CloseTerminals runs in the teardown and again after the drop, for a
	// terminal a client opened meanwhile: OpenTerminal needs the row, so
	// none can open after the drop.
	CloseTerminals func(threadID string) error
	// CloseBrowserPages runs in the teardown, so a failure keeps the row
	// for the next attempt, and again after the drop, for a page a
	// companion action opened meanwhile (the action leaves it to the delete
	// while the row is present).
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
	// home it may not remove, and otherwise returns the removal, which runs
	// in the teardown. nil remove for any other thread.
	AssistantHome func(threadID string) (remove func() error, err error)
	// Dropped fires once the row is dropped. spawned lists the live
	// threads the thread spawned on this computer that outlive it, read
	// before the drop took their spawn rows: each lost its spawner, and an
	// owned one is standalone now (trg_thread_placement_spawn_gone) and
	// joins the sidebar, so they go out again.
	Dropped func(thread store.Thread, spawned []string)
	Logf    func(format string, args ...any)
	// ChunkPause, when set, runs between the bounded item-delete chunks
	// of the store delete so a background sweep yields the write lock
	// between transactions. nil keeps user-initiated deletes at full
	// speed.
	ChunkPause store.ChunkPause
}

// A thread a family delete committed (store.CommitDeleteClaim) is destroyed
// in two halves, so the worktree it was the last thread on is removed while
// its row still names it: TearDown stops everything it runs and removes what
// it owns outside its row, and Drop drops the row
// (docs/specs/standing-agents.md, "Owned workers"). The threads under it are
// the caller's, destroyed first.

// TearDown stops everything the committed thread runs and removes what it
// owns outside its row; the row stays for Drop. An assistant's current
// thread is refused, with nothing touched, while a past thread or helper
// still lives under it (store.AssistantDependentsError): its home and
// history go only once they are gone. Call it under the thread's action
// lock.
func (s *Service) TearDown(threadID string, ports DeletePorts) error {
	retired, err := s.CheckDelete(threadID, true)
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
	if threadFound {
		if err := database.CheckNoAssistantDependents(threadID); err != nil {
			return fmt.Errorf("delete thread %s: %w", threadID, err)
		}
	}
	var removeHome func() error
	if ports.AssistantHome != nil {
		if removeHome, err = ports.AssistantHome(threadID); err != nil {
			return err
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
	if ports.CancelShellCommand != nil {
		ports.CancelShellCommand(threadID)
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
	// A command is registered under this lock, so one that started during
	// the cleanup before it is stopped here and none starts after.
	if ports.CancelShellCommand != nil {
		ports.CancelShellCommand(threadID)
	}
	if ports.StopRemoteWork != nil {
		if err := ports.StopRemoteWork(threadID); err != nil {
			return err
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
	return nil
}

// Drop drops the row of a thread TearDown finished, with its history
// (store.DeleteThreadPaced), and closes what a client opened for it since.
// A row already gone is nothing to do. Call it under the thread's action
// lock.
func (s *Service) Drop(threadID string, ports DeletePorts) error {
	database, err := s.database("delete thread")
	if err != nil {
		return err
	}
	thread, err := database.GetThread(threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete thread %s: lookup: %w", threadID, err)
	}
	spawned, err := database.ListSpawnedThreadIDs(threadID)
	if err != nil {
		return fmt.Errorf("delete thread %s: %w", threadID, err)
	}
	err = func() error {
		// Deferred, so a pause that never returns, as a crash, still
		// releases it.
		defer s.mutations.Lock(threadID)()
		return database.DeleteThreadPaced(threadID, ports.ChunkPause)
	}()
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("delete thread %s: drop row: %w", threadID, err)
	}
	if ports.Dropped != nil {
		ports.Dropped(thread, spawned)
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
