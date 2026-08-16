package claudetui

import (
	"strings"
	"testing"
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
	opts, err := buildLaunchOptions(cfg, "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
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
	opts, err := buildLaunchOptions(cfg, "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
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
	opts, err := buildLaunchOptions(cfg, "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
	if err != nil {
		t.Fatalf("buildLaunchOptions: %v", err)
	}
	for i, a := range opts.Args {
		if a == "--effort" {
			t.Errorf("launch args unexpectedly contain --effort (at index %d); got %v", i, opts.Args)
		}
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
		env := buildEnv([]string{"FOO=bar"}, "http://gw", "http://hook", "tok")
		if n, v := countEnv(env, todoToolsEnvVar); n != 1 || v != "true" {
			t.Fatalf("%s: got %d entries (last %q), want exactly one =true; env %v", todoToolsEnvVar, n, v, env)
		}
	})

	t.Run("user opt-out survives without a duplicate", func(t *testing.T) {
		env := buildEnv(
			[]string{"FOO=bar", todoToolsEnvVar + "=false"},
			"http://gw", "http://hook", "tok",
		)
		if n, v := countEnv(env, todoToolsEnvVar); n != 1 || v != "false" {
			t.Fatalf("%s: got %d entries (last %q), want exactly the caller's =false; env %v", todoToolsEnvVar, n, v, env)
		}
	})

	t.Run("owned gateway keys still replaced", func(t *testing.T) {
		env := buildEnv(
			[]string{BaseURLEnv + "=https://dirty.example"},
			"http://gw", "http://hook", "tok",
		)
		if n, v := countEnv(env, BaseURLEnv); n != 1 || v != "http://gw" {
			t.Fatalf("%s: got %d entries (last %q), want exactly the gateway URL; env %v", BaseURLEnv, n, v, env)
		}
	})
}
