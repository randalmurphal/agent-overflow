package app

import (
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

func TestAppDeleteProjectReturnsThreadIDs(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	dir := t.TempDir()
	p, err := app.CreateProject(dir)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	thread, err := app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID: p.ID,
		Provider:  "claude",
		Model:     "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	var stopped []string
	app.stopSessionFn = func(threadID string) error {
		stopped = append(stopped, threadID)
		return nil
	}

	result, err := app.DeleteProject(p.ID)
	if err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if len(result.ThreadIDs) != 1 || result.ThreadIDs[0] != thread.ID {
		t.Fatalf("DeleteProject thread ids = %v, want [%s]", result.ThreadIDs, thread.ID)
	}
	if len(stopped) != 1 || stopped[0] != thread.ID {
		t.Fatalf("stopped sessions = %v, want [%s]", stopped, thread.ID)
	}

	// Thread row must be gone through the normal teardown path.
	if _, err := app.store.GetThread(thread.ID); err == nil {
		t.Fatal("thread survived DeleteProject")
	}
}

func TestAppDeleteProjectRejectsRunningThread(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		seed func(*testing.T, *App, string)
	}{
		{
			name: "active turn",
			seed: func(t *testing.T, app *App, threadID string) {
				t.Helper()
				if err := app.store.InsertTurn(store.Turn{
					TurnID:    "turn-project-delete-active",
					ThreadID:  threadID,
					TurnIndex: 0,
					StartedAt: 1,
				}); err != nil {
					t.Fatalf("InsertTurn: %v", err)
				}
			},
		},
		{
			name: "background task",
			seed: func(t *testing.T, app *App, threadID string) {
				t.Helper()
				insertRunningBackgroundToolCall(t, app.store, threadID, "bg-project-delete", 0, 0)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newTestAppWithStore(t)
			p, err := app.CreateProject(t.TempDir())
			if err != nil {
				t.Fatalf("CreateProject: %v", err)
			}
			thread, err := app.CreateThread(t.Context(), CreateThreadOptions{
				ProjectID: p.ID,
				Provider:  "claude",
				Model:     "claude-sonnet-4-6",
			})
			if err != nil {
				t.Fatalf("CreateThread: %v", err)
			}
			tt.seed(t, app, thread.ID)

			stopCalls := 0
			app.stopSessionFn = func(string) error {
				stopCalls++
				return nil
			}
			if _, err := app.DeleteProject(p.ID); err == nil || !strings.Contains(err.Error(), "cannot delete project") {
				t.Fatalf("DeleteProject() error = %v, want running-thread refusal", err)
			}
			if stopCalls != 0 {
				t.Fatalf("stopSession calls = %d, want 0 on preflight refusal", stopCalls)
			}
			if _, err := app.store.GetProject(p.ID); err != nil {
				t.Fatalf("project removed after refused delete: %v", err)
			}
			if _, err := app.store.GetThread(thread.ID); err != nil {
				t.Fatalf("thread removed after refused delete: %v", err)
			}
		})
	}
}

func TestAppDeleteProjectTeardownFailureKeepsProject(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	p, err := app.CreateProject(t.TempDir())
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	thread, err := app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID: p.ID,
		Provider:  "claude",
		Model:     "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	wantErr := errors.New("stop failed")
	app.stopSessionFn = func(string) error { return wantErr }

	if _, err := app.DeleteProject(p.ID); !errors.Is(err, wantErr) {
		t.Fatalf("DeleteProject() error = %v, want errors.Is(stop failed)", err)
	}
	if _, err := app.store.GetProject(p.ID); err != nil {
		t.Fatalf("project removed after teardown failure: %v", err)
	}
	if _, err := app.store.GetThread(thread.ID); err != nil {
		t.Fatalf("thread removed after teardown failure: %v", err)
	}
}

// A deletion that waits on a thread's lock while that thread's own deleter (the
// retention sweep, another client) removes it finishes the project; a thread
// added in that window escapes its locks, so it asks for a retry.
func TestAppDeleteProjectAcrossConcurrentThreadChanges(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		change  func(t *testing.T, app *App, projectID, heldID string)
		wantErr bool
	}{
		{
			name: "thread deleted",
			change: func(t *testing.T, app *App, _, heldID string) {
				if err := app.deleteThreadTreeWithSubtreeLocksHeld(heldID); err != nil {
					t.Fatalf("delete held thread: %v", err)
				}
			},
		},
		{
			name: "thread added",
			change: func(t *testing.T, app *App, projectID, _ string) {
				if _, err := app.CreateThread(t.Context(), CreateThreadOptions{ProjectID: projectID, Provider: "claude", Model: "claude-sonnet-4-6"}); err != nil {
					t.Fatalf("CreateThread: %v", err)
				}
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			app.stopSessionFn = func(string) error { return nil }
			p, err := app.CreateProject(t.TempDir())
			if err != nil {
				t.Fatalf("CreateProject: %v", err)
			}
			var ids []string
			for range 2 {
				thread, err := app.CreateThread(t.Context(), CreateThreadOptions{ProjectID: p.ID, Provider: "claude", Model: "claude-sonnet-4-6"})
				if err != nil {
					t.Fatalf("CreateThread: %v", err)
				}
				ids = append(ids, thread.ID)
			}
			held, kept := ids[0], ids[1]
			unlock := app.threadLocks().Lock(held)
			type outcome struct {
				result ProjectDeletionResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := app.DeleteProject(p.ID)
				done <- outcome{result, err}
			}()
			waitForThreadLockRefs(t, app.threadLocks(), held, 2)
			tc.change(t, app, p.ID, held)
			unlock()
			got := <-done

			if tc.wantErr {
				if got.err == nil || !strings.Contains(got.err.Error(), "contained threads changed during deletion") {
					t.Fatalf("DeleteProject() error = %v, want a retry", got.err)
				}
				if _, err := app.store.GetProject(p.ID); err != nil {
					t.Fatalf("project removed by a refused delete: %v", err)
				}
				return
			}
			if got.err != nil {
				t.Fatalf("DeleteProject() error = %v", got.err)
			}
			if len(got.result.ThreadIDs) != 1 || got.result.ThreadIDs[0] != kept {
				t.Fatalf("DeleteProject thread ids = %v, want [%s]", got.result.ThreadIDs, kept)
			}
			if _, err := app.store.GetProject(p.ID); err == nil {
				t.Fatal("project survived DeleteProject")
			}
		})
	}
}

func TestAppCreateThreadRequiresProjectID(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	_, err := app.CreateThread(t.Context(), CreateThreadOptions{})
	if err == nil {
		t.Fatal("CreateThread() error = nil, want projectId required")
	}
	if !strings.Contains(err.Error(), "projectId is required") {
		t.Fatalf("error = %v, want 'projectId is required'", err)
	}
}

func TestAppCreateThreadResolvesProvider(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	dir := t.TempDir()
	p, err := app.CreateProject(dir)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	thread, err := app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID: p.ID,
		// Provider intentionally empty; should fall back to settings default.
	})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	// Default provider from settings is "claude" (see settings.DefaultSettings).
	if thread.Provider != "claude" {
		t.Fatalf("Provider = %q, want claude (default)", thread.Provider)
	}
	if thread.WorkspacePath != p.Path {
		t.Fatalf("WorkspacePath = %q, want project path %q", thread.WorkspacePath, p.Path)
	}
}
