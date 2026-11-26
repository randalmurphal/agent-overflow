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

// ticketsByKeySQL resolves each key of the JSON array ?1 against the
// mirror of every Jira and Linear site's link reader (ticketSiteReaderSQL).
// A key's project is the text before its dash. Which readers' mirrors hold
// any issue of a project is read once per project (held), and a key is
// looked up only in those, so a reader holding none of the keys' projects
// costs one probe per project. Every probe is one seek of
// idx_tracker_cache_issues_key in one root (each probe states the index's
// condition, a key that is not empty). A row per reader holding the
// key's project, with the issue when it holds it (mirrorIssueHeldSQL);
// the rows of a key sort with the issue rows first, then the readers the
// JSON array ?2 names, then by connection.
var ticketsByKeySQL = `WITH k AS MATERIALIZED (SELECT DISTINCT value AS key,
	    substr(value, 1, instr(value, '-')) AS low, substr(value, 1, instr(value, '-') - 1) || '.' AS high
	  FROM json_each(?1) WHERE instr(value, '-') > 1),
	readers AS MATERIALIZED (SELECT c.id AS connection_id, r.id AS root_id FROM tracker_connections c
	  JOIN tracker_cache_roots r ON r.connection_id = c.id
	  WHERE c.kind IN ('jira', 'linear') AND c.id = ` + ticketSiteReaderSQL("c.kind", "c.site") + `),
	held AS MATERIALIZED (SELECT p.low, rd.connection_id, rd.root_id
	  FROM (SELECT DISTINCT low, high FROM k) p CROSS JOIN readers rd
	  WHERE EXISTS (SELECT 1 FROM tracker_cache_issues q
	    WHERE q.root_id = rd.root_id AND q.issue_key >= p.low AND q.issue_key < p.high AND q.issue_key <> ''))
	SELECT k.key, h.connection_id, iif(` + mirrorIssueHeldSQL + `, i.issue_id, ''), iif(` + mirrorIssueHeldSQL + `, i.title, '')
	FROM k CROSS JOIN held h ON h.low = k.low
	LEFT JOIN tracker_cache_issues i ON i.root_id = h.root_id AND i.issue_key = k.key AND i.issue_key <> ''
	LEFT JOIN tracker_cache_issue_missing m ON m.root_id = h.root_id AND m.issue_id = i.issue_id
	ORDER BY k.key, NOT ` + mirrorIssueHeldSQL + `, NOT EXISTS (SELECT 1 FROM json_each(?2) WHERE value = h.connection_id), h.connection_id`

// TicketsByKey answers, for each of keys (capitals, as
// trackerapp.TicketKeys gives them) whose project a Jira or Linear site's
// link reader here mirrors, the tickets it names, one per site whose
// mirror holds its project. A key no mirror holds a project of is left
// out: it is not a ticket of this computer's trackers. The first ticket of
// a key is the one a reader of a single answer takes: a reader whose
// mirror holds the issue, then one of prefer (the connections whose boards
// the asker's project lists), then the lowest connection ID.
func (s *Store) TicketsByKey(keys, prefer []string) (map[string][]KeyedTicket, error) {
	found := map[string][]KeyedTicket{}
	if len(keys) == 0 {
		return found, nil
	}
	list, err := json.Marshal(keys)
	if err != nil {
		return nil, fmt.Errorf("store: tickets by key: %w", err)
	}
	if prefer == nil {
		prefer = []string{}
	}
	preferred, err := json.Marshal(prefer)
	if err != nil {
		return nil, fmt.Errorf("store: tickets by key: %w", err)
	}
	rows, err := s.reader().Query(ticketsByKeySQL, string(list), string(preferred))
	if err != nil {
		return nil, fmt.Errorf("store: tickets by key: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var t KeyedTicket
		if err := rows.Scan(&t.Key, &t.ConnectionID, &t.IssueID, &t.Title); err != nil {
			return nil, fmt.Errorf("store: tickets by key: %w", err)
		}
		found[t.Key] = append(found[t.Key], t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: tickets by key: %w", err)
	}
	return found, nil
}
