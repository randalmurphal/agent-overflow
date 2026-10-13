package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// A standing agent's home (docs/specs/standing-agents.md, "Standing agent"):
// its sessions carry the home block whatever the guide setting says, a
// demoted thread's next session does not, and the home is deleted only by
// the demotion that asks for it.

// TestStartSession_StandingAgentCarriesItsHome starts a promoted Claude
// thread with the app guide off, so the append file holds the home block
// alone, then demotes it and checks the next session carries nothing.
func TestStartSession_StandingAgentCarriesItsHome(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	binary := writeArgvRecordingClaude(t, argvPath)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary, "agentGuideEnabled": false}); err != nil {
		t.Fatalf("settings: %v", err)
	}
	row, err := app.PromoteStandingAgent(thread.ID)
	if err != nil {
		t.Fatalf("PromoteStandingAgent: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	argv := spawnedClaudeArgv(t, argvPath)
	appendPath, found := argValue(argv, "--append-system-prompt-file")
	if !found {
		t.Fatalf("a standing agent's argv has no --append-system-prompt-file: %v", argv)
	}
	content, err := os.ReadFile(appendPath)
	if err != nil {
		t.Fatalf("read the append file: %v", err)
	}
	if want := standingAgentPromptText(row); string(content) != want {
		t.Errorf("append file:\n%s\nwant:\n%s", content, want)
	}
	if !strings.Contains(string(content), row.Home) || !strings.Contains(string(content), thread.ID) {
		t.Errorf("append file does not name the home %s and thread %s:\n%s", row.Home, thread.ID, content)
	}
	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	if err := app.DemoteStandingAgent(thread.ID); err != nil {
		t.Fatalf("DemoteStandingAgent: %v", err)
	}
	if err := os.Remove(argvPath); err != nil {
		t.Fatalf("reset argv: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession after demotion: %v", err)
	}
	argv = spawnedClaudeArgv(t, argvPath)
	if _, found := argValue(argv, "--append-system-prompt-file"); found {
		t.Errorf("a demoted thread's session still carries an append file: %v", argv)
	}
	if _, err := os.Stat(filepath.Join(row.Home, "README.md")); err != nil {
		t.Errorf("demotion must leave the home: %v", err)
	}
}

func TestCodexDeveloperInstructionsCarryTheHomeBlock(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "agent")
	row, err := f.app.PromoteStandingAgent(thread.ID)
	if err != nil {
		t.Fatalf("PromoteStandingAgent: %v", err)
	}
	block, err := f.app.standingAgentPrompt(thread)
	if err != nil || block != standingAgentPromptText(row) {
		t.Fatalf("standingAgentPrompt = %q, %v", block, err)
	}
	// What spawnProviderSession hands Codex with the guide off.
	if got := f.app.codexDeveloperInstructions(thread.ID, joinPromptParts("", block), nil); got != block {
		t.Errorf("developer instructions = %q, want the home block alone", got)
	}
	if got := joinPromptParts("GUIDE", block); got != "GUIDE\n\n"+block {
		t.Errorf("joined = %q", got)
	}
}

func TestDemoteStandingAgentAndDeleteHome(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "agent")
	row, err := f.app.PromoteStandingAgent(thread.ID)
	if err != nil {
		t.Fatalf("PromoteStandingAgent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(row.Home, "notes.md"), []byte("kept by the agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(filepath.Dir(row.Home), "other-agent")
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}

	// The plain demotion keeps the home, and a re-promotion reuses it.
	if err := f.app.DemoteStandingAgent(thread.ID); err != nil {
		t.Fatalf("DemoteStandingAgent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(row.Home, "notes.md")); err != nil {
		t.Fatalf("plain demotion removed the home: %v", err)
	}
	if again, err := f.app.PromoteStandingAgent(thread.ID); err != nil || again.Home != row.Home {
		t.Fatalf("re-promotion = %+v, %v", again, err)
	}

	if err := f.app.DemoteStandingAgentAndDeleteHome(thread.ID); err != nil {
		t.Fatalf("DemoteStandingAgentAndDeleteHome: %v", err)
	}
	if _, found, _ := f.app.store.GetStandingAgent(thread.ID); found {
		t.Fatal("the agent row survived")
	}
	if _, err := os.Stat(row.Home); !os.IsNotExist(err) {
		t.Fatalf("home still there: %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("another agent's home was touched: %v", err)
	}
	// Not an agent any more: nothing to delete, nothing refused.
	if err := f.app.DemoteStandingAgentAndDeleteHome(thread.ID); err != nil {
		t.Fatalf("second call: %v", err)
	}
}

// A recorded home that is not exactly `<config>/agents/<thread-id>` is never
// deleted: the demotion lands, the error names the recorded path, and every
// file the path could reach survives.
func TestDemoteStandingAgentAndDeleteHomeRefusesAnyOtherPath(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	agents := filepath.Join(f.app.configDir, "agents")
	otherProfile := t.TempDir()
	for name, homeFor := range map[string]func(id string) string{
		"sibling":               func(id string) string { return filepath.Join(agents, id+"-x") },
		"another agent":         func(string) string { return filepath.Join(agents, "11111111-2222-3333-4444-555555555555") },
		"escape through parent": func(id string) string { return filepath.Join(agents, id) + "/../victim" },
		"agents root":           func(string) string { return agents },
		"relative":              func(id string) string { return filepath.Join("agents", id) },
		"another profile":       func(id string) string { return filepath.Join(otherProfile, "agents", id) },
		"config dir":            func(string) string { return f.app.configDir },
	} {
		t.Run(name, func(t *testing.T) {
			thread := f.thread(t, "agent "+name)
			home := homeFor(thread.ID)
			// Markers in every place the path or a lax check could reach.
			markers := []string{
				filepath.Join(agents, thread.ID, "keep"),
				filepath.Join(agents, "victim", "keep"),
				filepath.Join(filepath.Clean(home), "keep"),
			}
			if filepath.IsAbs(home) {
				markers = append(markers, filepath.Join(home, "keep"))
			}
			for _, marker := range markers {
				if !filepath.IsAbs(marker) {
					continue
				}
				if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(marker, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.app.store.PromoteStandingAgent(thread.ID, home, 1); err != nil {
				t.Fatalf("seed the row: %v", err)
			}
			err := f.app.DemoteStandingAgentAndDeleteHome(thread.ID)
			if err == nil || !strings.Contains(err.Error(), home) || !strings.Contains(err.Error(), "was not deleted") {
				t.Fatalf("err = %v, want a refusal naming %s", err, home)
			}
			for _, marker := range markers {
				if !filepath.IsAbs(marker) {
					continue
				}
				if _, statErr := os.Stat(marker); statErr != nil {
					t.Fatalf("%s was deleted: %v", marker, statErr)
				}
			}
			if _, found, _ := f.app.store.GetStandingAgent(thread.ID); found {
				t.Fatal("the demotion must land even when the deletion is refused")
			}
		})
	}
}

// A row that recorded no home is refused rather than read as "nothing to
// delete": the user asked for a deletion and must hear that none happened.
func TestDemoteStandingAgentAndDeleteHomeRefusesAnEmptyHome(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "agent")
	if _, err := f.app.store.PromoteStandingAgent(thread.ID, "", 1); err != nil {
		t.Fatalf("seed the row: %v", err)
	}
	err := f.app.DemoteStandingAgentAndDeleteHome(thread.ID)
	if err == nil || !strings.Contains(err.Error(), "records no home") {
		t.Fatalf("err = %v, want the empty home refused", err)
	}
	if _, found, _ := f.app.store.GetStandingAgent(thread.ID); found {
		t.Fatal("the demotion must land")
	}
}
