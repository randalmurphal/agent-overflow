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
	s.forgetHolds()
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
		var exited []<-chan struct{}
		s.followConnectionsLocked(&exited)
		s.siteLinksChanged(row)
		if row.Enabled {
			s.timeSourceChanged(row)
		}
		return true, nil
	})
	return out, err
}

// timeSourceChanged has every board of the Jira connection whose time
// source row is (a Tempo connection added, turned on or off, or removed
// while on) read again: the change dropped the connection's worklogs and
// moved its boards' totals (migration v177), whose credit counts them.
// Any other connection tells nobody. The caller holds opMu.
func (s *Service) timeSourceChanged(row store.TrackerConnection) {
	if row.Kind != store.TrackerKindTempo || row.ParentID == "" {
		return
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for scope, p := range s.subs.boards {
		if scope.Connection != row.ParentID || p.over || p.core.DeadLocked() {
			continue
		}
		p.pending.add(nil, nil, true)
		s.flushBoardLocked(p)
	}
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
		return Connection{}, invalid("a token expiry is a time or 0")
	}
	return s.update(id, func() (store.TrackerConnection, bool, error) {
		return s.store.SetTrackerConnectionTokenExpiry(id, expiresAt)
	})
}

// SetEnabled turns connection id on or off. Off keeps the row and the key
// file and stops using the connection: its client is closed, ending its
// calls in flight, its problem is forgotten, its subscriptions end (their
// pumps have exited when SetEnabled returns, and each one's subscribers
// are told once) and its mirror is detached with opMu held, so no tick
// begun before the change can write into the mirror of a later turn on.
// The detached mirror is then drained with opMu released (cleanup.go); a
// drain that fails is logged and the change stands. A detach that fails returns the connection as turned off with
// an error: turning it off again retries. On builds the client again on
// next use, and the next tick makes a new mirror.
func (s *Service) SetEnabled(id string, enabled bool) (Connection, error) {
	if err := s.enter(); err != nil {
		return Connection{}, err
	}
	defer s.leave()
	out, changed, detached, exited, err := s.setEnabled(id, enabled)
	join(exited)
	if changed {
		s.changed()
	}
	if detached {
		s.cleanupAfter("turning off connection " + id)
	}
	return out, err
}

func (s *Service) setEnabled(id string, enabled bool) (_ Connection, changed, detached bool, exited []<-chan struct{}, _ error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	defer s.forgetHolds()
	row, changed, err := s.store.SetTrackerConnectionEnabled(id, enabled)
	if err != nil {
		return Connection{}, false, false, nil, s.writeError(id, "turn on or off", err)
	}
	if changed {
		// After the detach below: a link shows its copy current while its
		// root is attached.
		defer s.siteLinksChanged(row)
		s.timeSourceChanged(row)
	}
	if row.Enabled {
		s.followConnectionsLocked(&exited)
		return s.view(row), changed, false, exited, nil
	}
	if s.stopUsing(id) {
		changed = true
	}
	exited = s.endConnection(id)
	s.followConnectionsLocked(&exited)
	if err := s.detachMirror(id); err != nil {
		return s.view(row), changed, false, exited, fmt.Errorf("trackerapp: connection %s is turned off, but its mirror stays attached until it is turned off again: %w", id, err)
	}
	return s.view(row), changed, true, exited, nil
}

// endConnection ends every subscription of connection id and returns
// what to join (endConnectionLocked). The caller holds opMu.
func (s *Service) endConnection(id string) []<-chan struct{} {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	return s.endConnectionLocked(id)
}

// followConnectionsLocked starts or stops the linked loop after a change
// to the connections, adding a loop it stopped to exited. A failure to
// read the connections leaves the loop running (syncLoopLocked), and is
// the loop's failure: the change stands. The caller holds opMu.
func (s *Service) followConnectionsLocked(exited *[]<-chan struct{}) {
	done, err := s.syncLoopLocked()
	if err != nil {
		s.loopFailure(err)
	}
	if done != nil {
		*exited = append(*exited, done)
	}
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
// to it, ends its subscriptions (as SetEnabled does when it turns one
// off), removes their key files and then deletes their rows, which
// detaches the mirror; the mirror is drained after (cleanup.go), and a
// drain that fails is logged with the removal standing. A key file that
// cannot be removed stops the removal with the rows kept, so a step that
// fails leaves a row with a missing key file, never a key file with no
// row; removing again finishes it.
func (s *Service) Remove(id string) error {
	deleted := false
	var exited []<-chan struct{}
	defer func() { join(exited) }()
	err := s.change(func() (bool, error) {
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
			exited = append(exited, s.endConnection(r.ID)...)
		}
		for _, r := range removed {
			if err := s.keyFile(r).Remove(); err != nil {
				return true, fmt.Errorf("trackerapp: remove the key file of connection %s: %w", r.ID, err)
			}
		}
		// The store detaches the connection's mirror root with the row, so
		// a tick still running stores nothing more.
		if _, err := s.store.DeleteTrackerConnection(id); err != nil {
			return true, fmt.Errorf("trackerapp: delete connection %s: %w", id, err)
		}
		deleted = true
		s.followConnectionsLocked(&exited)
		s.siteLinksChanged(row)
		if row.Enabled {
			s.timeSourceChanged(row)
		}
		return true, nil
	})
	if deleted {
		s.cleanupAfter("removing connection " + id)
	}
	return err
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
