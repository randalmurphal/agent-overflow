package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/mockexec"
)

// A standing agent's home contents (docs/specs/standing-agents.md, "Agent
// home contents") reach the provider at every start and resume, on Claude,
// claude-tui and Codex, through the channel the home block uses. No mock
// speaks claude-tui, so these tests read what each provider was started
// with: the append file the argv names on both Claude providers and the
// developerInstructions of Codex's thread/start or thread/resume.

const homeContentsRole = "Release lead for the test project."

// homeContentsWant is the block a home holding the fixture files injects,
// spelled out rather than built by the code under test.
func homeContentsWant(row store.StandingAgent, index string) string {
	return "You are a standing agent: the user promoted this thread (id " + row.ThreadID + "). Your home is " + row.Home +
		". You own that directory and keep your durable state there. Load the agent-home skill before any work. If it is not installed, tell the user to install the skills library from Settings, Agents, Skills, and keep your home files by hand until it is." +
		"\n\n## Your standing instructions (instructions.md in your home)\n\n" + homeContentsRole + "\n\nGrants: none." +
		"\n\n## Your memory index (MEMORY.md in your home, without its archive)\n\n" + index
}

func writeHomeFile(t *testing.T, home, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// injectedPrompt starts the thread's session and returns what it was started
// with beside the app guide.
type injectedPrompt func(t *testing.T) string

func claudeInjected(app *App, threadID, argvPath string) injectedPrompt {
	return func(t *testing.T) string {
		t.Helper()
		_ = os.Remove(argvPath)
		if err := app.StartSession(threadID); err != nil {
			t.Fatalf("StartSession: %v", err)
		}
		argv := spawnedClaudeArgv(t, argvPath)
		appendPath, found := argValue(argv, "--append-system-prompt-file")
		if !found {
			return ""
		}
		content, err := os.ReadFile(appendPath)
		if err != nil {
			t.Fatalf("read the append file: %v", err)
		}
		if err := app.StopSession(threadID); err != nil {
			t.Fatalf("StopSession: %v", err)
		}
		return string(content)
	}
}

// writeRecordingCodex is a Codex app-server mock that appends every request
// line to logPath.
func writeRecordingCodex(t *testing.T, logPath string) string {
	t.Helper()
	inner := testutil.WriteMockCodexSession(t, t.TempDir(), map[string]string{
		`"method":"initialize"`:    `{"jsonrpc":"2.0","id":%s,"result":{}}`,
		`"method":"thread/start"`:  `{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"codex-home-contents"}}}`,
		`"method":"thread/resume"`: `{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"codex-home-contents"}}}`,
		`"method":"config/read"`:   `{"jsonrpc":"2.0","id":%s,"result":{"config":{},"origins":{}}}`,
		"":                         `{"jsonrpc":"2.0","id":%s,"result":{}}`,
	})
	return mockexec.Write(t, filepath.Join(t.TempDir(), "codex-recording.sh"),
		"#!/bin/bash\ntee -a '"+logPath+"' | '"+inner+"'\n")
}

func codexInjected(app *App, threadID, logPath string) injectedPrompt {
	return func(t *testing.T) string {
		t.Helper()
		_ = os.Remove(logPath)
		if err := app.StartSession(threadID); err != nil {
			t.Fatalf("StartSession: %v", err)
		}
		var found string
		deadline := time.Now().Add(5 * time.Second)
		for found == "" {
			raw, _ := os.ReadFile(logPath)
			for _, line := range strings.Split(string(raw), "\n") {
				var request struct {
					Method string `json:"method"`
					Params struct {
						DeveloperInstructions *string `json:"developerInstructions"`
					} `json:"params"`
				}
				if json.Unmarshal([]byte(line), &request) != nil {
					continue
				}
				if request.Method == "thread/start" || request.Method == "thread/resume" {
					found = "\x00"
					if request.Params.DeveloperInstructions != nil {
						found = *request.Params.DeveloperInstructions
					}
				}
			}
			if found == "" && time.Now().After(deadline) {
				t.Fatal("no thread/start or thread/resume reached the mock")
			}
			if found == "" {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if err := app.StopSession(threadID); err != nil {
			t.Fatalf("StopSession: %v", err)
		}
		return strings.TrimPrefix(found, "\x00")
	}
}

func TestStandingAgentSessionsCarryTheirHomeFiles(t *testing.T) {
	t.Parallel()
	for _, providerName := range []string{string(provider.Claude), string(provider.ClaudeTUI), string(provider.Codex)} {
		t.Run(providerName, func(t *testing.T) {
			t.Parallel()
			app, _ := setupE2EApp(t)
			t.Cleanup(func() { _ = app.threadMCPServer().Close() })
			model := "claude-opus-4-7"
			if providerName == string(provider.Codex) {
				model = "gpt-5.4"
			}
			thread, err := createTestThread(t, app, providerName, t.TempDir(), model, "chat")
			if err != nil {
				t.Fatalf("CreateThread: %v", err)
			}
			record := filepath.Join(t.TempDir(), "record.txt")
			var start injectedPrompt
			settings := map[string]any{"agentGuideEnabled": false}
			switch providerName {
			case string(provider.Codex):
				settings["codexBinaryPath"] = writeRecordingCodex(t, record)
				start = codexInjected(app, thread.ID, record)
			default:
				settings["claudeBinaryPath"] = writeArgvRecordingClaude(t, record)
				start = claudeInjected(app, thread.ID, record)
			}
			if _, err := app.settings.Update(settings); err != nil {
				t.Fatalf("settings: %v", err)
			}
			row, err := app.PromoteStandingAgent(thread.ID)
			if err != nil {
				t.Fatalf("PromoteStandingAgent: %v", err)
			}
			// What the developer instructions carry after the block (the
			// thread tools' guide on Codex) is not this test's.
			blockOf := func(got, want string) string {
				if strings.HasPrefix(got, want) && (len(got) == len(want) || strings.HasPrefix(got[len(want):], "\n\n")) {
					return want
				}
				return got
			}

			// ---- Both files, the archive left out.
			writeHomeFile(t, row.Home, agenthome.InstructionsFile, homeContentsRole+"\n\nGrants: none.\n")
			writeHomeFile(t, row.Home, agenthome.MemoryIndexFile, "# Memory\n- [A](memory/a.md): first\n"+agenthome.ArchiveMarker+"\n- [Old](memory/old.md): archived\n")
			want := homeContentsWant(row, "# Memory\n- [A](memory/a.md): first")
			first := start(t)
			if got := blockOf(first, want); got != want {
				t.Fatalf("first start injected:\n%s\nwant:\n%s", got, want)
			}

			// ---- Unchanged files: the same bytes on the next start.
			if second := start(t); second != first {
				t.Fatalf("a second start over unchanged files differs:\n%q\n%q", first, second)
			}

			// ---- The hygiene loop's rewrite reaches the next start.
			writeHomeFile(t, row.Home, agenthome.MemoryIndexFile, "# Memory\n- [B](memory/b.md): rewritten\n")
			want = homeContentsWant(row, "# Memory\n- [B](memory/b.md): rewritten")
			if got := blockOf(start(t), want); got != want {
				t.Fatalf("after the rewrite:\n%s\nwant:\n%s", got, want)
			}

			// ---- Past the caps: the head of each file and where it stopped.
			line := strings.Repeat("x", 99) + "\n"
			writeHomeFile(t, row.Home, agenthome.InstructionsFile, homeContentsRole+"\n"+strings.Repeat(line, 100)+"INSTRUCTIONS TAIL\n")
			writeHomeFile(t, row.Home, agenthome.MemoryIndexFile, strings.Repeat(line, 50)+"INDEX TAIL\n")
			capped := start(t)
			for _, want := range []string{
				"[instructions.md stops here, at byte 8135: the session start injects at most 8 KiB of it. Read the rest from that byte yourself.]",
				"[MEMORY.md stops here, at byte 4000: the session start injects at most 4 KiB of it. Read the rest from that byte yourself.]",
			} {
				if !strings.Contains(capped, want) {
					t.Fatalf("capped start has no %q:\n%s", want, capped)
				}
			}
			if strings.Contains(capped, "INSTRUCTIONS TAIL") || strings.Contains(capped, "INDEX TAIL") {
				t.Fatal("a capped start carries a file's tail")
			}

			// ---- Demoted: nothing of the home.
			if err := app.DemoteStandingAgent(thread.ID); err != nil {
				t.Fatalf("DemoteStandingAgent: %v", err)
			}
			if got := start(t); strings.Contains(got, row.Home) || strings.Contains(got, homeContentsRole) {
				t.Fatalf("a demoted thread's session carries its home:\n%s", got)
			}
		})
	}
}

// A home file that cannot be read keeps nothing of it out of the start: the
// session starts, its block says which file and why, and the thread gets a
// warning row naming the path.
func TestStandingAgentStartsWhenAHomeFileCannotBeRead(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeArgvRecordingClaude(t, argvPath), "agentGuideEnabled": false}); err != nil {
		t.Fatalf("settings: %v", err)
	}
	row, err := app.PromoteStandingAgent(thread.ID)
	if err != nil {
		t.Fatalf("PromoteStandingAgent: %v", err)
	}
	if err := os.Mkdir(filepath.Join(row.Home, agenthome.InstructionsFile), 0o700); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, row.Home, agenthome.MemoryIndexFile, "index line\n")
	got := claudeInjected(app, thread.ID, argvPath)(t)
	if !strings.Contains(got, "[instructions.md could not be read: not a regular file. Read it yourself.]") || !strings.Contains(got, "index line") {
		t.Fatalf("injected:\n%s", got)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(row.Home, agenthome.InstructionsFile)
	for _, item := range items {
		if strings.Contains(item.Summary, "This standing agent's session started without one of its home files") && strings.Contains(item.Summary, path) {
			return
		}
	}
	t.Fatalf("no warning row naming %s in %+v", path, items)
}
