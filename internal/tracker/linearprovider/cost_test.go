package linearprovider

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
)

// The cost test runs every Provider call over the real linear client
// against the stateful fake at realistic sizes (a team of 500 issues,
// one issue with 60 comments, 20 issues with 100 history entries each)
// and pins the requests each call makes, by GraphQL operation, and the
// points the fake charges them (its X-Complexity, fitted to Linear's
// measured charges in trackerfake's linear_cost.go), so a change to a
// call's request count or cost fails here. The table it logs (go test
// -v -run TestRequestCost) also gives the answer bytes and the
// connections each call opened.

func TestRequestCost(t *testing.T) {
	r, clock := newCostRun(t)
	p := r.p
	ctx := t.Context()
	now := clock.Now()
	window := now.Add(-180 * 24 * time.Hour)
	board := tracker.BoardID(costTeam)

	r.measure("Identity (first call)", func() error { _, err := p.Identity(ctx); return err })
	r.measure("Identity (again)", func() error { _, err := p.Identity(ctx); return err })
	// The workspace check and the call it admits share one connection.
	if n := r.conns["Identity (first call)"]; n != 1 {
		t.Errorf("the first Identity opened %d connections, want 1", n)
	}
	r.measure("Projects", func() error { _, err := p.Projects(ctx); return err })
	r.measure("Boards", func() error { _, err := p.Boards(ctx, tracker.ProjectID(costTeam)); return err })
	r.measure("Structure", func() error { _, err := p.Structure(ctx, board, window); return err })

	var cursor tracker.Cursor
	walked := 0
	r.measure("ChangedRows walk of 500", func() error {
		for {
			page, err := p.ChangedRows(ctx, tracker.DeltaQuery{Board: board, From: cursor, Start: window})
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
	idle := make([]int, 3)
	for i := range idle {
		clock.advance(45 * time.Second)
		r.measure(fmt.Sprintf("ChangedRows idle tick %d", i+1), func() error {
			page, err := p.ChangedRows(ctx, tracker.DeltaQuery{Board: board, From: cursor, Start: window})
			cursor, idle[i] = page.Next, len(page.Rows)
			return err
		})
	}
	if !slices.Equal(idle, []int{0, 0, 0}) {
		t.Errorf("rows each idle tick delivered again: %v, want none", idle)
	}

	r.measure("BoardIssues (500)", func() error {
		l, err := p.BoardIssues(ctx, board, window)
		if err == nil && len(l.IDs) != costIssues {
			return fmt.Errorf("listed %d", len(l.IDs))
		}
		return err
	})
	r.measure("BoardRows (100)", func() error { _, err := p.BoardRows(ctx, board, costIDs(0, 100), tracker.Eligibility{}); return err })
	r.measure("BoardRows (500)", func() error { _, err := p.BoardRows(ctx, board, costIDs(0, 500), tracker.Eligibility{}); return err })
	// A new issue on its team's board and off another team's: one request.
	r.measure("NewRows (2 boards)", func() error {
		pl, err := p.NewRows(ctx, tracker.IssueID(costIssueID(0)), []tracker.BoardID{board, tracker.BoardID(sid(9999))})
		if err == nil && (len(pl.Rows) != 1 || pl.Rows[0].Board != board || !slices.Equal(pl.Off, []tracker.BoardID{tracker.BoardID(sid(9999))})) {
			err = fmt.Errorf("placement %+v", pl)
		}
		return err
	})
	r.measure("Cards (100)", func() error { _, err := p.Cards(ctx, costIDs(0, 100)); return err })
	r.measure("History (20 x 100 entries)", func() error {
		_, err := p.History(ctx, tracker.HistoryQuery{Issues: costIDs(0, costHistories), Boards: []tracker.BoardID{board}})
		return err
	})
	for _, n := range []int{50, 200} {
		r.measure(fmt.Sprintf("History (%d issues)", n), func() error {
			h, err := p.History(ctx, tracker.HistoryQuery{Issues: costIDs(0, n), Boards: []tracker.BoardID{board}})
			if err == nil && len(h.Issues) != n {
				return fmt.Errorf("%d issues", len(h.Issues))
			}
			return err
		})
	}
	first := tracker.IssueID(costIssueID(0))
	var next tracker.PageToken
	r.measure("Detail (60 comments, 100 entries)", func() error {
		d, err := p.Detail(ctx, first)
		next = d.Comments.Next
		return err
	})
	r.measure("Detail (again)", func() error { _, err := p.Detail(ctx, first); return err })
	if next != "" {
		r.measure("Comments (next page)", func() error { _, err := p.Comments(ctx, first, next); return err })
	}
	r.measure("Moves", func() error { _, err := p.Moves(ctx, first); return err })
	r.measure("AssignableUsers", func() error { _, err := p.AssignableUsers(ctx, tracker.OnIssue(first), "User"); return err })
	r.measure("Choices (labels)", func() error {
		_, err := p.Choices(ctx, tracker.OnIssue(first), tracker.FieldLabels, "")
		return err
	})
	r.measure("Lookup (10 keys, all found)", func() error {
		l, err := p.Lookup(ctx, []string{"WEB-1", "WEB-2", "WEB-3", "WEB-4", "WEB-5", "WEB-6", "WEB-7", "WEB-8", "WEB-9", "WEB-10"})
		if err == nil && len(l.Found) != 10 {
			return fmt.Errorf("found %d", len(l.Found))
		}
		return err
	})
	r.measure("Lookup (10 keys, 3 missing)", func() error {
		l, err := p.Lookup(ctx, []string{"WEB-1", "WEB-2", "WEB-3", "WEB-4", "WEB-5", "WEB-6", "WEB-7", "WEB-9001", "WEB-9002", "WEB-9003"})
		if err == nil && len(l.Unmatched) != 3 {
			return fmt.Errorf("unmatched %v", l.Unmatched)
		}
		return err
	})
	r.measure("Search", func() error {
		_, err := p.Search(ctx, tracker.SearchQuery{Text: "ordinary", Project: tracker.ProjectID(costTeam), Limit: 50})
		return err
	})
	r.measure("Search (no text, 2 pages of 50)", func() error {
		pg, err := p.Search(ctx, tracker.SearchQuery{Project: tracker.ProjectID(costTeam), Limit: 50})
		if err != nil {
			return err
		}
		_, err = p.Search(ctx, tracker.SearchQuery{Project: tracker.ProjectID(costTeam), Limit: 50, Page: pg.Next})
		return err
	})

	clock.advance(time.Hour)
	var created tracker.IssueID
	r.measure("Create", func() error {
		res := p.Create(ctx, tracker.CreateInput{Project: tracker.ProjectID(costTeam), Title: "Created",
			Fields: []tracker.FieldEdit{{Field: tracker.FieldDescription, Value: tracker.MarkdownEdit("Body text", "")}}})
		var ok bool
		if created, ok = res.Created(); !ok {
			return fmt.Errorf("create named no issue: %+v", res)
		}
		return nil
	})
	r.result("Edit (title, assignee, label)", func() tracker.Result {
		return p.Edit(ctx, tracker.EditInput{Issue: costIDs(1, 1)[0], Fields: []tracker.FieldEdit{
			{Field: tracker.FieldTitle, Value: tracker.TypedValues("Renamed")},
			{Field: tracker.FieldAssignee, Value: tracker.PickValues(costUsers[1])},
			{Field: tracker.FieldLabels, Value: tracker.PickValues(costLabels[0])},
		}})
	})
	r.result("Move", func() tracker.Result {
		return p.Move(ctx, tracker.MoveInput{Issue: costIDs(1, 1)[0], Move: tracker.MoveID(costStates[3])})
	})
	five := 5.0
	r.result("SetEstimate", func() tracker.Result {
		return p.SetEstimate(ctx, tracker.EstimateInput{Board: board, Issue: costIDs(2, 1)[0], Value: &five})
	})
	r.result("Rank (5)", func() tracker.Result {
		return p.Rank(ctx, tracker.RankInput{Board: board, Issues: costIDs(10, 5), Next: costIDs(3, 1)[0]})
	})
	r.result("MoveToSprint (30)", func() tracker.Result {
		return p.MoveToSprint(ctx, tracker.SprintMoveInput{Board: board, Sprint: tracker.SprintID(costNext), Issues: costIDs(100, 30)})
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
		return p.Link(ctx, tracker.LinkInput{Type: "blocks", Outward: costIDs(5, 1)[0], Inward: created})
	})
	d, err := p.Detail(ctx, costIDs(5, 1)[0])
	if err != nil || len(d.Links) != 1 {
		t.Fatalf("links %+v, %v", d.Links, err)
	}
	r.result("Unlink", func() tracker.Result {
		return p.Unlink(ctx, tracker.UnlinkInput{Issue: costIDs(5, 1)[0], Link: d.Links[0].ID, Other: created})
	})

	r.pin(map[string]pinned{
		"Identity (first call)": {2, 6}, "Identity (again)": {1, 3}, "Projects": {1, 401}, "Boards": {1, 5}, "Structure": {1, 345},
		"ChangedRows walk of 500": {2, 16}, "ChangedRows idle tick 1": {1, 8}, "ChangedRows idle tick 2": {1, 8}, "ChangedRows idle tick 3": {1, 8},
		"BoardIssues (500)": {3, 49}, "BoardRows (100)": {1, 8}, "BoardRows (500)": {2, 16}, "NewRows (2 boards)": {1, 8}, "Cards (100)": {1, 8},
		"History (20 x 100 entries)": {1, 1005}, "History (50 issues)": {1, 1005}, "History (200 issues)": {4, 4020},
		"Detail (60 comments, 100 entries)": {1, 2766}, "Detail (again)": {1, 2766}, "Comments (next page)": {1, 402},
		"Moves": {1, 104}, "AssignableUsers": {1, 104}, "Choices (labels)": {1, 604},
		"Lookup (10 keys, all found)": {1, 8}, "Lookup (10 keys, 3 missing)": {4, 47}, "Search": {1, 8}, "Search (no text, 2 pages of 50)": {2, 16},
		"Create": {1, 3}, "Edit (title, assignee, label)": {1, 3}, "Move": {2, 107}, "SetEstimate": {2, 9}, "Rank (5)": {2, 15},
		"MoveToSprint (30)": {2, 9}, "AddComment": {1, 3}, "EditComment": {2, 8}, "DeleteComment": {2, 8}, "Link": {1, 3}, "Unlink": {2, 9},
	})
}
