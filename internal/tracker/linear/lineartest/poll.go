package lineartest

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// PollStore is a fake workspace answering a poll as Linear does: one
// team's issues in update order with keyset cursors. It predicts the
// variables of the next request from what it answered, so the server
// checks each request exactly. Set its fields before the first request;
// change Updated during a walk only from OnPage.
type PollStore struct {
	// Team is the polled team's ID; IncludeArchived is what the poll
	// sends; PageSize is the client's page size.
	Team            string
	IncludeArchived bool
	PageSize        int
	// Updated maps issue ID to its update time.
	Updated map[string]time.Time
	// OnPage runs before page n (from 1) is computed.
	OnPage func(n int, s *PollStore)
	// RefuseCursor, when set, refuses the next request carrying a cursor.
	RefuseCursor bool

	mu    sync.Mutex
	pages int
	// wantSince and wantAfter are the next request's expected bounds;
	// floor is the latest update time delivered.
	wantSince, floor time.Time
	wantAfter        string
}

// NewPollStore is an empty store of team whose first poll starts at
// since.
func NewPollStore(team string, includeArchived bool, pageSize int, since time.Time) *PollStore {
	return &PollStore{Team: team, IncludeArchived: includeArchived, PageSize: pageSize, Updated: map[string]time.Time{}, wantSince: since, floor: since}
}

// FormatTime writes t as the client sends a DateTime.
func FormatTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func (s *PollStore) expected() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	after := ""
	if s.wantAfter != "" {
		after = fmt.Sprintf(`,"after":%q`, s.wantAfter)
	}
	archived := ""
	if s.IncludeArchived {
		archived = `"includeArchived":true,`
	}
	return fmt.Sprintf(`{"first":%d%s,"sort":[{"updatedAt":{"order":"Ascending"}}],%s`+
		`"filter":{"and":[{"team":{"id":{"in":[%q]}}},{"updatedAt":{"gte":%q}}]}}`, s.PageSize, after, archived, s.Team, FormatTime(s.wantSince))
}

// Script is n exchanges the store answers.
func (s *PollStore) Script(n int) []Exchange {
	out := make([]Exchange, n)
	for i := range out {
		out[i] = Exchange{Op: "Issues", VarsFn: s.expected, Respond: s.respond}
	}
	return out
}

func (s *PollStore) respond(raw json.RawMessage) string {
	var vars struct {
		After string `json:"after"`
	}
	if err := json.Unmarshal(raw, &vars); err != nil {
		panic(err) // the server checked the variables against the schema
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.RefuseCursor && vars.After != "" {
		s.RefuseCursor = false
		s.wantSince, s.wantAfter = s.floor, ""
		return `{"data":null,"errors":[{"message":"Invalid cursor","path":["page"],"extensions":{"code":"INPUT_ERROR","userError":true}}]}`
	}
	s.pages++
	if s.OnPage != nil {
		s.OnPage(s.pages, s)
	}
	rows := s.after(s.wantSince, vars.After)
	end := min(s.PageSize, len(rows))
	var nodes []string
	cursor := ""
	for _, r := range rows[:end] {
		issue := NewIssue(r.id, "WEB-"+r.id, FormatTime(r.at), 1)
		issue.Team = s.Team
		nodes = append(nodes, issue.JSON())
		if r.at.After(s.floor) {
			s.floor = r.at
		}
		cursor = r.at.UTC().Format(time.RFC3339Nano) + "|" + r.id
	}
	if end < len(rows) {
		s.wantAfter = cursor
	} else {
		s.wantSince, s.wantAfter = s.floor, ""
	}
	return Data(PageJSON(nodes, end < len(rows), cursor))
}

type pollRow struct {
	id string
	at time.Time
}

// after is the issues updated at or after since, past the keyset cursor.
func (s *PollStore) after(since time.Time, cursor string) []pollRow {
	var rows []pollRow
	for id, at := range s.Updated {
		if !at.Before(since) {
			rows = append(rows, pollRow{id, at})
		}
	}
	slices.SortFunc(rows, func(a, b pollRow) int { return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.id, b.id)) })
	if cursor == "" {
		return rows
	}
	at, id, _ := strings.Cut(cursor, "|")
	t, _ := time.Parse(time.RFC3339Nano, at)
	start := 0
	for start < len(rows) && (rows[start].at.Before(t) || (rows[start].at.Equal(t) && rows[start].id <= id)) {
		start++
	}
	return rows[start:]
}
