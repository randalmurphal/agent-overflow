package governor

import (
	"context"
	"fmt"
	"time"
)

// Monitor samples one lease owner's process tree until ctx is canceled. It
// emits an event for every sample over the ceiling. A caller may choose how
// to surface the event, but the governor does not terminate the application.
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
			rss, err := memory.RSS(lease.OwnerPID)
			if err != nil {
				return fmt.Errorf("harness governor: monitor process %d memory: %w", lease.OwnerPID, err)
			}
			if rss > lease.CeilingBytes {
				emit(Event{RunID: lease.RunID, Worktree: lease.Worktree, DataRoot: lease.DataRoot, Reason: ReasonSafetyCeiling, RSSBytes: rss, CeilingBytes: lease.CeilingBytes, At: m.clock()})
			}
		}
	}
}

func errorsForMonitor(message string) error {
	return fmt.Errorf("harness governor: monitor %s", message)
}
