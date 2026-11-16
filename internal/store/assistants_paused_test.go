package store

import (
	"slices"
	"testing"
)

// While assistants are paused, nothing scheduled for a thread under an
// assistant (its current thread or a helper) is due, and the
// sweep's next moment does not count it; an ordinary thread's loops and
// reminders are due as before, and a worker of the assistant asks the user. The rows are untouched, so unpausing makes
// them due again with the schedule they had.
func TestPausedAssistantsHaveNothingDue(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	homeThread(t, s, "ops", "")
	mustRetitle(t, s, "ops", "Ops")
	agent := StandingAgent{ID: "a-ops", ThreadID: "ops", Home: "/h/ops", CreatedAt: 1}
	if err := s.CreateStandingAgent(agent, defaultTestProjectID); err != nil {
		t.Fatal(err)
	}
	homeThread(t, s, "helper", agent.ID)
	mustCreateThread(t, s, "plain")
	mustCreateThread(t, s, "worker")
	seedAgentRequest(t, s, "ops-worker", "ops", ThreadRequestSpawn, "worker", false, 100)
	for _, row := range []AgentWake{
		{ID: "w-ops", ThreadID: "ops", Kind: AgentWakeLoop, Label: "board", IntervalSeconds: 60, NextAt: 100, CreatedAt: 1},
		{ID: "w-watch", ThreadID: "ops", Kind: AgentWakeWatch, Label: "tail", Command: "true", LongRunning: true, CreatedAt: 2},
		{ID: "w-helper", ThreadID: "helper", Kind: AgentWakeLoop, Label: "board", IntervalSeconds: 60, NextAt: 50, CreatedAt: 3},
		{ID: "w-plain", ThreadID: "plain", Kind: AgentWakeLoop, Label: "board", IntervalSeconds: 60, NextAt: 200, CreatedAt: 4},
	} {
		if _, err := s.RegisterAgentWake(row); err != nil {
			t.Fatal(err)
		}
	}
	for _, reminder := range []ThreadRequest{
		{Token: "r-ops", CallerThreadID: "ops", Kind: ThreadRequestRemind, DueAt: 10, State: ThreadRequestAccepted, CreatedAt: 1, UpdatedAt: 1},
		{Token: "r-plain", CallerThreadID: "plain", Kind: ThreadRequestRemind, DueAt: 300, State: ThreadRequestAccepted, CreatedAt: 1, UpdatedAt: 1},
	} {
		if err := s.InsertThreadRequest(reminder); err != nil {
			t.Fatal(err)
		}
	}

	check := func(paused bool, wakes, live, reminders []string, nextWake, nextReminder int64) {
		t.Helper()
		s.PauseAssistants(paused)
		due, err := s.DueAgentWakes(1000, 10)
		if err != nil {
			t.Fatal(err)
		}
		if got := wakeIDs(due); !slices.Equal(got, wakes) {
			t.Errorf("paused=%v DueAgentWakes = %v, want %v", paused, got, wakes)
		}
		all, err := s.ListLiveAgentWakes()
		if err != nil {
			t.Fatal(err)
		}
		if got := wakeIDs(all); !slices.Equal(got, live) {
			t.Errorf("paused=%v ListLiveAgentWakes = %v, want %v", paused, got, live)
		}
		next, err := s.NextAgentWakeDue()
		if err != nil {
			t.Fatal(err)
		}
		if !next.Set || next.At != nextWake {
			t.Errorf("paused=%v NextAgentWakeDue = %+v, want %d", paused, next, nextWake)
		}
		dueReminders, err := s.DueThreadReminders(1000, 10)
		if err != nil {
			t.Fatal(err)
		}
		var tokens []string
		for _, row := range dueReminders {
			tokens = append(tokens, row.Token)
		}
		if !slices.Equal(tokens, reminders) {
			t.Errorf("paused=%v DueThreadReminders = %v, want %v", paused, tokens, reminders)
		}
		// A worker's questions go to its spawner only while that one runs.
		spawner, _, found, err := s.SpawnerOf("worker")
		if err != nil || found == paused || (found && spawner != "ops") {
			t.Errorf("paused=%v SpawnerOf(worker) = %q found=%v err=%v", paused, spawner, found, err)
		}
		schedule, err := s.NextThreadRequestWork()
		if err != nil {
			t.Fatal(err)
		}
		if !schedule.Reminder.Set || schedule.Reminder.At != nextReminder {
			t.Errorf("paused=%v next reminder = %+v, want %d", paused, schedule.Reminder, nextReminder)
		}
	}
	check(false, []string{"w-helper", "w-ops", "w-plain"}, []string{"w-ops", "w-watch", "w-helper", "w-plain"}, []string{"r-ops", "r-plain"}, 50, 10)
	check(true, []string{"w-plain"}, []string{"w-plain"}, []string{"r-plain"}, 200, 300)

	resumed, err := s.ListLiveAssistantAgentWakes()
	if err != nil {
		t.Fatal(err)
	}
	if got := wakeIDs(resumed); !slices.Equal(got, []string{"w-ops", "w-watch", "w-helper"}) {
		t.Errorf("ListLiveAssistantAgentWakes = %v", got)
	}
	check(false, []string{"w-helper", "w-ops", "w-plain"}, []string{"w-ops", "w-watch", "w-helper", "w-plain"}, []string{"r-ops", "r-plain"}, 50, 10)

	threads, err := s.ListThreadsUnderAssistants()
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(threads)
	if !slices.Equal(threads, []string{"helper", "ops"}) {
		t.Errorf("ListThreadsUnderAssistants = %v", threads)
	}
	for id, want := range map[string]bool{"ops": true, "helper": true, "plain": false} {
		under, err := s.ThreadUnderAssistant(id)
		if err != nil || under != want {
			t.Errorf("ThreadUnderAssistant(%s) = %v, %v, want %v", id, under, err, want)
		}
	}
}

// While assistants are paused, a thread under an assistant is no message
// search hit, by title or by message; an ordinary thread still is.
func TestPausedAssistantsAreNoMessageSearchHits(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	homeThread(t, s, "ops", "")
	mustRetitle(t, s, "ops", "quokka board")
	if err := s.CreateStandingAgent(StandingAgent{ID: "a-ops", ThreadID: "ops", Home: "/h/ops", CreatedAt: 1}, defaultTestProjectID); err != nil {
		t.Fatal(err)
	}
	homeThread(t, s, "helper", "a-ops")
	mustCreateThread(t, s, "plain")
	mustRetitle(t, s, "plain", "quokka notes")
	mustInsertItemForSearch(t, s, "ops", "ops-item", 0, "quokka in ops", 1)
	mustInsertItemForSearch(t, s, "helper", "helper-item", 0, "quokka in helper", 2)
	mustInsertItemForSearch(t, s, "plain", "plain-item", 0, "quokka in plain", 3)

	threads := func(paused bool) []string {
		t.Helper()
		s.PauseAssistants(paused)
		hits, err := s.SearchThreadMessages("quokka", 10)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, hit := range hits {
			ids = append(ids, hit.MatchType+":"+hit.ThreadID)
		}
		slices.Sort(ids)
		return ids
	}
	if got, want := threads(true), []string{"item:plain", "title:plain"}; !slices.Equal(got, want) {
		t.Errorf("paused hits = %v, want %v", got, want)
	}
	if got, want := threads(false), []string{"item:helper", "item:ops", "item:plain", "title:ops", "title:plain"}; !slices.Equal(got, want) {
		t.Errorf("running hits = %v, want %v", got, want)
	}
}

func wakeIDs(rows []AgentWake) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}
