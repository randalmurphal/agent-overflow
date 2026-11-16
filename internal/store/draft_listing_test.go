package store

import (
	"slices"
	"strings"
	"testing"
)

func draftWith(threadID, content string) ThreadDraft {
	return ThreadDraft{ThreadID: threadID, Content: content, Attachments: "[]", TerminalChips: "[]"}
}

func listedThreadIDs(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.ListThreadsWithItems()
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

// A never-sent thread is listed by the draft write that gives it content and
// unlisted by the one that erases it, and those writes alone report it,
// matching what the sidebar's catalog read lists.
func TestDraftWriteReportsTheWriteThatListsTheThread(t *testing.T) {
	s := newTestStore(t)
	thread := createDraftCleanupThread(t, s, "listed-by-draft", "chat")

	steps := []struct {
		content string
		want    DraftWrite
		listed  bool
	}{
		{content: "", want: DraftWrite{Changed: true}, listed: false},
		{content: "half a thought", want: DraftWrite{Changed: true, Listing: ListingListed}, listed: true},
		{content: "half a thought, more", want: DraftWrite{Changed: true}, listed: true},
		{content: "half a thought, more", want: DraftWrite{}, listed: true},
		{content: "   ", want: DraftWrite{Changed: true, Listing: ListingUnlisted}, listed: false},
		{content: "again", want: DraftWrite{Changed: true, Listing: ListingListed}, listed: true},
	}
	for i, step := range steps {
		got, err := s.UpsertThreadDraft(draftWith(thread.ID, step.content))
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got != step.want {
			t.Fatalf("step %d (%q): write %+v, want %+v", i, step.content, got, step.want)
		}
		if listed := slices.Contains(listedThreadIDs(t, s), thread.ID); listed != step.listed {
			t.Fatalf("step %d (%q): ListThreadsWithItems lists it %v, want %v", i, step.content, listed, step.listed)
		}
	}
}

// A thread something else already lists, or one the sidebar never lists, is
// not listed by its draft.
func TestDraftWriteListsNothingForThreadsListedOtherwiseOrNever(t *testing.T) {
	s := newTestStore(t)
	sent := createDraftCleanupThread(t, s, "sent", "chat")
	if err := s.InsertItem(Item{ID: "sent-item", ThreadID: sent.ID, Kind: "user_text", Role: "user", Status: "completed", Summary: "hi"}); err != nil {
		t.Fatal(err)
	}
	archived := createDraftCleanupThread(t, s, "archived", "chat")
	if _, _, err := s.ArchiveThread(archived.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{sent.ID, archived.ID} {
		got, err := s.UpsertThreadDraft(draftWith(id, "typed"))
		if err != nil {
			t.Fatal(err)
		}
		if got != (DraftWrite{Changed: true}) {
			t.Fatalf("%s: write %+v, want changed only", id, got)
		}
	}
}

// Returning a queued message to the composer of a thread nothing else lists
// lists it, in the restore's own transaction.
func TestRestoreFlushQueueToDraftReportsTheThreadListed(t *testing.T) {
	s := newTestStore(t)
	thread := createDraftCleanupThread(t, s, "restore-listed", "chat")
	if err := s.InsertFlushQueueItem(FlushQueueItem{ID: "queue", ThreadID: thread.ID, Message: "queued", EnqueuedAt: 1}); err != nil {
		t.Fatal(err)
	}
	expected, _, err := s.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	merged := expected
	merged.Content = "queued"
	written, err := s.RestoreFlushQueueToDraft(QueueDraftRestore{Expected: expected, Merged: merged, QueueItemIDs: []string{"queue"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := written.Draft; got != (DraftWrite{Changed: true, Listing: ListingListed}) {
		t.Fatalf("restore write %+v, want changed and listed", got)
	}
	if ids := listedThreadIDs(t, s); !slices.Equal(ids, []string{thread.ID}) {
		t.Fatalf("listed %v, want the restored thread", ids)
	}
}

// Moving a draft to another never-sent thread lists the destination and
// unlists the source.
func TestMoveThreadDraftReportsTheDestinationListed(t *testing.T) {
	s := newTestStore(t)
	source := createDraftCleanupThread(t, s, "move-source", "chat")
	destination := createDraftCleanupThread(t, s, "move-destination", "chat")
	draft := draftWith(source.ID, "moving text")
	if _, err := s.UpsertThreadDraft(draft); err != nil {
		t.Fatal(err)
	}
	got, err := s.MoveThreadDraft(draft, draftWith(destination.ID, draft.Content))
	if err != nil {
		t.Fatal(err)
	}
	want := DraftMove{
		Destination: DraftWrite{Changed: true, Listing: ListingListed},
		Source:      DraftWrite{Changed: true, Listing: ListingUnlisted},
	}
	if got != want {
		t.Fatalf("move %+v, want %+v", got, want)
	}
	if ids := listedThreadIDs(t, s); !slices.Equal(ids, []string{destination.ID}) {
		t.Fatalf("listed %v, want only the destination", ids)
	}
}

// A worktree makes a never-sent thread real: binding one lists the thread
// with no draft text, the fresh catalog read lists it, the empty-draft
// cleanup keeps it, and unbinding it takes the thread off the list again.
func TestAWorktreeListsANeverSentThread(t *testing.T) {
	s := newTestStore(t)
	thread := createDraftCleanupThread(t, s, "bound", "chat")
	listed := func() bool { return slices.Contains(listedThreadIDs(t, s), thread.ID) }
	if listed() {
		t.Fatal("an empty thread with no worktree is listed")
	}

	write, err := s.UpdateThreadCheckout(thread.ID, "/tmp/wt/bound", "/tmp/wt/bound", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if write.Listing != ListingListed || !listed() {
		t.Fatalf("bind: listing %v, listed %v; want listed", write.Listing, listed())
	}
	if empty, err := s.IsEmptyDraftThread(thread.ID); err != nil || empty {
		t.Fatalf("IsEmptyDraftThread = %v, %v; a listed thread is not an empty draft", empty, err)
	}
	if deleted, err := s.DeleteEmptyDraftThread(thread.ID); err != nil || deleted {
		t.Fatalf("DeleteEmptyDraftThread = %v, %v; want the bound thread kept", deleted, err)
	}

	// Typing and erasing text in a bound thread leaves it listed throughout.
	for _, content := range []string{"typed", ""} {
		got, err := s.UpsertThreadDraft(draftWith(thread.ID, content))
		if err != nil {
			t.Fatal(err)
		}
		if got.Listing != ListingUnchanged || !listed() {
			t.Fatalf("draft %q on a bound thread: listing %v, listed %v", content, got.Listing, listed())
		}
	}

	write, err = s.UpdateThreadCheckout(thread.ID, "/tmp/root", "", "main")
	if err != nil {
		t.Fatal(err)
	}
	if write.Listing != ListingUnlisted || listed() {
		t.Fatalf("unbind: listing %v, listed %v; want unlisted", write.Listing, listed())
	}
	if deleted, err := s.DeleteEmptyDraftThread(thread.ID); err != nil || !deleted {
		t.Fatalf("DeleteEmptyDraftThread after unbind = %v, %v; want it deleted", deleted, err)
	}
}

// Deleting the only draft text of a never-sent thread unlists it.
func TestDeleteThreadDraftReportsTheThreadUnlisted(t *testing.T) {
	s := newTestStore(t)
	thread := createDraftCleanupThread(t, s, "cleared", "chat")
	if _, err := s.UpsertThreadDraft(draftWith(thread.ID, "typed")); err != nil {
		t.Fatal(err)
	}
	got, err := s.DeleteThreadDraft(thread.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != (DraftWrite{Changed: true, Listing: ListingUnlisted}) {
		t.Fatalf("delete %+v, want changed and unlisted", got)
	}
	if got, err := s.DeleteThreadDraft(thread.ID, nil); err != nil || got != (DraftWrite{}) {
		t.Fatalf("second delete %+v, %v; want nothing", got, err)
	}
}

// The cleanup never deletes a thread the sidebar lists: each state that makes
// a never-sent thread real keeps it.
func TestEmptyDraftCleanupKeepsEveryListedThread(t *testing.T) {
	s := newTestStore(t)
	cases := map[string]func(id string){
		"draft text": func(id string) {
			if _, err := s.UpsertThreadDraft(draftWith(id, "typed")); err != nil {
				t.Fatal(err)
			}
		},
		"worktree": func(id string) {
			if _, err := s.UpdateThreadCheckout(id, "/tmp/wt/"+id, "/tmp/wt/"+id, "b-"+id); err != nil {
				t.Fatal(err)
			}
		},
		"item": func(id string) {
			if err := s.InsertItem(Item{ID: id + "-item", ThreadID: id, Kind: "user_text", Role: "user", Status: "completed", Summary: "hi"}); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, realize := range cases {
		thread := createDraftCleanupThread(t, s, "real-"+strings.ReplaceAll(name, " ", "-"), "chat")
		realize(thread.ID)
		if !slices.Contains(listedThreadIDs(t, s), thread.ID) {
			t.Fatalf("%s: not listed", name)
		}
		if deleted, err := s.DeleteEmptyDraftThread(thread.ID); err != nil || deleted {
			t.Fatalf("%s: DeleteEmptyDraftThread = %v, %v; want kept", name, deleted, err)
		}
	}
}
