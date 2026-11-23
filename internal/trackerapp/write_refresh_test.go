package trackerapp

import (
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

func day(t *testing.T, s string) tracker.Date {
	t.Helper()
	return must[tracker.Date](t)(tracker.ParseDate(s))
}

// A write names every kind of resource, and each is read again through its
// own read, batched: the board's structure; one rows read per board for
// the rows named on it and the cards it holds; one cards read for the
// card kept on no board; one details read for the open ticket and the
// detail the mirror holds; one worklogs read per span of days. A card and
// a detail the mirror neither keeps nor holds are not read. The store
// writes are one per read, after one read of where everything lives, and
// only the open ticket's changed detail is built into a view.
func TestARefreshReadsEveryResourceKind(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	reads := f.state.recordRows(f.p)
	linked := tracker.Card{ID: "10009", Key: "WEB-9", Project: "10000", Title: "Linked", Status: "s1", Created: time.UnixMilli(1_700_000_000_000).UTC(),
		Updated: time.UnixMilli(1_799_000_000_000).UTC()}
	var cardReads [][]tracker.IssueID
	f.p.set(func(p *scripted) {
		p.cards = func(asked []tracker.IssueID) (tracker.Cards, error) {
			cardReads = append(cardReads, slices.Clone(asked))
			return tracker.Cards{Cards: []tracker.Card{linked}}, nil
		}
	})
	f.linkThread("thread-1", "10009")
	must[Outcome](t)(f.svc.tickCards(t.Context(), f.conn.ID, Background))
	f.openDetail(t, "10002")
	must[TicketSubscription](t)(f.svc.SubscribeTicket(f.conn.ID, "10003"))
	f.events.issueEvents(t, 1)
	f.settledTickets(t)
	u := tracker.UserID("acc-user1")
	d1, d2, d3 := day(t, "2026-11-02"), day(t, "2026-11-03"), day(t, "2026-12-20")
	f.w.time.logs = []tracker.Worklog{
		{ID: "w1", Issue: "10001", Author: u, Day: d1, Spent: time.Hour},
		{ID: "w2", Issue: "10001", Author: u, Day: d3, Spent: 2 * time.Hour},
	}
	f.answer(tracker.Result{Parts: []tracker.Part{appliedPart("10001", tracker.StepEdit,
		tracker.CardOf("10001"), tracker.DetailOf("10001"), tracker.RowOf(testBoard, "10002"), tracker.CardOf("10009"),
		tracker.DetailOf("10002"), tracker.DetailOf("10003"), tracker.StructureOf(testBoard), tracker.StructureOf("99"),
		tracker.CardOf("10404"), tracker.WorklogsOf(u, d1), tracker.WorklogsOf(u, d2), tracker.WorklogsOf(u, d3))}})
	f.state.comment("10002", "New comment")
	f.state.comment("10003", "New comment")
	cs := newCountingStore(f.st)
	f.svc.store = cs
	cardReads = nil
	before, details := len(f.p.callsInOrder()), len(f.p.detailsRead())
	v, err := f.svc.Edit(t.Context(), f.conn.ID, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"New"}}}})
	if err != nil || len(v.Stale) > 0 {
		t.Fatalf("%+v, %v", v, err)
	}
	provider := countsOf(f.callsSince(before))
	wantProvider := map[string]int{"Edit": 1, "Structure": 1, "BoardRows": 2, "Cards": 1, "Details": 1, "Worklogs": 2}
	if !maps.Equal(provider, wantProvider) {
		t.Errorf("provider calls %s, want %s", describeCounts(provider), describeCounts(wantProvider))
	}
	if got := reads(); !slices.Equal(got, []string{"1:[10001 10002]", "2:[10001]"}) {
		t.Errorf("rows read %v", got)
	}
	if fmt.Sprint(cardReads) != "[[10009]]" {
		t.Errorf("cards read %v", cardReads)
	}
	if got := f.p.detailsRead()[details:]; !slices.Equal(got[0], ids("10003", "10002")) || len(got) != 1 {
		t.Errorf("details read %v", got)
	}
	if got := fmt.Sprint(f.w.time.reads); got != "[{acc-user1 2026-11-02 2026-11-03} {acc-user1 2026-12-20 2026-12-20}]" {
		t.Errorf("worklogs read %s", got)
	}
	stored := cs.take()
	wantStore := map[string]int{"ReadMirrorRefreshTargets": 1, "ReplaceMirrorStructure": 1, "WriteMirrorRows": 2,
		"UpsertMirrorCards": 1, "StoreMirrorTickets": 1, "ReplaceMirrorWorklogs": 2, "ListMirrorCards": 1, "GetMirrorCatalog": 1}
	if !maps.Equal(stored, wantStore) {
		t.Errorf("store calls %s, want %s", describeCounts(stored), describeCounts(wantStore))
	}
	logs := must[WorklogDays](t)(f.svc.Worklogs(f.conn.ID, "acc-user1", "2026-11-01", "2026-12-31"))
	if len(logs.Worklogs) != 2 || logs.Worklogs[0].ID != "w1" || logs.Worklogs[1].SpentSeconds != 7200 {
		t.Errorf("worklogs %+v", logs)
	}
}

// A rank of 50 issues is one rows read and one store write of their rows.
func TestARankOfFiftyIsOneRowsRead(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	var issues []string
	var res tracker.Result
	for i := range 50 {
		id := fmt.Sprint(20001 + i)
		issues = append(issues, id)
		f.state.set(testBoard, writeRow(id, 0, fmt.Sprintf("b%03d", i)))
		res.Parts = append(res.Parts, rankPart(testBoard, id, tracker.Applied()))
	}
	tickOK(t, f.svc, f.scope(), BoardDue{Delta: true})
	reads := f.state.recordRows(f.p)
	f.answer(res)
	cs := newCountingStore(f.st)
	f.svc.store = cs
	if _, err := f.svc.Rank(t.Context(), f.conn.ID, RankRequest{Board: "1", Issues: issues, Prev: "10003"}); err != nil {
		t.Fatal(err)
	}
	if got := reads(); len(got) != 1 {
		t.Fatalf("rows read %v", got)
	}
	if got := describeCounts(cs.take()); got != "ReadMirrorRefreshTargets:1 WriteMirrorRows:1" {
		t.Fatalf("store calls %s", got)
	}
}

// targetsStore records the size of each refresh targets read: the rows,
// kept issues, details and boards it returned.
type targetsStore struct {
	*countingStore
	mu   sync.Mutex
	read []string
}

func (s *targetsStore) ReadMirrorRefreshTargets(root int64, cards, details, boards []string) (store.MirrorRefreshTargets, error) {
	t, err := s.countingStore.ReadMirrorRefreshTargets(root, cards, details, boards)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.read = append(s.read, fmt.Sprintf("rows %d kept %d details %d boards %d", len(t.Rows), len(t.Kept), len(t.Details), len(t.Boards)))
	return t, err
}

func (s *targetsStore) takeRead() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.read
	s.read = nil
	return out
}

// The targets a refresh reads, pinned: a comment, which needs a detail
// alone, reads no row or kept issue of the issue on its two boards; an
// edit, which needs the card too, reads its row on each board.
func TestRefreshTargetsReadWhatEachResourceNeeds(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.openDetail(t, "10001")
	ts := &targetsStore{countingStore: newCountingStore(f.st)}
	f.svc.store = ts
	f.answer(tracker.Result{Parts: []tracker.Part{appliedPart("10001", tracker.StepComment, tracker.DetailOf("10001"))}})
	if v, err := f.svc.AddComment(t.Context(), f.conn.ID, CommentRequest{Issue: "10001", Markdown: "New comment"}); err != nil || len(v.Stale) > 0 {
		t.Fatalf("%+v, %v", v, err)
	}
	if got := ts.takeRead(); !slices.Equal(got, []string{"rows 0 kept 0 details 1 boards 0"}) {
		t.Errorf("a comment's targets: %v", got)
	}
	f.answer(tracker.Result{Parts: []tracker.Part{appliedPart("10001", tracker.StepEdit, tracker.CardOf("10001"), tracker.DetailOf("10001"))}})
	if v, err := f.svc.Edit(t.Context(), f.conn.ID, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"x"}}}}); err != nil || len(v.Stale) > 0 {
		t.Fatalf("%+v, %v", v, err)
	}
	if got := ts.takeRead(); !slices.Equal(got, []string{"rows 2 kept 1 details 1 boards 0"}) {
		t.Errorf("an edit's targets: %v", got)
	}
	if got := describeCounts(ts.take()); got != "ReadMirrorRefreshTargets:2 StoreMirrorTickets:2 WriteMirrorRows:2" {
		t.Errorf("store calls %s", got)
	}
}

// The cost of each write, pinned: the provider calls and the store calls
// of the write and its refresh, on a connection already held, with no
// board subscribed (a subscribed board adds the funnel's one lookup of a
// changed card's boards, pinned by the sync cost tests).
func TestWriteCosts(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.openDetail(t, "10002")
	must[TicketSubscription](t)(f.svc.SubscribeTicket(f.conn.ID, "10003"))
	f.events.issueEvents(t, 1)
	f.settledTickets(t)
	var mu sync.Mutex
	var next tracker.Result
	f.w.set(func(*scripted) {
		f.w.result = func(string, any) tracker.Result {
			mu.Lock()
			defer mu.Unlock()
			return next
		}
	})
	cs := newCountingStore(f.st)
	f.svc.store = cs
	write := func(res tracker.Result, change func(), do func() (WriteView, error)) func(t *testing.T) {
		return func(t *testing.T) {
			mu.Lock()
			next = res
			mu.Unlock()
			if change != nil {
				change()
			}
			if v, err := do(); err != nil || len(v.Stale) > 0 || v.Ended {
				t.Fatalf("%+v, %v", v, err)
			}
		}
	}
	u, d := tracker.UserID("acc-user1"), day(t, "2026-11-02")
	ctx, id := t.Context(), f.conn.ID
	type step = struct {
		name string
		run  func(t *testing.T)
		want costOf
	}
	costTable(t, f.mirrorFixture, cs, []step{
		{"edit of an issue on two boards", write(tracker.Result{Parts: []tracker.Part{appliedPart("10001", tracker.StepEdit, tracker.CardOf("10001"), tracker.DetailOf("10001"))}},
			func() { f.state.set(testBoard, writeRow("10001", 1, "a")) },
			func() (WriteView, error) {
				return f.svc.Edit(ctx, id, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"x"}}}})
			}),
			costOf{map[string]int{"Edit": 1, "BoardRows": 2}, map[string]int{"ReadMirrorRefreshTargets": 1, "WriteMirrorRows": 2}}},
		{"comment on a cached detail", write(tracker.Result{Parts: []tracker.Part{appliedPart("10002", tracker.StepComment, tracker.DetailOf("10002"))}},
			func() { f.state.comment("10002", "New comment") },
			func() (WriteView, error) {
				return f.svc.AddComment(ctx, id, CommentRequest{Issue: "10002", Markdown: "New comment"})
			}),
			costOf{map[string]int{"AddComment": 1, "Details": 1}, map[string]int{"ReadMirrorRefreshTargets": 1, "StoreMirrorTickets": 1}}},
		{"comment on an open ticket", write(tracker.Result{Parts: []tracker.Part{appliedPart("10003", tracker.StepComment, tracker.DetailOf("10003"))}},
			func() { f.state.comment("10003", "New comment") },
			func() (WriteView, error) {
				return f.svc.AddComment(ctx, id, CommentRequest{Issue: "10003", Markdown: "New comment"})
			}),
			costOf{map[string]int{"AddComment": 1, "Details": 1},
				map[string]int{"ReadMirrorRefreshTargets": 1, "StoreMirrorTickets": 1, "ListMirrorCards": 1, "GetMirrorCatalog": 1}}},
		{"comment on an issue neither open nor cached", write(tracker.Result{Parts: []tracker.Part{appliedPart("10001", tracker.StepComment, tracker.DetailOf("10001"))}}, nil,
			func() (WriteView, error) {
				return f.svc.AddComment(ctx, id, CommentRequest{Issue: "10001", Markdown: "New comment"})
			}),
			costOf{map[string]int{"AddComment": 1}, map[string]int{"ReadMirrorRefreshTargets": 1}}},
		{"sprint create", write(tracker.Result{Parts: []tracker.Part{{Step: tracker.StepSprint, Outcome: tracker.Applied(), Changed: []tracker.Resource{tracker.StructureOf(testBoard)}}}}, nil,
			func() (WriteView, error) {
				return f.svc.CreateSprint(ctx, id, SprintRequest{Board: "1", Name: "Sprint 3"})
			}),
			costOf{map[string]int{"CreateSprint": 1, "Structure": 1}, map[string]int{"ReadMirrorRefreshTargets": 1, "ReplaceMirrorStructure": 1}}},
		{"worklog add", write(tracker.Result{Parts: []tracker.Part{appliedPart("10001", tracker.StepWorklog, tracker.DetailOf("10001"), tracker.WorklogsOf(u, d))}}, nil,
			func() (WriteView, error) {
				return f.svc.AddWorklog(ctx, id, WorklogRequest{Issue: "10001", Day: "2026-11-02", SpentSeconds: 3600})
			}),
			costOf{map[string]int{"AddWorklog": 1, "Worklogs": 1}, map[string]int{"ReadMirrorRefreshTargets": 1, "ReplaceMirrorWorklogs": 1}}},
		{"create", write(tracker.Result{Parts: []tracker.Part{appliedPart("10005", tracker.StepCreate, tracker.CardOf("10005"), tracker.DetailOf("10005"))}},
			func() { f.state.set(testBoard, writeRow("10005", 1, "e")) },
			func() (WriteView, error) {
				return f.svc.Create(ctx, id, CreateRequest{Board: "1", Project: "10000", Title: "New"})
			}),
			costOf{map[string]int{"Create": 1, "NewRows": 1}, map[string]int{"ReadMirrorRefreshTargets": 1, "WriteMirrorRows": 1}}},
		{"create of unknown outcome", write(tracker.Result{Parts: []tracker.Part{{Step: tracker.StepCreate, Outcome: tracker.Unknown(fmt.Errorf("lost"))}}},
			func() { f.state.set(testBoard, writeRow("10006", 1, "f")) },
			func() (WriteView, error) {
				return f.svc.Create(ctx, id, CreateRequest{Board: "1", Project: "10000", Title: "New"})
			}),
			costOf{map[string]int{"Create": 1, "ChangedRows": 1}, map[string]int{"GetMirrorSync": 1, "WriteMirrorRowPage": 1, "RecordMirrorSyncAttempt": 1}}},
	})
}

// A refresh of an open ticket whose bind the store refuses does not
// refresh the ticket: the write reports its detail stale with the bind's
// failure, which the tracker did not cause.
func TestARefreshWhoseTicketBindFailsReportsItStale(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.refuseKeeps()
	must[TicketSubscription](t)(f.svc.SubscribeTicket(f.conn.ID, "10003"))
	f.events.issueEvents(t, 1)
	f.settledTickets(t)
	f.answer(tracker.Result{Parts: []tracker.Part{appliedPart("10003", tracker.StepComment, tracker.DetailOf("10003"))}})
	v, err := f.svc.AddComment(t.Context(), f.conn.ID, CommentRequest{Issue: "10003", Markdown: "New comment"})
	if err != nil {
		t.Fatal(err)
	}
	stale := slices.ContainsFunc(v.Stale, func(sv StaleView) bool {
		return sv.Failure.Kind == string(FailureInternal) && slices.ContainsFunc(sv.Resources, func(r ResourceView) bool {
			return r.Kind == "detail" && r.Issue == "10003"
		})
	})
	if !stale {
		t.Fatalf("the write after a refused bind = %+v; want the ticket's detail stale with the bind's failure", v)
	}
}
