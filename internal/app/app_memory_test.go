package app

import (
	"agent-overflow/internal/provider/providertest"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/memory"
	"agent-overflow/internal/procutil"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/threadtools"
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
			launch := map[string]any{"agentGuideEnabled": false}
			var start startSession
			if providerName == string(provider.Codex) {
				launch["codexBinaryPath"] = writeRecordingCodex(t, record)
				start = codexStarted(app, thread.ID, record)
				codexWritesTheWorkspace(t, app, thread)
			} else {
				launch["claudeBinaryPath"] = writeArgvRecordingClaude(t, record)
				start = claudeStarted(app, thread.ID, record)
			}
			if _, err := app.settings.Update(launch); err != nil {
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
			writeHomeFile(t, projectDir, "build.md", "---\ndescription: Build with make.\ntype: convention\nsource: user: said so\nconfirmed: 2026-10-20\n---\n")
			writeHomeFile(t, projectDir, "broken.md", "no frontmatter\n")
			writeHomeFile(t, userDir, "terse.md", "---\ndescription: Prefers short answers.\ntype: preference\nsource: user: said so\nconfirmed: 2026-10-20\n---\n")
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

// With shared memory off, the threads that work for an assistant receive
// memory as any thread does with it on: a helper the user index alone, a
// worker the assistant spawned and that worker's own worker their
// project's index and the user's, none of them the assistant's home or
// warnings, whether the worker's row is owned or in the sidebar, from its
// spawn or moved there later. A thread no assistant spawned receives none.
func TestAnAssistantsSubThreadsReceiveMemory(t *testing.T) {
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
	thread := func() store.Thread {
		t.Helper()
		created, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	worker, below, unowned, shown, moved := thread(), thread(), thread(), thread(), thread()
	ownSpawn(t, app, "spawn-worker", owner.ID, worker.ID)
	ownSpawn(t, app, "spawn-below", worker.ID, below.ID)
	sidebarSpawn(t, app, "spawn-shown", owner.ID, shown.ID)
	ownSpawn(t, app, "spawn-moved", owner.ID, moved.ID)
	if _, err := app.store.SetThreadPlacement([]string{moved.ID}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	userDir := memory.UserDir(app.configDir)
	for name, sub := range map[string]store.Thread{
		"helper": helper, "depth one": worker, "depth two": below, "sidebar from its spawn": shown, "moved to the sidebar": moved,
	} {
		sub, err := app.store.GetThread(sub.ID)
		if err != nil {
			t.Fatal(err)
		}
		plan, ok, err := app.memoryPlan(app.currentSettings(), sub, nil)
		if err != nil || !ok || plan.Home != "" || plan.User != userDir {
			t.Fatalf("%s: plan = %+v, %v, %v", name, plan, ok, err)
		}
		var projects []string
		for _, project := range plan.Projects {
			projects = append(projects, project.Path)
		}
		var want []string
		if sub.ProjectPath != "" {
			want = []string{sub.ProjectPath}
		}
		if !slices.Equal(projects, want) {
			t.Fatalf("%s: projects %q, want %q", name, projects, want)
		}
		mem, err := app.readSessionMemory(sub, nil, false, false)
		if err != nil || mem.Hook == nil || !slices.Contains(mem.Dirs, userDir) {
			t.Fatalf("%s: session memory = %+v, %v", name, mem, err)
		}
		delivered := builtMemory(t, plan)
		if !strings.Contains(delivered, "### User memory") || strings.Contains(delivered, "### Your memory") || strings.Contains(delivered, "### Memory maintenance") ||
			strings.Contains(delivered, "### Project memory") != (len(want) == 1) {
			t.Fatalf("%s: delivered:\n%s", name, delivered)
		}
	}
	if _, ok, err := app.memoryPlan(app.currentSettings(), unowned, nil); ok || err != nil {
		t.Fatalf("a thread no assistant spawned: ok %v, %v", ok, err)
	}
}

// Each session decides the provider's own memory from its provider's
// setting and whether it receives Agent Overflow memory: by default an
// assistant, its sub-threads, a read-only one among them, and every thread
// while shared memory is on run without it; "always" turns it off
// everywhere and "never" nowhere. The other provider's setting plays no
// part.
func TestOwnMemoryFollowsItsSettingPerSession(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	for _, providerName := range []string{string(provider.Claude), string(provider.ClaudeTUI), string(provider.Codex)} {
		key, other, model := "claudeOwnMemoryOff", "codexOwnMemoryOff", "claude-opus-4-7"
		if providerName == string(provider.Codex) {
			key, other, model = other, key, "gpt-5.4"
		}
		thread := func() store.Thread {
			t.Helper()
			created, err := createTestThread(t, app, providerName, t.TempDir(), model, "chat")
			if err != nil {
				t.Fatal(err)
			}
			return created
		}
		owner, worker, ordinary := thread(), thread(), thread()
		// An assistant's name is unique.
		owner.Title = "Assistant on " + providerName
		if err := app.store.UpdateThread(owner); err != nil {
			t.Fatal(err)
		}
		row := testAssistant(t, app, owner.ID)
		ownSpawn(t, app, "spawn-"+providerName, owner.ID, worker.ID)
		for _, when := range []string{settings.OwnMemoryOffWhereAgentOverflowMemory, settings.OwnMemoryOffAlways, settings.OwnMemoryOffNever} {
			// The other provider's setting says the opposite.
			otherWhen := settings.OwnMemoryOffAlways
			if when == settings.OwnMemoryOffAlways {
				otherWhen = settings.OwnMemoryOffNever
			}
			for _, tc := range []struct {
				name     string
				thread   store.Thread
				agent    *store.StandingAgent
				readOnly bool
				shared   bool
				receives bool
			}{
				{"assistant", owner, &row, false, false, true},
				{"sub-thread", worker, nil, false, false, true},
				{"read-only sub-thread", worker, nil, true, false, true},
				{"ordinary, shared memory off", ordinary, nil, false, false, false},
				{"ordinary, shared memory on", ordinary, nil, false, true, true},
			} {
				if _, err := app.settings.Update(map[string]any{key: when, other: otherWhen, "sharedMemoryEnabled": tc.shared}); err != nil {
					t.Fatal(err)
				}
				mem, err := app.readSessionMemory(tc.thread, tc.agent, false, tc.readOnly)
				if err != nil {
					t.Fatal(err)
				}
				want := when == settings.OwnMemoryOffAlways || when == settings.OwnMemoryOffWhereAgentOverflowMemory && tc.receives
				if mem.OwnMemoryOff != want || (mem.Hook != nil) != tc.receives {
					t.Errorf("%s, %s %q, %s: own memory off %v with hook %v, want %v", providerName, key, when, tc.name, mem.OwnMemoryOff, mem.Hook != nil, want)
				}
			}
		}
	}
}

// A Codex fork a thread tool makes works for its caller: by default a
// spawn from a thread and an ask copy an assistant makes run without
// Codex's own memory, as their starts do, and the same calls from an
// ordinary thread leave it on.
func TestThreadToolForksDecideOwnMemoryFromTheirCaller(t *testing.T) {
	f := newRequestFixture(t)
	t.Cleanup(func() { _ = f.app.threadMCPServer().Close() })
	requests := filepath.Join(t.TempDir(), "codex.jsonl")
	inner := testutil.WriteMockCodexSession(t, t.TempDir(), map[string]string{
		`"method":"initialize"`:    `{"jsonrpc":"2.0","id":%s,"result":` + providertest.CodexInitializeResult + `}`,
		`"method":"thread/start"`:  `{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"codex-memory-fork"}}}`,
		`"method":"thread/resume"`: `{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"codex-memory-fork"}}}`,
		`"method":"thread/fork"`:   `{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"codex-memory-fork"}}}`,
		`"method":"config/read"`:   `{"jsonrpc":"2.0","id":%s,"result":{"config":{},"origins":{}}}`,
		`"method":"turn/start"`:    `{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn-1","status":"inProgress","items":[]}}}`,
		"":                         `{"jsonrpc":"2.0","id":%s,"result":{}}`,
	})
	binary := mockexec.Write(t, filepath.Join(t.TempDir(), "codex.sh"),
		"#!/bin/bash\nif [ \"$1\" = --version ]; then echo 'codex-cli 0.160.0'; exit 0; fi\ntee -a '"+requests+"' | '"+inner+"'\n")
	// The Codex catalog the app ships, in place of the live one tests turn off.
	f.app.providerDiscoveryCaches.CodexModels = codexmodels.NewWith(time.Minute, func(context.Context, codexmodels.Launch, procutil.Reaper) ([]provider.ModelInfo, error) {
		return provider.CodexModels, nil
	}, time.Now)
	if _, err := f.app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	source, err := createTestThread(t, f.app, string(provider.Codex), t.TempDir(), "gpt-5.6-sol", "chat")
	if err != nil {
		t.Fatal(err)
	}
	source.SessionRef = "codex-memory-source"
	if err := f.app.store.UpdateThread(source); err != nil {
		t.Fatal(err)
	}
	insertForkTestItems(t, f.app.store, source.ID)
	assistant, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatal(err)
	}
	testAssistant(t, f.app, assistant.ID)
	if _, found, err := f.app.store.SetAgentProjects(assistant.ID, true, nil); err != nil || !found {
		t.Fatalf("serve every project: %v %v", found, err)
	}
	adapter := f.adapter()
	for _, caller := range []struct {
		name string
		who  threadtools.Caller
		off  bool
	}{
		{"an assistant", threadtools.Caller{ThreadID: assistant.ID}, true},
		{"an ordinary thread", f.callerIdentity(), false},
	} {
		for kind, call := range map[string]func() error{
			"spawn": func() error {
				_, err := adapter.Spawn(t.Context(), caller.who, threadtools.SpawnCall{Prompt: "work", FromThread: source.ID})
				return err
			},
			"ask": func() error {
				_, err := adapter.Ask(t.Context(), caller.who, threadtools.AskCall{ThreadID: source.ID, Question: "why?"})
				return err
			},
		} {
			if err := os.Remove(requests); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
			if err := call(); err != nil {
				t.Fatalf("%s's %s: %v", caller.name, kind, err)
			}
			raw, err := os.ReadFile(requests)
			if err != nil {
				t.Fatal(err)
			}
			var fork string
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.Contains(line, `"method":"thread/fork"`) {
					fork = line
				}
			}
			if off := strings.Contains(fork, `"features.memories":false`) && strings.Contains(fork, `"memories.generate_memories":false`); fork == "" || off != caller.off {
				t.Errorf("%s's %s: thread/fork turns Codex's memory off = %v, want %v: %q", caller.name, kind, off, caller.off, fork)
			}
		}
	}
}

// sidebarSpawn records that caller spawned worker with a sidebar row.
func sidebarSpawn(t *testing.T, app *App, token, caller, worker string) {
	t.Helper()
	if _, err := app.store.AcceptLocalThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: store.ThreadRequestSpawn, State: store.ThreadRequestUnconfirmed,
	}, store.ThreadRequestReceipt{Token: token, OwnerDeviceID: threadReceiptLocalOwner, Kind: store.ThreadRequestSpawn, SourceThreadID: caller}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.NameLocalThreadRequestTarget(token, worker, false); err != nil {
		t.Fatal(err)
	}
}

// builtMemory is plan's whole delivery as one text, its parts in order.
func builtMemory(t *testing.T, plan memory.Plan) string {
	t.Helper()
	return builtMemoryText(plan)
}

// The status reads the provider home the sessions use, and the variable as
// a Claude session gets it: the user's custom value over the app's own, and
// ~/.claude.json's env over both. A provider whose configuration cannot be
// read says so without taking the other's answer with it.
func TestNativeMemoryStatusReadsTheSessionsConfiguration(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	home := testProviderHome(t, app)
	writeHomeFile(t, home, ".codex/config.toml", "[features]\nmemories = true\n")
	writeHomeFile(t, home, ".claude/settings.json", `{"autoMemoryEnabled": false}`)
	status, err := app.GetNativeMemoryStatus()
	if err != nil || status.Claude != (ProviderMemory{}) || status.Codex != (ProviderMemory{On: true}) {
		t.Fatalf("status = %+v, %v", status, err)
	}
	if _, err := app.settings.SetProviderEnvVar(string(provider.Claude), claude.AutoMemoryOffEnv, "0", false); err != nil {
		t.Fatal(err)
	}
	if status, err := app.GetNativeMemoryStatus(); err != nil || status.Claude != (ProviderMemory{On: true}) {
		t.Fatalf("with the variable forcing it on: %+v, %v", status, err)
	}
	writeHomeFile(t, home, ".claude.json", `{"env": {"CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1"}}`)
	if status, err := app.GetNativeMemoryStatus(); err != nil || status.Claude != (ProviderMemory{}) {
		t.Fatalf("with ~/.claude.json turning it off: %+v, %v", status, err)
	}
	if err := os.Remove(filepath.Join(home, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, home, ".codex/config.toml", "[features\n")
	status, err = app.GetNativeMemoryStatus()
	if err != nil || status.Claude != (ProviderMemory{On: true}) || status.Codex.On || !strings.Contains(status.Codex.Error, "config.toml") {
		t.Fatalf("with Codex's config unreadable: %+v, %v", status, err)
	}
	writeHomeFile(t, home, ".codex/config.toml", "[features]\nmemories = true\n")
	writeHomeFile(t, home, ".claude/settings.json", "{\n  \"autoMemoryEnabled\": yes\n}\n")
	status, err = app.GetNativeMemoryStatus()
	want := "Claude's settings " + filepath.Join(home, ".claude", "settings.json") + " could not be parsed at line 2."
	if err != nil || status.Codex != (ProviderMemory{On: true}) || status.Claude != (ProviderMemory{Error: want}) {
		t.Fatalf("with Claude's settings unreadable: %+v, %v", status, err)
	}
}

// A config the parser refuses can quote the value it stopped at, from any
// field, and the status reaches every screen allowed to read settings. The
// answer names the file and the line; the value stays on this computer.
func TestNativeMemoryStatusNeverCarriesTheConfigsText(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	home := testProviderHome(t, app)
	const secret = "sentinelpassword"
	writeHomeFile(t, home, ".codex/config.toml", "[features]\nmemories = true\n\n[mcp_servers.db.env]\nPASSWORD = "+secret+"\n")
	status, err := app.GetNativeMemoryStatus()
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), secret) {
		t.Fatalf("the answer carries the config's text: %s", wire)
	}
	want := "Codex's config " + filepath.Join(home, ".codex", "config.toml") + " could not be parsed at line 5."
	if status.Codex != (ProviderMemory{Error: want}) {
		t.Fatalf("Codex = %+v, want the error %q", status.Codex, want)
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

// A session that starts without its hook because the hook could not be
// built, here because the plan file cannot be written, forgets the
// thread's records as a session that receives no memory does: a compaction
// in it removes what they say the model holds.
func TestASessionWhoseHookCannotBeBuiltForgetsTheRecords(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.settings.Update(map[string]any{"sharedMemoryEnabled": true}); err != nil {
		t.Fatal(err)
	}
	seedMemoryRecords(t, app, thread.ID)
	if err := os.WriteFile(filepath.Join(app.configDir, "memory-plans"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	mem, err := app.readSessionMemory(thread, nil, true, false)
	if err != nil || mem.Hook != nil || !slices.ContainsFunc(mem.Problems, func(p memoryProblem) bool { return p.cost == memoryUndelivered }) {
		t.Fatalf("session memory = %+v, %v; want no hook and the undelivered problem", mem, err)
	}
	requireMemoryRecordsForgotten(t, app, thread.ID, "a start whose hook could not be built")
}

// seedMemoryPlan writes threadID's plan file, as a start of its session
// does, and returns its path.
func seedMemoryPlan(t *testing.T, app *App, threadID string) string {
	t.Helper()
	path, err := memory.PlanFile(app.configDir, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.WritePlan(path, memory.Plan{User: memory.UserDir(app.configDir)}); err != nil {
		t.Fatal(err)
	}
	return path
}

// Deleting a thread deletes the records of what its sessions were shown,
// and the plan file their hooks read.
func TestDeletingAThreadForgetsItsMemoryRecords(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatal(err)
	}
	seedMemoryRecords(t, app, thread.ID)
	plan := seedMemoryPlan(t, app, thread.ID)
	if err := app.deleteThread(thread.ID); err != nil {
		t.Fatal(err)
	}
	requireMemoryRecordsForgotten(t, app, thread.ID, "the delete")
	if _, err := os.Stat(plan); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the delete kept the memory plan: %v", err)
	}
}

// Deleting an empty draft thread deletes the records and the plan file a
// session started for it left.
func TestDeletingAnEmptyDraftForgetsItsMemoryRecords(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatal(err)
	}
	seedMemoryRecords(t, app, thread.ID)
	plan := seedMemoryPlan(t, app, thread.ID)
	if removed, err := app.DeleteEmptyDraftThread(thread.ID); err != nil || !removed {
		t.Fatalf("DeleteEmptyDraftThread = %v, %v; want the draft removed", removed, err)
	}
	requireMemoryRecordsForgotten(t, app, thread.ID, "the draft's delete")
	if _, err := os.Stat(plan); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the draft's delete kept the memory plan: %v", err)
	}
}
