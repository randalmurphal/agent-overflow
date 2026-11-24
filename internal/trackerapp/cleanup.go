package trackerapp

import (
	"errors"
	"fmt"
)

// A mirror ends in two steps. Turning a connection off or removing it
// detaches the mirror's root under opMu, one statement: from then on no
// write naming the root lands, and the next tick makes a new root.
// Draining the detached root's rows is the cleanup, run after opMu is
// released, so no connection change or tick waits for it. A cleanup that
// fails leaves the root detached, refused to every run, and the next
// cleanup finishes it. The same cleanup removes the boards no project lists any
// more and the issues nothing keeps (no board row and no linked thread),
// which a thread's unlink, delete, retire or transfer leaves.

// FinishMirrorCleanup drains everything the mirror owes (store
// FinishMirrorCleanup): detached mirrors, retired boards and unkept
// issues. It fails with the store's error when one cannot be drained. It
// runs after a turn off or a removal (cleanupAfter), after each round of
// the linked loop, and once the first reads settle at the app's start, whose call
// is the only one that runs with no connection on. With nothing pending
// it is one statement. The store serializes calls.
// Stop ends a cleanup in flight before its next transaction, which then
// returns ErrStopped; the next cleanup finishes what it left.
func (s *Service) FinishMirrorCleanup() error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	return s.clean()
}

func (s *Service) clean() error {
	err := s.store.FinishMirrorCleanup(s.halting, nil)
	if err != nil && s.halting.Err() != nil {
		return errors.Join(ErrStopped, err)
	}
	if err != nil {
		return fmt.Errorf("trackerapp: finish the tracker mirror cleanup: %w", err)
	}
	return nil
}

// cleanupAfter runs the cleanup after what (a turn off, a removal) and
// logs its failure: what stands, and the next cleanup retries.
func (s *Service) cleanupAfter(what string) {
	if err := s.enter(); err != nil {
		return
	}
	defer s.leave()
	if err := s.clean(); err != nil && !errors.Is(err, ErrStopped) {
		s.logf("trackerapp: after %s: %v; the next cleanup retries", what, err)
	}
}

// detachMirror detaches connection id's mirror root, so no run that holds
// it stores anything more and the next tick makes a new one. The caller
// holds opMu.
func (s *Service) detachMirror(id string) error {
	if _, err := s.store.DetachMirror(id); err != nil {
		return fmt.Errorf("trackerapp: detach the mirror of connection %s: %w", id, err)
	}
	return nil
}
