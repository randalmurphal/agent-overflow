package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Who a person said a forge account is in a connection's tracker
// (tracker_forge_pairings): the person's own setting, which credit reads
// before the name match (tracker_cache_credit_forge.go) and the match
// unit leaves alone (mirrorForgePeopleDueSQL).

// ErrInvalidForgePairing is matched by a pairing refused for its shape.
var ErrInvalidForgePairing = errors.New("store: invalid forge pairing")

// TrackerForgePairing is who the forge account Login on Host is in
// ConnectionID's tracker: Person, with the name it was picked by, or no
// person at all (Person empty: a bot, whose acts credit leaves out).
type TrackerForgePairing struct {
	ConnectionID, Host, Login, Person, PersonName string
}

// SetTrackerForgePairing stores p in place of the account's pairing and
// reports whether it changed who the account is: a new pairing, or one
// to another person. A new name for the same person is stored and is no
// change. A change advances the totals revision of boards, the boards of
// the connection's mirror whose credit reads the forge, in the same
// transaction (forgePairingMovedTx). A connection that is not stored
// fails with sql.ErrNoRows.
func (s *Store) SetTrackerForgePairing(p TrackerForgePairing, boards []string) (changed bool, err error) {
	if p.ConnectionID == "" || p.Host == "" || p.Login == "" || p.Person == "" && p.PersonName != "" {
		return false, fmt.Errorf("%w: account %q on %q of connection %q", ErrInvalidForgePairing, p.Login, p.Host, p.ConnectionID)
	}
	action := fmt.Sprintf("store: pair forge account %q on %q of connection %s", p.Login, p.Host, p.ConnectionID)
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	var was sql.NullString
	if err := tx.QueryRow(`SELECT (SELECT person FROM tracker_forge_pairings WHERE connection_id = c.id AND host = ?2 AND login = ?3)
		FROM tracker_connections c WHERE c.id = ?1`, p.ConnectionID, p.Host, p.Login).Scan(&was); err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	if _, err := tx.Exec(`INSERT INTO tracker_forge_pairings(connection_id, host, login, person, person_name) VALUES (?1, ?2, ?3, ?4, ?5)
		ON CONFLICT(connection_id, host, login) DO UPDATE SET person = excluded.person, person_name = excluded.person_name
		WHERE person IS NOT excluded.person OR person_name IS NOT excluded.person_name`, p.ConnectionID, p.Host, p.Login, p.Person, p.PersonName); err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	if changed = !was.Valid || was.String != p.Person; changed {
		if err := forgePairingMovedTx(tx, p.ConnectionID, boards); err != nil {
			return false, fmt.Errorf("%s: %w", action, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", action, err)
	}
	return changed, nil
}

// DeleteTrackerForgePairing removes the account's pairing and reports
// whether it had one, advancing the totals revision of boards with it as
// SetTrackerForgePairing does.
func (s *Store) DeleteTrackerForgePairing(connectionID, host, login string, boards []string) (deleted bool, err error) {
	action := fmt.Sprintf("store: unpair forge account %q on %q of connection %s", login, host, connectionID)
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	res, err := tx.Exec(`DELETE FROM tracker_forge_pairings WHERE connection_id = ? AND host = ? AND login = ?`, connectionID, host, login)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	if deleted = n > 0; deleted {
		if err := forgePairingMovedTx(tx, connectionID, boards); err != nil {
			return false, fmt.Errorf("%s: %w", action, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", action, err)
	}
	return deleted, nil
}

// forgePairingMovedTx advances the totals revision of boards on
// connectionID's mirror, none when it has no mirror: the credit their
// pace counts reads the pairing that changed (mirrorBoardsCreditSQL).
func forgePairingMovedTx(tx *sql.Tx, connectionID string, boards []string) error {
	if len(boards) == 0 {
		return nil
	}
	encoded, err := encodeMirrorIDs(boards)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE tracker_cache_boards SET totals_revision = totals_revision + 1
		WHERE root_id = (SELECT r.id FROM tracker_cache_roots r WHERE r.connection_id = ?1) AND board_id IN (SELECT value FROM json_each(?2))`, connectionID, encoded); err != nil {
		return fmt.Errorf("advance the totals of the boards whose credit reads the pairing: %w", err)
	}
	return nil
}

// ListTrackerForgePairings lists connectionID's pairings, by host and
// login.
func (s *Store) ListTrackerForgePairings(connectionID string) ([]TrackerForgePairing, error) {
	out, err := listTrackerForgePairings(s.reader(), connectionID)
	if err != nil {
		return nil, fmt.Errorf("store: list the forge pairings of connection %s: %w", connectionID, err)
	}
	return out, nil
}

func listTrackerForgePairings(q sqlQueryer, connectionID string) ([]TrackerForgePairing, error) {
	var out []TrackerForgePairing
	err := scanMirrorRows(q, `SELECT host, login, person, person_name FROM tracker_forge_pairings WHERE connection_id = ?1 ORDER BY host, login`,
		[]any{connectionID}, func(r rowScanner) error {
			p := TrackerForgePairing{ConnectionID: connectionID}
			err := r.Scan(&p.Host, &p.Login, &p.Person, &p.PersonName)
			out = append(out, p)
			return err
		})
	return out, err
}
