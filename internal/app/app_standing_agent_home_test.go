package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// An assistant's home (docs/specs/standing-agents.md, "Standing agent"): its
// sessions carry the home block whatever the guide setting says, any other
// thread's do not, and deleting the assistant removes the home, and only
// that folder.

// standingAgentIdentity is the identity sentence a session of the
// assistant's current thread opens with.
func standingAgentIdentity(name, threadID, home string) string {
	return agenthome.IdentitySentence(agenthome.Identity{Name: name, AgentID: threadID, ThreadID: threadID, Home: home})
}

// TestStartSession_StandingAgentCarriesItsHome starts an assistant's Claude
// thread with the app guide off, so the append file holds the home block
// alone, then starts an ordinary thread, whose session carries nothing.
func TestStartSession_StandingAgentCarriesItsHome(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	plain, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	binary := writeArgvRecordingClaude(t, argvPath)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary, "agentGuideEnabled": false}); err != nil {
		t.Fatalf("settings: %v", err)
	}
	row := testAssistant(t, app, thread.ID)

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	argv := spawnedClaudeArgv(t, argvPath)
	appendPath, found := argValue(argv, "--append-system-prompt-file")
	if !found {
		t.Fatalf("an assistant's argv has no --append-system-prompt-file: %v", argv)
	}
	content, err := os.ReadFile(appendPath)
	if err != nil {
		t.Fatalf("read the append file: %v", err)
	}
	if want := standingAgentIdentity(thread.Title, thread.ID, row.Home); !strings.HasPrefix(string(content), want) {
		t.Errorf("append file:\n%s\nwant it to open with:\n%s", content, want)
	}
	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	if err := os.Remove(argvPath); err != nil {
		t.Fatalf("reset argv: %v", err)
	}
	if err := app.StartSession(plain.ID); err != nil {
		t.Fatalf("StartSession of an ordinary thread: %v", err)
	}
	argv = spawnedClaudeArgv(t, argvPath)
	if _, found := argValue(argv, "--append-system-prompt-file"); found {
		t.Errorf("an ordinary thread's session carries an append file: %v", argv)
	}
	if settings, _ := argValue(argv, "--settings"); strings.Contains(settings, `"hooks"`) {
		t.Errorf("an ordinary thread's session registers hooks: %s", settings)
	}
}

func TestCodexDeveloperInstructionsCarryTheHomeBlock(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "agent")
	row := testAssistant(t, f.app, thread.ID)
	agent, err := f.app.readStandingAgentSession(thread)
	block := agent.Block
	if err != nil || len(agent.Problems) > 0 || !strings.HasPrefix(block, standingAgentIdentity(thread.Title, thread.ID, row.Home)) {
		t.Fatalf("readStandingAgentSession = %+v, %v", agent, err)
	}
	// What spawnProviderSession hands Codex with the guide off.
	if got := f.app.codexDeveloperInstructions(thread.ID, joinPromptParts("", block), nil); got != block {
		t.Errorf("developer instructions = %q, want the home block alone", got)
	}
	if got := joinPromptParts("GUIDE", block); got != "GUIDE\n\n"+block {
		t.Errorf("joined = %q", got)
	}
}

// Deleting an assistant's current thread deletes the assistant and its
// home, and nothing beside it; deleting a past thread touches neither.
func TestDeletingAnAssistantRemovesItsHome(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "agent")
	row := testAssistant(t, f.app, thread.ID)
	if err := os.WriteFile(filepath.Join(row.Home, "notes.md"), []byte("kept by the agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(filepath.Dir(row.Home), "other-agent")
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	successor := f.thread(t, "agent next")
	if _, err := f.app.store.HandOffAssistant(thread.ID, successor.ID, 1); err != nil {
		t.Fatal(err)
	}

	if err := f.app.DeleteThread(thread.ID); err != nil {
		t.Fatalf("delete the past thread: %v", err)
	}
	if _, found, _ := f.app.store.GetStandingAgent(successor.ID); !found {
		t.Fatal("deleting a past thread deleted the assistant")
	}
	if _, err := os.Stat(filepath.Join(row.Home, "notes.md")); err != nil {
		t.Fatalf("deleting a past thread touched the home: %v", err)
	}

	if err := f.app.DeleteThread(successor.ID); err != nil {
		t.Fatalf("delete the current thread: %v", err)
	}
	if _, found, _ := f.app.store.GetAgent(row.ID); found {
		t.Fatal("the assistant survived its current thread's delete")
	}
	if _, err := os.Stat(row.Home); !os.IsNotExist(err) {
		t.Fatalf("home still there: %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("another assistant's home was touched: %v", err)
	}
}

// A recorded home that is not exactly `<config>/agents/<agent-id>` refuses
// the delete: the thread and the record stay, the error names the recorded
// path, and every file the path could reach survives.
func TestDeletingAnAssistantRefusesAnyOtherHomePath(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	agents := filepath.Join(f.app.configDir, "agents")
	otherProfile := t.TempDir()
	for name, homeFor := range map[string]func(id string) string{
		"empty":                 func(string) string { return "" },
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
			markers := []string{
				filepath.Join(agents, thread.ID, "keep"),
				filepath.Join(agents, "victim", "keep"),
			}
			if home != "" {
				markers = append(markers, filepath.Join(filepath.Clean(home), "keep"))
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
			if err := f.app.store.CreateStandingAgent(storeAgent(thread.ID, home)); err != nil {
				t.Fatalf("seed the record: %v", err)
			}
			err := f.app.DeleteThread(thread.ID)
			if err == nil || !strings.Contains(err.Error(), "this assistant was not deleted") || !strings.Contains(err.Error(), "(expected "+filepath.Join(agents, thread.ID)+")") {
				t.Fatalf("err = %v, want a refusal naming the expected folder", err)
			}
			if home != "" && !strings.Contains(err.Error(), home) {
				t.Fatalf("err = %v, want the recorded home %s named", err, home)
			}
			// The toast line is the innermost layer, and names the folder.
			chain := errorsx.Chain(err)
			if line := chain[len(chain)-1]; !strings.HasPrefix(line, "this assistant was not deleted: ") || (home != "" && !strings.Contains(line, home)) {
				t.Fatalf("toast line = %q", line)
			}
			for _, marker := range markers {
				if !filepath.IsAbs(marker) {
					continue
				}
				if _, statErr := os.Stat(marker); statErr != nil {
					t.Fatalf("%s was deleted: %v", marker, statErr)
				}
			}
			if _, err := f.app.store.GetThread(thread.ID); err != nil {
				t.Fatalf("the refused delete removed the thread: %v", err)
			}
			if _, found, _ := f.app.store.GetStandingAgent(thread.ID); !found {
				t.Fatal("the refused delete removed the record")
			}
		})
	}
}

// A home that cannot be removed leaves the delete unfinished, the record
// with it, so the next attempt removes the home; the toast line names the
// home and the layer around it names where the removal stopped.
func TestDeletingAnAssistantWhoseHomeCannotBeRemovedFinishesLater(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root removes an unreadable folder")
	}
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "agent")
	row := testAssistant(t, f.app, thread.ID)
	locked := filepath.Join(row.Home, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	err := f.app.DeleteThread(thread.ID)
	chain := errorsx.Chain(err)
	if len(chain) < 2 {
		t.Fatalf("err = %v, chain = %q", err, chain)
	}
	if got := chain[len(chain)-1]; got != "the assistant's home folder "+row.Home+" could not be removed, so the delete is not finished: permission denied" {
		t.Errorf("innermost layer = %q", got)
	}
	if !strings.Contains(err.Error(), locked) {
		t.Errorf("err %q does not name %s", err, locked)
	}
	if _, found, _ := f.app.store.GetStandingAgent(thread.ID); !found {
		t.Fatal("the unfinished delete dropped the record")
	}
	if pending, err := f.app.store.ListPendingThreadDeletes(); err != nil || len(pending) != 1 || pending[0] != thread.ID {
		t.Fatalf("pending deletes = %q, %v", pending, err)
	}

	if err := os.Chmod(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := f.app.deleteThreadLocked(thread.ID); err != nil {
		t.Fatalf("finish the delete: %v", err)
	}
	if _, err := os.Stat(row.Home); !os.IsNotExist(err) {
		t.Fatalf("home still there after the delete finished: %v", err)
	}
	if _, found, _ := f.app.store.GetAgent(row.ID); found {
		t.Fatal("the record survived the finished delete")
	}
}

// An assistant's record leaving this computer drops the role line the app
// cached for its home, whichever writer deletes the record: deleting its
// thread, or committing its move, which keeps the home folder here. A home
// file of the same length and time read afterwards is read afresh rather
// than answered from the old entry.
func TestAnAssistantRecordDeletedForgetsItsCachedRole(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"delete", "move"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			f := newThreadToolsFixture(t)
			thread := f.thread(t, "agent")
			row := testAssistant(t, f.app, thread.ID)
			file := filepath.Join(row.Home, agenthome.InstructionsFile)
			old := time.Unix(1_700_000_000, 0)
			write := func(text string) {
				t.Helper()
				if err := os.MkdirAll(row.Home, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(file, old, old); err != nil {
					t.Fatal(err)
				}
			}
			write("Reviewer\n")
			if role, err := f.app.assistantRoles().Role(row.Home); err != nil || role != "Reviewer" {
				t.Fatalf("role = %q, %v", role, err)
			}
			switch how {
			case "delete":
				if err := f.app.DeleteThread(thread.ID); err != nil {
					t.Fatal(err)
				}
			case "move":
				move, err := f.app.store.CreateThreadTransfer(store.ThreadTransfer{ID: entityid.New(), ThreadID: thread.ID, PeerBackendID: entityid.New(),
					Kind: "move", Direction: "outgoing", ActivationHash: strings.Repeat("a", 64), PrivateState: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				for _, phase := range []string{"prepared", "committed"} {
					if _, err := f.app.store.AdvanceThreadTransfer(move.ID, phase, strings.Repeat("b", 64)); err != nil {
						t.Fatalf("advance %s: %v", phase, err)
					}
				}
				if _, found, err := f.app.store.GetStandingAgent(thread.ID); err != nil || found {
					t.Fatalf("the committed move kept the assistant: %v %v", found, err)
				}
			}
			write("Releaser\n")
			if role, err := f.app.assistantRoles().Role(row.Home); err != nil || role != "Releaser" {
				t.Fatalf("role after the record went = %q, %v; want the new file's", role, err)
			}
		})
	}
}

// codexHookMatchers returns the SessionStart matcher of every logged request
// for method, "" for a request that registers no hooks.
func codexHookMatchers(t *testing.T, requestLog, method string) []string {
	t.Helper()
	data, err := os.ReadFile(requestLog)
	if err != nil {
		t.Fatalf("read request log: %v", err)
	}
	var matchers []string
	for _, line := range strings.Split(string(data), "\n") {
		var req struct {
			Method string `json:"method"`
			Params struct {
				Config struct {
					Hooks struct {
						SessionStart []struct {
							Matcher string `json:"matcher"`
						} `json:"SessionStart"`
					} `json:"hooks"`
				} `json:"config"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &req) != nil || req.Method != method {
			continue
		}
		matcher := ""
		if groups := req.Params.Config.Hooks.SessionStart; len(groups) == 1 {
			matcher = groups[0].Matcher
		}
		matchers = append(matchers, matcher)
	}
	return matchers
}

// A Codex assistant's start is recorded as owed until a turn delivers it,
// and a session that ends first leaves the next one to register the hooks
// for its resume. An ordinary thread records nothing.
func TestCodexAssistantCarriesAnOwedDeliveryToItsNextSession(t *testing.T) {
	t.Parallel()
	app, bus := setupE2EApp(t)
	requestLog := filepath.Join(t.TempDir(), "requests.jsonl")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": writeCodexInstructionsAppServer(t, requestLog)}); err != nil {
		t.Fatal(err)
	}
	thread, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatal(err)
	}
	testAssistant(t, app, thread.ID)

	startCodexSessionAndWait(t, app, bus, thread.ID)
	awaitSessionRef(t, app, thread.ID, "codex-root")
	if got, err := app.store.CodexSessionStartPending(thread.ID, "codex-root"); err != nil || got != string(provider.SessionStartAfterStart) {
		t.Fatalf("owed after a fresh start = %q, %v; want start", got, err)
	}
	if err := app.StopSession(thread.ID); err != nil {
		t.Fatal(err)
	}
	startCodexSessionAndWait(t, app, bus, thread.ID)
	if got := codexHookMatchers(t, requestLog, "thread/start"); strings.Join(got, ",") != provider.SessionStartHookSources {
		t.Errorf("thread/start matchers = %q", got)
	}
	if got := codexHookMatchers(t, requestLog, "thread/resume"); strings.Join(got, ",") != provider.SessionStartHookSources+"|resume" {
		t.Errorf("thread/resume matchers = %q, want the hooks registered for the resume", got)
	}
	if err := app.StopSession(thread.ID); err != nil {
		t.Fatal(err)
	}

	plain, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatal(err)
	}
	startCodexSessionAndWait(t, app, bus, plain.ID)
	awaitSessionRef(t, app, plain.ID, "codex-root")
	if got, err := app.store.CodexSessionStartPending(plain.ID, "codex-root"); err != nil || got != "" {
		t.Fatalf("an ordinary thread recorded %q, %v", got, err)
	}
	if got := codexHookMatchers(t, requestLog, "thread/start"); len(got) != 2 || got[1] != "" {
		t.Errorf("an ordinary thread registered hooks: %q", got)
	}
}
