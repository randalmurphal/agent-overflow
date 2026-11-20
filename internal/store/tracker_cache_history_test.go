package store

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func webHistory() MirrorHistory {
	two, five := 2.0, 5.0
	return MirrorHistory{IssueID: "10001", CompleteFrom: 0, CompleteTo: 9, Changes: []MirrorChange{
		{Kind: MirrorChangeStatus, At: 2, AuthorID: "acc-user1", From: "1", To: "3"},
		{Kind: MirrorChangeSprint, At: 3, AuthorID: "acc-user1", FromSprints: []string{}, ToSprints: []string{"21", "20"}},
		{Kind: MirrorChangeEstimate, At: 4, BoardID: "10", FromNumber: nil, ToNumber: &two},
		{Kind: MirrorChangeEstimate, At: 5, BoardID: "10", FromNumber: &two, ToNumber: &five},
		{Kind: MirrorChangeAssignee, At: 6, To: "acc-user1"},
		{Kind: MirrorChangeStatus, At: 7, From: "3", To: "10001"},
	}}
}

// A history stores whole and reads back as rows; the same history again
// changes nothing; a shorter one removes the changes it no longer has.
func TestReplaceMirrorHistoryReportsChanges(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10001", 9, "a")}}, nil)

	h := webHistory()
	for i, want := range []bool{true, false} {
		if changed, err := s.ReplaceMirrorHistory(root, h); err != nil || changed != want {
			t.Fatalf("replace %d: changed %v, %v; want %v", i, changed, err, want)
		}
	}
	assertRows(t, s.db, `SELECT kind || ':' || seq || ':' || at || ':' || author_id || ':' || board_id || ':' || from_value || '>' || to_value
		|| ':' || coalesce(from_number, '-') || '>' || coalesce(to_number, '-')
		FROM tracker_cache_changes ORDER BY at`, strings.Join([]string{
		"status:0:2:acc-user1::1>3:->-",
		`sprint:0:3:acc-user1::[]>["20","21"]:->-`,
		"estimate:0:4::10:>:->2.0",
		"estimate:1:5::10:>:2.0>5.0",
		"assignee:0:6:::>acc-user1:->-",
		"status:1:7:::3>10001:->-",
	}, ","))
	assertRows(t, s.db, `SELECT history_from || ':' || history_to FROM tracker_cache_issues`, "0:9")

	for name, edit := range map[string]func(*MirrorHistory){
		"final later":    func(h *MirrorHistory) { h.CompleteTo = 10 },
		"complete later": func(h *MirrorHistory) { h.CompleteFrom = 1 },
		"author":         func(h *MirrorHistory) { h.Changes[0].AuthorID = "" },
		"sprint set":     func(h *MirrorHistory) { h.Changes[1].ToSprints = []string{"21"} },
		"estimate value": func(h *MirrorHistory) { h.Changes[3].ToNumber = nil },
		"change dropped": func(h *MirrorHistory) { h.Changes = h.Changes[:5] },
		"kind emptied": func(h *MirrorHistory) {
			h.Changes = slices.DeleteFunc(h.Changes, func(c MirrorChange) bool { return c.Kind == MirrorChangeAssignee })
		},
		"change appended": func(h *MirrorHistory) {
			h.Changes = append(h.Changes, MirrorChange{Kind: MirrorChangeAssignee, At: 8, From: "acc-user1"})
		},
	} {
		edited := webHistory()
		edit(&edited)
		if changed, err := s.ReplaceMirrorHistory(root, edited); err != nil || !changed {
			t.Errorf("%s: changed %v, %v", name, changed, err)
		}
		if changed, err := s.ReplaceMirrorHistory(root, edited); err != nil || changed {
			t.Errorf("%s again: changed %v, %v", name, changed, err)
		}
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM tracker_cache_changes`).Scan(&n); err != nil || n != len(edited.Changes) {
			t.Errorf("%s: %d changes stored; want %d", name, n, len(edited.Changes))
		}
		if _, err := s.ReplaceMirrorHistory(root, webHistory()); err != nil {
			t.Fatal(err)
		}
	}

	// The history of an issue the mirror no longer holds stores nothing.
	missing := webHistory()
	missing.IssueID = "10009"
	if changed, err := s.ReplaceMirrorHistory(root, missing); err != nil || changed {
		t.Fatalf("history of an issue not in the mirror: %v, %v", changed, err)
	}
	for name, edit := range map[string]func(*MirrorHistory){
		"no issue":            func(h *MirrorHistory) { h.IssueID = "" },
		"not final":           func(h *MirrorHistory) { h.CompleteTo = 0 },
		"negative complete":   func(h *MirrorHistory) { h.CompleteFrom = -1 },
		"status with sprints": func(h *MirrorHistory) { h.Changes[0].ToSprints = []string{"21"} },
		"status with number":  func(h *MirrorHistory) { h.Changes[0].ToNumber = h.Changes[2].ToNumber },
		"sprint with number":  func(h *MirrorHistory) { h.Changes[1].ToNumber = h.Changes[2].ToNumber },
		"estimate with IDs":   func(h *MirrorHistory) { h.Changes[2].To = "3" },
		"estimate with set":   func(h *MirrorHistory) { h.Changes[2].ToSprints = []string{"21"} },
		"unknown kind":        func(h *MirrorHistory) { h.Changes[0].Kind = "priority" },
		"no time":             func(h *MirrorHistory) { h.Changes[0].At = 0 },
		"estimate no board":   func(h *MirrorHistory) { h.Changes[2].BoardID = "" },
		"status with board":   func(h *MirrorHistory) { h.Changes[0].BoardID = "10" },
		"sprint with IDs":     func(h *MirrorHistory) { h.Changes[1].To = "21" },
		"assignee sprint set": func(h *MirrorHistory) { h.Changes[4].ToSprints = []string{"21"} },
	} {
		bad := webHistory()
		edit(&bad)
		if _, err := s.ReplaceMirrorHistory(root, bad); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The issues due a history read are those with none and those updated
// after the time theirs is final through, by ID from a keyset.
func TestListMirrorHistoryDue(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	rows := make([]MirrorRow, 0, mirrorHistoryDueLimit+3)
	for i := range mirrorHistoryDueLimit + 3 {
		rows = append(rows, webRow(fmt.Sprintf("1%04d", i), 9, "a"))
	}
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: rows}, nil)
	for id, to := range map[string]int64{"10000": 9, "10001": 8, "10002": 12} {
		if _, err := s.ReplaceMirrorHistory(root, MirrorHistory{IssueID: id, CompleteTo: to}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.ListMirrorHistoryDue(root, "")
	if err != nil || len(first) != mirrorHistoryDueLimit || first[0] != "10001" || first[1] != "10003" {
		t.Fatalf("first read = %d IDs from %v, %v; want %d from 10001, 10003", len(first), first[:2], err, mirrorHistoryDueLimit)
	}
	rest, err := s.ListMirrorHistoryDue(root, first[len(first)-1])
	if err != nil || !slices.Equal(rest, []string{"10102"}) {
		t.Fatalf("second read = %v, %v", rest, err)
	}
	// An update after the history moves the issue back into the due set.
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10000", 10, "a")}}, nil)
	if due, err := s.ListMirrorHistoryDue(root, ""); err != nil || due[0] != "10000" {
		t.Fatalf("after an update = %v, %v", due[:1], err)
	}
}

// A detail stores with its open time, reports a changed document, and the
// root keeps only the mirrorDetailCap most recently opened.
func TestStoreMirrorDetailKeepsTheMostRecent(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	rows := make([]MirrorRow, 0, mirrorDetailCap+2)
	for i := range mirrorDetailCap + 2 {
		rows = append(rows, webRow(fmt.Sprintf("1%04d", i), 9, "a"))
	}
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: rows}, nil)
	for i := range mirrorDetailCap {
		if got, err := s.StoreMirrorDetail(root, MirrorDetail{IssueID: fmt.Sprintf("1%04d", i), OpenedAt: int64(100 + i), Data: []byte(`{"v":1}`)}); err != nil || !got.Stored || !got.Changed || got.Dropped != nil {
			t.Fatalf("detail %d = %+v, %v", i, got, err)
		}
	}
	// Reopening the oldest makes it the newest; the next new one drops
	// the oldest left.
	if got, err := s.StoreMirrorDetail(root, MirrorDetail{IssueID: "10000", OpenedAt: 1000, Data: []byte(`{"v":1}`)}); err != nil || !reflect.DeepEqual(got, MirrorDetailWrite{Stored: true}) {
		t.Fatalf("reopen = %+v, %v", got, err)
	}
	got, err := s.StoreMirrorDetail(root, MirrorDetail{IssueID: "10200", OpenedAt: 1001, Data: []byte(`{"v":1}`)})
	if err != nil || !reflect.DeepEqual(got, MirrorDetailWrite{Stored: true, Changed: true, Dropped: []string{"10001"}}) {
		t.Fatalf("one over the cap = %+v, %v", got, err)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details`, fmt.Sprint(mirrorDetailCap))
	if got, err := s.StoreMirrorDetail(root, MirrorDetail{IssueID: "10000", OpenedAt: 1002, Data: []byte(`{"v":2}`)}); err != nil || !reflect.DeepEqual(got, MirrorDetailWrite{Stored: true, Changed: true}) {
		t.Fatalf("changed document = %+v, %v", got, err)
	}
	d, ok, err := s.GetMirrorDetail(root, "10000")
	if err != nil || !ok || !reflect.DeepEqual(d, MirrorDetail{IssueID: "10000", OpenedAt: 1002, Data: []byte(`{"v":2}`)}) {
		t.Fatalf("read = %+v, %v, %v", d, ok, err)
	}
	if _, ok, err := s.GetMirrorDetail(root, "10001"); err != nil || ok {
		t.Fatalf("dropped detail reads: %v, %v", ok, err)
	}
	if got, err := s.StoreMirrorDetail(root, MirrorDetail{IssueID: "10999", OpenedAt: 1003, Data: []byte(`{}`)}); err != nil || !reflect.DeepEqual(got, MirrorDetailWrite{}) {
		t.Fatalf("detail of an issue not in the mirror = %+v, %v", got, err)
	}
	if _, err := s.StoreMirrorDetail(root, MirrorDetail{IssueID: "10000"}); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("detail with no open time: %v", err)
	}
}

func webWorklog(id, day string, spent int64) MirrorWorklog {
	return MirrorWorklog{ID: id, IssueID: "10001", AuthorID: "acc-user1", Day: day, SpentSeconds: spent, Description: "Review", UpdatedAt: 5}
}

// A replace owns one author's days: it upserts what it lists, removes the
// rest of those days, and leaves other days and authors.
func TestReplaceMirrorWorklogs(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	week := []MirrorWorklog{webWorklog("w1", "2026-01-05", 3600), webWorklog("w2", "2026-01-06", 1800), webWorklog("w3", "2026-01-09", 600)}
	got, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-09", week)
	if err != nil || !reflect.DeepEqual(got, MirrorWorklogChanges{Changed: []string{"w1", "w2", "w3"}}) {
		t.Fatalf("first = %+v, %v", got, err)
	}
	other := webWorklog("w9", "2026-01-06", 60)
	other.AuthorID, other.IssueID = "acc-user2", "10002"
	if _, err := s.ReplaceMirrorWorklogs(root, "acc-user2", "2026-01-06", "2026-01-06", []MirrorWorklog{other}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-09", week); err != nil || !reflect.DeepEqual(got, MirrorWorklogChanges{}) {
		t.Fatalf("same again = %+v, %v", got, err)
	}
	edited := webWorklog("w2", "2026-01-06", 2700)
	got, err = s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-06", "2026-01-07", []MirrorWorklog{edited})
	if err != nil || !reflect.DeepEqual(got, MirrorWorklogChanges{Changed: []string{"w2"}}) {
		t.Fatalf("edit = %+v, %v", got, err)
	}
	got, err = s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-06", nil)
	if err != nil || !reflect.DeepEqual(got, MirrorWorklogChanges{Removed: []string{"w1", "w2"}}) {
		t.Fatalf("emptied days = %+v, %v", got, err)
	}
	logs, more, err := s.ListMirrorWorklogs(root, "acc-user1", "2026-01-01", "2026-01-31")
	if err != nil || more || !reflect.DeepEqual(logs, []MirrorWorklog{week[2]}) {
		t.Fatalf("author's month = %+v, %v, %v", logs, more, err)
	}
	logs, more, err = s.ListMirrorIssueWorklogs(root, "10002")
	if err != nil || more || !reflect.DeepEqual(logs, []MirrorWorklog{other}) {
		t.Fatalf("issue's worklogs = %+v, %v, %v", logs, more, err)
	}

	for name, call := range map[string]func() error{
		"bad day": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-01x", "2026-01-09", nil)
			return err
		},
		"days backwards": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-09", "2026-01-05", nil)
			return err
		},
		"no author": func() error { _, err := s.ReplaceMirrorWorklogs(root, "", "2026-01-05", "2026-01-09", nil); return err },
		"another author": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-09", []MirrorWorklog{other})
			return err
		},
		"no issue": func() error {
			w := week[0]
			w.IssueID = ""
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-09", []MirrorWorklog{w})
			return err
		},
		"no worklog ID": func() error {
			w := week[0]
			w.ID = ""
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-09", []MirrorWorklog{w})
			return err
		},
		"negative update": func() error {
			w := week[0]
			w.UpdatedAt = -1
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-09", []MirrorWorklog{w})
			return err
		},
		"bad log day": func() error {
			w := week[0]
			w.Day = "2026-01-05T"
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-09", []MirrorWorklog{w})
			return err
		},
		"bad last day": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-9", nil)
			return err
		},
		"before the days": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-06", "2026-01-09", week[:1])
			return err
		},
		"outside the days": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-08", week[2:])
			return err
		},
		"repeated": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-09", []MirrorWorklog{week[0], week[0]})
			return err
		},
		"no time spent": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-09", []MirrorWorklog{webWorklog("w4", "2026-01-05", 0)})
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A worklog read past its bound says so.
func TestListMirrorWorklogsIsBounded(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	logs := make([]MirrorWorklog, 0, mirrorWorklogLimit+1)
	for i := range mirrorWorklogLimit + 1 {
		logs = append(logs, webWorklog(fmt.Sprintf("w%04d", i), "2026-01-05", 60))
	}
	if _, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-05", logs); err != nil {
		t.Fatal(err)
	}
	got, more, err := s.ListMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-05")
	if err != nil || !more || len(got) != mirrorWorklogLimit {
		t.Fatalf("read = %d, %v, %v", len(got), more, err)
	}
	got, more, err = s.ListMirrorIssueWorklogs(root, "10001")
	if err != nil || !more || len(got) != mirrorWorklogLimit {
		t.Fatalf("issue read = %d, %v, %v", len(got), more, err)
	}
}

// Each part of a scope's sync state is written alone and leaves the
// others, the cursor included.
func TestMirrorSyncStateIsWrittenByPart(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10"}, []byte("c1"))
	failure := MirrorSyncFailure{At: 30, Kind: MirrorFailureRateLimited, Message: "The tracker asked to wait."}
	for _, step := range []struct {
		name  string
		write func() (bool, error)
		want  bool
	}{
		{"read", func() (bool, error) { return s.SetMirrorSyncRead(root, "10", 10) }, true},
		{"read again", func() (bool, error) { return s.SetMirrorSyncRead(root, "10", 10) }, false},
		{"reconcile", func() (bool, error) { return s.SetMirrorSyncReconciled(root, "10", 20) }, true},
		{"reconcile again", func() (bool, error) { return s.SetMirrorSyncReconciled(root, "10", 20) }, false},
		{"failure", func() (bool, error) { return s.SetMirrorSyncFailure(root, "10", failure) }, true},
		{"failure again", func() (bool, error) { return s.SetMirrorSyncFailure(root, "10", failure) }, false},
	} {
		if changed, err := step.write(); err != nil || changed != step.want {
			t.Fatalf("%s: changed %v, %v; want %v", step.name, changed, err, step.want)
		}
	}
	want := MirrorSync{Cursor: []byte("c1"), ReadAt: 10, ReconciledAt: 20, Failure: failure}
	if got, ok, err := s.GetMirrorSync(root, "10"); err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %+v, %v, %v\nwant %+v", got, ok, err, want)
	}
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10"}, []byte("c2"))
	if changed, err := s.SetMirrorSyncFailure(root, "10", MirrorSyncFailure{}); err != nil || !changed {
		t.Fatalf("clear failure: %v, %v", changed, err)
	}
	want.Cursor, want.Failure = []byte("c2"), MirrorSyncFailure{}
	if got, _, err := s.GetMirrorSync(root, "10"); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %+v, %v\nwant %+v", got, err, want)
	}

	// The linked issues are a scope of their own, with no board.
	if changed, err := s.SetMirrorSyncRead(root, "", 40); err != nil || !changed {
		t.Fatalf("linked scope read: %v, %v", changed, err)
	}
	if got, ok, err := s.GetMirrorSync(root, ""); err != nil || !ok || !reflect.DeepEqual(got, MirrorSync{ReadAt: 40}) {
		t.Fatalf("linked scope = %+v, %v, %v", got, ok, err)
	}
	for name, f := range map[string]MirrorSyncFailure{
		"unknown kind":        {At: 1, Kind: "flaky"},
		"no time":             {Kind: MirrorFailureSetup},
		"cleared with a note": {Message: "x"},
	} {
		if _, err := s.SetMirrorSyncFailure(root, "10", f); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
