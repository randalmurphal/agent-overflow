package store

import (
	"encoding/binary"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// The store calls of a ticket round of 50 tickets, idle or finding 10
// changed, as trackerapp's TestIdleRoundCosts and TestChangedRoundCosts
// pin them, cost the statements, row writes and body bytes pinned here,
// and the WAL bytes each writes are measured beside them. Jira: one write
// stores the check's cards (the cards read by ID) with each ticket's
// record, and a second the changed tickets read in full. Linear: one write
// stores every ticket, card and body, read in full. Both then read the
// changed tickets' bodies, each with its card, and the catalog they name
// for their views, which are built from the record and show no other
// issue. Every
// ticket write brings the catalog its cards name, here 4 users and 3
// statuses (a Jira round's two writes each stamp them), and stamps each entry with its read (read_at), one row each
// whether or not it changed: the stamp orders the entry's later reads, so
// a read that began before this one cannot replace what this one found.
// Every ticket write also clears the absences its read supersedes
// (mirrorAbsencesClearSQL), one statement that stops at its guard when the
// root has none, and one that stored a card copies the stored cards to
// the links of their tickets (mirrorLinkCopySQL), one statement that stops
// at its guard when no thread has a link. An idle round writes each ticket's read times and the catalog's, and no
// body bytes: a body read again is compared by its digest. Every input is
// built before the round is counted.
func TestChangedRoundStatements(t *testing.T) {
	type cost struct {
		statements int
		writes     map[string]int
	}
	catalog := MirrorCatalog{}
	for i := range 4 {
		catalog.Users = append(catalog.Users, MirrorUser{ID: fmt.Sprint("acc-user", i+1), Name: fmt.Sprint("User ", i+1)})
	}
	for i := range 3 {
		catalog.Statuses = append(catalog.Statuses, MirrorStatus{ID: fmt.Sprint(i + 1), Name: fmt.Sprint("Status ", i+1), Group: MirrorGroupToDo})
	}
	stamped := map[string]int{"tracker_cache_users": 4, "tracker_cache_statuses": 3}
	pinned := map[string]cost{
		"a Jira round":         {30, map[string]int{"tracker_cache_issue_bodies": 10, "tracker_cache_issues": 60, "tracker_cache_users": 8, "tracker_cache_statuses": 6}},
		"a Linear round":       {22, map[string]int{"tracker_cache_issue_bodies": 10, "tracker_cache_issues": 60}},
		"an idle Jira round":   {9, map[string]int{"tracker_cache_issues": 50}},
		"an idle Linear round": {9, map[string]int{"tracker_cache_issues": 50}},
	}
	for name, want := range pinned {
		for table, n := range stamped {
			if _, set := want.writes[table]; !set {
				want.writes[table] = n
			}
		}
		pinned[name] = want
	}
	for _, name := range []string{"a Jira round", "a Linear round", "an idle Jira round", "an idle Linear round"} {
		jira, idle := name == "a Jira round" || name == "an idle Jira round", name == "an idle Jira round" || name == "an idle Linear round"
		t.Run(name, func(t *testing.T) {
			s, _, root := newMirror(t)
			mustReplaceStructure(t, s, root, webStructure(), true)
			mustWriteRowPage(t, s, root, webPage("10", mirrorRows(20001, 50, "z")...), nil)
			var tickets, changed []string
			for i := range 50 {
				tickets = append(tickets, fmt.Sprint(20001+i))
			}
			changed = tickets[:10]
			if idle {
				changed = nil
			}
			sweep := currentSweep(s, root)
			before := heldTickets(t, s, root, tickets, []byte(`{"v":1}`))
			if _, err := s.StoreMirrorTickets(root, sweep, before, MirrorCatalog{}, mirrorReadAt(7), mirrorReadAt(7)); err != nil {
				t.Fatal(err)
			}
			// The catalog as an earlier round stored it.
			if _, err := s.UpsertMirrorCards(root, sweep, nil, catalog, mirrorReadAt(8)); err != nil {
				t.Fatal(err)
			}
			// What the tracker answers: each changed card a minute newer with
			// a new title, and its new body; each ticket read in full brings
			// the evidence of its status time.
			read := make([]MirrorTicket, len(tickets))
			checks := make([]MirrorTicket, len(tickets))
			for i := range tickets {
				read[i] = before[i]
				if i < len(changed) {
					read[i].Card.UpdatedAt = 60_005
					read[i].Card.Data = []byte(`{"title":"Renamed ` + tickets[i] + `"}`)
					read[i].Body = []byte(`{"v":2}`)
				}
				c := read[i].Card
				read[i].Status = &MirrorStatusEvidence{Status: c.StatusID, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Complete: true, Final: true}
				checks[i] = MirrorTicket{Card: c}
			}
			recorder := recordMirrorWrites(t, s)
			each := map[string]int{}
			call := func(name string, fn func() error) {
				var err error
				each[name] += countMirrorStatements(t, func() { err = fn() })
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			}
			var bodyBytes int
			store := func(tickets []MirrorTicket, at int64) func() error {
				return func() error {
					w, err := s.StoreMirrorTickets(root, sweep, tickets, catalog, MirrorRead{}, mirrorReadAt(at))
					for i, tw := range w.Tickets {
						if tw.BodyChanged {
							bodyBytes += len(tickets[i].Body)
						}
					}
					return err
				}
			}
			if _, err := s.TruncateCheckpoint(); err != nil {
				t.Fatal(err)
			}
			statements := countMirrorStatements(t, func() {
				call("GetMirrorSync", func() error { _, _, err := s.GetMirrorSync(root, "", testNow); return err })
				if jira {
					call("StoreMirrorTickets", store(checks, 100))
					if !idle {
						call("StoreMirrorTickets", store(read[:len(changed)], 101))
					}
				} else {
					call("StoreMirrorTickets", store(read, 100))
				}
				if !idle {
					call("ListMirrorBodies", func() error { _, err := s.ListMirrorBodies(root, changed); return err })
					call("GetMirrorCatalog", func() error {
						_, err := s.GetMirrorCatalog(root, []string{"acc-user1"}, []string{"1"})
						return err
					})
				}
			})
			wal := walSize(t, s.path)
			writes := recorder.rowWrites()
			t.Logf("%s, %d of 50 changed: %d statements (%v), rows written %v, %d body bytes, %d WAL bytes", name, len(changed), statements, each,
				writes, bodyBytes, wal)
			if want := pinned[name]; statements != want.statements || !maps.Equal(writes, want.writes) {
				t.Errorf("%s, %d of 50 changed: %d statements, rows written %v; pinned %d and %v", name, len(changed), statements, writes, want.statements, want.writes)
			}
			if want := 7 * len(changed); bodyBytes != want {
				t.Errorf("%s wrote %d body bytes; want %d, the changed bodies'", name, bodyBytes, want)
			}
		})
	}
}

// The ticket move (mirrorTicketMoveSQL) finds each ticket's row by key
// from the list it parses once, so its VM steps grow linearly with the
// tickets: an idle round (the read times alone, .reads) and an opening
// round (the opening too, .full) of 50, 200 and 500 tickets each run it
// once, and each ticket from 200 to 500 costs at most 5/4 of what each
// from 50 to 200 does, where a lookup that scans the list per row costs
// each ticket in proportion to the list. The root's other issues cost
// nothing: 50 tickets on a root of 2,000 issues cost at most 11/10 of 50
// on a root of 50, where a search of the root's rows costs each of them.
func TestTheTicketMoveGrowsLinearly(t *testing.T) {
	sizes := []int{50, 200, 500}
	type cost struct{ reads, full int }
	measure := func(n, held int) cost {
		s, _, root := newMirror(t)
		mustReplaceStructure(t, s, root, webStructure(), true)
		for page := range slices.Chunk(mirrorRows(20001, held, "z"), mirrorMaxPageRows) {
			mustWriteRowPage(t, s, root, webPage("10", page...), nil)
		}
		issues := make([]string, n)
		for i := range issues {
			issues[i] = fmt.Sprint(20001 + i)
		}
		sweep := currentSweep(s, root)
		tickets := heldTickets(t, s, root, issues, []byte(`{"v":1}`))
		for i := range tickets {
			c := tickets[i].Card
			tickets[i].Status = &MirrorStatusEvidence{Status: c.StatusID, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Complete: true, Final: true}
		}
		if _, err := s.StoreMirrorTickets(root, sweep, tickets, MirrorCatalog{}, mirrorReadAt(7), mirrorReadAt(7)); err != nil {
			t.Fatal(err)
		}
		var c cost
		for _, step := range []struct {
			query          string
			steps          *int
			opened, readAt int64
		}{{mirrorTicketMoveSQL.reads, &c.reads, 0, 100}, {mirrorTicketMoveSQL.full, &c.full, 200, 200}} {
			opened := MirrorRead{}
			if step.opened > 0 {
				opened = mirrorReadAt(step.opened)
			}
			var err error
			runs, steps := countStatementRuns(t, step.query, func() {
				_, err = s.StoreMirrorTickets(root, sweep, tickets, MirrorCatalog{}, opened, mirrorReadAt(step.readAt))
			})
			if err != nil || runs != 1 {
				t.Fatalf("%d tickets: the move ran %d times, %v; want once", n, runs, err)
			}
			*step.steps = steps
		}
		return c
	}
	costs := make([]cost, len(sizes))
	for i, n := range sizes {
		costs[i] = measure(n, n)
	}
	wide := measure(50, 2000)
	t.Logf("the ticket move's VM steps at %v tickets: %+v; 50 tickets of 2,000 issues: %+v", sizes, costs, wide)
	if 10*wide.reads > 11*costs[0].reads || 10*wide.full > 11*costs[0].full {
		t.Fatalf("50 tickets on a root of 2,000 issues cost %+v, on a root of 50 %+v; want the root's other issues to cost nothing", wide, costs[0])
	}
	for _, kind := range []struct {
		name string
		of   func(cost) int
	}{{"reads", func(c cost) int { return c.reads }}, {"full", func(c cost) int { return c.full }}} {
		low := float64(kind.of(costs[1])-kind.of(costs[0])) / float64(sizes[1]-sizes[0])
		high := float64(kind.of(costs[2])-kind.of(costs[1])) / float64(sizes[2]-sizes[1])
		if low <= 0 || high > 1.25*low {
			t.Fatalf("the %s move's steps per ticket: %.0f from 50 to 200, %.0f from 200 to 500; want linear", kind.name, low, high)
		}
	}
}

// The history times write (mirrorHistoryTimesSQL) finds each history's
// issue by key from the list it parses once, as the ticket move does: one
// write of 10, 25 and 50 histories (mirrorHistoryBatch) runs it once,
// each history from 25 to 50 costs at most 5/4 of what each from 10 to 25
// does, and 50 histories on a root of 2,000 issues cost at most 11/10 of
// 50 on a root of 50.
func TestTheHistoryTimesWriteGrowsLinearly(t *testing.T) {
	measure := func(n, held int) int {
		s, _, root := newMirror(t)
		mustReplaceStructure(t, s, root, webStructure(), true)
		for page := range slices.Chunk(mirrorRows(20001, held, "z"), mirrorMaxPageRows) {
			mustWriteRowPage(t, s, root, webPage("10", page...), nil)
		}
		histories := make([]MirrorHistory, n)
		for i := range histories {
			histories[i] = MirrorHistory{IssueID: fmt.Sprint(20001 + i), AnsweredAt: testNow}
		}
		histories = dueHistories(s, root, histories)
		var err error
		runs, steps := countStatementRuns(t, mirrorHistoryTimesSQL, func() {
			_, _, err = s.ReplaceMirrorHistories(root, currentSweep(s, root), histories, MirrorCatalog{}, mirrorReadAt(9))
		})
		if err != nil || runs != 1 {
			t.Fatalf("%d histories: the times write ran %d times, %v; want once", n, runs, err)
		}
		return steps
	}
	sizes := []int{10, 25, mirrorHistoryBatch}
	steps := make([]int, len(sizes))
	for i, n := range sizes {
		steps[i] = measure(n, mirrorHistoryBatch)
	}
	wide := measure(mirrorHistoryBatch, 2000)
	t.Logf("the history times write's VM steps at %v histories: %v; %d on a root of 2,000: %d", sizes, steps, mirrorHistoryBatch, wide)
	low := float64(steps[1]-steps[0]) / float64(sizes[1]-sizes[0])
	high := float64(steps[2]-steps[1]) / float64(sizes[2]-sizes[1])
	if low <= 0 || high > 1.25*low {
		t.Fatalf("the times write's steps per history: %.0f from 10 to 25, %.0f from 25 to 50; want linear", low, high)
	}
	if 10*wide > 11*steps[2] {
		t.Fatalf("%d histories on a root of 2,000 issues cost %d steps, on a root of 50 %d; want the root's other issues to cost nothing", mirrorHistoryBatch, wide, steps[2])
	}
}

// walPagesByTree is how many frames s's WAL holds of each b-tree (dbstat):
// the pages the writes since the last checkpoint rewrote.
func walPagesByTree(t *testing.T, s *Store) map[string]int {
	t.Helper()
	data, err := os.ReadFile(s.path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	names := map[uint32]string{}
	rows, err := s.db.Query(`SELECT name, pageno FROM dbstat`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		var page uint32
		if err := rows.Scan(&name, &page); err != nil {
			t.Fatal(err)
		}
		names[page] = name
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	pages := map[string]int{}
	if len(data) < 32 {
		return pages
	}
	size := int(binary.BigEndian.Uint32(data[8:12]))
	for off := 32; off+24+size <= len(data); off += 24 + size {
		pages[names[binary.BigEndian.Uint32(data[off:off+4])]]++
	}
	return pages
}

// An idle ticket round at a real size (50 open tickets spread over a root
// of 2,000 issues, each card blob the size TestBlobSizes measures for its
// tracker) rewrites one issue page per open ticket, for its read times,
// and one page of each catalog table, for their stamps; no page of the
// bodies' index (mirrorTicketMoveSQL.reads), and no body.
func TestAnIdleRoundRewritesOnePagePerOpenTicket(t *testing.T) {
	t.Parallel()
	catalog := MirrorCatalog{}
	for i := range 4 {
		catalog.Users = append(catalog.Users, MirrorUser{ID: fmt.Sprint("acc-user", i+1), Name: fmt.Sprint("User ", i+1)})
	}
	for i := range 3 {
		catalog.Statuses = append(catalog.Statuses, MirrorStatus{ID: fmt.Sprint(i + 1), Name: fmt.Sprint("Status ", i+1), Group: MirrorGroupToDo})
	}
	for name, size := range map[string]int{"jira": 211, "linear": 300} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, _, root := newMirror(t)
			mustReplaceStructure(t, s, root, webStructure(), true)
			rows := mirrorRows(20001, 2000, "z")
			for i := range rows {
				head := fmt.Sprintf(`{"title":"Issue %s","pad":"`, rows[i].Card.IssueID)
				rows[i].Card.Data = []byte(head + strings.Repeat("x", size-len(head)-2) + `"}`)
			}
			for page := range slices.Chunk(rows, mirrorMaxPageRows) {
				mustWriteRowPage(t, s, root, webPage("10", page...), nil)
			}
			var issues []string
			for i := range 50 {
				issues = append(issues, fmt.Sprint(20001+40*i))
			}
			sweep := currentSweep(s, root)
			tickets := heldTickets(t, s, root, issues, []byte(`{"v":1}`))
			for i := range tickets {
				c := tickets[i].Card
				tickets[i].Status = &MirrorStatusEvidence{Status: c.StatusID, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Complete: true, Final: true}
			}
			if _, err := s.StoreMirrorTickets(root, sweep, tickets, catalog, mirrorReadAt(7), mirrorReadAt(7)); err != nil {
				t.Fatal(err)
			}
			// The round: Jira checks the cards, Linear reads every ticket in
			// full; nothing changed.
			round := tickets
			if name == "jira" {
				round = make([]MirrorTicket, len(tickets))
				for i := range tickets {
					round[i] = MirrorTicket{Card: tickets[i].Card}
				}
			}
			if _, err := s.TruncateCheckpoint(); err != nil {
				t.Fatal(err)
			}
			w, err := s.StoreMirrorTickets(root, sweep, round, catalog, MirrorRead{}, mirrorReadAt(100))
			if err != nil {
				t.Fatal(err)
			}
			for _, tw := range w.Tickets {
				if tw.BodyChanged || tw.CardChanged || tw.StatusChanged {
					t.Fatalf("an idle round changed a ticket: %+v", tw)
				}
			}
			want := map[string]int{"tracker_cache_issues": 50, "tracker_cache_users": 1, "tracker_cache_statuses": 1}
			if got := walPagesByTree(t, s); !maps.Equal(got, want) {
				t.Errorf("the idle round rewrote pages %v; want %v", got, want)
			}
		})
	}
}
