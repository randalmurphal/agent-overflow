package store

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// creditRowsOf is the rows of creditOf that name ticket.
func creditRowsOf(rows []string, ticket string) []string {
	var out []string
	for _, r := range rows {
		if strings.Contains(r, " "+ticket+" ") {
			out = append(out, r)
		}
	}
	return out
}

// A support ticket is credited by the time logged on it alone: done in a
// closed sprint, it is neither a finished ticket nor reviewed, and the
// pace's finished part leaves it out.
func TestASupportTicketIsCreditedOnlyByItsTime(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	support := sectionRow("10003", "10003", "10001", -1, "20")
	support.Card.UpdatedAt = 970
	mustWriteRowPage(t, s, root, webPage("10", support), nil)
	regroup := creditRegroup(t, creditRules(false))
	rows, _ := creditOf(t, s, root, regroup)
	want := []string{
		"20 - 10003 support acc-user2 0.5 0.5 - - - - - 2.0 4 -",
		"21 - 10003 support acc-user2 0.5 0.5 - - - - - 2.0 4 -",
	}
	if got := creditRowsOf(rows, "10003"); !reflect.DeepEqual(got, want) {
		t.Fatalf("support ticket's credit\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if pace := paceOf(t, s, root, "10", regroup); len(pace) != 1 || pace[0].Done != 2 || pace[0].Estimate != 8 || pace[0].Support != 0.5 {
		t.Fatalf("pace %+v; want 2 finished tickets of 8 points and 0.5 support", pace)
	}
}

// A sprint member's finish is its last move into done by the sprint's
// end, else its resolution by then, and its assignee then is who had it:
// resolved at 400 and reassigned at 850, User 1 is credited, at 400. With
// neither, while its history holds an assignee change, who had it is not
// known: the row is counted for no one and says so.
func TestASprintTicketsFinishFallsBackToItsResolution(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	resolved := sectionRow("10002", "10002", "10001", 3, "20")
	resolved.Card.UpdatedAt, resolved.Card.ResolvedAt, resolved.Card.AssigneeID = 900, 400, "acc-user2"
	unknown := sectionRow("10006", "10006", "10001", 1, "20")
	unknown.Card.UpdatedAt, unknown.Card.AssigneeID = 900, "acc-user2"
	mustWriteRowPage(t, s, root, webPage("10", resolved, unknown), nil)
	reassigned := func(id string) MirrorHistory {
		return MirrorHistory{IssueID: id, Changes: []MirrorChange{{Kind: MirrorChangeAssignee, At: 850, From: "acc-user1", To: "acc-user2"}}}
	}
	if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{reassigned("10002"), reassigned("10006")}),
		MirrorCatalog{}, mirrorReadOf(root, 30)); err != nil {
		t.Fatal(err)
	}
	rows, _ := creditOf(t, s, root, creditRegroup(t, creditRules(false)))
	var finished []string
	for _, r := range rows {
		if strings.Contains(r, " finished ") && (strings.Contains(r, " 10002 ") || strings.Contains(r, " 10006 ")) {
			finished = append(finished, r)
		}
	}
	want := []string{
		"20 400 10002 finished acc-user1 3.0 1.0 3.0 - - - - - - -",
		"20 - 10006 finished - 1.0 1.0 1.0 - - - - - - who had it when it finished is not known",
	}
	if !reflect.DeepEqual(finished, want) {
		t.Fatalf("finished\n%s\nwant\n%s", strings.Join(finished, "\n"), strings.Join(want, "\n"))
	}
}

// Support time after the last closed sprint, before a dated future
// sprint, counts in the future sprint, the next to start; the future
// sprint's members are not credited before it starts.
func TestSupportTimeBeforeADatedFutureSprintCountsInIt(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	st := webStructure()
	for i, sp := range st.Sprints {
		if sp.ID == "21" {
			st.Sprints[i].State = MirrorSprintFuture
		}
	}
	st.ReadAt = mirrorReadAt(40)
	if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, st)); err != nil {
		t.Fatal(err)
	}
	rows, _ := creditOf(t, s, root, creditRegroup(t, creditRules(false)))
	var later []string
	for _, r := range rows {
		if strings.HasPrefix(r, "21 ") {
			later = append(later, r)
		}
	}
	want := []string{
		"21 - 10003 support acc-user2 0.5 0.5 - - - - - 2.0 4 -",
		"21 990 10004 support - - - - - - - - - - who worked on it is not read yet",
	}
	if !reflect.DeepEqual(later, want) {
		t.Fatalf("the future sprint's credit\n%s\nwant\n%s", strings.Join(later, "\n"), strings.Join(want, "\n"))
	}
}

// The pace and the export place support time alike, whatever the
// history window: sprint 20's two hours at 500 count in both with the
// window at 600. Worklogs held only from a later start than the sprint's
// count what is held and say what is not read.
func TestThePaceAndTheExportPlaceSupportAlike(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	regroup := creditRegroup(t, creditRules(false))
	if pace := paceOf(t, s, root, "10", regroup); len(pace) != 1 || pace[0].Support != 0.5 {
		t.Fatalf("pace %+v; want 0.5 support", pace)
	}
	rows, _ := creditAt(t, s, root, regroup, 600)
	want := []string{
		"20 - 10003 support acc-user2 0.5 0.5 - - - - - 2.0 4 -",
		"21 - 10003 support acc-user2 0.5 0.5 - - - - - 2.0 4 -",
	}
	if got := creditRowsOf(rows, "10003"); !reflect.DeepEqual(got, want) {
		t.Fatalf("export with the window at 600\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	replaceActs(t, s, root, 30, nil, MirrorIssueActs{IssueID: "10003", Source: MirrorActWorklog, ReadFor: 970, From: 300,
		Acts: []MirrorAct{act("w2", "acc-user2", 500, 7200), act("w3", "acc-user2", 960, 7200)}})
	rows, _ = creditAt(t, s, root, regroup, 0)
	want[0] = "20 - 10003 support acc-user2 0.5 0.5 - - - - - 2.0 4 time logged before 1970-01-01 is not read"
	if got := creditRowsOf(rows, "10003"); !reflect.DeepEqual(got, want) {
		t.Fatalf("worklogs held from 300\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Work dated after the evaluation does not count: a worklog dated
// tomorrow neither qualifies a reviewer on the active sprint nor counts
// as its support time.
func TestWorkDatedInTheFutureDoesNotCount(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	tomorrow := time.Now().Add(24 * time.Hour).UnixMilli()
	replaceActs(t, s, root, 30, nil,
		MirrorIssueActs{IssueID: "10005", Source: MirrorActWorklog, ReadFor: 1500, Acts: []MirrorAct{act("w9", "acc-user3", tomorrow, 3600)}},
		MirrorIssueActs{IssueID: "10003", Source: MirrorActWorklog, ReadFor: 970, Acts: []MirrorAct{act("w2", "acc-user2", 500, 7200),
			act("w3", "acc-user2", 960, 7200), act("w4", "acc-user2", tomorrow, 7200)}})
	rows, _ := creditAt(t, s, root, creditRegroup(t, creditRules(false)), 0)
	want := []string{
		"21 - 10003 support acc-user2 0.5 0.5 - - - - - 2.0 4 -",
		"21 - 10005 review acc-user3 0.0 0.0 2.0 10 2 commented 0 - - -",
	}
	var got []string
	for _, r := range rows {
		if strings.HasPrefix(r, "21 - 10003 support") || strings.HasPrefix(r, "21 - 10005 review") {
			got = append(got, r)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("with work dated tomorrow\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Every write that changes what credit reads moves the totals revision of
// the boards holding its ticket: a card edit that moves the card past its
// evidence read (the ticket's credit is not read until read again), and
// a change of the time source, which drops every worklog. A card edit
// that leaves the read current, and one of a ticket with no read, do not.
func TestWritesThatChangeWhatCreditReadsMoveTheTotals(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	regroup := creditRegroup(t, creditRules(false))
	var conn string
	if err := s.db.QueryRow(`SELECT connection_id FROM tracker_cache_roots WHERE id = ?`, root).Scan(&conn); err != nil {
		t.Fatal(err)
	}
	edit := func(id string, updated int64) func() {
		return func() {
			r := sectionRow(id, id, "10001", 5, "20")
			if id == "10002" {
				r.Estimate = new(3.0)
			}
			r.Card.UpdatedAt, r.Card.Data = updated, []byte(`{"summary":"edited `+strconv.FormatInt(updated, 10)+`"}`)
			if id == "10001" {
				r.Card.AssigneeID = "acc-user2"
			}
			mustWriteRowPage(t, s, root, webPage("10", r), nil)
		}
	}
	for _, step := range []struct {
		name   string
		write  func()
		moves  bool
		unread int
	}{
		{"an edit the read covers", edit("10001", 900), false, 1},
		{"an edit of a ticket with no read", edit("10002", 905), false, 1},
		{"an edit past the read", edit("10001", 1000), true, 2},
		{"another edit past it", edit("10001", 1100), false, 2},
		{"the time source changes", func() {
			mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.tempo.io", ParentID: conn, Label: "Tempo", CreatedAt: 11})
		}, true, 2},
	} {
		before, err := s.CountMirrorSections(root, "10", regroup)
		if err != nil {
			t.Fatal(err)
		}
		step.write()
		after, err := s.CountMirrorSections(root, "10", regroup)
		if err != nil {
			t.Fatal(err)
		}
		if moved := after.TotalsRevision != before.TotalsRevision; moved != step.moves || len(after.Pace) != 1 || after.Pace[0].Unread != step.unread {
			t.Errorf("%s: totals revision %d -> %d, pace %+v; want it to move %v, %d not read", step.name, before.TotalsRevision, after.TotalsRevision,
				after.Pace, step.moves, step.unread)
		}
	}
}
