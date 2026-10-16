package store

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"
)

// mustCreateAssistant records an assistant on an existing thread, its id the
// thread's as v154 migrates a promoted thread.
func mustCreateAssistant(t *testing.T, s *Store, threadID, home string) StandingAgent {
	t.Helper()
	agent := StandingAgent{ID: threadID, ThreadID: threadID, Home: home, CreatedAt: 1}
	if err := s.CreateStandingAgent(agent); err != nil {
		t.Fatalf("create assistant on %s: %v", threadID, err)
	}
	return agent
}

func mustRetitle(t *testing.T, s *Store, threadID, title string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE threads SET title = ? WHERE id = ?`, title, threadID); err != nil {
		t.Fatal(err)
	}
}

// Names are unique among this computer's assistants, ignoring case and
// surrounding space; the refusal names the holder, and a thread is one
// assistant's at most, current or past.
func TestAssistantNamesAreUniquePerComputer(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"ops", "clash", "blank", "other"} {
		mustCreateThread(t, s, id)
	}
	mustRetitle(t, s, "ops", "Ops")
	mustRetitle(t, s, "clash", " oPS ")
	mustRetitle(t, s, "blank", "  ")
	mustCreateAssistant(t, s, "ops", "/h/ops")

	var taken *AssistantNameTakenError
	if err := s.CreateStandingAgent(StandingAgent{ID: "a2", ThreadID: "clash", Home: "/h/a2"}); !errors.As(err, &taken) || taken.ThreadID != "ops" || taken.Name != "Ops" {
		t.Fatalf("a clashing name = %v, want the holder named", err)
	}
	if err := s.CheckAssistantName("OPS"); !errors.As(err, &taken) {
		t.Fatalf("CheckAssistantName(OPS) = %v", err)
	}
	if err := s.CreateStandingAgent(StandingAgent{ID: "a3", ThreadID: "blank", Home: "/h/a3"}); !errors.Is(err, ErrAssistantNameEmpty) {
		t.Fatalf("a blank name = %v", err)
	}
	if err := s.CreateStandingAgent(StandingAgent{ID: "a4", ThreadID: "ops", Home: "/h/a4"}); err == nil {
		t.Fatal("a second record on one thread was accepted")
	}

	// A rename to a name in use is refused; renaming a non-assistant
	// thread to it is not, and an assistant keeps its own name.
	if err := s.UpdateTitle("other", "ops"); err != nil {
		t.Fatalf("an ordinary thread may share an assistant's name: %v", err)
	}
	mustCreateThread(t, s, "second")
	mustRetitle(t, s, "second", "Second")
	mustCreateAssistant(t, s, "second", "/h/second")
	if err := s.UpdateTitle("second", "OPS"); !errors.As(err, &taken) || taken.ThreadID != "ops" {
		t.Fatalf("renaming an assistant to a name in use = %v", err)
	}
	if err := s.UpdateTitle("ops", "ops"); err != nil {
		t.Fatalf("an assistant renamed to its own name in another case: %v", err)
	}
	// A provider's title never renames an assistant.
	if renamed, err := s.UpdateProviderTitle("second", "Generated"); err != nil || renamed {
		t.Fatalf("provider title on an assistant = %v, %v", renamed, err)
	}
	if renamed, err := s.UpdateProviderTitle("other", "Generated"); err != nil || !renamed {
		t.Fatalf("provider title on an ordinary thread = %v, %v", renamed, err)
	}
}

// A handoff moves the record, its live loops and watches and every request
// the old thread made, keeps the maker, archives the old thread and records
// it as past; a second handoff onto a taken thread is refused whole.
func TestHandOffAssistantMovesEverythingAtOnce(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"old", "new", "worker", "far", "taken"} {
		mustCreateThread(t, s, id)
	}
	mustCreateAssistant(t, s, "old", "/h/old")
	for _, w := range []AgentWake{
		{ID: "loop", ThreadID: "old", Kind: AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 60, NextAt: 100},
		{ID: "gone", ThreadID: "old", Kind: AgentWakeLoop, Label: "stale", Note: "n", IntervalSeconds: 60, NextAt: 100},
	} {
		if _, err := s.RegisterAgentWake(w); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.CancelAgentWake("old", "stale", "", "cancelled"); err != nil {
		t.Fatal(err)
	}
	seedThreadRequest(t, s, "spawn", "old", ThreadRequestSpawn, 10)
	if _, err := s.SetThreadRequestTarget("spawn", "", "worker", "", ""); err != nil {
		t.Fatal(err)
	}
	seedThreadRequest(t, s, "remote", "old", ThreadRequestSpawn, 11)
	if _, err := s.SetThreadRequestTarget("remote", "desk", "far", "Far", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertThreadRequest(ThreadRequest{Token: "remind", CallerThreadID: "old", Kind: ThreadRequestRemind,
		TargetThreadID: "old", State: ThreadRequestUnconfirmed, CreatedAt: 12, UpdatedAt: 12}); err != nil {
		t.Fatal(err)
	}

	handOff, err := s.HandOffAssistant("old", "new", 500)
	if err != nil {
		t.Fatalf("hand off: %v", err)
	}
	if handOff.Agent.ID != "old" || handOff.Agent.ThreadID != "new" || !handOff.Archived || !handOff.Old.Archived ||
		!slices.Equal(handOff.MovedWakeIDs, []string{"loop"}) || handOff.MovedRequests != 3 {
		t.Fatalf("handoff = %+v", handOff)
	}
	if agent, found, err := s.GetStandingAgent("new"); err != nil || !found || agent.Home != "/h/old" {
		t.Fatalf("record on the successor = %+v %v %v", agent, found, err)
	}
	if _, found, _ := s.GetStandingAgent("old"); found {
		t.Fatal("the old thread is still current")
	}
	if current, found, err := s.AssistantCurrentThreadOf("old"); err != nil || !found || current != "new" {
		t.Fatalf("current thread of the past one = %q %v %v", current, found, err)
	}
	if _, found, _ := s.AssistantCurrentThreadOf("new"); found {
		t.Fatal("a current thread maps to a current thread")
	}
	old, _ := s.GetThread("old")
	if old.PastAgentID != "old" || old.AgentSupersededAt == nil || *old.AgentSupersededAt != 500 || old.AgentID != "" {
		t.Fatalf("old row = past %q superseded %v agent %q", old.PastAgentID, old.AgentSupersededAt, old.AgentID)
	}
	if wake, _, _ := s.GetAgentWake("loop"); wake.ThreadID != "new" {
		t.Errorf("live loop on %q", wake.ThreadID)
	}
	if wake, _, _ := s.GetAgentWake("gone"); wake.ThreadID != "old" {
		t.Errorf("a cancelled loop moved to %q", wake.ThreadID)
	}
	for token, target := range map[string]string{"spawn": "worker", "remote": "far", "remind": "new"} {
		row := rawThreadRequest(t, s, token)
		if row.CallerThreadID != "new" || row.MadeInThreadID != "old" || row.MadeIn() != "old" || row.TargetThreadID != target {
			t.Errorf("%s = caller %q made in %q target %q", token, row.CallerThreadID, row.MadeInThreadID, row.TargetThreadID)
		}
	}
	if source, err := s.RemoteRequestSource("new", "desk", "far"); err != nil || source != "old" {
		t.Errorf("remote source = %q %v, want the thread that made it", source, err)
	}
	if spawner, _, found, err := s.StandingAgentSpawnerOf("worker"); err != nil || !found || spawner != "new" {
		t.Errorf("worker's spawner = %q %v %v, want the current thread", spawner, found, err)
	}

	// A second handoff keeps the first maker.
	mustCreateAssistant(t, s, "taken", "/h/taken")
	if _, err := s.HandOffAssistant("new", "taken", 600); err == nil {
		t.Fatal("handed off onto another assistant's thread")
	}
	if agent, found, _ := s.GetStandingAgent("new"); !found || agent.ID != "old" {
		t.Fatal("a refused handoff moved the record")
	}
	mustCreateThread(t, s, "third")
	if _, err := s.HandOffAssistant("new", "third", 700); err != nil {
		t.Fatal(err)
	}
	if row := rawThreadRequest(t, s, "spawn"); row.CallerThreadID != "third" || row.MadeInThreadID != "old" {
		t.Errorf("after two handoffs spawn = caller %q made in %q", row.CallerThreadID, row.MadeInThreadID)
	}
	if current, _, _ := s.AssistantCurrentThreadOf("old"); current != "third" {
		t.Errorf("the first past thread maps to %q", current)
	}
	if _, err := s.HandOffAssistant("worker", "taken", 800); !errors.Is(err, ErrNotAssistantCurrentThread) {
		t.Errorf("hand off a non-assistant = %v", err)
	}
}

// The agent columns follow a handoff: the past thread is the assistant's
// archived child, the workers nest under the current thread, and an
// assistant another created is never a child of its creator.
func TestThreadRowsFollowAHandOff(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"old", "new", "child", "created"} {
		mustCreateThread(t, s, id)
	}
	mustCreateAssistant(t, s, "old", "/h/old")
	for token, target := range map[string]string{"tok-child": "child", "tok-created": "created"} {
		seedThreadRequest(t, s, token, "old", ThreadRequestSpawn, 10)
		if _, err := s.SetThreadRequestTarget(token, "", target, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateStandingAgent(StandingAgent{ID: "c", ThreadID: "created", Home: "/h/c", CreatorAgentID: "old", CreatedAt: 2}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ArchiveThread("created"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandOffAssistant("old", "new", 500); err != nil {
		t.Fatal(err)
	}
	row := func(id string) Thread {
		t.Helper()
		got, err := s.GetThread(id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := row("new"); got.AgentID != "old" || got.ArchivedChildCount != 1 {
		t.Errorf("current row = agent %q archived children %d, want the past thread alone", got.AgentID, got.ArchivedChildCount)
	}
	if got := row("child"); got.SpawnedByThreadID != "new" || !got.SpawnedByAgent {
		t.Errorf("worker row = spawnedBy %q agent %v", got.SpawnedByThreadID, got.SpawnedByAgent)
	}
	listed, err := s.ListArchivedSpawnedThreads("new")
	if err != nil || len(listed) != 1 || listed[0].ID != "old" {
		t.Errorf("archived children of the current thread = %+v %v", listed, err)
	}
}

// A receipt still waiting on a past thread moves to the current one, with
// the local request row; one already running stays.
func TestRetargetThreadReceipt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"sender", "old", "new"} {
		mustCreateThread(t, s, id)
	}
	seedThreadRequest(t, s, "tok", "sender", ThreadRequestSend, 10)
	if _, err := s.SetThreadRequestTarget("tok", "", "old", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{Token: "tok", OwnerDeviceID: "local", SourceThreadID: "sender", Kind: ThreadRequestSend, TargetThreadID: "old"}); err != nil {
		t.Fatal(err)
	}
	if moved, err := s.RetargetThreadReceipt("tok", "old", "new"); err != nil || !moved {
		t.Fatalf("retarget = %v %v", moved, err)
	}
	if row := rawThreadRequest(t, s, "tok"); row.TargetThreadID != "new" {
		t.Errorf("request target = %q", row.TargetThreadID)
	}
	if moved, err := s.RetargetThreadReceipt("tok", "old", "new"); err != nil || moved {
		t.Fatalf("a second retarget = %v %v, want no change", moved, err)
	}
}

// The frontend folds names the same way (stores/assistants.svelte.ts); both
// sides read testdata/assistant_name_folds.json so the two cannot drift.
func TestSameAssistantNameFolds(t *testing.T) {
	raw, err := os.ReadFile("testdata/assistant_name_folds.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		A, B string
		Same bool
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if got := SameAssistantName(c.A, c.B); got != c.Same {
			t.Errorf("SameAssistantName(%q, %q) = %v, want %v", c.A, c.B, got, c.Same)
		}
	}
}
