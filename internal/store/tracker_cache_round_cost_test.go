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
// changed tickets' cards and the catalog they name for their views. Every
// ticket write brings the catalog its cards name, here 4 users and 3
// statuses (a Jira round's two writes each stamp them), and stamps each entry with its read (read_at), one row each
// whether or not it changed: the stamp orders the entry's later reads, so
// a read that began before this one cannot replace what this one found.
// An idle round writes each ticket's read times and the catalog's, and no
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
		"a Jira round":         {28, map[string]int{"tracker_cache_issue_bodies": 10, "tracker_cache_issues": 60, "tracker_cache_users": 8, "tracker_cache_statuses": 6}},
		"a Linear round":       {21, map[string]int{"tracker_cache_issue_bodies": 10, "tracker_cache_issues": 60}},
		"an idle Jira round":   {8, map[string]int{"tracker_cache_issues": 50}},
		"an idle Linear round": {8, map[string]int{"tracker_cache_issues": 50}},
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
					call("ListMirrorCards", func() error { _, err := s.ListMirrorCards(root, changed); return err })
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
