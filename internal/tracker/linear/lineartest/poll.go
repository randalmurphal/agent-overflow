package lineartest

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"
)

// PollStore is a fake workspace answering a poll as Linear does: one
// team's issues oldest update first, each page asked for by an update
// time and the IDs left out at it, never by a page cursor. It predicts
// the variables of the next request from what it answered, so the
// server checks each request exactly. Set its fields before the first
// request; change Updated during a walk only from OnPage.
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
	// Clock, when set, is the time each answer's Date header names;
	// nil sends none.
	Clock func() time.Time
	// EndWalk, when set, is where the walk after a finished one starts,
	// from the floor and the IDs it left out there, Linear's clock at
	// the walk's first page and whether the walk delivered an issue; nil
	// resumes exactly there.
	EndWalk func(floor time.Time, atFloor []string, start time.Time, delivered bool) (time.Time, []string)

	mu    sync.Mutex
	pages int
	// floor and atFloor are the next request's expected bounds; start
	// and delivered describe the walk in progress, which a page with no
	// next page ends.
	floor     time.Time
	atFloor   []string
	start     time.Time
	inWalk    bool
	delivered bool
}

// NewPollStore is an empty store of team whose first poll starts at
// since.
func NewPollStore(team string, includeArchived bool, pageSize int, since time.Time) *PollStore {
	return &PollStore{Team: team, IncludeArchived: includeArchived, PageSize: pageSize, Updated: map[string]time.Time{}, floor: since}
}

// FormatTime writes t as the client sends a DateTime.
func FormatTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// PollVars are the variables of a poll page of team (empty: every
// team) from floor (zero: no bound), leaving out atFloor while they
// are still updated at floor.
func PollVars(team string, includeArchived bool, pageSize int, floor time.Time, atFloor []string) string {
	var and []any
	if team != "" {
		and = append(and, map[string]any{"team": map[string]any{"id": map[string]any{"in": []string{team}}}})
	}
	if !floor.IsZero() {
		and = append(and, map[string]any{"updatedAt": map[string]string{"gte": FormatTime(floor)}})
	}
	if len(atFloor) > 0 {
		and = append(and, map[string]any{"or": []any{
			map[string]any{"updatedAt": map[string]string{"gt": FormatTime(floor)}},
			map[string]any{"id": map[string]any{"nin": atFloor}},
		}})
	}
	v := map[string]any{"first": pageSize, "sort": []any{map[string]any{"updatedAt": map[string]string{"order": "Ascending"}}}}
	if len(and) > 0 {
		v["filter"] = map[string]any{"and": and}
	}
	if includeArchived {
		v["includeArchived"] = true
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // every value above encodes
	}
	return string(b)
}

func (s *PollStore) expected() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return PollVars(s.Team, s.IncludeArchived, s.PageSize, s.floor, s.atFloor)
}

func (s *PollStore) header() http.Header {
	if s.Clock == nil {
		return http.Header{"Date": nil}
	}
	return http.Header{"Date": {s.Clock().UTC().Format(http.TimeFormat)}}
}

// Script is n exchanges the store answers.
func (s *PollStore) Script(n int) []Exchange {
	out := make([]Exchange, n)
	for i := range out {
		out[i] = Exchange{Op: "Issues", VarsFn: s.expected, Respond: s.respond, HeaderFn: s.header}
	}
	return out
}

func (s *PollStore) respond(json.RawMessage) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pages++
	if s.OnPage != nil {
		s.OnPage(s.pages, s)
	}
	if !s.inWalk {
		s.inWalk, s.delivered = true, false
		s.start = time.Time{}
		if s.Clock != nil {
			s.start = s.Clock().Truncate(time.Second)
		}
	}
	rows := s.rows()
	end := min(s.PageSize, len(rows))
	nodes := make([]string, 0, end)
	for _, r := range rows[:end] {
		issue := NewIssue(r.id, "WEB-"+r.id, FormatTime(r.at), 1)
		issue.Team = s.Team
		nodes = append(nodes, issue.JSON())
		if r.at.After(s.floor) {
			s.floor, s.atFloor = r.at, nil
		}
		s.atFloor = append(s.atFloor, r.id)
		s.delivered = true
	}
	more := end < len(rows)
	if !more {
		s.inWalk = false
		if s.EndWalk != nil {
			s.floor, s.atFloor = s.EndWalk(s.floor, s.atFloor, s.start, s.delivered)
		}
	}
	return Data(PageJSON(nodes, more, fmt.Sprintf("unused-%d", s.pages)))
}

type pollRow struct {
	id string
	at time.Time
}

// rows are the issues updated at or after the floor, less those left
// out still updated at it, oldest update first.
func (s *PollStore) rows() []pollRow {
	var rows []pollRow
	for id, at := range s.Updated {
		if at.After(s.floor) || at.Equal(s.floor) && !slices.Contains(s.atFloor, id) {
			rows = append(rows, pollRow{id, at})
		}
	}
	slices.SortFunc(rows, func(a, b pollRow) int { return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.id, b.id)) })
	return rows
}
