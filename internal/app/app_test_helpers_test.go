package app

import (
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// defaultTestProjectID is the stable project id that test-only helpers
// seed into every test store so inline `store.Thread{}` literals satisfy
// the v13 project_id FK without each test having to call CreateProject.
const defaultTestProjectID = "default-test-project"

// ensureDefaultTestProject inserts the default test project if it's not
// already present. Idempotent so helpers that call it multiple times
// within the same test don't trip the UNIQUE constraint.
func ensureDefaultTestProject(t *testing.T, a *App) {
	t.Helper()
	if a.store == nil {
		return
	}
	if _, err := a.store.GetProject(defaultTestProjectID); err == nil {
		return
	}
	now := time.Now().UnixMilli()
	if _, err := a.store.CreateProject(store.Project{
		ID:        defaultTestProjectID,
		Path:      "/tmp/workspace",
		Name:      "Default Test Project",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("ensureDefaultTestProject: %v", err)
	}
}

// createTestThread is a test-only convenience that preserves the four
// positional args tests used to pass to CreateThread (provider, workspace,
// model, mode) by wrapping the v13 CreateThreadOptions struct. It auto-
// creates a project row for the workspace path when one doesn't yet
// exist — every v13+ thread requires a project_id FK, and threading that
// through every test call-site by hand is noise.
//
// Returns the Thread and any error from the binding. The test should
// pass through errors verbatim (`_, err := createTestThread(...)`) so
// failure modes are the same as calling a.CreateThread directly.
func createTestThread(t *testing.T, a *App, providerName, workspacePath, model, mode string) (store.Thread, error) {
	t.Helper()
	project, err := a.ensureProjectForWorkspace(workspacePath)
	if err != nil {
		return store.Thread{}, err
	}
	return a.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID: project.ID,
		Provider:  providerName,
		Model:     model,
		Mode:      strings.TrimSpace(mode),
	})
}

// projectPathForThread returns the project.Path associated with a
// thread's ProjectID. Tests that used to read the now-removed
// thread.ProjectPath field call this to get the equivalent value.
// Fails the test on lookup error so callers can treat the return as a
// plain string.
func projectPathForThread(t *testing.T, a *App, thread store.Thread) string {
	t.Helper()
	if thread.ProjectID == "" {
		return ""
	}
	p, err := a.store.GetProject(thread.ProjectID)
	if err != nil {
		t.Fatalf("projectPathForThread %s: %v", thread.ID, err)
	}
	return p.Path
}

// waitForThreadLockRefs proves a concurrent caller is parked on a keyed thread
// action lock without depending on scheduler sleeps.
func waitForThreadLockRefs(t *testing.T, locks interface{ Refs(string) int }, threadID string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if locks.Refs(threadID) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("lock refs for %s = %d, want %d", threadID, locks.Refs(threadID), want)
}
