package store

import "testing"

// A worker whose spawn row the retention sweep reduced to lineage still
// names its agent, though the ledger no longer knows the token.
func TestStandingAgentSpawnerOfReadsALineageOnlySpawn(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "agent")
	mustCreateThread(t, s, "worker")
	seedThreadRequest(t, s, "tok-worker", "agent", ThreadRequestSpawn, 100)
	if _, err := s.SetThreadRequestTarget("tok-worker", "", "worker", "", ""); err != nil {
		t.Fatal(err)
	}
	if applied, err := s.SettleThreadRequest("tok-worker", ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, SettledAt: 200,
	}); err != nil || !applied {
		t.Fatalf("settle: applied=%v err=%v", applied, err)
	}
	mustCreateAssistant(t, s, "agent", "/home/agent")
	if _, err := s.DeleteThreadRequestsBefore(1000); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.GetThreadRequest("tok-worker"); err != nil || found {
		t.Fatalf("the spawn row is still in the ledger: found=%v err=%v", found, err)
	}
	if !rawThreadRequest(t, s, "tok-worker").LineageOnly {
		t.Fatal("the spawn row was not kept as lineage")
	}
	if agentID, title, found, err := s.StandingAgentSpawnerOf("worker"); err != nil || !found || agentID != "agent" || title != "Thread agent" {
		t.Errorf("spawner of the worker = %q %q found=%v err=%v; want the agent", agentID, title, found, err)
	}
}
