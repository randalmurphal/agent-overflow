package trackerapp

import (
	"database/sql"
	"errors"
	"fmt"

	"agent-overflow/internal/store"
)

// change runs one change to the connections under opMu, then tells
// Changed when the change says the list moved.
func (s *Service) change(op func() (bool, error)) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	s.opMu.Lock()
	moved, err := op()
	s.opMu.Unlock()
	if moved {
		s.changed()
	}
	return err
}

// Add checks and normalizes c, stores the connection and makes its empty
// key file, and returns it. A key file that cannot be made deletes the
// row again and fails the add. A duplicate fails with
// store.ErrTrackerConnectionExists, an input refused here with
// ErrInvalid and one the store refuses with
// store.ErrInvalidTrackerConnection.
func (s *Service) Add(c NewConnection) (Connection, error) {
	in, err := normalize(c)
	if err != nil {
		return Connection{}, err
	}
	var out Connection
	err = s.change(func() (bool, error) {
		in.CreatedAt = s.now().UnixMilli()
		row, err := s.store.CreateTrackerConnection(in)
		if err != nil {
			return false, fmt.Errorf("trackerapp: add a %s connection: %w", in.Kind, err)
		}
		if _, err := s.keyFile(row).Create(); err != nil {
			_, deleteErr := s.store.DeleteTrackerConnection(row.ID)
			if deleteErr != nil {
				deleteErr = fmt.Errorf("delete connection %s again: %w", row.ID, deleteErr)
			}
			return deleteErr != nil, errors.Join(fmt.Errorf("trackerapp: create the key file of %s: %w", row.ID, err), deleteErr)
		}
		out = s.view(row)
		return true, nil
	})
	return out, err
}

// Rename sets connection id's label, trimmed. An unchanged label tells
// nobody.
func (s *Service) Rename(id, label string) (Connection, error) {
	label, err := normalizeLabel(label)
	if err != nil {
		return Connection{}, err
	}
	return s.update(id, func() (store.TrackerConnection, bool, error) {
		return s.store.SetTrackerConnectionLabel(id, label)
	})
}

// SetTokenExpiry records when connection id's token expires, in unix
// milliseconds, as the person read it from the tracker; 0 clears it.
func (s *Service) SetTokenExpiry(id string, expiresAt int64) (Connection, error) {
	if expiresAt < 0 {
		return Connection{}, fmt.Errorf("%w: a token expiry is a time or 0", ErrInvalid)
	}
	return s.update(id, func() (store.TrackerConnection, bool, error) {
		return s.store.SetTrackerConnectionTokenExpiry(id, expiresAt)
	})
}

// SetEnabled turns connection id on or off. Off keeps the row and the key
// file and stops using the connection: its client is closed, ending its
// calls in flight, and its problem is forgotten. On builds the client
// again on next use.
func (s *Service) SetEnabled(id string, enabled bool) (Connection, error) {
	var out Connection
	err := s.change(func() (bool, error) {
		row, changed, err := s.store.SetTrackerConnectionEnabled(id, enabled)
		if err != nil {
			return false, s.writeError(id, "turn on or off", err)
		}
		if !row.Enabled && s.stopUsing(id) {
			changed = true
		}
		out = s.view(row)
		return changed, nil
	})
	return out, err
}

// update applies one row write to connection id and returns the row.
func (s *Service) update(id string, write func() (store.TrackerConnection, bool, error)) (Connection, error) {
	var out Connection
	err := s.change(func() (bool, error) {
		row, changed, err := write()
		if err != nil {
			return false, s.writeError(id, "change", err)
		}
		out = s.view(row)
		return changed, nil
	})
	return out, err
}

// writeError wraps a failed row write, a missing row as ErrNotFound.
func (s *Service) writeError(id, what string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w %s: %w", ErrNotFound, id, err)
	}
	return fmt.Errorf("trackerapp: %s connection %s: %w", what, id, err)
}

// Remove stops using connection id and the Tempo connection that belongs
// to it, removes their key files and then deletes their rows. A key file
// that cannot be removed stops the removal with the rows kept, so a step
// that fails leaves a row with a missing key file, never a key file with
// no row; removing again finishes it.
func (s *Service) Remove(id string) error {
	return s.change(func() (bool, error) {
		row, err := s.get(id)
		if err != nil {
			return false, err
		}
		removed := []store.TrackerConnection{row}
		if row.Kind == store.TrackerKindJira {
			child, ok, err := s.tempoOf(id)
			if err != nil {
				return false, err
			}
			if ok {
				removed = append(removed, child)
			}
		}
		for _, r := range removed {
			s.stopUsing(r.ID)
		}
		for _, r := range removed {
			if err := s.keyFile(r).Remove(); err != nil {
				return true, fmt.Errorf("trackerapp: remove the key file of connection %s: %w", r.ID, err)
			}
		}
		if _, err := s.store.DeleteTrackerConnection(id); err != nil {
			return true, fmt.Errorf("trackerapp: delete connection %s: %w", id, err)
		}
		return true, nil
	})
}

// PrepareKeyFile makes connection id's key file, empty and owner-only,
// when it is missing, for the person to paste the token into. A token
// already replaced there is read by the connection's client on its next
// call.
func (s *Service) PrepareKeyFile(id string) (Connection, error) {
	var out Connection
	err := s.change(func() (bool, error) {
		row, err := s.get(id)
		if err != nil {
			return false, err
		}
		created, err := s.keyFile(row).Create()
		if err != nil {
			return false, fmt.Errorf("trackerapp: create the key file of %s: %w", id, err)
		}
		out = s.look(row)
		return created, nil
	})
	return out, err
}

// TokenPage is the address where the person makes connection id's token:
// the Atlassian account's API token page, the Jira site a Tempo
// connection belongs to (Tempo documents steps there, not an address), or
// Linear's API key settings.
func (s *Service) TokenPage(id string) (string, error) {
	if err := s.enter(); err != nil {
		return "", err
	}
	defer s.leave()
	row, err := s.get(id)
	if err != nil {
		return "", err
	}
	parentSite := ""
	if row.Kind == store.TrackerKindTempo {
		parent, err := s.get(row.ParentID)
		if err != nil {
			return "", fmt.Errorf("trackerapp: the Jira connection of %s: %w", id, err)
		}
		parentSite = parent.Site
	}
	return tokenPage(row.Kind, parentSite)
}
