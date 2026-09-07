package store

import (
	"agent-overflow/internal/owndevices"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
)

type ownQuery interface {
	Query(string, ...any) (*sql.Rows, error)
}

func readOwnDevices(q ownQuery) ([]owndevices.Member, error) {
	rows, e := q.Query(`SELECT member FROM own_devices ORDER BY key_thumbprint`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []owndevices.Member{}
	for rows.Next() {
		var raw string
		var m owndevices.Member
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(raw), &m); e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) OwnDevices() ([]owndevices.Member, error) { return readOwnDevices(s.reader()) }
func (s *Store) OwnDevice(key string) (owndevices.Member, error) {
	var raw string
	var m owndevices.Member
	e := s.reader().QueryRow(`SELECT member FROM own_devices WHERE key_thumbprint=?`, key).Scan(&raw)
	if e != nil {
		return m, e
	}
	e = json.Unmarshal([]byte(raw), &m)
	return m, e
}
func (s *Store) OwnDeviceForSession(id string) (owndevices.Member, error) {
	var raw string
	var m owndevices.Member
	e := s.reader().QueryRow(`SELECT d.member FROM own_device_sessions o JOIN own_devices d ON d.key_thumbprint=o.key_thumbprint AND d.generation=o.generation WHERE o.session_id=? AND d.removed=0`, id).Scan(&raw)
	if e != nil {
		return m, e
	}
	e = json.Unmarshal([]byte(raw), &m)
	return m, e
}
func writeOwnDevice(tx *sql.Tx, m owndevices.Member) error {
	if err := owndevices.Validate(m); err != nil {
		return err
	}
	raw, e := json.Marshal(m)
	if e != nil {
		return e
	}
	_, e = tx.Exec(`INSERT INTO own_devices(key_thumbprint,member,generation,removed) VALUES(?,?,?,?) ON CONFLICT(key_thumbprint) DO UPDATE SET member=excluded.member,generation=excluded.generation,removed=excluded.removed`, m.KeyThumbprint, string(raw), m.Generation, m.Removed)
	return e
}

// MergeOwnDevices commits membership and revokes obsolete group sessions together.
// Returned IDs must be invalidated in the identity cache after the transaction.
func (s *Store) MergeOwnDevices(in []owndevices.Member, at int64, backendID string) (bool, []string, error) {
	tx, e := s.db.Begin()
	if e != nil {
		return false, nil, e
	}
	defer tx.Rollback()
	old, e := readOwnDevices(tx)
	if e != nil {
		return false, nil, e
	}
	next, changed, e := owndevices.Merge(old, in)
	if e != nil || !changed {
		return false, nil, e
	}
	for _, m := range next {
		if e = writeOwnDevice(tx, m); e != nil {
			return false, nil, e
		}
	}
	// Withdrawing this serving host withdraws every personal inbound grant.
	// Ordinary shares are not group-derived and remain independent.
	hostingRemoved := false
	if backendID != "" {
		removed, active := false, false
		for _, m := range next {
			if m.BackendID == backendID {
				if m.Removed {
					removed = true
				} else {
					active = true
				}
			}
		}
		hostingRemoved = removed && !active
	}
	if hostingRemoved {
		if _, e = tx.Exec(`UPDATE pairing_links SET canceled_at=? WHERE purpose='own-device' AND confirmed_at IS NULL AND canceled_at IS NULL`, at); e != nil {
			return false, nil, e
		}
	}
	_, e = tx.Exec(`UPDATE pairing_links SET canceled_at=? WHERE purpose='own-introduction' AND canceled_at IS NULL AND (? OR sponsor_key IN(SELECT key_thumbprint FROM own_devices WHERE removed=1) OR EXISTS(SELECT 1 FROM own_devices d WHERE d.key_thumbprint=expected_key AND (d.removed=1 OR d.generation<>member_generation))) AND NOT EXISTS(SELECT 1 FROM refresh_secrets r WHERE r.session_id=pairing_links.session_id AND r.consumed_at IS NOT NULL)`, at, hostingRemoved)
	if e != nil {
		return false, nil, e
	}
	rows, e := tx.Query(`UPDATE sessions SET revoked_at=? WHERE revoked_at IS NULL AND id IN (SELECT o.session_id FROM own_device_sessions o JOIN own_devices d ON d.key_thumbprint=o.key_thumbprint WHERE ? OR d.removed=1 OR d.generation<>o.generation) RETURNING id`, at, hostingRemoved)
	if e != nil {
		return false, nil, e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return false, nil, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return false, nil, e
	}
	if e = tx.Commit(); e != nil {
		return false, nil, e
	}
	return true, ids, nil
}

// RegisterOwnDevice changes metadata only, never membership or removal state.
func (s *Store) RegisterOwnDevice(m owndevices.Member) error {
	if e := owndevices.Validate(m); e != nil {
		return e
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var raw string
	e = tx.QueryRow(`SELECT member FROM own_devices WHERE key_thumbprint=? AND removed=0`, m.KeyThumbprint).Scan(&raw)
	if e != nil {
		return e
	}
	var old owndevices.Member
	if e = json.Unmarshal([]byte(raw), &old); e != nil {
		return e
	}
	if old.BackendID != "" && old.BackendID != m.BackendID {
		return errors.New("a device cannot change its backend identity")
	}
	var count int
	if m.BackendID != "" {
		e = tx.QueryRow(`SELECT count(*) FROM own_devices WHERE json_extract(member,'$.backendId')=? AND key_thumbprint<>? AND removed=0`, m.BackendID, m.KeyThumbprint).Scan(&count)
		if e != nil {
			return e
		}
		if count > 0 {
			return errors.New("backend identity already belongs to another device")
		}
	}
	m.Generation, m.Removed = old.Generation, false
	if m.DeviceClass == "" {
		m.DeviceClass = old.DeviceClass
	}
	if reflect.DeepEqual(m, old) {
		return nil
	}
	if e = writeOwnDevice(tx, m); e != nil {
		return e
	}
	return tx.Commit()
}

// AdmitOwnPairing is restart-safe: the personal invitation is durable evidence
// of human approval; introductions may only use a still-active generation.
func (s *Store) AdmitOwnPairing(link PairingLink) error {
	if link.Purpose == "" {
		return nil
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var confirmed int64
	e = tx.QueryRow(`SELECT confirmed_at FROM pairing_links WHERE id=? AND confirmed_at IS NOT NULL AND canceled_at IS NULL`, link.ID).Scan(&confirmed)
	if e != nil {
		return e
	}
	var exists int
	e = tx.QueryRow(`SELECT count(*) FROM own_device_sessions WHERE session_id=?`, link.SessionID).Scan(&exists)
	if e != nil || exists > 0 {
		return e
	}
	var m owndevices.Member
	var raw string
	e = tx.QueryRow(`SELECT member FROM own_devices WHERE key_thumbprint=?`, link.KeyThumbprint).Scan(&raw)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if e == nil {
		if e = json.Unmarshal([]byte(raw), &m); e != nil {
			return e
		}
	}
	if link.Purpose == "own-introduction" {
		if m.Removed || m.Generation != link.MemberGeneration {
			return errors.New("own-device introduction is no longer authorized")
		}
		var active int
		e = tx.QueryRow(`SELECT count(*) FROM own_devices WHERE key_thumbprint=? AND removed=0`, link.SponsorKey).Scan(&active)
		if e != nil {
			return e
		}
		if active != 1 {
			return errors.New("own-device sponsor was removed")
		}
	} else {
		if m.KeyThumbprint == "" {
			var count int
			if e = tx.QueryRow(`SELECT count(*) FROM own_devices`).Scan(&count); e != nil {
				return e
			}
			if count >= owndevices.MaxMembers {
				return errors.New("too many own devices")
			}
			m = owndevices.Member{KeyThumbprint: link.KeyThumbprint, Generation: 1}
			if e = tx.QueryRow(`SELECT label,class FROM devices WHERE id=?`, link.DeviceID).Scan(&m.Name, &m.DeviceClass); e != nil {
				return e
			}
		}
		if link.MemberGeneration < 1 || m.Generation > link.MemberGeneration || (m.Removed && m.Generation >= link.MemberGeneration) {
			return errors.New("personal pairing admission was superseded")
		}
		m.Generation = link.MemberGeneration
		m.Removed = false
		if e = writeOwnDevice(tx, m); e != nil {
			return e
		}
	}
	_, e = tx.Exec(`INSERT INTO own_device_sessions(session_id,key_thumbprint,generation) VALUES(?,?,?)`, link.SessionID, link.KeyThumbprint, m.Generation)
	if e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Store) PairingLinkByTokenHash(hash []byte) (PairingLink, error) {
	return scanPairingLink(s.reader().QueryRow(`SELECT `+pairingLinkColumns+` FROM pairing_links WHERE token_hash=?`, hash))
}

// RetireOwnIntroductions replaces only enrollments whose first renewal was never
// acknowledged. A completed independent session is never revoked by a retry.
func (s *Store) RetireOwnIntroductions(key string, at int64) ([]string, error) {
	tx, e := s.db.Begin()
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	_, e = tx.Exec(`UPDATE pairing_links SET canceled_at=? WHERE purpose='own-introduction' AND expected_key=? AND canceled_at IS NULL AND NOT EXISTS(SELECT 1 FROM refresh_secrets r WHERE r.session_id=pairing_links.session_id AND r.consumed_at IS NOT NULL)`, at, key)
	if e != nil {
		return nil, e
	}
	rows, e := tx.Query(`UPDATE sessions SET revoked_at=? WHERE revoked_at IS NULL AND id IN(SELECT session_id FROM pairing_links WHERE purpose='own-introduction' AND expected_key=? AND canceled_at IS NOT NULL) RETURNING id`, at, key)
	if e != nil {
		return nil, e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return nil, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return ids, nil
}

// PrepareOwnPairing freezes the generation authorized by this human approval.
// Crash recovery may finish that admission, never invent a newer restoration.
func (s *Store) PrepareOwnPairing(id string) error {
	_, e := s.db.Exec(`UPDATE pairing_links SET member_generation=COALESCE((SELECT generation+removed FROM own_devices WHERE key_thumbprint=pairing_links.key_thumbprint),1) WHERE id=? AND purpose='own-device' AND confirmed_at IS NULL AND canceled_at IS NULL`, id)
	return e
}
func (s *Store) IncompleteOwnAdmissions() ([]PairingLink, error) {
	rows, e := s.reader().Query(`SELECT ` + pairingLinkColumns + ` FROM pairing_links WHERE purpose<>'' AND confirmed_at IS NOT NULL AND canceled_at IS NULL AND session_id NOT IN(SELECT session_id FROM own_device_sessions) AND session_id IN(SELECT id FROM sessions WHERE revoked_at IS NULL)`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []PairingLink{}
	for rows.Next() {
		link, e := scanPairingLink(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, link)
	}
	return out, rows.Err()
}
