package store

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// creditReadColumn is a column of an issue's own rows that the credit
// fragment reads: its table and name.
type creditReadColumn struct{ table, column string }

// creditReadColumns is every column of a table keyed by issue that the
// credit fragment reads, as its SQL names them: each table it reads under
// an alias, and each alias.column of a table with an issue_id column,
// but for its key columns, which change only as rows come and go (the
// test's check of each write's credit rows covers those). It reads the
// statement, so a column credit starts reading is listed here with no
// edit.
func creditReadColumns(t *testing.T, s *Store) []creditReadColumn {
	t.Helper()
	with, rows := mirrorCreditSQL(mirrorExportCreditParams, "", func(string) string { return "1" }, true)
	text := with + "\n" + rows
	aliases := map[string]string{}
	for _, m := range regexp.MustCompile(`\b(tracker_cache_\w+)\s+([a-z][a-z0-9_]*)\b`).FindAllStringSubmatch(text, -1) {
		switch strings.ToUpper(m[2]) {
		case "ON", "WHERE", "AND", "OR", "JOIN", "CROSS", "LEFT", "GROUP", "ORDER", "SET", "AS":
			continue
		}
		aliases[m[2]] = m[1]
	}
	seen := map[creditReadColumn]bool{}
	var out []creditReadColumn
	for alias, table := range aliases {
		cols, err := tableColumns(s.db, table)
		if err != nil || len(cols) == 0 {
			t.Fatalf("columns of %s: %v, %v", table, cols, err)
		}
		keys, err := queryIDs(s.db, `SELECT name FROM pragma_table_info(?) WHERE pk > 0`, table)
		if err != nil {
			t.Fatal(err)
		}
		if !cols["issue_id"] {
			continue
		}
		for _, m := range regexp.MustCompile(`\b`+regexp.QuoteMeta(alias)+`\.([a-z_][a-z0-9_]*)\b`).FindAllStringSubmatch(text, -1) {
			c := creditReadColumn{table, m[1]}
			if !cols[m[1]] || slices.Contains(keys, m[1]) || seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b creditReadColumn) int { return strings.Compare(a.table+"."+a.column, b.table+"."+b.column) })
	return out
}

// issueColumnValues is, for each column of cols, every value issue's rows
// of its table hold, keyed by the row's primary key.
func issueColumnValues(t *testing.T, s *Store, root int64, issue string, cols []creditReadColumn) map[creditReadColumn]string {
	t.Helper()
	out := map[creditReadColumn]string{}
	for _, c := range cols {
		key, err := queryIDs(s.db, `SELECT name FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk`, c.table)
		if err != nil {
			t.Fatal(err)
		}
		for i, k := range key {
			key[i] = "quote(" + k + ")"
		}
		var v *string
		q := fmt.Sprintf(`SELECT group_concat(k || '=' || v, ';') FROM (SELECT %s AS k, quote(%s) AS v FROM %s WHERE root_id = ? AND issue_id = ? ORDER BY k)`,
			strings.Join(key, ` || ',' || `), c.column, c.table)
		if err := s.db.QueryRow(q, root, issue).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v != nil {
			out[c] = *v
		}
	}
	return out
}

// Every write that changes what credit counts of a ticket names the
// ticket in its report, which the sync passes to the boards holding it:
// for each column of the ticket's rows that credit reads, a write here
// changes it on a row the ticket held already, and each write that
// changed the ticket's credit rows reported the ticket. A column credit
// starts reading fails until a write that changes it is added, and then
// until its writer reports it.
func TestEveryWriteThatChangesATicketsCreditReportsIt(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	regroup := creditRegroup(t, creditRules(false))
	cols := creditReadColumns(t, s)
	card := func(updated int64, edit ...func(*MirrorRow)) func() bool {
		return func() bool {
			r := sectionRow("10007", "10007", "10001", 3, "20")
			r.Card.UpdatedAt, r.Card.ResolvedAt, r.Card.AssigneeID = updated, 400, "acc-user2"
			r.Card.Data = []byte(fmt.Sprintf(`{"summary":"edited %d"}`, updated))
			for _, e := range edit {
				e(&r)
			}
			ch := mustWriteRowPage(t, s, root, webPage("10", r), nil)
			return slices.Contains(ch.Cards, "10007") || slices.Contains(ch.Rows, "10007")
		}
	}
	changes := []MirrorChange{{Kind: MirrorChangeStatus, At: 400, From: "1", To: "10001"},
		{Kind: MirrorChangeAssignee, At: 850, From: "acc-user1", To: "acc-user2"}}
	history := func(gen int64, h MirrorHistory) func() bool {
		return func() bool {
			h.IssueID = "10007"
			results, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{h}), MirrorCatalog{}, mirrorReadOf(root, gen))
			if err != nil || len(results) != 1 {
				t.Fatalf("history: %+v, %v", results, err)
			}
			return results[0].Changed
		}
	}
	acts := func(gen int64, tried []MirrorActsAttempt, in ...MirrorIssueActs) func() bool {
		return func() bool {
			out, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, gen), in, tried, MirrorCatalog{})
			if err != nil {
				t.Fatal(err)
			}
			return slices.Contains(out.Credited, "10007")
		}
	}
	comments := func(readFor int64) MirrorIssueActs {
		return MirrorIssueActs{IssueID: "10007", Source: MirrorActComment, ReadFor: readFor, Acts: []MirrorAct{act("c7", "acc-user3", 600, 0)}}
	}
	worklogs := func(readFor, from int64) MirrorIssueActs {
		return MirrorIssueActs{IssueID: "10007", Source: MirrorActWorklog, ReadFor: readFor, From: from, Acts: []MirrorAct{act("w7", "acc-user3", 650, 3600)}}
	}
	failed := func(reason string) []MirrorActsAttempt {
		return []MirrorActsAttempt{{IssueID: "10007", Source: MirrorActComment, Reason: "issue 10007: " + reason}}
	}
	// opened is the ticket's detail read for its card, which decides its
	// status time for the card from the tracker's own answer.
	opened := func() bool {
		ticket := heldTicket(t, s, root, "10007", []byte(`{"description":"read"}`))
		ticket.Status = &MirrorStatusEvidence{Status: "10001", CreatedAt: 100, UpdatedAt: 1000, LatestTo: "10001", LatestAt: 400, Complete: true, Final: true}
		w, err := s.StoreMirrorTickets(root, currentSweep(s, root), []MirrorTicket{ticket}, MirrorCatalog{}, MirrorRead{}, mirrorReadOf(root, 35))
		if err != nil {
			t.Fatal(err)
		}
		return w.Tickets[0].CardChanged || w.Tickets[0].StatusChanged
	}
	// reset is a read of the board whose estimate field changed, which
	// tells the board to read again (a structure change): its rows'
	// estimates count again as each is read after it.
	reset := func() bool {
		st := webStructure()
		st.Board.EstimateField, st.ReadAt, st.Complete = "customfield_2", mirrorReadAt(46), true
		w, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, st))
		if err != nil {
			t.Fatal(err)
		}
		return w.Changed
	}
	// reread is the ticket's row read again, as it was, after the reset.
	reread := func() bool {
		r := sectionRow("10007", "10007", "1", 5, "21")
		r.Card.UpdatedAt, r.Card.AssigneeID, r.Card.TypeID, r.Card.Labels = 1100, "acc-user3", "10004", []string{"support"}
		r.Card.Data = []byte(`{"summary":"edited 1100"}`)
		page := webPage("10", r)
		page.ReadAt = mirrorReadAt(47)
		ch := mustWriteRowPage(t, s, root, page, nil)
		return slices.Contains(ch.Cards, "10007") || slices.Contains(ch.Rows, "10007")
	}
	long := longHistory("10007", clearMirrorChunk+1, 1)
	changed := map[creditReadColumn]bool{}
	for _, step := range []struct {
		name  string
		write func() bool
	}{
		{"the card is read", card(900)},
		{"its history is read, not yet final", history(30, MirrorHistory{Changes: changes, Wait: 60_000})},
		{"its history is read final", history(31, MirrorHistory{Changes: changes})},
		{"its comments are read", acts(32, nil, comments(900))},
		{"its worklogs are read from 300", acts(33, nil, worklogs(900, 300))},
		{"its worklogs are read whole", acts(34, nil, worklogs(900, 0))},
		{"its card is edited", card(1000)},
		{"its detail is read", opened},
		{"its history is read for the edit, the same changes", history(40, MirrorHistory{Changes: changes})},
		{"its comments fail to read", acts(41, failed("500 Internal Server Error"))},
		{"its comments fail again for another reason", acts(42, failed("403 Forbidden"))},
		{"its evidence is read for the edit", acts(43, nil, comments(1000), worklogs(1000, 0))},
		{"its history is known only from after its finish", history(44, MirrorHistory{Changes: changes[1:], CompleteFrom: 500})},
		{"its history is read again, long", history(45, long)},
		{"everything credit reads of its card changes", card(1100, func(r *MirrorRow) {
			r.Card.StatusID, r.Card.AssigneeID, r.Card.ResolvedAt, r.Card.TypeID, r.Card.Labels = "1", "acc-user3", 0, "10004", []string{"support"}
			r.Card.Sprints, r.Estimate = []string{"21"}, new(5.0)
		})},
		{"the board's estimate field changes", reset},
		{"its row is read again after it", reread},
	} {
		beforeCols := issueColumnValues(t, s, root, "10007", cols)
		beforeRows, _ := creditOf(t, s, root, regroup)
		reported := step.write()
		afterCols := issueColumnValues(t, s, root, "10007", cols)
		afterRows, _ := creditOf(t, s, root, regroup)
		for _, c := range cols {
			if beforeCols[c] != "" && beforeCols[c] != afterCols[c] {
				changed[c] = true
			}
		}
		if before, after := creditRowsOf(beforeRows, "10007"), creditRowsOf(afterRows, "10007"); !slices.Equal(before, after) && !reported {
			t.Errorf("%s changed the ticket's credit and did not report it:\n%s\nto\n%s", step.name, strings.Join(before, "\n"), strings.Join(after, "\n"))
		}
	}
	for _, c := range cols {
		if !changed[c] {
			t.Errorf("credit reads %s.%s, which no write of this test changes: add the write that does", c.table, c.column)
		}
	}
	if len(cols) == 0 {
		t.Fatal("found no column credit reads")
	}
}
