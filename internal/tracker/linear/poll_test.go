package linear

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// pollStore is a fake workspace answering a poll as Linear does: issues
// in update order with keyset cursors. It predicts the variables of the
// next request from what it answered, so the fake checks each request
// exactly.
type pollStore struct {
	mu      sync.Mutex
	updated map[string]time.Time // issue ID to updatedAt
	pages   int                  // pages answered
	// onPage runs before page n (from 1) is computed.
	onPage func(n int, s *pollStore)
	// refuseCursor, when set, refuses the next request carrying a cursor.
	refuseCursor bool
	// wantSince and wantAfter are the next request's expected bounds;
	// floor is the latest update time delivered.
	wantSince, floor time.Time
	wantAfter        string
}

func newPollStore(since time.Time) *pollStore {
	return &pollStore{updated: map[string]time.Time{}, wantSince: since, floor: since}
}

func (s *pollStore) expected() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	after := ""
	if s.wantAfter != "" {
		after = fmt.Sprintf(`,"after":%q`, s.wantAfter)
	}
	return fmt.Sprintf(`{"first":100%s,"sort":[{"updatedAt":{"order":"Ascending"}}],"includeArchived":true,`+
		`"filter":{"and":[{"team":{"id":{"in":["team-web"]}}},{"updatedAt":{"gte":%q}}]}}`, after, formatTime(s.wantSince))
}

// script is n exchanges answered by the store.
func (s *pollStore) script(n int) []exchange {
	out := make([]exchange, n)
	for i := range out {
		out[i] = exchange{op: "Issues", varsFn: s.expected, respond: s.respond}
	}
	return out
}

func (s *pollStore) respond(raw json.RawMessage) string {
	var vars struct {
		After string `json:"after"`
	}
	if err := json.Unmarshal(raw, &vars); err != nil {
		panic(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuseCursor && vars.After != "" {
		s.refuseCursor = false
		s.wantSince, s.wantAfter = s.floor, ""
		return `{"data":null,"errors":[{"message":"Invalid cursor","path":["page"],"extensions":{"code":"INPUT_ERROR","userError":true}}]}`
	}
	s.pages++
	if s.onPage != nil {
		s.onPage(s.pages, s)
	}
	rows := s.after(s.wantSince, vars.After)
	end := min(issuePage, len(rows))
	var nodes []string
	cursor := ""
	for _, r := range rows[:end] {
		nodes = append(nodes, issueJSON(r.id, "WEB-"+r.id, formatTime(r.at), 1))
		s.floor = maxTime(s.floor, r.at)
		cursor = r.at.UTC().Format(time.RFC3339Nano) + "|" + r.id
	}
	if end < len(rows) {
		s.wantAfter = cursor
	} else {
		s.wantSince, s.wantAfter = s.floor, ""
	}
	return data(pageJSON(nodes, end < len(rows), cursor))
}

type pollRow struct {
	id string
	at time.Time
}

// after is the issues updated at or after since, past the keyset cursor.
func (s *pollStore) after(since time.Time, cursor string) []pollRow {
	var rows []pollRow
	for id, at := range s.updated {
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

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// drain polls page by page, each page through a new client as a process
// restarted after every page would, until a walk ends. It returns every
// issue delivered with its update times, the last Next, and whether any
// page restarted.
func drain(t *testing.T, f *fake, from PollCursor) (map[string][]time.Time, PollCursor, bool) {
	t.Helper()
	seen := map[string][]time.Time{}
	restarted := false
	for range 10 {
		p, err := f.client(t).PollIssues(context.Background(), PollQuery{TeamID: "team-web", IncludeArchived: true, From: from})
		if err != nil {
			t.Fatal(err)
		}
		for _, issue := range p.Items {
			seen[issue.ID] = append(seen[issue.ID], issue.UpdatedAt)
		}
		if p.Next.Floor.Before(from.Floor) {
			t.Fatalf("Floor went back from %v to %v", from.Floor, p.Next.Floor)
		}
		restarted = restarted || p.Restarted
		from = p.Next
		if !p.HasNextPage {
			return seen, from, restarted
		}
	}
	t.Fatal("the walk did not end")
	return nil, from, false
}

// A run of 101 issues sharing one update time, more than a page, is
// drained across restarts: the stored cursor carries the walk past the
// first 100, and the next poll starts at that time, inclusive.
func TestPollCohortLargerThanAPage(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	since := at.Add(-time.Hour)
	store := newPollStore(since)
	for i := range 101 {
		store.updated[fmt.Sprintf("%03d", i)] = at
	}
	f := newFake(t, store.script(4)...)
	seen, next, restarted := drain(t, f, PollCursor{Since: since})
	if len(seen) != 101 || restarted {
		t.Fatalf("%d issues delivered, restarted %t", len(seen), restarted)
	}
	if next != (PollCursor{Since: at, Floor: at}) {
		t.Errorf("next %+v, want a poll from the cohort's time", next)
	}
	// The next poll redelivers the cohort, inclusive, and still drains.
	seen, _, _ = drain(t, f, next)
	if len(seen) != 101 {
		t.Errorf("the next poll delivered %d issues", len(seen))
	}
}

// When Linear refuses a stored cursor, the walk restarts at the Floor,
// inclusive, and still delivers every issue.
func TestPollRefusedCursorRestartsAtFloor(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	since := at.Add(-time.Hour)
	store := newPollStore(since)
	for i := range 101 {
		store.updated[fmt.Sprintf("%03d", i)] = at
	}
	store.updated["early"] = since.Add(time.Minute)
	store.refuseCursor = true
	// Page 1, the refused cursor, the restarted page 1, page 2.
	f := newFake(t, store.script(4)...)
	seen, next, restarted := drain(t, f, PollCursor{Since: since})
	if len(seen) != 102 || !restarted {
		t.Fatalf("%d issues delivered, restarted %t", len(seen), restarted)
	}
	if next.Since != at || next.Floor != at {
		t.Errorf("next %+v", next)
	}
}

// An issue updated during the walk is delivered at its new position, and
// one updated after it was read is delivered again.
func TestPollIssueUpdatedDuringWalk(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	since := base.Add(-time.Hour)
	store := newPollStore(since)
	for i := range 250 {
		store.updated[fmt.Sprintf("%03d", i)] = base.Add(time.Duration(i) * time.Minute)
	}
	later := base.Add(24 * time.Hour)
	store.onPage = func(n int, s *pollStore) {
		if n == 2 {
			s.updated["150"] = later                  // not read yet
			s.updated["010"] = later.Add(time.Second) // read on page 1
		}
	}
	f := newFake(t, store.script(3)...)
	seen, next, _ := drain(t, f, PollCursor{Since: since})
	if len(seen) != 250 {
		t.Errorf("%d distinct issues delivered, want 250", len(seen))
	}
	if got := seen["150"]; len(got) != 1 || !got[0].Equal(later) {
		t.Errorf("issue updated during the walk delivered at %v", got)
	}
	if got := seen["010"]; len(got) != 2 || !got[1].Equal(later.Add(time.Second)) {
		t.Errorf("issue updated after it was read delivered at %v", got)
	}
	if !next.Since.Equal(later.Add(time.Second)) || next.After != "" {
		t.Errorf("next %+v, want the newest update", next)
	}
}

// An empty poll keeps its bounds; with no team and no time it reads every
// issue. A cursor refused for another reason than the input is not
// retried.
func TestPollIssuesEmptyAndOtherErrors(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	nested := `{"data":null,"errors":[{"message":"bad","path":["page","nodes",0,"project"],"extensions":{"code":"INPUT_ERROR","userError":true}}]}`
	f := newFake(t,
		exchange{op: "Issues", vars: `{"first":100,"sort":[{"updatedAt":{"order":"Ascending"}}]}`, body: data(pageJSON(nil, false, ""))},
		exchange{op: "Issues", vars: `{"first":100,"sort":[{"updatedAt":{"order":"Ascending"}}],"filter":{"and":[{"updatedAt":{"gte":"2026-11-01T00:00:00.000Z"}}]}}`, body: data(pageJSON(nil, false, ""))},
		exchange{op: "Issues", vars: `{"first":100,"after":"c1","sort":[{"updatedAt":{"order":"Ascending"}}],"filter":{"and":[{"updatedAt":{"gte":"2026-11-01T00:00:00.000Z"}}]}}`, body: nested},
		exchange{op: "Issues", vars: `{"first":100,"after":"c2","sort":[{"updatedAt":{"order":"Ascending"}}],"filter":{"and":[{"updatedAt":{"gte":"2026-11-01T00:00:00.000Z"}}]}}`, status: 500, body: `{"errors":[{"message":"boom"}]}`},
	)
	c := f.client(t)
	p, err := c.PollIssues(context.Background(), PollQuery{})
	if err != nil || len(p.Items) != 0 || p.Next != (PollCursor{}) {
		t.Fatalf("poll %+v %v", p, err)
	}
	// A cursor stored without a Floor never restarts before its Since.
	p, err = c.PollIssues(context.Background(), PollQuery{From: PollCursor{Since: since}})
	if err != nil || p.Next != (PollCursor{Since: since, Floor: since}) {
		t.Fatalf("poll %+v %v", p, err)
	}
	for _, after := range []string{"c1", "c2"} {
		if _, err := c.PollIssues(context.Background(), PollQuery{From: PollCursor{Since: since, After: after}}); err == nil {
			t.Errorf("cursor %s: no error", after)
		}
	}
}
