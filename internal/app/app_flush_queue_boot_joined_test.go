package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// joinedMember is one queued message a joined row carries.
type joinedMember struct {
	sendID, text string
	// queued keeps the member's durable queue copy beside the row.
	queued bool
}

// renderedSummaryKey in seedJoinedBootResidue's meta replaces the row's
// summary (a message that rendered its review comments) instead of landing
// in the meta.
const renderedSummaryKey = "test:renderedSummary"

// seedJoinedBootResidue writes what a crash leaves of one headless Claude
// provider message that joined members: its unconfirmed row and the queue
// copies of the members whose settlement had not run.
func seedJoinedBootResidue(t *testing.T, app *App, threadID, rowID string, members []joinedMember, meta map[string]any) {
	t.Helper()
	texts := make([]string, 0, len(members))
	sendIDs := make([]string, 0, len(members))
	for i, member := range members {
		texts = append(texts, member.text)
		sendIDs = append(sendIDs, member.sendID)
		if !member.queued {
			continue
		}
		if err := app.store.InsertFlushQueueItem(store.FlushQueueItem{
			ID: "queue-" + member.sendID, ThreadID: threadID, SendID: member.sendID, Message: member.text, EnqueuedAt: int64(i + 1),
		}); err != nil {
			t.Fatalf("InsertFlushQueueItem(%s): %v", member.sendID, err)
		}
	}
	summary := strings.Join(texts, usermessage.JoinSeparator)
	fields := map[string]any{"sendId": sendIDs[0], "joinedSendIds": sendIDs, "pendingFlush": true}
	for key, value := range meta {
		if key == renderedSummaryKey {
			summary = value.(string)
			continue
		}
		fields[key] = value
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID: rowID, ThreadID: threadID, TurnIndex: 1, Kind: "user_text", Role: "user",
		Status: "completed", Summary: summary, Meta: string(encoded), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("AppendItem: %v", err)
	}
}

func newJoinedBootThread(t *testing.T, app *App, id string) store.Thread {
	t.Helper()
	thread := testThread(id)
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return thread
}

// requireBootLeftNothing checks that the boot took the row and every queue
// row of the thread.
func requireBootLeftNothing(t *testing.T, app *App, threadID, rowID string) {
	t.Helper()
	if _, found, err := app.store.GetThreadItem(threadID, rowID); err != nil || found {
		t.Fatalf("joined row after boot: found=%v err=%v, want it gone", found, err)
	}
	if rows := durableQueueRows(t, app, threadID); len(rows) != 0 {
		t.Fatalf("queue rows after boot = %+v, want none", rows)
	}
}

// A headless Claude drain sends several queued messages as one provider
// message with one row; the dispatch settles their queue rows after the
// write. A crash in between leaves the unconfirmed row beside the queue
// copies of some, all or none of its members. The row is restored whole, so
// every member's text reaches the composer once and in send order, and the
// queue copies add nothing. A second boot changes nothing.
func TestBootRestoresAJoinedRowWholeBesideItsQueueCopies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		members []joinedMember
	}{
		{name: "the second copy left", members: []joinedMember{{"one", "first message", false}, {"two", "second message", true}}},
		{name: "every copy left", members: []joinedMember{{"one", "first message", true}, {"two", "second message", true}}},
		{name: "no copy left", members: []joinedMember{{"one", "first message", false}, {"two", "second message", false}}},
		{name: "the middle copy of three left", members: []joinedMember{{"one", "first message", false}, {"two", "second message", true}, {"three", "third message", false}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app, _ := newAppForFlushQueueRPC(t)
			thread := newJoinedBootThread(t, app, "boot-joined")
			seedJoinedBootResidue(t, app, thread.ID, "user:1:flush:1", tc.members, nil)

			app.restoreDurableFlushQueueAtBoot()

			draft, _, err := app.store.GetThreadDraft(thread.ID)
			if err != nil {
				t.Fatalf("GetThreadDraft: %v", err)
			}
			texts := make([]string, 0, len(tc.members))
			for _, member := range tc.members {
				texts = append(texts, member.text)
			}
			if want := strings.Join(texts, usermessage.JoinSeparator); draft.Content != want {
				t.Fatalf("draft = %q, want %q", draft.Content, want)
			}
			requireBootLeftNothing(t, app, thread.ID, "user:1:flush:1")

			app.restoreDurableFlushQueueAtBoot()
			again, _, err := app.store.GetThreadDraft(thread.ID)
			if err != nil {
				t.Fatalf("GetThreadDraft: %v", err)
			}
			if again != draft {
				t.Fatalf("draft after a second boot = %+v, want %+v unchanged", again, draft)
			}
		})
	}
}

// The joined row keeps its members' attachments and the review comments it
// claimed for the whole message. The boot gives the composer every member's
// attachment and the text without the comments, and returns the comments
// to drafts, whichever member's queue copy survived.
func TestBootRestoresAJoinedRowsAttachmentsAndReviewComments(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	thread := newJoinedBootThread(t, app, "boot-joined-carried")
	const rowID = "user:1:flush:1"
	owner := store.ThreadReviewOwner(thread.ID)
	comment, err := app.store.CreateDiffReviewComment(store.DiffReviewComment{
		ID: "comment-1", ThreadID: thread.ID, Scope: string(store.DiffReviewScopeWorkspace), SourceKey: "diff-1",
		FilePath: "a.go", NewLine: 1, Side: "new", Body: "Rename it.", CreatedAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatalf("CreateDiffReviewComment: %v", err)
	}
	if _, err := app.store.ClaimDiffReviewCommentsSent(owner, comment.Scope, comment.SourceKey,
		[]store.DiffReviewCommentSnapshot{{ID: comment.ID, Body: comment.Body}},
		store.DiffReviewDelivery{SentAt: 2, TurnID: thread.ID + ":1", ThreadID: thread.ID, ItemID: rowID, Queued: true}); err != nil {
		t.Fatalf("ClaimDiffReviewCommentsSent: %v", err)
	}
	// Member one carries an image; member two carries the review comment,
	// and its queue copy is the one that survived.
	text := "look at [Image #1]" + usermessage.JoinSeparator + "fix the review"
	seedJoinedBootResidue(t, app, thread.ID, rowID, []joinedMember{
		{"one", "look at [Image #1]", false},
		{"two", "fix the review", true},
	}, map[string]any{
		"attachments":                  []map[string]any{{"id": "att-1", "threadId": thread.ID, "filename": "a.png", "mimeType": "image/png", "size": 1}},
		"revisionSourceDiffReview":     map[string]any{"threadId": thread.ID, "scope": comment.Scope, "sourceKey": comment.SourceKey},
		"revisionSourceDiffCommentIds": []string{comment.ID},
		"textBeforeReviewComments":     text,
		renderedSummaryKey:             text + "\n\nReview comments:\n- a.go:1 Rename it.",
	})

	app.restoreDurableFlushQueueAtBoot()

	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if draft.Content != text {
		t.Fatalf("draft = %q, want %q", draft.Content, text)
	}
	if draft.Attachments != `["att-1"]` {
		t.Fatalf("draft attachments = %s, want the joined row's image", draft.Attachments)
	}
	if got, err := app.store.GetDiffReviewComment(owner, comment.ID); err != nil || got.Status != "draft" {
		t.Fatalf("comment after boot = %+v, %v; want a draft", got, err)
	}
	requireBootLeftNothing(t, app, thread.ID, rowID)
}

// An answer to an agent's question keeps its queue row until the provider
// echoes it. As a member of an unconfirmed joined row it is restored once,
// with the row, and its question reads as restored.
func TestBootRestoresAJoinedRowsAsyncAnswerMemberOnce(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	thread := newJoinedBootThread(t, app, "boot-joined-answer")
	question, err := app.store.RecordAsyncQuestions(store.Item{
		ID: "question:ask", ThreadID: thread.ID, Kind: "assistant_text", Role: "assistant", Status: "completed",
		Meta: `{"delivery":"async","questions":[{"title":"Which?"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	answers := []store.AsyncQuestionAnswer{{ItemID: question.ID, Index: 0, Answer: "First"}}
	body, _, err := app.store.AsyncAnswerMessage(thread.ID, "two", answers)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.QueueAsyncAnswers(store.FlushQueueItem{ID: "queue-two", ThreadID: thread.ID, SendID: "two", Message: body, EnqueuedAt: 2}, answers); err != nil {
		t.Fatal(err)
	}
	// The dispatch settled both members; the answer's row stays by design.
	app.settleFlushGroup(thread.ID, []triage.QueuedFlushItem{{ID: "queue-one"}, {ID: "queue-two"}})
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 1 {
		t.Fatalf("queue rows after the write = %+v, want the answer's", rows)
	}
	seedJoinedBootResidue(t, app, thread.ID, "user:1:flush:1", []joinedMember{
		{"one", "first message", false},
		{"two", body, false},
	}, nil)

	app.restoreDurableFlushQueueAtBoot()

	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if want := "first message" + usermessage.JoinSeparator + body; draft.Content != strings.TrimSpace(want) {
		t.Fatalf("draft = %q, want %q", draft.Content, want)
	}
	rows, err := app.store.ListAsyncQuestions(thread.ID, question.ID)
	if err != nil || len(rows) != 1 || rows[0].State != "restored" {
		t.Fatalf("question after boot = %+v, %v; want restored", rows, err)
	}
	requireBootLeftNothing(t, app, thread.ID, "user:1:flush:1")
}

// The provider can confirm a message before the dispatch that wrote it
// settles its queue row. A crash in between leaves a confirmed row beside
// the copy: the message was delivered, so the boot drops the copy and leaves
// the composer alone. A row the provider has not confirmed (an interrupt
// revealed it before the echo) proves nothing, so the copy is restored.
func TestBootDropsTheQueueCopyOfAConfirmedMessage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, meta, wantDraft string
	}{
		{name: "confirmed", meta: `{"sendId":"one","joinedSendIds":["one","two"],"provider_item_id":"uuid-1"}`},
		{name: "revealed unconfirmed", meta: `{"sendId":"one","joinedSendIds":["one","two"],"promoted_at_interrupt":true}`, wantDraft: "second message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app, _ := newAppForFlushQueueRPC(t)
			thread := newJoinedBootThread(t, app, "boot-confirmed-copy")
			now := time.Now().UnixMilli()
			if _, err := app.store.AppendItem(store.Item{
				ID: "user:1:flush:1", ThreadID: thread.ID, TurnIndex: 1, Kind: "user_text", Role: "user", Status: "completed",
				Summary: "first message" + usermessage.JoinSeparator + "second message", Meta: tc.meta, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("AppendItem: %v", err)
			}
			if err := app.store.InsertFlushQueueItem(store.FlushQueueItem{ID: "queue-two", ThreadID: thread.ID, SendID: "two", Message: "second message", EnqueuedAt: 2}); err != nil {
				t.Fatalf("InsertFlushQueueItem: %v", err)
			}

			app.restoreDurableFlushQueueAtBoot()

			draft, _, err := app.store.GetThreadDraft(thread.ID)
			if err != nil {
				t.Fatalf("GetThreadDraft: %v", err)
			}
			if draft.Content != tc.wantDraft {
				t.Fatalf("draft = %q, want %q", draft.Content, tc.wantDraft)
			}
			if rows := durableQueueRows(t, app, thread.ID); len(rows) != 0 {
				t.Fatalf("queue rows after boot = %+v, want none", rows)
			}
			if _, found, err := app.store.GetThreadItem(thread.ID, "user:1:flush:1"); err != nil || !found {
				t.Fatalf("confirmed row after boot: found=%v err=%v, want it kept", found, err)
			}
		})
	}
}

// A dispatch settles the queue rows of every message it carried in one
// write: when that write fails, every member keeps its row for the boot,
// never some of them.
func TestFlushGroupSettlesItsMembersQueueRowsTogether(t *testing.T) {
	t.Parallel()
	app, dbPath := newTestAppWithStorePath(t)
	thread := newJoinedBootThread(t, app, "settle-joined-together")
	group := []triage.QueuedFlushItem{{ID: "queue-one"}, {ID: "queue-two"}}
	for i, item := range group {
		if err := app.store.InsertFlushQueueItem(store.FlushQueueItem{ID: item.ID, ThreadID: thread.ID, Message: item.ID, EnqueuedAt: int64(i + 1)}); err != nil {
			t.Fatalf("InsertFlushQueueItem: %v", err)
		}
	}

	execOnStore(t, dbPath, `CREATE TRIGGER refuse_queue_delete BEFORE DELETE ON flush_queue_items WHEN OLD.id = 'queue-two' BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`)
	app.settleFlushGroup(thread.ID, group)
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 2 {
		t.Fatalf("queue rows after a failed settlement = %+v, want both members'", rows)
	}
	execOnStore(t, dbPath, `DROP TRIGGER refuse_queue_delete`)
	app.settleFlushGroup(thread.ID, group)
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 0 {
		t.Fatalf("queue rows after settlement = %+v, want none", rows)
	}
}
