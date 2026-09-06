package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestTransferActivationRequiresSecretAndCompleteHistory(t *testing.T) {
	for _, kind := range []string{"move", "copy"} {
		t.Run(kind, func(t *testing.T) {
			s := newTestStore(t)
			request := transferRequest("incoming", kind, "incoming")
			if _, err := s.CreateThreadTransfer(request); err != nil {
				t.Fatal(err)
			}
			digest := strings.Repeat("b", 64)
			if _, err := s.CheckThreadTransferActivation(request.ID, digest, transferTestSecret()); err == nil {
				t.Fatal("activated before preparation")
			}
			advanceTransfer(t, s, request.ID, "prepared")
			for _, phase := range []string{"committed", "complete"} {
				if _, err := s.AdvanceThreadTransfer(request.ID, phase, digest); err == nil {
					t.Fatal("phase-only destination activation")
				}
			}
			for _, secret := range [][]byte{nil, []byte("short"), bytes.Repeat([]byte{0xb6}, 32)} {
				if _, err := s.CheckThreadTransferActivation(request.ID, digest, secret); err == nil {
					t.Fatal("accepted wrong activation secret")
				}
			}
			if _, err := s.CheckThreadTransferActivation(request.ID, strings.Repeat("c", 64), transferTestSecret()); err == nil {
				t.Fatal("activated different content")
			}
			if _, err := s.CommitIncomingThreadTransfer(context.Background(), request.ID, digest, transferTestSecret(), makeThread("other", "claude"), nil); err == nil {
				t.Fatal("activated another conversation")
			}
			if _, err := s.CommitIncomingThreadTransfer(context.Background(), request.ID, digest, transferTestSecret(), makeThread(request.ThreadID, "claude"), strings.NewReader("{}\n")); err == nil {
				t.Fatal("published malformed history")
			}
			row, err := s.GetThreadTransfer(request.ID)
			if err != nil || row.Phase != "prepared" {
				t.Fatalf("failed import advanced: %+v %v", row, err)
			}
			var busy *ThreadTransferError
			if !errors.As(s.CheckThreadTransferAccess(request.ThreadID), &busy) {
				t.Fatal("failed import released ownership")
			}
			activateTestTransfer(t, s, request)
			if err := s.CheckThreadTransferAccess(request.ThreadID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CommitIncomingThreadTransfer(context.Background(), request.ID, digest, transferTestSecret(), makeThread(request.ThreadID, "claude"), nil); err != nil {
				t.Fatalf("lost reply retry: %v", err)
			}
		})
	}
}

func TestReturningTransferReplacesHistoryWithoutDeletingLocalReferences(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "return.db")
	if err := copyTestStoreTemplate(dbPath); err != nil {
		t.Fatal(err)
	}
	s, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	mustCreateThread(t, s, "returning")
	child := makeThread("local-fork", "claude")
	child.ForkedFromThreadID, child.ParentThreadID = "returning", "returning"
	if err := s.CreateThread(child); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertItem(Item{ID: "old", ThreadID: "returning", Kind: "assistant_text", Role: "assistant", Summary: "old history"}); err != nil {
		t.Fatal(err)
	}
	oldAttachment := Attachment{ID: "old-photo", ThreadID: "returning", Kind: AttachmentKindImage, RelativePath: "returning/old-photo.png", Filename: "old.png", Size: 3, MimeType: "image/png"}
	if err := s.InsertAttachment(oldAttachment); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE threads SET history_rev = 100, history_epoch = 50 WHERE id = 'returning'`); err != nil {
		t.Fatal(err)
	}
	outgoing := transferRequest("returning", "move", "outgoing")
	if _, err := s.CreateThreadTransfer(outgoing); err != nil {
		t.Fatal(err)
	}
	advanceTransfer(t, s, outgoing.ID, "prepared", "committed", "complete")
	incoming := transferRequest("returning", "move", "incoming")
	incoming.OwnershipEpoch = 2
	if _, err := s.CreateThreadTransfer(incoming); err != nil {
		t.Fatal(err)
	}
	advanceTransfer(t, s, incoming.ID, "prepared")
	remote := newTestStore(t)
	target := makeThread("returning", "claude")
	target.Title, target.WorkspacePath = "Returned conversation", "/new/checkout"
	if err := remote.CreateThread(target); err != nil {
		t.Fatal(err)
	}
	if err := remote.InsertItem(Item{ID: "new", ThreadID: target.ID, Kind: "assistant_text", Role: "assistant", Summary: "new history"}); err != nil {
		t.Fatal(err)
	}
	if err := remote.InsertAttachment(oldAttachment); err != nil {
		t.Fatal(err)
	}
	var history bytes.Buffer
	if err := remote.ExportThreadHistory(context.Background(), target.ID, &history); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("b", 64)
	// A failed replacement must retain the old row, history, counters and
	// incoming fence together, even after it began deleting the old cache.
	broken := bytes.Replace(history.Bytes(), []byte(`"kind":"end"`), []byte(`"kind":"future"`), 1)
	if _, err := s.CommitIncomingThreadTransfer(context.Background(), incoming.ID, digest, transferTestSecret(), target, bytes.NewReader(broken)); err == nil {
		t.Fatal("committed incomplete returning history")
	}
	items, err := s.ListItems(target.ID)
	if err != nil || len(items) != 1 || items[0].ID != "old" {
		t.Fatalf("failed return erased source cache: %+v %v", items, err)
	}
	stamp, _, err := s.ThreadHistoryStamp(target.ID)
	if err != nil || stamp.Rev != 100 || stamp.Epoch != 50 {
		t.Fatalf("failed return changed stamp: %+v %v", stamp, err)
	}
	if _, err := s.CommitIncomingThreadTransfer(context.Background(), incoming.ID, digest, transferTestSecret(), target, &history); err != nil {
		t.Fatal(err)
	}
	items, err = s.ListItems(target.ID)
	if err != nil || len(items) != 1 || items[0].ID != "new" {
		t.Fatalf("history not replaced: %+v %v", items, err)
	}
	got, err := s.GetThread(child.ID)
	if err != nil || got.ParentThreadID != target.ID || got.ForkedFromThreadID != target.ID {
		t.Fatalf("local fork links lost: %+v %v", got, err)
	}
	photo, found, err := s.GetAttachment(oldAttachment.ID)
	if err != nil || !found || photo != oldAttachment {
		t.Fatalf("old upload changed: %+v %v", photo, err)
	}
	stamp, _, err = s.ThreadHistoryStamp(target.ID)
	if err != nil || stamp.Rev <= 100 || stamp.Epoch <= 50 {
		t.Fatalf("stale renderer cache still valid: %+v %v", stamp, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitIncomingThreadTransfer(context.Background(), incoming.ID, digest, transferTestSecret(), target, nil); err != nil {
		t.Fatalf("restart and lost reply: %v", err)
	}
	if err := s.CheckThreadTransferAccess(target.ID); err != nil {
		t.Fatal(err)
	}
}

func TestTransferActivationRefusesAnExistingUnretiredConversation(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "existing")
	request := transferRequest("existing", "move", "incoming")
	if _, err := s.CreateThreadTransfer(request); err == nil {
		t.Fatal("reserved an existing conversation")
	}
	if err := s.CheckThreadTransferAccess("existing"); err != nil {
		t.Fatal(err)
	}
	request = transferRequest("incoming", "move", "incoming")
	if _, err := s.CreateThreadTransfer(request); err != nil {
		t.Fatal(err)
	}
	// Another local writer bypassing the reservation must still be refused
	// by activation's transaction, rather than letting its row be replaced.
	mustCreateThread(t, s, "incoming")
	advanceTransfer(t, s, request.ID, "prepared")
	if _, err := s.CommitIncomingThreadTransfer(context.Background(), request.ID, strings.Repeat("b", 64), transferTestSecret(), makeThread("incoming", "claude"), strings.NewReader("unused")); err == nil {
		t.Fatal("overwrote existing conversation")
	}
	if _, err := s.CancelIncomingThreadTransfer(request.ID, transferTestSecret()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckThreadTransferActivation(request.ID, strings.Repeat("b", 64), transferTestSecret()); err == nil {
		t.Fatal("late activation after cancellation")
	}
	if err := s.CheckThreadTransferAccess("incoming"); err != nil {
		t.Fatal(err)
	}
}
