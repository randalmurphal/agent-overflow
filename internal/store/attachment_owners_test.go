package store

import (
	"context"
	"testing"
)

func TestForkAttachmentOwnersFollowCutAndRollback(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"source", "fork", "foreign", "failed"} {
		if err := s.CreateThread(makeThread(id, "claude")); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []Attachment{
		{ID: "early", ThreadID: "source"}, {ID: "late", ThreadID: "source"}, {ID: "unrelated", ThreadID: "foreign"},
	} {
		a.Kind = AttachmentKindImage
		a.Filename = "image.png"
		a.MimeType = "image/png"
		a.RelativePath = a.ThreadID + "/" + a.ID + ".png"
		if err := s.InsertAttachment(a); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []Item{
		{ID: "first", Kind: "user_text", Role: "user", Meta: `{"attachments":[{"id":"early","threadId":"source"},{"id":"unrelated","threadId":"foreign"}]}`},
		{ID: "generated", TurnIndex: 1, Kind: "assistant_text", Role: "assistant", Meta: `{"attachments":[{"id":"late","threadId":"source"}]}`},
	} {
		item.ThreadID = "source"
		item.Status = "completed"
		if err := s.InsertItem(item); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PrepareThreadHistory(context.Background(), "source"); err != nil {
		t.Fatal(err)
	}
	cut := 0
	if _, err := s.CloneThreadHistoryThroughTurn("source", "fork", &cut); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"early", "late", "unrelated"} {
		owned, err := s.OwnsAttachment("fork", id)
		if err != nil || owned != (id == "early") {
			t.Fatalf("cut ownership %s=%v err=%v", id, owned, err)
		}
	}
	mustExec(t, s.db, `INSERT INTO turns(turn_id,thread_id,turn_index,started_at,completed_at) VALUES('source:0','source',0,1,2)`)
	mustExec(t, s.db, `CREATE TRIGGER fail_fork_turn BEFORE INSERT ON turns WHEN NEW.thread_id='failed' BEGIN SELECT RAISE(ABORT,'fail after owner clone'); END`)
	if _, err := s.CloneThreadHistoryThroughTurn("source", "failed", nil); err == nil {
		t.Fatal("failed clone succeeded")
	}
	for _, id := range []string{"early", "late"} {
		owned, err := s.OwnsAttachment("failed", id)
		if err != nil || owned {
			t.Fatalf("failed fork retained %s=%v err=%v", id, owned, err)
		}
		owned, err = s.OwnsAttachment("source", id)
		if err != nil || !owned {
			t.Fatalf("failed fork changed source %s=%v err=%v", id, owned, err)
		}
	}
}
