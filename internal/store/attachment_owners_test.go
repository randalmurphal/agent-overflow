package store

import (
	"context"
	"strings"
	"testing"
)

// TestOwnsAttachmentReadsOnlyAttachmentBearingRows pins the inherited
// ownership check to the partial indexes over rows whose meta lists
// attachments. Without them a check on a fork of a long thread reads every
// row the fork inherits: 20 to 70 ms per check on an 81,000-row fork.
func TestOwnsAttachmentReadsOnlyAttachmentBearingRows(t *testing.T) {
	s := newTestStore(t)
	query, args := ownsAttachmentQuery("fork", "a")
	plan := explainPlan(t, s, query, args...)
	used := map[string]bool{"idx_items_attachment_refs": false, "idx_import_history_items_attachment_refs": false}
	for _, row := range plan {
		if !strings.HasPrefix(row.detail, "SEARCH items ") && !strings.HasPrefix(row.detail, "SCAN items") {
			continue
		}
		index := ""
		for name := range used {
			if strings.Contains(row.detail, "USING INDEX "+name+" ") {
				index = name
			}
		}
		if index == "" {
			t.Errorf("ownership check reads items without an attachment index: %s\n%s", row.detail, planText(plan))
			continue
		}
		used[index] = true
	}
	for name, ok := range used {
		if !ok {
			t.Errorf("ownership check does not use %s\n%s", name, planText(plan))
		}
	}
}

// TestForkAttachmentOwnersFollowCutAndRollback pins attachment access for
// pointer forks: a fork may read an attachment its source owns only while
// it shows a row that references it, a fork that takes its own copy of such
// a row takes ownership with it, and a failed fork leaves nothing behind.
func TestForkAttachmentOwnersFollowCutAndRollback(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"source", "foreign"} {
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
	if err := s.InsertItem(Item{ID: "first", ThreadID: "source", Kind: "user_text", Role: "user", Status: "completed", Meta: `{"attachments":[{"id":"early","threadId":"source"},{"id":"unrelated","threadId":"foreign"}]}`}); err != nil {
		t.Fatal(err)
	}
	// Imported history carries attachment references too, here in the
	// bare id shape.
	if err := s.ApplyImportBatch("source", ImportBatch{Rows: []ImportRow{{Item: Item{ID: "generated", TurnIndex: 1, Kind: "assistant_text", Role: "assistant", Status: "completed", Meta: `{"attachments":["late"]}`}}}}); err != nil {
		t.Fatal(err)
	}
	requireOwnership := func(stage, thread string, want map[string]bool) {
		t.Helper()
		for id, owned := range want {
			got, err := s.OwnsAttachment(thread, id)
			if err != nil || got != owned {
				t.Fatalf("%s: %s owns %s = %v err=%v, want %v", stage, thread, id, got, err, owned)
			}
		}
	}

	mustPointerFork(t, s, "source", "fork", throughTurn(0))
	requireOwnership("cut", "fork", map[string]bool{"early": true, "late": false, "unrelated": false})
	mustPointerFork(t, s, "source", "whole", ForkCut{})
	requireOwnership("whole", "whole", map[string]bool{"early": true, "late": true, "unrelated": false})

	// A row the fork stops showing takes its attachment access with it.
	if err := s.DeleteThreadItem("whole", "generated"); err != nil {
		t.Fatal(err)
	}
	requireOwnership("hidden", "whole", map[string]bool{"early": true, "late": false})

	// A copy owns what it shows, and keeps it once the source is gone.
	if err := s.MaterializeForkHistory(context.Background(), "fork"); err != nil {
		t.Fatal(err)
	}
	var direct int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM attachment_owners WHERE thread_id = 'fork'`).Scan(&direct); err != nil || direct != 1 {
		t.Fatalf("materialized fork owns %d attachments err=%v, want early", direct, err)
	}

	mustExec(t, s.db, `INSERT INTO turns(turn_id,thread_id,turn_index,started_at,completed_at) VALUES('source:1','source',1,1,2)`)
	mustExec(t, s.db, `CREATE TRIGGER fail_fork_turn BEFORE INSERT ON turns WHEN NEW.thread_id='failed' BEGIN SELECT RAISE(ABORT,'fail after linking'); END`)
	failed := makeThread("failed", "claude")
	if err := s.CreatePointerFork(failed, "source", ForkCut{}, testInterruptedSummary, 1); err == nil {
		t.Fatal("failed fork succeeded")
	}
	requireOwnership("failed fork", "failed", map[string]bool{"early": false, "late": false})
	requireOwnership("source after failed fork", "source", map[string]bool{"early": true, "late": true})

	if err := s.DeleteThread("source"); err != nil {
		t.Fatal(err)
	}
	requireOwnership("source deleted", "fork", map[string]bool{"early": true, "late": false})
	requireOwnership("source deleted", "whole", map[string]bool{"early": false, "late": false})
}
