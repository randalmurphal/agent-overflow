package app

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// Assistants above projects (docs/specs/standing-agents.md, rulings 37 to
// 40): an assistant has no project, runs in its home and serves projects;
// its spawns are helpers in its home or work in a project it serves; its
// deletion takes what lives under it and a project's deletion unassigns it.

// secondProject adds a project to the fixture's computer.
func (f *requestFixture) secondProject(t *testing.T, name string) store.Project {
	t.Helper()
	project, err := f.app.store.CreateProject(store.Project{ID: entityid.New(), Path: t.TempDir(), Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return project
}

// newOps creates the assistant Ops serving projectIDs and makes it the
// fixture's caller.
func (f *requestFixture) newOps(t *testing.T, projectIDs ...string) store.StandingAgent {
	t.Helper()
	thread, err := f.app.CreateAssistant(t.Context(), CreateAssistantOptions{Name: "Ops", ProjectIDs: projectIDs})
	if err != nil {
		t.Fatalf("CreateAssistant: %v", err)
	}
	agent, found, err := f.app.store.GetStandingAgent(thread.ID)
	if err != nil || !found {
		t.Fatalf("record = %v %v", found, err)
	}
	f.caller = thread
	return agent
}

// New Assistant makes a thread outside every project, in the home, serving
// the projects chosen without repeats, or all of them; no choice or an
// unknown project is refused before anything is made.
func TestANewAssistantLivesOutsideEveryProject(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaudeRecording(t, "ready")
	second := f.secondProject(t, "Second")
	before, err := f.app.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	for name, opts := range map[string]CreateAssistantOptions{
		"no projects":     {Name: "Ops"},
		"blank ids":       {Name: "Ops", ProjectIDs: []string{" ", ""}},
		"unknown project": {Name: "Ops", ProjectIDs: []string{f.caller.ProjectID, "gone"}},
	} {
		if _, err := f.app.CreateAssistant(t.Context(), opts); publicCode(t, err) != transport.ErrCodeBadParams {
			t.Errorf("%s: %v, want the bad-params refusal", name, err)
		}
	}
	after, err := f.app.store.ListThreads()
	if err != nil || len(after) != len(before) {
		t.Fatalf("a refused creation left a thread: %d rows, had %d (%v)", len(after), len(before), err)
	}
	if entries, err := os.ReadDir(filepath.Join(f.app.configDir, "agents")); err == nil && len(entries) != 0 {
		t.Fatalf("a refused creation left a home: %v", entries)
	}

	want := []string{f.caller.ProjectID, second.ID}
	agent := f.newOps(t, second.ID, f.caller.ProjectID, second.ID)
	thread, err := f.app.store.GetThread(agent.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(want)
	if thread.ProjectID != "" || thread.WorkspacePath != agent.Home || thread.WorktreePath != "" || thread.Branch != "" ||
		thread.AgentAllProjects || !slices.Equal(thread.AgentProjectIDs, want) {
		t.Fatalf("thread = project %q workspace %q worktree %q branch %q all %v projects %q, want home %s serving %q",
			thread.ProjectID, thread.WorkspacePath, thread.WorktreePath, thread.Branch, thread.AgentAllProjects, thread.AgentProjectIDs, agent.Home, want)
	}

	chief, err := f.app.CreateAssistant(t.Context(), CreateAssistantOptions{Name: "Chief", AllProjects: true, ProjectIDs: []string{"ignored"}})
	if err != nil {
		t.Fatalf("CreateAssistant(all): %v", err)
	}
	if !chief.AgentAllProjects || chief.AgentProjectIDs != nil || chief.ProjectID != "" {
		t.Fatalf("chief = all %v projects %q project %q", chief.AgentAllProjects, chief.AgentProjectIDs, chief.ProjectID)
	}
}

// The projects change from the assistant's menu: an empty or unknown
// choice is refused and changes nothing, an ordinary thread is refused,
// and the change is broadcast on the assistant's row. Deleting a project
// unassigns it and leaves the assistant.
func TestAnAssistantsProjectsChange(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaudeRecording(t, "ready")
	plain := f.caller
	second := f.secondProject(t, "Second")
	agent := f.newOps(t, plain.ProjectID)

	var mu sync.Mutex
	var broadcast [][]string
	f.app.testEmitHook = func(name string, data any) {
		event, ok := data.(triage.ThreadUpdateEvent)
		if name == "thread:updated" && ok && event.Thread != nil && event.Thread.ID == agent.ThreadID {
			mu.Lock()
			broadcast = append(broadcast, event.Thread.AgentProjectIDs)
			mu.Unlock()
		}
	}
	if _, err := f.app.SetAssistantProjects(agent.ThreadID, false, nil); publicCode(t, err) != transport.ErrCodeBadParams {
		t.Fatalf("an empty choice = %v", err)
	}
	if _, err := f.app.SetAssistantProjects(agent.ThreadID, false, []string{"gone"}); publicCode(t, err) != transport.ErrCodeBadParams {
		t.Fatalf("an unknown project = %v", err)
	}
	if _, err := f.app.SetAssistantProjects(plain.ID, true, nil); !errors.Is(err, errNotAssistantThread) {
		t.Fatalf("an ordinary thread = %v", err)
	}
	if ids, err := f.app.store.AgentProjectIDs(agent.ID); err != nil || !slices.Equal(ids, []string{plain.ProjectID}) {
		t.Fatalf("a refused change left %q (%v)", ids, err)
	}
	mu.Lock()
	if len(broadcast) != 0 {
		t.Fatalf("a refused change was broadcast: %q", broadcast)
	}
	mu.Unlock()

	updated, err := f.app.SetAssistantProjects(agent.ThreadID, false, []string{second.ID})
	if err != nil || updated.AllProjects {
		t.Fatalf("SetAssistantProjects = %+v, %v", updated, err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(broadcast) > 0 && slices.Equal(broadcast[len(broadcast)-1], []string{second.ID})
	})
	if _, err := f.app.SetAssistantProjects(agent.ThreadID, true, nil); err != nil {
		t.Fatal(err)
	}
	if row, err := f.app.store.GetThread(agent.ThreadID); err != nil || !row.AgentAllProjects || row.AgentProjectIDs != nil {
		t.Fatalf("after all projects: %+v %v", row, err)
	}

	if _, err := f.app.SetAssistantProjects(agent.ThreadID, false, []string{second.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.DeleteProject(second.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if row, found, err := f.app.store.GetStandingAgent(agent.ThreadID); err != nil || !found || row.ID != agent.ID {
		t.Fatalf("the assistant after its project's deletion: %+v %v %v", row, found, err)
	}
	if ids, err := f.app.store.AgentProjectIDs(agent.ID); err != nil || len(ids) != 0 {
		t.Fatalf("projects after the deletion = %q (%v)", ids, err)
	}
}

// An assistant's spawn without a project is a helper in its home, and a
// helper's own is the same assistant's; a spawn into a project it serves
// works there, and one into a project it does not serve is refused, as is
// a helper given a checkout. A fork follows the same rules. An ordinary
// thread's spawn still inherits its project.
func TestAnAssistantSpawnsHelpersAndProjectWork(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaudeRecording(t, "on it")
	plain := f.caller
	project, err := f.app.store.GetProject(plain.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	unserved := f.secondProject(t, "Unserved")
	agent := f.newOps(t, plain.ProjectID)
	spawn := func(call threadtools.SpawnCall) (store.Thread, error) {
		t.Helper()
		call.Prompt = "do it"
		ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), call)
		if err != nil {
			return store.Thread{}, err
		}
		return f.app.store.GetThread(ack.ThreadID)
	}

	helper, err := spawn(threadtools.SpawnCall{})
	if err != nil {
		t.Fatalf("spawn a helper: %v", err)
	}
	if helper.ProjectID != "" || helper.WorkspacePath != agent.Home || helper.HelperOfAgentID != agent.ID {
		t.Fatalf("helper = project %q workspace %q helperOf %q", helper.ProjectID, helper.WorkspacePath, helper.HelperOfAgentID)
	}
	work, err := spawn(threadtools.SpawnCall{ProjectID: plain.ProjectID})
	if err != nil {
		t.Fatalf("spawn into a served project: %v", err)
	}
	if work.ProjectID != plain.ProjectID || work.WorkspacePath != project.Path || work.HelperOfAgentID != "" {
		t.Fatalf("project work = project %q workspace %q helperOf %q", work.ProjectID, work.WorkspacePath, work.HelperOfAgentID)
	}
	if _, err := spawn(threadtools.SpawnCall{ProjectID: unserved.ID}); err == nil ||
		!strings.Contains(err.Error(), "This assistant serves "+project.Name+" ("+project.ID+"), not project "+unserved.ID) {
		t.Fatalf("spawn into an unserved project = %v", err)
	}
	if _, err := spawn(threadtools.SpawnCall{WorktreeBranch: "feature"}); err == nil || !strings.Contains(err.Error(), "takes no worktree") {
		t.Fatalf("a helper with a worktree = %v", err)
	}
	if _, err := spawn(threadtools.SpawnCall{FromThread: helper.ID}); err != nil {
		t.Fatalf("fork its own helper: %v", err)
	}
	stranger, err := createTestThread(t, f.app, "claude", unserved.Path, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spawn(threadtools.SpawnCall{FromThread: stranger.ID}); err == nil || !strings.Contains(err.Error(), "not project "+unserved.ID) {
		t.Fatalf("fork in an unserved project = %v", err)
	}

	f.caller = helper
	nested, err := spawn(threadtools.SpawnCall{})
	if err != nil {
		t.Fatalf("a helper's spawn: %v", err)
	}
	if nested.ProjectID != "" || nested.WorkspacePath != agent.Home || nested.HelperOfAgentID != agent.ID {
		t.Fatalf("a helper's helper = project %q workspace %q helperOf %q", nested.ProjectID, nested.WorkspacePath, nested.HelperOfAgentID)
	}

	f.caller = plain
	ordinary, err := spawn(threadtools.SpawnCall{})
	if err != nil {
		t.Fatalf("an ordinary spawn: %v", err)
	}
	if ordinary.ProjectID != plain.ProjectID || ordinary.HelperOfAgentID != "" {
		t.Fatalf("an ordinary spawn = project %q helperOf %q", ordinary.ProjectID, ordinary.HelperOfAgentID)
	}
}

// New Thread, Same Assistant keeps the successor in the home; deleting the
// assistant's current thread deletes its past threads, its helpers and its
// home, and its project work stays.
func TestDeletingAnAssistantDeletesWhatLivesUnderIt(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaudeRecording(t, "on it")
	projectID := f.caller.ProjectID
	agent := f.newOps(t, projectID)
	helper, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "research"})
	if err != nil {
		t.Fatal(err)
	}
	work, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "build", ProjectID: projectID})
	if err != nil {
		t.Fatal(err)
	}
	successor, err := f.app.StartNextAssistantThread(t.Context(), agent.ThreadID)
	if err != nil {
		t.Fatalf("StartNextAssistantThread: %v", err)
	}
	if got, err := f.app.GetAssistantDependents(successor.ID); err != nil || got != (AssistantDependents{PastThreads: 1, Helpers: 1}) {
		t.Fatalf("dependents = %+v, %v", got, err)
	}
	if got, err := f.app.GetAssistantDependents(work.ThreadID); err != nil || got != (AssistantDependents{}) {
		t.Fatalf("an ordinary thread's dependents = %+v, %v", got, err)
	}
	if successor.ProjectID != "" || successor.WorkspacePath != agent.Home || successor.AgentID != agent.ID {
		t.Fatalf("successor = project %q workspace %q agent %q", successor.ProjectID, successor.WorkspacePath, successor.AgentID)
	}

	if err := f.app.DeleteThread(successor.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	for _, id := range []string{successor.ID, agent.ThreadID, helper.ThreadID} {
		if _, err := f.app.store.GetThread(id); err == nil {
			t.Errorf("thread %s outlived its assistant", id)
		}
	}
	if _, err := f.app.store.GetThread(work.ThreadID); err != nil {
		t.Fatalf("the project work went with the assistant: %v", err)
	}
	if _, err := os.Stat(agent.Home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("home after the delete: %v", err)
	}
}

// An assistant's delete refuses before it deletes anything under it: a
// current thread a transfer holds, or a home that is not the assistant's
// own folder, leaves its past threads and helpers in place. A delete that
// goes through takes them on every path, the boot's included.
func TestARefusedAssistantDeleteKeepsWhatLivesUnderIt(t *testing.T) {
	t.Parallel()
	// assistant records the assistant "agent" with home, a past thread
	// (itself, after a handoff) and a helper, and returns its current
	// thread and the threads under it.
	assistant := func(t *testing.T, home func(configDir string) string) (*threadToolsFixture, string, []string) {
		t.Helper()
		f := newThreadToolsFixture(t)
		at := home(f.app.configDir)
		old := f.thread(t, "agent")
		if err := f.app.store.PlaceThreadInHome(old.ID, at); err != nil {
			t.Fatal(err)
		}
		if err := f.app.store.CreateStandingAgent(storeAgent(old.ID, at)); err != nil {
			t.Fatal(err)
		}
		current := f.thread(t, "agent-next")
		if err := f.app.store.PlaceThreadInHome(current.ID, at); err != nil {
			t.Fatal(err)
		}
		if _, err := f.app.store.HandOffAssistant(old.ID, current.ID, 1); err != nil {
			t.Fatal(err)
		}
		f.thread(t, "helper", func(thread *store.Thread) {
			thread.ProjectID, thread.WorkspacePath, thread.HelperOfAgentID = "", at, old.ID
		})
		return f, current.ID, []string{"agent", "helper"}
	}
	own := func(configDir string) string { return filepath.Join(configDir, "agents", "agent") }
	underIt := func(t *testing.T, f *threadToolsFixture, ids []string, want bool) {
		t.Helper()
		for _, id := range ids {
			if _, err := f.app.store.GetThread(id); (err == nil) != want {
				t.Errorf("thread %s under the assistant: present %v, want %v (%v)", id, err == nil, want, err)
			}
		}
	}

	t.Run("a transfer holds the current thread", func(t *testing.T) {
		t.Parallel()
		f, current, ids := assistant(t, own)
		if _, err := f.app.store.CreateThreadTransfer(store.ThreadTransfer{
			ID: entityid.New(), ThreadID: current, PeerBackendID: entityid.New(), Kind: "move", Direction: "outgoing",
			ActivationHash: strings.Repeat("a", 64), PrivateState: []byte(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
		var transfer *store.ThreadTransferError
		if err := f.app.DeleteThread(current); !errors.As(err, &transfer) {
			t.Fatalf("DeleteThread = %v, want the transfer's refusal", err)
		}
		underIt(t, f, ids, true)
	})
	t.Run("a transfer holds a helper", func(t *testing.T) {
		t.Parallel()
		f, current, ids := assistant(t, own)
		helper := "helper"
		if _, err := f.app.store.CreateThreadTransfer(store.ThreadTransfer{
			ID: entityid.New(), ThreadID: helper, PeerBackendID: entityid.New(), Kind: "move", Direction: "outgoing",
			ActivationHash: strings.Repeat("a", 64), PrivateState: []byte(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
		var transfer *store.ThreadTransferError
		err := f.app.DeleteThread(current)
		if err == nil || errors.As(err, &transfer) || !strings.Contains(err.Error(), "thread "+helper+", which lives under this assistant, is being transferred") {
			t.Fatalf("DeleteThread = %v, want the helper named and no transfer of the assistant's own", err)
		}
		underIt(t, f, append(ids, current), true)
	})
	t.Run("a home that is not its own folder", func(t *testing.T) {
		t.Parallel()
		f, current, ids := assistant(t, func(configDir string) string { return own(configDir) + "-x" })
		if err := f.app.DeleteThread(current); err == nil || !strings.Contains(err.Error(), "this assistant was not deleted") {
			t.Fatalf("DeleteThread = %v, want the home refused", err)
		}
		underIt(t, f, ids, true)
	})
	t.Run("the boot's delete", func(t *testing.T) {
		t.Parallel()
		f, current, ids := assistant(t, own)
		if err := f.app.deleteThreadLocked(current); err != nil {
			t.Fatalf("deleteThreadLocked: %v", err)
		}
		underIt(t, f, append(ids, current), false)
	})
}

// thread_new_assistant: the new assistant serves the projects named, each
// served by its creator, or every one its creator serves.
func TestACreatedAssistantServesItsCreatorsProjects(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaudeRecording(t, "ready")
	served := f.caller.ProjectID
	other := f.secondProject(t, "Other")
	f.newOps(t, served)
	call := threadtools.NewAssistantCall{Name: "Scout", Role: "Watch the build.", Projects: []string{other.ID}}
	if _, err := f.adapter().NewAssistant(t.Context(), f.callerIdentity(), call); err == nil || !strings.Contains(err.Error(), ", not project "+other.ID) {
		t.Fatalf("a project the creator does not serve = %v", err)
	}
	call.Projects = nil
	ack, err := f.adapter().NewAssistant(t.Context(), f.callerIdentity(), call)
	if err != nil {
		t.Fatal(err)
	}
	created := mustThread(t, f.app, ack.ThreadID)
	if created.ProjectID != "" || created.WorkspacePath != ack.Home || !slices.Equal(created.AgentProjectIDs, []string{served}) {
		t.Fatalf("created = project %q workspace %q projects %q", created.ProjectID, created.WorkspacePath, created.AgentProjectIDs)
	}
}

// An assistant works in what its sessions are told it serves: serving all
// projects, that is every unarchived one, so a spawn into an archived
// project, or an assistant it creates naming one, is refused; a project it
// names stays served when it is archived.
func TestAnAssistantServesWhatItsSessionsAreTold(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaudeRecording(t, "ready")
	open := f.caller.ProjectID
	archived := f.secondProject(t, "Archived")
	if _, _, err := f.app.store.ArchiveProject(archived.ID); err != nil {
		t.Fatal(err)
	}
	chief, err := f.app.CreateAssistant(t.Context(), CreateAssistantOptions{Name: "Chief", AllProjects: true})
	if err != nil {
		t.Fatal(err)
	}
	f.caller = chief
	spawn := func(projectID string) error {
		t.Helper()
		_, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "do it", ProjectID: projectID})
		return err
	}
	if err := spawn(open); err != nil {
		t.Fatalf("an all-projects spawn into an unarchived project: %v", err)
	}
	if err := spawn(archived.ID); err == nil || !strings.Contains(err.Error(), ", not project "+archived.ID) {
		t.Fatalf("an all-projects spawn into an archived project = %v", err)
	}
	call := threadtools.NewAssistantCall{Name: "Scout", Role: "Watch the build.", Projects: []string{archived.ID}}
	if _, err := f.adapter().NewAssistant(t.Context(), f.callerIdentity(), call); err == nil || !strings.Contains(err.Error(), ", not project "+archived.ID) {
		t.Fatalf("an all-projects assistant creating one for an archived project = %v", err)
	}

	f.newOps(t, archived.ID)
	if err := spawn(archived.ID); err != nil {
		t.Fatalf("a spawn into an archived project the assistant names: %v", err)
	}
}

// A creation the store refuses once its thread is placed hands the thread
// to unplace and removes the home it made; a failed placement only removes
// the home.
func TestAnAssistantThatIsNotRecordedLeavesNoHome(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	taken := f.thread(t, "taken")
	testAssistant(t, f.app, taken.ID)
	for name, place := range map[string]func(string) (store.Thread, error){
		"refused record": func(string) (store.Thread, error) { return f.app.store.GetThread(taken.ID) },
		"failed place":   func(string) (store.Thread, error) { return store.Thread{}, errors.New("no thread") },
	} {
		var home string
		var unplaced []string
		_, _, err := f.app.recordNewAssistant(func(at string) (store.Thread, error) {
			home = at
			return place(at)
		}, func(thread store.Thread) { unplaced = append(unplaced, thread.ID) }, assistantProjects{all: true}, "")
		if err == nil {
			t.Fatalf("%s: recorded", name)
		}
		if want := map[string][]string{"refused record": {taken.ID}}[name]; !slices.Equal(unplaced, want) {
			t.Errorf("%s: unplaced %q, want %q", name, unplaced, want)
		}
		if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: home %s after the failure: %v", name, home, err)
		}
	}
}

func mustThread(t *testing.T, a *App, id string) store.Thread {
	t.Helper()
	thread, err := a.store.GetThread(id)
	if err != nil {
		t.Fatal(err)
	}
	return thread
}

// archivedTotal is how many archived threads live under an assistant,
// over all its containers.
func archivedTotal(counts map[string]int) int {
	total := 0
	for _, n := range counts {
		total += n
	}
	return total
}
