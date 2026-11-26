package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/trackertools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"github.com/google/uuid"
)

// trackerWriteThread is a Codex thread of the board fixture's project
// with a live session, in mode, and its tracker tools endpoint.
func trackerWriteThread(t *testing.T, f *trackerBoardFixture, mode provider.RuntimeMode) (store.Thread, string) {
	t.Helper()
	f.a.ensureTriageRouter()
	thread := store.Thread{ID: uuid.NewString(), ProjectID: f.project, Provider: string(provider.Codex), WorkspacePath: t.TempDir(), RuntimeMode: string(mode)}
	if err := f.a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	f.a.sessionManager().put(thread.ID, session{Token: token, Provider: thread.Provider})
	f.a.triage.MarkThreadActive(thread.ID, token)
	endpoint := trackerMCPEndpoint(t, f.a, thread, token)
	if endpoint == "" {
		t.Fatal("the fixture's thread has no tracker tools")
	}
	return thread, endpoint
}

// trackerWritePrompt waits for the thread's tracker write approval and
// answers it with its request and the Approval it carries.
func trackerWritePrompt(t *testing.T, a *App, threadID string) (provider.ApprovalRequest, trackertools.Approval) {
	t.Helper()
	var open provider.ApprovalRequest
	waitFor(t, "the tracker write's approval", func() bool {
		for _, req := range a.triage.PendingInteractiveRequests(threadID).Approvals {
			if req.Kind == triage.ApprovalKindTrackerWrite {
				open = req
				return true
			}
		}
		return false
	})
	var approval trackertools.Approval
	if err := json.Unmarshal(open.Input, &approval); err != nil {
		t.Fatalf("the approval's input %s: %v", open.Input, err)
	}
	return open, approval
}

// trackerWriteCall runs one tool call in the background, answering its
// text and whether it is an error on the channel.
func trackerWriteCall(t *testing.T, a *App, endpoint, name string, args any) <-chan [2]any {
	t.Helper()
	done := make(chan [2]any, 1)
	go func() {
		text, isError := trackerMCPStreamedCall(t, a, endpoint, name, args)
		done <- [2]any{text, isError}
	}()
	return done
}

// fakeComments is the bodies of the comments the fake site holds on the
// issue with id, as plain text.
func fakeComments(t *testing.T, f *trackerBoardFixture, id string) []string {
	t.Helper()
	var out []string
	for _, site := range f.e.Snapshot().Jira {
		for _, issue := range site.Issues {
			if issue.ID != id {
				continue
			}
			for _, c := range issue.Comments {
				out = append(out, string(c.Body))
			}
		}
	}
	return out
}

func noTrackerWritePrompt(t *testing.T, a *App, threadID string) {
	t.Helper()
	for _, req := range a.triage.PendingInteractiveRequests(threadID).Approvals {
		if req.Kind == triage.ApprovalKindTrackerWrite {
			t.Fatalf("the tracker write's approval is still open: %+v", req)
		}
	}
}

// A call naming two tickets waits for the person: the prompt shows each
// ticket's change, an approval sends both, a second answer is refused,
// and the call's timeline row names both tickets.
func TestTrackerWriteOfSeveralTicketsWaitsForThePerson(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	done := trackerWriteCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1", "WEB-2"}, "comment": "Checked on Safari."})
	open, approval := trackerWritePrompt(t, f.a, thread.ID)
	if len(approval.Tickets) != 2 || approval.Tickets[0].Key != "WEB-1" || approval.Tickets[0].Title != "First story" ||
		approval.Tickets[1].Key != "WEB-2" || approval.Comment != "Checked on Safari." {
		t.Fatalf("approval %+v", approval)
	}
	if open.AvailableDecisions == nil || len(*open.AvailableDecisions) != 2 || string((*open.AvailableDecisions)[0]) != `"accept"` || string((*open.AvailableDecisions)[1]) != `"decline"` {
		t.Fatalf("the approval offers %v, want accept and decline alone", open.AvailableDecisions)
	}
	if got := fakeComments(t, f, "10101"); len(got) != 0 {
		t.Fatalf("a comment was sent before the approval: %q", got)
	}
	answer := provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Decision: "allow"}
	if err := f.a.RespondToApproval(t.Context(), thread.ID, answer); err != nil {
		t.Fatalf("approve: %v", err)
	}
	result := <-done
	if result[1].(bool) || result[0] != `{"tickets":[{"key":"WEB-1","applied":["comment"]},{"key":"WEB-2","applied":["comment"]}]}` {
		t.Fatalf("ticket_update = %v", result)
	}
	if err := f.a.RespondToApproval(t.Context(), thread.ID, answer); !errors.Is(err, transport.ErrAlreadyHandled) {
		t.Fatalf("a second answer = %v, want already handled", err)
	}
	noTrackerWritePrompt(t, f.a, thread.ID)
	for _, id := range []string{"10101", "10102"} {
		if got := fakeComments(t, f, id); len(got) != 1 || !strings.Contains(got[0], "Checked on Safari.") {
			t.Fatalf("issue %s comments %q", id, got)
		}
	}
	row, ok, err := f.a.store.GetThreadItem(thread.ID, open.RequestID)
	if err != nil || !ok || row.Summary != "Changed 2 tickets" || !strings.Contains(row.Meta, `"key":"WEB-2"`) || !strings.Contains(row.Meta, `"issue":"10102"`) {
		t.Fatalf("the timeline row = %+v, %v, %v", row, ok, err)
	}
}

// A denial sends nothing and is what the call answers.
func TestTrackerWriteDeniedSendsNothing(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	done := trackerWriteCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1", "WEB-2"}, "comment": "Checked."})
	open, _ := trackerWritePrompt(t, f.a, thread.ID)
	if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Decision: "maybe"}); err == nil {
		t.Fatal("an answer that is neither approve nor deny was taken")
	}
	if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Decision: "deny"}); err != nil {
		t.Fatalf("deny: %v", err)
	}
	result := <-done
	if !result[1].(bool) || result[0] != "[tracker_write_denied] The user denied this change; nothing was sent." {
		t.Fatalf("ticket_update = %v", result)
	}
	noTrackerWritePrompt(t, f.a, thread.ID)
	if got := append(fakeComments(t, f, "10101"), fakeComments(t, f, "10102")...); len(got) != 0 {
		t.Fatalf("a denied call commented %q", got)
	}
	if _, ok, err := f.a.store.GetThreadItem(thread.ID, open.RequestID); err != nil || ok {
		t.Fatalf("a denied call wrote a timeline row: %v, %v", ok, err)
	}
}

// A wait that runs out is the call's answer, takes the prompt off every
// screen, and leaves an answer that comes after it refused.
func TestTrackerWriteUnansweredEndsTheWait(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	f.a.trackerMCP.approvals.mu.Lock()
	f.a.trackerMCP.approvals.wait = 200 * time.Millisecond
	f.a.trackerMCP.approvals.mu.Unlock()
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	done := trackerWriteCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1", "WEB-2"}, "comment": "Checked."})
	open, _ := trackerWritePrompt(t, f.a, thread.ID)
	result := <-done
	if !result[1].(bool) || !strings.HasPrefix(result[0].(string), "[tracker_write_unanswered] The user did not answer") {
		t.Fatalf("ticket_update = %v", result)
	}
	noTrackerWritePrompt(t, f.a, thread.ID)
	if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Decision: "allow"}); !errors.Is(err, transport.ErrAlreadyHandled) {
		t.Fatalf("an answer after the wait = %v, want already handled", err)
	}
	if got := fakeComments(t, f, "10101"); len(got) != 0 {
		t.Fatalf("an unanswered call commented %q", got)
	}
}

// A call that ends while it waits withdraws its prompt.
func TestTrackerWriteApprovalEndsWithTheCall(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	thread, _ := trackerWriteThread(t, f, provider.RuntimeAuto)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- trackerToolsHost{f.a}.Approve(ctx, thread.ID, trackertools.Approval{ID: "call-7", Tickets: []trackertools.ApprovalTicket{{Key: "WEB-1"}, {Key: "WEB-2"}}})
	}()
	trackerWritePrompt(t, f.a, thread.ID)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Approve = %v, want the call's end", err)
	}
	noTrackerWritePrompt(t, f.a, thread.ID)
}

// One ticket is changed at once, with its timeline row, and a read-only
// thread is refused before anything is read or sent.
func TestTrackerWriteOfOneTicketAndFromAReadOnlyThread(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1"}, "comment": "Looked."})
	if isError || text != `{"tickets":[{"key":"WEB-1","applied":["comment"]}]}` {
		t.Fatalf("ticket_update = %s (error %v)", text, isError)
	}
	if got := fakeComments(t, f, "10101"); len(got) != 1 {
		t.Fatalf("comments %q", got)
	}
	items, err := f.a.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows := slices.DeleteFunc(items, func(it store.Item) bool { return it.ToolName != "tracker_write" })
	if len(rows) != 1 || rows[0].Summary != "Changed WEB-1" {
		t.Fatalf("timeline rows %+v", rows)
	}

	_, endpoint = trackerWriteThread(t, f, provider.RuntimeReadOnly)
	since := 0
	if log := f.e.Requests(0).Requests; len(log) > 0 {
		since = log[len(log)-1].Seq
	}
	text, isError = trackerMCPStreamedCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-2"}, "comment": "Looked."})
	if !isError || text != "[tracker_read_only] This thread is read-only, so it cannot change tickets; ask the user to change its access." {
		t.Fatalf("a read-only update = %s (error %v)", text, isError)
	}
	text, isError = trackerMCPStreamedCall(t, f.a, endpoint, "ticket_create", map[string]any{"project": "WEB", "title": "New"})
	if !isError || !strings.HasPrefix(text, "[tracker_read_only]") {
		t.Fatalf("a read-only create = %s (error %v)", text, isError)
	}
	// The board's own sync may read the site meanwhile; the refused calls
	// neither read the ticket nor write anything.
	for _, r := range f.e.Requests(since).Requests {
		if r.Method != "GET" || strings.Contains(r.Path, "WEB-2") || strings.Contains(r.Path, "10102") || strings.Contains(r.Path, "createmeta") {
			t.Fatalf("a read-only thread reached the tracker: %s %s", r.Method, r.Path)
		}
	}
}

// A create goes through the project's board here and answers the new
// key; its row says it was created.
func TestTrackerWriteCreatesThroughTheProjectsBoard(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_create", map[string]any{"project": "web", "title": "Login fails on Safari"})
	if !isError || text != "[tracker_not_found] Project WEB's board on example.atlassian.net has not been read on this computer yet; ask the user to open it in Issues, then call again." {
		t.Fatalf("a create before the board was read = %s (error %v)", text, isError)
	}
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	sub := f.subscribe(t, ctx)
	f.boardEvent(t, "for the first read", func(e trackerapp.BoardEvent) bool { return e.ReadAgain && e.Seq > sub.Seq })
	text, isError = trackerMCPStreamedCall(t, f.a, endpoint, "ticket_create", map[string]any{"project": "web", "title": "Login fails on Safari"})
	if isError || !strings.HasPrefix(text, `{"key":"WEB-`) {
		t.Fatalf("ticket_create = %s (error %v)", text, isError)
	}
	var created struct{ Key string }
	if err := json.Unmarshal([]byte(text), &created); err != nil {
		t.Fatal(err)
	}
	items, err := f.a.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows := slices.DeleteFunc(items, func(it store.Item) bool { return it.ToolName != "tracker_write" })
	if len(rows) != 1 || rows[0].Summary != "Created "+created.Key || !strings.Contains(rows[0].Meta, `"created":true`) {
		t.Fatalf("timeline rows %+v", rows)
	}
	text, isError = trackerMCPStreamedCall(t, f.a, endpoint, "ticket_create", map[string]any{"project": "OPS", "title": "Elsewhere"})
	if !isError || !strings.HasPrefix(text, "[tracker_not_found] No board of project OPS") {
		t.Fatalf("a create in a project with no board here = %s (error %v)", text, isError)
	}
}

// A prompt triage does not keep, because the session that asked no longer
// holds the thread, ends the call at once instead of waiting on a prompt
// no screen shows.
func TestTrackerWriteApprovalOfAnEndedSession(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	f.a.triage.MarkThreadActive(thread.ID, uuid.NewString())
	text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1", "WEB-2"}, "comment": "Checked."})
	if !isError || !strings.HasPrefix(text, "[tracker_session_inactive]") {
		t.Fatalf("ticket_update = %s (error %v)", text, isError)
	}
	if got := fakeComments(t, f, "10101"); len(got) != 0 {
		t.Fatalf("comments %q", got)
	}
}

// A waiter settles once: an answer that comes after the wait ended, or a
// second answer, finds nothing to settle, so the screen that sent it is
// told it was already handled and no decision is announced for it. A
// waiter is its thread's: the same request ID on another thread finds
// none.
func TestTrackerApprovalWaiterSettlesOnce(t *testing.T) {
	t.Parallel()
	var w trackerApprovals
	ch := w.open("thread-1", "tracker:call-1")
	if w.settle("thread-2", "tracker:call-1", true) {
		t.Fatal("another thread's answer settled the waiter")
	}
	if !w.settle("thread-1", "tracker:call-1", false) {
		t.Fatal("the first settlement lost")
	}
	if w.settle("thread-1", "tracker:call-1", true) {
		t.Fatal("a second settlement won")
	}
	if got := <-ch; got {
		t.Fatal("the waiter got the second settlement")
	}
	if w.settle("thread-1", "tracker:call-2", true) {
		t.Fatal("a settlement of a waiter never opened won")
	}
}

// The requests one call makes, end to end on the fake Jira site: a move
// and a comment on a ticket the mirror does not hold, on a computer no
// screen is reading boards on, so every request is the call's.
func TestTrackerWriteRequestsOfAMoveAndAComment(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	_, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	since := 0
	if log := f.e.Requests(0).Requests; len(log) > 0 {
		since = log[len(log)-1].Seq
	}
	text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-2"}, "set": map[string]any{"status": "Done"}, "comment": "Done."})
	if isError || text != `{"tickets":[{"key":"WEB-2","applied":["status","comment"]}]}` {
		t.Fatalf("ticket_update = %s (error %v)", text, isError)
	}
	var routes []string
	for _, r := range f.e.Requests(since).Requests {
		routes = append(routes, r.Route)
	}
	want := []string{
		"GET rest/api/3/field",                 // the site's field catalog, read once per connection
		"POST rest/api/3/issue/bulkfetch",      // the key, which no mirror holds
		"GET rest/api/3/issue/{issue}",         // the ticket as it is now, with its moves, which the move takes
		"GET rest/api/3/issue/{issue}/comment", // with its latest comments
		"POST rest/api/3/issue/{issue}/transitions",
		"POST rest/api/3/issue/bulkfetch", // the card after the move, just before the comment: its project
		"POST rest/api/3/issue/{issue}/comment",
	}
	if !slices.Equal(routes, want) {
		t.Fatalf("requests %q\nwant %q", routes, want)
	}
}
