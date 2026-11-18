package store

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Each recursive step seeks the first row for the next computer on
// the corresponding partial index.
const ownerComputersSQL = `WITH RECURSIVE links(id) AS (
 SELECT '' UNION ALL SELECT (SELECT target_computer_id FROM thread_requests WHERE target_computer_id > links.id
 AND target_computer_id <> '' AND kind='spawn' AND target_thread_id <> '' AND link_end IN ('','released') ORDER BY target_computer_id LIMIT 1) FROM links WHERE id IS NOT NULL),
 requests(id) AS (SELECT '' UNION ALL SELECT (SELECT target_computer_id FROM thread_requests WHERE target_computer_id > requests.id
 AND target_computer_id <> '' AND polling=1 AND state IN ('unconfirmed','accepted','running','finished') ORDER BY target_computer_id LIMIT 1)
 FROM requests WHERE id IS NOT NULL)
 SELECT id FROM links WHERE id<>'' UNION SELECT id FROM requests WHERE id<>''`

func (s *Store) OwnerComputers() ([]string, error) {
	return s.queryThreadIDs("list computers with owner work", ownerComputersSQL)
}

func (s *Store) OwnerRequests(computer string, tokens []string) ([]ThreadRequest, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	keys, err := json.Marshal(tokens)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader().Query(`SELECT `+threadRequestColumns+` FROM json_each(?2) keys CROSS JOIN thread_requests
	  WHERE token = keys.value AND target_computer_id = `+textParam("?1"), computer, string(keys))
	if err != nil {
		return nil, err
	}
	return collectThreadRequests(rows, "read changed requests on "+computer)
}

var recoverOwnerRequestsSQL = `SELECT token FROM thread_requests
	  WHERE target_computer_id = ` + textParam("?1") + ` AND target_computer_id <> '' AND token > ?2
	    AND (nested_open = 1 OR (polling = 1 AND state IN ('accepted','running')))
	  ORDER BY token LIMIT `

func (s *Store) RecoverOwnerRequests(computer, after string, limit int) ([]string, error) {
	if limit < 1 {
		return nil, fmt.Errorf("store: positive recovery page size required")
	}
	return s.queryThreadIDs("read requests to reconcile", recoverOwnerRequestsSQL+strconv.Itoa(limit), computer, after)
}

const uncollectedOwnerTokensSQL = `SELECT token FROM thread_request_receipts
	  WHERE owner_device_id=?1 AND token>?2 AND lineage_only=0 AND settled_at IS NOT NULL
	    AND collected_revision < revision ORDER BY token LIMIT `

func (s *Store) UncollectedOwnerTokens(device, after string, limit int) ([]string, error) {
	if device == "" || limit < 1 {
		return nil, fmt.Errorf("store: listing uncollected answers requires a device and a positive limit")
	}
	return s.queryThreadIDs("read uncollected owner answers", uncollectedOwnerTokensSQL+strconv.Itoa(limit), device, after)
}

func (s *Store) DueLinkContacts(computer string, now int64, limit int) ([]ThreadRequest, error) {
	if limit < 1 {
		return nil, fmt.Errorf("store: positive contact batch size required")
	}
	rows, err := s.reader().Query(`SELECT `+threadRequestColumns+` FROM thread_requests
	  WHERE target_computer_id = `+textParam("?1")+` AND target_computer_id <> '' AND kind='spawn' AND target_thread_id <> '' AND link_end=''
	    AND contact_due <= ?2 ORDER BY contact_due, token LIMIT `+strconv.Itoa(limit), computer, now)
	if err != nil {
		return nil, err
	}
	return collectThreadRequests(rows, "read remote links due for owner contact")
}

func (s *Store) ScheduleLinkContacts(computer string, tokens []string, next int64) error {
	keys, err := json.Marshal(tokens)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE thread_requests SET contact_due=?3 WHERE target_computer_id = `+textParam("?1")+`
	  AND token IN (SELECT value FROM json_each(?2)) AND link_end=''`, computer, string(keys), next)
	return err
}
