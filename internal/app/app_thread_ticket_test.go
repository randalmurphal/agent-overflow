package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

func TestThreadTicketMethodsCarryTheirScopeAndRoute(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]struct {
		scope transport.Scope
		route transport.MethodRoute
	}{
		"SetThreadTicket":   {transport.ScopeThreadsOperate, transport.RouteThread},
		"ClearThreadTicket": {transport.ScopeThreadsOperate, transport.RouteThread},
		"ThreadsForTicket":  {transport.ScopeThreadsRead, transport.RouteSelected},
	} {
		i := slices.IndexFunc(transport.GeneratedMethods, func(m transport.MethodMeta) bool { return m.Name == name })
		if i < 0 {
			t.Errorf("%s is not a generated method", name)
			continue
		}
		if m := transport.GeneratedMethods[i]; m.Scope != want.scope || m.Route != want.route || m.CallerBound || m.Budget != 0 {
			t.Errorf("%s = %+v; want scope %s, route %s, not caller-bound, the default budget", name, m, want.scope, want.route)
		}
	}
}

// threadFrame waits for the next thread:updated frame of threadID that ok
// accepts, skipping every other event.
func (f *trackerBoardFixture) threadFrame(t *testing.T, threadID, what string, ok func(store.Thread) bool) store.Thread {
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
			if got.Thread != nil && got.Thread.ID == threadID && ok(*got.Thread) {
				return *got.Thread
			}
		case <-deadline:
			t.Fatalf("no thread:updated frame of %s %s", threadID, what)
		}
	}
}

// noThreadFrame fails when a thread:updated frame of threadID is waiting.
func (f *trackerBoardFixture) noThreadFrame(t *testing.T, threadID, what string) {
	t.Helper()
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
				t.Fatalf("%s sent a frame of %s: %+v", what, threadID, got.Thread.TicketLink)
			}
		default:
			return
		}
	}
}

// A thread linked to a ticket of the fake Jira shows its key, title and
// state in its row, which every client is sent before the call returns;
// the ticket lists it; a ticket the tracker does not return is refused with
// the link as it was; a connection turned off sends the row again with
// its new state; a clear sends the row without the link, and a clear of no
// link sends nothing.
func TestThreadTicketMethodsLinkListAndClear(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	if err := f.a.store.CreateThread(store.Thread{ID: "t1", ProjectID: f.project, Title: "Thread", Provider: "claude", Model: "test-model",
		WorkspacePath: "/tmp/web", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ticket := ThreadTicketRef{Kind: store.TrackerKindJira, Site: "https://example.atlassian.net", IssueID: "10101"}

	row, err := f.a.SetThreadTicket(ctx, "t1", ticket)
	if err != nil {
		t.Fatal(err)
	}
	link := row.TicketLink
	if link == nil || link.Title != "First story" || link.State != store.TicketLinkCurrent || link.ConnectionID != f.conn.ID || link.Origin != store.TicketOriginManual {
		t.Fatalf("the linked row's link = %+v", link)
	}
	f.threadFrame(t, "t1", "for the link", func(r store.Thread) bool { return r.TicketLink != nil && *r.TicketLink == *link })
	f.a.threadLinkAnnounces.Wait()
	f.noThreadFrame(t, "t1", "the link's second announce")
	if rows, err := f.a.ThreadsForTicket(ctx, ticket); err != nil || len(rows) != 1 || rows[0].ID != "t1" {
		t.Fatalf("ThreadsForTicket = %+v, %v", rows, err)
	}
	if _, err := f.a.ThreadsForTicket(ctx, ThreadTicketRef{Kind: "tempo", Site: "s", IssueID: "1"}); !hasCode(err, "thread_ticket_invalid") {
		t.Fatalf("ThreadsForTicket of a Tempo ticket = %v", err)
	}

	missing := ticket
	missing.IssueID = "99999"
	if _, err := f.a.SetThreadTicket(ctx, "t1", missing); !hasCode(err, "thread_ticket_not_found") {
		t.Fatalf("a link to a ticket the tracker does not return = %v", err)
	}
	if row, err := f.a.store.GetThread("t1"); err != nil || row.TicketLink == nil || row.TicketLink.IssueID != "10101" {
		t.Fatalf("the link after a refusal = %+v, %v", row.TicketLink, err)
	}
	if _, err := f.a.SetThreadTicket(ctx, "missing", ticket); !hasCode(err, "thread_ticket_thread_missing") {
		t.Fatalf("a link of a missing thread = %v", err)
	}

	if _, err := f.a.SetTrackerConnectionEnabled(ctx, f.conn.ID, false); err != nil {
		t.Fatal(err)
	}
	f.threadFrame(t, "t1", "for the turn off", func(r store.Thread) bool {
		return r.TicketLink != nil && r.TicketLink.State == store.TicketLinkTurnedOff
	})
	off := ticket
	if _, err := f.a.SetThreadTicket(ctx, "t1", off); !hasCode(err, "thread_ticket_connection_off") {
		t.Fatalf("a link through a connection turned off = %v", err)
	}

	if row, err := f.a.ClearThreadTicket(ctx, "t1"); err != nil || row.TicketLink != nil {
		t.Fatalf("ClearThreadTicket = %+v, %v", row.TicketLink, err)
	}
	f.threadFrame(t, "t1", "for the clear", func(r store.Thread) bool { return r.TicketLink == nil })
	if _, err := f.a.ClearThreadTicket(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	f.noThreadFrame(t, "t1", "a clear of no link")
}

// A thread's delete ends its link: the boards that show the ticket the
// link was the last to keep, as the parent of their rows, are told, so
// they stop showing its card.
func TestDeletingALinkedThreadTellsTheBoardsOfItsTicket(t *testing.T) {
	t.Parallel()
	site := trackerBoardSite()
	site.Projects = append(site.Projects, trackerfake.JiraProject{ID: "10001", Key: "OPS", Name: "Ops"})
	day := site.Issues[0].Created
	site.Issues = append(site.Issues, trackerfake.JiraIssue{ID: "20001", Key: "OPS-1", Project: "OPS", Type: "10000", Summary: "The epic",
		Status: "10000", Created: day, Updated: day})
	site.Issues[1].Parent = "20001"
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: site})
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	f.subscribe(t, ctx)
	f.boardEvent(t, "for the first read", func(e trackerapp.BoardEvent) bool { return slices.Contains(e.Changed, "10102") })
	if err := f.a.store.CreateThread(store.Thread{ID: "t1", ProjectID: f.project, Title: "Thread", Provider: "claude", Model: "test-model",
		WorkspacePath: "/tmp/web", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.SetThreadTicket(ctx, "t1", ThreadTicketRef{Kind: store.TrackerKindJira, Site: "https://example.atlassian.net", IssueID: "20001"}); err != nil {
		t.Fatal(err)
	}
	f.boardEvent(t, "for the parent's card", func(e trackerapp.BoardEvent) bool { return slices.Contains(e.Changed, "10102") })

	if _, err := f.a.DeleteThread("t1"); err != nil {
		t.Fatal(err)
	}
	f.boardEvent(t, "for the parent no longer kept", func(e trackerapp.BoardEvent) bool { return slices.Contains(e.Changed, "10102") })
}

// withTrackers gives a, a transfer backend, the tracker service with a
// verified connection to the fake Jira site of e, and returns the
// connection.
func withTrackers(t *testing.T, a *App, jiraURL string) TrackerConnection {
	t.Helper()
	a.trackerOriginOverride = func(string, string) (string, error) { return jiraURL, nil }
	a.version = "1.2.3"
	if err := a.initTrackers(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.trackers.Stop)
	ctx := context.Background()
	conn, err := a.AddTrackerConnection(ctx, NewTrackerConnection{Kind: store.TrackerKindJira, Site: "example.atlassian.net", Account: "user1@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conn.KeyFilePath, []byte(trackerTestToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if conn, err = a.VerifyTrackerConnection(ctx, conn.ID); err != nil || conn.Problem != nil {
		t.Fatalf("VerifyTrackerConnection = %+v, %v", conn, err)
	}
	return conn
}

// A moved thread arrives with its link, which takes the card the
// destination's mirror holds and shows current there, read through the
// destination's own connection; the source no longer keeps the ticket for
// it, and tells the boards that showed it as a parent.
func TestAMovedThreadCarriesItsLink(t *testing.T) {
	remotetest.Require(t)
	e := trackerfake.New(trackerfake.Options{})
	t.Cleanup(e.Close)
	site := trackerBoardSite()
	site.Projects = append(site.Projects, trackerfake.JiraProject{ID: "10001", Key: "OPS", Name: "Ops"})
	day := site.Issues[0].Created
	site.Issues = append(site.Issues, trackerfake.JiraIssue{ID: "20001", Key: "OPS-1", Project: "OPS", Type: "10000", Summary: "The epic",
		Status: "10000", Created: day, Updated: day})
	site.Issues[1].Parent = "20001"
	if err := e.Seed(trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}); err != nil {
		t.Fatal(err)
	}
	srv, err := trackerfake.Serve(e, "example.atlassian.net")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		for _, r := range e.Unhandled() {
			t.Errorf("unhandled request %s %s: %s", r.Method, r.Path, r.Detail)
		}
	})
	source, destination := transferTestBackend(t), transferTestBackend(t)
	sourceConn := withTrackers(t, source, srv.JiraURL("example.atlassian.net"))
	destConn := withTrackers(t, destination, srv.JiraURL("example.atlassian.net"))
	ctx := context.Background()
	ticket := ThreadTicketRef{Kind: store.TrackerKindJira, Site: "https://example.atlassian.net", IssueID: "20001"}

	sourcePath := testutil.InitGitRepo(t)
	sourceProject, err := source.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "Source", Path: sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	destProject, err := destination.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "Destination", Path: testutil.InitGitRepo(t)})
	if err != nil {
		t.Fatal(err)
	}
	thread := store.Thread{ID: uuid.NewString(), ProjectID: sourceProject.ID, Title: "Linked", Provider: "claude",
		RuntimeMode: string(provider.RuntimeApprovalRequired), SessionRef: uuid.NewString(), WorkspacePath: sourcePath, CreatedAt: 1, UpdatedAt: 1}
	if err := source.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if _, err := source.SetThreadTicket(ctx, thread.ID, ticket); err != nil {
		t.Fatal(err)
	}
	// The source shows the board whose row 10102 has the ticket as parent.
	bus := transport.NewEventBus(64)
	t.Cleanup(bus.Close)
	source.SetEventBus(bus)
	events := bus.Subscribe()
	t.Cleanup(events.Close)
	board := &trackerBoardFixture{a: source, e: e, conn: sourceConn, project: sourceProject.ID, events: events}
	listed := []trackerapp.ProjectBoard{{Connection: sourceConn.ID, Board: "1", OpensFirst: true}}
	if _, err := source.SetProjectTrackerBoards(ctx, sourceProject.ID, nil, listed); err != nil {
		t.Fatal(err)
	}
	subCtx, _ := newTestConnection(t, transport.ConnPrincipal{})
	board.subscribe(t, subCtx)
	board.boardEvent(t, "for the first read", func(e trackerapp.BoardEvent) bool { return slices.Contains(e.Changed, "10102") })
	// The destination's mirror holds the ticket's card through a thread of
	// its own.
	if err := destination.store.CreateThread(store.Thread{ID: uuid.NewString(), ProjectID: destProject.ID, Title: "Here", Provider: "claude",
		WorkspacePath: destProject.Path, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	here, err := destination.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := destination.SetThreadTicket(ctx, here[0].ID, ticket); err != nil {
		t.Fatal(err)
	}
	projects, err := source.claudeProjectsDir()
	if err != nil {
		t.Fatal(err)
	}
	nativeDir, err := sessionfork.WorkspaceProjectDir(projects, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nativeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	native := `{"type":"user","sessionId":"` + thread.SessionRef + `","uuid":"` + uuid.NewString() + `","parentUuid":null,"message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(filepath.Join(nativeDir, thread.SessionRef+".jsonl"), []byte(native), 0o600); err != nil {
		t.Fatal(err)
	}
	destinationID, _ := destination.backendIdentity()
	operation := uuid.NewString()
	intent, err := source.BeginThreadTransfer(ctx, thread.ID, operation, destinationID, "move", true)
	if err != nil {
		t.Fatal(err)
	}
	offer, err := destination.CreateThreadTransferOffer(ctx, intent, destProject.ID, filepath.Join(t.TempDir(), "moved"), "moved-history")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.BindThreadTransferDestination(ctx, thread.ID, offer); err != nil {
		t.Fatal(err)
	}
	awaitTransferComplete(t, source, destination, operation, false)
	board.boardEvent(t, "for the parent the move stopped keeping", func(e trackerapp.BoardEvent) bool { return slices.Contains(e.Changed, "10102") })

	moved, err := destination.store.GetThread(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if l := moved.TicketLink; l == nil || l.Key != "OPS-1" || l.Title != "The epic" || l.State != store.TicketLinkCurrent || l.ConnectionID != destConn.ID {
		t.Fatalf("the moved thread's link = %+v; want current on %s", moved.TicketLink, destConn.ID)
	}
	if links, err := source.store.ListThreadTickets([]string{thread.ID}); err != nil || len(links) != 0 {
		t.Fatalf("the source still reads a link of the moved thread: %+v, %v", links, err)
	}
}
