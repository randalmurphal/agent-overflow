package store

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// receiveThread commits an incoming transfer of kind of target, as the
// sender's manifest carried it, into s.
func receiveThread(t *testing.T, s *Store, kind string, target Thread, epoch int64) {
	t.Helper()
	request := transferRequest(target.ID, kind, "incoming")
	if epoch > 0 {
		request.OwnershipEpoch = epoch
	}
	if _, err := s.CreateThreadTransfer(request); err != nil {
		t.Fatal(err)
	}
	advanceTransfer(t, s, request.ID, "prepared")
	source := newTestStore(t)
	if err := source.CreateThread(makeThread(target.ID, "claude")); err != nil {
		t.Fatal(err)
	}
	var history bytes.Buffer
	if err := source.ExportThreadHistory(context.Background(), target.ID, &history); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitIncomingThreadTransfer(context.Background(), request.ID, strings.Repeat("b", 64), transferTestSecret(), target, &history); err != nil {
		t.Fatal(err)
	}
}

// forgedManifestThread is thread id as a manifest carries it, its link
// carrying the sender's state and connection and members an earlier
// version carried, decoded as the receiver decodes a manifest.
func forgedManifestThread(t *testing.T, id string) Thread {
	t.Helper()
	carried := []byte(`{"id":"` + id + `","provider":"claude","ticketLink":{"kind":"jira","site":"` + mirrorSite + `","issueId":"10001",
		"key":"WEB-1","title":"Fix the login page","statusId":"3","statusName":"In Progress","statusGroup":"in_progress",
		"origin":"start_work","linkedAt":10,"state":"current","connectionId":"sender-connection","seenRoot":1,"seen_root":1,"missing":1}}`)
	var thread Thread
	if err := json.Unmarshal(carried, &thread); err != nil {
		t.Fatal(err)
	}
	base := makeThread(id, "claude")
	base.TicketLink = thread.TicketLink
	return base
}

// A received thread keeps the ticket, its key, title and status as the
// sender last saw them, the origin and the time; its state and connection
// are this computer's, and its link names no mirror root here, whatever
// the manifest carried.
func TestReceivedLinkTrustsNothingTheSenderDerived(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		connected    bool
		state        string
		toConnection bool
	}{
		{"with the site's connection", true, TicketLinkNotChecked, true},
		{"with no connection to the site", false, TicketLinkNoConnection, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			var connection string
			if c.connected {
				connection = mustCreateTrackerConnection(t, s, jiraConnection(mirrorSite, "user1@example.com", 1)).ID
			}
			receiveThread(t, s, "copy", forgedManifestThread(t, "incoming"), 0)

			want := ThreadTicket{ThreadID: "incoming", Ticket: webTicket, IssueKey: "WEB-1", IssueTitle: "Fix the login page",
				StatusID: "3", StatusName: "In Progress", StatusGroup: MirrorGroupInProgress, Origin: TicketOriginStartWork, LinkedAt: 10}
			if got, ok := storedTicket(t, s, "incoming"); !ok || got != want {
				t.Fatalf("stored link = %+v, %v\nwant %+v", got, ok, want)
			}
			row, err := s.GetThread("incoming")
			if err != nil || row.TicketLink == nil {
				t.Fatalf("received row link = %+v, %v", row.TicketLink, err)
			}
			if row.TicketLink.State != c.state || row.TicketLink.ConnectionID != connection {
				t.Fatalf("received link shows %q on %q; want %q on %q", row.TicketLink.State, row.TicketLink.ConnectionID, c.state, connection)
			}
		})
	}
}

// A returning move replaces the link this computer kept with the one the
// manifest carries, and a manifest with none clears it; a carried link of
// a kind the store refuses fails the commit and leaves the thread as it
// was.
func TestReturningThreadTakesTheCarriedLink(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		carried func(t *testing.T) *ThreadTicketLink
	}{
		{"carried link", func(t *testing.T) *ThreadTicketLink { return forgedManifestThread(t, "returning").TicketLink }},
		{"no link", func(*testing.T) *ThreadTicketLink { return nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, _, root := newMirror(t)
			mustCreateThread(t, s, "returning")
			mustSetThreadTicket(t, s, readLink(s, root, "returning", titledCard("10002", 5, "Kept here")))
			outgoing := transferRequest("returning", "move", "outgoing")
			if _, err := s.CreateThreadTransfer(outgoing); err != nil {
				t.Fatal(err)
			}
			advanceTransfer(t, s, outgoing.ID, "prepared", "committed", "complete")

			target := makeThread("returning", "claude")
			target.TicketLink = c.carried(t)
			receiveThread(t, s, "move", target, 2)
			got, ok := storedTicket(t, s, "returning")
			if target.TicketLink == nil {
				if ok {
					t.Fatalf("link after a return with none = %+v; want none", got)
				}
				return
			}
			if !ok || got.Ticket != webTicket || got.IssueKey != target.TicketLink.Key || got.IssueTitle != target.TicketLink.Title {
				t.Fatalf("link after the return = %+v, %v; want the carried ticket and what it showed", got, ok)
			}
		})
	}
}

// A carried link the store would refuse from its own writer fails the
// whole commit: the thread is not received.
func TestReceivedInvalidLinkFailsTheCommit(t *testing.T) {
	t.Parallel()
	for name, spoil := range map[string]func(*ThreadTicketLink){
		"unknown kind":   func(l *ThreadTicketLink) { l.Kind = "tempo" },
		"no key":         func(l *ThreadTicketLink) { l.Key = "" },
		"unknown origin": func(l *ThreadTicketLink) { l.Origin = "pasted" },
		"status group":   func(l *ThreadTicketLink) { l.StatusGroup = "left_out" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			target := forgedManifestThread(t, "incoming")
			spoil(target.TicketLink)
			request := transferRequest(target.ID, "copy", "incoming")
			if _, err := s.CreateThreadTransfer(request); err != nil {
				t.Fatal(err)
			}
			advanceTransfer(t, s, request.ID, "prepared")
			source := newTestStore(t)
			if err := source.CreateThread(makeThread(target.ID, "claude")); err != nil {
				t.Fatal(err)
			}
			var history bytes.Buffer
			if err := source.ExportThreadHistory(context.Background(), target.ID, &history); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CommitIncomingThreadTransfer(context.Background(), request.ID, strings.Repeat("b", 64), transferTestSecret(), target, &history); err == nil {
				t.Fatal("a commit carrying an invalid link was taken")
			}
			assertRows(t, s.db, `SELECT count(*) FROM threads WHERE id = 'incoming'`, "0")
			assertRows(t, s.db, `SELECT count(*) FROM thread_tickets`, "0")
		})
	}
}
