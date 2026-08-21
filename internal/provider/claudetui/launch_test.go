package claudetui

import (
	"os"
	"slices"
	"strings"
	"testing"

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
// is LIVE-confirmed against 2.1.170 in spike/claude-mitm/probe_thinking_title.py.
func TestBuildLaunchOptionsEnablesThinkingDisplay(t *testing.T) {
	cfg := Config{Binary: "claude", WorkDir: t.TempDir(), HookCmd: "/tmp/ao-exe"}
	opts, err := buildLaunchOptions(cfg, "", "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
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
	cfg := Config{Binary: "claude", WorkDir: t.TempDir(), HookCmd: "/tmp/ao-exe", ReasoningEffort: "high"}
	opts, err := buildLaunchOptions(cfg, "", "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
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
	cfg := Config{Binary: "claude", WorkDir: t.TempDir(), HookCmd: "/tmp/ao-exe"}
	opts, err := buildLaunchOptions(cfg, "", "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
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
	path, err := claude.WriteSystemPromptFile(prompt)
	if err != nil {
		t.Fatalf("WriteSystemPromptFile() error = %v", err)
	}
	t.Cleanup(func() { claude.RemoveSystemPromptFile(path) })

	cfg := Config{Binary: "claude", WorkDir: t.TempDir(), HookCmd: "/tmp/ao-exe", SystemPrompt: prompt}
	opts, err := buildLaunchOptions(cfg, path, "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
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
	cfg := Config{Binary: "claude", WorkDir: t.TempDir(), HookCmd: "/tmp/ao-exe"}
	opts, err := buildLaunchOptions(cfg, "", "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
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
		Binary:          "claude",
		WorkDir:         t.TempDir(),
		HookCmd:         "/tmp/ao-exe",
		DisallowedTools: []string{"Workflow", "WebSearch"},
	}
	opts, err := buildLaunchOptions(cfg, "", "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
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

// TestBuildEnvDefaultsTodoToolsOptIn pins the claude ≥2.1.233 adaptation on
// the TUI path: the session defaults into the todo tool surface, but a
// caller-provided value — the user's custom provider environment — wins.
func TestBuildEnvDefaultsTodoToolsOptIn(t *testing.T) {
	t.Run("default applied when absent", func(t *testing.T) {
		env := buildEnv([]string{"FOO=bar"}, "http://gw", "http://hook", "tok", false, false)
		if n, v := countEnv(env, todoToolsEnvVar); n != 1 || v != "true" {
			t.Fatalf("%s: got %d entries (last %q), want exactly one =true; env %v", todoToolsEnvVar, n, v, env)
		}
	})

	t.Run("user opt-out survives without a duplicate", func(t *testing.T) {
		env := buildEnv(
			[]string{"FOO=bar", todoToolsEnvVar + "=false"},
			"http://gw", "http://hook", "tok", false, false,
		)
		if n, v := countEnv(env, todoToolsEnvVar); n != 1 || v != "false" {
			t.Fatalf("%s: got %d entries (last %q), want exactly the caller's =false; env %v", todoToolsEnvVar, n, v, env)
		}
	})

	t.Run("owned gateway keys still replaced", func(t *testing.T) {
		env := buildEnv(
			[]string{BaseURLEnv + "=https://dirty.example"},
			"http://gw", "http://hook", "tok", false, false,
		)
		if n, v := countEnv(env, BaseURLEnv); n != 1 || v != "http://gw" {
			t.Fatalf("%s: got %d entries (last %q), want exactly the gateway URL; env %v", BaseURLEnv, n, v, env)
		}
	})
}

// TestBuildEnvTodoReminderMode pins the Settings nudge toggle's TUI leg:
// disabling reminders exports CLAUDE_CODE_TODO_REMINDER_MODE=off, leaving
// it enabled exports nothing (the CLI owns its default), and a base-env
// value outranks the setting.
func TestBuildEnvTodoReminderMode(t *testing.T) {
	t.Run("disabled exports off", func(t *testing.T) {
		env := buildEnv([]string{"FOO=bar"}, "http://gw", "http://hook", "tok", true, false)
		if n, v := countEnv(env, todoReminderModeEnvVar); n != 1 || v != "off" {
			t.Fatalf("%s: got %d entries (last %q), want exactly one =off; env %v", todoReminderModeEnvVar, n, v, env)
		}
	})

	t.Run("enabled exports nothing", func(t *testing.T) {
		env := buildEnv([]string{"FOO=bar"}, "http://gw", "http://hook", "tok", false, false)
		if n, v := countEnv(env, todoReminderModeEnvVar); n != 0 {
			t.Fatalf("%s: got %d entries (last %q), want none; env %v", todoReminderModeEnvVar, n, v, env)
		}
	})

	t.Run("user value survives without a duplicate", func(t *testing.T) {
		env := buildEnv(
			[]string{todoReminderModeEnvVar + "=baseline"},
			"http://gw", "http://hook", "tok", true, false,
		)
		if n, v := countEnv(env, todoReminderModeEnvVar); n != 1 || v != "baseline" {
			t.Fatalf("%s: got %d entries (last %q), want exactly the caller's =baseline; env %v", todoReminderModeEnvVar, n, v, env)
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
		cfg := Config{CrossSessionInbound: policy}
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
	rendered, err := hookSettingsJSON(Config{CrossSessionInbound: "   "})
	if err != nil {
		t.Fatalf("hookSettingsJSON: %v", err)
	}
	if strings.Contains(rendered, "crossSessionInbound") {
		t.Fatalf("an empty policy rendered a key: %s", rendered)
	}
}

// B18, TUI half. The PTY launch assembles a full []string environment
// instead of the override map Spawn takes, so it has to remove the inherited
// gate itself. A host that exported CLAUDE_CODE_HARBOR_KITE must not bind the
// peer inbox for a session whose setting says off — refusing inbound messages
// does not undo being discoverable in every peer's ListAgents.
func TestBuildEnvDropsAnInheritedCrossSessionGateWhenDisabled(t *testing.T) {
	base := []string{
		claude.CrossSessionGateEnv + "=1",
		"CLAUDE_CODE_SESSION_NAME=someone-elses-name",
		"HOME=/home/test",
	}
	env := buildEnv(base, "http://gw", "http://hook", "tok", false, false)

	if value, found := envValueIn(env, claude.CrossSessionGateEnv); found {
		t.Fatalf("%s = %q, want it absent — the setting says off", claude.CrossSessionGateEnv, value)
	}
	for _, key := range claude.CrossSessionUnsetEnv() {
		if value, found := envValueIn(env, key); found {
			t.Fatalf("%s = %q survived, want it owned and dropped", key, value)
		}
	}
	if value, _ := envValueIn(env, "HOME"); value != "/home/test" {
		t.Fatalf("HOME = %q, want the inherited value untouched", value)
	}
}

// Enabled states the gate explicitly, overriding whatever the host carried.
func TestBuildEnvStatesTheCrossSessionGateWhenEnabled(t *testing.T) {
	base := []string{claude.CrossSessionGateEnv + "=totally-bogus", "HOME=/home/test"}
	env := buildEnv(base, "http://gw", "http://hook", "tok", false, true)

	if value, found := envValueIn(env, claude.CrossSessionGateEnv); !found || value != "1" {
		t.Fatalf("%s = %q (present=%v), want \"1\"", claude.CrossSessionGateEnv, value, found)
	}
	if count := slices.IndexFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, claude.CrossSessionGateEnv+"=totally-bogus")
	}); count >= 0 {
		t.Fatalf("the inherited gate value survived beside the stated one: %v", env)
	}
}
