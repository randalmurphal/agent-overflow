package claudetui

import "testing"

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
