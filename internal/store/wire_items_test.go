package store

import (
	"sort"
	"strings"
	"testing"
)

func TestItemReadIsDecorated(t *testing.T) {
	cases := []struct {
		name string
		item Item
		want bool
	}{
		{"tool call", Item{Kind: "tool_call", ToolName: "Agent"}, true},
		{"plain tool call", Item{Kind: "tool_call", ToolName: "Bash"}, true},
		{"collab tool call", Item{Kind: "tool_call", ToolName: "collab_agent"}, false},
		{"completion sibling", Item{Kind: "tool_completion", ToolName: "Agent", CompletionOf: "launch"}, true},
		{"wait carrier completion", Item{Kind: "tool_completion", ToolName: "wait_agent", CompletionOf: "wait"}, true},
		{"completion without launch", Item{Kind: "tool_completion", ToolName: "Agent"}, false},
		{"proposed plan", Item{Kind: "assistant_text", Role: "assistant", PayloadKind: "proposed_plan"}, true},
		{"user text", Item{Kind: "user_text", Role: "user"}, false},
		{"assistant text", Item{Kind: "assistant_text", Role: "assistant"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ItemReadIsDecorated(tc.item); got != tc.want {
				t.Fatalf("ItemReadIsDecorated = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestItemReadNeedsDecoration pins the gate the emitter's hot path
// relies on: a childless tool call is left alone by the decorator, so
// its write read-back may go out as the page read; every other admitted
// row must be read.
func TestItemReadNeedsDecoration(t *testing.T) {
	s := newTestStore(t)
	seedAnchorThread(t, s)
	bash := contractItem("t", "bash", 20)
	bash.Kind, bash.ToolName = "tool_call", "Bash"
	if err := s.InsertItem(bash); err != nil {
		t.Fatalf("insert bash: %v", err)
	}
	rowOf := func(id string) Item {
		item, found, err := s.GetThreadItem("t", id)
		if err != nil || !found {
			t.Fatalf("read %s: found=%v err=%v", id, found, err)
		}
		return item
	}
	cases := []struct {
		id   string
		want bool
	}{
		{"bash", false},
		{"nested", true},
		{"launch", true},
		{"carrier", true},
		{"completion", true},
		{"text", false},
	}
	for _, tc := range cases {
		got, err := s.ItemReadNeedsDecoration(rowOf(tc.id))
		if err != nil {
			t.Fatalf("%s: %v", tc.id, err)
		}
		if got != tc.want {
			t.Errorf("%s: needs decoration = %v, want %v", tc.id, got, tc.want)
		}
	}
	// The decorator agrees: the childless call comes back byte-identical.
	rows, err := s.ListWireItems("t", []string{"bash"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("list bash: rows=%d err=%v", len(rows), err)
	}
	if rows[0] != rowOf("bash") {
		t.Fatalf("decorator altered a childless tool call:\n got %+v\nwant %+v", rows[0], rowOf("bash"))
	}
}

// TestListWireItemsIsThePageRead pins that the emitter's read is the
// page's read: the anchor comes back wearing its descendant aggregate,
// which the write's own read-back never carries, and a plain row comes
// back unchanged.
func TestListWireItemsIsThePageRead(t *testing.T) {
	s := newTestStore(t)
	seedAnchorThread(t, s)

	rows, err := s.ListWireItems("t", []string{"launch", "text", "missing"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := make(map[string]Item, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	if len(byID) != 2 {
		t.Fatalf("got %d rows, want launch and text only: %v", len(byID), rows)
	}
	launch := byID["launch"]
	if !strings.Contains(launch.Meta, `"subagentDescendantCount":3`) {
		t.Fatalf("launch read is not decorated: meta = %s", launch.Meta)
	}
	stored, _, err := s.GetThreadItem("t", "launch")
	if err != nil {
		t.Fatalf("get launch: %v", err)
	}
	if launch.Rev != stored.Rev {
		t.Fatalf("decorated launch rev = %d, stored %d", launch.Rev, stored.Rev)
	}
	plain, _, err := s.GetThreadItem("t", "text")
	if err != nil {
		t.Fatalf("get text: %v", err)
	}
	if got := byID["text"]; got != plain {
		t.Fatalf("plain row differs from GetThreadItem:\n got %+v\nwant %+v", got, plain)
	}
}

func idsOf(items []Item) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	sort.Strings(ids)
	return ids
}

func hasID(items []Item, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

// TestListWireItemsBehind pins the refresh set: the rows a write
// stamped without being written, the launch a completion settles, and a
// written row only when a sibling write moved it past its own push.
func TestListWireItemsBehind(t *testing.T) {
	t.Run("child write returns every anchor walked from its launch", func(t *testing.T) {
		s := newTestStore(t)
		seedAnchorThread(t, s)
		if err := s.UpdateItemMeta("t", "seed", `{"changed":true}`); err != nil {
			t.Fatalf("update child: %v", err)
		}
		rows, err := s.ListWireItemsBehind("t", map[string]int64{"seed": itemRevisionOf(t, s, "t", "seed")})
		if err != nil {
			t.Fatalf("behind: %v", err)
		}
		want := []string{"carrier", "carrier-end", "completion", "launch"}
		if got := idsOf(rows); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("behind = %v, want %v", got, want)
		}
		for _, row := range rows {
			if row.Rev != itemRevisionOf(t, s, "t", row.ID) {
				t.Errorf("%s pushed at rev %d, stored %d", row.ID, row.Rev, itemRevisionOf(t, s, "t", row.ID))
			}
			if row.ID == "launch" && !strings.Contains(row.Meta, `"subagentDescendantCount":3`) {
				t.Errorf("launch refresh is not the page read: meta = %s", row.Meta)
			}
		}
	})

	t.Run("a grandchild write returns the outer anchors too", func(t *testing.T) {
		s := newTestStore(t)
		seedAnchorThread(t, s)
		if err := s.UpdateItemMeta("t", "grandchild", `{"changed":true}`); err != nil {
			t.Fatalf("update grandchild: %v", err)
		}
		rows, err := s.ListWireItemsBehind("t", map[string]int64{"grandchild": itemRevisionOf(t, s, "t", "grandchild")})
		if err != nil {
			t.Fatalf("behind: %v", err)
		}
		want := []string{"carrier", "carrier-end", "completion", "launch", "nested"}
		if got := idsOf(rows); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("behind = %v, want %v", got, want)
		}
		for _, row := range rows {
			if row.ID == "launch" && !strings.Contains(row.Meta, `"subagentDescendantCount":3`) {
				t.Errorf("outer launch refresh does not carry the transitive count: meta = %s", row.Meta)
			}
		}
	})

	t.Run("a written row is behind only when a sibling write moved it", func(t *testing.T) {
		s := newTestStore(t)
		seedAnchorThread(t, s)
		pushed := itemRevisionOf(t, s, "t", "completion")
		rows, err := s.ListWireItemsBehind("t", map[string]int64{"completion": pushed})
		if err != nil {
			t.Fatalf("behind: %v", err)
		}
		if hasID(rows, "completion") {
			t.Fatalf("completion at its pushed rev came back: %v", idsOf(rows))
		}
		// The launch write stamps its completion sibling.
		if err := s.UpdateItemMeta("t", "launch", `{"settled":true}`); err != nil {
			t.Fatalf("update launch: %v", err)
		}
		rows, err = s.ListWireItemsBehind("t", map[string]int64{"completion": pushed})
		if err != nil {
			t.Fatalf("behind: %v", err)
		}
		if !hasID(rows, "completion") {
			t.Fatalf("completion moved past its push but was not returned: %v", idsOf(rows))
		}
	})

	t.Run("a completion sibling refreshes the launch it settles", func(t *testing.T) {
		s := newTestStore(t)
		seedAnchorThread(t, s)
		rows, err := s.ListWireItemsBehind("t", map[string]int64{"carrier-end": itemRevisionOf(t, s, "t", "carrier-end")})
		if err != nil {
			t.Fatalf("behind: %v", err)
		}
		if !hasID(rows, "carrier") {
			t.Fatalf("launch of the completion not returned: %v", idsOf(rows))
		}
	})

	t.Run("a deleted written row and an empty set contribute nothing", func(t *testing.T) {
		s := newTestStore(t)
		seedAnchorThread(t, s)
		if err := s.DeleteThreadItem("t", "seed"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		rows, err := s.ListWireItemsBehind("t", map[string]int64{"seed": 1})
		if err != nil {
			t.Fatalf("behind: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("deleted row produced %v", idsOf(rows))
		}
		rows, err = s.ListWireItemsBehind("t", nil)
		if err != nil || len(rows) != 0 {
			t.Fatalf("empty set: rows=%v err=%v", rows, err)
		}
	})
}

// TestStampedRowIDsByParamsProbesIndexes pins the refresh's candidate
// select to the same index-only plan as the trigger it mirrors.
func TestStampedRowIDsByParamsProbesIndexes(t *testing.T) {
	s := newTestStore(t)
	seedAnchorThread(t, s)
	plan := explainPlan(t, s, stampedRowIDsByParamsSQL, "t", "child", "launch")
	for _, r := range plan {
		if strings.HasPrefix(r.detail, "SCAN items") {
			t.Fatalf("candidate plan scans items:\n%s", planText(plan))
		}
	}
}
