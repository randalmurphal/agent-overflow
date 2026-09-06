package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestTransferHistoryMetadataTransformPreservesReverseIDTimelineAcrossPages(t *testing.T) {
	source, destination := newTestStore(t), newTestStore(t)
	mustCreateThread(t, source, "source")
	thread, err := source.GetThread("source")
	if err != nil {
		t.Fatal(err)
	}
	const count = 260
	for i := range count {
		item := Item{ID: fmt.Sprintf("item-%03d", count-i), ThreadID: thread.ID,
			TurnIndex: 0, ItemIndex: i, Kind: "assistant_text", Role: "assistant",
			Summary: fmt.Sprintf("message %d", i), Status: "completed", Meta: `{"native":"original"}`}
		if err := source.InsertItem(item); err != nil {
			t.Fatal(err)
		}
	}
	var snapshot bytes.Buffer
	transformed := 0
	err = source.ExportThreadHistoryWith(context.Background(), thread.ID, &snapshot, ThreadHistoryExport{
		ItemMeta: func(meta string) (string, error) {
			if meta != `{"native":"original"}` {
				t.Fatalf("unexpected source metadata: %s", meta)
			}
			transformed++
			return `{"native":"copy"}`, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if transformed != count {
		t.Fatalf("transformed %d rows, want %d", transformed, count)
	}
	thread.ID = "destination"
	if err := destination.ImportThreadHistory(context.Background(), thread, &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, storeAndMeta := range []struct {
		store    *Store
		id, meta string
	}{
		{source, "source", `{"native":"original"}`}, {destination, "destination", `{"native":"copy"}`},
	} {
		items, err := storeAndMeta.store.ListItems(storeAndMeta.id)
		if err != nil || len(items) != count {
			t.Fatalf("rows=%d, err=%v", len(items), err)
		}
		for i, item := range items {
			if item.ID != fmt.Sprintf("item-%03d", count-i) || item.ItemIndex != i || item.Summary != fmt.Sprintf("message %d", i) || item.Meta != storeAndMeta.meta {
				t.Fatalf("changed timeline row %d: %+v", i, item)
			}
		}
	}
}

func TestTransferHistoryChecksLiveIdentityConflictsBeforeActivation(t *testing.T) {
	live, candidate := newTestStore(t), newTestStore(t)
	mustCreateThread(t, live, "existing")
	mustCreateThread(t, candidate, "incoming")
	for _, pair := range []struct {
		s  *Store
		id string
	}{{live, "existing"}, {candidate, "incoming"}} {
		if err := pair.s.InsertTurn(Turn{ThreadID: pair.id, TurnID: "colliding-turn", TurnIndex: 0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := live.CheckTransferHistoryConflicts(context.Background(), candidate, "incoming"); err == nil {
		t.Fatal("accepted globally conflicting turn identity")
	}
	if _, err := candidate.db.Exec(`UPDATE turns SET turn_id = 'incoming:independent' WHERE thread_id = 'incoming'`); err != nil {
		t.Fatal(err)
	}
	if err := live.CheckTransferHistoryConflicts(context.Background(), candidate, "incoming"); err != nil {
		t.Fatal(err)
	}
}

func TestTransferHistoryRoundTripWithLargePayloadAndSubagentRows(t *testing.T) {
	source, destination := newTestStore(t), newTestStore(t)
	mustCreateThread(t, source, "thread-a")
	thread, err := source.GetThread("thread-a")
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("long command output\n"), 50_000)
	parent := Item{ID: "parent", ThreadID: thread.ID, TurnIndex: 0, ItemIndex: 0, Kind: "tool_call", Role: "assistant", Status: "completed", Summary: "command", PayloadID: "output", Meta: `{}`}
	if err := source.InsertItemWithPayload(parent, Payload{ID: "output", Kind: "tool_output", Meta: `{}`, Data: body[:100], CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := source.AppendPayloadData(thread.ID, "output", body[100:], `{}`, 1); err != nil {
		t.Fatal(err)
	}
	if err := source.InsertItem(Item{ID: "child", ThreadID: thread.ID, TurnIndex: 0, ItemIndex: 1, ParentID: "parent", Kind: "assistant_text", Role: "assistant", Summary: "nested", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := source.InsertTurn(Turn{ThreadID: thread.ID, TurnID: "turn-1", TurnIndex: 0, StartedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.db.Exec(`UPDATE turns SET completed_at = 2, stop_reason = 'end_turn', token_usage_json = '{"input_tokens":1}' WHERE thread_id = ?`, thread.ID); err != nil {
		t.Fatal(err)
	}
	if err := source.InsertAttachment(Attachment{ID: "image-a", ThreadID: thread.ID, Kind: AttachmentKindImage, Filename: "photo.png", MimeType: "image/png", Size: 3, RelativePath: "thread-a/photo.png", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.UpsertThreadDraft(ThreadDraft{ThreadID: thread.ID, Content: "continue here", Attachments: `["image-a"]`, TerminalChips: `["source-terminal"]`}); err != nil {
		t.Fatal(err)
	}
	var snapshot bytes.Buffer
	if err := source.ExportThreadHistory(context.Background(), thread.ID, &snapshot); err != nil {
		t.Fatal(err)
	}
	target := thread
	target.ID = "thread-b"
	target.WorkspacePath = "/another/checkout"
	if err := destination.ImportThreadHistory(context.Background(), target, bytes.NewReader(snapshot.Bytes())); err != nil {
		t.Fatal(err)
	}
	got, err := destination.GetPayloadData(target.ID, "output")
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("lost payload bytes: %d %v", len(got), err)
	}
	items, err := destination.ListItems(target.ID)
	if err != nil || len(items) != 2 || items[1].ParentID != "parent" {
		t.Fatalf("lost nested history: %+v %v", items, err)
	}
	turn, found, err := destination.GetTurnByThreadIndex(target.ID, 0)
	if err != nil || !found || turn.CompletedAt == nil || turn.TokenUsageJSON == "" {
		t.Fatalf("lost turn: %+v %v", turn, err)
	}
	attachments, err := destination.ListAttachments(target.ID)
	if err != nil || len(attachments) != 1 {
		t.Fatalf("lost attachments: %+v %v", attachments, err)
	}
	draft, found, err := destination.GetThreadDraft(target.ID)
	if err != nil || !found || draft.Content != "continue here" || draft.TerminalChips != "[]" {
		t.Fatalf("draft: %+v %v", draft, err)
	}
	stored, err := destination.GetThread(target.ID)
	if err != nil || stored.WorkspacePath != target.WorkspacePath {
		t.Fatalf("archive changed execution target: %+v %v", stored, err)
	}
}

func TestTransferHistoryRefusesPartialOrForeignDataAtomically(t *testing.T) {
	source := newTestStore(t)
	mustCreateThread(t, source, "thread-a")
	thread, err := source.GetThread("thread-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.InsertItemWithPayload(Item{ID: "item", ThreadID: thread.ID, Kind: "assistant_text", Role: "assistant", PayloadID: "payload"}, Payload{ID: "payload", Kind: "markdown", Data: []byte("answer"), Meta: `{}`}); err != nil {
		t.Fatal(err)
	}
	var original bytes.Buffer
	if err := source.ExportThreadHistory(context.Background(), thread.ID, &original); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing end", "missing chunk", "foreign item", "future version", "duplicate record", "canceled"} {
		t.Run(name, func(t *testing.T) {
			destination := newTestStore(t)
			target := thread
			target.ID = "thread-b"
			var edited bytes.Buffer
			for _, line := range bytes.Split(bytes.TrimSpace(original.Bytes()), []byte{'\n'}) {
				var record transferHistoryRecord
				if err := json.Unmarshal(line, &record); err != nil {
					t.Fatal(err)
				}
				if name == "missing end" && record.Kind == "end" || name == "missing chunk" && record.Kind == "payload_chunk" {
					continue
				}
				if name == "foreign item" && record.Kind == "item" {
					record.Data = bytes.ReplaceAll(record.Data, []byte("thread-a"), []byte("another"))
				}
				if name == "future version" {
					record.Version++
				}
				if err := json.NewEncoder(&edited).Encode(record); err != nil {
					t.Fatal(err)
				}
				if name == "duplicate record" && record.Kind == "item" {
					if err := json.NewEncoder(&edited).Encode(record); err != nil {
						t.Fatal(err)
					}
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "canceled" {
				cancel()
			}
			if err := destination.ImportThreadHistory(ctx, target, bytes.NewReader(edited.Bytes())); err == nil {
				t.Fatal("accepted invalid history")
			}
			if exists, err := destination.ThreadExists(target.ID); err != nil || exists {
				t.Fatalf("partial destination is visible: %v %v", exists, err)
			}
		})
	}
	if strings.Contains(original.String(), "preview_spans") {
		t.Fatal("exported derived render caches")
	}
}

func TestTransferHistoryCarriesInheritedAttachmentsAndAllowsIndependentCopies(t *testing.T) {
	source, destination := newTestStore(t), newTestStore(t)
	mustCreateThread(t, source, "parent")
	mustCreateThread(t, source, "fork")
	if err := source.InsertAttachment(Attachment{ID: "inherited", ThreadID: "parent", Kind: AttachmentKindFile, Filename: "notes.txt", RelativePath: "parent/inherited/notes.txt", MimeType: "text/plain", Size: 3}); err != nil {
		t.Fatal(err)
	}
	if err := source.InsertItem(Item{ID: "user", ThreadID: "fork", Kind: "user_text", Role: "user", Meta: `{"provider_item_id":"native-wire-id","attachments":[{"id":"inherited","threadId":"parent","filename":"notes.txt"}]}`}); err != nil {
		t.Fatal(err)
	}
	thread, err := source.GetThread("fork")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot bytes.Buffer
	if err := source.ExportThreadHistory(context.Background(), thread.ID, &snapshot); err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]bool)
	for _, id := range []string{"copy-one", "copy-two"} {
		target := thread
		target.ID = id
		if err := destination.ImportThreadHistory(context.Background(), target, bytes.NewReader(snapshot.Bytes())); err != nil {
			t.Fatal(err)
		}
		attachments, err := destination.ListAttachments(id)
		if err != nil || len(attachments) != 1 {
			t.Fatalf("inherited file missing: %+v %v", attachments, err)
		}
		a := attachments[0]
		if a.ID == "inherited" || ids[a.ID] || !strings.HasPrefix(a.RelativePath, id+"/") {
			t.Fatalf("copy aliases original or another copy: %+v", a)
		}
		ids[a.ID] = true
		items, err := destination.ListItems(id)
		if err != nil || len(items) != 1 {
			t.Fatalf("items: %+v %v", items, err)
		}
		if !strings.Contains(items[0].Meta, a.ID) || !strings.Contains(items[0].Meta, `"threadId":"`+id+`"`) || !strings.Contains(items[0].Meta, "native-wire-id") {
			t.Fatalf("wrong attachment owner: %s", items[0].Meta)
		}
	}
}

func TestTransferHistoryCopiesReviewNotesWithoutAliasingTheirIDs(t *testing.T) {
	source, destination := newTestStore(t), newTestStore(t)
	mustCreateThread(t, source, "source")
	if err := source.InsertTurn(Turn{ThreadID: "source", TurnID: "source:wire-turn", TurnIndex: 0, ProviderTurnID: "wire-turn"}); err != nil {
		t.Fatal(err)
	}
	if err := source.UpdateTurnCompleted("source:wire-turn", 2, "end_turn", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := source.InsertItem(Item{ID: "plan", ThreadID: "source", Kind: "assistant_text", Role: "assistant", Summary: "plan"}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.EnsureProposedPlanState("source", "plan", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := source.db.Exec(`INSERT INTO proposed_plan_comments (` + proposedPlanCommentColumns + `) VALUES ('note','source','plan','sent',1,1,'selected','keep this note',2,'source:wire-turn',1,2)`); err != nil {
		t.Fatal(err)
	}
	if err := source.InsertItem(Item{ID: "revision", ThreadID: "source", Kind: "user_text", Role: "user", ItemIndex: 1, Meta: `{"revisionSourceProposedPlan":{"threadId":"source","itemId":"plan"},"revisionSourceCommentIds":["note"]}`}); err != nil {
		t.Fatal(err)
	}
	thread, err := source.GetThread("source")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot bytes.Buffer
	if err := source.ExportThreadHistory(context.Background(), "source", &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"copy-a", "copy-b", "source"} {
		target := thread
		target.ID = id
		if err := destination.ImportThreadHistory(context.Background(), target, bytes.NewReader(snapshot.Bytes())); err != nil {
			t.Fatal(err)
		}
		comments, err := destination.ListProposedPlanComments(id, "plan")
		if err != nil || len(comments) != 1 || (comments[0].ID == "note") != (id == "source") || comments[0].Body != "keep this note" {
			t.Fatalf("comments: %+v %v", comments, err)
		}
		turn, found, err := destination.GetTurnByThreadIndex(id, 0)
		if err != nil || !found || turn.TurnID != id+":wire-turn" || turn.ProviderTurnID != "wire-turn" || comments[0].SentTurnID != turn.TurnID {
			t.Fatalf("copy/move lost independent turn identity or comment reference: %+v %+v %v", turn, comments, err)
		}
		items, err := destination.ListItems(id)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(items[1].Meta, comments[0].ID) || !strings.Contains(items[1].Meta, `"threadId":"`+id+`"`) {
			t.Fatalf("revision note link is stale: %s", items[1].Meta)
		}
	}
}
