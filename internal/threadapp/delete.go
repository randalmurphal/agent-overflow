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
	CleanProviderBackground func(store.Thread) error
	StopSession             func(threadID string) error
	CancelWorktreeSetup     func(threadID string)
	CloseTerminals          func(threadID string) error
	ClearSystemPrompt       func(threadID string)
	RemoveDiscussion        func(store.Thread)
	ClearAutoReconnect      func(threadID string)
	CleanupAttachments      func(threadID string) error
	CleanupReplayLog        func(threadID string) error
	// Deleted fires once per row actually dropped from SQLite, children
	// included, after the row is gone. Root broadcasts it on
	// `thread:updated` so a second attached client drops the same rows
	// without a refresh; a tree deletion is several rows, and the child
	// ids are only knowable here.
	Deleted func(store.Thread)
	Logf    func(format string, args ...any)
}

func (s *Service) DeleteTree(threadID string, subtreeLocksHeld bool, ports DeletePorts) error {
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

	var errs []error
	children, err := database.ListChildThreads(threadID)
	if err != nil {
		return fmt.Errorf("delete thread %s: list children: %w", threadID, err)
	}
	for _, child := range children {
		var unlockChild func()
		if !subtreeLocksHeld {
			unlockChild = s.Lock(child.ID)
		}
		err := s.DeleteTree(child.ID, subtreeLocksHeld, ports)
		if unlockChild != nil {
			unlockChild()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("delete child %s: %w", child.ID, err))
		}
	}

	if threadFound && ports.CleanProviderBackground != nil {
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
	if ports.ClearSystemPrompt != nil {
		ports.ClearSystemPrompt(threadID)
	}
	if threadFound && ports.RemoveDiscussion != nil {
		ports.RemoveDiscussion(thread)
	}
	if ports.ClearAutoReconnect != nil {
		ports.ClearAutoReconnect(threadID)
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
	if err := database.DeleteThread(threadID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("delete thread %s: drop row: %w", threadID, err)
	}
	if ports.Deleted != nil {
		ports.Deleted(thread)
	}
	return nil
}
