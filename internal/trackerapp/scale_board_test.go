package trackerapp

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
)

// scaleSizes are the board sizes AO_TRACKER_SCALE names (a comma list),
// for the measurements a normal run leaves out; the test is skipped when
// it names none.
func scaleSizes(t *testing.T) []int {
	t.Helper()
	env := os.Getenv("AO_TRACKER_SCALE")
	if env == "" {
		t.Skip("AO_TRACKER_SCALE names no sizes")
	}
	var sizes []int
	for field := range strings.SplitSeq(env, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || n < readBatch {
			t.Fatalf("AO_TRACKER_SCALE=%q: each size is a whole number of at least %d", env, readBatch)
		}
		sizes = append(sizes, n)
	}
	return sizes
}

// scaleRow is issue n of a large board, a realisticRow placed as a real
// board's rows are: one in 50 is an epic the next 49 name as parent, by
// its key and title as the epic was first read, one
// in 10 is in the active sprint and one in 10 in the future sprint, the
// rest in the backlog, and a third in each of the three statuses.
func scaleRow(n int, updated time.Time) tracker.Row {
	r := realisticRow(n, updated)
	parent := realisticRow(n-n%50, updated).Card
	r.Card.Parent, r.Card.ParentName = parent.ID, &tracker.ParentName{Key: parent.Key, Title: parent.Title}
	if n%50 == 0 {
		r.Card.Parent, r.Card.ParentName = "", nil
	}
	switch n % 10 {
	case 0:
		r.Card.Sprints = []tracker.SprintID{"1"}
	case 1:
		r.Card.Sprints = []tracker.SprintID{"2"}
	default:
		r.Card.Sprints = nil
	}
	return r
}

// scaleBoard is the tracker side of a large board the scripted provider
// answers for every board: its rows, each with the change sequence that
// last touched it, walked in that order by a delta whose cursor is a
// sequence number.
type scaleBoard struct {
	mu   sync.Mutex
	rows []tracker.Row
	seq  []int
	// log is every row index in the order of its sequence numbers; an
	// entry a later change superseded is skipped.
	log  []int
	last int
	at   map[tracker.IssueID]int
}

func newScaleBoard(n int) *scaleBoard {
	b := &scaleBoard{at: make(map[tracker.IssueID]int, n)}
	updated := time.UnixMilli(1_799_000_000_000).UTC()
	for i := range n {
		b.rows = append(b.rows, scaleRow(i, updated))
		b.at[b.rows[i].Card.ID] = i
		b.last++
		b.seq = append(b.seq, b.last)
		b.log = append(b.log, i)
	}
	return b
}

func scaleCursor(seq int) tracker.Cursor { return tracker.Cursor(fmt.Sprint("seq:", seq)) }

// edit changes the title of each row of indices, a later update.
func (b *scaleBoard) edit(indices []int, rev int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, i := range indices {
		r := &b.rows[i]
		r.Card.Updated = r.Card.Updated.Add(time.Second)
		r.Card.Title = fmt.Sprintf("%-60s", fmt.Sprintf("Renamed %d, revision %d", i, rev))
		b.last++
		b.seq[i] = b.last
		b.log = append(b.log, i)
	}
}

func (b *scaleBoard) changedRows(q tracker.DeltaQuery) (tracker.RowPage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	from := 0
	if q.From != nil {
		var err error
		if from, err = strconv.Atoi(strings.TrimPrefix(string(q.From), "seq:")); err != nil {
			return tracker.RowPage{}, fmt.Errorf("%w: cursor %q", tracker.ErrInvalidInput, q.From)
		}
	}
	page := tracker.RowPage{Next: scaleCursor(from), Catalog: tracker.Catalog{Users: []tracker.User{user1}}}
	// The log's entry k holds sequence number k+1 or a later one's index.
	for k := from; k < len(b.log); k++ {
		i := b.log[k]
		if b.seq[i] != k+1 {
			continue
		}
		if len(page.Rows) == readBatch {
			page.More = true
			break
		}
		page.Rows = append(page.Rows, b.rows[i])
		page.Next = scaleCursor(k + 1)
	}
	return page, nil
}

func (b *scaleBoard) listing(tracker.BoardID, time.Time) (tracker.Listing, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := tracker.Listing{IDs: make([]tracker.IssueID, len(b.rows)), Complete: true}
	for i, r := range b.rows {
		out.IDs[i] = r.Card.ID
	}
	return out, nil
}

func (b *scaleBoard) boardRows(_ tracker.BoardID, ids []tracker.IssueID) (tracker.Rows, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := tracker.Rows{Catalog: tracker.Catalog{Users: []tracker.User{user1}}}
	for _, id := range ids {
		if i, ok := b.at[id]; ok {
			out.Rows = append(out.Rows, b.rows[i])
		} else {
			out.Missing = append(out.Missing, id)
		}
	}
	return out, nil
}

func (b *scaleBoard) cards(ids []tracker.IssueID) (tracker.Cards, error) {
	rows, err := b.boardRows("", ids)
	out := tracker.Cards{Missing: rows.Missing, Catalog: rows.Catalog}
	for _, r := range rows.Rows {
		out.Cards = append(out.Cards, r.Card)
	}
	return out, err
}

// history answers each issue with four status changes, final through a
// time after every update.
func (b *scaleBoard) history(q tracker.HistoryQuery) (tracker.History, error) {
	at := time.UnixMilli(1_790_000_000_000).UTC()
	out := tracker.History{Catalog: tracker.Catalog{Users: []tracker.User{user1}}}
	for _, id := range q.Issues {
		h := tracker.IssueHistory{Issue: id, CompleteTo: time.UnixMilli(1_900_000_000_000).UTC()}
		for c := range 4 {
			h.Status = append(h.Status, tracker.StatusChange{Change: tracker.Change{At: at.Add(time.Duration(c) * time.Hour), Author: "acc-user1"},
				From: tracker.StatusID([]string{"s1", "s2"}[c%2]), To: tracker.StatusID([]string{"s2", "s1"}[c%2])})
		}
		out.Issues = append(out.Issues, h)
	}
	return out, nil
}

func (b *scaleBoard) detail(id tracker.IssueID) (tracker.Detail, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d := realisticDetail()
	d.Card = b.rows[b.at[id]].Card
	return d, nil
}

// ids are the IDs of the rows of indices.
func (b *scaleBoard) ids(indices []int) []tracker.IssueID {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]tracker.IssueID, len(indices))
	for k, i := range indices {
		out[k] = b.rows[i].Card.ID
	}
	return slices.Clone(out)
}

// serve makes p answer for b on every board, as a tracker whose update
// time moves with its detail when jira is set.
func (b *scaleBoard) serve(p *scripted, jira bool) {
	p.set(func(p *scripted) {
		p.changedRows, p.boardIssues, p.boardRows, p.history, p.detail = b.changedRows, b.listing, b.boardRows, b.history, b.detail
		p.caps.DetailMovesUpdated = jira
		if jira {
			p.cards = b.cards
		}
	})
}

// spread is k row indices spread evenly over a board of n, none of them
// an epic, whose change tells the rows of its children too.
func spread(n, k int) []int {
	out := make([]int, k)
	for i := range out {
		out[i] = i*(n/k) + 1
	}
	return out
}
