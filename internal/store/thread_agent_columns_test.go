package store

import "testing"

// A thread row names the local thread that spawned it, carries its own
// promotion time once promoted, and, as a standing agent, counts the
// threads it spawned that are now archived.
func TestThreadRowsCarryAgentLineage(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"agent", "child-1", "child-2", "far-spawner", "far-child", "plain"} {
		mustCreateThread(t, s, id)
	}
	spawn := func(token, caller, computer, target string, at int64) {
		t.Helper()
		seedThreadRequest(t, s, token, caller, ThreadRequestSpawn, at)
		if _, err := s.SetThreadRequestTarget(token, computer, target, ""); err != nil {
			t.Fatalf("target %s: %v", token, err)
		}
	}
	spawn("tok-1", "agent", "", "child-1", 100)
	spawn("tok-2", "agent", "", "child-2", 200)
	// A spawn this computer accepted for another one is not local lineage.
	spawn("tok-far", "far-spawner", "laptop", "far-child", 300)
	// A send to a thread is not a spawn of it.
	seedThreadRequest(t, s, "tok-send", "plain", ThreadRequestSend, 400)
	if _, err := s.SetThreadRequestTarget("tok-send", "", "plain", ""); err != nil {
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
	if got := row("agent"); got.AgentPromotedAt != nil || got.ArchivedChildCount != 0 {
		t.Errorf("unpromoted agent row = promotedAt %v, archived children %d", got.AgentPromotedAt, got.ArchivedChildCount)
	}

	if _, err := s.PromoteStandingAgent("agent", "/h/agent", 500); err != nil {
		t.Fatal(err)
	}
	if got := row("agent"); got.AgentPromotedAt == nil || *got.AgentPromotedAt != 500 || got.ArchivedChildCount != 0 {
		t.Errorf("promoted agent row = promotedAt %v, archived children %d", got.AgentPromotedAt, got.ArchivedChildCount)
	}
	if _, _, err := s.ArchiveThread("child-1"); err != nil {
		t.Fatal(err)
	}
	if got := row("agent").ArchivedChildCount; got != 1 {
		t.Errorf("archived children after one archive = %d, want 1", got)
	}
	// A spawner that is not an agent never pays for the count.
	if _, _, err := s.ArchiveThread("far-child"); err != nil {
		t.Fatal(err)
	}
	if got := row("far-spawner").ArchivedChildCount; got != 0 {
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
			if thread.AgentPromotedAt == nil || thread.ArchivedChildCount != 1 {
				t.Errorf("listed agent row = %+v", thread)
			}
		case "child-2":
			if thread.SpawnedByThreadID != "agent" {
				t.Errorf("listed child spawnedBy = %q", thread.SpawnedByThreadID)
			}
		}
	}

	if _, err := s.DemoteStandingAgent("agent"); err != nil {
		t.Fatal(err)
	}
	if got := row("agent"); got.AgentPromotedAt != nil || got.ArchivedChildCount != 0 {
		t.Errorf("demoted agent row = promotedAt %v, archived children %d", got.AgentPromotedAt, got.ArchivedChildCount)
	}
}
