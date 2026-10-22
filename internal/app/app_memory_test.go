package app

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/memory"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
)

// Shared memory (docs/specs/memory.md, "Delivery"): an ordinary thread's
// sessions receive no memory while the setting is off. On, they receive the
// user and project indexes and may write both folders, with no maintenance
// warnings; turning it off again removes the hook and the folders, on every
// provider. A start with no hook forgets the thread's records.
func TestSharedMemoryFollowsItsSetting(t *testing.T) {
	t.Parallel()
	for _, providerName := range []string{string(provider.Claude), string(provider.ClaudeTUI), string(provider.Codex)} {
		t.Run(providerName, func(t *testing.T) {
			t.Parallel()
			app, _ := setupE2EApp(t)
			t.Cleanup(func() { _ = app.threadMCPServer().Close(); _ = app.workflowMCPServer().Close() })
			model := "claude-opus-4-7"
			if providerName == string(provider.Codex) {
				model = "gpt-5.4"
			}
			thread, err := createTestThread(t, app, providerName, t.TempDir(), model, "chat")
			if err != nil {
				t.Fatalf("CreateThread: %v", err)
			}
			record := filepath.Join(t.TempDir(), "record.txt")
			settings := map[string]any{"agentGuideEnabled": false}
			var start startSession
			if providerName == string(provider.Codex) {
				settings["codexBinaryPath"] = writeRecordingCodex(t, record)
				start = codexStarted(app, thread.ID, record)
				codexWritesTheWorkspace(t, app, thread)
			} else {
				settings["claudeBinaryPath"] = writeArgvRecordingClaude(t, record)
				start = claudeStarted(app, thread.ID, record)
			}
			if _, err := app.settings.Update(settings); err != nil {
				t.Fatal(err)
			}
			project, err := app.store.GetProject(thread.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			projectDir, err := memory.ProjectDir(app.configDir, app.credentialHomeOverride, project.Path)
			if err != nil {
				t.Fatal(err)
			}
			userDir := memory.UserDir(app.configDir)
			writeHomeFile(t, projectDir, "build.md", "---\ndescription: Build with make.\ntype: convention\nsource: user\nconfirmed: 2026-10-20\n---\n")
			writeHomeFile(t, projectDir, "broken.md", "no frontmatter\n")
			writeHomeFile(t, userDir, "terse.md", "---\ndescription: Prefers short answers.\ntype: preference\nsource: user\nconfirmed: 2026-10-20\n---\n")
			noMemory := func(label string) {
				t.Helper()
				started := start(t)
				if len(started.hooks) != 0 || slices.Contains(started.dirs, userDir) || slices.Contains(started.dirs, projectDir) {
					t.Fatalf("%s: hooks %q, dirs %q", label, started.hooks, started.dirs)
				}
			}

			noMemory("off by default")

			if _, err := app.settings.Update(map[string]any{"sharedMemoryEnabled": true}); err != nil {
				t.Fatal(err)
			}
			started := start(t)
			delivered := runMemoryHook(t, started.hooks)
			for _, want := range []string{
				"### Keeping memory\n",
				"### User memory (" + userDir + "/)\nPreferences:\n- Prefers short answers. (terse.md)",
				"### Project memory: " + project.Name + " (" + projectDir + "/)\nConventions:\n- Build with make. (build.md)",
			} {
				if !strings.Contains(delivered, want) {
					t.Fatalf("no %q in:\n%s", want, delivered)
				}
			}
			for _, unwanted := range []string{"### Your memory", "### Your standing instructions", "### Memory maintenance", "broken.md"} {
				if strings.Contains(delivered, unwanted) {
					t.Fatalf("an ordinary thread got %q:\n%s", unwanted, delivered)
				}
			}
			if !slices.Contains(started.dirs, userDir) || !slices.Contains(started.dirs, projectDir) || slices.Contains(started.dirs, project.Path) {
				t.Fatalf("writable folders = %q, want the two memory folders and not the workspace", started.dirs)
			}
			app.waitMemoryReports()
			if homeWarningRows(t, app, thread.ID) != 0 {
				t.Fatal("a malformed fact put a warning row on an ordinary thread")
			}

			seedMemoryRecords(t, app, thread.ID)
			if _, err := app.settings.Update(map[string]any{"sharedMemoryEnabled": false}); err != nil {
				t.Fatal(err)
			}
			noMemory("off again")
			requireMemoryRecordsForgotten(t, app, thread.ID, "a start with no hook")
		})
	}
}

// An assistant's helper, with shared memory on, receives the user index
// alone: the assistant's home and projects are the assistant's. Off, it
// receives nothing.
func TestAnAssistantsHelperReceivesTheUserMemory(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	owner, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatal(err)
	}
	row := testAssistant(t, app, owner.ID)
	helper := store.Thread{
		ID: entityid.New(), Title: "helper", Provider: string(provider.Claude), Model: "claude-opus-4-7",
		WorkspacePath: row.Home, HelperOfAgentID: row.ID,
	}
	if err := app.store.CreateThread(helper); err != nil {
		t.Fatal(err)
	}
	helper, err = app.store.GetThread(helper.ID)
	if err != nil || helper.HelperOfAgentID != row.ID {
		t.Fatalf("helper = %+v, %v", helper, err)
	}
	if _, ok, err := app.memoryPlan(helper, nil); ok || err != nil {
		t.Fatalf("with shared memory off: ok %v, %v", ok, err)
	}
	if _, err := app.settings.Update(map[string]any{"sharedMemoryEnabled": true}); err != nil {
		t.Fatal(err)
	}
	plan, ok, err := app.memoryPlan(helper, nil)
	if err != nil || !ok || plan.Home != "" || len(plan.Projects) != 0 || plan.User != memory.UserDir(app.configDir) {
		t.Fatalf("plan = %+v, %v, %v", plan, ok, err)
	}
	mem, err := app.readSessionMemory(helper, nil, false, false)
	if err != nil || mem.Hook == nil || !slices.Equal(mem.Dirs, []string{plan.User}) {
		t.Fatalf("session memory = %+v, %v", mem, err)
	}
	delivered, _ := memory.Build(plan, time.Now())
	if !strings.Contains(delivered, "### User memory") || strings.Contains(delivered, "### Project memory") || strings.Contains(delivered, "### Your memory") {
		t.Fatalf("delivered:\n%s", delivered)
	}
}

// The status reads the provider home the sessions use, and the variable as
// a Claude session gets it: the user's custom value over the app's own, and
// ~/.claude.json's env over both.
func TestNativeMemoryStatusReadsTheSessionsConfiguration(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	home := testProviderHome(t, app)
	writeHomeFile(t, home, ".codex/config.toml", "[features]\nmemories = true\n")
	writeHomeFile(t, home, ".claude/settings.json", `{"autoMemoryEnabled": false}`)
	status, err := app.GetNativeMemoryStatus()
	if err != nil || status.Claude || !status.Codex {
		t.Fatalf("status = %+v, %v", status, err)
	}
	if _, err := app.settings.SetProviderEnvVar(string(provider.Claude), claude.AutoMemoryOffEnv, "0", false); err != nil {
		t.Fatal(err)
	}
	if status, err := app.GetNativeMemoryStatus(); err != nil || !status.Claude {
		t.Fatalf("with the variable forcing it on: %+v, %v", status, err)
	}
	writeHomeFile(t, home, ".claude.json", `{"env": {"CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1"}}`)
	if status, err := app.GetNativeMemoryStatus(); err != nil || status.Claude {
		t.Fatalf("with ~/.claude.json turning it off: %+v, %v", status, err)
	}
}

// seedMemoryRecords puts a record in threadID's records folder, as a run of
// the memory hook would. The app must have a config directory.
func seedMemoryRecords(t *testing.T, app *App, threadID string) {
	t.Helper()
	if app.configDir == "" {
		t.Fatal("the app has no config directory to keep records in")
	}
	dir, err := memory.RecordsDir(app.configDir, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.PrepareRecords(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "seeded.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// requireMemoryRecordsForgotten fails unless threadID's records folder is
// gone, after what label names.
func requireMemoryRecordsForgotten(t *testing.T, app *App, threadID, label string) {
	t.Helper()
	dir, err := memory.RecordsDir(app.configDir, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s kept the memory records of %s: %v", label, threadID, err)
	}
}

// Deleting a thread deletes the records of what its sessions were shown.
func TestDeletingAThreadForgetsItsMemoryRecords(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatal(err)
	}
	seedMemoryRecords(t, app, thread.ID)
	if err := app.DeleteThread(thread.ID); err != nil {
		t.Fatal(err)
	}
	requireMemoryRecordsForgotten(t, app, thread.ID, "the delete")
}

// Deleting an empty draft thread deletes the records a session started for
// it left.
func TestDeletingAnEmptyDraftForgetsItsMemoryRecords(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatal(err)
	}
	seedMemoryRecords(t, app, thread.ID)
	if removed, err := app.DeleteEmptyDraftThread(thread.ID); err != nil || !removed {
		t.Fatalf("DeleteEmptyDraftThread = %v, %v; want the draft removed", removed, err)
	}
	requireMemoryRecordsForgotten(t, app, thread.ID, "the draft's delete")
}
