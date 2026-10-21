package store

import "testing"

// A thread row names the local thread that spawned it, carries its
// assistant's id while it is an assistant's current thread, and, as one,
// counts the threads it spawned that are now archived.
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
		if computer == "" {
			if _, err := s.SetThreadPlacement([]string{target}, PlacementNested); err != nil {
				t.Fatalf("nest %s: %v", target, err)
			}
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
	if got := row("agent"); got.AgentID != "" || archivedTotal(got) != 0 {
		t.Errorf("ordinary row = agent %q, archived children %d", got.AgentID, archivedTotal(got))
	}
	if row("child-1").SpawnedByAgent {
		t.Error("a thread spawned by an ordinary thread says it is agent-bound")
	}

	mustCreateAssistant(t, s, "agent", "/h/agent")
	if got := row("agent"); got.AgentID != "agent" || archivedTotal(got) != 0 {
		t.Errorf("assistant row = agent %q, archived children %d", got.AgentID, archivedTotal(got))
	}
	if !row("child-1").SpawnedByAgent || row("far-child").SpawnedByAgent {
		t.Error("spawnedByAgent follows whether the local spawner is an assistant's")
	}
	// The bond holds while the agent is archived: the group refusal does.
	if _, _, err := s.ArchiveThread("agent"); err != nil {
		t.Fatal(err)
	}
	if !row("child-2").SpawnedByAgent {
		t.Error("a thread spawned by an archived agent says it is not agent-bound")
	}
	if _, _, err := s.UnarchiveThread("agent"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ArchiveThread("child-1"); err != nil {
		t.Fatal(err)
	}
	if got := archivedTotal(row("agent")); got != 1 {
		t.Errorf("archived children after one archive = %d, want 1", got)
	}
	// Only a nested child counts in the agent's done footer: placed as a
	// plain row it leaves the count and the list, and nested again returns.
	if _, err := s.SetThreadPlacement([]string{"child-1"}, PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if got := archivedTotal(row("agent")); got != 0 {
		t.Errorf("archived children with the child standalone = %d, want 0", got)
	}
	if listed, err := s.ListArchivedSpawnedThreads("agent"); err != nil || len(listed) != 0 {
		t.Errorf("archived threads with the child standalone = %+v, %v; want none", listed, err)
	}
	if _, err := s.SetThreadPlacement([]string{"child-1"}, PlacementNested); err != nil {
		t.Fatal(err)
	}
	// A spawner that is not an agent never pays for the count.
	if _, _, err := s.ArchiveThread("far-child"); err != nil {
		t.Fatal(err)
	}
	if got := archivedTotal(row("far-spawner")); got != 0 {
		t.Errorf("plain spawner archived children = %d, want 0", got)
	}
	if listed, err := s.ListArchivedSpawnedThreads("agent"); err != nil || len(listed) != 1 || listed[0].ID != "child-1" {
		t.Errorf("archived threads spawned by the agent = %+v, %v; want child-1 alone", listed, err)
	}
	if listed, err := s.ListArchivedSpawnedThreads("far-spawner"); err != nil || len(listed) != 0 {
		t.Errorf("archived threads spawned on another computer = %+v, %v; want none", listed, err)
	}
	// Listing reads the same columns as a point read.
	listed, err := s.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	for _, thread := range listed {
		switch thread.ID {
		case "agent":
			if thread.AgentID != "agent" || archivedTotal(thread) != 1 {
				t.Errorf("listed agent row = %+v", thread)
			}
		case "child-2":
			if thread.SpawnedByThreadID != "agent" {
				t.Errorf("listed child spawnedBy = %q", thread.SpawnedByThreadID)
			}
		}
	}

	// Without its record (the assistant was moved away), its workers are
	// ordinary threads again.
	if _, err := s.db.Exec(`DELETE FROM agents WHERE thread_id = 'agent'`); err != nil {
		t.Fatal(err)
	}
	if got := row("agent"); got.AgentID != "" || archivedTotal(got) != 0 {
		t.Errorf("row without a record = agent %q, archived children %d", got.AgentID, archivedTotal(got))
	}
	if row("child-2").SpawnedByAgent {
		t.Error("a thread spawned by a thread that is no longer an assistant's still says it is agent-bound")
	}
}

// An assistant another assistant created is spawned by it but is a row of
// its own: archived, it is neither counted nor listed among its creator's
// finished work.
func TestACreatedAssistantIsNeverItsCreatorsChild(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "creator")
	mustCreateThread(t, s, "created")
	mustCreateThread(t, s, "worker")
	mustCreateAssistant(t, s, "creator", "/h/creator")
	mustCreateAssistant(t, s, "created", "/h/created")
	for token, target := range map[string]string{"tok-created": "created", "tok-worker": "worker"} {
		seedThreadRequest(t, s, token, "creator", ThreadRequestSpawn, 100)
		if _, err := s.SetThreadRequestTarget(token, "", target, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetThreadPlacement([]string{"worker"}, PlacementNested); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"created", "worker"} {
		if _, _, err := s.ArchiveThread(id); err != nil {
			t.Fatal(err)
		}
	}
	creator, err := s.GetThread("creator")
	if err != nil {
		t.Fatal(err)
	}
	if archivedTotal(creator) != 1 {
		t.Errorf("archived children = %d, want the worker alone", archivedTotal(creator))
	}
	if listed, err := s.ListArchivedSpawnedThreads("creator"); err != nil || len(listed) != 1 || listed[0].ID != "worker" {
		t.Errorf("archived threads spawned by the creator = %+v, %v; want the worker alone", listed, err)
	}
}
