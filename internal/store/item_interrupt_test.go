package store

import "testing"

func TestErrorActiveItemIfRevisionGuardsLifecycleAndPreservesFields(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "thread")
	for index, state := range []string{"running", "streaming", "completed", "declined", "errored", "background"} {
		t.Run(state, func(t *testing.T) {
			status := state
			if state == "background" {
				status = "running"
			}
			row := Item{ID: state, ThreadID: "thread", TurnIndex: 0, ItemIndex: index, Kind: "tool_call", Role: "assistant", ToolName: "Bash", Status: status, Summary: "command", Decision: "approved", IsBackground: state == "background", Meta: `{"marker":"preserved"}`}
			if err := s.InsertItem(row); err != nil {
				t.Fatal(err)
			}
			before, found, err := s.GetThreadItem("thread", state)
			if err != nil || !found {
				t.Fatalf("read: %v", err)
			}
			if _, changed, err := s.ErrorActiveItemIfRevision("thread", state, before.Rev-1, "stale", 20, nil); err != nil || changed {
				t.Fatalf("stale write: changed=%v err=%v", changed, err)
			}
			got, changed, err := s.ErrorActiveItemIfRevision("thread", state, before.Rev, "stopped", 21, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantChanged := state == "running" || state == "streaming"
			if changed != wantChanged {
				t.Fatalf("changed=%v want %v", changed, wantChanged)
			}
			after, _, err := s.GetThreadItem("thread", state)
			if err != nil {
				t.Fatal(err)
			}
			if wantChanged {
				if got.Status != "errored" || got.Summary != "stopped" || got.Rev <= before.Rev || got.Rev != after.Rev {
					t.Fatalf("settled snapshot: %+v", got)
				}
				if _, changed, err := s.ErrorActiveItemIfRevision("thread", state, got.Rev, "twice", 22, nil); err != nil || changed {
					t.Fatalf("repeat write: changed=%v err=%v", changed, err)
				}
			} else if after.Rev != before.Rev || after.Status != before.Status || after.Summary != before.Summary {
				t.Fatalf("settled/background row changed: %+v", after)
			}
			if after.Decision != before.Decision || after.Meta != before.Meta || after.IsBackground != before.IsBackground {
				t.Fatalf("unrelated fields changed: %+v", after)
			}
		})
	}
}
