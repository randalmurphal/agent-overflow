package store

import (
	"database/sql"
	"errors"
	"testing"
)

// TestThreadRowWriteSeparatesNoOpFromMissingRow pins the distinction the whole
// broadcast scheme rests on. Three outcomes, and conflating any two of them
// either loses an error or puts a write that changed nothing on the wire:
//
//   - the write moved the row      → (row, true, nil)
//   - the value was already set    → (zero, false, nil)
//   - the row is not there at all  → (zero, false, sql.ErrNoRows)
func TestThreadRowWriteSeparatesNoOpFromMissingRow(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("trw-1", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	row, changed, err := s.ArchiveThread(thread.ID)
	if err != nil || !changed {
		t.Fatalf("ArchiveThread = (%+v, %v, %v), want a changed row", row, changed, err)
	}
	if row.ID != thread.ID || !row.Archived {
		t.Fatalf("ArchiveThread row = %+v, want archived %s", row, thread.ID)
	}

	row, changed, err = s.ArchiveThread(thread.ID)
	if err != nil {
		t.Fatalf("ArchiveThread(repeat) error = %v, want nil (a no-op is not an error)", err)
	}
	if changed {
		t.Fatalf("ArchiveThread(repeat) reported a change, row %+v", row)
	}

	if _, _, err := s.ArchiveThread("no-such-thread"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ArchiveThread(missing) error = %v, want sql.ErrNoRows", err)
	}
}

// TestThreadRowWriteKeepsAnIneligibleRowAnError is the Match-predicate half:
// an unpinned row is not a no-op for a pin-group move, it is a refusal, and
// the miss probe is what keeps the two apart.
func TestThreadRowWriteKeepsAnIneligibleRowAnError(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("trw-2", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	if _, _, err := s.SetThreadPinGroup(thread.ID, PinGroupBack); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SetThreadPinGroup(unpinned) error = %v, want sql.ErrNoRows", err)
	}

	if _, _, err := s.PinThread(thread.ID); err != nil {
		t.Fatalf("PinThread: %v", err)
	}
	row, changed, err := s.SetThreadPinGroup(thread.ID, PinGroupBack)
	if err != nil || !changed {
		t.Fatalf("SetThreadPinGroup = (%+v, %v, %v), want a changed row", row, changed, err)
	}
	if row.PinGroup == nil || *row.PinGroup != PinGroupBack {
		t.Fatalf("SetThreadPinGroup row PinGroup = %v, want %d", row.PinGroup, PinGroupBack)
	}
	if _, changed, err = s.SetThreadPinGroup(thread.ID, PinGroupBack); err != nil || changed {
		t.Fatalf("SetThreadPinGroup(repeat) = (%v, %v), want no change and no error", changed, err)
	}
}
