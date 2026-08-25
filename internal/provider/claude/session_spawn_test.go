package claude

import (
	"os"
	"slices"
	"testing"
)

// -- Session unit tests (wire format verification) --

func TestBuildArgsDefault(t *testing.T) {
	args := buildArgs(Config{}, "")

	// Baseline flags that every spawn must include. Adding a new flag to
	// buildArgs should extend this list intentionally.
	expected := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
		"--include-partial-messages",
		"--replay-user-messages",
		"--forward-subagent-text",
		"--session-mirror",
		"--thinking-display", "summarized",
	}
	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("args[%d]: got %q, want %q", i, args[i], want)
		}
	}
}

// An agent launched with an explicit `run_in_background: false` runs the
// CLI's synchronous Task path, whose progress emitter drops every content
// block that is not a tool_use or tool_result before it reaches the parent
// stream. Without this flag such an agent's prose, thinking and final
// answer never leave the CLI, and its pane is a wall of tool rows.
//
// The flag is unconditional: it only relaxes a filter, an agent that
// already forwards everything is unaffected, and the CLI has carried it
// since 2.1.211 (bisected against published linux-x64 builds), below the
// supported floor.
func TestBuildArgsForwardsSubagentText(t *testing.T) {
	for _, cfg := range []Config{{}, {Resume: "sess-1"}, {Model: "opus"}} {
		args := buildArgs(cfg, "")
		if !slices.Contains(args, "--forward-subagent-text") {
			t.Fatalf("spawn args omit --forward-subagent-text: %v", args)
		}
	}
}

func TestBuildArgsOmitsResumeAtWithoutResume(t *testing.T) {
	args := buildArgs(Config{ResumeAt: "leaf-456"}, "")
	for _, arg := range args {
		if arg == "--resume-session-at" {
			t.Fatalf("args include --resume-session-at without --resume: %v", args)
		}
	}
}

func TestBuildArgsWithAllOptions(t *testing.T) {
	cfg := Config{
		Model:           "opus",
		Resume:          "session-123",
		ResumeAt:        "leaf-456",
		ForkSession:     true,
		SystemPrompt:    "Be helpful",
		OutputSchema:    `{"type":"object"}`,
		PermissionFlags: []string{"--permission-mode", "acceptEdits"},
		MaxTurns:        5,
		AllowedTools:    []string{"Bash", "Edit"},
	}
	systemPromptPath, err := WriteSystemPromptFile(cfg.SystemPrompt)
	if err != nil {
		t.Fatalf("WriteSystemPromptFile() error = %v", err)
	}
	t.Cleanup(func() { RemoveSystemPromptFile(systemPromptPath) })
	args := buildArgs(cfg, systemPromptPath)

	// Check that all flags are present.
	findFlag := func(flag, value string) bool {
		for i, a := range args {
			if a == flag && i+1 < len(args) && args[i+1] == value {
				return true
			}
		}
		return false
	}

	if !findFlag("--model", "opus") {
		t.Error("missing --model opus")
	}
	if !findFlag("--resume", "session-123") {
		t.Error("missing --resume session-123")
	}
	if !findFlag("--resume-session-at", "leaf-456") {
		t.Error("missing --resume-session-at leaf-456")
	}
	foundForkFlag := false
	for _, arg := range args {
		if arg == "--fork-session" {
			foundForkFlag = true
			break
		}
	}
	if !foundForkFlag {
		t.Error("missing --fork-session")
	}
	// The prompt travels by path, never in argv (MAX_ARG_STRLEN + /proc —
	// see WriteSystemPromptFile), so the flag is only half the assertion:
	// what the CLI will actually read is the file's content.
	if !findFlag("--system-prompt-file", systemPromptPath) {
		t.Errorf("missing --system-prompt-file %s: %v", systemPromptPath, args)
	}
	if slices.Contains(args, "--system-prompt") {
		t.Errorf("argv still carries --system-prompt: %v", args)
	}
	written, err := os.ReadFile(systemPromptPath)
	if err != nil {
		t.Fatalf("read system prompt file: %v", err)
	}
	if string(written) != "Be helpful" {
		t.Errorf("system prompt file content = %q, want %q", written, "Be helpful")
	}
	if !findFlag("--json-schema", `{"type":"object"}`) {
		t.Error("missing --json-schema inline JSON")
	}
	if !findFlag("--permission-mode", "acceptEdits") {
		t.Error("missing --permission-mode acceptEdits")
	}
	if !findFlag("--max-turns", "5") {
		t.Error("missing --max-turns 5")
	}
	if !findFlag("--allowedTools", "Bash") {
		t.Error("missing --allowedTools Bash")
	}
	if !findFlag("--allowedTools", "Edit") {
		t.Error("missing --allowedTools Edit")
	}
}

// The system-prompt file is the one spawn artifact AO leaves on disk, so
// its two properties are pinned: a session without an override writes
// nothing at all, and the file a session DOES write is readable only by the
// user running AO (the prompt carries workspace paths and git state).
func TestWriteSystemPromptFile(t *testing.T) {
	path, err := WriteSystemPromptFile("")
	if err != nil {
		t.Fatalf("WriteSystemPromptFile(\"\") error = %v", err)
	}
	if path != "" {
		RemoveSystemPromptFile(path)
		t.Fatalf("WriteSystemPromptFile(\"\") = %q, want no file for a session with no override", path)
	}
	if args := buildArgs(Config{}, ""); slices.Contains(args, "--system-prompt-file") {
		t.Errorf("argv carries --system-prompt-file without a prompt: %v", args)
	}

	path, err = WriteSystemPromptFile("secret prompt")
	if err != nil {
		t.Fatalf("WriteSystemPromptFile() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat system prompt file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("system prompt file mode = %o, want 0600", perm)
	}

	RemoveSystemPromptFile(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("system prompt file still present after removal: %v", err)
	}
	// Removal is idempotent: Close runs after a failed-spawn path may
	// already have cleaned up.
	RemoveSystemPromptFile(path)
}

func TestBuildArgsNoPermissionFlagsOmitsAll(t *testing.T) {
	args := buildArgs(Config{PermissionFlags: nil}, "")

	for _, a := range args {
		if a == "--permission-mode" || a == "--allow-dangerously-skip-permissions" {
			t.Errorf("permission flag %q should be omitted when PermissionFlags is nil", a)
		}
	}
}

// TestBuildArgsDangerousSkipPermissions confirms the full-access flow emits
// the bypass permission mode plus the bare dangerous-skip allow flag.
func TestBuildArgsDangerousSkipPermissions(t *testing.T) {
	args := buildArgs(Config{PermissionFlags: []string{"--permission-mode", "bypassPermissions", "--allow-dangerously-skip-permissions"}}, "")
	found := false
	for i, a := range args {
		if a != "--allow-dangerously-skip-permissions" {
			continue
		}
		found = true
		// Next arg should not be a companion value — either end of slice
		// or another flag.
		if i+1 < len(args) && !isFlagToken(args[i+1]) {
			t.Errorf("--allow-dangerously-skip-permissions should not carry a value; got %q", args[i+1])
		}
	}
	if !found {
		t.Errorf("expected --allow-dangerously-skip-permissions in args: %v", args)
	}
}

// TestWithClaudeSessionEnvDefaults pins the env-merge helper that tags
// every spawned `claude` subprocess with
// `CLAUDE_CODE_ENTRYPOINT=agent-overflow` and opts it into the todo
// tool surface with `CLAUDE_CODE_ENABLE_TODO_TOOLS=true`.
//
// The CLI's resume picker filters sessions whose entrypoint is `sdk-cli`
// (the auto-detected default for stream-json invocations); setting our
// own value keeps agent-overflow's threads resumable from a normal
// `claude --resume`. See docs/references/claude.md and the `Ka8(H)`
// override in the binary that rewrites the literal string `"cli"` to
// `"sdk-cli"` — any other preset value survives. The todo opt-in exists
// because claude ≥2.1.233 removes TodoWrite/Task* for modern models
// unless the session opts back in (claudeTodoToolsEnvVar's comment has
// the gate details).
func TestWithClaudeSessionEnvDefaults(t *testing.T) {
	t.Run("nil env gets both defaults set", func(t *testing.T) {
		got := withClaudeSessionEnvDefaults(nil, false)
		if got["CLAUDE_CODE_ENTRYPOINT"] != "agent-overflow" {
			t.Fatalf("CLAUDE_CODE_ENTRYPOINT = %q, want agent-overflow", got["CLAUDE_CODE_ENTRYPOINT"])
		}
		if got["CLAUDE_CODE_ENABLE_TODO_TOOLS"] != "true" {
			t.Fatalf("CLAUDE_CODE_ENABLE_TODO_TOOLS = %q, want true", got["CLAUDE_CODE_ENABLE_TODO_TOOLS"])
		}
	})

	t.Run("preserves caller-provided keys", func(t *testing.T) {
		got := withClaudeSessionEnvDefaults(map[string]string{"FOO": "bar"}, false)
		if got["FOO"] != "bar" {
			t.Errorf("FOO clobbered: got %q, want bar", got["FOO"])
		}
		if got["CLAUDE_CODE_ENTRYPOINT"] != "agent-overflow" {
			t.Errorf("CLAUDE_CODE_ENTRYPOINT not added: got %q", got["CLAUDE_CODE_ENTRYPOINT"])
		}
		if got["CLAUDE_CODE_ENABLE_TODO_TOOLS"] != "true" {
			t.Errorf("CLAUDE_CODE_ENABLE_TODO_TOOLS not added: got %q", got["CLAUDE_CODE_ENABLE_TODO_TOOLS"])
		}
	})

	t.Run("respects caller-provided overrides per variable", func(t *testing.T) {
		got := withClaudeSessionEnvDefaults(map[string]string{
			"CLAUDE_CODE_ENTRYPOINT": "test-override",
		}, false)
		if got["CLAUDE_CODE_ENTRYPOINT"] != "test-override" {
			t.Errorf("entrypoint override clobbered: got %q, want test-override", got["CLAUDE_CODE_ENTRYPOINT"])
		}
		// The other default still applies — opting out of one variable
		// must not opt out of the rest.
		if got["CLAUDE_CODE_ENABLE_TODO_TOOLS"] != "true" {
			t.Errorf("CLAUDE_CODE_ENABLE_TODO_TOOLS not added alongside an entrypoint override: got %q", got["CLAUDE_CODE_ENABLE_TODO_TOOLS"])
		}
	})

	t.Run("user can disable the todo opt-in", func(t *testing.T) {
		got := withClaudeSessionEnvDefaults(map[string]string{
			"CLAUDE_CODE_ENABLE_TODO_TOOLS": "false",
		}, false)
		if got["CLAUDE_CODE_ENABLE_TODO_TOOLS"] != "false" {
			t.Errorf("todo opt-out clobbered: got %q, want false", got["CLAUDE_CODE_ENABLE_TODO_TOOLS"])
		}
	})

	t.Run("returns the same map when every default is present", func(t *testing.T) {
		input := map[string]string{
			"CLAUDE_CODE_ENTRYPOINT":        "x",
			"CLAUDE_CODE_ENABLE_TODO_TOOLS": "false",
		}
		got := withClaudeSessionEnvDefaults(input, false)
		// Maps are reference types: a write through the return proves it
		// is the caller's map, not a pointless copy.
		got["PROBE"] = "1"
		if input["PROBE"] != "1" {
			t.Errorf("expected the input map back unchanged when nothing is missing")
		}
	})

	t.Run("does not mutate caller's map", func(t *testing.T) {
		input := map[string]string{"FOO": "bar"}
		_ = withClaudeSessionEnvDefaults(input, false)
		if len(input) != 1 {
			t.Errorf("input map was mutated; helper must return a copy")
		}
	})

	t.Run("reminders disabled exports the off mode", func(t *testing.T) {
		got := withClaudeSessionEnvDefaults(nil, true)
		if got["CLAUDE_CODE_TODO_REMINDER_MODE"] != "off" {
			t.Fatalf("CLAUDE_CODE_TODO_REMINDER_MODE = %q with reminders disabled, want off", got["CLAUDE_CODE_TODO_REMINDER_MODE"])
		}
	})

	t.Run("reminders enabled leaves the mode to the CLI", func(t *testing.T) {
		got := withClaudeSessionEnvDefaults(nil, false)
		if v, ok := got["CLAUDE_CODE_TODO_REMINDER_MODE"]; ok {
			t.Fatalf("CLAUDE_CODE_TODO_REMINDER_MODE = %q with reminders enabled, want unset (the CLI owns its default)", v)
		}
	})

	t.Run("user reminder-mode value outranks the setting", func(t *testing.T) {
		got := withClaudeSessionEnvDefaults(map[string]string{
			"CLAUDE_CODE_TODO_REMINDER_MODE": "baseline",
		}, true)
		if got["CLAUDE_CODE_TODO_REMINDER_MODE"] != "baseline" {
			t.Errorf("user reminder mode clobbered: got %q, want baseline", got["CLAUDE_CODE_TODO_REMINDER_MODE"])
		}
	})
}
