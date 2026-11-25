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
			if out := must[Outcome](t)(m.svc.syncTick(t.Context(), scope, due, Background)); out.Failure != nil || out.Ended {
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

// plus is a and b's counts added.
func plus(a, b map[string]int) map[string]int {
	out := maps.Clone(a)
	for k, n := range b {
		out[k] += n
	}
	return out
}

func idsOfRows(rows []tracker.Row) []tracker.IssueID {
	out := make([]tracker.IssueID, len(rows))
	for i, r := range rows {
		out[i] = r.Card.ID
	}
	return out
}

// The stored size of each blob for a realistic record, pinned so a change
// that grows one shows. A Linear card stores its URL (the workspace, the
// identifier and a slug of the title) where a Jira card builds its own
// from the site and the key.
func TestBlobSizes(t *testing.T) {
	t.Parallel()
	c := realisticRow(1, time.UnixMilli(1_799_000_000_000).UTC()).Card
	card := must[store.MirrorCard](t)(mirrorCard(c))
	c.URL = "https://linear.app/example/issue/WEB-2/make-the-checkout-page-load-faster-part-1"
	linear := must[store.MirrorCard](t)(mirrorCard(c))
	catalog := must[store.MirrorCatalog](t)(mirrorCatalog(tracker.Catalog{Users: []tracker.User{{
		ID: "acc-user1", Name: "User 1", Email: "user1@example.com", AvatarURL: "https://avatar.example.com/acc-user1/48.png", Active: true,
	}}}))
	st := webStructure(testBoard)
	st.Estimate = tracker.Estimate{Unit: tracker.UnitPoints, Choices: []tracker.EstimateChoice{{Value: 1}, {Value: 2}, {Value: 3}, {Value: 5}, {Value: 8}}}
	board := must[store.MirrorStructure](t)(mirrorStructure(st, store.MirrorRead{}))
	body := must[[]byte](t)(mirrorBody(realisticDetail()))
	sizes := map[string]int{"card": len(card.Data), "linear card": len(linear.Data), "user": len(catalog.Users[0].Data), "board": len(board.Board.Data), "body": len(body)}
	t.Logf("blob bytes: %s", describeCounts(sizes))
	want := map[string]int{"card": 140, "linear card": 229, "user": 108, "board": 79, "body": 16468}
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
	// The tracker's card of the issue, which its detail shows.
	held := seeded[0].Card
	// It links three issues, whose cards a view shows from the mirror.
	m.p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
		d := realisticDetail()
		d.Card = held
		for i := range d.Links {
			d.Links[i].Issue.ID = tracker.IssueID(fmt.Sprint(30002 + i))
		}
		return d, nil
	}
	var asked []tracker.IssueID
	m.p.updatedMovesWithDetail(func(ids []tracker.IssueID) (tracker.Cards, error) {
		asked = ids
		return cardsAnswering(&held)(ids)
	})
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
			if out := must[Outcome](t)(m.svc.syncTick(t.Context(), scope, due, Background)); out.Failure != nil || out.Ended {
				t.Fatalf("%+v", out)
			}
		}
	}
	detail := func(t *testing.T) {
		if _, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, issue, Interactive); err != nil {
			t.Fatal(err)
		}
	}
	// Each check reads the issue's own card, not its links'.
	checked := func(t *testing.T) {
		t.Helper()
		detail(t)
		if want := []tracker.IssueID{issue}; !slices.Equal(asked, want) {
			t.Fatalf("the check read the cards of %v, want %v", asked, want)
		}
	}
	// Every detail answer reads the names it shows and the mirror's cards
	// of the issue, its parent and the issues it links, in one lookup.
	view := map[string]int{"GetMirrorCatalog": 1, "ListMirrorCards": 1}
	costTable(t, m, cs, []struct {
		name string
		run  func(t *testing.T)
		want costOf
	}{
		// 120 due: one listing, reads of 50, 50 and 20, each stored in one
		// write; the boards come with the listing.
		{"history, 120 due", tick(BoardDue{History: true}), costOf{
			map[string]int{"History": 3},
			with(map[string]int{"ListMirrorHistoryDue": 1, "ReplaceMirrorHistories": 3}),
		}},
		{"history, nothing due", tick(BoardDue{History: true}), costOf{
			map[string]int{},
			with(map[string]int{"ListMirrorHistoryDue": 1}),
		}},
		{"linked cards, none linked", func(t *testing.T) { must[Outcome](t)(m.svc.tickCards(t.Context(), m.conn.ID, Background)) }, costOf{
			map[string]int{},
			with(map[string]int{"ListMirrorOffBoardLinkedIssues": 1, "RecordMirrorSyncAttempt": 1}),
		}},
		{"detail, none stored", detail, costOf{
			map[string]int{"Details": 1},
			with(plus(view, map[string]int{"ListMirrorBodies": 1, "StoreMirrorTickets": 1})),
		}},
		// The mirror keeps none of the linked issues: they show what the
		// detail holds.
		{"detail, verified unchanged", checked, costOf{
			map[string]int{"Cards": 1},
			with(plus(view, map[string]int{"ListMirrorBodies": 1, "StoreMirrorTickets": 1})),
		}},
		// A read stored the three linked issues' cards, one renamed: the
		// view shows them from the mirror in its one cards lookup, and the
		// stored detail is not rewritten.
		{"detail, verified unchanged with 3 links", func(t *testing.T) {
			var linked []store.MirrorCard
			for i := range 3 {
				c := tracker.Card{ID: tracker.IssueID(fmt.Sprint(30002 + i)), Key: "WEB-3", Title: "Another task", Status: "s1", Updated: at}
				if i == 1 {
					c.Title = "Renamed"
				}
				linked = append(linked, must[store.MirrorCard](t)(mirrorCard(c)))
			}
			must[store.MirrorChanges](t)(m.st.UpsertMirrorCards(m.root(), m.sync("").Sweep, linked, store.MirrorCatalog{}, m.read()))
			v, ch, err := m.svc.IssueDetail(t.Context(), m.conn.ID, issue, Interactive)
			if err != nil || v.Links[1].Issue.Title != "Renamed" || len(ch.Details) != 0 || len(asked) != 1 {
				t.Fatalf("the links: %+v, changes %+v, cards of %v, %v", v.Links, ch, asked, err)
			}
		}, costOf{
			map[string]int{"Cards": 1},
			with(plus(view, map[string]int{"ListMirrorBodies": 1, "StoreMirrorTickets": 1})),
		}},
		{"detail, verified changed", func(t *testing.T) {
			held.Updated = held.Updated.Add(time.Minute)
			checked(t)
		}, costOf{
			map[string]int{"Cards": 1, "Details": 1},
			with(plus(view, map[string]int{"ListMirrorBodies": 1, "StoreMirrorTickets": 2})),
		}},
		// Sync stored a newer card than the stored detail's: no check.
		{"detail, the mirror already newer", func(t *testing.T) {
			held.Updated = held.Updated.Add(time.Minute)
			newer := must[store.MirrorCard](t)(mirrorCard(held))
			must[store.MirrorChanges](t)(m.st.UpsertMirrorCards(m.root(), m.sync("").Sweep, []store.MirrorCard{newer}, store.MirrorCatalog{}, m.read()))
			detail(t)
		}, costOf{
			map[string]int{"Details": 1},
			with(plus(view, map[string]int{"ListMirrorBodies": 1, "StoreMirrorTickets": 1})),
		}},
		{"detail, older than the age bound", func(t *testing.T) {
			sd, _, _ := m.storedDetail(t, string(issue))
			later := time.UnixMilli(sd.Wall).Add(detailMaxAge + time.Millisecond)
			m.svc.now = func() time.Time { return later }
			detail(t)
		}, costOf{
			map[string]int{"Details": 1},
			with(plus(view, map[string]int{"ListMirrorBodies": 1, "StoreMirrorTickets": 1})),
		}},
		// A person's refresh reads no stored detail first.
		{"detail, forced refresh", func(t *testing.T) {
			if _, _, err := m.svc.issueDetail(t.Context(), m.conn.ID, issue, Interactive, true); err != nil {
				t.Fatal(err)
			}
		}, costOf{
			map[string]int{"Details": 1},
			with(plus(view, map[string]int{"StoreMirrorTickets": 1})),
		}},
		// No run: no sync state read, no client.
		{"detail, the mirror's copy", func(t *testing.T) {
			if _, _, err := m.svc.mirroredDetail(m.conn.ID, issue); err != nil {
				t.Fatal(err)
			}
		}, costOf{
			map[string]int{},
			plus(view, map[string]int{"GetMirrorRoot": 1, "ListMirrorBodies": 1}),
		}},
		// A tracker whose update time does not move with its detail: read
		// every time, with no stored detail read first.
		{"detail, unverifiable", func(t *testing.T) {
			m.p.set(func(p *scripted) { p.caps.DetailMovesUpdated = false })
			detail(t)
		}, costOf{
			map[string]int{"Details": 1},
			with(plus(view, map[string]int{"StoreMirrorTickets": 1})),
		}},
		{"board view, three sections", func(t *testing.T) {
			must[BoardView](t)(m.svc.Board(scope, []Section{{Kind: SectionColumn}, {Kind: SectionColumn, Column: 1}, {Kind: SectionColumn, Column: 2}}))
		}, costOf{
			map[string]int{},
			// The structure, totals, pages, the names of the columns and
			// cards and their parents in one snapshot (6+3+2 statements in
			// the store).
			map[string]int{"GetMirrorRoot": 1, "ReadMirrorBoardView": 1, "GetMirrorSync": 1},
		}},
		{"section page", func(t *testing.T) {
			must[SectionPage](t)(m.svc.SectionRows(scope, Section{Kind: SectionColumn, Column: 1}, nil))
		}, costOf{
			map[string]int{},
			// The page with its names and parents in one snapshot (4
			// statements in the store).
			map[string]int{"GetMirrorRoot": 1, "ListMirrorSection": 1},
		}},
	})
}
