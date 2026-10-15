package store

import (
	"strings"
	"testing"
)

// A promotion whose ungrouping fails leaves nothing behind: no agent row,
// and the agent and its spawned thread still in their group. A retry once
// the cause is gone promotes and ungroups in full.
func TestPromoteStandingAgentIsOneTransaction(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "agent")
	mustCreateThread(t, s, "child")
	seedThreadRequest(t, s, "tok-child", "agent", ThreadRequestSpawn, 100)
	if _, err := s.SetThreadRequestTarget("tok-child", "", "child", "", ""); err != nil {
		t.Fatal(err)
	}
	group, err := s.CreateThreadGroup(defaultTestProjectID, "Shared")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetThreadGroup([]string{"agent", "child"}, group.ID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `CREATE TRIGGER refuse_ungroup BEFORE UPDATE OF group_id ON threads
		BEGIN SELECT RAISE(ABORT, 'ungroup refused'); END`)

	if _, err := s.PromoteStandingAgent("agent", "/home/agent", 5); err == nil || !strings.Contains(err.Error(), "ungroup refused") {
		t.Fatalf("promote with the ungroup refused: %v", err)
	}
	if _, found, err := s.GetStandingAgent("agent"); err != nil || found {
		t.Errorf("the failed promotion left an agent row: found=%v err=%v", found, err)
	}
	for _, id := range []string{"agent", "child"} {
		if got, err := s.GetThread(id); err != nil || got.GroupID != group.ID {
			t.Errorf("%s after the failed promotion = group %q, %v; want %s", id, got.GroupID, err, group.ID)
		}
	}

	mustExec(t, s.db, `DROP TRIGGER refuse_ungroup`)
	promotion, err := s.PromoteStandingAgent("agent", "/home/agent", 6)
	if err != nil || !promotion.Promoted {
		t.Fatalf("retry = promoted %v, %v; want a fresh promotion", promotion.Promoted, err)
	}
	for _, id := range []string{"agent", "child"} {
		if got, err := s.GetThread(id); err != nil || got.GroupID != "" {
			t.Errorf("%s after the retry = group %q, %v; want ungrouped", id, got.GroupID, err)
		}
	}
}

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
	if _, err := s.PromoteStandingAgent("agent", "/home/agent", 300); err != nil {
		t.Fatal(err)
	}
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
