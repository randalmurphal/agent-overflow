package triage

import (
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func TestInterruptPreservesConcurrentToolSettlement(t *testing.T) {
	for _, settlement := range []string{"approval", "approved", "completion", "background"} {
		t.Run(settlement, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")
			if err := st.InsertItem(store.Item{ID: "tool", ThreadID: "t1", TurnIndex: 0, Kind: "tool_call", ToolName: "Bash", Role: "assistant", Status: "running", Summary: "command"}); err != nil {
				t.Fatal(err)
			}
			interleaved := false
			err := router.flipTurnItemsErrored("t1", 0, 100, func(summary string) string {
				if !interleaved {
					interleaved = true
					if settlement == "approval" || settlement == "approved" {
						decision := "lost"
						if settlement == "approved" {
							decision = "approved"
						}
						if err := router.applyApprovalDecision("t1", "tool", provider.ApprovalRequest{ToolName: "Bash"}, decision, 101); err != nil {
							t.Fatal(err)
						}
					} else {
						row, found, err := st.GetThreadItem("t1", "tool")
						if err != nil || !found {
							t.Fatalf("read tool: %v", err)
						}
						if settlement == "completion" {
							row.Status = "completed"
							row.Summary = "finished"
						} else {
							row.IsBackground = true
						}
						if _, err := st.UpsertItemWithInputPayload(row, nil, nil); err != nil {
							t.Fatal(err)
						}
					}
				}
				return stoppedSummary(summary)
			})
			if err != nil {
				t.Fatal(err)
			}
			row, found, err := st.GetThreadItem("t1", "tool")
			if err != nil || !found {
				t.Fatalf("read result: %v", err)
			}
			switch settlement {
			case "approval":
				if row.Status != "errored" || row.Decision != "lost" {
					t.Fatalf("approval overwritten: %+v", row)
				}
			case "approved":
				if row.Status != "errored" || row.Decision != "approved" || row.Summary != stoppedSummary("command") {
					t.Fatalf("approval retry lost fields: %+v", row)
				}
			case "completion":
				if row.Status != "completed" || row.Summary != "finished" {
					t.Fatalf("completion overwritten: %+v", row)
				}
			case "background":
				if row.Status != "running" || !row.IsBackground {
					t.Fatalf("background launch overwritten: %+v", row)
				}
			}
		})
	}
}

func TestApprovalPreservesAnInterruptAfterItsInitialRead(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := st.InsertItem(store.Item{ID: "tool", ThreadID: "t1", TurnIndex: 0, Kind: "tool_call", ToolName: "Bash", Role: "assistant", Status: "running", Summary: "command"}); err != nil {
		t.Fatal(err)
	}
	observed, found, err := st.GetThreadItem("t1", "tool")
	if err != nil || !found {
		t.Fatalf("read approval candidate: %v", err)
	}
	if err := router.flipTurnItemsErrored("t1", 0, 100, stoppedSummary); err != nil {
		t.Fatal(err)
	}
	if err := router.updateApprovalItem(observed, provider.ApprovalRequest{ToolName: "Bash"}, "approved", 101); err != nil {
		t.Fatal(err)
	}
	row, found, err := st.GetThreadItem("t1", "tool")
	if err != nil || !found {
		t.Fatalf("read result: %v", err)
	}
	if row.Status != "errored" || row.Summary != stoppedSummary("command") || row.Decision != "approved" {
		t.Fatalf("approval overwrote interrupt: %+v", row)
	}
}
