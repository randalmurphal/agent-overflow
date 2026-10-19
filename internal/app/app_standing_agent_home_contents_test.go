package app

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// A standing agent's sessions (docs/specs/standing-agents.md, "Agent home
// contents") start on Claude, claude-tui and Codex with a static block: who
// the agent is, the delivery and skill sentences and the base instructions,
// through the channel the home block uses. The home files reach the model
// through SessionStart hooks the session registers: Claude's inline
// --settings and Codex's thread config, each running this binary's hook
// verb. No mock speaks claude-tui, so these tests read what each provider
// was started with (the append file and --settings of the argv, the
// thread/start or thread/resume params) and run the registered hook
// commands as the provider would.

const homeContentsRole = "Release lead for the test project."

// homeContentsWant is the block every session of the agent starts with: its
// sentences spelled out rather than built by the code under test, then the
// base instructions, which are one constant.
func homeContentsWant(name string, row store.StandingAgent) string {
	return "You are the standing agent " + strconv.Quote(name) + " (assistant id " + row.ID + "; this thread is " + row.ThreadID + "). Your home is " + row.Home +
		". You own that directory and keep your durable state there." +
		" Agent Overflow puts your instructions.md and MEMORY.md in your context, each under its own heading, when this thread starts and after each compaction, and again when the thread resumes after one of them changed; the latest copy is current. When you do not see them, read them from your home before you act." +
		" Load the agent-home skill before any work. If it is not installed, tell the user to install the skills library from Settings, Agents, Skills, and keep your home files by hand until it is." +
		"\n\n" + agenthome.BaseInstructions
}

// homeContextWant is what the two hooks deliver for the fixture files, in
// order.
func homeContextWant(index string) string {
	return "## Your standing instructions (instructions.md in your home)\n\n" + homeContentsRole + "\n\nGrants: none.\n\n[End of instructions.md]" +
		"\n\n## Your memory index (MEMORY.md in your home)\n\n" + index + "\n\n[End of MEMORY.md]"
}

func writeHomeFile(t *testing.T, home, name, text string) {
	t.Helper()
	path := filepath.Join(home, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// startedSession is what a session was started with beside the app guide:
// the block, and the SessionStart hook commands AO registered.
type startedSession struct {
	block string
	hooks []string
}

// startSession starts the thread's session and returns what it was started
// with.
type startSession func(t *testing.T) startedSession

// homeHookCommands are the commands of AO's SessionStart groups in a hooks
// map: those whose matcher is provider.SessionStartHookSources, or
// provider.SessionStartResumeSources for a hook a resume runs.
func homeHookCommands(t *testing.T, hooks any) []string {
	t.Helper()
	raw, _ := json.Marshal(hooks)
	var decoded struct {
		SessionStart []struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"SessionStart"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("hooks %s: %v", raw, err)
	}
	var commands []string
	for _, group := range decoded.SessionStart {
		if group.Matcher != provider.SessionStartHookSources && group.Matcher != provider.SessionStartResumeSources {
			continue
		}
		for _, hook := range group.Hooks {
			if hook.Type != "command" || hook.Timeout != provider.SessionStartHookTimeoutSeconds {
				t.Fatalf("hook %+v", hook)
			}
			commands = append(commands, hook.Command)
		}
	}
	return commands
}

func claudeStarted(app *App, threadID, argvPath string) startSession {
	return func(t *testing.T) startedSession {
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
		var started startedSession
		if appendPath, found := argValue(argv, "--append-system-prompt-file"); found {
			content, err := os.ReadFile(appendPath)
			if err != nil {
				t.Fatalf("read the append file: %v", err)
			}
			started.block = string(content)
		}
		if settings, found := argValue(argv, "--settings"); found {
			var block struct {
				Hooks any `json:"hooks"`
			}
			if err := json.Unmarshal([]byte(settings), &block); err != nil {
				t.Fatalf("--settings %s: %v", settings, err)
			}
			started.hooks = homeHookCommands(t, block.Hooks)
		}
		if err := app.StopSession(threadID); err != nil {
			t.Fatalf("StopSession: %v", err)
		}
		return started
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

func codexStarted(app *App, threadID, logPath string) startSession {
	return func(t *testing.T) startedSession {
		t.Helper()
		_ = os.Remove(logPath)
		if err := app.StartSession(threadID); err != nil {
			t.Fatalf("StartSession: %v", err)
		}
		var started *startedSession
		deadline := time.Now().Add(5 * time.Second)
		for started == nil {
			raw, _ := os.ReadFile(logPath)
			for _, line := range strings.Split(string(raw), "\n") {
				var request struct {
					Method string `json:"method"`
					Params struct {
						DeveloperInstructions *string `json:"developerInstructions"`
						Config                struct {
							Hooks map[string]any `json:"hooks"`
						} `json:"config"`
					} `json:"params"`
				}
				if json.Unmarshal([]byte(line), &request) != nil || (request.Method != "thread/start" && request.Method != "thread/resume") {
					continue
				}
				started = &startedSession{hooks: homeHookCommands(t, request.Params.Config.Hooks)}
				if request.Params.DeveloperInstructions != nil {
					started.block = *request.Params.DeveloperInstructions
				}
				// Each hook is trusted by its own hash, and nothing else.
				state, _ := request.Params.Config.Hooks["state"].(map[string]any)
				if len(state) != len(started.hooks) {
					t.Fatalf("hook trust state %v for %d hooks", state, len(started.hooks))
				}
				for i := range started.hooks {
					if _, ok := state["/<session-flags>/config.toml:session_start:"+strconv.Itoa(i)+":0"]; !ok {
						t.Fatalf("hook %d is not trusted: %v", i, state)
					}
				}
			}
			if started == nil && time.Now().After(deadline) {
				t.Fatal("no thread/start or thread/resume reached the mock")
			}
			if started == nil {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if err := app.StopSession(threadID); err != nil {
			t.Fatalf("StopSession: %v", err)
		}
		return *started
	}
}

// runHomeHooks runs the hook commands as both providers do, through a POSIX
// shell with the hook input on stdin, and joins the context each delivered,
// each without its stamp.
// The commands run this test binary, whose TestMain serves the hook verb.
func runHomeHooks(t *testing.T, commands []string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the providers run hooks through a POSIX shell")
	}
	if len(commands) != len(agenthome.ContextFiles) {
		t.Fatalf("hook commands = %q, want one per context file", commands)
	}
	var contexts []string
	for _, command := range commands {
		cmd := exec.Command("/bin/sh", "-c", command)
		cmd.Stdin = strings.NewReader(`{"hook_event_name":"SessionStart","source":"startup"}`)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		stdout, err := cmd.Output()
		if err != nil {
			t.Fatalf("hook %s: %v: %s", command, err, stderr.String())
		}
		var output struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(stdout, &output); err != nil || output.HookSpecificOutput.HookEventName != "SessionStart" {
			t.Fatalf("hook output %q: %v", stdout, err)
		}
		// Each delivery carries its stamp on the line under its heading.
		heading, rest, _ := strings.Cut(output.HookSpecificOutput.AdditionalContext, "\n")
		stampLine, body, _ := strings.Cut(rest, "\n")
		if !strings.HasPrefix(stampLine, agenthome.StampPrefix) {
			t.Fatalf("hook output %q has no stamp under its heading", output.HookSpecificOutput.AdditionalContext)
		}
		contexts = append(contexts, heading+"\n"+body)
	}
	return strings.Join(contexts, "\n\n")
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
			var start startSession
			settings := map[string]any{"agentGuideEnabled": false}
			switch providerName {
			case string(provider.Codex):
				settings["codexBinaryPath"] = writeRecordingCodex(t, record)
				start = codexStarted(app, thread.ID, record)
			default:
				settings["claudeBinaryPath"] = writeArgvRecordingClaude(t, record)
				start = claudeStarted(app, thread.ID, record)
			}
			if _, err := app.settings.Update(settings); err != nil {
				t.Fatalf("settings: %v", err)
			}
			row := testAssistant(t, app, thread.ID)
			want := homeContentsWant(thread.Title, row)
			// What the developer instructions carry after the block (the
			// thread tools' guide on Codex) is not this test's.
			blockOf := func(got string) string {
				if strings.HasPrefix(got, want) && (len(got) == len(want) || strings.HasPrefix(got[len(want):], "\n\n")) {
					return want
				}
				return got
			}

			// ---- Both files: the block names neither's text; the hooks
			// deliver both.
			writeHomeFile(t, row.Home, agenthome.InstructionsFile, homeContentsRole+"\n\nGrants: none.\n")
			writeHomeFile(t, row.Home, agenthome.MemoryIndexFile, "# Memory\n- [A](memory/a.md): first\n")
			writeHomeFile(t, row.Home, "memory/a.md", "A")
			first := start(t)
			if got := blockOf(first.block); got != want {
				t.Fatalf("first start's block:\n%s\nwant:\n%s", got, want)
			}
			if got, want := runHomeHooks(t, first.hooks), homeContextWant("# Memory\n- [A](memory/a.md): first"); got != want {
				t.Fatalf("the hooks delivered:\n%s\nwant:\n%s", got, want)
			}

			// ---- An edit changes nothing the session starts with, so the
			// provider's prompt cache holds, and reaches the next delivery.
			writeHomeFile(t, row.Home, agenthome.MemoryIndexFile, "# Memory\n- [B](memory/b.md): rewritten\n")
			writeHomeFile(t, row.Home, "memory/b.md", "B")
			if got := runHomeHooks(t, first.hooks); got != homeContextWant("# Memory\n- [B](memory/b.md): rewritten\n[Index check: memory/ holds 1 file MEMORY.md does not link: memory/a.md.]") {
				t.Fatalf("after the rewrite the hooks delivered:\n%s", got)
			}
			second := start(t)
			if second.block != first.block || !slices.Equal(second.hooks, first.hooks) {
				t.Fatalf("a start after the edit differs:\n%q %q\n%q %q", first.block, first.hooks, second.block, second.hooks)
			}

			// ---- Past the caps: the head of each file and where it stopped.
			line := strings.Repeat("x", 99) + "\n"
			writeHomeFile(t, row.Home, agenthome.InstructionsFile, homeContentsRole+"\n"+strings.Repeat(line, 100)+"INSTRUCTIONS TAIL\n")
			writeHomeFile(t, row.Home, agenthome.MemoryIndexFile, strings.Repeat(line, 50)+"INDEX TAIL\n")
			capped := runHomeHooks(t, first.hooks)
			for _, want := range []string{
				"[instructions.md stops here, after line 82 (byte 8135 of its text with hidden characters removed): Agent Overflow delivers at most 8 KiB of it. Read it yourself from line 83.]",
				"[MEMORY.md stops here, after line 40 (byte 4000 of its text with hidden characters removed): Agent Overflow delivers at most 4 KiB of it. Read it yourself from line 41.]",
			} {
				if !strings.Contains(capped, want) {
					t.Fatalf("capped delivery has no %q:\n%s", want, capped)
				}
			}
			if strings.Contains(capped, "INSTRUCTIONS TAIL") || strings.Contains(capped, "INDEX TAIL") {
				t.Fatal("a capped delivery carries a file's tail")
			}
			if third := start(t); third.block != first.block {
				t.Fatalf("the capped files changed the block:\n%s", third.block)
			}
		})
	}
}

// A home file that cannot be read starts the session: its hook delivers a
// notice in its place, and the thread gets a warning row naming its path,
// whatever kept it out.
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
			started := claudeStarted(app, thread.ID, argvPath)(t)
			got := runHomeHooks(t, started.hooks)
			if !strings.Contains(got, "[instructions.md could not be read: ") || !strings.Contains(got, tc.why) ||
				!strings.Contains(got, "index line") || strings.Contains(got, "secret") {
				t.Fatalf("delivered:\n%s", got)
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

// A compaction runs the home hooks again, so a home file that became
// unreadable after the start is reported then, and only for the thread's
// own compaction.
func TestStandingAgentCompactionReportsAnUnreadableHomeFile(t *testing.T) {
	t.Parallel()
	app, thread, home, _ := unreadableHomeFixture(t)
	path := filepath.Join(home, agenthome.InstructionsFile)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	handler := startedSessionEventHandler(app, thread.ID, "token-live", string(provider.Claude))
	handler(provider.ProviderEvent{Kind: provider.EventCompactBoundary, ThreadID: thread.ID, ParentToolUseID: "toolu_child", Timestamp: time.Now()})
	waitProviderEvents(t, app, thread.ID)
	if homeWarningRows(t, app, thread.ID) != 0 {
		t.Fatal("a subagent's compaction reported the home")
	}
	handler(provider.ProviderEvent{Kind: provider.EventCompactBoundary, ThreadID: thread.ID, Timestamp: time.Now()})
	waitProviderEvents(t, app, thread.ID)
	requireHomeWarningRow(t, app, thread.ID, path, "not a regular file")

	// An ordinary thread's compaction reads no home.
	plain, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	startedSessionEventHandler(app, plain.ID, "token-plain", string(provider.Claude))(provider.ProviderEvent{Kind: provider.EventCompactBoundary, ThreadID: plain.ID, Timestamp: time.Now()})
	waitProviderEvents(t, app, plain.ID)
	if homeWarningRows(t, app, plain.ID) != 0 {
		t.Fatal("an ordinary thread's compaction reported a home")
	}
}

// The hooks are not built from a path a hook could not run with, and the
// reason is a problem the start reports.
func TestStandingAgentHooksRefuseWhatTheyCannotRun(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	exe := filepath.Join(t.TempDir(), "agent-overflow")
	if hooks, problems := standingAgentHooks(home, false, nil, func() (string, error) { return "", errors.New("no /proc") }); hooks != nil || len(problems) != 1 ||
		problems[0].Error() != "locate this executable for the SessionStart hooks that deliver the home files: no /proc" {
		t.Fatalf("a failed lookup: %v, %v", hooks, problems)
	}
	if hooks, problems := standingAgentHooks("agents/a", false, nil, func() (string, error) { return exe, nil }); hooks != nil || len(problems) != 1 ||
		!strings.HasPrefix(problems[0].Error(), "build the SessionStart hook that delivers instructions.md: ") {
		t.Fatalf("a relative home: %v, %v", hooks, problems)
	}
	hooks, problems := standingAgentHooks(home, false, nil, func() (string, error) { return exe, nil })
	if len(problems) > 0 || len(hooks) != len(agenthome.ContextFiles) {
		t.Fatalf("hooks = %+v, %v", hooks, problems)
	}
	for i, file := range agenthome.ContextFiles {
		want, _ := agenthome.HookCommand(exe, home, file)
		if hooks[i] != (provider.SessionStartHook{Label: file.Name, Command: want, Heading: file.Heading, Ending: file.Ending, Stamp: agenthome.StampPrefix}) {
			t.Errorf("hook %d = %+v", i, hooks[i])
		}
	}
}

// A resume runs the hook of each file whose context now differs from the
// one the thread's model last received, and of each file it has no receipt
// for; a fresh start runs every hook anyway and marks none.
func TestStandingAgentHooksResumeWhatChanged(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, agenthome.InstructionsFile), []byte("Release lead\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "agent-overflow")
	locate := func() (string, error) { return exe, nil }
	current := map[string]string{}
	for _, file := range agenthome.ContextFiles {
		text, _ := agenthome.ReadContext(home, file)
		current[file.Name] = agenthome.ContextDigest(text)
	}
	resumes := func(resume bool, received map[string]string) []bool {
		t.Helper()
		hooks, problems := standingAgentHooks(home, resume, received, locate)
		if len(problems) > 0 || len(hooks) != len(agenthome.ContextFiles) {
			t.Fatalf("hooks = %+v, %v", hooks, problems)
		}
		marked := make([]bool, len(hooks))
		for i, hook := range hooks {
			marked[i] = hook.Resume
		}
		return marked
	}
	for name, tc := range map[string]struct {
		resume   bool
		received map[string]string
		want     []bool
	}{
		"fresh":         {false, nil, []bool{false, false}},
		"no receipts":   {true, nil, []bool{true, true}},
		"none read":     {true, map[string]string{}, []bool{true, true}},
		"both current":  {true, current, []bool{false, false}},
		"index changed": {true, map[string]string{agenthome.InstructionsFile: current[agenthome.InstructionsFile], agenthome.MemoryIndexFile: "old"}, []bool{false, true}},
		"one receipt":   {true, map[string]string{agenthome.MemoryIndexFile: current[agenthome.MemoryIndexFile]}, []bool{true, false}},
	} {
		if got := resumes(tc.resume, tc.received); !slices.Equal(got, tc.want) {
			t.Errorf("%s: resume = %v, want %v", name, got, tc.want)
		}
	}
	// An edit changes the context, and so the decision.
	if err := os.WriteFile(filepath.Join(home, agenthome.InstructionsFile), []byte("Release lead, now also on call\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resumes(true, current); !slices.Equal(got, []bool{true, false}) {
		t.Errorf("after an edit: resume = %v", got)
	}
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

// homeWarningRows counts the thread's home warning rows.
func homeWarningRows(t *testing.T, app *App, threadID string) int {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, item := range items {
		if strings.Contains(item.Summary, "One of this standing agent's home files could not be read, and its sessions receive a notice about it: ") {
			n++
		}
	}
	return n
}

func requireHomeWarningRow(t *testing.T, app *App, threadID, path, why string) {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if strings.Contains(item.Summary, "One of this standing agent's home files could not be read, and its sessions receive a notice about it: ") &&
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
// StartSession and from a queued notice's start, and its hook delivers the
// notice in its place.
func TestAHomeFileStoppedByTheRawBoundStartsTheSession(t *testing.T) {
	t.Parallel()
	notice := "[instructions.md stops here, inside line 1: Agent Overflow reads at most the first 1 MiB of a file and the rest of it was not read. Read it yourself from line 1.]"

	app, thread, home, argvPath := unreadableHomeFixture(t)
	homeAtTheRawBound(t, home)
	if got := runHomeHooks(t, claudeStarted(app, thread.ID, argvPath)(t).hooks); !strings.Contains(got, notice) || !strings.Contains(got, "index line") {
		t.Fatalf("StartSession's hooks delivered:\n%s", got)
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
	settings, found := argValue(argv, "--settings")
	if !found {
		t.Fatalf("no --settings in %v", argv)
	}
	var block struct {
		Hooks any `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(settings), &block); err != nil {
		t.Fatalf("--settings %s: %v", settings, err)
	}
	if got := runHomeHooks(t, homeHookCommands(t, block.Hooks)); !strings.Contains(got, notice) {
		t.Fatalf("the notice's start delivered:\n%s", got)
	}
	if err := queued.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
}
