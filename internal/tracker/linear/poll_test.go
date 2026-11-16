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

// pollStore is a fake workspace's issues answered in update order with
// keyset cursors, as a poll reads them.
type pollStore struct {
	mu      sync.Mutex
	updated map[string]time.Time // issue ID to updatedAt
	cursor  string               // the cursor last handed out
	served  int                  // pages answered
	// onPage runs before page n (from 1) is computed.
	onPage func(n int, s *pollStore)
}

func (s *pollStore) expected(since time.Time) func() string {
	return func() string {
		s.mu.Lock()
		defer s.mu.Unlock()
		after := ""
		if s.cursor != "" {
			after = fmt.Sprintf(`,"after":%q`, s.cursor)
		}
		return fmt.Sprintf(`{"first":100%s,"sort":[{"updatedAt":{"order":"Ascending"}}],"includeArchived":true,`+
			`"filter":{"and":[{"team":{"id":{"in":["team-web"]}}},{"updatedAt":{"gte":%q}}]}}`, after, formatTime(since))
	}
}

func (s *pollStore) respond(raw json.RawMessage) string {
	var vars struct {
		After  string `json:"after"`
		Filter struct {
			And []struct {
				UpdatedAt *struct {
					Gte time.Time `json:"gte"`
				} `json:"updatedAt"`
			} `json:"and"`
		} `json:"filter"`
	}
	if err := json.Unmarshal(raw, &vars); err != nil {
		panic(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.served++
	if s.onPage != nil {
		s.onPage(s.served, s)
	}
	since := vars.Filter.And[1].UpdatedAt.Gte
	type row struct {
		id string
		at time.Time
	}
	var rows []row
	for id, at := range s.updated {
		if !at.Before(since) {
			rows = append(rows, row{id, at})
		}
	}
	slices.SortFunc(rows, func(a, b row) int {
		return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.id, b.id))
	})
	start := 0
	if vars.After != "" {
		at, id, _ := strings.Cut(vars.After, "|")
		t, _ := time.Parse(time.RFC3339Nano, at)
		for start < len(rows) && (rows[start].at.Before(t) || (rows[start].at.Equal(t) && rows[start].id <= id)) {
			start++
		}
	}
	end := min(start+issuePage, len(rows))
	var nodes []string
	for _, r := range rows[start:end] {
		nodes = append(nodes, issueJSON(r.id, "WEB-"+r.id, formatTime(r.at), 1))
	}
	s.cursor = ""
	if end > start {
		last := rows[end-1]
		s.cursor = last.at.UTC().Format(time.RFC3339Nano) + "|" + last.id
	}
	return data(pageJSON(nodes, end < len(rows), s.cursor))
}

// A poll walked to the end delivers every issue, including one updated
// during the walk (delivered at its new position) and one updated after
// it was read (delivered again); Resume advances to the newest update
// seen, and the next poll starts there, inclusive.
func TestPollIssuesResume(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	store := &pollStore{updated: map[string]time.Time{}}
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
	since := base.Add(-time.Hour)
	var script []exchange
	for range 3 {
		script = append(script, exchange{op: "Issues", varsFn: store.expected(since), respond: store.respond})
	}
	f := newFake(t, script...)
	c := f.client(t)
	q := PollQuery{TeamID: "team-web", Since: since, IncludeArchived: true}
	seen := map[string][]time.Time{}
	resume, after := since, ""
	for {
		p, err := c.PollIssues(context.Background(), q, after)
		if err != nil {
			t.Fatal(err)
		}
		for _, issue := range p.Items {
			seen[issue.ID] = append(seen[issue.ID], issue.UpdatedAt)
		}
		if p.Resume.Before(resume) {
			t.Fatalf("Resume went back from %v to %v", resume, p.Resume)
		}
		resume = p.Resume
		if !p.HasNextPage {
			break
		}
		after = p.EndCursor
	}
	if len(seen) != 250 {
		t.Errorf("%d distinct issues delivered, want 250", len(seen))
	}
	if got := seen["150"]; len(got) != 1 || !got[0].Equal(later) {
		t.Errorf("issue updated during the walk delivered at %v", got)
	}
	if got := seen["010"]; len(got) != 2 || !got[1].Equal(later.Add(time.Second)) {
		t.Errorf("issue updated after it was read delivered at %v", got)
	}
	if !resume.Equal(later.Add(time.Second)) {
		t.Errorf("Resume %v, want the newest update", resume)
	}

	// The next poll starts at Resume and re-delivers the issue updated
	// exactly then.
	next := &pollStore{updated: map[string]time.Time{"010": resume}}
	g := newFake(t, exchange{op: "Issues", varsFn: next.expected(resume), respond: next.respond})
	p, err := g.client(t).PollIssues(context.Background(), PollQuery{TeamID: "team-web", Since: resume, IncludeArchived: true}, "")
	if err != nil || len(p.Items) != 1 || !p.Resume.Equal(resume) {
		t.Fatalf("next poll %+v %v", p, err)
	}
}

// An empty poll keeps Since as Resume; with no team and no Since it
// reads every issue.
func TestPollIssuesEmpty(t *testing.T) {
	t.Parallel()
	f := newFake(t, exchange{op: "Issues", vars: `{"first":100,"sort":[{"updatedAt":{"order":"Ascending"}}]}`, body: data(pageJSON(nil, false, ""))})
	p, err := f.client(t).PollIssues(context.Background(), PollQuery{}, "")
	if err != nil || len(p.Items) != 0 || !p.Resume.IsZero() {
		t.Fatalf("poll %+v %v", p, err)
	}
}
