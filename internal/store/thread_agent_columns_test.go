package store

import "testing"

// A thread row names the local thread that spawned it, and carries its
// assistant's id while it is an assistant's current thread.
func TestThreadRowsCarryAgentLineage(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"agent", "child-1", "child-2", "far-spawner", "far-child", "plain"} {
		mustCreateThread(t, s, id)
	}
	spawn := func(token, caller, computer, target string, at int64) {
		t.Helper()
		seedThreadRequest(t, s, token, caller, ThreadRequestSpawn, at)
		if _, err := s.SetThreadRequestTarget(token, computer, target, "", ""); err != nil {
			t.Fatalf("target %s: %v", token, err)
		}
	}
	spawn("tok-1", "agent", "", "child-1", 100)
	spawn("tok-2", "agent", "", "child-2", 200)
	// A spawn this computer accepted for another one is not local lineage.
	spawn("tok-far", "far-spawner", "laptop", "far-child", 300)
	// A send to a thread is not a spawn of it.
	seedThreadRequest(t, s, "tok-send", "plain", ThreadRequestSend, 400)
	if _, err := s.SetThreadRequestTarget("tok-send", "", "plain", "", ""); err != nil {
		t.Fatal(err)
	}

	row := func(id string) Thread {
		t.Helper()
		got, err := s.GetThread(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		return got
	}
	for id, want := range map[string]string{"child-1": "agent", "child-2": "agent", "far-child": "", "plain": "", "agent": ""} {
		if got := row(id).SpawnedByThreadID; got != want {
			t.Errorf("%s spawnedBy = %q, want %q", id, got, want)
		}
	}
	if got := row("agent"); got.AgentID != "" {
		t.Errorf("ordinary row = agent %q", got.AgentID)
	}

	mustCreateAssistant(t, s, "agent", "/h/agent")
	if got := row("agent"); got.AgentID != "agent" {
		t.Errorf("assistant row = agent %q", got.AgentID)
	}
	// Listing reads the same columns as a point read.
	listed, err := s.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	for _, thread := range listed {
		switch thread.ID {
		case "agent":
			if thread.AgentID != "agent" {
				t.Errorf("listed agent row = %+v", thread)
			}
		case "child-2":
			if thread.SpawnedByThreadID != "agent" {
				t.Errorf("listed child spawnedBy = %q", thread.SpawnedByThreadID)
			}
		}
	}

	// Without its record (the assistant was moved away), the row is an
	// ordinary thread's again; its workers still name it.
	if _, err := s.db.Exec(`DELETE FROM agents WHERE thread_id = 'agent'`); err != nil {
		t.Fatal(err)
	}
	if got := row("agent"); got.AgentID != "" {
		t.Errorf("row without a record = agent %q", got.AgentID)
	}
	if got := row("child-2").SpawnedByThreadID; got != "agent" {
		t.Errorf("child-2 spawnedBy = %q, want agent", got)
	}
}
