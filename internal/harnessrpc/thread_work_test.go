package harnessrpc

import (
	"encoding/json"
	"testing"
)

// TestHarnessThreadsWorkingLeavesOutThreadsAtRest: the receiver asks the
// host for each trimmed id, in order, and answers only the threads with
// something running or owed, each by id with what. The app's own answer
// is tested in internal/app (TestHarnessThreadWorkReadsWhatTheThreadWaitsOn).
func TestHarnessThreadsWorkingLeavesOutThreadsAtRest(t *testing.T) {
	h, host := newHarnessTestHost(t)
	host.threadWork = map[string]ThreadWork{
		"turn":   {ActiveTurn: true},
		"owed":   {PendingSends: true},
		"asked":  {WaitingOn: "approval"},
		"packed": {Compacting: true},
	}
	got, err := h.HarnessThreadsWorking([]string{" turn ", "idle", "owed", "asked", "packed"})
	if err != nil {
		t.Fatalf("HarnessThreadsWorking: %v", err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"threadId":"turn","activeTurn":true,"pendingSends":false,"compacting":false},` +
		`{"threadId":"owed","activeTurn":false,"pendingSends":true,"compacting":false},` +
		`{"threadId":"asked","activeTurn":false,"pendingSends":false,"waitingOn":"approval","compacting":false},` +
		`{"threadId":"packed","activeTurn":false,"pendingSends":false,"compacting":true}]`
	if string(encoded) != want {
		t.Fatalf("result = %s\nwant %s", encoded, want)
	}
	rest, err := h.HarnessThreadsWorking([]string{"idle"})
	if err != nil {
		t.Fatalf("HarnessThreadsWorking: %v", err)
	}
	if encoded, _ := json.Marshal(rest); string(encoded) != `[]` {
		t.Fatalf("threads at rest answered %s, want []", encoded)
	}
}

// TestHarnessThreadsWorkingRefusesNoThreadAndABlankOne: an empty question
// has no answer, and a blank id is a spec bug, not a thread at rest.
func TestHarnessThreadsWorkingRefusesNoThreadAndABlankOne(t *testing.T) {
	h, _ := newHarnessTestHost(t)
	if _, err := h.HarnessThreadsWorking(nil); err == nil {
		t.Fatal("an empty list was answered")
	}
	if _, err := h.HarnessThreadsWorking([]string{"turn", " "}); err == nil {
		t.Fatal("a blank id was answered")
	}
}
