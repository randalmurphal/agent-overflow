package governor

import (
	"context"
	"fmt"
	"time"
)

// Monitor samples one lease owner's process tree and host available memory
// until ctx is canceled. It emits one event per threshold-crossing episode for
// each reason. A caller may choose how to surface the event, but the governor
// does not terminate the application.
// The owner birth identity is checked on every sample, so a reused PID cannot
// produce an event for the old run.
func (m *Manager) Monitor(ctx context.Context, lease Lease, interval time.Duration, memory ProcessMemoryReader, emit func(Event)) error {
	if lease.ID == "" || lease.OwnerPID <= 0 {
		return fmt.Errorf("harness governor: monitor requires a lease with an owner")
	}
	if interval <= 0 {
		return errorsForMonitor("interval must be positive")
	}
	if emit == nil {
		return errorsForMonitor("event callback is required")
	}
	if memory == nil {
		memory = defaultProcessMemory()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var floorTriggered, ceilingTriggered bool
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			state, err := m.processes.State(lease.OwnerPID)
			if err != nil {
				return fmt.Errorf("harness governor: monitor owner %d: %w", lease.OwnerPID, err)
			}
			if !state.Alive || state.BirthID == "" {
				return fmt.Errorf("harness governor: monitored owner %d is no longer live", lease.OwnerPID)
			}
			if state.BirthID != lease.OwnerBirthID {
				return fmt.Errorf("harness governor: monitored owner %d birth identity changed", lease.OwnerPID)
			}
			available, err := m.memory.AvailableMemory()
			if err != nil {
				return fmt.Errorf("harness governor: monitor host available memory: %w", err)
			}
			// Host pressure is independent of the owner's RSS. Handle it before
			// the process-tree query so a failing child sample cannot hide the
			// signal that protects every worktree on this host.
			if available < m.floor {
				if !floorTriggered {
					if err := m.emitAfterOwnerCheck(lease, ReasonAvailableFloor, 0, available, emit); err != nil {
						return err
					}
					floorTriggered = true
				}
				// Host pressure is already a run-stopping condition. Do not let
				// an unavailable child RSS sample mask it, and re-arm the RSS
				// crossing for the next pressure-free episode.
				ceilingTriggered = false
				continue
			} else {
				floorTriggered = false
			}

			rss, err := memory.RSS(lease.OwnerPID)
			if err != nil {
				return fmt.Errorf("harness governor: monitor process %d memory: %w", lease.OwnerPID, err)
			}
			// RSS is a separate OS query. Recheck the birth marker after it
			// so a PID that exits and is reused during the sample cannot emit
			// an event for the old lease.
			after, err := m.processes.State(lease.OwnerPID)
			if err != nil {
				return fmt.Errorf("harness governor: recheck owner %d: %w", lease.OwnerPID, err)
			}
			if !after.Alive || after.BirthID == "" || after.BirthID != lease.OwnerBirthID {
				return fmt.Errorf("harness governor: monitored owner %d changed during memory sample", lease.OwnerPID)
			}
			if rss > lease.CeilingBytes {
				if !ceilingTriggered {
					emit(Event{RunID: lease.RunID, Worktree: lease.Worktree, DataRoot: lease.DataRoot, Reason: ReasonSafetyCeiling, RSSBytes: rss, CeilingBytes: lease.CeilingBytes, AvailableBytes: available, AvailableFloorBytes: m.floor, At: m.clock()})
					ceilingTriggered = true
				}
			} else {
				ceilingTriggered = false
			}
		}
	}
}

func (m *Manager) emitAfterOwnerCheck(lease Lease, reason string, rss, available uint64, emit func(Event)) error {
	after, err := m.processes.State(lease.OwnerPID)
	if err != nil {
		return fmt.Errorf("harness governor: recheck owner %d: %w", lease.OwnerPID, err)
	}
	if !after.Alive || after.BirthID == "" || after.BirthID != lease.OwnerBirthID {
		return fmt.Errorf("harness governor: monitored owner %d changed during memory sample", lease.OwnerPID)
	}
	emit(Event{RunID: lease.RunID, Worktree: lease.Worktree, DataRoot: lease.DataRoot, Reason: reason, RSSBytes: rss, CeilingBytes: lease.CeilingBytes, AvailableBytes: available, AvailableFloorBytes: m.floor, At: m.clock()})
	return nil
}

func errorsForMonitor(message string) error {
	return fmt.Errorf("harness governor: monitor %s", message)
}
