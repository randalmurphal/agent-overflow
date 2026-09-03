package threadapp

import (
	"testing"

	"agent-overflow/internal/store"
)

func TestACreatedThreadRecordsWhereItsWorkspaceStood(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	observed := store.ThreadOrigin{
		Branch:     "feature/one",
		RemoteURL:  "git@example.com:owner/repo.git",
		HeadCommit: "0123456789abcdef0123456789abcdef01234567",
	}
	service.deps.Workspace = testWorkspace{currentBranch: "feature/one", origin: observed}

	thread, err := service.Create(CreateOptions{ProjectID: "project", CreatedByDevice: "device-a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if thread.Origin != observed {
		t.Fatalf("origin = %+v, want %+v", thread.Origin, observed)
	}
	if thread.CreatedByDevice != "device-a" {
		t.Fatalf("createdByDevice = %q, want device-a", thread.CreatedByDevice)
	}

	// The values must survive the round trip, not just the in-memory return.
	reloaded, err := database.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if reloaded.Origin != observed || reloaded.CreatedByDevice != "device-a" {
		t.Fatalf("reloaded = %+v / %q", reloaded.Origin, reloaded.CreatedByDevice)
	}
}

// A workspace that is not a repository, or one whose repository has no remote,
// is an ordinary situation — a scratch directory, a fresh `git init`, a home
// directory terminal. Creation must succeed and report nothing known.
func TestAWorkspaceWithNoGitCoordinatesCreatesAnyway(t *testing.T) {
	service, _, _ := newServiceFixture(t)
	service.deps.Workspace = testWorkspace{}

	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !thread.Origin.IsZero() {
		t.Fatalf("origin = %+v, want zero", thread.Origin)
	}
	if thread.CreatedByDevice != "" {
		t.Fatalf("createdByDevice = %q, want empty", thread.CreatedByDevice)
	}
}

// A terminal is a thread too, and it is the case most likely to sit outside a
// repository — which is exactly why it must record what it can when it does
// sit inside one.
func TestATerminalRecordsItsWorkspaceCoordinates(t *testing.T) {
	service, _, _ := newServiceFixture(t)
	observed := store.ThreadOrigin{Branch: "main", RemoteURL: "https://example.com/o/r.git", HeadCommit: "abc"}
	service.deps.Workspace = testWorkspace{origin: observed}

	thread, err := service.StartTerminal(TerminalOptions{ProjectID: "project", CreatedByDevice: "device-b"})
	if err != nil {
		t.Fatalf("StartTerminal: %v", err)
	}
	if thread.Origin != observed || thread.CreatedByDevice != "device-b" {
		t.Fatalf("terminal provenance = %+v / %q", thread.Origin, thread.CreatedByDevice)
	}
}

// Creation provenance is write-once: it is deliberately absent from
// updateThreadSetSQL, so no later mutation — from this device or any other —
// can restate where the thread came from.
func TestALaterUpdateCannotRestateCreationProvenance(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	observed := store.ThreadOrigin{Branch: "main", RemoteURL: "https://example.com/o/r.git", HeadCommit: "abc"}
	service.deps.Workspace = testWorkspace{origin: observed}

	thread, err := service.Create(CreateOptions{ProjectID: "project", CreatedByDevice: "device-a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rewritten := thread
	rewritten.CreatedByDevice = "device-b"
	rewritten.Origin = store.ThreadOrigin{Branch: "other", RemoteURL: "https://elsewhere", HeadCommit: "def"}
	rewritten.Title = "renamed"
	if err := database.UpdateThread(rewritten); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}

	reloaded, err := database.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if reloaded.Title != "renamed" {
		t.Fatalf("title = %q; the update itself should have applied", reloaded.Title)
	}
	if reloaded.CreatedByDevice != "device-a" || reloaded.Origin != observed {
		t.Fatalf("provenance was rewritten: %q / %+v", reloaded.CreatedByDevice, reloaded.Origin)
	}
}
