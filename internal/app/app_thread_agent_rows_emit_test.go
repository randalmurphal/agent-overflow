package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// Every request transition reaches the caller's pane as a frame with the row
// as it stands, and so does the target becoming blocked on a person.
func TestThreadAgentRowsChangesAreEmitted(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.mockClaudeHoldingTheTurn(t, "working")
	ack := f.runningRequest(t, "rerun the deploy")
	frames := func() int {
		f.bus.mu.Lock()
		defer f.bus.mu.Unlock()
		n := 0
		for _, event := range f.bus.all {
			if event.Name != "thread:agent_rows_changed" {
				continue
			}
			change, ok := event.Data.(ThreadAgentRowChange)
			if !ok {
				t.Fatalf("frame data %T", event.Data)
			}
			if change.ID == ack.Token {
				if change.ThreadID != f.caller.ID {
					t.Fatalf("frame addressed to %q; want the caller", change.ThreadID)
				}
				n++
			}
		}
		return n
	}
	// after waits for a frame naming the request past those seen before,
	// and for the row it names to read as want says.
	after := func(seen int, want func(ThreadAgentRow) bool) ThreadAgentRow {
		t.Helper()
		var row ThreadAgentRow
		waitUntil(t, 10*time.Second, func() bool {
			if frames() <= seen {
				return false
			}
			reads, err := f.app.GetThreadAgentRows(t.Context(), f.caller.ID, []string{ack.Token})
			if err != nil {
				t.Fatalf("GetThreadAgentRows: %v", err)
			}
			read := reads[0]
			if !read.Listed || read.Row == nil {
				return false
			}
			row = *read.Row
			return want(row)
		})
		return row
	}
	after(0, func(r ThreadAgentRow) bool { return r.State == threadtools.RequestRunning })

	approval, err := json.Marshal(provider.ApprovalRequest{RequestID: "approval-1", ToolName: "Bash", Description: "rm -rf build", Kind: "command"})
	if err != nil {
		t.Fatal(err)
	}
	seen := frames()
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventApprovalRequest, ThreadID: ack.ThreadID, ItemID: "approval-1", Meta: approval, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("approval request: %v", err)
	}
	blocked := after(seen, func(r ThreadAgentRow) bool { return r.State == threadtools.RequestBlocked })
	if blocked.BlockedOn != "approval" || !strings.Contains(blocked.BlockedDetail, "Bash") {
		t.Fatalf("blocked row = %+v; want what it is blocked on", blocked)
	}

	// A rename reaches the row through the target's thread:updated frame.
	seen = frames()
	if err := f.app.RenameThread(ack.ThreadID, "Deploy rerun"); err != nil {
		t.Fatalf("RenameThread: %v", err)
	}
	after(seen, func(r ThreadAgentRow) bool { return r.Title == "Deploy rerun" })

	seen = frames()
	if _, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token); err != nil {
		t.Fatalf("StopThreadAgentRow: %v", err)
	}
	after(seen, func(r ThreadAgentRow) bool { return r.State == threadtools.RequestCancelled })
}

// A row of a thread no request runs on costs nothing: the running check
// comes before the line is read.
//
// Serial: it measures the process's allocations.
func TestThreadAgentActivityIgnoresThreadsNoRequestRuns(t *testing.T) {
	a := &App{}
	item := store.Item{Kind: "tool_call", Summary: strings.Repeat("output ", 4096)}
	if allocs := testing.AllocsPerRun(20, func() { a.noteThreadAgentActivity("idle-thread", item) }); allocs != 0 {
		t.Fatalf("a row of a thread nothing runs on allocated %v times", allocs)
	}
}
