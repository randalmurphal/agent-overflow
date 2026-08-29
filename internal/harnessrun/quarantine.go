package harnessrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func (s *Supervisor) quarantine() error {
	return s.quarantineContext(context.Background())
}

func (s *Supervisor) quarantineContext(ctx context.Context) error {
	s.mu.Lock()
	if s.manifest.Plan.Ownership != OwnershipFresh {
		s.closed = true
		s.mu.Unlock()
		return nil
	}
	qroot := s.root + QuarantineSuffix
	qpath := filepath.Join(qroot, s.manifest.Plan.RunID)
	s.mu.Unlock()
	if err := rejectSymlinkedParents(s.root); err != nil {
		return fmt.Errorf("quarantine root parent: %w", err)
	}
	if err := rejectSymlinkedParents(qroot); err != nil {
		return fmt.Errorf("quarantine parent: %w", err)
	}
	if st, err := os.Lstat(qroot); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return errors.New("quarantine parent is a symlink")
	}
	if st, err := os.Lstat(s.root); err != nil {
		return fmt.Errorf("quarantine root: %w", err)
	} else if st.Mode()&os.ModeSymlink != 0 {
		return errors.New("quarantine root is a symlink")
	}
	if err := s.verifyDestructiveRoot(); err != nil {
		return fmt.Errorf("refuse quarantine of fresh root: %w", err)
	}
	if err := os.MkdirAll(qroot, 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(qpath); err == nil {
		return errors.New("quarantine destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.mu.Lock()
	old := s.manifest
	s.manifest.State, s.manifest.Phase, s.manifest.Quarantine = StateQuarantined, PhaseFinalize, qpath
	if err := s.persistLocked(); err != nil {
		s.manifest = old
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	if err := s.verifyDestructiveRoot(); err != nil {
		return fmt.Errorf("refuse quarantine of fresh root: %w", err)
	}
	if err := os.Rename(s.root, qpath); err != nil {
		s.mu.Lock()
		s.manifest = old
		_ = s.persistLocked()
		s.mu.Unlock()
		return fmt.Errorf("quarantine failed root: %w", err)
	}
	s.mu.Lock()
	if s.lease != nil {
		s.lease.rehome(qpath)
	}
	retention := s.retention
	manifest := s.manifest
	s.mu.Unlock()
	if retention != nil {
		if err := retention.RegisterQuarantineContext(ctx, manifest, qpath); err != nil {
			return fmt.Errorf("register quarantined run: %w", err)
		}
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}
