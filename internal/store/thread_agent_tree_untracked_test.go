package store

import (
	"slices"
	"testing"
)

// untrackRequests clears the run columns of tokens, as a build before runs
// were kept wrote them.
func untrackRequests(t *testing.T, s *Store, tokens ...string) {
	t.Helper()
	for _, token := range tokens {
		mustExec(t, s.db, `UPDATE thread_requests SET run_base_tool_calls = NULL, run_base_tokens = NULL, run_base_turn_ms = NULL,
			run_tool_calls = NULL, run_tokens = NULL, run_turn_ms = NULL, run_token = NULL WHERE token = ?`, token)
	}
}

// A worker whose spawn and sends were written before runs were kept is one
// open row of its spawner's however many of those requests are open: each
// open send of the spawner's to it falls in the spawn's row, which shows
// the oldest open request, the spawn while it is open. A send to a thread
// the caller did not spawn stays a row of its own, and each listing, by
// tree and by token, reads the same rows. No row is rewritten.
func TestAWorkerWhoseRequestsTrackNoRunIsOneOpenRow(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// spawnSettled is when the spawn settled, 0 for not yet: inside
		// the settled window or before it.
		spawnSettled int64
		want         []string
	}{
		{"its spawn open", 0, []string{"old-spawn@0", "other@0"}},
		{"its spawn settled in the window", 20_000, []string{"old-a@0", "other@0"}},
		{"its spawn settled before the window", 5_000, []string{"old-a@0", "other@0"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			for _, id := range []string{"lead", "w", "other-thread"} {
				mustCreateThread(t, s, id)
			}
			seedAgentRequest(t, s, "old-spawn", "lead", ThreadRequestSpawn, "w", false, 100)
			seedAgentRequest(t, s, "old-a", "lead", ThreadRequestSend, "w", true, 110)
			seedAgentRequest(t, s, "old-b", "lead", ThreadRequestSend, "w", true, 120)
			seedAgentRequest(t, s, "other", "lead", ThreadRequestSend, "other-thread", true, 130)
			untrackRequests(t, s, "old-spawn", "old-a", "old-b", "other")
			if c.spawnSettled != 0 {
				settleAgentRequest(t, s, "old-spawn", c.spawnSettled)
			}

			rows, _, err := s.ListThreadAgentTree(agentTreeRead("lead", 4))
			if err != nil {
				t.Fatal(err)
			}
			if got := agentTreeEntries(rows); !slices.Equal(got, c.want) {
				t.Fatalf("rows %v; want %v, one per worker", got, c.want)
			}
			for _, row := range rows {
				if row.Token != "other" && row.Node != "old-spawn" {
					t.Fatalf("row %s is at node %q; want the spawn's place", row.Token, row.Node)
				}
			}
			named := agentTreeRead("lead", 4)
			named.Tokens = []string{"old-spawn", "old-a", "old-b", "other"}
			if rows, _, err = s.ListThreadAgentTree(named); err != nil {
				t.Fatal(err)
			}
			if got := agentTreeEntries(rows); !slices.Equal(got, c.want) {
				t.Fatalf("named rows %v; want %v", got, c.want)
			}
			if row := rawThreadRequest(t, s, "old-a"); row.RunToken != "" || row.RunBase != nil {
				t.Fatalf("old-a %+v; the read rewrote its run", row)
			}
		})
	}
}
