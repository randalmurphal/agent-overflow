package store

import "errors"

var ErrProjectReceivingTransfer = errors.New("This project is receiving a conversation. Finish or cancel its transfer before deleting the project.")

// CheckProjectTransferAccess is the early user-facing refusal. DeleteProject's
// conditional write repeats the predicate atomically with offer reservation.
func (s *Store) CheckProjectTransferAccess(projectID string) error {
	var reserved bool
	if err := s.reader().QueryRow(`SELECT EXISTS(SELECT 1 FROM thread_transfers WHERE project_id = ?
AND direction = 'incoming' AND phase NOT IN ('complete','canceled'))`, projectID).Scan(&reserved); err != nil {
		return err
	}
	if reserved {
		return ErrProjectReceivingTransfer
	}
	return nil
}
