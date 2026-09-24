package store

import (
	"context"
	"strings"
	"testing"
)

// seedAnchorThread lays out every top-level row shape a page decorates
// from a subagent subtree it is not the parent of, all walked from the
// one launch "launch":
//
//	i0 launch      tool_call, the transcript root; "seed" is its child
//	i1 text        an ordinary row between the anchors
//	i2 carrier     tool_call resuming the launch (transcript_root_id)
//	i3 completion  tool_completion of the launch (completion_of)
//	i4 carrier-end tool_completion of the carrier
//	i5 seed        the launch's child
//	i6 nested      a launch under the launch; i7 grandchild is its child
func seedAnchorThread(t *testing.T, s *Store) {
	t.Helper()
	seedContractThread(t, s, "t")
	rows := []Item{
		func() Item {
			it := contractItem("t", "launch", 0)
			it.Kind, it.ToolName, it.Status = "tool_call", "Agent", "completed"
			return it
		}(),
		contractItem("t", "text", 1),
		func() Item {
			it := contractItem("t", "carrier", 2)
			it.Kind, it.ToolName, it.Status = "tool_call", "Agent", "completed"
			it.Meta = `{"transcript_root_id":"launch"}`
			return it
		}(),
		func() Item {
			it := contractItem("t", "completion", 3)
			it.Kind, it.ToolName, it.CompletionOf = "tool_completion", "Agent", "launch"
			return it
		}(),
		func() Item {
			it := contractItem("t", "carrier-end", 4)
			it.Kind, it.ToolName, it.CompletionOf = "tool_completion", "Agent", "carrier"
			return it
		}(),
		func() Item {
			it := contractItem("t", "seed", 5)
			it.ParentID = "launch"
			return it
		}(),
		// i6 nested: a launch the agent itself made, under "launch";
		// "grandchild" is its child, so a write there is two levels
		// below the top-level anchors.
		func() Item {
			it := contractItem("t", "nested", 6)
			it.Kind, it.ToolName, it.Status, it.ParentID = "tool_call", "Agent", "completed", "launch"
			return it
		}(),
		func() Item {
			it := contractItem("t", "grandchild", 7)
			it.ParentID = "nested"
			return it
		}(),
	}
	for _, row := range rows {
		if err := s.InsertItem(row); err != nil {
			t.Fatalf("insert %s: %v", row.ID, err)
		}
	}
}

// heldWindowOver describes a window of exactly the named top-level rows,
// at their current stamps, so a test can hold a window that EXCLUDES the
// launch the anchors inside it are walked from.
func heldWindowOver(t *testing.T, s *Store, ids ...string) HeldWindow {
	t.Helper()
	rows := make([]WindowDigestRow, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, WindowDigestRow{ID: id, Rev: itemRevisionOf(t, s, "t", id)})
	}
	return HeldWindow{
		OldestItemID: ids[0],
		NewestItemID: ids[len(ids)-1],
		Count:        len(ids),
		HasMoreOlder: true,
		Digest:       WindowDigest(rows),
	}
}

func syncStatusFor(t *testing.T, s *Store, held HeldWindow) SyncStatus {
	t.Helper()
	stale := historyStampOf(t, s, "t")
	stale.Rev--
	got, err := s.SyncThreadWindow(context.Background(), "t", "", 200, testRunWindowRows, stale, &held, TimelineSelection{})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return got.Status
}

// TestHeldWindowSeesThroughToAnchorsWalkedFromOutside is the hard half of
// TestHeldWindowSeesThroughToChildWrites. A resume carrier and a
// completion sibling render the launch's subtree, but the children are
// parented to the LAUNCH, so a stamp that only followed parent_id would
// leave them untouched. With the launch scrolled out of the window, that
// is a window the server would verify while the cards inside it are
// stale: a false `fresh`, the one failure the digest may never produce.
func TestHeldWindowSeesThroughToAnchorsWalkedFromOutside(t *testing.T) {
	cases := []struct {
		name  string
		write func(t *testing.T, s *Store)
	}{
		{
			name: "child inserted under the launch",
			write: func(t *testing.T, s *Store) {
				child := contractItem("t", "child", 9)
				child.ParentID = "launch"
				if err := s.InsertItem(child); err != nil {
					t.Fatalf("insert child: %v", err)
				}
			},
		},
		{
			name: "child updated under the launch",
			write: func(t *testing.T, s *Store) {
				if err := s.UpdateItemMeta("t", "seed", `{"changed":true}`); err != nil {
					t.Fatalf("update child: %v", err)
				}
			},
		},
		{
			name: "child deleted under the launch",
			write: func(t *testing.T, s *Store) {
				if err := s.DeleteThreadItem("t", "seed"); err != nil {
					t.Fatalf("delete child: %v", err)
				}
			},
		},
		{
			name: "grandchild inserted under a nested launch",
			write: func(t *testing.T, s *Store) {
				row := contractItem("t", "grandchild-2", 8)
				row.ParentID = "nested"
				if err := s.InsertItem(row); err != nil {
					t.Fatalf("insert grandchild: %v", err)
				}
			},
		},
		{
			name: "grandchild updated under a nested launch",
			write: func(t *testing.T, s *Store) {
				if err := s.UpdateItemMeta("t", "grandchild", `{"changed":true}`); err != nil {
					t.Fatalf("update grandchild: %v", err)
				}
			},
		},
		{
			name: "the launch row itself changed",
			write: func(t *testing.T, s *Store) {
				if err := s.UpdateItemMeta("t", "launch", `{"transcript_root_id":"elsewhere"}`); err != nil {
					t.Fatalf("update launch: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			seedAnchorThread(t, s)
			held := heldWindowOver(t, s, "text", "carrier", "completion", "carrier-end")
			if got := syncStatusFor(t, s, held); got != SyncFresh {
				t.Fatalf("clean window before the write: status = %q, want fresh", got)
			}
			before := historyStampOf(t, s, "t").Rev
			tc.write(t, s)
			if got := historyStampOf(t, s, "t").Rev; got != before+1 {
				t.Fatalf("history_rev advanced by %d for one write, want 1: an overlapping stamp re-fired the thread bump", got-before)
			}
			if got := syncStatusFor(t, s, held); got == SyncFresh {
				t.Fatal("verified a window whose anchors are walked from a launch outside it")
			}
		})
	}
}

// TestItemRevisionStampProbesIndexes pins the plan of the stamping UPDATE
// every item write runs. It is the hot path under a streaming delta, so a
// leg that fell off its index would be a per-chunk scan of the whole
// thread. EXPLAIN cannot see inside a trigger, so the statement is
// rebuilt with the trigger's NEW references as parameters.
func TestItemRevisionStampProbesIndexes(t *testing.T) {
	s := newTestStore(t)
	seedAnchorThread(t, s)
	params := strings.NewReplacer("NEW.thread_id", "?1", "NEW.id", "?2", "NEW.parent_id", "?3")
	query := stampRowsSQL + ` WHERE thread_id = ?1 AND id IN (` + params.Replace(stampedRowIDsSQL("NEW")) + `)`
	plan := explainPlan(t, s, query, "t", "child", "launch")
	text := planText(plan)
	for _, r := range plan {
		if strings.HasPrefix(r.detail, "SCAN items") {
			t.Fatalf("stamp plan scans items:\n%s", text)
		}
	}
	if !strings.Contains(text, "SEARCH items USING INDEX sqlite_autoindex_items_1") && !strings.Contains(text, "USING PRIMARY KEY") {
		t.Errorf("ancestor walk does not probe the primary key:\n%s", text)
	}
	for _, index := range []string{"idx_items_completion_of", carrierProbeByValue} {
		if !strings.Contains(text, index) {
			t.Errorf("stamp plan does not probe %s:\n%s", index, text)
		}
	}
}
