package store

import (
	"encoding/json"
	"fmt"
)

// KeyedTicket is a ticket key a text names, as this computer's mirrors
// know it: ConnectionID is the link reader of the site whose mirror holds
// the key's project, and IssueID and Title the issue when that mirror
// holds it, empty when it holds only other issues of the project. A pull
// request row carries the tickets it names in this shape (app.PRListItem).
type KeyedTicket struct {
	Key          string `json:"key"`
	ConnectionID string `json:"connectionId"`
	IssueID      string `json:"issueId,omitempty"`
	Title        string `json:"title,omitempty"`
}

// ticketsByKeySQL resolves each key of the bound JSON array against the
// mirror of every Jira and Linear site's link reader (ticketSiteReaderSQL):
// a row per reader that holds the issue (mirrorIssueHeldSQL), or holds any
// issue of the key's project, which is the text before the key's dash.
// Each probe is one seek of idx_tracker_cache_issues_key; the issue rows
// sort first, then by connection, so the first row of a key is its answer.
var ticketsByKeySQL = `WITH k AS MATERIALIZED (SELECT DISTINCT value AS key,
	    substr(value, 1, instr(value, '-')) AS low, substr(value, 1, instr(value, '-') - 1) || '.' AS high
	  FROM json_each(?1) WHERE instr(value, '-') > 1),
	readers AS MATERIALIZED (SELECT c.id AS connection_id, r.id AS root_id FROM tracker_connections c
	  JOIN tracker_cache_roots r ON r.connection_id = c.id
	  WHERE c.kind IN ('jira', 'linear') AND c.id = ` + ticketSiteReaderSQL("c.kind", "c.site") + `)
	SELECT k.key, rd.connection_id, iif(` + mirrorIssueHeldSQL + `, i.issue_id, ''), iif(` + mirrorIssueHeldSQL + `, i.title, '')
	FROM k CROSS JOIN readers rd
	LEFT JOIN tracker_cache_issues i ON i.root_id = rd.root_id AND i.issue_key = k.key
	LEFT JOIN tracker_cache_issue_missing m ON m.root_id = rd.root_id AND m.issue_id = i.issue_id
	WHERE ` + mirrorIssueHeldSQL + ` OR EXISTS (SELECT 1 FROM tracker_cache_issues p
	  WHERE p.root_id = rd.root_id AND p.issue_key >= k.low AND p.issue_key < k.high)
	ORDER BY k.key, NOT ` + mirrorIssueHeldSQL + `, rd.connection_id`

// TicketsByKey answers, for each of keys (capitals, as
// trackerapp.TicketKeys gives them) whose project a Jira or Linear site's
// link reader here mirrors, the ticket it names. A key no mirror holds a
// project of is left out: it is not a ticket of this computer's trackers.
func (s *Store) TicketsByKey(keys []string) (map[string]KeyedTicket, error) {
	found := map[string]KeyedTicket{}
	if len(keys) == 0 {
		return found, nil
	}
	list, err := json.Marshal(keys)
	if err != nil {
		return nil, fmt.Errorf("store: tickets by key: %w", err)
	}
	rows, err := s.reader().Query(ticketsByKeySQL, string(list))
	if err != nil {
		return nil, fmt.Errorf("store: tickets by key: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var t KeyedTicket
		if err := rows.Scan(&t.Key, &t.ConnectionID, &t.IssueID, &t.Title); err != nil {
			return nil, fmt.Errorf("store: tickets by key: %w", err)
		}
		if _, ok := found[t.Key]; !ok {
			found[t.Key] = t
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: tickets by key: %w", err)
	}
	return found, nil
}
