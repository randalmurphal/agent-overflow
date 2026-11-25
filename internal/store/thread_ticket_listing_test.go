package store

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/transferwire"
)

// listedAndEmpty reads whether the sidebar lists threadID and whether the
// empty-draft cleanup may delete it.
func listedAndEmpty(t *testing.T, s *Store, threadID string) (listed, empty bool) {
	t.Helper()
	listed, err := s.ThreadListed(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if empty, err = s.IsEmptyDraftThread(threadID); err != nil {
		t.Fatal(err)
	}
	return listed, empty
}

// A link makes a never-sent thread real: the write that links it lists it
// and the empty-draft cleanup refuses it, whatever its composer holds;
// the write that removes the link takes it off the list and makes it
// deletable again. A thread that is real without its link moves across
// nothing either way.
func TestALinkMakesANeverSentThreadReal(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	createDraftCleanupThread(t, s, "draft", "chat")
	sent := createDraftCleanupThread(t, s, "sent", "chat")
	if err := s.InsertItem(Item{ID: "sent:0", ThreadID: sent.ID, Kind: "user_text", Role: "user", Summary: "hello", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if listed, empty := listedAndEmpty(t, s, "draft"); listed || !empty {
		t.Fatalf("the unlinked draft: listed %v, empty %v; want neither listed nor kept", listed, empty)
	}

	if set := mustSetThreadTicket(t, s, readLink(s, root, "draft", webCard("10001", 5))); set.Listing != ListingListed || !set.Changed {
		t.Fatalf("the link of a never-sent thread = %+v; want it listed", set)
	}
	if listed, empty := listedAndEmpty(t, s, "draft"); !listed || empty {
		t.Fatalf("the linked draft: listed %v, empty %v; want listed and kept", listed, empty)
	}
	if deleted, err := s.DeleteEmptyDraftThread("draft"); err != nil || deleted {
		t.Fatalf("the cleanup's delete of the linked draft = %v, %v; want refused", deleted, err)
	}
	// Typed, then erased: the link still keeps it.
	for _, content := range []string{"a thought", ""} {
		write, err := s.UpsertThreadDraft(ThreadDraft{ThreadID: "draft", Content: content, Attachments: "[]", TerminalChips: "[]"})
		if err != nil || write.Listing != ListingUnchanged {
			t.Fatalf("the draft write %q = %+v, %v; want no listing change", content, write, err)
		}
	}
	if listed, empty := listedAndEmpty(t, s, "draft"); !listed || empty {
		t.Fatalf("the linked draft after its text was erased: listed %v, empty %v; want listed and kept", listed, empty)
	}

	cleared, err := s.ClearThreadTicket("draft")
	if err != nil || !cleared.Had || cleared.Listing != ListingUnlisted {
		t.Fatalf("the unlink of the never-sent thread = %+v, %v; want it unlisted", cleared, err)
	}
	if listed, empty := listedAndEmpty(t, s, "draft"); listed || !empty {
		t.Fatalf("the unlinked draft: listed %v, empty %v; want deletable", listed, empty)
	}
	if deleted, err := s.DeleteEmptyDraftThread("draft"); err != nil || !deleted {
		t.Fatalf("the cleanup's delete of the unlinked draft = %v, %v; want deleted", deleted, err)
	}

	if set := mustSetThreadTicket(t, s, readLink(s, root, "sent", webCard("10002", 5))); set.Listing != ListingUnchanged {
		t.Fatalf("the link of a sent thread = %+v; want no listing change", set)
	}
	if cleared, err := s.ClearThreadTicket("sent"); err != nil || cleared.Listing != ListingUnchanged {
		t.Fatalf("the unlink of a sent thread = %+v, %v; want no listing change", cleared, err)
	}
}

// The listing rule's link term probes the thread's link by key, in the
// sidebar's list, the empty-draft test and the listing read a write makes.
func TestTheListingRuleProbesTheLinkByKey(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	listed, args := sidebarListedClause()
	for name, query := range map[string]string{
		"the empty-draft test":   emptyDraftThreadSQL,
		"a write's listing read": `SELECT EXISTS (SELECT 1 FROM owned_threads AS threads WHERE threads.id = ? AND ` + listed + `)`,
	} {
		plan := queryPlanText(t, s.db, "EXPLAIN QUERY PLAN "+query, append([]any{"t"}, args...)...)
		if !strings.Contains(plan, "SEARCH thread_tickets USING COVERING INDEX sqlite_autoindex_thread_tickets_1 (thread_id=?)") ||
			strings.Contains(plan, "SCAN thread_tickets") {
			t.Errorf("%s does not probe the link by key:\n%s", name, plan)
		}
	}
	list, listArgs := listThreadsWithItemsQuery()
	plan := queryPlanText(t, s.db, "EXPLAIN QUERY PLAN "+list, listArgs...)
	if !strings.Contains(plan, "SEARCH thread_tickets USING COVERING INDEX sqlite_autoindex_thread_tickets_1 (thread_id=?)") {
		t.Errorf("the sidebar's list does not probe the link by key:\n%s", plan)
	}
}

// A draft moved to another project on this computer takes its link: the
// destination is the linked thread, listed, and the emptied source is
// deletable. A draft with no link moves as before.
func TestADraftMoveTakesItsLink(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	for _, linked := range []bool{true, false} {
		suffix := map[bool]string{true: "linked", false: "plain"}[linked]
		source := createDraftCleanupThread(t, s, "source-"+suffix, "chat")
		destination := createDraftCleanupThread(t, s, "destination-"+suffix, "chat")
		draft := ThreadDraft{ThreadID: source.ID, Content: "moving text", Attachments: "[]", TerminalChips: "[]"}
		if _, err := s.UpsertThreadDraft(draft); err != nil {
			t.Fatal(err)
		}
		if linked {
			mustSetThreadTicket(t, s, readLink(s, root, source.ID, webCard("10001", 5)))
		}
		moved := draft
		moved.ThreadID = destination.ID
		move, err := s.MoveThreadDraft(draft, moved)
		if err != nil {
			t.Fatal(err)
		}
		if move.LinkMoved != linked || move.Destination.Listing != ListingListed || move.Source.Listing != ListingUnlisted {
			t.Fatalf("%s: the move = %+v; want the link moved %v, the destination listed, the source unlisted", suffix, move, linked)
		}
		_, sourceLinked, err := s.GetThreadTicket(source.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, destinationLinked, err := s.GetThreadTicket(destination.ID)
		if err != nil {
			t.Fatal(err)
		}
		if sourceLinked || destinationLinked != linked {
			t.Fatalf("%s: links after the move: source %v, destination %v", suffix, sourceLinked, destinationLinked)
		}
		if deleted, err := s.DeleteEmptyDraftThread(source.ID); err != nil || !deleted {
			t.Fatalf("%s: the emptied source's delete = %v, %v; want deleted", suffix, deleted, err)
		}
	}
}

// A draft move whose transaction fails moves nothing: the source keeps its
// draft and its link, and the destination has neither.
func TestADraftMoveThatFailsLeavesTheLinkOnTheSource(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	source := createDraftCleanupThread(t, s, "source", "chat")
	destination := createDraftCleanupThread(t, s, "destination", "chat")
	draft := ThreadDraft{ThreadID: source.ID, Content: "moving text", Attachments: "[]", TerminalChips: "[]"}
	if _, err := s.UpsertThreadDraft(draft); err != nil {
		t.Fatal(err)
	}
	mustSetThreadTicket(t, s, readLink(s, root, source.ID, webCard("10001", 5)))
	// The last write of the move fails.
	mustExec(t, s.db, `CREATE TRIGGER fail_link_move BEFORE UPDATE OF thread_id ON thread_tickets BEGIN SELECT RAISE(ABORT, 'the link move failed'); END`)
	moved := draft
	moved.ThreadID = destination.ID
	if _, err := s.MoveThreadDraft(draft, moved); err == nil || !strings.Contains(err.Error(), "the link move failed") {
		t.Fatalf("the failing move = %v", err)
	}
	if got, _, err := s.GetThreadDraft(source.ID); err != nil || got.Content != draft.Content {
		t.Fatalf("the source's draft after the failed move = %+v, %v", got, err)
	}
	if got, _, err := s.GetThreadDraft(destination.ID); err != nil || got.Content != "" {
		t.Fatalf("the destination's draft after the failed move = %+v, %v", got, err)
	}
	if _, linked, err := s.GetThreadTicket(source.ID); err != nil || !linked {
		t.Fatalf("the source's link after the failed move = %v, %v", linked, err)
	}
	if _, linked, err := s.GetThreadTicket(destination.ID); err != nil || linked {
		t.Fatalf("the destination's link after the failed move = %v, %v", linked, err)
	}
}

// draftCopy opens an outgoing copy of source's draft to another computer
// and takes it to committed, where the destination has installed it.
func draftCopy(t *testing.T, s *Store, draft ThreadDraft) (ThreadTransfer, transferwire.Upload) {
	t.Helper()
	request := transferRequest(draft.ThreadID, "copy", "outgoing")
	private, err := json.Marshal(struct {
		Draft *ThreadDraft `json:"draftToConsume"`
	}{&draft})
	if err != nil {
		t.Fatal(err)
	}
	request.PrivateState = private
	row, err := s.CreateThreadTransfer(request)
	if err != nil {
		t.Fatal(err)
	}
	archive := transferwire.Upload{SHA256: strings.Repeat("a", 64), Size: 1024}
	if _, err := s.BindThreadTransferArchive(row.ID, archive); err != nil {
		t.Fatal(err)
	}
	return row, archive
}

// A draft's copy to another computer's project carries the link, so the
// transaction that completes it ends the source's link with its draft,
// and only that one: a restart after the destination installed the copy
// leaves the source linked and listed until the completion; a completion
// that fails, and a copy canceled before it, leave it linked and listed.
func TestADraftCopyEndsTheSourceLinkOnlyWhenItCompletes(t *testing.T) {
	t.Parallel()
	t.Run("completed across a restart", func(t *testing.T) {
		t.Parallel()
		s, _, root := newMirror(t)
		createDraftCleanupThread(t, s, "source", "chat")
		draft := ThreadDraft{ThreadID: "source", Content: "portable draft", Attachments: "[]", TerminalChips: "[]"}
		if _, err := s.UpsertThreadDraft(draft); err != nil {
			t.Fatal(err)
		}
		mustSetThreadTicket(t, s, readLink(s, root, "source", webCard("10001", 5)))
		row, archive := draftCopy(t, s, draft)
		for _, phase := range []string{"prepared", "committed"} {
			if _, err := s.AdvanceThreadTransfer(row.ID, phase, archive.SHA256); err != nil {
				t.Fatal(err)
			}
		}
		s = reopenStore(t, s)
		if _, linked, err := s.GetThreadTicket("source"); err != nil || !linked {
			t.Fatalf("the source's link before the completion = %v, %v", linked, err)
		}
		if listed, empty := listedAndEmpty(t, s, "source"); !listed || empty {
			t.Fatalf("the source before the completion: listed %v, empty %v", listed, empty)
		}
		if _, err := s.AdvanceThreadTransfer(row.ID, "complete", archive.SHA256); err != nil {
			t.Fatal(err)
		}
		if _, linked, err := s.GetThreadTicket("source"); err != nil || linked {
			t.Fatalf("the source's link after the completion = %v, %v", linked, err)
		}
		if listed, empty := listedAndEmpty(t, s, "source"); listed || !empty {
			t.Fatalf("the source after the completion: listed %v, empty %v; want deletable", listed, empty)
		}
	})
	t.Run("a completion that fails", func(t *testing.T) {
		t.Parallel()
		s, _, root := newMirror(t)
		createDraftCleanupThread(t, s, "source", "chat")
		draft := ThreadDraft{ThreadID: "source", Content: "portable draft", Attachments: "[]", TerminalChips: "[]"}
		if _, err := s.UpsertThreadDraft(draft); err != nil {
			t.Fatal(err)
		}
		mustSetThreadTicket(t, s, readLink(s, root, "source", webCard("10001", 5)))
		row, archive := draftCopy(t, s, draft)
		for _, phase := range []string{"prepared", "committed"} {
			if _, err := s.AdvanceThreadTransfer(row.ID, phase, archive.SHA256); err != nil {
				t.Fatal(err)
			}
		}
		mustExec(t, s.db, `CREATE TRIGGER fail_link_end BEFORE DELETE ON thread_tickets BEGIN SELECT RAISE(ABORT, 'the link end failed'); END`)
		if _, err := s.AdvanceThreadTransfer(row.ID, "complete", archive.SHA256); err == nil || !strings.Contains(err.Error(), "the link end failed") {
			t.Fatalf("the failing completion = %v", err)
		}
		if got, _, err := s.GetThreadDraft("source"); err != nil || got.Content != draft.Content {
			t.Fatalf("the source's draft after the failed completion = %+v, %v", got, err)
		}
		if listed, empty := listedAndEmpty(t, s, "source"); !listed || empty {
			t.Fatalf("the source after the failed completion: listed %v, empty %v", listed, empty)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		t.Parallel()
		s, _, root := newMirror(t)
		createDraftCleanupThread(t, s, "source", "chat")
		draft := ThreadDraft{ThreadID: "source", Content: "portable draft", Attachments: "[]", TerminalChips: "[]"}
		if _, err := s.UpsertThreadDraft(draft); err != nil {
			t.Fatal(err)
		}
		mustSetThreadTicket(t, s, readLink(s, root, "source", webCard("10001", 5)))
		row, archive := draftCopy(t, s, draft)
		if _, err := s.AdvanceThreadTransfer(row.ID, "prepared", archive.SHA256); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AdvanceThreadTransfer(row.ID, "canceled", archive.SHA256); err != nil {
			t.Fatal(err)
		}
		if _, linked, err := s.GetThreadTicket("source"); err != nil || !linked {
			t.Fatalf("the source's link after the cancel = %v, %v", linked, err)
		}
		if listed, _ := listedAndEmpty(t, s, "source"); !listed {
			t.Fatal("the source after the cancel is not listed")
		}
	})
}
