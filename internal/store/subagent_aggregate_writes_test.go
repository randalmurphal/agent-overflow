package store

import (
	"errors"
	"testing"
)

// TestSubagentAnchorIsValidated pins the store's check of a claimed
// write's anchor: it must be a local tool call on the row's parent chain,
// the parent itself or an ancestor. A rejected write changes
// nothing; an accepted one keeps its launches' stamps without a
// recompute.
func TestSubagentAnchorIsValidated(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-anchor"
	mustCreateThread(t, s, thread)
	for _, r := range []stampFixtureRow{
		{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: outer", turn: 1},
		{id: "Other", kind: "tool_call", tool: "Agent", summary: "Agent: other", turn: 1, index: 1},
		{id: "T", kind: "assistant_text", summary: "top level", turn: 1, index: 2},
	} {
		if err := s.InsertItem(r.item(thread)); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	claimed := func(r stampFixtureRow, anchor string) Item {
		item := r.item(thread)
		item.SubagentAnchor = anchor
		return item
	}
	for _, item := range []Item{
		claimed(stampFixtureRow{id: "N", kind: "tool_call", tool: "Agent", summary: "Agent: nested", parent: "L", turn: 1, index: 3}, "L"),
		claimed(stampFixtureRow{id: "L-bash", kind: "tool_call", tool: "Bash", summary: "Bash: ls", parent: "L", turn: 1, index: 4}, "L"),
		claimed(stampFixtureRow{id: "N-c1", kind: "tool_call", tool: "Read", summary: "Read: a", parent: "N", turn: 1, index: 5}, "N"),
	} {
		if err := s.InsertItem(item); err != nil {
			t.Fatalf("insert %s: %v", item.ID, err)
		}
	}
	gens := make(map[string]int64)
	for _, id := range []string{"L", "N"} {
		gen, mode := subagentStampStateForTest(t, s, thread, id)
		if mode != subagentStampClean {
			t.Fatalf("%s is mode %d, want clean", id, mode)
		}
		gens[id] = gen
	}

	for _, tc := range []struct {
		name string
		item Item
	}{
		{"no parent", claimed(stampFixtureRow{id: "x1", kind: "assistant_text", summary: "x", turn: 2}, "L")},
		{"missing anchor", claimed(stampFixtureRow{id: "x2", kind: "tool_call", tool: "Bash", summary: "Bash: x", parent: "N", turn: 2}, "gone")},
		{"anchor is not a tool call", claimed(stampFixtureRow{id: "x3", kind: "tool_call", tool: "Bash", summary: "Bash: x", parent: "T", turn: 2}, "T")},
		{"anchor is not on the chain", claimed(stampFixtureRow{id: "x4", kind: "tool_call", tool: "Bash", summary: "Bash: x", parent: "N", turn: 2}, "Other")},
		{"anchor below the parent", claimed(stampFixtureRow{id: "x5", kind: "tool_call", tool: "Bash", summary: "Bash: x", parent: "L", turn: 2}, "N")},
		{"parent is not in the thread", claimed(stampFixtureRow{id: "x6", kind: "tool_call", tool: "Bash", summary: "Bash: x", parent: "absent", turn: 2}, "L")},
	} {
		if err := s.InsertItem(tc.item); !errors.Is(err, ErrSubagentAnchor) {
			t.Errorf("insert, %s: got %v, want ErrSubagentAnchor", tc.name, err)
		}
		if _, found, err := s.GetThreadItem(thread, tc.item.ID); err != nil || found {
			t.Errorf("insert, %s: rejected row stored (found=%v, err=%v)", tc.name, found, err)
		}
	}

	summary := "Read: b"
	if _, err := s.UpdateItemFields(thread, "N-c1", ItemPartialUpdate{Summary: &summary, SubagentAnchor: "Other"}); !errors.Is(err, ErrSubagentAnchor) {
		t.Errorf("partial update with an anchor off the chain: got %v, want ErrSubagentAnchor", err)
	}
	row, _, err := s.GetThreadItem(thread, "N-c1")
	if err != nil {
		t.Fatal(err)
	}
	if row.Summary != "Read: a" {
		t.Errorf("rejected partial update stored summary %q", row.Summary)
	}
	row.Summary, row.SubagentAnchor = "Read: c", "T"
	if _, err := s.UpsertItem(row, nil); !errors.Is(err, ErrSubagentAnchor) {
		t.Errorf("whole-row update with a text row as anchor: got %v, want ErrSubagentAnchor", err)
	}

	// An ancestor above the parent is a valid anchor, including above a
	// parent that is not a subagent launch.
	for _, item := range []Item{
		claimed(stampFixtureRow{id: "N-c2", kind: "tool_call", tool: "Bash", summary: "Bash: deep", parent: "N", turn: 3, index: 1}, "L"),
		claimed(stampFixtureRow{id: "L-bash-out", kind: "tool_completion", tool: "Bash", summary: "done", parent: "L-bash", turn: 3, index: 2}, "L"),
	} {
		if err := s.InsertItem(item); err != nil {
			t.Fatalf("insert %s under ancestor anchor: %v", item.ID, err)
		}
	}
	for id, was := range gens {
		if gen, mode := subagentStampStateForTest(t, s, thread, id); mode != subagentStampClean || gen != was {
			t.Errorf("%s is mode %d at gen %d, want clean at gen %d (kept by the keyed writes)", id, mode, gen, was)
		}
	}
	assertSubagentStampParity(t, s, thread, "after the claimed writes", true)
}

// TestSubagentClaimUnderAnImportedParent: a claimed update of an imported
// row whose parent is an imported launch makes both local, as an insert
// under the launch does, so the claim finds its anchor on the local chain
// and the launch is stamped from both arms.
func TestSubagentClaimUnderAnImportedParent(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-imported-claim"
	newImportTargetThread(t, s, thread)
	imported := func(r stampFixtureRow) ImportRow { return ImportRow{Item: r.item(thread)} }
	if err := s.ApplyImportBatch(thread, ImportBatch{
		Turns: []Turn{{TurnID: thread + ":0", ThreadID: thread, TurnIndex: 0, StartedAt: 1_000}},
		Rows: []ImportRow{
			imported(stampFixtureRow{id: "imp-a", kind: "tool_call", tool: "Agent", summary: "Agent: a"}),
			imported(stampFixtureRow{id: "imp-a1", kind: "tool_call", tool: "Bash", summary: "Bash: a1", parent: "imp-a", index: 1}),
			imported(stampFixtureRow{id: "imp-a2", kind: "tool_call", tool: "Read", summary: "Read: a2", parent: "imp-a", index: 2}),
			imported(stampFixtureRow{id: "imp-b", kind: "tool_call", tool: "Agent", summary: "Agent: b", index: 3}),
			imported(stampFixtureRow{id: "imp-b1", kind: "tool_call", tool: "Bash", summary: "Bash: b1", parent: "imp-b", index: 4}),
		},
	}); err != nil {
		t.Fatalf("apply import batch: %v", err)
	}

	summary := "Bash: a1 edited"
	if _, err := s.UpdateItemFields(thread, "imp-a1", ItemPartialUpdate{Summary: &summary, SubagentAnchor: "imp-a"}); err != nil {
		t.Fatalf("claimed field update under an imported launch: %v", err)
	}
	row, found, err := s.GetThreadItem(thread, "imp-b1")
	if err != nil || !found {
		t.Fatalf("read imp-b1: found=%v err=%v", found, err)
	}
	row.Summary, row.SubagentAnchor = "Bash: b1 edited", "imp-b"
	if _, err := s.UpsertItem(row, nil); err != nil {
		t.Fatalf("claimed rewrite under an imported launch: %v", err)
	}
	for _, id := range []string{"imp-a", "imp-b"} {
		if _, mode := subagentStampStateForTest(t, s, thread, id); mode != subagentStampClean {
			t.Errorf("%s is mode %d, want stamped clean", id, mode)
		}
	}
	assertSubagentStampParity(t, s, thread, "after the claimed updates", true)
}
