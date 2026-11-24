package jiraprovider

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// The scenario runs the Provider over the jira and tempo clients against
// the stateful fake Jira and Tempo in internal/harness/trackerfake: it
// lists and reads boards and tickets, makes every write and reads back
// what each changed, and checks that a delta read returns exactly the
// issues the writes changed.

func TestFakeScenario(t *testing.T) {
	s := newScenario(t, true)
	t0 := s.clock.Now()
	window := t0.Add(-30 * 24 * time.Hour)

	// Reads: who, where, which boards, a board, a ticket.
	if u := scenarioMust[tracker.User](t)(s.p.Identity(s.ctx)); u.ID != "acc-user1" {
		t.Fatalf("identity %+v", u)
	}
	if ps := scenarioMust[[]tracker.Project](t)(s.p.Projects(s.ctx)); len(ps) != 1 || ps[0].Key != "WEB" {
		t.Fatalf("projects %+v", ps)
	}
	if ts := scenarioMust[[]tracker.IssueType](t)(s.p.IssueTypes(s.ctx, "10000")); len(ts) < 3 {
		t.Fatalf("issue types %+v", ts)
	}
	boards := scenarioMust[[]tracker.Board](t)(s.p.Boards(s.ctx, "10000"))
	if len(boards) != 1 || boards[0].ID != "1" || !boards[0].Sprints {
		t.Fatalf("boards %+v", boards)
	}
	st := scenarioMust[tracker.Structure](t)(s.p.Structure(s.ctx, "1", window))
	if len(st.Columns) != 3 || len(st.Sprints) != 2 || st.Estimate.Unit != tracker.UnitPoints {
		t.Fatalf("structure %+v", st)
	}
	if lts := scenarioMust[[]tracker.LinkType](t)(s.p.LinkTypes(s.ctx)); len(lts) != 2 {
		t.Fatalf("link types %+v", lts)
	}
	listing := scenarioMust[tracker.Listing](t)(s.p.BoardIssues(s.ctx, "1", window))
	slices.Sort(listing.IDs)
	if !listing.Complete || !slices.Equal(listing.IDs, []tracker.IssueID{"10101", "10102", "10103"}) {
		t.Fatalf("listing %+v", listing)
	}
	rows := scenarioMust[tracker.Rows](t)(s.p.BoardRows(s.ctx, "1", listing.IDs, listing.Rule))
	if len(rows.Rows) != 3 || len(rows.Missing) != 0 {
		t.Fatalf("rows %+v", rows)
	}
	if r := s.row(t, "10101"); r.Estimate == nil || *r.Estimate != 5 || r.Card.Status != "10001" || !slices.Equal(r.Card.Sprints, []tracker.SprintID{"1"}) {
		t.Fatalf("row %+v", r)
	}
	cards := scenarioMust[tracker.Cards](t)(s.p.Cards(s.ctx, []tracker.IssueID{"10102", "10199"}))
	if !slices.Equal(cardIDs(cards.Cards), []tracker.IssueID{"10102"}) || !slices.Equal(cards.Missing, []tracker.IssueID{"10199"}) {
		t.Fatalf("cards %+v", cards)
	}
	d := s.detail(t, "10101")
	if d.Card.Key != "WEB-1" || strings.TrimSpace(d.Description) != "first" || len(d.Comments.Comments) != 1 || len(d.History) == 0 || len(d.Moves) == 0 ||
		IssueURL("https://"+scenarioSite, d.Card.Key) != "https://"+scenarioSite+"/browse/WEB-1" {
		t.Fatalf("detail %+v", d)
	}
	if cs := scenarioMust[tracker.CommentPage](t)(s.p.Comments(s.ctx, "10101", "")); len(cs.Comments) != 1 {
		t.Fatalf("comments %+v", cs)
	}
	if l := scenarioMust[tracker.KeyLookup](t)(s.p.Lookup(s.ctx, []string{"WEB-2", "WEB-99"})); len(l.Found) != 1 || !slices.Equal(l.Unmatched, []string{"WEB-99"}) {
		t.Fatalf("lookup %+v", l)
	}
	if sp := scenarioMust[tracker.SearchPage](t)(s.p.Search(s.ctx, tracker.SearchQuery{Text: "task", Project: "10000"})); len(sp.Cards) != 2 {
		t.Fatalf("search %+v", sp)
	}
	if fs := scenarioMust[[]tracker.Field](t)(s.p.CreateFields(s.ctx, "10000", "10001")); len(fs) == 0 {
		t.Fatalf("create fields %+v", fs)
	}
	if us := scenarioMust[tracker.UserList](t)(s.p.AssignableUsers(s.ctx, tracker.OnIssue("10101"), "User")); len(us.Users) != 2 {
		t.Fatalf("assignable %+v", us)
	}
	if cs := scenarioMust[tracker.ChoiceList](t)(s.p.Choices(s.ctx, tracker.OnIssue("10101"), tracker.FieldPriority, "")); len(cs.Choices) == 0 {
		t.Fatalf("priority choices %+v", cs)
	}

	// The first delta walk delivers every issue on the board; the cursor
	// it ends with sees only what changes after it.
	first, cursor := s.drain(t, nil, window)
	if !slices.Equal(rowIDs(first), []tracker.IssueID{"10101", "10102", "10103"}) {
		t.Fatalf("first walk %v", rowIDs(first))
	}
	s.clock.advance(time.Minute)

	// Writes, each read back.
	created := allApplied(t, "create", s.p.Create(s.ctx, tracker.CreateInput{Project: "10000", Type: "10002", Title: "Created task",
		Fields: []tracker.FieldEdit{{Field: tracker.FieldDescription, Value: tracker.MarkdownEdit("Body text", "")}}}))
	newID, ok := created.Created()
	if !ok {
		t.Fatal("create named no issue")
	}
	if d := s.detail(t, newID); d.Card.Title != "Created task" || strings.TrimSpace(d.Description) != "Body text" {
		t.Fatalf("created %+v", d.Card)
	}

	allApplied(t, "edit", s.p.Edit(s.ctx, tracker.EditInput{Issue: "10102", Fields: []tracker.FieldEdit{
		{Field: tracker.FieldTitle, Value: tracker.TypedValues("Second task, renamed")},
		{Field: tracker.FieldAssignee, Value: tracker.PickValues("acc-user2")},
		{Field: tracker.FieldLabels, Value: tracker.TypedValues("frontend")},
	}}))
	if c := s.detail(t, "10102").Card; c.Title != "Second task, renamed" || c.Assignee != "acc-user2" || len(c.Labels) != 1 {
		t.Fatalf("edited %+v", c)
	}

	moves := scenarioMust[[]tracker.Move](t)(s.p.Moves(s.ctx, "10101"))
	done := slices.IndexFunc(moves, func(m tracker.Move) bool { return m.To == "10002" })
	if done < 0 {
		t.Fatalf("moves %+v", moves)
	}
	allApplied(t, "move", s.p.Move(s.ctx, tracker.MoveInput{Issue: "10101", Move: moves[done].ID}))
	if c := s.detail(t, "10101").Card; c.Status != "10002" || c.Resolved.IsZero() {
		t.Fatalf("moved %+v", c)
	}

	eight := 8.0
	allApplied(t, "estimate", s.p.SetEstimate(s.ctx, tracker.EstimateInput{Board: "1", Issue: "10102", Value: &eight}))
	if r := s.row(t, "10102"); r.Estimate == nil || *r.Estimate != 8 {
		t.Fatalf("estimated %+v", r)
	}

	allApplied(t, "rank", s.p.Rank(s.ctx, tracker.RankInput{Board: "1", Issues: []tracker.IssueID{"10102"}, Next: "10101"}))
	if r2, r1 := s.row(t, "10102"), s.row(t, "10101"); r2.Order >= r1.Order {
		t.Fatalf("ranked %s not before %s", r2.Order, r1.Order)
	}

	allApplied(t, "sprint move", s.p.MoveToSprint(s.ctx, tracker.SprintMoveInput{Board: "1", Sprint: "2", Issues: []tracker.IssueID{"10103"}}))
	if c := s.row(t, "10103").Card; !slices.Equal(c.Sprints, []tracker.SprintID{"2"}) {
		t.Fatalf("sprint move %+v", c)
	}

	allApplied(t, "comment", s.p.AddComment(s.ctx, tracker.CommentInput{Issue: "10102", Markdown: "New comment"}))
	cs := scenarioMust[tracker.CommentPage](t)(s.p.Comments(s.ctx, "10102", ""))
	if len(cs.Comments) != 1 || strings.TrimSpace(cs.Comments[0].Body) != "New comment" {
		t.Fatalf("comments %+v", cs)
	}
	allApplied(t, "comment edit", s.p.EditComment(s.ctx, tracker.CommentEdit{Issue: "10102", Comment: cs.Comments[0].ID, Markdown: "Edited comment", From: cs.Comments[0].Body}))
	if cs := scenarioMust[tracker.CommentPage](t)(s.p.Comments(s.ctx, "10102", "")); strings.TrimSpace(cs.Comments[0].Body) != "Edited comment" {
		t.Fatalf("edited comment %+v", cs)
	}
	allApplied(t, "comment delete", s.p.DeleteComment(s.ctx, tracker.CommentDelete{Issue: "10102", Comment: cs.Comments[0].ID}))
	if cs := scenarioMust[tracker.CommentPage](t)(s.p.Comments(s.ctx, "10102", "")); len(cs.Comments) != 0 {
		t.Fatalf("deleted comment %+v", cs)
	}

	allApplied(t, "link", s.p.Link(s.ctx, tracker.LinkInput{Type: "10000", Outward: "10102", Inward: "10103"}))
	links := s.detail(t, "10102").Links
	if len(links) != 1 || links[0].Issue.ID != "10103" || !links[0].Outward {
		t.Fatalf("links %+v", links)
	}
	allApplied(t, "unlink", s.p.Unlink(s.ctx, tracker.UnlinkInput{Issue: "10102", Link: links[0].ID, Other: "10103"}))
	if links := s.detail(t, "10103").Links; len(links) != 0 {
		t.Fatalf("unlinked %+v", links)
	}

	allApplied(t, "attach", s.p.Attach(s.ctx, tracker.AttachInput{Issue: "10102", Files: []tracker.File{{Name: "notes.txt", Content: strings.NewReader("hello"), Size: 5}}}))
	atts := s.detail(t, "10102").Attachments
	if len(atts) != 1 || atts[0].Name != "notes.txt" {
		t.Fatalf("attachments %+v", atts)
	}
	var got bytes.Buffer
	if err := s.p.Download(s.ctx, tracker.DownloadInput{Issue: "10102", Attachment: atts[0].ID, To: &got}); err != nil || got.String() != "hello" {
		t.Fatalf("download %q, %v", got.String(), err)
	}

	// Sprints: create, edit, complete the active one into the new one,
	// start the new one.
	allApplied(t, "sprint create", s.p.CreateSprint(s.ctx, tracker.SprintInput{Board: "1", Name: "Sprint 3", Goal: "Ship"}))
	st = scenarioMust[tracker.Structure](t)(s.p.Structure(s.ctx, "1", window))
	i3 := slices.IndexFunc(st.Sprints, func(sp tracker.Sprint) bool { return sp.Name == "Sprint 3" })
	if i3 < 0 || st.Sprints[i3].State != tracker.SprintFuture || st.Sprints[i3].Goal != "Ship" {
		t.Fatalf("sprints %+v", st.Sprints)
	}
	sprint3 := st.Sprints[i3].ID
	renamed := "Sprint 3, renamed"
	allApplied(t, "sprint edit", s.p.EditSprint(s.ctx, tracker.SprintEdit{Board: "1", Sprint: sprint3, Name: &renamed}))
	allApplied(t, "sprint complete", s.p.CompleteSprint(s.ctx, tracker.SprintCompletion{Board: "1", Sprint: "1", MoveTo: sprint3}))
	if c := s.row(t, "10102").Card; !slices.Equal(c.Sprints, []tracker.SprintID{sprint3}) {
		t.Fatalf("an unfinished issue did not move: %+v", c)
	}
	start := s.clock.Now()
	allApplied(t, "sprint start", s.p.StartSprint(s.ctx, tracker.SprintStart{Board: "1", Sprint: sprint3, Start: start, End: start.Add(14 * 24 * time.Hour)}))
	st = scenarioMust[tracker.Structure](t)(s.p.Structure(s.ctx, "1", window))
	states := map[tracker.SprintID]tracker.SprintState{}
	for _, sp := range st.Sprints {
		states[sp.ID] = sp.State
	}
	if states["1"] != tracker.SprintClosed || states[sprint3] != tracker.SprintActive {
		t.Fatalf("sprints %+v", st.Sprints)
	}

	// Logged time through Tempo.
	tt := s.p.Time()
	today := scenarioMust[tracker.Date](t)(tracker.DateOf(t0))
	allApplied(t, "worklog add", tt.AddWorklog(s.ctx, tracker.WorklogInput{Issue: "10102", Day: today, Spent: 30 * time.Minute, Description: "Review"}))
	wls := scenarioMust[[]tracker.Worklog](t)(tt.Worklogs(s.ctx, tracker.WorklogQuery{Author: "acc-user1", From: today, To: today}))
	if len(wls) != 1 || wls[0].Issue != "10102" || wls[0].Spent != 30*time.Minute {
		t.Fatalf("worklogs %+v", wls)
	}
	hour := time.Hour
	allApplied(t, "worklog edit", tt.EditWorklog(s.ctx, tracker.WorklogEdit{From: wls[0], Spent: &hour}))
	wls = scenarioMust[[]tracker.Worklog](t)(tt.Worklogs(s.ctx, tracker.WorklogQuery{Author: "acc-user1", From: today, To: today}))
	if len(wls) != 1 || wls[0].Spent != time.Hour {
		t.Fatalf("edited worklogs %+v", wls)
	}
	allApplied(t, "worklog delete", tt.DeleteWorklog(s.ctx, tracker.WorklogDelete{From: wls[0]}))
	if wls := scenarioMust[[]tracker.Worklog](t)(tt.Worklogs(s.ctx, tracker.WorklogQuery{Author: "acc-user1", From: today, To: today})); len(wls) != 0 {
		t.Fatalf("deleted worklogs %+v", wls)
	}

	// History shows the status, sprint and assignee changes; estimates
	// are TestFakeScenarioEstimateHistory's.
	h := s.history(t, "10101", "10102")
	if st := h["10101"].Status; len(st) != 2 || st[1].To != "10002" {
		t.Fatalf("status history %+v", st)
	}
	if as := h["10102"].Assignee; len(as) != 1 || as[0].To != "acc-user2" {
		t.Fatalf("assignee history %+v", as)
	}
	if sp := h["10102"].Sprint; len(sp) != 1 || !slices.Equal(sp[0].To, []tracker.SprintID{sprint3}) {
		t.Fatalf("sprint history %+v", sp)
	}

	// The delta from the first walk's cursor is exactly the issues the
	// writes changed.
	delta, _ := s.drain(t, cursor, window)
	want := []tracker.IssueID{"10101", "10102", "10103", newID}
	slices.Sort(want)
	if !slices.Equal(rowIDs(delta), want) {
		t.Fatalf("delta %v, want %v", rowIDs(delta), want)
	}
}

// TestFakeScenarioEstimateHistory reads back an estimate write as the
// board's estimate history. The fake records a custom number field's
// change in fromString and toString with from and to null, as Jira Cloud
// is believed to (unverified); the Provider reads only from and to.
func TestFakeScenarioEstimateHistory(t *testing.T) {
	s := newScenario(t, false)
	eight := 8.0
	allApplied(t, "estimate", s.p.SetEstimate(s.ctx, tracker.EstimateInput{Board: "1", Issue: "10101", Value: &eight}))
	es := s.history(t, "10101")["10101"].Estimate
	if len(es) != 1 || es[0].From == nil || *es[0].From != 5 || es[0].To == nil || *es[0].To != 8 {
		t.Fatalf("estimate history %+v, want one change from 5 to 8", es)
	}
}

// TestFakeScenarioJiraWorklogs runs logged time over Jira's own worklogs,
// as a connection without Tempo does.
func TestFakeScenarioJiraWorklogs(t *testing.T) {
	s := newScenario(t, false)
	tt := s.p.Time()
	today := scenarioMust[tracker.Date](t)(tracker.DateOf(s.clock.Now()))
	allApplied(t, "worklog add", tt.AddWorklog(s.ctx, tracker.WorklogInput{Issue: "10102", Day: today, Spent: 30 * time.Minute, Description: "Review"}))
	q := tracker.WorklogQuery{Author: "acc-user1", From: today, To: today}
	wls := scenarioMust[[]tracker.Worklog](t)(tt.Worklogs(s.ctx, q))
	if len(wls) != 1 || wls[0].Issue != "10102" || wls[0].Spent != 30*time.Minute {
		t.Fatalf("worklogs %+v", wls)
	}
	hour := time.Hour
	allApplied(t, "worklog edit", tt.EditWorklog(s.ctx, tracker.WorklogEdit{From: wls[0], Spent: &hour}))
	if wls = scenarioMust[[]tracker.Worklog](t)(tt.Worklogs(s.ctx, q)); len(wls) != 1 || wls[0].Spent != time.Hour {
		t.Fatalf("edited worklogs %+v", wls)
	}
	allApplied(t, "worklog delete", tt.DeleteWorklog(s.ctx, tracker.WorklogDelete{From: wls[0]}))
	if wls := scenarioMust[[]tracker.Worklog](t)(tt.Worklogs(s.ctx, q)); len(wls) != 0 {
		t.Fatalf("deleted worklogs %+v", wls)
	}
}

// TestDetailReadsNothingBeforeItsVersion proves the comments a detail
// holds are read after the issue read that gives its card's update time
// (tracker.Capabilities.DetailMovesUpdated): a comment added while the
// issue's read is in flight, which moves that time, shows in the detail
// whose card has the moved time. The issue's read is held until the
// comment lands; a comments read made meanwhile, as when the two ran
// together, is answered before it, and the detail would pair the new
// update time with the old comments, which a later check of the card
// would then certify.
func TestDetailReadsNothingBeforeItsVersion(t *testing.T) {
	const issueRoute, commentsRoute = "GET rest/api/3/issue/{issue}", "GET rest/api/3/issue/{issue}/comment"
	seen := make(chan trackerfake.Request, 1024)
	s := newObservedScenario(t, false, func(r trackerfake.Request) {
		select {
		case seen <- r:
		default:
			t.Error("the requests outran the test")
		}
	})
	origin := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: scenarioSite}
	if err := s.e.AddFault(trackerfake.Fault{Origin: origin, Route: issueRoute, Kind: trackerfake.FaultHold, Times: 1}); err != nil {
		t.Fatal(err)
	}
	type read struct {
		d   tracker.Detail
		err error
	}
	done := make(chan read, 1)
	go func() {
		d, err := s.p.Detail(s.ctx, "10102")
		done <- read{d, err}
	}()
	for r := range seen {
		if r.Route == issueRoute && r.Held {
			break
		}
	}
	// A comments read sent with the issue's arrives at once; the grace
	// bounds the wait for one that is never sent.
	early := false
	grace := time.After(300 * time.Millisecond)
wait:
	for {
		select {
		case r := <-seen:
			if r.Route == commentsRoute && r.Status != 0 {
				early = true
				break wait
			}
		case <-grace:
			break wait
		}
	}
	s.clock.advance(time.Minute)
	allApplied(t, "comment", s.p.AddComment(s.ctx, tracker.CommentInput{Issue: "10102", Markdown: "Added while read"}))
	if err := s.e.Release(origin, issueRoute, false); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	if early {
		t.Error("the comments were read while the issue's read was in flight")
	}
	if !got.d.Card.Updated.Equal(s.clock.Now()) {
		t.Fatalf("the card's update time is %v, not the comment's %v", got.d.Card.Updated, s.clock.Now())
	}
	if !slices.ContainsFunc(got.d.Comments.Comments, func(c tracker.Comment) bool { return strings.TrimSpace(c.Body) == "Added while read" }) {
		t.Errorf("the card has the update time the comment moved it to, and the comments lack it: %+v", got.d.Comments.Comments)
	}
}
