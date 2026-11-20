package trackerapp

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The provider calls and store calls of each unit, pinned. A store call is
// one write transaction or one read snapshot.

// costOf is a pinned cost: provider calls and store calls by name.
type costOf struct {
	provider map[string]int
	store    map[string]int
}

func countsOf(calls []string) map[string]int {
	out := map[string]int{}
	for _, c := range calls {
		out[c]++
	}
	return out
}

func describeCounts(m map[string]int) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s:%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

// costTable runs each step on m, whose store counts its calls, and fails
// when a step's calls differ from its pin.
func costTable(t *testing.T, m *mirrorFixture, cs *countingStore, steps []struct {
	name string
	run  func(t *testing.T)
	want costOf
}) {
	t.Helper()
	for _, st := range steps {
		before := len(m.p.callsInOrder())
		cs.take()
		st.run(t)
		provider := countsOf(m.p.callsInOrder()[before:])
		stored := cs.take()
		t.Logf("%-34s provider %-30s store %s", st.name, describeCounts(provider), describeCounts(stored))
		if !maps.Equal(provider, st.want.provider) || !maps.Equal(stored, st.want.store) {
			t.Errorf("%s: provider %s, store %s; pinned provider %s, store %s", st.name,
				describeCounts(provider), describeCounts(stored), describeCounts(st.want.provider), describeCounts(st.want.store))
		}
	}
}

func TestUnitCosts(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	cs := newCountingStore(m.st)
	m.svc.store = cs
	scope := m.scope()
	at := time.UnixMilli(1_799_000_000_000).UTC()
	page := realisticRows(0, readBatch, at)
	page100 := func(rows []tracker.Row) func(tracker.DeltaQuery) (tracker.RowPage, error) {
		return func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{Rows: rows, Next: cursorAfter("x"), Catalog: webStructure(testBoard).Catalog}, nil
		}
	}
	tick := func(due BoardDue) func(t *testing.T) {
		return func(t *testing.T) {
			if out := must[Outcome](t)(m.svc.SyncBoard(t.Context(), scope, due, Background)); out.Failure != nil || out.Ended {
				t.Fatalf("%+v", out)
			}
		}
	}
	// Every tick reads the scope's sync state; the first tick of a
	// connection (structure here) also reads what the service then holds:
	// the connection, its Tempo connection (a Jira connection's provider)
	// and the mirror root.
	with := func(extra map[string]int) map[string]int {
		out := map[string]int{"GetMirrorSync": 1}
		maps.Copy(out, extra)
		return out
	}
	var full [][]tracker.Row
	for p := range deltaPages + 1 {
		full = append(full, realisticRows(1000+p*readBatch, readBatch, at))
	}
	listed := idsOfRows(page)
	costTable(t, m, cs, []struct {
		name string
		run  func(t *testing.T)
		want costOf
	}{
		{"structure", tick(BoardDue{Structure: true}), costOf{
			map[string]int{"Structure": 1},
			with(map[string]int{"GetTrackerConnection": 1, "ListTrackerConnections": 1, "GetOrCreateMirrorRoot": 1, "ReplaceMirrorStructure": 1}),
		}},
		{"delta, a page of 100 new rows", func(t *testing.T) {
			m.p.set(func(p *scripted) { p.changedRows = page100(page) })
			tick(BoardDue{Delta: true})(t)
		}, costOf{
			map[string]int{"ChangedRows": 1},
			with(map[string]int{"GetMirrorBoard": 1, "WriteMirrorRowPage": 1, "RecordMirrorSyncAttempt": 1}),
		}},
		{"delta, the same page again", func(t *testing.T) {
			before := must[Outcome](t)(m.svc.tickDelta(t.Context(), scope, Background))
			if !before.Changes().Empty() {
				t.Fatalf("an identical page changed %+v", before.Changes())
			}
		}, costOf{
			map[string]int{"ChangedRows": 1},
			with(map[string]int{"GetMirrorSync": 1, "WriteMirrorRowPage": 1, "RecordMirrorSyncAttempt": 1}),
		}},
		{"delta, one changed issue", func(t *testing.T) {
			changed := slices.Clone(page)
			changed[3] = realisticRow(3, at.Add(time.Hour))
			m.p.set(func(p *scripted) { p.changedRows = page100(changed) })
			if out := must[Outcome](t)(m.svc.tickDelta(t.Context(), scope, Background)); !slices.Equal(out.Changes().Cards, ids(string(changed[3].Card.ID))) {
				t.Fatalf("changed %+v", out.Changes())
			}
		}, costOf{
			map[string]int{"ChangedRows": 1},
			with(map[string]int{"WriteMirrorRowPage": 1, "RecordMirrorSyncAttempt": 1}),
		}},
		{"delta, more pages than its bound", func(t *testing.T) {
			walk, known := pagedDelta(t, full...), map[string]bool{}
			for _, pg := range full {
				known[string(cursorAfter(string(pg[len(pg)-1].Card.ID)))] = true
			}
			m.p.set(func(p *scripted) {
				p.changedRows = func(q tracker.DeltaQuery) (tracker.RowPage, error) {
					if !known[string(q.From)] {
						q.From = nil
					}
					return walk(q)
				}
			})
			if out := must[Outcome](t)(m.svc.tickDelta(t.Context(), scope, Background)); !out.More() {
				t.Fatal("no More")
			}
		}, costOf{
			map[string]int{"ChangedRows": deltaPages},
			with(map[string]int{"WriteMirrorRowPage": deltaPages, "RecordMirrorSyncAttempt": 1}),
		}},
		{"reconcile, 100 held", func(t *testing.T) {
			m.p.set(func(p *scripted) {
				p.boardIssues = func(tracker.BoardID, time.Time) (tracker.Listing, error) {
					return tracker.Listing{IDs: listed, Complete: false}, nil
				}
			})
			tick(BoardDue{Reconcile: true})(t)
		}, costOf{
			map[string]int{"BoardIssues": 1},
			with(map[string]int{"ReconcileMirrorBoard": 1, "RecordMirrorSyncAttempt": 1}),
		}},
		{"reconcile, 150 missing", func(t *testing.T) {
			missing := realisticRows(5000, 150, at)
			m.p.set(func(p *scripted) {
				p.boardIssues = func(tracker.BoardID, time.Time) (tracker.Listing, error) {
					return tracker.Listing{IDs: append(slices.Clone(listed), idsOfRows(missing)...), Complete: false}, nil
				}
				p.boardRows = func(_ tracker.BoardID, asked []tracker.IssueID) (tracker.Rows, error) {
					var out tracker.Rows
					for _, r := range missing {
						if slices.Contains(asked, r.Card.ID) {
							out.Rows = append(out.Rows, r)
						}
					}
					return out, nil
				}
			})
			tick(BoardDue{Reconcile: true})(t)
		}, costOf{
			map[string]int{"BoardIssues": 1, "BoardRows": 2},
			with(map[string]int{"ReconcileMirrorBoard": 1, "WriteMirrorRows": 2, "RecordMirrorSyncAttempt": 1}),
		}},
	})
}

func idsOfRows(rows []tracker.Row) []tracker.IssueID {
	out := make([]tracker.IssueID, len(rows))
	for i, r := range rows {
		out[i] = r.Card.ID
	}
	return out
}

// The stored size of each blob for a realistic record, pinned so a change
// that grows one shows.
func TestBlobSizes(t *testing.T) {
	t.Parallel()
	card := must[store.MirrorCard](t)(mirrorCard(realisticRow(1, time.UnixMilli(1_799_000_000_000).UTC()).Card))
	catalog := must[store.MirrorCatalog](t)(mirrorCatalog(tracker.Catalog{Users: []tracker.User{{
		ID: "acc-user1", Name: "User 1", Email: "user1@example.com", AvatarURL: "https://avatar.example.com/acc-user1/48.png", Active: true,
	}}}))
	st := webStructure(testBoard)
	st.Estimate = tracker.Estimate{Unit: tracker.UnitPoints, Choices: []tracker.EstimateChoice{{Value: 1}, {Value: 2}, {Value: 3}, {Value: 5}, {Value: 8}}}
	board := must[store.MirrorStructure](t)(mirrorStructure(st, 1))
	detail := must[[]byte](t)(mirrorDetail(realisticDetail()))
	sizes := map[string]int{"card": len(card.Data), "user": len(catalog.Users[0].Data), "board": len(board.Board.Data), "detail": len(detail)}
	t.Logf("blob bytes: %s", describeCounts(sizes))
	want := map[string]int{"card": 211, "user": 108, "board": 79, "detail": 16582}
	if !maps.Equal(sizes, want) {
		t.Errorf("blob bytes %s, pinned %s", describeCounts(sizes), describeCounts(want))
	}
}

func TestHistoryDetailAndViewCosts(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	scope := m.scope()
	at := time.UnixMilli(1_799_000_000_000).UTC()
	seeded := realisticRows(0, 120, at)
	m.p.changedRows = pagedDelta(t, seeded[:100], seeded[100:])
	must[Outcome](t)(m.svc.tickDelta(t.Context(), scope, Background))
	m.p.history = func(tracker.HistoryQuery) (tracker.History, error) { return tracker.History{}, nil }
	issue := seeded[0].Card.ID
	m.p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
		d := realisticDetail()
		d.Card = seeded[0].Card
		return d, nil
	}
	cs := newCountingStore(m.st)
	m.svc.store = cs
	// The service holds the connection from the seeding tick: a tick or a
	// read through reads the scope's sync state and nothing of the
	// connection.
	with := func(extra map[string]int) map[string]int {
		out := map[string]int{"GetMirrorSync": 1}
		maps.Copy(out, extra)
		return out
	}
	tick := func(due BoardDue) func(t *testing.T) {
		return func(t *testing.T) {
			if out := must[Outcome](t)(m.svc.SyncBoard(t.Context(), scope, due, Background)); out.Failure != nil || out.Ended {
				t.Fatalf("%+v", out)
			}
		}
	}
	detail := func(t *testing.T) {
		if _, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, issue, Interactive); err != nil {
			t.Fatal(err)
		}
	}
	costTable(t, m, cs, []struct {
		name string
		run  func(t *testing.T)
		want costOf
	}{
		// 120 due: two listing pages (100, 20), reads of 50, 50 and 20, each
		// stored in one write; the boards come with the listing.
		{"history, 120 due", tick(BoardDue{History: true}), costOf{
			map[string]int{"History": 3},
			with(map[string]int{"ListMirrorHistoryDue": 2, "ReplaceMirrorHistories": 3}),
		}},
		{"history, nothing due", tick(BoardDue{History: true}), costOf{
			map[string]int{},
			with(map[string]int{"ListMirrorHistoryDue": 1}),
		}},
		{"linked cards, none linked", func(t *testing.T) { must[Outcome](t)(m.svc.tickCards(t.Context(), m.conn.ID, Background)) }, costOf{
			map[string]int{},
			with(map[string]int{"ListMirrorOffBoardLinkedIssues": 1, "RecordMirrorSyncAttempt": 1}),
		}},
		{"detail, read through", detail, costOf{
			map[string]int{"Detail": 1},
			with(map[string]int{"GetMirrorRoot": 1, "GetMirrorDetail": 1, "StoreMirrorDetail": 1, "GetMirrorCatalog": 1, "ListMirrorCards": 1}),
		}},
		// No run: no sync state read, no client.
		{"detail, from the mirror", detail, costOf{
			map[string]int{},
			map[string]int{"GetMirrorRoot": 1, "GetMirrorDetail": 1, "ListMirrorCards": 2, "TouchMirrorDetail": 1, "GetMirrorCatalog": 1},
		}},
		{"board view, three sections", func(t *testing.T) {
			must[BoardView](t)(m.svc.Board(scope, []Section{{Kind: SectionColumn}, {Kind: SectionColumn, Column: 1}, {Kind: SectionColumn, Column: 2}}))
		}, costOf{
			map[string]int{},
			// The structure, totals and pages in one snapshot (6+3 statements
			// in the store), the names of the columns and cards in one
			// catalog read, the parents in one card read.
			map[string]int{"GetMirrorRoot": 1, "ReadMirrorBoardView": 1, "GetMirrorSync": 1, "GetMirrorCatalog": 1, "ListMirrorCards": 1},
		}},
		{"section page", func(t *testing.T) {
			must[SectionPage](t)(m.svc.SectionRows(scope, Section{Kind: SectionColumn, Column: 1}, nil))
		}, costOf{
			map[string]int{},
			map[string]int{"GetMirrorRoot": 1, "ListMirrorSection": 1, "GetMirrorCatalog": 1, "ListMirrorCards": 1},
		}},
	})
}

// A detail served from the mirror builds no client and makes no call.
func TestCachedDetailBuildsNoClient(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
		return tracker.Detail{Card: card("10001", 1_799_000_000_000)}, nil
	}
	if _, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive); err != nil {
		t.Fatal(err)
	}
	svc := must[*Service](t)(New(Deps{
		Store: m.st, Changed: func() {}, CredentialsDir: m.creds, Origin: APIOrigin, Version: "1", Now: time.Now, Life: t.Context(),
	}))
	svc.makeProvider = func(store.TrackerConnection, *clients, *clients) (tracker.Provider, error) {
		t.Error("a provider was built for a detail the mirror holds")
		return m.p, nil
	}
	if _, _, err := svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if len(svc.clients) != 0 || m.p.count("Detail") != 1 {
		t.Fatalf("%d clients, %d detail reads", len(svc.clients), m.p.count("Detail"))
	}
}

// A detail keeps its newest maxDetailEvents events and stays complete
// from its HistoryFrom.
func TestDetailHistoryIsBounded(t *testing.T) {
	t.Parallel()
	base := time.UnixMilli(1_799_000_000_000).UTC()
	var d tracker.Detail
	for i := range maxDetailEvents + 100 {
		// Pairs of events share a time; the newest come first, as a tracker
		// may list them.
		d.History = append(d.History, tracker.Event{At: base.Add(time.Duration((maxDetailEvents+100-i)/2) * time.Minute), Field: fmt.Sprint(i)})
	}
	all := slices.Clone(d.History)
	boundHistory(&d)
	if len(d.History) > maxDetailEvents || len(d.History) < maxDetailEvents-1 {
		t.Fatalf("%d events kept", len(d.History))
	}
	from := 0
	for _, e := range all {
		if !e.At.Before(d.HistoryFrom) {
			from++
		}
	}
	if from != len(d.History) {
		t.Fatalf("%d events at or after %v, %d kept", from, d.HistoryFrom, len(d.History))
	}
	for i, e := range d.History {
		if e.At.Before(d.HistoryFrom) || (i > 0 && e.At.Before(d.History[i-1].At)) {
			t.Fatalf("event %d at %v, history from %v", i, e.At, d.HistoryFrom)
		}
	}
	if !d.HistoryFrom.After(base) {
		t.Fatalf("history from %v", d.HistoryFrom)
	}
	short := tracker.Detail{History: []tracker.Event{{At: base}}}
	if boundHistory(&short); len(short.History) != 1 || !short.HistoryFrom.IsZero() {
		t.Fatalf("a short history changed: %+v", short)
	}
}

// A detail read through keeps and shows at most maxDetailEvents events.
func TestDetailReadBoundsItsHistory(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	base := time.UnixMilli(1_798_000_000_000).UTC()
	m.p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
		d := tracker.Detail{Card: card("10001", 1_799_000_000_000)}
		for i := range maxDetailEvents + 10 {
			d.History = append(d.History, tracker.Event{At: base.Add(time.Duration(i) * time.Minute), Field: "status"})
		}
		return d, nil
	}
	for range 2 {
		v, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
		if err != nil || len(v.History) != maxDetailEvents || v.HistoryFrom != millis(base.Add(10*time.Minute)) {
			t.Fatalf("%d events from %d, %v", len(v.History), v.HistoryFrom, err)
		}
	}
	if m.p.count("Detail") != 1 {
		t.Fatalf("%d detail reads", m.p.count("Detail"))
	}
}
