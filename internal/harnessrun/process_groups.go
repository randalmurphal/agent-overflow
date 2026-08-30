package harnessrun

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ProcessGroup is the small ownership seam used by the supervisor. Concrete
// launchers may wrap Unix process groups, Windows jobs, or a test double.
type ProcessGroup interface {
	Record() ProcessGroupRecord
	Terminate(context.Context) error
	Kill(context.Context) error
}

// ProcessGroups returns the identities registered for this run.
func (s *Supervisor) ProcessGroups() []ProcessGroupRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ProcessGroupRecord(nil), s.manifest.ProcessGroups...)
}

// StopProcessGroups is the immediate safety path. It is safe to call while a
// managed adapter is running and is idempotent for groups that already exited.
func (s *Supervisor) StopProcessGroups(ctx context.Context) error { return s.terminateGroups(ctx) }

// RegisterProcessGroup records a group before it is used. A group not marked
// owned is rejected because killing an unowned group is a cross-run safety
// violation.
func (s *Supervisor) RegisterProcessGroup(group ProcessGroup) error {
	if group == nil {
		return errors.New("process group is nil")
	}
	record := group.Record()
	if !record.Owned || strings.TrimSpace(record.ID) == "" {
		return errors.New("process group is not owned")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("run supervisor is closed")
	}
	for _, existing := range s.manifest.ProcessGroups {
		if existing.ID == record.ID {
			return fmt.Errorf("process group %q already registered", record.ID)
		}
	}
	s.manifest.ProcessGroups = append(s.manifest.ProcessGroups, record)
	if err := s.persistLocked(); err != nil {
		s.manifest.ProcessGroups = s.manifest.ProcessGroups[:len(s.manifest.ProcessGroups)-1]
		return err
	}
	s.groups = append(s.groups, group)
	return nil
}

func (s *Supervisor) terminateGroups(ctx context.Context) error {
	s.groupsMu.Lock()
	defer s.groupsMu.Unlock()
	s.mu.Lock()
	s.groupsStopping = true
	groups := append([]ProcessGroup(nil), s.groups...)
	s.mu.Unlock()
	var joined error
	for i := len(groups) - 1; i >= 0; i-- {
		if err := groups[i].Terminate(ctx); err != nil {
			joined = errors.Join(joined, err)
			if killErr := groups[i].Kill(ctx); killErr != nil {
				joined = errors.Join(joined, killErr)
			}
		}
	}
	return joined
}
