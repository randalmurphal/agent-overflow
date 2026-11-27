package app

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

// The tracker tools' writes end to end on the fake Jira site, in the
// sequences where a write could be sent twice, sent without authority, or
// lost from its answer.

// heldRequests counts the requests to route the fake site is holding.
func heldRequests(f *trackerBoardFixture, route string) int {
	n := 0
	for _, r := range f.e.Requests(0).Requests {
		if r.Held && r.Route == route {
			n++
		}
	}
	return n
}

// describedSite is the board site with descriptions on WEB-1 and WEB-2.
func describedSite() trackerfake.JiraSite {
	site := trackerBoardSite()
	for i, text := range []string{"The first old text.", "The second old text."} {
		site.Issues[i].Description = json.RawMessage(`{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"` + text + `"}]}]}`)
	}
	return site
}

const commentRoute = "POST rest/api/3/issue/{issue}/comment"

// Two copies of one call at once send the comment once: the second is told
// the first is still being sent.
func TestTrackerWriteOfOneCommentTwiceAtOnceSendsItOnce(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	_, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	if err := f.e.AddFault(trackerfake.Fault{Origin: trackerBoardOrigin, Route: commentRoute, Kind: trackerfake.FaultHold}); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"keys": []string{"WEB-1"}, "comment": "Checked on Safari."}
	first := trackerWriteCall(t, f.a, endpoint, "ticket_update", args)
	waitFor(t, "the first call's comment held", func() bool { return heldRequests(f, commentRoute) == 1 })
	second := trackerWriteCall(t, f.a, endpoint, "ticket_update", args)
	var secondResult [2]any
	answered := false
	waitFor(t, "the second call's answer, or its comment held", func() bool {
		select {
		case secondResult = <-second:
			answered = true
			return true
		default:
		}
		return heldRequests(f, commentRoute) == 2
	})
	f.e.ClearFaults(trackerBoardOrigin, commentRoute)
	firstResult := <-first
	if !answered {
		secondResult = <-second
	}
	if firstResult[1].(bool) || firstResult[0] != `{"tickets":[{"key":"WEB-1","applied":["comment"]}]}` {
		t.Fatalf("the first call = %v", firstResult)
	}
	if got := fakeComments(t, f, "10101"); len(got) != 1 {
		t.Fatalf("two calls at once commented %d times: %q", len(got), got)
	}
	if !strings.Contains(secondResult[0].(string), "still being sent") {
		t.Fatalf("the second call = %v, want the first still being sent", secondResult)
	}
}

// A comment whose answer never came is not settled by a person's comment
// from the same account: the same call again sends the agent's comment.
func TestTrackerWriteLostCommentIsNotAPersonsComment(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	_, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	if err := f.e.AddFault(trackerfake.Fault{Origin: trackerBoardOrigin, Route: commentRoute, Kind: trackerfake.FaultHold, Times: 1}); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"keys": []string{"WEB-1"}, "comment": "Fixed in the login form."}
	lost := trackerWriteCall(t, f.a, endpoint, "ticket_update", args)
	waitFor(t, "the agent's comment held", func() bool { return heldRequests(f, commentRoute) == 1 })
	if err := f.e.Release(trackerBoardOrigin, commentRoute, true); err != nil {
		t.Fatal(err)
	}
	if result := <-lost; !strings.Contains(result[0].(string), "may or may not have been added") {
		t.Fatalf("the lost comment = %v", result)
	}
	if _, err := f.a.AddTrackerComment(t.Context(), f.conn.ID, trackerapp.CommentRequest{Issue: "10101", Markdown: "Investigating."}); err != nil {
		t.Fatal(err)
	}
	text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_update", args)
	if isError || text != `{"tickets":[{"key":"WEB-1","applied":["comment"]}]}` {
		t.Fatalf("the same call again = %s (error %v)", text, isError)
	}
	got := fakeComments(t, f, "10101")
	if len(got) != 2 || !slices.ContainsFunc(got, func(c string) bool { return strings.Contains(c, "Fixed in the login form.") }) {
		t.Fatalf("comments %q, want the person's and the agent's", got)
	}
}

// A clear approved after a person changed one of the descriptions it
// clears leaves that description as the person wrote it.
func TestTrackerWriteClearOfADescriptionChangedWhileWaiting(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: describedSite()})
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	for _, key := range []string{"WEB-1", "WEB-2"} {
		if text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_read", map[string]any{"key": key}); isError {
			t.Fatalf("read %s = %s", key, text)
		}
	}
	done := trackerWriteCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1", "WEB-2"}, "set": map[string]any{"description": nil}})
	open, _ := trackerWritePrompt(t, f.a, thread.ID)
	if v, err := f.a.EditTrackerIssue(t.Context(), f.conn.ID, trackerapp.EditRequest{Issue: "10101", Fields: []trackerapp.FieldEditInput{
		{Field: "description", Markdown: &trackerapp.MarkdownEditInput{Markdown: "The person's text.", From: "The first old text.\n"}},
	}}); err != nil || v.Parts[0].State != "applied" {
		t.Fatalf("the person's edit = %+v, %v", v, err)
	}
	if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result[1].(bool) || !strings.Contains(result[0].(string), `{"key":"WEB-1","refused":`) || !strings.Contains(result[0].(string), `{"key":"WEB-2","applied":["description"]}`) {
		t.Fatalf("ticket_update = %v", result)
	}
	for _, site := range f.e.Snapshot().Jira {
		for _, issue := range site.Issues {
			switch issue.ID {
			case "10101":
				if !strings.Contains(string(issue.Description), "The person's text.") {
					t.Fatalf("WEB-1's description = %s, want the person's", issue.Description)
				}
			case "10102":
				if strings.Contains(string(issue.Description), "old text") {
					t.Fatalf("WEB-2's description = %s, want it cleared", issue.Description)
				}
			}
		}
	}
}

// A thread made read-only while its approval waits sends nothing when the
// person then approves.
func TestTrackerWriteApprovedAfterTheThreadBecameReadOnly(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	done := trackerWriteCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1", "WEB-2"}, "comment": "Checked."})
	open, _ := trackerWritePrompt(t, f.a, thread.ID)
	if err := f.a.store.UpdateRuntimeMode(thread.ID, string(provider.RuntimeReadOnly)); err != nil {
		t.Fatal(err)
	}
	if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if !result[1].(bool) || !strings.HasPrefix(result[0].(string), "[tracker_read_only]") {
		t.Fatalf("ticket_update = %v", result)
	}
	if got := append(fakeComments(t, f, "10101"), fakeComments(t, f, "10102")...); len(got) != 0 {
		t.Fatalf("a read-only thread commented %q", got)
	}
}

// An answer that names a tracker write's request ID on another thread
// settles nothing there: the approval stays open on its own thread, and
// the person's answer there is the one that counts.
func TestTrackerWriteApprovalAnsweredOnAnotherThread(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	other, _ := trackerWriteThread(t, f, provider.RuntimeAuto)
	done := trackerWriteCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1", "WEB-2"}, "comment": "Checked."})
	open, _ := trackerWritePrompt(t, f.a, thread.ID)
	if err := f.a.RespondToApproval(t.Context(), other.ID, provider.ApprovalResponse{RequestID: open.RequestID, Decision: "allow"}); err == nil {
		t.Fatal("an answer on another thread was taken")
	}
	select {
	case result := <-done:
		t.Fatalf("an answer on another thread settled the call: %v", result)
	default:
	}
	if !f.a.trackerApprovalOpen(thread.ID, open.RequestID) {
		t.Fatal("the approval left its thread")
	}
	if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Decision: "decline"}); err != nil {
		t.Fatal(err)
	}
	if result := <-done; !result[1].(bool) || !strings.HasPrefix(result[0].(string), "[tracker_write_denied]") {
		t.Fatalf("ticket_update = %v", result)
	}
	if got := append(fakeComments(t, f, "10101"), fakeComments(t, f, "10102")...); len(got) != 0 {
		t.Fatalf("a denied call commented %q", got)
	}
}

// An answer with no prompt occurrence, as a paired v0.0.19 screen sends
// after showing a tracker write's tool name and no change, is refused:
// nothing is written, the prompt stays open, and a screen that shows the
// change still answers it.
func TestTrackerWriteAnswerFromAScreenThatCannotShowItIsRefused(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	done := trackerWriteCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1", "WEB-2"}, "comment": "Checked."})
	open, _ := trackerWritePrompt(t, f.a, thread.ID)
	err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Decision: "accept"})
	if err == nil || errors.Is(err, transport.ErrAlreadyHandled) || !strings.Contains(err.Error(), "cannot show what this change sends") {
		t.Fatalf("an answer with no occurrence = %v; want a refusal that says the screen cannot show the change", err)
	}
	if got := append(fakeComments(t, f, "10101"), fakeComments(t, f, "10102")...); len(got) != 0 {
		t.Fatalf("an answer from a screen that showed no change commented %q", got)
	}
	if !f.a.trackerApprovalOpen(thread.ID, open.RequestID) {
		t.Fatal("the refused answer closed the approval")
	}
	if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	if result := <-done; result[1].(bool) {
		t.Fatalf("ticket_update = %v", result)
	}
	if got := append(fakeComments(t, f, "10101"), fakeComments(t, f, "10102")...); len(got) != 2 {
		t.Fatalf("the approval from a screen that shows it commented %q", got)
	}
}

// A create the tracker confirmed answers its key and writes its row when
// the ticket cannot be read after it.
func TestTrackerWriteConfirmedCreateWhoseReadFails(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	sub := f.subscribe(t, ctx)
	f.boardEvent(t, "for the first read", func(e trackerapp.BoardEvent) bool { return e.ReadAgain && e.Seq > sub.Seq })
	if err := f.e.AddFault(trackerfake.Fault{Origin: trackerBoardOrigin, Route: "GET rest/api/3/issue/{issue}", Kind: trackerfake.FaultRefuse,
		Status: 500, Body: json.RawMessage(`{"errorMessages":["down"]}`)}); err != nil {
		t.Fatal(err)
	}
	text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_create", map[string]any{"project": "WEB", "title": "Login fails on Safari"})
	if isError || text != `{"key":"WEB-4"}` {
		t.Fatalf("ticket_create = %s (error %v)", text, isError)
	}
	items, err := f.a.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows := slices.DeleteFunc(items, func(it store.Item) bool { return it.ToolName != "tracker_write" })
	if len(rows) != 1 || rows[0].Summary != "Created WEB-4" {
		t.Fatalf("timeline rows %+v", rows)
	}
}

// trackerWriteRows are the thread's tracker write timeline rows.
func trackerWriteRows(t *testing.T, f *trackerBoardFixture, threadID string) []store.Item {
	t.Helper()
	items, err := f.a.store.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(items, func(it store.Item) bool { return it.ToolName != "tracker_write" })
}

// The same call again after its comment was added sends nothing and adds
// no timeline row: the timeline says what each call changed.
func TestTrackerWriteRepeatOfAnAddedCommentWritesNoRow(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	args := map[string]any{"keys": []string{"WEB-1"}, "comment": "Checked."}
	if text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_update", args); isError {
		t.Fatalf("ticket_update = %s", text)
	}
	text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_update", args)
	if rows := trackerWriteRows(t, f, thread.ID); len(rows) != 1 {
		t.Fatalf("%d timeline rows, want the first call's alone", len(rows))
	}
	if isError || text != `{"tickets":[{"key":"WEB-1","already":["comment"],"note":"Your earlier call added this comment; it was not added again."}]}` {
		t.Fatalf("the same call again = %s (error %v)", text, isError)
	}
	if got := fakeComments(t, f, "10101"); len(got) != 1 {
		t.Fatalf("comments %q", got)
	}
}

// twoBoardSite is the board site with a second scrum board of project WEB,
// with its own active sprint.
func twoBoardSite() trackerfake.JiraSite {
	site := trackerBoardSite()
	site.Boards = append(site.Boards, trackerfake.JiraBoard{ID: 2, Name: "WEB mobile", Type: "scrum", Project: "WEB", Columns: site.Boards[0].Columns})
	site.Sprints = append(site.Sprints, trackerfake.JiraSprint{ID: 2, Board: 2, Name: "Mobile 1", State: "active", Start: site.Sprints[0].Start, End: site.Sprints[0].End})
	return site
}

// A create goes through a read board of its project when the first board
// listed has not been read.
func TestTrackerWriteCreatesThroughAReadBoardAfterAnUnreadOne(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: twoBoardSite()})
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	_, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	sub, err := f.a.SubscribeTrackerBoard(ctx, f.project, f.conn.ID, "2", []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: "2"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	f.boardEvent(t, "for board 2's first read", func(e trackerapp.BoardEvent) bool { return e.ReadAgain && e.Seq > sub.Seq && e.Scope.Board == "2" })
	text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_create", map[string]any{"project": "WEB", "title": "Login fails on Safari"})
	if isError || !strings.HasPrefix(text, `{"key":"WEB-`) {
		t.Fatalf("ticket_create = %s (error %v)", text, isError)
	}
}

// The read marks hold through a call's write and its refresh: after a
// read, an edit of the description, and a second edit in a later call
// with no read between, are both sent; once a person changes the ticket,
// the agent's next edit is refused until it reads the ticket again. Each
// call sends to the ticket before the refresh, which no longer reads the
// detail of a ticket no screen has open, so the mark follows the card the
// refresh reads.
func TestTrackerWriteReadMarksHoldAcrossTheAgentsOwnEdits(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: describedSite()})
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	_, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	readBoard(t, f, f.conn.ID, "1", []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: "1"}, {Kind: trackerapp.SectionBacklog}})
	if text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_read", map[string]any{"key": "WEB-1"}); isError {
		t.Fatalf("read = %s", text)
	}
	edit := func(text string) string {
		t.Helper()
		answer, _ := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1"}, "set": map[string]any{"description": text}})
		return answer
	}
	const applied = `{"tickets":[{"key":"WEB-1","applied":["description"]}]}`
	for _, text := range []string{"The agent's first text.", "The agent's second text."} {
		if got := edit(text); got != applied {
			t.Fatalf("edit to %q = %s", text, got)
		}
	}
	if v, err := f.a.EditTrackerIssue(t.Context(), f.conn.ID, trackerapp.EditRequest{Issue: "10101", Fields: []trackerapp.FieldEditInput{
		{Field: "description", Markdown: &trackerapp.MarkdownEditInput{Markdown: "The person's text.", From: "The agent's second text.\n"}},
	}}); err != nil || v.Parts[0].State != "applied" {
		t.Fatalf("the person's edit = %+v, %v", v, err)
	}
	if got := edit("The agent's third text."); !strings.Contains(got, "WEB-1 changed since you read it") {
		t.Fatalf("an edit after the person's = %s; want it refused as changed since read", got)
	}
}

// A ticket of a board this computer read, which an agent changed while a
// screen had it open, shows its stored body, with the change, the next
// time a screen opens it: the body the refresh after the write stored
// reads back (the provider's kept read is not encoded in it).
func TestTrackerWriteStoredBodyOfATicketTheAgentChangedOpens(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: describedSite()})
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	_, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	readBoard(t, f, f.conn.ID, "1", []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: "1"}, {Kind: trackerapp.SectionBacklog}})
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	sub, err := f.a.SubscribeTrackerTicket(ctx, f.conn.ID, "10101", true)
	if err != nil {
		t.Fatal(err)
	}
	f.ticketShows(t, ctx, sub.ID, "its title", func(d *trackerapp.DetailView) bool { return d.Card.Title == "First story" })
	if text, _ := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1"}, "set": map[string]any{"title": "The agent's title"}}); text != `{"tickets":[{"key":"WEB-1","applied":["title"]}]}` {
		t.Fatalf("ticket_update = %s", text)
	}
	f.ticketShows(t, ctx, sub.ID, "the agent's title", func(d *trackerapp.DetailView) bool { return d.Card.Title == "The agent's title" })
	if err := f.a.ReleaseTrackerSubscription(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	again, err := f.a.SubscribeTrackerTicket(ctx, f.conn.ID, "10101", true)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Loaded || again.Detail == nil || again.Detail.Card.Title != "The agent's title" || !strings.Contains(again.Detail.Description, "The first old text.") {
		t.Fatalf("the ticket opened again from the mirror = %+v", again.TicketView)
	}
}
