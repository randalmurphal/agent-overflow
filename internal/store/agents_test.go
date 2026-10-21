package store

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

// mustCreateAssistant records an assistant on an existing thread, its id the
// thread's as v155 migrates a promoted thread.
// mustCreateAssistant moves threadID into home and records an assistant on
// it, its id the thread's.
func mustCreateAssistant(t *testing.T, s *Store, threadID, home string) StandingAgent {
	t.Helper()
	mustPlaceInHome(t, s, threadID, home)
	agent := StandingAgent{ID: threadID, ThreadID: threadID, Home: home, CreatedAt: 1}
	if err := s.CreateStandingAgent(agent); err != nil {
		t.Fatalf("create assistant on %s: %v", threadID, err)
	}
	return agent
}

// mustPlaceInHome moves a thread outside every project into home, where an
// assistant's thread and a handoff's successor live.
func mustPlaceInHome(t *testing.T, s *Store, threadID, home string) {
	t.Helper()
	if err := s.PlaceThreadInHome(threadID, home); err != nil {
		t.Fatalf("place %s in %s: %v", threadID, home, err)
	}
}

func mustRetitle(t *testing.T, s *Store, threadID, title string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE threads SET title = ? WHERE id = ?`, title, threadID); err != nil {
		t.Fatal(err)
	}
}

// An assistant is recorded only on a thread in its home, outside every
// project, with no group, worktree or branch: PlaceThreadInHome moves an
// existing thread there, origin cleared, and refuses a thread that is
// already an assistant's.
func TestAnAssistantsThreadLivesInItsHome(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	group := mustCreateGroup(t, s, defaultTestProjectID, "Release")
	thread := makeThread("ops", "claude")
	thread.Title, thread.GroupID, thread.Branch, thread.WorktreePath = "Ops", group.ID, "feature", "/repo/.worktrees/feature"
	thread.Origin = ThreadOrigin{Branch: "feature", HeadCommit: "abc"}
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	agent := StandingAgent{ID: "ops", ThreadID: "ops", Home: "/config/agents/a", CreatedAt: 1}
	if err := s.CreateStandingAgent(agent); !errors.Is(err, ErrAssistantThreadNotInHome) {
		t.Fatalf("a thread in a project = %v, want ErrAssistantThreadNotInHome", err)
	}
	if _, found, err := s.GetStandingAgent("ops"); err != nil || found {
		t.Fatalf("a refused record stayed: %v %v", found, err)
	}
	mustPlaceInHome(t, s, "ops", "/config/agents/other")
	if err := s.CreateStandingAgent(agent); !errors.Is(err, ErrAssistantThreadNotInHome) {
		t.Fatalf("a thread in another home = %v, want ErrAssistantThreadNotInHome", err)
	}
	mustPlaceInHome(t, s, "ops", agent.Home)
	if err := s.CreateStandingAgent(agent); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetThread("ops")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProjectID != "" || got.WorkspacePath != "/config/agents/a" || got.GroupID != "" || got.Placement != "standalone" ||
		got.Branch != "" || got.WorktreePath != "" || !got.Origin.IsZero() || got.AgentID != "ops" {
		t.Fatalf("the assistant's thread = %+v", got)
	}
	if err := s.PlaceThreadInHome("ops", "/config/agents/b"); err == nil {
		t.Fatal("an assistant's thread was moved to another home")
	}
}

// Nothing deletes an assistant's record while threads live under it: a
// thread delete's mark and a move's commit are refused, naming the counts,
// and leave everything in place. With them gone the mark goes through, and
// from then on no helper is recorded for the assistant.
func TestAnAssistantIsDeletedOnlyWithNothingUnderIt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	homeThread(t, s, "old", "")
	mustRetitle(t, s, "old", "Ops")
	agent := StandingAgent{ID: "a-ops", ThreadID: "old", Home: "/h/old", CreatedAt: 1}
	if err := s.CreateStandingAgent(agent); err != nil {
		t.Fatal(err)
	}
	homeThread(t, s, "next", "")
	if _, err := s.HandOffAssistant("old", "next", 10); err != nil {
		t.Fatal(err)
	}
	homeThread(t, s, "helper", agent.ID)
	refused := func(what string, err error) {
		t.Helper()
		var dependents *AssistantDependentsError
		if !errors.As(err, &dependents) || dependents.AgentID != agent.ID || dependents.PastThreads != 1 || dependents.Helpers != 1 {
			t.Fatalf("%s = %v, want the past thread and the helper named", what, err)
		}
		if ids, err := s.AssistantDependentThreadIDs(agent.ID); err != nil || !slices.Equal(ids, []string{"old", "helper"}) {
			t.Fatalf("after %s the threads under the assistant = %q %v", what, ids, err)
		}
		if _, found, err := s.GetStandingAgent("next"); err != nil || !found {
			t.Fatalf("after %s the record = %v %v", what, found, err)
		}
	}
	if past, helpers, err := s.AssistantDependentsOf("next"); err != nil || past != 1 || helpers != 1 {
		t.Fatalf("dependents = %d %d %v", past, helpers, err)
	}
	if past, helpers, err := s.AssistantDependentsOf("helper"); err != nil || past != 0 || helpers != 0 {
		t.Fatalf("a helper's dependents = %d %d %v", past, helpers, err)
	}
	refused("the delete mark", s.BeginThreadDelete("next"))
	if pending, err := s.ListPendingThreadDeletes(); err != nil || len(pending) != 0 {
		t.Fatalf("a refused mark marked %q %v", pending, err)
	}
	refused("the store delete", s.DeleteThread("next"))
	move, err := s.CreateThreadTransfer(transferRequest("next", "move", "outgoing"))
	if err != nil {
		t.Fatal(err)
	}
	advanceTransfer(t, s, move.ID, "prepared")
	_, err = s.AdvanceThreadTransfer(move.ID, "committed", strings.Repeat("b", 64))
	refused("the move's commit", err)
	advanceTransfer(t, s, move.ID, "canceled")

	for _, id := range []string{"old", "helper"} {
		if err := s.DeleteThread(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.BeginThreadDelete("next"); err != nil {
		t.Fatalf("mark with nothing under the assistant: %v", err)
	}
	late := makeThread("late", "claude")
	late.ProjectID, late.WorkspacePath, late.HelperOfAgentID = "", agent.Home, agent.ID
	if err := s.CreateThread(late); !errors.Is(err, ErrHelperOfNoAssistant) {
		t.Fatalf("a helper of an assistant being deleted = %v, want ErrHelperOfNoAssistant", err)
	}
	if _, err := s.GetThread("late"); err == nil {
		t.Fatal("a refused helper left its thread")
	}
	if err := s.DeleteThread("next"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.GetAgent(agent.ID); err != nil || found {
		t.Fatalf("the record after the delete = %v %v", found, err)
	}
}

// A helper lives outside every project: one in a project is refused with
// its thread.
func TestAHelperInAProjectIsRefused(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	homeThread(t, s, "ops", "")
	mustRetitle(t, s, "ops", "Ops")
	if err := s.CreateStandingAgent(StandingAgent{ID: "a-ops", ThreadID: "ops", Home: "/h/ops", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	helper := makeThread("helper", "claude")
	helper.HelperOfAgentID = "a-ops"
	if err := s.CreateThread(helper); !errors.Is(err, ErrHelperInProject) {
		t.Fatalf("a helper in a project = %v, want ErrHelperInProject", err)
	}
	if _, err := s.GetThread("helper"); err == nil {
		t.Fatal("a refused helper left its thread")
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

	// The successor becomes the assistant's thread, so it must be in no
	// project.
	if _, err := s.HandOffAssistant("old", "new", 500); !errors.Is(err, ErrAssistantThreadNotInHome) {
		t.Fatalf("a handoff onto a thread in a project = %v", err)
	}
	if agent, found, _ := s.GetStandingAgent("old"); !found || agent.ID != "old" {
		t.Fatal("a refused handoff moved the record")
	}
	mustPlaceInHome(t, s, "new", "/h/old")
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
	mustPlaceInHome(t, s, "third", "/h/old")
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
	mustPlaceInHome(t, s, "created", "/h/c")
	if err := s.CreateStandingAgent(StandingAgent{ID: "c", ThreadID: "created", Home: "/h/c", CreatorAgentID: "old", CreatedAt: 2}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ArchiveThread("created"); err != nil {
		t.Fatal(err)
	}
	mustPlaceInHome(t, s, "new", "/h/old")
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
	if got := row("new"); got.AgentID != "old" || archivedTotal(got) != 1 {
		t.Errorf("current row = agent %q archived children %d, want the past thread alone", got.AgentID, archivedTotal(got))
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

// homeThread seeds a thread outside every project, as an assistant's and
// its helpers' are.
func homeThread(t *testing.T, s *Store, id, helperOf string) Thread {
	t.Helper()
	thread := makeThread(id, "claude")
	thread.ProjectID, thread.WorkspacePath, thread.HelperOfAgentID = "", "/h/"+id, helperOf
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("create thread %s: %v", id, err)
	}
	got, err := s.GetThread(id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// An assistant serves the projects its record names, without repeats, or
// all of them; its thread row carries the choice, a project's deletion
// unassigns it, and a project that does not exist is refused whole.
func TestAnAssistantServesItsProjects(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	other, err := s.CreateProject(Project{ID: "other", Path: "/tmp/other", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	homeThread(t, s, "ops", "")
	mustRetitle(t, s, "ops", "Ops")
	agent := StandingAgent{ID: "a-ops", ThreadID: "ops", Home: "/h/ops", CreatedAt: 1}
	if err := s.CreateStandingAgent(agent, other.ID, defaultTestProjectID, other.ID); err != nil {
		t.Fatal(err)
	}
	served := func() []string {
		t.Helper()
		projects, err := s.ServedProjects(agent.ID)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, p := range projects {
			names = append(names, p.Name)
		}
		return names
	}
	row, err := s.GetThread("ops")
	if err != nil || row.ProjectID != "" || row.AgentID != agent.ID || row.AgentAllProjects ||
		!slices.Equal(row.AgentProjectIDs, []string{defaultTestProjectID, other.ID}) {
		t.Fatalf("thread row = project %q agent %q all %v projects %q (%v)", row.ProjectID, row.AgentID, row.AgentAllProjects, row.AgentProjectIDs, err)
	}
	listed, err := s.ListAssistants()
	if err != nil || len(listed) != 1 || listed[0].Agent != agent || listed[0].Name != "Ops" {
		t.Fatalf("listed = %+v (%v)", listed, err)
	}
	if got := served(); !slices.Equal(got, []string{"Default Test Project", "Other"}) {
		t.Fatalf("served = %q", got)
	}

	// All projects: no rows, every unarchived project, a new one included.
	if _, found, err := s.SetAgentProjects(agent.ID, true, []string{other.ID}); err != nil || !found {
		t.Fatalf("SetAgentProjects(all) = %v %v", found, err)
	}
	if _, err := s.CreateProject(Project{ID: "later", Path: "/tmp/later", Name: "Later"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ArchiveProject(other.ID); err != nil {
		t.Fatal(err)
	}
	if got := served(); !slices.Equal(got, []string{"Default Test Project", "Later"}) {
		t.Fatalf("served by a chief of staff = %q", got)
	}
	row, _ = s.GetThread("ops")
	if !row.AgentAllProjects || row.AgentProjectIDs != nil {
		t.Fatalf("thread row = all %v projects %q", row.AgentAllProjects, row.AgentProjectIDs)
	}

	// A project named is served while archived; deleting it unassigns it
	// and leaves the assistant.
	if _, _, err := s.SetAgentProjects(agent.ID, false, []string{other.ID}); err != nil {
		t.Fatal(err)
	}
	if got := served(); !slices.Equal(got, []string{"Other"}) {
		t.Fatalf("served = %q", got)
	}
	if err := s.DeleteProject(other.ID); err != nil {
		t.Fatal(err)
	}
	if got, found, err := s.GetStandingAgent("ops"); err != nil || !found || got.ID != agent.ID {
		t.Fatalf("the assistant after its project's deletion = %+v %v %v", got, found, err)
	}
	if ids, err := s.AgentProjectIDs(agent.ID); err != nil || len(ids) != 0 {
		t.Fatalf("projects after the deletion = %q %v", ids, err)
	}

	if _, found, err := s.SetAgentProjects("nobody", true, nil); err != nil || found {
		t.Fatalf("an unknown assistant = %v %v", found, err)
	}
	if _, _, err := s.SetAgentProjects(agent.ID, false, []string{"gone"}); err == nil {
		t.Fatal("an unknown project was accepted")
	}
	homeThread(t, s, "scout", "")
	mustRetitle(t, s, "scout", "Scout")
	if err := s.CreateStandingAgent(StandingAgent{ID: "a-scout", ThreadID: "scout", Home: "/h/scout"}, "gone"); err == nil {
		t.Fatal("an assistant serving an unknown project was recorded")
	}
	if _, found, err := s.GetStandingAgent("scout"); err != nil || found {
		t.Fatalf("a refused record stayed: %v %v", found, err)
	}
}

// A helper is recorded with its thread, and a fork of a thread under an
// assistant is its helper; the threads under the assistant list past
// threads first, then helpers, each oldest first, and go with their rows.
func TestHelpersLiveUnderTheirAssistant(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	homeThread(t, s, "ops", "")
	mustRetitle(t, s, "ops", "Ops")
	agent := StandingAgent{ID: "a-ops", ThreadID: "ops", Home: "/h/ops", CreatedAt: 1}
	if err := s.CreateStandingAgent(agent, defaultTestProjectID); err != nil {
		t.Fatal(err)
	}
	helper := homeThread(t, s, "helper", agent.ID)
	if helper.HelperOfAgentID != agent.ID || helper.AgentID != "" || helper.AgentProjectIDs != nil {
		t.Fatalf("helper row = helperOf %q agent %q projects %q", helper.HelperOfAgentID, helper.AgentID, helper.AgentProjectIDs)
	}
	if err := s.CreateThread(Thread{ID: "stray", Provider: "claude", Model: "m", WorkspacePath: "/h", HelperOfAgentID: "nobody"}); err == nil {
		t.Fatal("a helper of no assistant was created")
	}
	if _, err := s.GetThread("stray"); err == nil {
		t.Fatal("a refused helper left its thread")
	}

	ops, err := s.GetThread("ops")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []Thread{ops, helper} {
		fork := BuildForkedThread(source)
		if fork.HelperOfAgentID != agent.ID {
			t.Fatalf("a fork of %s is a helper of %q", source.ID, fork.HelperOfAgentID)
		}
	}
	project := makeThread("project-work", "claude")
	if err := s.CreateThread(project); err != nil {
		t.Fatal(err)
	}
	if fork := BuildForkedThread(project); fork.HelperOfAgentID != "" {
		t.Fatalf("a fork of project work is a helper of %q", fork.HelperOfAgentID)
	}
	fork := BuildForkedThread(helper)
	fork.ID = "helper-fork"
	if err := s.CreatePointerFork(fork, helper.ID, ForkCut{}, testInterruptedSummary, 2); err != nil {
		t.Fatal(err)
	}

	homeThread(t, s, "next", "")
	if _, err := s.HandOffAssistant("ops", "next", 10); err != nil {
		t.Fatal(err)
	}
	ids, err := s.AssistantDependentThreadIDs(agent.ID)
	if err != nil || !slices.Equal(ids, []string{"ops", "helper", "helper-fork"}) {
		t.Fatalf("threads under the assistant = %q %v", ids, err)
	}
	for _, placement := range []string{PlacementStandalone, PlacementNested} {
		if _, err := s.SetThreadPlacement([]string{"helper"}, placement); !errors.Is(err, ErrPlacementOutsideProjects) {
			t.Fatalf("placing a helper %s = %v", placement, err)
		}
	}
	if err := s.DeleteThread("helper"); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.AssistantDependentThreadIDs(agent.ID); err != nil || !slices.Equal(ids, []string{"ops", "helper-fork"}) {
		t.Fatalf("after deleting a helper = %q %v", ids, err)
	}
}

// archivedTotal is how many archived threads live under the assistant
// whose current thread this row is, over all its containers.
func archivedTotal(thread Thread) int {
	total := 0
	for _, n := range thread.ArchivedChildCounts {
		total += n
	}
	return total
}

// An assistant's archived threads are counted by the container that holds
// them: its card (helpers, and past threads nested or outside every
// project) and its row in each project (the nested threads it spawned
// there). The listing returns each of them once.
func TestArchivedChildrenAreCountedByContainer(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.CreateProject(Project{ID: "other", Path: "/tmp/other", Name: "Other"}); err != nil {
		t.Fatal(err)
	}
	homeThread(t, s, "old", "")
	mustRetitle(t, s, "old", "Ops")
	agent := StandingAgent{ID: "a-ops", ThreadID: "old", Home: "/h/old", CreatedAt: 1}
	if err := s.CreateStandingAgent(agent, defaultTestProjectID, "other"); err != nil {
		t.Fatal(err)
	}
	homeThread(t, s, "helper", agent.ID)
	homeThread(t, s, "helper-standalone", agent.ID)
	mustCreateThread(t, s, "here")
	other := makeThread("there", "claude")
	other.ProjectID = "other"
	if err := s.CreateThread(other); err != nil {
		t.Fatal(err)
	}
	mustCreateThread(t, s, "grouped")
	for token, target := range map[string]string{"t-helper": "helper", "t-here": "here", "t-there": "there", "t-grouped": "grouped"} {
		seedThreadRequest(t, s, token, "old", ThreadRequestSpawn, 10)
		if _, err := s.SetThreadRequestTarget(token, "", target, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetThreadPlacement([]string{"here", "there"}, PlacementNested); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetThreadPlacement([]string{"grouped"}, PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	// A helper whose spawn row went is standalone by the v159 trigger; it
	// is still the assistant's.
	if _, err := s.db.Exec(`UPDATE threads SET placement = 'standalone' WHERE id = 'helper-standalone'`); err != nil {
		t.Fatal(err)
	}
	homeThread(t, s, "new", "")
	if _, err := s.HandOffAssistant("old", "new", 20); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"helper", "helper-standalone", "here", "there", "grouped"} {
		if _, _, err := s.ArchiveThread(id); err != nil {
			t.Fatal(err)
		}
	}
	row, err := s.GetThread("new")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"": 3, defaultTestProjectID: 1, "other": 1}
	if len(row.ArchivedChildCounts) != len(want) {
		t.Fatalf("counts = %v, want %v", row.ArchivedChildCounts, want)
	}
	for scope, n := range want {
		if row.ArchivedChildCounts[scope] != n {
			t.Fatalf("counts = %v, want %v", row.ArchivedChildCounts, want)
		}
	}
	listed, err := s.ListArchivedSpawnedThreads("new")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, thread := range listed {
		ids = append(ids, thread.ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"helper", "helper-standalone", "here", "old", "there"}) {
		t.Fatalf("listed = %q", ids)
	}
	if plain, err := s.GetThread("here"); err != nil || plain.ArchivedChildCounts != nil {
		t.Fatalf("a child's row carries counts: %v %v", plain.ArchivedChildCounts, err)
	}
}
