package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// rowWrite is one single-row UPDATE that reports whether it actually moved
// anything and hands back the row it wrote.
//
// Every persisted thread row and project row is broadcast (`thread:updated`,
// `project:updated`) so a second attached client converges without a refresh,
// and a write that changed nothing broadcasts nothing. That makes "did this
// write change the row?" a value the store owes its caller rather than
// something the App layer can infer from a rows-affected count: SQLite counts
// a row as affected when the assignment restates the value it already held.
//
//   - Set is the assignment list ("archived = 1, updated_at = ?").
//   - Match is the eligibility predicate the write shares with its miss
//     probe ("pinned_at IS NOT NULL"). Empty means the id alone qualifies a
//     row, and a miss then means the row does not exist.
//   - Change excludes rows the assignment would leave untouched
//     ("archived IS NOT 1"). Empty means the assignment always moves the row
//     it matches, which is PinThread restamping pinned_at on every call.
//
// `IS NOT` rather than `<>` throughout: these columns are nullable and
// `NULL <> 0` is NULL, which SQLite reads as false. That is the same
// null-safe comparison UpdateBranchForWorkspace's branch predicate relies on.
//
// The table is not a field callers fill in. Each table has one entry point
// (applyThreadRowWrite, applyProjectRowWrite) that names its own table AND
// its own read-back projection, because those two always have to agree.
type rowWrite struct {
	Action     string
	table      string
	ID         string
	Set        string
	SetArgs    []any
	Match      string
	MatchArgs  []any
	Change     string
	ChangeArgs []any
}

// applyThreadRowWrite runs a thread-row write and reads the changed row back
// through the full threadColumns projection.
func (s *Store) applyThreadRowWrite(write rowWrite) (Thread, bool, error) {
	write.table = "threads"
	return applyRowWrite(s, write, func(tx *sql.Tx, id string) (Thread, error) {
		rows, err := listThreadsByIDTx(tx, []string{id})
		if err != nil {
			return Thread{}, err
		}
		if len(rows) != 1 {
			return Thread{}, fmt.Errorf("read back %d rows, want 1", len(rows))
		}
		return rows[0], nil
	})
}

// applyProjectRowWrite runs a project-row write and reads the changed row back
// through the projectColumns projection.
func (s *Store) applyProjectRowWrite(write rowWrite) (Project, bool, error) {
	write.table = "projects"
	return applyRowWrite(s, write, func(tx *sql.Tx, id string) (Project, error) {
		return scanProject(tx.QueryRow(`SELECT `+projectColumns+` FROM projects WHERE id = ?`, id))
	})
}

// applyRowWrite runs the write and reads back exactly the row it changed,
// inside one transaction — the shape UpdateBranchForWorkspace established.
// `RETURNING id` anchors the read on the write, so neither a concurrent writer
// nor a row the Change predicate excluded can widen the answer, and the
// read-back projection is paid only when something actually moved. RETURNING
// cannot carry that projection itself: threadColumns has two correlated
// subqueries per row, and SQLite forbids subqueries in a RETURNING clause.
//
// The bool is "this write changed the row". A write the Change predicate
// excluded reports (zero row, false, nil): the value was already what the
// caller asked for, which is a normal outcome and not an error. A write whose
// Match predicate excluded the row keeps reporting sql.ErrNoRows, the answer
// requireRowsAffected gave before, so a missing id and an ineligible row stay
// distinguishable from a no-op. The second probe runs only on the miss path.
func applyRowWrite[T any](s *Store, write rowWrite, readBack func(*sql.Tx, string) (T, error)) (T, bool, error) {
	var zero T
	conditions := []string{"id = ?"}
	args := append([]any{}, write.SetArgs...)
	args = append(args, write.ID)
	if write.Match != "" {
		conditions = append(conditions, write.Match)
		args = append(args, write.MatchArgs...)
	}
	eligible := strings.Join(conditions, " AND ")
	eligibleArgs := append([]any{}, args[len(write.SetArgs):]...)
	if write.Change != "" {
		conditions = append(conditions, write.Change)
		args = append(args, write.ChangeArgs...)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return zero, false, fmt.Errorf("%s: begin: %w", write.Action, err)
	}
	defer tx.Rollback()

	var id string
	err = tx.QueryRow(
		`UPDATE `+write.table+` SET `+write.Set+` WHERE `+strings.Join(conditions, " AND ")+` RETURNING id`,
		args...,
	).Scan(&id)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		var present int
		probeErr := tx.QueryRow(
			`SELECT 1 FROM `+write.table+` WHERE `+eligible, eligibleArgs...,
		).Scan(&present)
		if probeErr != nil {
			if errors.Is(probeErr, sql.ErrNoRows) {
				return zero, false, fmt.Errorf("%s: %w", write.Action, sql.ErrNoRows)
			}
			return zero, false, fmt.Errorf("%s: probe row: %w", write.Action, probeErr)
		}
		if err := tx.Commit(); err != nil {
			return zero, false, fmt.Errorf("%s: commit no-op: %w", write.Action, err)
		}
		return zero, false, nil
	default:
		return zero, false, fmt.Errorf("%s: %w", write.Action, err)
	}

	row, err := readBack(tx, id)
	if err != nil {
		return zero, false, fmt.Errorf("%s: read back: %w", write.Action, err)
	}
	if err := tx.Commit(); err != nil {
		return zero, false, fmt.Errorf("%s: commit: %w", write.Action, err)
	}
	return row, true, nil
}
