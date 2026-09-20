package store

import (
	"database/sql"
	"fmt"
)

// One thread's whole organizing change, written in one transaction.
//
// The sidebar changes a title, a group, a pin and the archive flag one
// gesture at a time, and each gesture has its own accessor. The agent
// thread tools send all four at once under a stricter rule: a thread is
// either fully updated or untouched. Four accessors cannot keep that rule:
// a refusal on the third leaves the first two committed. So the four
// writes share one transaction here, and every refusal rolls the rest back.
//
// The policy that decides WHICH of the four to write, and which patches are
// refused before any of them runs, belongs to the caller
// (threadapp.ApplyOrganizePatch). This is the atomic write it plans.

// ThreadGroupCreate names the group the organize write moves the thread
// into by NAME: the write resolves it inside its own transaction and
// creates it only when that project has none of that name.
type ThreadGroupCreate struct {
	ProjectID string
	Name      string
}

// ThreadPinWrite is one pin tier in the store's own terms: pinned or not,
// and on which burner. An unpinned row carries no burner.
type ThreadPinWrite struct {
	Pinned bool
	Burner int
}

// ThreadOrganizeWrite is what ApplyThreadOrganize writes. A nil or zero
// field is a field this call leaves alone.
type ThreadOrganizeWrite struct {
	// Title replaces the display name and its search index entry.
	Title *string
	// CreateGroup is the destination named by project and name, resolved
	// or inserted before the move. MoveGroup then moves the thread into it,
	// and ThreadOrganizeResult.CreatedGroup is set only when this write
	// inserted the row.
	CreateGroup *ThreadGroupCreate
	// MoveGroup writes group_id; GroupID is the destination, empty to
	// ungroup. It is ignored when CreateGroup minted the destination.
	MoveGroup bool
	GroupID   string
	// Pin is the tier to write.
	Pin *ThreadPinWrite
	// Archived flips the archive flag.
	Archived *bool
}

// ThreadOrganizeResult is what one organize transaction committed, read
// back inside it.
type ThreadOrganizeResult struct {
	// Thread is the row as it now stands, changed or not.
	Thread Thread
	// Changed is false when every field already held the written value.
	Changed bool
	// ArchivedChanged says the archive flag itself moved.
	ArchivedChanged bool
	// Carried are the OTHER rows the group move wrote: the discussion
	// children that travel with their root.
	Carried []Thread
	// CreatedGroup is the group this write inserted, if any. A destination
	// that already existed is not announced: every client already has it.
	CreatedGroup *ThreadGroup
}

// ApplyThreadOrganize writes a title, a group move, a pin tier and the
// archive flag as one change.
//
// Order is load-bearing rather than incidental: grouping strips a thread's
// own pin and pinning refuses a grouped row, so the group move runs before
// the pin. Any refusal (a group that is gone, a pin the move just made
// impossible, a thread handed to another computer) rolls back everything,
// including a group this call created.
func (s *Store) ApplyThreadOrganize(threadID string, write ThreadOrganizeWrite) (ThreadOrganizeResult, error) {
	if threadID == "" {
		return ThreadOrganizeResult{}, fmt.Errorf("store: organize thread: thread id is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ThreadOrganizeResult{}, fmt.Errorf("store: begin organize thread %s: %w", threadID, err)
	}
	defer tx.Rollback()

	before, err := getThreadTx(tx, threadID)
	if err != nil {
		return ThreadOrganizeResult{}, fmt.Errorf("store: organize thread %s: %w", threadID, err)
	}
	result := ThreadOrganizeResult{Thread: before}

	if write.Title != nil && *write.Title != before.Title {
		if err := updateTitleTx(tx, threadID, *write.Title); err != nil {
			return ThreadOrganizeResult{}, err
		}
		result.Changed = true
	}
	groupID := write.GroupID
	if write.CreateGroup != nil {
		// Resolved inside this transaction, not from the caller's plan: two
		// calls naming the same new group are two plans that both read "no
		// such group", and the second must join the first's group rather
		// than insert a second row of the same name.
		group, created, err := ensureThreadGroup(tx, write.CreateGroup.ProjectID, write.CreateGroup.Name)
		if err != nil {
			return ThreadOrganizeResult{}, err
		}
		if created {
			result.CreatedGroup = &group
		}
		groupID = group.ID
	}
	if write.MoveGroup {
		moved, err := setThreadGroupTx(tx, []string{threadID}, groupID)
		if err != nil {
			return ThreadOrganizeResult{}, err
		}
		for _, row := range moved {
			if row.ID != threadID {
				result.Carried = append(result.Carried, row)
			}
		}
		if groupID != before.GroupID {
			result.Changed = true
		}
	}
	if write.Pin != nil {
		changed, err := applyThreadPinTx(tx, threadID, *write.Pin)
		if err != nil {
			return ThreadOrganizeResult{}, err
		}
		result.Changed = result.Changed || changed
	}
	if write.Archived != nil {
		_, changed, err := applyThreadRowWriteTx(tx, threadArchivedWrite(threadID, *write.Archived))
		if err != nil {
			return ThreadOrganizeResult{}, err
		}
		result.Changed = result.Changed || changed
		result.ArchivedChanged = changed
	}

	if result.Changed {
		current, err := getThreadTx(tx, threadID)
		if err != nil {
			return ThreadOrganizeResult{}, fmt.Errorf("store: read back organized thread %s: %w", threadID, err)
		}
		result.Thread = current
	}
	if err := tx.Commit(); err != nil {
		return ThreadOrganizeResult{}, fmt.Errorf("store: commit organize thread %s: %w", threadID, err)
	}
	return result, nil
}

// applyThreadPinTx writes one pin tier, taking the same two steps the
// sidebar takes for the back burner: a row is pinned first and moved
// between burners second, because the store refuses a burner on an unpinned
// row. The front burner restamps pinned_at exactly as PinThread does; the
// back burner leaves an already-pinned row's stamp alone.
func applyThreadPinTx(tx *sql.Tx, threadID string, pin ThreadPinWrite) (bool, error) {
	if !pin.Pinned {
		_, changed, err := setThreadPinnedAtTx(tx, threadID, nil)
		return changed, err
	}
	if pin.Burner != PinGroupFront && pin.Burner != PinGroupBack {
		return false, fmt.Errorf("%w: %d", ErrInvalidPinGroup, pin.Burner)
	}
	if pin.Burner == PinGroupFront {
		now := nowMillis()
		_, changed, err := setThreadPinnedAtTx(tx, threadID, &now)
		return changed, err
	}

	// Read inside the transaction rather than from the caller's snapshot: a
	// group move in this same patch has already stripped whatever pin the
	// row carried when the caller read it.
	pinned, err := threadIsPinnedTx(tx, threadID)
	if err != nil {
		return false, err
	}
	changed := false
	if !pinned {
		now := nowMillis()
		_, pinChanged, err := setThreadPinnedAtTx(tx, threadID, &now)
		if err != nil {
			return false, err
		}
		changed = pinChanged
	}
	_, burnerChanged, err := setThreadPinGroupTx(tx, threadID, PinGroupBack)
	if err != nil {
		return false, err
	}
	return changed || burnerChanged, nil
}

func threadIsPinnedTx(tx *sql.Tx, threadID string) (bool, error) {
	var pinned bool
	if err := tx.QueryRow(
		`SELECT pinned_at IS NOT NULL FROM threads WHERE id = ?`, threadID,
	).Scan(&pinned); err != nil {
		return false, fmt.Errorf("store: read pin state for %s: %w", threadID, err)
	}
	return pinned, nil
}

// getThreadTx reads one row through the full projection inside the caller's
// transaction, so a multi-write change describes the state it wrote.
func getThreadTx(tx *sql.Tx, threadID string) (Thread, error) {
	rows, err := listThreadsByIDTx(tx, []string{threadID})
	if err != nil {
		return Thread{}, err
	}
	if len(rows) != 1 {
		return Thread{}, sql.ErrNoRows
	}
	return rows[0], nil
}
