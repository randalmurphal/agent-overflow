package jiraprovider

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
)

// The cost test runs every Provider and TimeTracker call over the real
// jira and tempo clients against the stateful fake at realistic sizes (a
// board of 500 issues, one issue with 60 comments, 20 issues with 100
// changes each, a month of worklogs) and pins the requests each call
// makes, in order, so a change to a call's request count fails here.
// The table it logs (go test -v -run TestRequestCost) also gives the
// answer bytes each call read.

func TestRequestCost(t *testing.T) {
	r := newCostRun(t, true)
	p := r.p
	ctx := t.Context()
	now := r.clock.Now()
	window := now.Add(-scenarioWindow)

	r.measure("Identity", func() error { _, err := p.Identity(ctx); return err })
	r.measure("Projects", func() error { _, err := p.Projects(ctx); return err })
	r.measure("IssueTypes", func() error { _, err := p.IssueTypes(ctx, "10000"); return err })
	r.measure("Boards", func() error { _, err := p.Boards(ctx, "10000"); return err })
	r.measure("LinkTypes", func() error { _, err := p.LinkTypes(ctx); return err })
	r.measure("Structure (cold)", func() error { _, err := p.Structure(ctx, costBoard, window); return err })
	r.measure("Structure (again)", func() error { _, err := p.Structure(ctx, costBoard, window); return err })

	// The first delta walk of the 500-issue board, then idle ticks.
	var cursor tracker.Cursor
	var walked int
	r.measure("ChangedRows walk of 500", func() error {
		for {
			page, err := p.ChangedRows(ctx, tracker.DeltaQuery{Board: costBoard, From: cursor, Start: window})
			if err != nil {
				return err
			}
			walked += len(page.Rows)
			cursor = page.Next
			if !page.More {
				return nil
			}
		}
	})
	if walked != costIssues {
		t.Fatalf("the walk delivered %d rows, want %d", walked, costIssues)
	}
	idleRows := make([]int, 3)
	for i := range idleRows {
		r.clock.advance(45 * time.Second)
		r.measure(fmt.Sprintf("ChangedRows idle tick %d", i+1), func() error {
			page, err := p.ChangedRows(ctx, tracker.DeltaQuery{Board: costBoard, From: cursor, Start: window})
			cursor = page.Next
			idleRows[i] = len(page.Rows)
			return err
		})
	}
	t.Logf("rows each idle tick delivers again: %v", idleRows)
	// The fixture's newest update is older than the overlap before the
	// fake's clock, so an idle board delivers nothing.
	if slices.ContainsFunc(idleRows, func(n int) bool { return n != 0 }) {
		t.Errorf("idle ticks delivered rows again: %v", idleRows)
	}

	r.measure("BoardIssues (500)", func() error {
		l, err := p.BoardIssues(ctx, costBoard, window)
		if err == nil && len(l.IDs) != costIssues {
			return fmt.Errorf("listed %d", len(l.IDs))
		}
		return err
	})
	r.measure("BoardRows (100)", func() error { _, err := p.BoardRows(ctx, costBoard, costIDs(0, 100)); return err })
	r.measure("BoardRows (500)", func() error { _, err := p.BoardRows(ctx, costBoard, costIDs(0, 500)); return err })
	r.measure("Cards (100)", func() error { _, err := p.Cards(ctx, costIDs(0, 100)); return err })
	r.measure("Cards (250)", func() error { _, err := p.Cards(ctx, costIDs(0, 250)); return err })
	r.measure("Cards (500)", func() error { _, err := p.Cards(ctx, costIDs(0, 500)); return err })
	// The 501st issue is not on the site; it is the second batch's.
	r.measure("Cards (501)", func() error { _, err := p.Cards(ctx, costIDs(0, 501)); return err })
	r.measure("History (20 x 100 changes)", func() error {
		_, err := p.History(ctx, tracker.HistoryQuery{Issues: costIDs(0, costHistories), Boards: []tracker.BoardID{costBoard}})
		return err
	})
	r.measure("History (100, 80 unchanged)", func() error {
		_, err := p.History(ctx, tracker.HistoryQuery{Issues: costIDs(0, 100), Boards: []tracker.BoardID{costBoard}})
		return err
	})
	first := tracker.IssueID(costIssueID(0))
	var next tracker.PageToken
	r.measure("Detail (60 comments)", func() error {
		d, err := p.Detail(ctx, first)
		next = d.Comments.Next
		return err
	})
	r.measure("Comments (next page)", func() error { _, err := p.Comments(ctx, first, next); return err })
	var moves []tracker.Move
	r.measure("Moves", func() error { var err error; moves, err = p.Moves(ctx, first); return err })
	r.measure("CreateFields", func() error { _, err := p.CreateFields(ctx, "10000", "10002"); return err })
	r.measure("AssignableUsers", func() error { _, err := p.AssignableUsers(ctx, tracker.OnIssue(first), "User"); return err })
	r.measure("Choices (priority)", func() error {
		_, err := p.Choices(ctx, tracker.OnIssue(first), tracker.FieldPriority, "")
		return err
	})
	r.measure("Lookup (10 keys, 3 missing)", func() error {
		keys := []string{"WEB-1", "WEB-2", "WEB-3", "WEB-4", "WEB-5", "WEB-6", "WEB-7", "WEB-9001", "WEB-9002", "WEB-9003"}
		_, err := p.Lookup(ctx, keys)
		return err
	})
	r.measure("Search", func() error {
		_, err := p.Search(ctx, tracker.SearchQuery{Text: "ordinary", Project: "10000", Limit: 50})
		return err
	})

	// Structure on one board drops that board's cached configuration and
	// sprint listing only, so another board's reads keep theirs.
	r.measure("ChangedRows board 2 (cold)", func() error {
		_, err := p.ChangedRows(ctx, tracker.DeltaQuery{Board: costOther, Start: now.Add(-time.Hour)})
		return err
	})
	r.measure("Structure board 1 (warm 2)", func() error { _, err := p.Structure(ctx, costBoard, window); return err })
	r.measure("ChangedRows board 2 (after)", func() error {
		_, err := p.ChangedRows(ctx, tracker.DeltaQuery{Board: costOther, Start: now.Add(-time.Hour)})
		return err
	})
	// A new issue placed on two boards: one match, one bulk fetch. A rows
	// read whose list leaves an issue out confirms it with one match.
	r.measure("NewRows (2 boards)", func() error {
		pl, err := p.NewRows(ctx, first, []tracker.BoardID{costBoard, costOther})
		if err == nil && len(pl.Rows)+len(pl.Off) != 2 {
			err = fmt.Errorf("placement %+v", pl)
		}
		return err
	})
	r.measure("BoardRows (100, 1 not on the board)", func() error {
		rows, err := p.BoardRows(ctx, costBoard, append(costIDs(0, 100), "99999999"))
		if err == nil && (len(rows.Rows) != 100 || len(rows.Missing) != 1) {
			err = fmt.Errorf("%d rows, missing %v", len(rows.Rows), rows.Missing)
		}
		return err
	})

	// Writes.
	dir := t.TempDir()
	for _, name := range []string{"a.png", "b.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), pngBytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.result("Create (two images)", func() tracker.Result {
		return p.Create(ctx, tracker.CreateInput{Project: "10000", Type: "10002", Title: "Created", ImageDir: dir,
			Fields: []tracker.FieldEdit{{Field: tracker.FieldDescription, Value: tracker.MarkdownEdit("![a](a.png) ![b](b.png)", "")}}})
	})
	r.result("Edit (title, assignee)", func() tracker.Result {
		return p.Edit(ctx, tracker.EditInput{Issue: costIDs(1, 1)[0], Fields: []tracker.FieldEdit{
			{Field: tracker.FieldTitle, Value: tracker.TypedValues("Renamed")},
			{Field: tracker.FieldAssignee, Value: tracker.PickValues("acc-user2")},
		}})
	})
	done := slices.IndexFunc(moves, func(m tracker.Move) bool { return m.To == "10001" || m.To == "10002" })
	if done < 0 {
		t.Fatalf("moves %+v", moves)
	}
	r.result("Move", func() tracker.Result { return p.Move(ctx, tracker.MoveInput{Issue: first, Move: moves[done].ID}) })
	eight := 8.0
	r.result("SetEstimate", func() tracker.Result {
		return p.SetEstimate(ctx, tracker.EstimateInput{Board: costBoard, Issue: costIDs(2, 1)[0], Value: &eight})
	})
	r.result("Rank (5)", func() tracker.Result {
		return p.Rank(ctx, tracker.RankInput{Board: costBoard, Issues: costIDs(10, 5), Next: costIDs(3, 1)[0]})
	})
	r.result("MoveToSprint (30)", func() tracker.Result {
		return p.MoveToSprint(ctx, tracker.SprintMoveInput{Board: costBoard, Sprint: "3", Issues: costIDs(100, 30)})
	})
	r.result("MoveToSprint backlog (30)", func() tracker.Result {
		return p.MoveToSprint(ctx, tracker.SprintMoveInput{Board: costBoard, Issues: costIDs(100, 30)})
	})
	r.result("AddComment", func() tracker.Result {
		return p.AddComment(ctx, tracker.CommentInput{Issue: first, Markdown: "A new comment"})
	})
	cs, err := p.Comments(ctx, first, "")
	if err != nil {
		t.Fatal(err)
	}
	r.result("EditComment", func() tracker.Result {
		return p.EditComment(ctx, tracker.CommentEdit{Issue: first, Comment: cs.Comments[0].ID, Markdown: "Edited", From: cs.Comments[0].Body})
	})
	r.result("DeleteComment", func() tracker.Result {
		return p.DeleteComment(ctx, tracker.CommentDelete{Issue: first, Comment: cs.Comments[0].ID})
	})
	r.result("Link", func() tracker.Result {
		return p.Link(ctx, tracker.LinkInput{Type: "10000", Outward: costIDs(5, 1)[0], Inward: costIDs(6, 1)[0]})
	})
	d, err := p.Detail(ctx, costIDs(5, 1)[0])
	if err != nil || len(d.Links) != 1 {
		t.Fatalf("links %+v, %v", d.Links, err)
	}
	r.result("Unlink", func() tracker.Result {
		return p.Unlink(ctx, tracker.UnlinkInput{Issue: costIDs(5, 1)[0], Link: d.Links[0].ID, Other: costIDs(6, 1)[0]})
	})
	r.result("Attach", func() tracker.Result {
		return p.Attach(ctx, tracker.AttachInput{Issue: first, Files: []tracker.File{{Name: "notes.txt", Content: strings.NewReader("hello")}}})
	})
	d, err = p.Detail(ctx, first)
	if err != nil || len(d.Attachments) == 0 {
		t.Fatalf("attachments %+v, %v", d.Attachments, err)
	}
	r.measure("Download", func() error {
		var got bytes.Buffer
		return p.Download(ctx, tracker.DownloadInput{Issue: first, Attachment: d.Attachments[0].ID, To: &got, Limit: 1 << 20})
	})
	r.result("CreateSprint", func() tracker.Result {
		return p.CreateSprint(ctx, tracker.SprintInput{Board: costBoard, Name: "Sprint new"})
	})
	renamed := "Sprint future, renamed"
	r.result("EditSprint", func() tracker.Result {
		return p.EditSprint(ctx, tracker.SprintEdit{Board: costBoard, Sprint: "2", Name: &renamed})
	})
	r.result("CompleteSprint (125 issues)", func() tracker.Result {
		return p.CompleteSprint(ctx, tracker.SprintCompletion{Board: costBoard, Sprint: "1", MoveTo: "2"})
	})
	r.result("StartSprint", func() tracker.Result {
		return p.StartSprint(ctx, tracker.SprintStart{Board: costBoard, Sprint: "2", Start: now, End: now.Add(14 * 24 * time.Hour)})
	})

	tt := p.Time()
	from := mustDate(t, now.AddDate(0, 0, -30))
	to := mustDate(t, now)
	var wls []tracker.Worklog
	r.measure("Tempo Worklogs (month)", func() error {
		var err error
		wls, err = tt.Worklogs(ctx, tracker.WorklogQuery{Author: "acc-user1", From: from, To: to})
		if err == nil && len(wls) != 20 {
			return fmt.Errorf("%d worklogs", len(wls))
		}
		return err
	})
	r.result("Tempo AddWorklog", func() tracker.Result {
		return tt.AddWorklog(ctx, tracker.WorklogInput{Issue: first, Day: to, Spent: 30 * time.Minute, Description: "Review"})
	})
	hour := 2 * time.Hour
	r.result("Tempo EditWorklog", func() tracker.Result { return tt.EditWorklog(ctx, tracker.WorklogEdit{From: wls[0], Spent: &hour}) })
	r.result("Tempo DeleteWorklog", func() tracker.Result { return tt.DeleteWorklog(ctx, tracker.WorklogDelete{From: wls[1]}) })

	r.log()
	r.pin(map[string]int{
		"Identity": 1, "Projects": 1, "IssueTypes": 1, "Boards": 1, "LinkTypes": 1,
		"Structure (cold)": 3, "Structure (again)": 3,
		"ChangedRows walk of 500": 2, "ChangedRows idle tick 1": 1, "ChangedRows idle tick 2": 1, "ChangedRows idle tick 3": 1,
		"BoardIssues (500)": 1, "BoardRows (100)": 1, "BoardRows (500)": 1, "Cards (100)": 1, "Cards (250)": 1, "Cards (500)": 1, "Cards (501)": 2,
		"History (20 x 100 changes)": 2, "History (100, 80 unchanged)": 3,
		"Detail (60 comments)": 2, "Comments (next page)": 1, "Moves": 1, "CreateFields": 1,
		"AssignableUsers": 1, "Choices (priority)": 1, "Lookup (10 keys, 3 missing)": 4, "Search": 1,
		"ChangedRows board 2 (cold)": 2, "Structure board 1 (warm 2)": 3, "ChangedRows board 2 (after)": 1,
		"NewRows (2 boards)": 2, "BoardRows (100, 1 not on the board)": 2,
		"Create (two images)": 7, "Edit (title, assignee)": 3, "Move": 2, "SetEstimate": 2, "Rank (5)": 2,
		"MoveToSprint (30)": 3, "MoveToSprint backlog (30)": 2, "AddComment": 2, "EditComment": 2, "DeleteComment": 2,
		"Link": 1, "Unlink": 2, "Attach": 1, "Download": 2,
		"CreateSprint": 1, "EditSprint": 2, "CompleteSprint (125 issues)": 7, "StartSprint": 2,
		"Tempo Worklogs (month)": 1, "Tempo AddWorklog": 3, "Tempo EditWorklog": 4, "Tempo DeleteWorklog": 2,
	})
	// A full bulk fetch batch's answer fits the transport's pooled answer
	// buffer (trackerapi's answerBufferKeep, 2 MiB), so reading it keeps the
	// buffer for the next read.
	if n := r.largest["Cards (500)"]; n > 2<<20 {
		t.Errorf("a 500-issue bulk fetch answered %d bytes, past the 2 MiB pooled buffer", n)
	}
	// Each read's bytes sent and read: the value measured on the fake,
	// which answers the same bytes every run, with 5% headroom, rounded up
	// to ten bytes.
	r.pinBytes(map[string]int{
		"Identity": 470, "Projects": 320, "IssueTypes": 840, "Boards": 630, "LinkTypes": 330, // 446, 303, 793, 598, 312
		"Structure (cold)": 17480, "Structure (again)": 17480, "Structure board 1 (warm 2)": 17480, // 16641
		"ChangedRows walk of 500": 964970,                                                           // 919015
		"ChangedRows idle tick 1": 60, "ChangedRows idle tick 2": 60, "ChangedRows idle tick 3": 60, // 48
		"ChangedRows board 2 (cold)": 830, "ChangedRows board 2 (after)": 60, // 790, 48
		"NewRows (2 boards)": 2450, "BoardRows (100, 1 not on the board)": 188260, // 2333, 179290
		"BoardIssues (500)": 61370, "BoardRows (100)": 188100, "BoardRows (500)": 958730, // 58439, 179135, 913074
		"Cards (100)": 179880, "Cards (250)": 449850, // 171314, 428420
		"Cards (500)": 899490, "Cards (501)": 899720, // 856653, 856872
		"History (20 x 100 changes)": 1332940, "History (100, 80 unchanged)": 1367170, // 1269458, 1302059
		"Detail (60 comments)": 159140, "Comments (next page)": 15510, // 151643, 14763
		"Moves": 1350, "CreateFields": 3340, "AssignableUsers": 1410, "Choices (priority)": 3190, // 1283, 3180, 1342, 3036
		"Lookup (10 keys, 3 missing)": 13160, "Search": 89730, // 12533, 85454
		"Tempo Worklogs (month)": 12430, // 11837
	})
}

// TestRequestCostJiraWorklogs measures logged time over Jira's own
// worklogs, which a connection without Tempo uses.
func TestRequestCostJiraWorklogs(t *testing.T) {
	r := newCostRun(t, false)
	ctx := t.Context()
	now := r.clock.Now()
	tt := r.p.Time()
	from, to := mustDate(t, now.AddDate(0, 0, -30)), mustDate(t, now)
	var wls []tracker.Worklog
	r.measure("Jira Worklogs (month)", func() error {
		var err error
		wls, err = tt.Worklogs(ctx, tracker.WorklogQuery{Author: "acc-user1", From: from, To: to})
		if err == nil && len(wls) != 20 {
			return fmt.Errorf("%d worklogs", len(wls))
		}
		return err
	})
	r.result("Jira AddWorklog", func() tracker.Result {
		return tt.AddWorklog(ctx, tracker.WorklogInput{Issue: tracker.IssueID(costIssueID(0)), Day: to, Spent: 30 * time.Minute, Description: "Review"})
	})
	hour := 2 * time.Hour
	r.result("Jira EditWorklog", func() tracker.Result { return tt.EditWorklog(ctx, tracker.WorklogEdit{From: wls[0], Spent: &hour}) })
	r.result("Jira DeleteWorklog", func() tracker.Result { return tt.DeleteWorklog(ctx, tracker.WorklogDelete{From: wls[1]}) })
	r.log()
	r.pin(map[string]int{"Jira Worklogs (month)": 2, "Jira AddWorklog": 2, "Jira EditWorklog": 2, "Jira DeleteWorklog": 2})
	// Measured 28014 bytes; bound as in TestRequestCost.
	r.pinBytes(map[string]int{"Jira Worklogs (month)": 29420})
}

func mustDate(t *testing.T, at time.Time) tracker.Date {
	t.Helper()
	d, err := tracker.DateOf(at)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
