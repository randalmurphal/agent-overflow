package trackerapp

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// writing is a scripted tracker that also writes: each write is recorded
// as a call of its name, with its input, and answered by result. A write
// with no result fails the test and answers a result with no parts.
type writing struct {
	*scripted
	result func(name string, in any) tracker.Result
	// inputs are the inputs of the writes made, in order.
	inputs []any
	// time is the time tracker; nil for a tracker with none.
	time     *scriptedTime
	projects func() ([]tracker.Project, error)
	search   func(tracker.SearchQuery) (tracker.SearchPage, error)
}

func (p *writing) answer(ctx context.Context, name string, in any) tracker.Result {
	p.mu.Lock()
	h := p.result
	p.inputs = append(p.inputs, in)
	p.mu.Unlock()
	if !p.called(ctx, name, func() bool { return h != nil }) {
		return tracker.Result{}
	}
	if err := p.wait(ctx, name); err != nil {
		return tracker.Result{Parts: []tracker.Part{{Step: tracker.StepEdit, Outcome: tracker.NotSent(err)}}}
	}
	return h(name, in)
}

func (p *writing) Create(ctx context.Context, in tracker.CreateInput) tracker.Result {
	return p.answer(ctx, "Create", in)
}
func (p *writing) Edit(ctx context.Context, in tracker.EditInput) tracker.Result {
	return p.answer(ctx, "Edit", in)
}
func (p *writing) Move(ctx context.Context, in tracker.MoveInput) tracker.Result {
	return p.answer(ctx, "Move", in)
}
func (p *writing) SetEstimate(ctx context.Context, in tracker.EstimateInput) tracker.Result {
	return p.answer(ctx, "SetEstimate", in)
}
func (p *writing) Rank(ctx context.Context, in tracker.RankInput) tracker.Result {
	return p.answer(ctx, "Rank", in)
}
func (p *writing) MoveToSprint(ctx context.Context, in tracker.SprintMoveInput) tracker.Result {
	return p.answer(ctx, "MoveToSprint", in)
}
func (p *writing) CreateSprint(ctx context.Context, in tracker.SprintInput) tracker.Result {
	return p.answer(ctx, "CreateSprint", in)
}
func (p *writing) StartSprint(ctx context.Context, in tracker.SprintStart) tracker.Result {
	return p.answer(ctx, "StartSprint", in)
}
func (p *writing) CompleteSprint(ctx context.Context, in tracker.SprintCompletion) tracker.Result {
	return p.answer(ctx, "CompleteSprint", in)
}
func (p *writing) EditSprint(ctx context.Context, in tracker.SprintEdit) tracker.Result {
	return p.answer(ctx, "EditSprint", in)
}
func (p *writing) AddComment(ctx context.Context, in tracker.CommentInput) tracker.Result {
	return p.answer(ctx, "AddComment", in)
}
func (p *writing) EditComment(ctx context.Context, in tracker.CommentEdit) tracker.Result {
	return p.answer(ctx, "EditComment", in)
}
func (p *writing) DeleteComment(ctx context.Context, in tracker.CommentDelete) tracker.Result {
	return p.answer(ctx, "DeleteComment", in)
}
func (p *writing) Link(ctx context.Context, in tracker.LinkInput) tracker.Result {
	return p.answer(ctx, "Link", in)
}
func (p *writing) Unlink(ctx context.Context, in tracker.UnlinkInput) tracker.Result {
	return p.answer(ctx, "Unlink", in)
}
func (p *writing) Attach(ctx context.Context, in tracker.AttachInput) tracker.Result {
	return p.answer(ctx, "Attach", in)
}

func (p *writing) Time() tracker.TimeTracker {
	if p.time == nil {
		return nil
	}
	return p.time
}

// Poker is nil: a scripted tracker has no planning poker.
func (p *writing) Poker() tracker.Poker { return nil }

func (p *writing) Projects(ctx context.Context) ([]tracker.Project, error) {
	p.mu.Lock()
	h := p.projects
	p.mu.Unlock()
	if !p.called(ctx, "Projects", func() bool { return h != nil }) {
		return nil, errUnscripted
	}
	return h()
}

func (p *writing) Search(ctx context.Context, q tracker.SearchQuery) (tracker.SearchPage, error) {
	p.mu.Lock()
	h := p.search
	p.mu.Unlock()
	if !p.called(ctx, "Search", func() bool { return h != nil }) {
		return tracker.SearchPage{}, errUnscripted
	}
	if err := q.Check(); err != nil {
		return tracker.SearchPage{}, err
	}
	return h(q)
}

// scriptedTime is the writing tracker's time tracker: its reads answer
// from logs, its writes as the tracker's writes are answered.
type scriptedTime struct {
	p    *writing
	mu   sync.Mutex
	logs []tracker.Worklog
	// reads are the queries of each Worklogs call.
	reads []tracker.WorklogQuery
	fail  error
}

func (tt *scriptedTime) Worklogs(ctx context.Context, q tracker.WorklogQuery) ([]tracker.Worklog, error) {
	tt.p.called(ctx, "Worklogs", func() bool { return true })
	if err := tt.p.wait(ctx, "Worklogs"); err != nil {
		return nil, err
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.reads = append(tt.reads, q)
	if tt.fail != nil {
		return nil, tt.fail
	}
	var out []tracker.Worklog
	for _, w := range tt.logs {
		if w.Author == q.Author && w.Day.Compare(q.From) >= 0 && w.Day.Compare(q.To) <= 0 {
			out = append(out, w)
		}
	}
	return out, nil
}

func (tt *scriptedTime) AddWorklog(ctx context.Context, in tracker.WorklogInput) tracker.Result {
	return tt.p.answer(ctx, "AddWorklog", in)
}
func (tt *scriptedTime) EditWorklog(ctx context.Context, in tracker.WorklogEdit) tracker.Result {
	return tt.p.answer(ctx, "EditWorklog", in)
}
func (tt *scriptedTime) DeleteWorklog(ctx context.Context, in tracker.WorklogDelete) tracker.Result {
	return tt.p.answer(ctx, "DeleteWorklog", in)
}

// boardsState is the write tests' tracker: each board's rows, which a
// delta returns whole once after every change and a rows read by ID.
type boardsState struct {
	mu       sync.Mutex
	rows     map[tracker.BoardID]map[tracker.IssueID]tracker.Row
	comments map[tracker.IssueID]string
	version  int
}

// set makes row the issue's row on board, its card on every board.
func (s *boardsState) set(board tracker.BoardID, r tracker.Row) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows[board] == nil {
		s.rows[board] = map[tracker.IssueID]tracker.Row{}
	}
	s.rows[board][r.Card.ID] = r
	for _, rows := range s.rows {
		if other, ok := rows[r.Card.ID]; ok {
			other.Card = r.Card
			rows[r.Card.ID] = other
		}
	}
	s.version++
}

func (s *boardsState) changedRows(q tracker.DeltaQuery) (tracker.RowPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := cursorAfter(fmt.Sprint("v", s.version))
	if bytes.Equal(q.From, now) {
		return tracker.RowPage{Next: now}, nil
	}
	page := tracker.RowPage{Next: now, Catalog: tracker.Catalog{Users: []tracker.User{user1}}}
	for _, id := range slices.Sorted(maps.Keys(s.rows[q.Board])) {
		page.Rows = append(page.Rows, s.rows[q.Board][id])
	}
	return page, nil
}

func (s *boardsState) boardRows(b tracker.BoardID, asked []tracker.IssueID) (tracker.Rows, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out tracker.Rows
	for _, id := range asked {
		if r, ok := s.rows[b][id]; ok {
			out.Rows = append(out.Rows, r)
		} else {
			out.Missing = append(out.Missing, id)
		}
	}
	return out, nil
}

// placement places issue on each board as the state holds it: its row
// where a board holds it, Off elsewhere.
func (s *boardsState) placement(issue tracker.IssueID, boards []tracker.BoardID) (tracker.Placement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := tracker.Placement{Catalog: tracker.Catalog{Users: []tracker.User{user1}}}
	for _, b := range boards {
		if r, ok := s.rows[b][issue]; ok {
			out.Rows = append(out.Rows, tracker.BoardRow{Board: b, Row: r})
		} else {
			out.Off = append(out.Off, b)
		}
	}
	return out, nil
}

func (s *boardsState) listing(b tracker.BoardID, _ time.Time) (tracker.Listing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return tracker.Listing{IDs: slices.Sorted(maps.Keys(s.rows[b])), Complete: true}, nil
}

// asked records the issues of each rows read, by board.
func (s *boardsState) recordRows(p *scripted) func() []string {
	var mu sync.Mutex
	var reads []string
	p.set(func(p *scripted) {
		p.boardRows = func(b tracker.BoardID, asked []tracker.IssueID) (tracker.Rows, error) {
			mu.Lock()
			reads = append(reads, fmt.Sprintf("%s:%v", b, asked))
			mu.Unlock()
			return s.boardRows(b, asked)
		}
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := reads
		reads = nil
		return out
	}
}

func (s *boardsState) card(id tracker.IssueID) (tracker.Card, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rows := range s.rows {
		if r, ok := rows[id]; ok {
			return r.Card, true
		}
	}
	return tracker.Card{}, false
}

func (s *boardsState) cardsRead(asked []tracker.IssueID) (tracker.Cards, error) {
	var out tracker.Cards
	for _, id := range asked {
		if c, ok := s.card(id); ok {
			out.Cards = append(out.Cards, c)
		}
	}
	return out, nil
}

func (s *boardsState) detail(id tracker.IssueID) (tracker.Detail, error) {
	c, ok := s.card(id)
	if !ok {
		return tracker.Detail{}, fmt.Errorf("test: no issue %s", id)
	}
	d := detailOfCard(c)
	d.Links = nil
	s.mu.Lock()
	if body, ok := s.comments[id]; ok {
		d.Comments.Comments[0].Body = body
	}
	s.mu.Unlock()
	return d, nil
}

// comment makes body issue's comment, which moves no update time.
func (s *boardsState) comment(id tracker.IssueID, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.comments == nil {
		s.comments = map[tracker.IssueID]string{}
	}
	s.comments[id] = body
}

// writeFixture is a mirror fixture whose tracker writes, with board 1
// holding 10001 to 10003 and board 2 holding 10001, both synced. Pump
// timers are an hour: only what a test runs reads the tracker.
type writeFixture struct {
	*mirrorFixture
	w     *writing
	state *boardsState
}

// writeRow is issue id's row with order key order, updated at the base
// time plus minutes.
func writeRow(id string, minutes int, order string) tracker.Row {
	return row(id, 1_799_000_000_000+int64(minutes)*60_000, order)
}

func newWriteFixture(t *testing.T) *writeFixture {
	t.Helper()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
	w := &writing{scripted: m.p}
	w.time = &scriptedTime{p: w}
	m.svc.makeProvider = func(_ store.TrackerConnection, _ *clients, _ *clients) (tracker.Provider, error) { return w, nil }
	st := &boardsState{rows: map[tracker.BoardID]map[tracker.IssueID]tracker.Row{}}
	st.set(testBoard, writeRow("10001", 0, "a"))
	st.set(testBoard, writeRow("10002", 0, "b"))
	st.set(testBoard, writeRow("10003", 0, "c"))
	st.set("2", writeRow("10001", 0, "m"))
	m.p.set(func(p *scripted) {
		p.changedRows, p.boardRows, p.cards, p.detail, p.boardIssues = st.changedRows, st.boardRows, st.cardsRead, st.detail, st.listing
		p.newRows = st.placement
		p.history = func(tracker.HistoryQuery) (tracker.History, error) { return tracker.History{}, nil }
	})
	f := &writeFixture{mirrorFixture: m, w: w, state: st}
	for _, b := range []tracker.BoardID{testBoard, "2"} {
		tickOK(t, m.svc, Scope{Connection: m.conn.ID, Board: b}, BoardDue{Structure: true, Delta: true})
	}
	return f
}

// answer makes every write answer res.
func (f *writeFixture) answer(res tracker.Result) {
	f.w.set(func(*scripted) { f.w.result = func(string, any) tracker.Result { return res } })
}

// calls are the provider calls made since the first n.
func (f *writeFixture) callsSince(n int) []string { return f.p.callsInOrder()[n:] }

// orderOf is issue's order key on board as the mirror holds it; "" for
// no row.
func (f *writeFixture) orderOf(t *testing.T, board tracker.BoardID, issue string) string {
	t.Helper()
	root := f.root()
	if root == 0 {
		return ""
	}
	rows, _, err := f.st.ListMirrorIssueRows(root, issue, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.BoardID == string(board) {
			return r.OrderKey
		}
	}
	return ""
}

// applied is part, applied, about issue with step, naming changed.
func appliedPart(issue string, step tracker.Step, changed ...tracker.Resource) tracker.Part {
	return tracker.Part{Issue: tracker.IssueID(issue), Step: step, Outcome: tracker.Applied(), Changed: changed}
}

// rankPart is a rank part of issue on board with outcome o.
func rankPart(board tracker.BoardID, issue string, o tracker.Outcome) tracker.Part {
	id := tracker.IssueID(issue)
	return tracker.Part{Issue: id, Step: tracker.StepRank, Outcome: o, Changed: []tracker.Resource{tracker.RowOf(board, id), tracker.CardOf(id)}}
}
