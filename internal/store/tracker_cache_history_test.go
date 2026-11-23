package store

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func webHistory(issue string) MirrorHistory {
	two, five := 2.0, 5.0
	return MirrorHistory{IssueID: issue, CompleteFrom: 0, Changes: []MirrorChange{
		{Kind: MirrorChangeStatus, At: 2, AuthorID: "acc-user1", From: "1", To: "3"},
		{Kind: MirrorChangeSprint, At: 3, AuthorID: "acc-user1", FromSprints: []string{}, ToSprints: []string{"21", "20"}},
		{Kind: MirrorChangeEstimate, At: 4, BoardID: "10", FromNumber: nil, ToNumber: &two},
		{Kind: MirrorChangeEstimate, At: 5, BoardID: "10", FromNumber: &two, ToNumber: &five},
		{Kind: MirrorChangeAssignee, At: 6, To: "acc-user1"},
		{Kind: MirrorChangeStatus, At: 7, From: "3", To: "10001"},
	}}
}

// replaceHistory writes one history, as a batch of one.
func replaceHistory(s *Store, root int64, h MirrorHistory) (MirrorWrite, error) {
	results, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{h}), MirrorCatalog{}, mirrorReadAt(0))
	if err != nil {
		return MirrorWrite{}, err
	}
	return MirrorWrite{Changed: results[0].Changed, Superseded: results[0].Superseded}, nil
}

func mustReplaceHistory(t *testing.T, s *Store, root int64, h MirrorHistory, want MirrorWrite) {
	t.Helper()
	if got, err := replaceHistory(s, root, h); err != nil || got != want {
		t.Fatalf("replace %s history (for %d): %+v, %v; want %+v", h.IssueID, h.For, got, err, want)
	}
}

// A history stores whole and reads back as rows; the same history again
// changes nothing; a shorter one removes the changes it no longer has.
func TestReplaceMirrorHistoryReportsChanges(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 9, "a")), nil)

	h := webHistory("10001")
	mustReplaceHistory(t, s, root, h, MirrorWrite{Changed: true})
	mustReplaceHistory(t, s, root, h, MirrorWrite{})
	assertRows(t, s.db, `SELECT kind || ':' || seq || ':' || at || ':' || author_id || ':' || board_id || ':' || from_value || '>' || to_value
		|| ':' || coalesce(from_number, '-') || '>' || coalesce(to_number, '-')
		FROM tracker_cache_changes ORDER BY at`, strings.Join([]string{
		"1:0:2:acc-user1::1>3:->-",
		`2:0:3:acc-user1::[]>["20","21"]:->-`,
		"3:0:4::10:>:->2.0",
		"3:1:5::10:>:2.0>5.0",
		"4:0:6:::>acc-user1:->-",
		"1:1:7:::3>10001:->-",
	}, ","))
	assertRows(t, s.db, `SELECT history_from || ':' || history_for || ':' || history_answered_at || ':' || history_wait FROM tracker_cache_issues`, "0:9:1:0")

	for name, edit := range map[string]func(*MirrorHistory){
		"answered later": func(h *MirrorHistory) { h.AnsweredAt = 10 },
		"not final":      func(h *MirrorHistory) { h.Wait = 60_000 },
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
		s, _, root := newMirror(t)
		writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 9, "a")})
		mustReplaceHistory(t, s, root, webHistory("10001"), MirrorWrite{Changed: true})
		edited := webHistory("10001")
		edit(&edited)
		if got, err := replaceHistory(s, root, edited); err != nil || got != (MirrorWrite{Changed: true}) {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
		if got, err := replaceHistory(s, root, edited); err != nil || got != (MirrorWrite{}) {
			t.Errorf("%s again: %+v, %v", name, got, err)
		}
		if n := countMirrorRows(t, s.db)["tracker_cache_changes"]; n != len(edited.Changes) {
			t.Errorf("%s: %d changes stored; want %d", name, n, len(edited.Changes))
		}
	}

	// The history of an issue the mirror does not keep stores nothing.
	mustReplaceHistory(t, s, root, webHistory("10009"), MirrorWrite{})
	for name, bad := range map[string]MirrorHistory{
		"no issue":      {For: 9},
		"no version":    {IssueID: "10001", For: -1},
		"version ahead": {IssueID: "10001", For: 10},
		"negative from": {IssueID: "10001", CompleteFrom: -1},
		"no answer":     {IssueID: "10001", AnsweredAt: -1},
		"negative wait": {IssueID: "10001", Wait: -1},
	} {
		if _, err := replaceHistory(s, root, bad); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, edit := range map[string]func(*MirrorHistory){
		"status with sprints": func(h *MirrorHistory) { h.Changes[0].ToSprints = []string{"21"} },
		"status with number":  func(h *MirrorHistory) { h.Changes[0].ToNumber = h.Changes[2].ToNumber },
		"status with board":   func(h *MirrorHistory) { h.Changes[0].BoardID = "10" },
		"sprint with number":  func(h *MirrorHistory) { h.Changes[1].ToNumber = h.Changes[2].ToNumber },
		"sprint with IDs":     func(h *MirrorHistory) { h.Changes[1].To = "21" },
		"estimate with IDs":   func(h *MirrorHistory) { h.Changes[2].To = "3" },
		"estimate with set":   func(h *MirrorHistory) { h.Changes[2].ToSprints = []string{"21"} },
		"estimate no board":   func(h *MirrorHistory) { h.Changes[2].BoardID = "" },
		"assignee sprint set": func(h *MirrorHistory) { h.Changes[4].ToSprints = []string{"21"} },
		"unknown kind":        func(h *MirrorHistory) { h.Changes[0].Kind = "priority" },
		"no time":             func(h *MirrorHistory) { h.Changes[0].At = 0 },
	} {
		bad := webHistory("10001")
		edit(&bad)
		if _, err := replaceHistory(s, root, bad); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A history listed before the stored one was written (its receipt is
// stale) is superseded and leaves it, however its content differs; one
// listed since replaces it.
func TestMirrorHistoryIsNeverReplacedByAnEarlierOne(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 9, "a")), nil)
	listed := receiptOf(t, s, root, "10001")
	mustReplaceHistory(t, s, root, webHistory("10001"), MirrorWrite{Changed: true})
	stored := countMirrorRows(t, s.db)["tracker_cache_changes"]

	early := listedAt(s, root, statusHistory("10001", 3), listed)
	if results, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), []MirrorHistory{early}, MirrorCatalog{}, mirrorReadAt(0)); err != nil || !results[0].Superseded {
		t.Fatalf("a history listed before the stored one = %+v, %v; want superseded", results, err)
	}
	if n := countMirrorRows(t, s.db)["tracker_cache_changes"]; n != stored {
		t.Fatalf("an earlier history left %d changes; want the stored %d", n, stored)
	}
	assertRows(t, s.db, `SELECT history_receipt FROM tracker_cache_issues`, "1")
	mustReplaceHistory(t, s, root, statusHistory("10001", 3), MirrorWrite{Changed: true})
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_changes`, "3")
}

func mustStatusSince(t *testing.T, s *Store, root int64, issue string) int64 {
	t.Helper()
	cards := mustMirrorCards(t, s, root, issue)
	if len(cards) != 1 {
		t.Fatalf("issue %s has no card", issue)
	}
	return cards[0].StatusSince
}

// A stored history sets the time the card's status began when it shows
// it (MirrorStatusSince): the history was read for the card's update time
// and is final, and its last status change led to the card's status, or it
// changed no status and is complete from the issue's creation; a history
// that is not final shows its last change's time provisionally when that
// change is within the settle margin of the update; a history that shows
// otherwise gives zero. A card updated after the update its history was
// read for keeps the time it had while it keeps its status, through the
// histories that do not cover it, and reads as unknown once its status
// changes, until a history read for it is stored.
func TestMirrorStatusSinceComesFromTheHistory(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	row := func(id, status string) MirrorRow {
		r := webRow(id, 9, "a"+id)
		r.Card.StatusID = status
		return r
	}
	mustWriteRowPage(t, s, root, webPage("10", row("10001", "10001"), row("10002", "10001"), row("10003", "3"), row("10004", "1"), row("10005", "1"),
		row("10006", "10001")), nil)
	assignee := []MirrorChange{{Kind: MirrorChangeAssignee, At: 6, To: "acc-user1"}}
	for _, tc := range []struct {
		name string
		h    MirrorHistory
		want int64
	}{
		{"last change leads to the status", webHistory("10001"), 7},
		{"card newer than the history", func() MirrorHistory { h := webHistory("10002"); h.For = 8; return h }(), 0},
		{"history not final, its change within the margin", func() MirrorHistory { h := webHistory("10006"); h.Wait = 60_000; return h }(), 7},
		{"last change leads elsewhere", webHistory("10003"), 0},
		{"no change, complete from creation", MirrorHistory{IssueID: "10004", Changes: assignee}, 1},
		{"no change, complete from later", MirrorHistory{IssueID: "10005", CompleteFrom: 4, Changes: assignee}, 0},
	} {
		mustReplaceHistory(t, s, root, tc.h, MirrorWrite{Changed: true})
		if got := mustStatusSince(t, s, root, tc.h.IssueID); got != tc.want {
			t.Errorf("%s: status since %d; want %d", tc.name, got, tc.want)
		}
	}

	same := row("10001", "10001")
	same.Card.UpdatedAt = 10
	mustWriteRowPage(t, s, root, webPage("10", same), nil)
	if got := mustStatusSince(t, s, root, "10001"); got != 7 {
		t.Fatalf("after an update that kept the status: %d; want the time it had, 7", got)
	}
	for _, uncovering := range []struct {
		name string
		edit func(*MirrorHistory)
	}{
		{"read for an earlier update", func(h *MirrorHistory) { h.For = 9; h.Changes[len(h.Changes)-1].At = 8 }},
		{"not final", func(h *MirrorHistory) { h.Wait = 60_000 }},
	} {
		h := webHistory("10001")
		uncovering.edit(&h)
		mustReplaceHistory(t, s, root, h, MirrorWrite{Changed: true})
		if got := mustStatusSince(t, s, root, "10001"); got != 7 {
			t.Fatalf("after a history %s: %d; want the time it had, 7", uncovering.name, got)
		}
	}
	moved := row("10001", "3")
	moved.Card.UpdatedAt = 11
	mustWriteRowPage(t, s, root, webPage("10", moved), nil)
	if got := mustStatusSince(t, s, root, "10001"); got != 0 {
		t.Fatalf("after a status change: %d; want unknown", got)
	}
	h := webHistory("10001")
	h.Changes = append(h.Changes, MirrorChange{Kind: MirrorChangeStatus, At: 11, From: "10001", To: "3"})
	mustReplaceHistory(t, s, root, h, MirrorWrite{Changed: true})
	if got := mustStatusSince(t, s, root, "10001"); got != 11 {
		t.Fatalf("after the new history: %d; want 11", got)
	}

	// A history read for an earlier update than the card's does not cover
	// the card, even holding the change that made it: a card of another
	// status reads as unknown until a history read for its update is
	// stored.
	h.Changes = append(h.Changes, MirrorChange{Kind: MirrorChangeStatus, At: 15, From: "3", To: "1"})
	mustReplaceHistory(t, s, root, h, MirrorWrite{Changed: true})
	back := row("10001", "1")
	back.Card.UpdatedAt = 15
	mustWriteRowPage(t, s, root, webPage("10", back), nil)
	if got := mustStatusSince(t, s, root, "10001"); got != 0 {
		t.Fatalf("a card newer than the update its history was read for: %d; want unknown", got)
	}
	mustReplaceHistory(t, s, root, h, MirrorWrite{Changed: true})
	if got := mustStatusSince(t, s, root, "10001"); got != 15 {
		t.Fatalf("the history read for the card's update: %d; want 15", got)
	}
}

// A batch writes every admitted history in one transaction and reports
// each issue's outcome in input order, an issue the mirror holds but no
// longer keeps included; one whose removals would pass a
// chunk goes to a further transaction, so none deletes more than a chunk.
func TestReplaceMirrorHistoriesInBatches(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 9, "a"), webRow("10002", 9, "b"), webRow("10003", 9, "c"), webRow("10004", 9, "d")})
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10004"}, ReadAt: mirrorReadAt(2)})); err != nil {
		t.Fatal(err)
	}
	mustReplaceHistory(t, s, root, webHistory("10001"), MirrorWrite{Changed: true})
	listed := receiptOf(t, s, root, "10003")
	mustReplaceHistory(t, s, root, webHistory("10003"), MirrorWrite{Changed: true})

	// 10003's history was listed before the one stored.
	batch := dueHistories(s, root, []MirrorHistory{webHistory("10009"), webHistory("10003"), webHistory("10002"), webHistory("10004"), webHistory("10001")})
	batch[1].Receipt = listed
	commits := s.CommitCount()
	results, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), batch, MirrorCatalog{}, mirrorReadAt(0))
	want := []MirrorHistoryResult{{IssueID: "10009", Unkept: true}, {IssueID: "10003", Superseded: true}, {IssueID: "10002", Changed: true},
		{IssueID: "10004", Unkept: true}, {IssueID: "10001"}}
	if err != nil || !reflect.DeepEqual(results, want) {
		t.Fatalf("batch = %+v, %v\nwant %+v", results, err, want)
	}
	if n := s.CommitCount() - commits; n != 1 {
		t.Fatalf("the batch committed %d transactions; want 1", n)
	}

	mustReplaceHistory(t, s, root, statusHistory("10001", clearMirrorChunk), MirrorWrite{Changed: true})
	mustReplaceHistory(t, s, root, statusHistory("10002", clearMirrorChunk), MirrorWrite{Changed: true})
	recorder := recordMirrorWrites(t, s)
	commits = s.CommitCount()
	results, _, err = s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{{IssueID: "10001"}, {IssueID: "10002"}}), MirrorCatalog{}, mirrorReadAt(0))
	if err != nil || !reflect.DeepEqual(results, []MirrorHistoryResult{{IssueID: "10001", Changed: true}, {IssueID: "10002", Changed: true}}) {
		t.Fatalf("emptying batch = %+v, %v", results, err)
	}
	if most, total := recorder.mostDeletedInATransaction(); most > clearMirrorChunk || total != 2*clearMirrorChunk || s.CommitCount()-commits != 2 {
		t.Fatalf("deleted %d, at most %d in one of %d transactions", total, most, s.CommitCount()-commits)
	}

	stored := countMirrorRows(t, s.db)["tracker_cache_changes"]
	tooMany := make([]MirrorHistory, mirrorHistoryBatch+1)
	for i := range tooMany {
		tooMany[i] = MirrorHistory{IssueID: fmt.Sprint(20000 + i)}
	}
	for name, batch := range map[string][]MirrorHistory{
		"too many histories": tooMany,
		"issue twice":        {webHistory("10001"), webHistory("10001")},
		"one invalid":        {webHistory("10002"), {IssueID: "10001", For: -1}},
	} {
		if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, batch), MirrorCatalog{}, mirrorReadAt(0)); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := countMirrorRows(t, s.db)["tracker_cache_changes"]; n != stored {
		t.Fatalf("refused batches left %d changes; want %d", n, stored)
	}
}

// The users and statuses a history read names are stored with its
// histories, in their one transaction, and reported changed only when
// they changed; with no history they are stored alone. A refused batch
// stores neither.
func TestMirrorHistoryReadStoresItsCatalog(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a")), nil)
	user := func(name string) MirrorCatalog {
		return MirrorCatalog{Users: []MirrorUser{{ID: "acc-user2", Name: name}}, Statuses: []MirrorStatus{{ID: "4", Name: "Review", Group: MirrorGroupInProgress}}}
	}
	named := func() string {
		t.Helper()
		c, err := s.GetMirrorCatalog(root, []string{"acc-user2"}, []string{"4"})
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(c)
	}
	write := func(hs []MirrorHistory, c MirrorCatalog, readAt int64) (bool, uint64, error) {
		t.Helper()
		commits := s.CommitCount()
		results, changed, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, hs), c, mirrorReadAt(readAt))
		if err == nil && len(hs) > 0 && !results[0].Changed {
			t.Fatalf("the history did not change: %+v", results)
		}
		return changed, s.CommitCount() - commits, err
	}
	if _, _, err := write([]MirrorHistory{webHistory("10001")}, user("User 2"), 0); !errors.Is(err, ErrInvalidTrackerCache) || named() != fmt.Sprint(MirrorCatalog{}) {
		t.Fatalf("a catalog with no read time: %v, stored %s", err, named())
	}
	if changed, commits, err := write([]MirrorHistory{webHistory("10001")}, user("User 2"), 10); err != nil || !changed || commits != 1 {
		t.Fatalf("history and catalog: changed %v in %d transactions, %v; want changed in one", changed, commits, err)
	}
	if got, want := named(), fmt.Sprint(user("User 2")); got != want {
		t.Fatalf("stored %s; want %s", got, want)
	}
	if changed, _, err := write(nil, user("User 2"), 11); err != nil || changed {
		t.Fatalf("the same catalog again: changed %v, %v", changed, err)
	}
	if changed, commits, err := write(nil, user("User Two"), 12); err != nil || !changed || commits != 1 || named() != fmt.Sprint(user("User Two")) {
		t.Fatalf("a renamed user alone: changed %v in %d transactions, %v; stored %s", changed, commits, err, named())
	}
}
