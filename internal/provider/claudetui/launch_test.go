package claudetui

import (
	"os"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// hasArgPair reports whether args contains flag immediately followed by value.
func hasArgPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestBuildLaunchOptionsEnablesThinkingDisplay guards the thinking fix: the
// interactive TUI must launch with `--thinking-display summarized`, or Opus 4.7+
// defaults thinking.display to `omitted` and the wire carries an empty
// signature-only thinking block (no thinking_delta) — the reconstruction then
// has nothing to surface, and neither AO nor the TUI shows any thinking. The flag
// is LIVE-confirmed against 2.1.170 in the CLI capture spike thinking_title.
func TestBuildLaunchOptionsEnablesThinkingDisplay(t *testing.T) {
	cfg := Config{Reaper: procutiltest.Reaper(), Binary: "claude", WorkDir: t.TempDir(), HookCmd: "/tmp/ao-exe"}
	opts, err := buildLaunchOptions(withTestRetention(cfg), "", "", "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
	if err != nil {
		t.Fatalf("buildLaunchOptions: %v", err)
	}
	if !hasArgPair(opts.Args, "--thinking-display", "summarized") {
		t.Errorf("launch args missing --thinking-display summarized; got %v", opts.Args)
	}
}

// TestBuildLaunchOptionsPassesEffort guards the effort fix: when the session
// carries a reasoning effort, the interactive TUI must launch with
// `--effort <level>`. Without it the TUI falls back to the model's default tier
// (xhigh on opus-4-8) and the AO effort selection is silently ignored — the
// reported bug. Same global flag headless passes (provider/claude/session.go).
func TestBuildLaunchOptionsPassesEffort(t *testing.T) {
	cfg := Config{Reaper: procutiltest.Reaper(), Binary: "claude", WorkDir: t.TempDir(), HookCmd: "/tmp/ao-exe", ReasoningEffort: "high"}
	opts, err := buildLaunchOptions(withTestRetention(cfg), "", "", "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
	if err != nil {
		t.Fatalf("buildLaunchOptions: %v", err)
	}
	if !hasArgPair(opts.Args, "--effort", "high") {
		t.Errorf("launch args missing --effort high; got %v", opts.Args)
	}
}

// TestBuildLaunchOptionsOmitsEffortWhenUnset: a session with no effort selection
// must NOT pass --effort, so the CLI keeps its own default rather than receiving
// an empty value.
func TestBuildLaunchOptionsOmitsEffortWhenUnset(t *testing.T) {
	cfg := Config{Reaper: procutiltest.Reaper(), Binary: "claude", WorkDir: t.TempDir(), HookCmd: "/tmp/ao-exe"}
	opts, err := buildLaunchOptions(withTestRetention(cfg), "", "", "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
	if err != nil {
		t.Fatalf("buildLaunchOptions: %v", err)
	}
	for i, a := range opts.Args {
		if a == "--effort" {
			t.Errorf("launch args unexpectedly contain --effort (at index %d); got %v", i, opts.Args)
		}
	}
}

// The system-prompt override reaches the interactive TUI as
// `--system-prompt-file <path>` — the flag the 2.1.234 PTY spike proved the
// TUI honors — and the file, not argv, is what carries the text. Both halves
// are asserted: a flag pointing at a file with the wrong content replaces the
// prompt with the wrong prompt, which no flag-only test would catch.
func TestBuildLaunchOptionsPassesTheSystemPromptFile(t *testing.T) {
	const prompt = "You are the agent.\nWork in the repo."
	path, err := claude.WriteSystemPromptFile(t.TempDir(), prompt)
	if err != nil {
		t.Fatalf("WriteSystemPromptFile() error = %v", err)
	}
	t.Cleanup(func() { claude.RemoveSystemPromptFile(path) })

	cfg := Config{Reaper: procutiltest.Reaper(), Binary: "claude", WorkDir: t.TempDir(), HookCmd: "/tmp/ao-exe", SystemPrompt: prompt}
	opts, err := buildLaunchOptions(withTestRetention(cfg), path, "", "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
	if err != nil {
		t.Fatalf("buildLaunchOptions: %v", err)
	}
	if !hasArgPair(opts.Args, "--system-prompt-file", path) {
		t.Fatalf("launch args missing --system-prompt-file %s; got %v", path, opts.Args)
	}
	if slices.Contains(opts.Args, prompt) {
		t.Fatalf("the prompt text reached argv; got %v", opts.Args)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	if string(written) != prompt {
		t.Fatalf("system prompt file = %q, want %q", written, prompt)
	}
}

// A session with no override must pass no flag at all, so the CLI keeps its
// own prompt rather than being handed an empty file path.
func TestBuildLaunchOptionsOmitsTheSystemPromptFlagWithoutAnOverride(t *testing.T) {
	cfg := Config{Reaper: procutiltest.Reaper(), Binary: "claude", WorkDir: t.TempDir(), HookCmd: "/tmp/ao-exe"}
	opts, err := buildLaunchOptions(withTestRetention(cfg), "", "", "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
	if err != nil {
		t.Fatalf("buildLaunchOptions: %v", err)
	}
	if slices.Contains(opts.Args, "--system-prompt-file") {
		t.Fatalf("launch args unexpectedly contain --system-prompt-file; got %v", opts.Args)
	}
}

// One `--disallowedTools <name>` flag per entry, in the configured order.
// The TUI honors the flag the same way headless does (2.1.234 spike): the
// named tools' schemas are absent from the request.
func TestBuildLaunchOptionsPassesOneDisallowedToolsFlagPerName(t *testing.T) {
	cfg := Config{
		Reaper:          procutiltest.Reaper(),
		Binary:          "claude",
		WorkDir:         t.TempDir(),
		HookCmd:         "/tmp/ao-exe",
		DisallowedTools: []string{"Workflow", "WebSearch"},
	}
	opts, err := buildLaunchOptions(withTestRetention(cfg), "", "", "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
	if err != nil {
		t.Fatalf("buildLaunchOptions: %v", err)
	}
	for _, tool := range cfg.DisallowedTools {
		if !hasArgPair(opts.Args, "--disallowedTools", tool) {
			t.Fatalf("launch args missing --disallowedTools %s; got %v", tool, opts.Args)
		}
	}
	if got := strings.Count(strings.Join(opts.Args, "\x00"), "--disallowedTools"); got != 2 {
		t.Fatalf("--disallowedTools appears %d times, want one per name; got %v", got, opts.Args)
	}
}

// The app's MCP servers reach the interactive claude through the same
// `--mcp-config` payload the headless provider sends, with the transport type
// backfilled, and each allowed tool as its own `--allowedTools` value. No
// servers means no flag.
func TestBuildLaunchOptionsPassesMCPServersAndAllowedTools(t *testing.T) {
	cfg := Config{
		Binary:       "claude",
		WorkDir:      t.TempDir(),
		HookCmd:      "/tmp/ao-exe",
		MCPServers:   map[string]any{"ao-workflow-tools": map[string]any{"url": "http://127.0.0.1:9/mcp/x"}},
		AllowedTools: []string{"mcp__ao-workflow-tools__*", "-bad"},
	}
	opts, err := buildLaunchOptions(withTestRetention(cfg), "", "", "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
	if err != nil {
		t.Fatalf("buildLaunchOptions: %v", err)
	}
	want := `{"mcpServers":{"ao-workflow-tools":{"type":"http","url":"http://127.0.0.1:9/mcp/x"}}}`
	if !hasArgPair(opts.Args, "--mcp-config", want) {
		t.Fatalf("launch args missing --mcp-config %s; got %v", want, opts.Args)
	}
	if !hasArgPair(opts.Args, "--allowedTools", "mcp__ao-workflow-tools__*") {
		t.Fatalf("launch args missing the workflow tools allowlist entry; got %v", opts.Args)
	}
	// The allowlist goes through the headless provider's argv boundary: a
	// value that would read as a flag is dropped, not passed.
	if got := strings.Count(strings.Join(opts.Args, "\x00"), "--allowedTools"); got != 1 {
		t.Fatalf("--allowedTools appears %d times, want 1; got %v", got, opts.Args)
	}

	bare := Config{Binary: "claude", WorkDir: t.TempDir(), HookCmd: "/tmp/ao-exe"}
	opts, err = buildLaunchOptions(withTestRetention(bare), "", "", "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
	if err != nil {
		t.Fatalf("buildLaunchOptions: %v", err)
	}
	if slices.Contains(opts.Args, "--mcp-config") || slices.Contains(opts.Args, "--allowedTools") {
		t.Fatalf("a session with no servers carries MCP flags: %v", opts.Args)
	}
}

// countEnv returns how many entries of env carry the given key, and the last
// value seen. A duplicated key would leave which value wins to exec-env
// ordering semantics, so tests assert exactly one.
func countEnv(env []string, key string) (int, string) {
	count, last := 0, ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			count++
			last = v
		}
	}
	return count, last
}

// tuiEnv is the environment buildEnv gives a TUI child for cfg.
func tuiEnv(cfg Config) []string {
	line, err := claude.NewCommandLine(claude.UserHomeRetention(0), "")
	if err != nil {
		panic(err)
	}
	cfg.Reaper = procutiltest.Reaper()
	return buildEnv(line, cfg, "http://gw", "http://hook", "tok")
}

// unsetForTest removes name from the process environment for t, so a value
// the host exported cannot stand in for the default under test.
func unsetForTest(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

// custom is a Config.Env holding only the user's custom environment.
func custom(kv ...string) provider.EnvInput {
	m := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return provider.EnvInput{Custom: m}
}

// TestBuildEnvDefaultsTodoToolsOptIn pins the claude >=2.1.233 adaptation on
// the TUI path: the session defaults into the todo tool surface, but a value
// in the user's custom provider environment wins. Serial: it sets the process
// environment.
func TestBuildEnvDefaultsTodoToolsOptIn(t *testing.T) {
	todoTools := provider.ClaudeTodoToolsEnv.Name()
	unsetForTest(t, todoTools)
	t.Run("default applied when absent", func(t *testing.T) {
		env := tuiEnv(Config{Env: custom("FOO", "bar")})
		if n, v := countEnv(env, todoTools); n != 1 || v != "true" {
			t.Fatalf("%s: got %d entries (last %q), want exactly one =true", todoTools, n, v)
		}
	})

	t.Run("user opt-out survives without a duplicate", func(t *testing.T) {
		env := tuiEnv(Config{Env: custom("FOO", "bar", todoTools, "false")})
		if n, v := countEnv(env, todoTools); n != 1 || v != "false" {
			t.Fatalf("%s: got %d entries (last %q), want exactly the caller's =false", todoTools, n, v)
		}
	})

	t.Run("owned gateway keys still replaced", func(t *testing.T) {
		baseURL := provider.ClaudeBaseURLEnv.Name()
		env := tuiEnv(Config{Env: custom(baseURL, "https://dirty.example")})
		if n, v := countEnv(env, baseURL); n != 1 || v != "http://gw" {
			t.Fatalf("%s: got %d entries (last %q), want exactly the gateway URL", baseURL, n, v)
		}
	})
}

// The one place claude-tui's environment differs from headless Claude's: a
// todo variable exported in the shell Agent Overflow runs under outranks the
// TUI's default, where headless Claude's default outranks it. Serial: it
// sets the process environment.
func TestBuildEnvLeavesAnInheritedTodoValueOverItsDefault(t *testing.T) {
	todoTools := provider.ClaudeTodoToolsEnv.Name()
	reminder := provider.ClaudeTodoReminderModeEnv.Name()
	t.Setenv(todoTools, "false")
	t.Setenv(reminder, "baseline")

	env := tuiEnv(Config{DisableTodoReminders: true})
	if n, v := countEnv(env, todoTools); n != 1 || v != "false" {
		t.Fatalf("%s: got %d entries (last %q), want the inherited =false", todoTools, n, v)
	}
	if n, v := countEnv(env, reminder); n != 1 || v != "baseline" {
		t.Fatalf("%s: got %d entries (last %q), want the inherited =baseline", reminder, n, v)
	}
}

// TestBuildEnvTodoReminderMode pins the Settings nudge toggle's TUI leg:
// disabling reminders exports CLAUDE_CODE_TODO_REMINDER_MODE=off, leaving
// it enabled exports nothing (the CLI owns its default), and a custom value
// outranks the setting. Serial: it sets the process environment.
func TestBuildEnvTodoReminderMode(t *testing.T) {
	reminder := provider.ClaudeTodoReminderModeEnv.Name()
	unsetForTest(t, reminder)
	t.Run("disabled exports off", func(t *testing.T) {
		env := tuiEnv(Config{Env: custom("FOO", "bar"), DisableTodoReminders: true})
		if n, v := countEnv(env, reminder); n != 1 || v != "off" {
			t.Fatalf("%s: got %d entries (last %q), want exactly one =off", reminder, n, v)
		}
	})

	t.Run("enabled exports nothing", func(t *testing.T) {
		env := tuiEnv(Config{Env: custom("FOO", "bar")})
		if n, v := countEnv(env, reminder); n != 0 {
			t.Fatalf("%s: got %d entries (last %q), want none", reminder, n, v)
		}
	})

	t.Run("user value survives without a duplicate", func(t *testing.T) {
		env := tuiEnv(Config{Env: custom(reminder, "baseline"), DisableTodoReminders: true})
		if n, v := countEnv(env, reminder); n != 1 || v != "baseline" {
			t.Fatalf("%s: got %d entries (last %q), want exactly the caller's =baseline", reminder, n, v)
		}
	})
}

// TestBuildEnvOwnsTheTaskListID pins the TUI leg of the thread-keyed task
// list: the variable is stated from Config.TaskListID and an inherited value
// is dropped whether or not there is one to state, so a host that exported
// it can never key a thread's tasks to someone else's list. Serial: it sets
// the process environment.
func TestBuildEnvOwnsTheTaskListID(t *testing.T) {
	taskList := provider.ClaudeTaskListIDEnv.Name()
	t.Setenv(taskList, "inherited")
	t.Run("stated from the config", func(t *testing.T) {
		env := tuiEnv(Config{Env: custom("FOO", "bar"), TaskListID: "thread-1"})
		if n, v := countEnv(env, taskList); n != 1 || v != "thread-1" {
			t.Fatalf("%s: got %d entries (last %q), want exactly one =thread-1", taskList, n, v)
		}
	})

	t.Run("inherited value dropped when the config has none", func(t *testing.T) {
		env := tuiEnv(Config{Env: custom("FOO", "bar")})
		if n, v := countEnv(env, taskList); n != 0 {
			t.Fatalf("%s: got %d entries (last %q), want none", taskList, n, v)
		}
	})
}

// envValueIn reads one variable out of a buildEnv result, reporting presence
// separately from value: for the peer-inbox gate, absent and empty are
// different answers.
func envValueIn(env []string, key string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		name, candidate, ok := strings.Cut(kv, "=")
		if ok && name == key {
			value, found = candidate, true
		}
	}
	return value, found
}

// B7: the TUI drives the same `claude` binary and the same settings file, so
// the peer-inbox POLICY has to reach it the same way — through the
// `--settings` block, which is the only delivery route for a key with no
// flag. Without it a TUI session with the inbox on would fall into the CLI's
// mode-parity default, which holds and then silently drops peer messages.
func TestHookSettingsCarryTheCrossSessionPolicy(t *testing.T) {
	for _, policy := range []string{"accept", "refuse"} {
		cfg := Config{Reaper: procutiltest.Reaper(), CrossSessionInbound: policy}
		rendered, err := hookSettingsJSON(cfg)
		if err != nil {
			t.Fatalf("hookSettingsJSON: %v", err)
		}
		if !strings.Contains(rendered, `"crossSessionInbound":"`+policy+`"`) {
			t.Fatalf("policy %q missing from the settings block: %s", policy, rendered)
		}
		if !strings.Contains(rendered, `"hooks"`) {
			t.Fatalf("policy %q displaced the relay hooks: %s", policy, rendered)
		}
	}
}

// An absent policy states nothing rather than pinning one: the same
// zero-value-means-say-nothing rule the rest of the block follows.
func TestHookSettingsOmitAnEmptyCrossSessionPolicy(t *testing.T) {
	rendered, err := hookSettingsJSON(Config{Reaper: procutiltest.Reaper(), CrossSessionInbound: "   "})
	if err != nil {
		t.Fatalf("hookSettingsJSON: %v", err)
	}
	if strings.Contains(rendered, "crossSessionInbound") {
		t.Fatalf("an empty policy rendered a key: %s", rendered)
	}
}

// The TUI sweeps the same config home a headless session does, so its
// settings block carries the same transcript retention beside the hooks.
func TestLaunchSettingsCarryCleanupPeriodDays(t *testing.T) {
	block := func(retention claude.Retention) string {
		t.Helper()
		opts, err := buildLaunchOptions(Config{Reaper: procutiltest.Reaper(), Binary: "claude", WorkDir: t.TempDir(), HookCmd: "/tmp/ao-exe", Retention: retention},
			"", "", "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
		if err != nil {
			t.Fatalf("buildLaunchOptions: %v", err)
		}
		if got := strings.Count(strings.Join(opts.Args, " "), "--settings"); got != 1 {
			t.Fatalf("want one --settings flag, got %d in %v", got, opts.Args)
		}
		for i, arg := range opts.Args {
			if arg == "--settings" {
				return opts.Args[i+1]
			}
		}
		return ""
	}
	rendered := block(claude.UserHomeRetention(90))
	if !strings.Contains(rendered, `"cleanupPeriodDays":90`) {
		t.Fatalf("retention missing from the settings block: %s", rendered)
	}
	if !strings.Contains(rendered, `"hooks"`) {
		t.Fatalf("retention displaced the relay hooks: %s", rendered)
	}
	if rendered := block(claude.UserHomeRetention(0)); strings.Contains(rendered, "cleanupPeriodDays") {
		t.Fatalf("no period rendered a key: %s", rendered)
	}
}

// A launch with no retention answer is refused.
func TestBuildLaunchOptionsRefusesAnUnansweredRetention(t *testing.T) {
	if _, err := buildLaunchOptions(Config{Reaper: procutiltest.Reaper(), Binary: "claude", WorkDir: t.TempDir(), HookCmd: "/tmp/ao-exe"},
		"", "", "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok"); err == nil {
		t.Fatal("buildLaunchOptions accepted a Config with no Retention")
	}
}

// withTestRetention answers for the user's home with no period when cfg, in
// a test that does not exercise retention, gives no answer.
func withTestRetention(cfg Config) Config {
	if cfg.Retention == (claude.Retention{}) {
		cfg.Retention = claude.UserHomeRetention(0)
	}
	return cfg
}

// B18, TUI half. A host that exported CLAUDE_CODE_HARBOR_KITE must not bind
// the peer inbox for a session whose setting says off: refusing inbound
// messages does not undo being discoverable in every peer's ListAgents.
// Serial: it sets the process environment.
func TestBuildEnvDropsAnInheritedCrossSessionGateWhenDisabled(t *testing.T) {
	gate, name := provider.ClaudeCrossSessionGateEnv.Name(), provider.ClaudePeerSessionNameEnv.Name()
	t.Setenv(gate, "1")
	t.Setenv(name, "someone-elses-name")
	t.Setenv("HOME", "/home/test")
	env := tuiEnv(Config{})

	for _, key := range []string{gate, name} {
		if value, found := envValueIn(env, key); found {
			t.Fatalf("%s = %q survived, want it owned and dropped", key, value)
		}
	}
	if value, _ := envValueIn(env, "HOME"); value != "/home/test" {
		t.Fatalf("HOME = %q, want the inherited value untouched", value)
	}
}

// The TUI runs in the config home the headless session uses: an inherited
// override would move it, and its transcript sweep, to a home the launch's
// retention period was not resolved against. Serial: it sets the process
// environment.
func TestBuildEnvDropsAnInheritedConfigHome(t *testing.T) {
	keys := []string{provider.ClaudeConfigDirEnv.Name(), provider.ClaudeSecureStorageConfigDirEnv.Name()}
	for _, key := range keys {
		t.Setenv(key, "/alternate")
	}
	t.Setenv("HOME", "/home/test")
	env := tuiEnv(Config{})

	for _, key := range keys {
		if value, found := envValueIn(env, key); found {
			t.Fatalf("%s = %q survived, want it dropped", key, value)
		}
	}
	if value, _ := envValueIn(env, "HOME"); value != "/home/test" {
		t.Fatalf("HOME = %q, want the inherited value untouched", value)
	}
}

// Enabled states the gate explicitly, overriding whatever the host carried.
// Serial: it sets the process environment.
func TestBuildEnvStatesTheCrossSessionGateWhenEnabled(t *testing.T) {
	gate := provider.ClaudeCrossSessionGateEnv.Name()
	t.Setenv(gate, "totally-bogus")
	env := tuiEnv(Config{CrossSessionEnabled: true})

	if n, v := countEnv(env, gate); n != 1 || v != "1" {
		t.Fatalf("%s: got %d entries (last %q), want exactly one =1", gate, n, v)
	}
}

// The interactive launch carries the same `--add-dir` the headless one
// does: one binary, one attachments root, and a take-control session must
// not be the surface that prompts for a file AO put there itself.
func TestBuildLaunchOptionsEmitsAddDir(t *testing.T) {
	opts, err := buildLaunchOptions(Config{Reaper: procutiltest.Reaper(), Retention: claude.UserHomeRetention(0),
		Binary:         "claude",
		WorkDir:        "/tmp/work",
		AdditionalDirs: []string{"/var/attachments", "/var/other"},
	}, "", "", "http://127.0.0.1:1/", "http://127.0.0.1:2/hook", "tok")
	if err != nil {
		t.Fatalf("buildLaunchOptions: %v", err)
	}
	var dirs []string
	for i, arg := range opts.Args {
		if arg == "--add-dir" {
			if i+1 >= len(opts.Args) {
				t.Fatal("--add-dir is the last argument, with no directory")
			}
			dirs = append(dirs, opts.Args[i+1])
		}
	}
	if len(dirs) != 2 || dirs[0] != "/var/attachments" || dirs[1] != "/var/other" {
		t.Fatalf("--add-dir values = %v, want both directories in order", dirs)
	}
}

// NativeMemoryOff turns Claude Code's auto memory off through the same
// settings block as the headless provider; without it the key is absent.
func TestHookSettingsCarryNativeMemoryOff(t *testing.T) {
	rendered, err := hookSettingsJSON(Config{NativeMemoryOff: true})
	if err != nil {
		t.Fatalf("hookSettingsJSON: %v", err)
	}
	if !strings.Contains(rendered, `"autoMemoryEnabled":false`) || !strings.Contains(rendered, `"env":{"`+claude.AutoMemoryOffEnv+`":"1"}`) || !strings.Contains(rendered, `"hooks"`) {
		t.Fatalf("settings block: %s", rendered)
	}
	rendered, err = hookSettingsJSON(Config{})
	if err != nil {
		t.Fatalf("hookSettingsJSON: %v", err)
	}
	if strings.Contains(rendered, "autoMemoryEnabled") || strings.Contains(rendered, claude.AutoMemoryOffEnv) {
		t.Fatalf("sent the memory switch without NativeMemoryOff: %s", rendered)
	}
}

// AccessDirs reach the TUI the way they reach the headless provider: in the
// settings block's permissions, never as `--add-dir`.
func TestHookSettingsCarryTheAccessDirs(t *testing.T) {
	rendered, err := hookSettingsJSON(Config{AccessDirs: []string{"/memory/user"}})
	if err != nil {
		t.Fatalf("hookSettingsJSON: %v", err)
	}
	if !strings.Contains(rendered, `"permissions":{"additionalDirectories":["/memory/user"]}`) {
		t.Fatalf("settings block: %s", rendered)
	}
	if rendered, _ := hookSettingsJSON(Config{}); strings.Contains(rendered, "permissions") {
		t.Fatalf("permissions sent without AccessDirs: %s", rendered)
	}
}
