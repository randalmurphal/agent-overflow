package store

import (
	"errors"
	"fmt"
)

var ErrForkPreparing = errors.New("This fork is still being prepared.")

func (s *Store) FinishForkPreparation(threadID string) (Thread, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Thread{}, fmt.Errorf("store: begin fork publication: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE threads SET fork_preparing = 0 WHERE id = ? AND fork_preparing = 1`, threadID)
	if err != nil {
		return Thread{}, fmt.Errorf("store: finish fork preparation: %w", err)
	}
	if err := requireRowsAffected(result, "store: finish fork preparation"); err != nil {
		return Thread{}, err
	}
	row, err := scanThread(tx.QueryRow(`SELECT `+threadColumns+` FROM threads WHERE id = ?`, threadID))
	if err != nil {
		return Thread{}, fmt.Errorf("store: read ready fork: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Thread{}, fmt.Errorf("store: commit fork publication: %w", err)
	}
	return row, nil
}

// CheckForkReady gates public history loads independently of client state.
// Fork creation reads the store directly while holding the fork action lock.
func (s *Store) CheckForkReady(threadID string) error {
	var preparing bool
	err := s.reader().QueryRow(`SELECT EXISTS(SELECT 1 FROM threads WHERE id = ? AND fork_preparing = 1)`, threadID).Scan(&preparing)
	if err != nil {
		return fmt.Errorf("store: check fork preparation: %w", err)
	}
	if preparing {
		return ErrForkPreparing
	}
	return nil
}

func (s *Store) ListPreparingForks() ([]string, error) {
	rows, err := s.reader().Query(`SELECT id FROM threads WHERE fork_preparing = 1`)
	if err != nil {
		return nil, fmt.Errorf("store: list preparing forks: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
