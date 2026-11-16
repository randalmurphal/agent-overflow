package store

import (
	"fmt"
	"testing"
)

// A thread's own state is the one a spawn of it reads (TargetOpen): open
// work it made, or made at any depth below it, stored or held in memory,
// remote included, and nothing settled. Its rows are whatever its listing
// shows, open or settled inside the window.
func TestThreadAgentStateIsTheOpenStateASpawnOfItReads(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	chain := []string{"root", "a", "b", "c", "d", "e"}
	for _, id := range append([]string{"lead", "peer"}, chain...) {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "to-root", "lead", ThreadRequestSpawn, "root", false, 100)
	settleAgentRequest(t, s, "to-root", 200)
	for i, id := range chain[1:] {
		token := "to-" + id
		seedAgentRequest(t, s, token, chain[i], ThreadRequestSpawn, id, false, int64(110+i))
		settleAgentRequest(t, s, token, int64(210+i))
	}
	const window = 10_000
	check := func(what string, waiting, live []string, open, rows bool) {
		t.Helper()
		state, err := s.ReadThreadAgentState(ThreadAgentStateRead{Thread: "root", Waiting: waiting, Live: live, SettledSince: window})
		if err != nil {
			t.Fatal(err)
		}
		listed, _, err := s.ListThreadAgentTree(ThreadAgentTreeRead{
			Root: "lead", Waiting: waiting, Live: live, SettledSince: window, Depth: 4, Limit: 8, Tokens: []string{"to-root"},
		})
		if err != nil {
			t.Fatal(err)
		}
		target := len(listed) == 1 && listed[0].TargetOpen
		if state.OpenBelow != open || target != open || state.Rows != rows {
			t.Fatalf("%s: state %+v, the spawn's target open %v; want open %v, rows %v", what, state, target, open, rows)
		}
	}
	check("at rest", nil, nil, false, false)

	for depth, id := range chain {
		token := fmt.Sprintf("open-%d", depth)
		seedAgentRequest(t, s, token, id, ThreadRequestSend, "peer", true, int64(300+depth))
		check(fmt.Sprintf("a send owed an answer %d below", depth), nil, nil, true, true)
		settleAgentRequest(t, s, token, int64(400+depth))
		check(fmt.Sprintf("that send settled %d below", depth), nil, nil, false, false)
	}
	// The thread's own liveness is its computer's to read, not the ledger's.
	for depth, id := range chain[1:] {
		check(fmt.Sprintf("a live thread %d below", depth+1), nil, []string{id}, true, true)
	}
	seedAgentRequest(t, s, "parked", "e", ThreadRequestSend, "peer", false, 500)
	check("a quiet send no call waits on", nil, nil, false, false)
	check("a call parked on that send", []string{"parked"}, nil, true, true)
	settleAgentRequest(t, s, "parked", 510)

	seedAgentRequest(t, s, "far", "d", ThreadRequestSpawn, "", false, 600)
	mustExec(t, s.db, `UPDATE thread_requests SET target_computer_id = 'pc', target_thread_id = 'far-thread' WHERE token = 'far'`)
	settleAgentRequest(t, s, "far", 610)
	check("a remote spawn at rest", nil, nil, false, false)
	if _, err := s.SetThreadRequestNestedOpen("far", true); err != nil {
		t.Fatal(err)
	}
	check("a remote spawn with open work there", nil, nil, true, true)
	if _, err := s.SetThreadRequestNestedOpen("far", false); err != nil {
		t.Fatal(err)
	}

	seedAgentRequest(t, s, "recent", "root", ThreadRequestSpawn, "", false, 700)
	settleAgentRequest(t, s, "recent", window+1)
	check("a spawn settled inside the window", nil, nil, false, true)
}
