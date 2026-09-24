package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

func TestActivityRunScanGrowsChunksWithoutLosingMembers(t *testing.T) {
	const members = 1800 // crosses both the 512- and 1024-row scan chunks
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("close seed transaction: %v", err)
		}
	}()
	w := s.bulkItemWrites(tx, "t", false)
	for i := 0; i <= members+1; i++ {
		item := Item{
			ID: fmt.Sprintf("row-%d", i), ThreadID: "t", TurnIndex: 0,
			ItemIndex: i, Role: "assistant", Status: "completed",
			CreatedAt: int64(i + 1), Meta: "{}",
		}
		if i == 0 || i == members+1 {
			item.Kind = "assistant_text"
		} else {
			item.Kind, item.ToolName = "tool_call", "Bash"
		}
		if err := insertItemTx(tx, w, item, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	answer, err := s.ListActivityRunMembers(context.Background(), "t", ActivityRunMembersRequest{
		RunFirstItemID: "row-1", Direction: ActivityRunMembersBefore, Limit: 25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Stub.MemberCount != members || answer.Stub.UnshippedBefore != members-25 ||
		answer.Stub.LastItemID != fmt.Sprintf("row-%d", members) ||
		len(answer.Items) != 25 || answer.Items[0].ID != fmt.Sprintf("row-%d", members-24) {
		t.Fatalf("chunked run was truncated: stub=%+v shipped=%d", answer.Stub, len(answer.Items))
	}
}
