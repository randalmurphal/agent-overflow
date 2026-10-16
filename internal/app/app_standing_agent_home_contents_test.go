package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
func homeContentsWant(name string, row store.StandingAgent, index string) string {
	return "You are the standing agent " + strconv.Quote(name) + " (assistant id " + row.ID + "; this thread is " + row.ThreadID + "). Your home is " + row.Home +
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
		// Slice 7's workflow tools ride the same launch (claude-tui hosts
		// only them): the home block must coexist with them, not displace
		// them.
		if !hasArgPair(argv, "--allowedTools", WorkflowToolsAllowedTool) ||
			!slices.ContainsFunc(argv, func(arg string) bool { return strings.Contains(arg, `"`+workflowMCPName+`"`) }) {
			t.Fatalf("the launch lost ao-workflow-tools: %v", argv)
		}
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
			row := testAssistant(t, app, thread.ID)
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
			want := homeContentsWant(thread.Title, row, "# Memory\n- [A](memory/a.md): first")
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
			want = homeContentsWant(thread.Title, row, "# Memory\n- [B](memory/b.md): rewritten")
			if got := blockOf(start(t), want); got != want {
				t.Fatalf("after the rewrite:\n%s\nwant:\n%s", got, want)
			}

			// ---- Past the caps: the head of each file and where it stopped.
			line := strings.Repeat("x", 99) + "\n"
			writeHomeFile(t, row.Home, agenthome.InstructionsFile, homeContentsRole+"\n"+strings.Repeat(line, 100)+"INSTRUCTIONS TAIL\n")
			writeHomeFile(t, row.Home, agenthome.MemoryIndexFile, strings.Repeat(line, 50)+"INDEX TAIL\n")
			capped := start(t)
			for _, want := range []string{
				"[instructions.md stops here, after line 82 (byte 8135 of its text with hidden characters removed): the session start injects at most 8 KiB of it. Read it yourself from line 83.]",
				"[MEMORY.md stops here, after line 40 (byte 4000 of its text with hidden characters removed): the session start injects at most 4 KiB of it. Read it yourself from line 41.]",
			} {
				if !strings.Contains(capped, want) {
					t.Fatalf("capped start has no %q:\n%s", want, capped)
				}
			}
			if strings.Contains(capped, "INSTRUCTIONS TAIL") || strings.Contains(capped, "INDEX TAIL") {
				t.Fatal("a capped start carries a file's tail")
			}

		})
	}
}

// A home file that cannot be read keeps nothing of it out of the start: the
// session starts, its block says which file and why, and the thread gets a
// warning row naming the path.
// A home file that cannot be read starts the session without it, says so in
// its place and puts a warning row naming its path on the thread, whatever
// kept it out.
func TestStandingAgentStartsWhenAHomeFileCannotBeRead(t *testing.T) {
	t.Parallel()
	cases := map[string]homeBreak{
		"directory": {func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
		"link out of the home": {func(t *testing.T, path string) {
			outside := filepath.Join(t.TempDir(), "outside.md")
			if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Skip("no symlinks here:", err)
			}
		}, "path escapes from parent"},
	}
	addUnixCases(cases)
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app, thread, home, argvPath := unreadableHomeFixture(t)
			path := filepath.Join(home, agenthome.InstructionsFile)
			tc.apply(t, path)
			got := claudeInjected(app, thread.ID, argvPath)(t)
			if !strings.Contains(got, "[instructions.md could not be read: ") || !strings.Contains(got, tc.why) ||
				!strings.Contains(got, "index line") || strings.Contains(got, "secret") {
				t.Fatalf("injected:\n%s", got)
			}
			requireHomeWarningRow(t, app, thread.ID, path, tc.why)
		})
	}
}

// A start that fails still reports the home file it could not read.
func TestStandingAgentStartFailureStillReportsAnUnreadableHomeFile(t *testing.T) {
	t.Parallel()
	app, thread, home, _ := unreadableHomeFixture(t)
	path := filepath.Join(home, agenthome.InstructionsFile)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": filepath.Join(t.TempDir(), "no-claude")}); err != nil {
		t.Fatalf("settings: %v", err)
	}
	if err := app.StartSession(thread.ID); err == nil {
		t.Fatal("StartSession with no provider binary succeeded")
	}
	requireHomeWarningRow(t, app, thread.ID, path, "not a regular file")
}

// homeBreak puts something unreadable at path and names the reason the read
// reports.
type homeBreak struct {
	apply func(t *testing.T, path string)
	why   string
}

func unreadableHomeFixture(t *testing.T) (*App, store.Thread, string, string) {
	t.Helper()
	app, _ := setupE2EApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close(); _ = app.workflowMCPServer().Close() })
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeArgvRecordingClaude(t, argvPath), "agentGuideEnabled": false}); err != nil {
		t.Fatalf("settings: %v", err)
	}
	row := testAssistant(t, app, thread.ID)
	writeHomeFile(t, row.Home, agenthome.MemoryIndexFile, "index line\n")
	return app, thread, row.Home, argvPath
}

func requireHomeWarningRow(t *testing.T, app *App, threadID, path, why string) {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if strings.Contains(item.Summary, "One of this standing agent's home files could not be read for its session start: ") &&
			strings.Contains(item.Summary, path) && strings.Contains(item.Summary, why) {
			return
		}
	}
	t.Fatalf("no warning row naming %s (%s) in %+v", path, why, items)
}

// homeAtTheRawBound is an instructions.md the read stops at the 1 MiB raw
// bound inside the cap: two MiB of NUL, which cleaning removes.
func homeAtTheRawBound(t *testing.T, home string) {
	t.Helper()
	writeHomeFile(t, home, agenthome.InstructionsFile, strings.Repeat("\x00", 2*agenthome.MaxRawRead))
}

// A home file the raw bound stops inside its cap starts the session, from
// StartSession and from a queued notice's start, with the notice in its
// place.
func TestAHomeFileStoppedByTheRawBoundStartsTheSession(t *testing.T) {
	t.Parallel()
	notice := "[instructions.md stops here, inside line 1: the session start reads at most the first 1 MiB of a file and the rest of it was not read. Read it yourself from line 1.]"

	app, thread, home, argvPath := unreadableHomeFixture(t)
	homeAtTheRawBound(t, home)
	if got := claudeInjected(app, thread.ID, argvPath)(t); !strings.Contains(got, notice) || !strings.Contains(got, "index line") {
		t.Fatalf("StartSession injected:\n%s", got)
	}

	queued, thread, home, argvPath := unreadableHomeFixture(t)
	homeAtTheRawBound(t, home)
	if err := queued.queueAgentNotice(agentNotice{
		threadID:    thread.ID,
		sendID:      agentSendIDRoot + "raw-bound",
		prepare:     func() (string, bool, error) { return "the deploy finished", true, nil },
		startFailed: func(err error) string { return "the agent could not start: " + err.Error() },
	}); err != nil {
		t.Fatalf("queueAgentNotice: %v", err)
	}
	queued.waitAgentNoticeWork()
	if rows := threadErrorRows(t, queued.store, thread.ID, ""); len(rows) > 0 {
		t.Fatalf("the notice's start failed: %q", rows)
	}
	argv := spawnedClaudeArgv(t, argvPath)
	appendPath, found := argValue(argv, "--append-system-prompt-file")
	if !found {
		t.Fatalf("no append file in %v", argv)
	}
	content, err := os.ReadFile(appendPath)
	if err != nil {
		t.Fatalf("read the append file: %v", err)
	}
	if !strings.Contains(string(content), notice) {
		t.Fatalf("the notice's start injected:\n%s", content)
	}
	if err := queued.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
}
