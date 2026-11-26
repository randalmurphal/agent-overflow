package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"
)

// threadAction waits for the next thread:updated frame of threadID,
// skipping every other event, and returns its action and row.
func (f *trackerBoardFixture) threadAction(t *testing.T, threadID, what string) (string, store.Thread) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-f.events.Events():
			if e.Channel != string(eventchan.ThreadUpdated) {
				continue
			}
			var got triage.ThreadUpdateEvent
			if err := json.Unmarshal(e.Data, &got); err != nil {
				t.Fatal(err)
			}
			if got.Thread != nil && got.Thread.ID == threadID {
				return got.Action, *got.Thread
			}
		case <-deadline:
			t.Fatalf("no thread:updated frame of %s %s", threadID, what)
		}
	}
}

// drainThreadFrames drops the events waiting, after f's announcers are idle.
func (f *trackerBoardFixture) drainThreadFrames() {
	f.a.threadLinkAnnounces.Wait()
	for len(f.events.Events()) > 0 {
		<-f.events.Events()
	}
}

var linkListingTicket = ThreadTicketRef{Kind: store.TrackerKindJira, Site: "https://example.atlassian.net", IssueID: "10101"}

// A link makes a never-sent thread real, as a bound worktree does: the
// link lists it on every screen (a listed frame), the empty-draft cleanup
// refuses it, and text typed and erased keeps it with its link; removing
// the link takes it off every sidebar (an emptied frame, which leaves the
// panes showing it) and the cleanup may delete it again.
func TestALinkKeepsANeverSentThread(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	if err := f.a.store.CreateThread(store.Thread{ID: "t1", ProjectID: f.project, Title: "New Thread", Provider: "claude", Model: "test-model",
		Mode: "chat", WorkspacePath: "/tmp/web", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f.drainThreadFrames()

	if _, err := f.a.SetThreadTicket(ctx, "t1", linkListingTicket); err != nil {
		t.Fatal(err)
	}
	if action, row := f.threadAction(t, "t1", "for the link"); action != triage.ThreadActionListed || row.TicketLink == nil {
		t.Fatalf("the link's frame = %s with %+v; want listed with the link", action, row.TicketLink)
	}
	if deleted, err := f.a.DeleteEmptyDraftThread("t1"); err != nil || deleted {
		t.Fatalf("the cleanup's delete of the linked draft = %v, %v; want refused", deleted, err)
	}
	for _, content := range []string{"a thought", ""} {
		if err := f.a.SaveDraft(ctx, "t1", content, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if deleted, err := f.a.DeleteEmptyDraftThread("t1"); err != nil || deleted {
		t.Fatalf("the cleanup's delete after the text was erased = %v, %v; want refused", deleted, err)
	}
	if row, err := f.a.store.GetThread("t1"); err != nil || row.TicketLink == nil {
		t.Fatalf("the link after the text was erased = %+v, %v", row.TicketLink, err)
	}
	if listed, err := f.a.store.ThreadListed("t1"); err != nil || !listed {
		t.Fatalf("the linked draft after its text was erased is listed: %v, %v", listed, err)
	}
	// Text typed and erased moved the thread across nothing.
	f.noThreadFrame(t, "t1", "the typed and erased text")

	if _, err := f.a.ClearThreadTicket(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if action, row := f.threadAction(t, "t1", "for the unlink"); action != triage.ThreadActionEmptied || row.TicketLink != nil {
		t.Fatalf("the unlink's frame = %s with %+v; want emptied with no link", action, row.TicketLink)
	}
	if deleted, err := f.a.DeleteEmptyDraftThread("t1"); err != nil || !deleted {
		t.Fatalf("the cleanup's delete of the unlinked draft = %v, %v; want deleted", deleted, err)
	}
}

// A linked draft moved to a project of another computer is copied there
// with its link, and the copy's completion ends the source's link: the
// source no longer keeps the ticket, its board still names the ticket as
// its row's parent, and the emptied source is deletable.
func TestALinkedDraftCopiedToAnotherComputerEndsTheSourceLink(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	e, jiraURL := parentTicketSite(t)
	source, destination := transferTestBackend(t), transferTestBackend(t)
	sourceConn := withTrackers(t, source, jiraURL)
	destConn := withTrackers(t, destination, jiraURL)
	ctx := context.Background()
	project, err := source.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "Source", Path: testutil.InitGitRepo(t)})
	if err != nil {
		t.Fatal(err)
	}
	targetProject, err := destination.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "Destination", Path: testutil.InitGitRepo(t)})
	if err != nil {
		t.Fatal(err)
	}
	thread := store.Thread{ID: uuid.NewString(), ProjectID: project.ID, Title: "New Thread", Mode: "chat", Provider: "claude", Model: "claude-sonnet-4-6",
		WorkspacePath: project.Path, CreatedAt: 1, UpdatedAt: 1}
	if err := source.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	snapshot := DraftSnapshot{Content: "portable prompt"}
	if err := source.SaveDraft(ctx, thread.ID, snapshot.Content, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := source.SetThreadTicket(ctx, thread.ID, parentTicket); err != nil {
		t.Fatal(err)
	}
	board := showParentBoard(t, source, e, sourceConn, project.ID)

	destinationID, _ := destination.backendIdentity()
	operation := uuid.NewString()
	intent, err := source.BeginDraftProjectTransfer(ctx, thread.ID, operation, destinationID, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	offer, err := destination.CreateThreadTransferOffer(ctx, intent, targetProject.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.BindThreadTransferDestination(ctx, thread.ID, offer); err != nil {
		t.Fatal(err)
	}
	awaitTransferComplete(t, source, destination, operation, false)
	board.parentNamed(t, "after the copy")

	copied, err := destination.store.GetThread(intent.TargetThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if l := copied.TicketLink; l == nil || l.Key != "OPS-1" || l.ConnectionID != destConn.ID {
		t.Fatalf("the copy's link = %+v; want OPS-1 on %s", copied.TicketLink, destConn.ID)
	}
	if links, err := source.store.ListThreadTickets([]string{thread.ID}); err != nil || len(links) != 0 {
		t.Fatalf("the source's links after the copy = %+v, %v; want none", links, err)
	}
	if deleted, err := source.DeleteEmptyDraftThread(thread.ID); err != nil || !deleted {
		t.Fatalf("the emptied source's delete = %v, %v; want deleted", deleted, err)
	}
}

// A linked draft moved to another project of this computer takes its link
// in the move's transaction: the destination is listed with it, the
// emptied source leaves every sidebar and the cleanup deletes it. A draft
// whose link is all it holds moves the same way. A source a bound worktree
// keeps listed is sent its row without the link, and stays.
func TestALinkedDraftMovedToAnotherProjectTakesItsLink(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		content  string
		worktree bool
	}{
		"with text":           {content: "moving text"},
		"with nothing typed":  {},
		"bound to a worktree": {content: "moving text", worktree: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testALinkedDraftMove(t, c.content, c.worktree)
		})
	}
}

func testALinkedDraftMove(t *testing.T, content string, worktree bool) {
	f := newTrackerBoardFixture(t)
	other, err := f.a.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "Other", Path: testutil.InitGitRepo(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, thread := range []store.Thread{
		{ID: "source", ProjectID: f.project, Title: "New Thread", Provider: "claude", Model: "test-model", Mode: "chat", WorkspacePath: "/tmp/web", CreatedAt: 1, UpdatedAt: 1},
		{ID: "destination", ProjectID: other.ID, Title: "New Thread", Provider: "claude", Model: "test-model", Mode: "chat", WorkspacePath: other.Path, CreatedAt: 1, UpdatedAt: 1},
	} {
		if err := f.a.store.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
	}
	if worktree {
		if _, err := f.a.store.UpdateThreadCheckout("source", "/tmp/wt/source", "/tmp/wt/source", "source"); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if err := f.a.SaveDraft(ctx, "source", content, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.SetThreadTicket(ctx, "source", linkListingTicket); err != nil {
		t.Fatal(err)
	}
	f.drainThreadFrames()
	if _, err := f.a.MoveDraftToThread(ctx, "source", "destination", DraftSnapshot{Content: content}); err != nil {
		t.Fatal(err)
	}
	action, row := f.threadAction(t, "destination", "for the move")
	if action != triage.ThreadActionListed || row.TicketLink == nil || row.TicketLink.IssueID != linkListingTicket.IssueID {
		t.Fatalf("the destination's frame = %s with %+v; want listed with the link", action, row.TicketLink)
	}
	wantSource := triage.ThreadActionEmptied
	if worktree {
		wantSource = triage.ThreadActionFull
	}
	if action, row := f.threadAction(t, "source", "for the move"); action != wantSource || row.TicketLink != nil {
		t.Fatalf("the source's frame = %s with %+v; want %s with no link", action, row.TicketLink, wantSource)
	}
	links, err := f.a.store.ListThreadTickets([]string{"source", "destination"})
	if err != nil || len(links) != 1 || links["destination"].Ticket.IssueID != linkListingTicket.IssueID {
		t.Fatalf("the links after the move = %+v, %v; want the destination's alone", links, err)
	}
	if deleted, err := f.a.DeleteEmptyDraftThread("source"); err != nil || deleted == worktree {
		t.Fatalf("the emptied source's delete = %v, %v; want deleted %v", deleted, err, !worktree)
	}
}
