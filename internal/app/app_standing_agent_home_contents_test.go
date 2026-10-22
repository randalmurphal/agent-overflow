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
	"agent-overflow/internal/memory"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/mockexec"
)

// A standing agent's sessions (docs/specs/standing-agents.md, "Agent home
// contents") start on Claude, claude-tui and Codex with a static block: who
// the agent is, the delivery sentence and the base instructions, through the
// channel the home block uses. Its home files and the rest of its memory
// reach the model through the memory hook the session registers
// (docs/specs/memory.md, "Delivery"), in Claude's inline --settings and
// Codex's thread config: this binary's hook verb at SessionStart, whole,
// and at UserPromptSubmit, what changed. No mock speaks claude-tui,
// so these tests read what each provider was started with (the append file,
// --settings and --add-dir of the argv, the thread/start or thread/resume
// params) and run the registered hook command as the provider would.

const homeContentsRole = "Release lead for the test project."

// homeContentsWant is the block every session of the agent starts with: its
// sentences spelled out rather than built by the code under test, then the
// base instructions, which are one constant.
func homeContentsWant(name string, row store.StandingAgent) string {
	return "You are the standing agent " + strconv.Quote(name) + " (assistant id " + row.ID + "; this thread is " + row.ThreadID + "). Your home is " + row.Home +
		". You own that directory and keep your durable state there." +
		" Agent Overflow puts your instructions.md and your memory in your context when this thread starts and after each compaction, and with the next prompt after any of it changes, only what changed; the latest copy, with the changes after it, is current. When you do not see them, read instructions.md and memory/ from your home before you act." +
		"\n\n" + agenthome.BaseInstructions
}

// homeFact is a fact file for a home's memory folder.
func homeFact(description string) string {
	return "---\ndescription: " + description + "\ntype: ruling\nsource: user\nconfirmed: 2026-10-20\n---\n"
}

// memoryBlock is the block of a delivery that opens with heading, through
// the line before the next blank line.
func memoryBlock(t *testing.T, delivery, heading string) string {
	t.Helper()
	at := strings.Index(delivery, "\n\n"+heading)
	if at < 0 {
		t.Fatalf("no block %q in:\n%s", heading, delivery)
	}
	block := delivery[at+2:]
	if end := strings.Index(block, "\n\n"); end >= 0 {
		block = block[:end]
	}
	return block
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
// the block, the SessionStart and UserPromptSubmit hook commands AO
// registered, the folders
// outside the workspace it may write (Claude's
// permissions.additionalDirectories, the writable roots of Codex's first
// turn), Claude's `--add-dir` folders and whether its provider's own memory
// was turned off.
type startedSession struct {
	block     string
	hooks     []string
	prompts   []string
	dirs      []string
	addDirs   []string
	memoryOff bool
}

// startSession starts the thread's session and returns what it was started
// with.
type startSession func(t *testing.T) startedSession

// hookGroup is a matcher group of a hooks map as both providers read it.
type hookGroup struct {
	Matcher string `json:"matcher"`
	Hooks   []struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	} `json:"hooks"`
}

// memoryHookCommands are the commands of AO's SessionStart groups in a hooks
// map, those whose matcher is provider.SessionStartHookSources, and of its
// UserPromptSubmit groups, which have no matcher.
func memoryHookCommands(t *testing.T, hooks any) (start, prompt []string) {
	t.Helper()
	raw, _ := json.Marshal(hooks)
	var decoded struct {
		SessionStart     []hookGroup `json:"SessionStart"`
		UserPromptSubmit []hookGroup `json:"UserPromptSubmit"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("hooks %s: %v", raw, err)
	}
	commands := func(groups []hookGroup, matcher string) []string {
		var out []string
		for _, group := range groups {
			if group.Matcher != matcher {
				continue
			}
			for _, hook := range group.Hooks {
				if hook.Type != "command" || hook.Timeout != provider.SessionStartHookTimeoutSeconds {
					t.Fatalf("hook %+v", hook)
				}
				out = append(out, hook.Command)
			}
		}
		return out
	}
	return commands(decoded.SessionStart, provider.SessionStartHookSources), commands(decoded.UserPromptSubmit, "")
}

func claudeStarted(app *App, threadID, argvPath string) startSession {
	return func(t *testing.T) startedSession {
		t.Helper()
		_ = os.Remove(argvPath)
		if err := app.StartSession(threadID); err != nil {
			t.Fatalf("StartSession: %v", err)
		}
		argv := spawnedClaudeArgv(t, argvPath)
		thread, err := app.store.GetThread(threadID)
		if err != nil {
			t.Fatal(err)
		}
		// Slice 7's workflow tools ride the same launch of a thread in a
		// project (claude-tui hosts only them): memory must coexist with
		// them, not displace them. An assistant's thread lives outside
		// every project and has no runs to start, so it gets none.
		hasWorkflowTools := hasArgPair(argv, "--allowedTools", WorkflowToolsAllowedTool) ||
			slices.ContainsFunc(argv, func(arg string) bool { return strings.Contains(arg, `"`+workflowMCPName+`"`) })
		if inProject := thread.ProjectID != ""; hasWorkflowTools != inProject {
			t.Fatalf("ao-workflow-tools on the launch = %v, want %v (in a project): %v", hasWorkflowTools, inProject, argv)
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
				Hooks             any               `json:"hooks"`
				AutoMemoryEnabled *bool             `json:"autoMemoryEnabled"`
				Env               map[string]string `json:"env"`
				Permissions       struct {
					AdditionalDirectories []string `json:"additionalDirectories"`
				} `json:"permissions"`
			}
			if err := json.Unmarshal([]byte(settings), &block); err != nil {
				t.Fatalf("--settings %s: %v", settings, err)
			}
			started.hooks, started.prompts = memoryHookCommands(t, block.Hooks)
			started.memoryOff = block.AutoMemoryEnabled != nil && !*block.AutoMemoryEnabled
			// The variable outranks the setting, so the off switch states
			// it beside the setting.
			if started.memoryOff != (block.Env[claude.AutoMemoryOffEnv] == "1") {
				t.Fatalf("memory off %v, but the settings env says %s=%q", started.memoryOff, claude.AutoMemoryOffEnv, block.Env[claude.AutoMemoryOffEnv])
			}
			started.dirs = block.Permissions.AdditionalDirectories
		}
		for i, arg := range argv {
			if arg == "--add-dir" && i+1 < len(argv) {
				started.addDirs = append(started.addDirs, argv[i+1])
			}
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
		`"method":"turn/start"`:    `{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn-1","status":"inProgress","items":[]}}}`,
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
							Hooks          map[string]any `json:"hooks"`
							FeatureMemory  *bool          `json:"features.memories"`
							GenerateMemory *bool          `json:"memories.generate_memories"`
						} `json:"config"`
					} `json:"params"`
				}
				if json.Unmarshal([]byte(line), &request) != nil || (request.Method != "thread/start" && request.Method != "thread/resume") {
					continue
				}
				config := request.Params.Config
				started = &startedSession{memoryOff: config.FeatureMemory != nil && !*config.FeatureMemory && config.GenerateMemory != nil && !*config.GenerateMemory}
				started.hooks, started.prompts = memoryHookCommands(t, config.Hooks)
				if request.Params.DeveloperInstructions != nil {
					started.block = *request.Params.DeveloperInstructions
				}
				// Each hook is trusted by its own hash, and nothing else.
				state, _ := request.Params.Config.Hooks["state"].(map[string]any)
				if len(state) != len(started.hooks)+len(started.prompts) {
					t.Fatalf("hook trust state %v for %d hooks", state, len(started.hooks)+len(started.prompts))
				}
				for event, commands := range map[string][]string{"session_start": started.hooks, "user_prompt_submit": started.prompts} {
					for i := range commands {
						if _, ok := state["/<session-flags>/config.toml:"+event+":"+strconv.Itoa(i)+":0"]; !ok {
							t.Fatalf("%s hook %d is not trusted: %v", event, i, state)
						}
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
		// A turn carries the folders it may write.
		if err := app.SendMessage(threadID, "hello", nil); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		started.dirs = codexTurnWritableRoots(t, logPath)
		if err := app.StopSession(threadID); err != nil {
			t.Fatalf("StopSession: %v", err)
		}
		return *started
	}
}

// codexWritesTheWorkspace puts thread in the runtime mode whose Codex sandbox
// is workspace-write, the one that names the folders outside the workspace
// a turn may write.
func codexWritesTheWorkspace(t *testing.T, app *App, thread store.Thread) {
	t.Helper()
	thread.RuntimeMode = string(provider.RuntimeAutoAcceptEdits)
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatal(err)
	}
}

// codexTurnWritableRoots waits for the first turn/start in logPath and
// returns the writable roots of its sandbox policy.
func codexTurnWritableRoots(t *testing.T, logPath string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, _ := os.ReadFile(logPath)
		for _, line := range strings.Split(string(raw), "\n") {
			var request struct {
				Method string `json:"method"`
				Params struct {
					SandboxPolicy struct {
						WritableRoots []string `json:"writableRoots"`
					} `json:"sandboxPolicy"`
				} `json:"params"`
			}
			if json.Unmarshal([]byte(line), &request) == nil && request.Method == "turn/start" {
				return request.Params.SandboxPolicy.WritableRoots
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no turn/start reached the mock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// memoryHookSession is the provider session the tests' hook runs record
// what they delivered for.
const memoryHookSession = "provider-session-1"

// runMemoryHook runs the one SessionStart memory hook command as both
// providers do, through a POSIX shell with the hook input on stdin, and
// returns what it delivered without the stamp under its heading.
func runMemoryHook(t *testing.T, commands []string) string {
	t.Helper()
	context := runHookCommand(t, commands, "SessionStart", `{"hook_event_name":"SessionStart","source":"startup","session_id":"`+memoryHookSession+`"}`)
	// Each delivery carries its stamp on the line under its heading.
	heading, rest, _ := strings.Cut(context, "\n")
	stampLine, body, _ := strings.Cut(rest, "\n")
	if heading != memory.Heading || !strings.HasPrefix(stampLine, memory.StampPrefix) {
		t.Fatalf("hook output %q has no stamp under its heading", context)
	}
	return heading + "\n" + body
}

// runPromptHook runs the one UserPromptSubmit memory hook command as both
// providers do and returns what it delivered, "" for nothing.
func runPromptHook(t *testing.T, commands []string) string {
	t.Helper()
	return runHookCommand(t, commands, "UserPromptSubmit", `{"hook_event_name":"UserPromptSubmit","prompt":"next","session_id":"`+memoryHookSession+`"}`)
}

// runHookCommand runs the one command of event with input and returns the
// context its output carries, "" for no output.
func runHookCommand(t *testing.T, commands []string, event, input string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the providers run hooks through a POSIX shell")
	}
	if len(commands) != 1 {
		t.Fatalf("%s hook commands = %q, want the one memory hook", event, commands)
	}
	cmd := exec.Command("/bin/sh", "-c", commands[0])
	cmd.Stdin = strings.NewReader(input)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("hook %s: %v: %s", commands[0], err, stderr.String())
	}
	if len(stdout) == 0 {
		return ""
	}
	var output struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(stdout, &output); err != nil || output.HookSpecificOutput.HookEventName != event {
		t.Fatalf("hook output %q: %v", stdout, err)
	}
	return output.HookSpecificOutput.AdditionalContext
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
				codexWritesTheWorkspace(t, app, thread)
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

			project, err := app.store.GetProject(thread.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			projectDir, err := memory.ProjectDir(app.configDir, app.credentialHomeOverride, project.Path)
			if err != nil {
				t.Fatal(err)
			}
			homeMemory := filepath.Join(row.Home, "memory")

			// ---- The block names no file's text; the hook delivers the
			// instructions and the home's facts, as the app builds them.
			writeHomeFile(t, row.Home, agenthome.InstructionsFile, homeContentsRole+"\nGrants: none.\n")
			writeHomeFile(t, row.Home, "memory/a.md", homeFact("Deploys go through CI."))
			first := start(t)
			if got := blockOf(first.block); got != want {
				t.Fatalf("first start's block:\n%s\nwant:\n%s", got, want)
			}
			delivered := runMemoryHook(t, first.hooks)
			if got, want := memoryBlock(t, delivered, "### Your standing instructions"), "### Your standing instructions ("+filepath.Join(row.Home, "instructions.md")+")\n"+homeContentsRole+"\nGrants: none."; got != want {
				t.Fatalf("instructions block:\n%s\nwant:\n%s", got, want)
			}
			if got, want := memoryBlock(t, delivered, "### Your memory"), "### Your memory ("+homeMemory+"/)\nRulings:\n- Deploys go through CI. (a.md)"; got != want {
				t.Fatalf("home index:\n%s\nwant:\n%s", got, want)
			}
			if !strings.Contains(delivered, "\n\n### Project memory: "+project.Name+" ("+projectDir+"/)\nNo facts yet.") {
				t.Fatalf("no project index for %s:\n%s", projectDir, delivered)
			}
			plan, ok, err := app.memoryPlan(thread, &row)
			if err != nil || !ok {
				t.Fatalf("memoryPlan = %v, %v", ok, err)
			}
			if built, _ := memory.Build(plan, time.Now()); delivered != built {
				t.Fatalf("the hook delivered what the app does not build:\n%s\nbuilt:\n%s", delivered, built)
			}
			// The session may write every memory folder and the project:
			// Claude through its settings' additional directories, never
			// `--add-dir`, which would load skills from them; Codex as the
			// writable roots of each turn.
			for _, dir := range []string{memory.UserDir(app.configDir), projectDir, homeMemory, project.Path} {
				if !slices.Contains(first.dirs, dir) {
					t.Errorf("the writable folders lack %s: %q", dir, first.dirs)
				}
				if slices.Contains(first.addDirs, dir) {
					t.Errorf("%s is an --add-dir: %q", dir, first.addDirs)
				}
			}
			if first.memoryOff {
				t.Error("the provider's own memory was turned off by default")
			}

			// ---- A prompt after the start delivers nothing while nothing
			// changed, then only the change, through the records the
			// start's run wrote.
			if got := runPromptHook(t, first.prompts); got != "" {
				t.Fatalf("a prompt with nothing changed delivered:\n%s", got)
			}
			writeHomeFile(t, row.Home, "memory/b.md", homeFact("Release notes live in the issue."))
			changed := runPromptHook(t, first.prompts)
			if !strings.HasPrefix(changed, memory.ChangesHeading+"\n") || !strings.HasSuffix(changed, memory.ChangesEnding) ||
				!strings.Contains(changed, "\nNew or changed:\n- ruling: Release notes live in the issue. (b.md)") ||
				strings.Contains(changed, "Deploys go through CI.") || strings.Contains(changed, homeContentsRole) {
				t.Fatalf("a prompt after an edit delivered:\n%s", changed)
			}
			if got := runPromptHook(t, first.prompts); got != "" {
				t.Fatalf("the change was delivered twice:\n%s", got)
			}

			// ---- An edit changes nothing the session starts with, so the
			// provider's prompt cache holds, and reaches the next delivery.
			if got := runMemoryHook(t, first.hooks); !strings.Contains(got, "- Release notes live in the issue. (b.md)") {
				t.Fatalf("after the edit the hook delivered:\n%s", got)
			}
			second := start(t)
			if second.block != first.block || !slices.Equal(second.hooks, first.hooks) || !slices.Equal(second.prompts, first.prompts) {
				t.Fatalf("a start after the edit differs:\n%q %q %q\n%q %q %q", first.block, first.hooks, first.prompts, second.block, second.hooks, second.prompts)
			}

			// ---- Past the limit nothing is cut: the delivery opens with
			// its size, so the start a provider shows carries it.
			long := homeContentsRole + "\n" + strings.Repeat(strings.Repeat("x", 99)+"\n", 120) + "INSTRUCTIONS TAIL\n"
			writeHomeFile(t, row.Home, agenthome.InstructionsFile, long)
			whole := runMemoryHook(t, first.hooks)
			if !strings.Contains(whole, "INSTRUCTIONS TAIL") || !strings.HasPrefix(whole, memory.Heading+"\n\n[This memory is ") {
				t.Fatalf("a delivery over the limit:\n%s", whole)
			}
			if third := start(t); third.block != first.block {
				t.Fatalf("the long file changed the block:\n%s", third.block)
			}

			// ---- The provider's own memory goes off by its setting.
			off := map[string]any{"claudeNativeMemoryDisabled": true}
			if providerName == string(provider.Codex) {
				off = map[string]any{"codexNativeMemoryDisabled": true}
			}
			if _, err := app.settings.Update(off); err != nil {
				t.Fatal(err)
			}
			if !start(t).memoryOff {
				t.Fatal("the setting did not turn the provider's own memory off")
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
			got := runMemoryHook(t, started.hooks)
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

// A compaction runs the memory hook again, so a home file that became
// unreadable after the start is reported then, and only for the thread's
// own compaction.
func TestStandingAgentCompactionReportsAnUnreadableHomeFile(t *testing.T) {
	t.Parallel()
	app, thread, home, argvPath := unreadableHomeFixture(t)
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	spawnedClaudeArgv(t, argvPath)
	t.Cleanup(func() { _ = app.StopSession(thread.ID) })
	live, ok := app.sessionManager().get(thread.ID)
	if !ok || live.MemoryPlan == nil {
		t.Fatalf("the live session holds no memory plan: %+v", live)
	}
	if homeWarningRows(t, app, thread.ID) != 0 {
		t.Fatal("the start reported a readable home")
	}
	path := filepath.Join(home, agenthome.InstructionsFile)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	handler := startedSessionEventHandler(app, thread.ID, live.Token, string(provider.Claude))
	handler(provider.ProviderEvent{Kind: provider.EventCompactBoundary, ThreadID: thread.ID, ParentToolUseID: "toolu_child", Timestamp: time.Now()})
	waitProviderEvents(t, app, thread.ID)
	if homeWarningRows(t, app, thread.ID) != 0 {
		t.Fatal("a subagent's compaction reported the home")
	}
	handler(provider.ProviderEvent{Kind: provider.EventCompactBoundary, ThreadID: thread.ID, Timestamp: time.Now()})
	waitProviderEvents(t, app, thread.ID)
	requireHomeWarningRow(t, app, thread.ID, path, "not a regular file")

	// An ordinary thread's compaction, with shared memory off, reads no
	// memory.
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

// The hook is not built from a path it could not run with, and the reason is
// a problem the start reports.
func TestMemoryHookRefusesWhatItCannotRun(t *testing.T) {
	t.Parallel()
	plan := memory.Plan{User: t.TempDir(), Home: t.TempDir(), Records: t.TempDir()}
	exe := filepath.Join(t.TempDir(), "agent-overflow")
	if hook, err := memoryHook(memory.Plan{User: plan.User, Home: plan.Home}, func() (string, error) { return exe, nil }); hook != nil ||
		err == nil || err.Error() != "build the UserPromptSubmit hook that delivers changes to memory: a prompt's memory hook needs the records folder" {
		t.Fatalf("a plan without records: %v, %v", hook, err)
	}
	if hook, err := memoryHook(plan, func() (string, error) { return "", errors.New("no /proc") }); hook != nil ||
		err == nil || err.Error() != "locate this executable for the hook that delivers memory: no /proc" {
		t.Fatalf("a failed lookup: %v, %v", hook, err)
	}
	if hook, err := memoryHook(plan, func() (string, error) { return "agent-overflow", nil }); hook != nil ||
		err == nil || !strings.HasPrefix(err.Error(), "build the SessionStart hook that delivers memory: ") {
		t.Fatalf("a relative executable: %v, %v", hook, err)
	}
	hook, err := memoryHook(plan, func() (string, error) { return exe, nil })
	if err != nil || hook == nil {
		t.Fatalf("hook = %+v, %v", hook, err)
	}
	start, _ := memory.HookCommand(exe, memory.AtSessionStart, plan)
	prompt, _ := memory.HookCommand(exe, memory.AtPrompt, plan)
	if *hook != (provider.SessionStartHook{
		Label: memory.Label, Command: start, PromptCommand: prompt, Heading: memory.Heading, Ending: memory.Ending,
		Stamp: memory.StampPrefix, Failure: memory.HookSubcommand + ":",
	}) {
		t.Errorf("hook = %+v", *hook)
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
	writeHomeFile(t, row.Home, "memory/a.md", homeFact("index line"))
	return app, thread, row.Home, argvPath
}

// homeWarningRows counts the thread's home warning rows.
func homeWarningRows(t *testing.T, app *App, threadID string) int {
	t.Helper()
	app.waitMemoryReports()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, item := range items {
		if strings.Contains(item.Summary, "Part of this conversation's memory could not be read, and its sessions receive a notice about it: ") {
			n++
		}
	}
	return n
}

func requireHomeWarningRow(t *testing.T, app *App, threadID, path, why string) {
	t.Helper()
	app.waitMemoryReports()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if strings.Contains(item.Summary, "Part of this conversation's memory could not be read, and its sessions receive a notice about it: ") &&
			strings.Contains(item.Summary, path) && strings.Contains(item.Summary, why) {
			return
		}
	}
	t.Fatalf("no warning row naming %s (%s) in %+v", path, why, items)
}

// homeAtTheRawBound is an instructions.md the read stops at the 1 MiB raw
// bound: two MiB of NUL, which cleaning removes.
func homeAtTheRawBound(t *testing.T, home string) {
	t.Helper()
	writeHomeFile(t, home, agenthome.InstructionsFile, strings.Repeat("\x00", 2*agenthome.MaxRawRead))
}

// A home file the raw bound stops starts the session, from StartSession and
// from a queued notice's start, and the delivery names its path in its
// place.
func TestAHomeFileStoppedByTheRawBoundStartsTheSession(t *testing.T) {
	t.Parallel()
	notice := "[instructions.md is 2097152 bytes, more than a delivery carries. Read it from "

	app, thread, home, argvPath := unreadableHomeFixture(t)
	homeAtTheRawBound(t, home)
	if got := runMemoryHook(t, claudeStarted(app, thread.ID, argvPath)(t).hooks); !strings.Contains(got, notice) || !strings.Contains(got, "index line") {
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
	hooks, _ := memoryHookCommands(t, block.Hooks)
	if got := runMemoryHook(t, hooks); !strings.Contains(got, notice) {
		t.Fatalf("the notice's start delivered:\n%s", got)
	}
	if err := queued.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
}
