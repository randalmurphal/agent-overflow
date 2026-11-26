package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/triage"
)

// The tracker tools obey the Agents settings of the calling thread's
// project (trackerapp.AgentRules), end to end on the fake Jira site.

// saveAgentRules saves rules as project's Agents settings.
func (f *trackerBoardFixture) saveAgentRules(t *testing.T, project string, rules trackerapp.AgentRules) {
	t.Helper()
	svc, err := f.a.trackerService()
	if err != nil {
		t.Fatal(err)
	}
	stored, err := svc.ProjectSetup(project)
	if err != nil {
		t.Fatal(err)
	}
	setup := stored.Setup
	setup.Agents = rules
	if save, err := svc.SetProjectSetup(project, stored.Revision, setup); err != nil || !save.Saved {
		t.Fatalf("saving the Agents settings = %+v, %v", save, err)
	}
}

// With Change tickets off, a thread of the project is refused every write
// with a sentence it can act on, and nothing reaches the tracker, while a
// thread of another project listing the same board writes; with Ask before
// changing several at once off, a change to two tickets goes with no
// approval.
func TestTrackerWritesFollowTheProjectsAgentsSettings(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	off := false
	f.saveAgentRules(t, f.project, trackerapp.AgentRules{ChangeTickets: &off})
	_, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	const sentence = "[tracker_changes_off] This project's Agents settings do not let its threads change tickets, so nothing was sent; ask the user to turn on Change tickets in Settings > Issues > Agents if this change is wanted."
	text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1"}, "comment": "Checked."})
	if !isError || text != sentence {
		t.Fatalf("ticket_update with changes off = %s (error %v)", text, isError)
	}
	text, isError = trackerMCPStreamedCall(t, f.a, endpoint, "ticket_create", map[string]any{"project": "WEB", "title": "New"})
	if !isError || text != sentence {
		t.Fatalf("ticket_create with changes off = %s (error %v)", text, isError)
	}
	if text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_read", map[string]any{"key": "WEB-1"}); isError {
		t.Fatalf("ticket_read with changes off = %s", text)
	}
	if got := fakeComments(t, f, "10101"); len(got) != 0 {
		t.Fatalf("a thread whose project turns changes off commented %q", got)
	}

	// The rules are the thread's project's, not the board's.
	other := "project-other"
	if _, err := f.a.store.CreateProject(store.Project{ID: other, Path: t.TempDir(), Name: other, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.a.store.ReplaceProjectTrackerBoards(other, nil, []store.ProjectTrackerBoard{{ConnectionID: f.conn.ID, BoardID: "1"}}); err != nil {
		t.Fatal(err)
	}
	_, otherEndpoint := trackerWriteThreadIn(t, f, other, provider.RuntimeAuto)
	if text, isError := trackerMCPStreamedCall(t, f.a, otherEndpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1"}, "comment": "From the other project."}); isError {
		t.Fatalf("a thread of another project = %s", text)
	}

	f.saveAgentRules(t, f.project, trackerapp.AgentRules{AskSeveral: &off})
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	// An approval nobody answers ends the call at once.
	f.a.trackerMCP.approvals.wait = 200 * time.Millisecond
	text, isError = trackerMCPStreamedCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1", "WEB-2"}, "comment": "Both checked."})
	if isError || !strings.Contains(text, `"applied":["comment"]`) {
		t.Fatalf("two tickets with asking off = %s (error %v)", text, isError)
	}
	for _, req := range f.a.triage.PendingInteractiveRequests(thread.ID).Approvals {
		if req.Kind == triage.ApprovalKindTrackerWrite {
			t.Fatalf("a project that does not ask raised %+v", req)
		}
	}
	if len(fakeComments(t, f, "10101")) != 2 || len(fakeComments(t, f, "10102")) != 1 {
		t.Fatalf("comments %q and %q", fakeComments(t, f, "10101"), fakeComments(t, f, "10102"))
	}
}

// Agents settings this build cannot read refuse the write rather than take
// the built-in rules: a setup file that cannot be read is named.
func TestTrackerWritesRefusedWhenTheAgentsSettingsCannotBeRead(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	path := f.setupPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1"}, "comment": "Checked."})
	if !isError || !strings.HasPrefix(text, "[tracker_setup_unreadable] Nothing was sent: this project's tracker setup cannot be read from "+path) {
		t.Fatalf("a write under an unreadable setup = %s (error %v)", text, isError)
	}
	if got := fakeComments(t, f, "10101"); len(got) != 0 {
		t.Fatalf("a write under an unreadable setup commented %q", got)
	}
}

// trackerWriteThreadIn is trackerWriteThread for a thread of project.
func trackerWriteThreadIn(t *testing.T, f *trackerBoardFixture, project string, mode provider.RuntimeMode) (store.Thread, string) {
	t.Helper()
	f.a.ensureTriageRouter()
	thread := store.Thread{ID: uuid.NewString(), ProjectID: project, Provider: string(provider.Codex), WorkspacePath: t.TempDir(), RuntimeMode: string(mode)}
	if err := f.a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	f.a.sessionManager().put(thread.ID, session{Token: token, Provider: thread.Provider})
	f.a.triage.MarkThreadActive(thread.ID, token)
	endpoint := trackerMCPEndpoint(t, f.a, thread, token)
	if endpoint == "" {
		t.Fatal("the thread has no tracker tools")
	}
	return thread, endpoint
}
