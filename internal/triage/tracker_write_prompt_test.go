package triage

import (
	"encoding/json"
	"slices"
	"strconv"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// A tracker write's approval is the person's whatever the prompt router
// says of its thread: it is raised with no route, and a reconcile that
// routes the thread's other prompts again, the person's included, leaves
// it the person's. Another approval of the same worker goes to its
// spawner.
func TestATrackerWriteApprovalIsNeverRouted(t *testing.T) {
	t.Parallel()
	f := newPromptRouteFixture(t)
	f.route("worker", "lead")
	f.router.MarkThreadActive("worker", "s1")
	meta := mustMarshalJSON(t, provider.ApprovalRequest{RequestID: "tracker:call-1", ThreadID: "worker", Kind: ApprovalKindTrackerWrite, ToolName: "ticket_update"})
	if err := f.router.HandleSynthetic(provider.ProviderEvent{
		Kind: provider.EventApprovalRequest, ThreadID: "worker", ItemID: "tracker:call-1", Meta: meta, Timestamp: time.Now(), SessionToken: "s1",
	}); err != nil {
		t.Fatal(err)
	}
	f.approval(t, "worker", "bash", "s1")
	write, bash := f.open(t, "worker", "tracker:call-1"), f.open(t, "worker", "bash")
	// A claim says which kind of prompt it answers, so the answer reaches
	// the tracker write and not a provider.
	if claim, ok := f.router.ClaimPrompt("worker", "tracker:call-1", write.Occurrence); !ok || claim.Kind != ApprovalKindTrackerWrite {
		t.Fatalf("the tracker write's claim = %+v, %v", claim, ok)
	}
	f.router.ReleasePrompt(PromptClaim{Ref: write, SessionToken: "s1", Kind: ApprovalKindTrackerWrite})
	if claim, ok := f.router.ClaimPrompt("worker", "bash", bash.Occurrence); !ok || claim.Kind == ApprovalKindTrackerWrite {
		t.Fatalf("the provider's claim = %+v, %v", claim, ok)
	}
	f.router.ReleasePrompt(PromptClaim{Ref: bash, SessionToken: "s1"})
	f.wantRoute(t, write, true, "")
	f.wantRoute(t, bash, true, "lead")

	f.route("worker", "next")
	if moved := f.router.RouteThreadPrompts("worker", true, func(PromptRef) bool { return false }); moved != 1 {
		t.Fatalf("moved %d prompts, want the other approval alone", moved)
	}
	f.wantRoute(t, write, true, "")
	f.wantRoute(t, bash, true, "next")
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.announced["tracker:call-1"]; !slices.Equal(got, []string{""}) {
		t.Fatalf("the tracker approval was announced with routes %q, want the person's alone", got)
	}
}

// A tracker write's row keeps each ticket with what opens it and the
// steps applied, bounded, and the same call's row written again is the
// same row.
func TestATrackerWriteRowKeepsItsTickets(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	tickets := make([]map[string]any, maxTrackerWriteTickets+3)
	for i := range tickets {
		tickets[i] = map[string]any{"connection": "c1", "issue": "1000" + strconv.Itoa(i), "key": "WEB-" + strconv.Itoa(i), "title": "Fix", "applied": []string{"status", "comment"}}
	}
	row := func() {
		t.Helper()
		if err := router.HandleSynthetic(provider.ProviderEvent{
			Kind: provider.EventNotification, ThreadID: "t1", ItemID: "tracker:call-1", Content: "Changed 23 tickets",
			Meta: mustMarshalJSON(t, map[string]any{"kind": trackerWriteNotificationKind, "tickets": tickets}), Timestamp: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	row()
	row()
	rows := findItemsByKind(t, st, "t1", itemKindNotification)
	if len(rows) != 1 || rows[0].ID != "tracker:call-1" || rows[0].ToolName != trackerWriteNotificationKind || rows[0].ParentID != "" {
		t.Fatalf("rows = %+v", rows)
	}
	var meta struct {
		Tickets []struct {
			Connection, Issue, Key string
			Applied                []string
		}
	}
	if err := json.Unmarshal([]byte(rows[0].Meta), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Tickets) != maxTrackerWriteTickets || meta.Tickets[0].Key != "WEB-0" || meta.Tickets[0].Issue != "10000" || !slices.Equal(meta.Tickets[0].Applied, []string{"status", "comment"}) {
		t.Fatalf("meta = %+v", meta)
	}
}
