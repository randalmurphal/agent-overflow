package store

import (
	"database/sql"
	"slices"
	"sort"
	"testing"
)

// A v153 database with two standing agents carrying loops,
// watches, reminders and open requests opens through every pending
// migration with everything still attached to the same assistant.
func TestV155TwoAssistantsKeepEverything(t *testing.T) {
	path := fileDBThrough(t, agentRecordsMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived) VALUES
		('a', 'p', 'Ops', 'claude', '/p', '', 1, 1, 0),
		('b', 'p', 'Mail', 'codex', '/p', '', 1, 1, 0),
		('wa', 'p', 'Worker A', 'claude', '/p', '', 1, 1, 0),
		('wb', 'p', 'Worker B', 'codex', '/p', '', 1, 1, 0)`)
	mustExec(t, db, `INSERT INTO standing_agents (thread_id, home, notify_on_tick, promoted_at) VALUES
		('a', '/config/agents/a', 1, 10), ('b', '/config/agents/b', 0, 20)`)
	mustExec(t, db, `INSERT INTO agent_wakes (id, thread_id, kind, label, interval_seconds, next_at, state, created_at) VALUES
		('la', 'a', 'loop', 'board', 600, 100, 'armed', 1),
		('wwb', 'b', 'watch', 'inbox', 600, 100, 'armed', 1)`)
	mustExec(t, db, `UPDATE agent_wakes SET command = 'true' WHERE id = 'wwb'`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, state, notify, created_at, updated_at) VALUES
		('ra', 'a', 'remind', 'a', 'accepted', 1, 5, 5),
		('sa', 'a', 'spawn', 'wa', 'running', 1, 5, 5),
		('sb', 'b', 'spawn', 'wb', 'running', 1, 6, 6),
		('xb', 'b', 'send', 'wa', 'running', 1, 7, 7)`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := New(path)
	if err != nil {
		t.Fatalf("open through every migration: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for _, id := range []string{"a", "b"} {
		agent, found, err := s.GetStandingAgent(id)
		if err != nil || !found || agent.ID != id || agent.Home != "/config/agents/"+id {
			t.Fatalf("assistant on %s = %+v %v %v", id, agent, found, err)
		}
		thread, err := s.GetThread(id)
		if err != nil || thread.AgentID != id {
			t.Fatalf("thread %s agentId = %q %v", id, thread.AgentID, err)
		}
	}
	wakes, err := s.ListStandingAgentWakes()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range wakes {
		got = append(got, w.ID+"@"+w.ThreadID)
	}
	sort.Strings(got)
	if want := []string{"la@a", "wwb@b"}; !slices.Equal(got, want) {
		t.Fatalf("assistant wakes = %q, want %q", got, want)
	}
	for worker, want := range map[string]string{"wa": "a", "wb": "b"} {
		spawner, _, found, err := s.StandingAgentSpawnerOf(worker)
		if err != nil || !found || spawner != want {
			t.Fatalf("spawner of %s = %q %v %v, want %s", worker, spawner, found, err, want)
		}
	}
	// A handoff of a migrated assistant carries its loop and its reminder.
	if _, err := s.db.Exec(`INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived)
		VALUES ('a2', NULL, 'Ops', 'claude', '/config/agents/a', '', 2, 2, 0)`); err != nil {
		t.Fatal(err)
	}
	handOff, err := s.HandOffAssistant("a", "a2", 50)
	if err != nil {
		t.Fatal(err)
	}
	if handOff.Agent.ID != "a" || !slices.Equal(handOff.MovedWakeIDs, []string{"la"}) || handOff.MovedRequests != 2 {
		t.Fatalf("handoff = %+v", handOff)
	}
	var remindTarget string
	if err := s.db.QueryRow(`SELECT target_thread_id FROM thread_requests WHERE token = 'ra'`).Scan(&remindTarget); err != nil || remindTarget != "a2" {
		t.Fatalf("reminder target after handoff = %q %v", remindTarget, err)
	}
}

// Names differing only by case clash, and the refusal
// names the holder.
func TestAssistantNameCaseClash(t *testing.T) {
	path := fileDBThrough(t, agentRecordsMigrationVersion-1)
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	mustExec(t, s.db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, s.db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived) VALUES
		('a', NULL, 'Ops', 'claude', '/h/A', '', 1, 1, 0),
		('b', NULL, 'Mail', 'claude', '/h/B', '', 1, 1, 0),
		('c', NULL, '  OPS ', 'claude', '/h/C', '', 1, 1, 0)`)
	if err := s.CreateStandingAgent(StandingAgent{ID: "A", ThreadID: "a", Home: "/h/A", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateStandingAgent(StandingAgent{ID: "B", ThreadID: "b", Home: "/h/B", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	var taken *AssistantNameTakenError
	if err := s.CreateStandingAgent(StandingAgent{ID: "C", ThreadID: "c", Home: "/h/C", CreatedAt: 1}); err == nil {
		t.Fatal("a case-and-space variant of a taken name was accepted at creation")
	} else if !asNameTaken(err, &taken) || taken.Name != "Ops" {
		t.Fatalf("creation refusal = %v", err)
	}
	if err := s.UpdateTitle("b", "oPs"); err == nil {
		t.Fatal("rename to a case variant of a taken name was accepted")
	} else if !asNameTaken(err, &taken) || taken.Name != "Ops" {
		t.Fatalf("rename refusal = %v", err)
	}
	// A provider title never renames an assistant.
	if renamed, err := s.UpdateProviderTitle("a", "Provider title"); err != nil || renamed {
		t.Fatalf("provider title renamed an assistant: %v %v", renamed, err)
	}
	if ok, err := s.UpdateTitleIfCurrent("a", "Ops", "Generated"); err != nil || ok {
		t.Fatalf("generated title renamed an assistant: %v %v", ok, err)
	}
}

func asNameTaken(err error, out **AssistantNameTakenError) bool {
	t, ok := err.(*AssistantNameTakenError)
	if ok {
		*out = t
	}
	return ok
}

// A handoff moves the remote jobs still owed to the old
// thread (pending, or queued and never confirmed) and leaves a confirmed or
// dismissed one where it was. It moves the owner only: every watch still
// names the thread that launched its job.
func TestHandOffMovesOwedRemoteJobs(t *testing.T) {
	path := fileDBThrough(t, agentRecordsMigrationVersion-1)
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	mustExec(t, s.db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, s.db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived) VALUES
		('a', NULL, 'Ops', 'claude', '/h/A', '', 1, 1, 0), ('a2', NULL, 'Ops', 'claude', '/h/A', '', 2, 2, 0)`)
	if err := s.CreateStandingAgent(StandingAgent{ID: "A", ThreadID: "a", Home: "/h/A", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `INSERT INTO remote_watches (computer_id, request_id, thread_id, launched_in_thread_id, fingerprint, label, notification, created_at, confirmed_at) VALUES
		('c', 'pending', 'a', 'a', 'f', 'l', 'pending', 1, NULL),
		('c', 'unconfirmed', 'a', 'a', 'f', 'l', 'queued', 1, NULL),
		('c', 'confirmed', 'a', 'a', 'f', 'l', 'queued', 1, 5),
		('c', 'dismissed', 'a', 'a', 'f', 'l', 'dismissed', 1, NULL)`)
	if _, err := s.HandOffAssistant("a", "a2", 10); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"pending": "a2", "unconfirmed": "a2", "confirmed": "a", "dismissed": "a"}
	for request, thread := range want {
		w, err := s.GetRemoteWatch("c", request)
		if err != nil || w.ThreadID != thread {
			t.Errorf("watch %s on %q %v, want %s", request, w.ThreadID, err, thread)
		}
		// The destination's receipt keeps naming the thread that launched it.
		if w.LaunchedIn != "a" {
			t.Errorf("watch %s launched in %q, want a", request, w.LaunchedIn)
		}
	}
}
