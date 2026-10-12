package claudetui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// The TUI launch carries the append file and the version-gated snapshot
// opt-out exactly as the headless argv does (2.1.284 PTY + wire capture).
func TestBuildLaunchOptionsPassesTheAppendSystemPromptFileAndSnapshotFlag(t *testing.T) {
	const guide = "APPENDED GUIDE"
	path, err := claude.WriteSystemPromptFile(guide)
	if err != nil {
		t.Fatalf("WriteSystemPromptFile: %v", err)
	}
	t.Cleanup(func() { claude.RemoveSystemPromptFile(path) })

	cfg := Config{Binary: "claude", WorkDir: t.TempDir(), HookCmd: "/tmp/ao-exe", AppendSystemPrompt: guide, InstalledCLIVersion: "2.1.284 (Claude Code)"}
	opts, err := buildLaunchOptions(cfg, "", path, "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
	if err != nil {
		t.Fatalf("buildLaunchOptions: %v", err)
	}
	if !hasArgPair(opts.Args, "--append-system-prompt-file", path) {
		t.Errorf("launch args lack --append-system-prompt-file %s: %v", path, opts.Args)
	}
	if slices.Contains(opts.Args, guide) {
		t.Errorf("the append text reached argv: %v", opts.Args)
	}
	if slices.Contains(opts.Args, "--system-prompt-file") {
		t.Errorf("an append-only launch carries --system-prompt-file: %v", opts.Args)
	}
	if !hasArgPair(opts.Args, "--system-prompt-snapshot", "off") {
		t.Errorf("launch args lack --system-prompt-snapshot off on 2.1.284: %v", opts.Args)
	}

	cfg = Config{Binary: "claude", WorkDir: t.TempDir(), HookCmd: "/tmp/ao-exe", InstalledCLIVersion: "2.1.266"}
	opts, err = buildLaunchOptions(cfg, "", "", "http://127.0.0.1:1", "http://127.0.0.1:2/hook", "tok")
	if err != nil {
		t.Fatalf("buildLaunchOptions: %v", err)
	}
	if slices.Contains(opts.Args, "--append-system-prompt-file") || slices.Contains(opts.Args, "--system-prompt-snapshot") {
		t.Errorf("launch args carry append or snapshot flags without a guide on an old build: %v", opts.Args)
	}
}

// Close removes the append file alongside the replacement, and a second
// Close stays a no-op.
func TestSessionCloseRemovesBothPromptFiles(t *testing.T) {
	replacement, err := claude.WriteSystemPromptFile("You are the agent.")
	if err != nil {
		t.Fatalf("WriteSystemPromptFile: %v", err)
	}
	t.Cleanup(func() { claude.RemoveSystemPromptFile(replacement) })
	appended, err := claude.WriteSystemPromptFile("APPENDED GUIDE")
	if err != nil {
		t.Fatalf("WriteSystemPromptFile: %v", err)
	}
	t.Cleanup(func() { claude.RemoveSystemPromptFile(appended) })

	s := &Session{
		threadID:               testThread,
		parser:                 claude.NewParser(),
		feed:                   make(chan json.RawMessage, 1),
		done:                   make(chan struct{}),
		logf:                   func(string, ...any) {},
		onEvent:                func(provider.ProviderEvent) {},
		systemPromptPath:       replacement,
		appendSystemPromptPath: appended,
	}
	s.rec = newReconstructor(s.feedEnvelope)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, path := range []string{replacement, appended} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("os.Stat(%s) = %v after Close, want removed", path, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close should be a no-op: %v", err)
	}
}

// A launch that fails after the prompt files are written must remove both:
// here the launch is refused for lacking a binary, which happens after both
// writes.
func TestNewSessionRemovesBothPromptFilesWhenTheLaunchFails(t *testing.T) {
	dir := privatePromptDir(t)
	_, err := NewSession(context.Background(), testThread, Config{
		WorkDir:            t.TempDir(),
		SystemPrompt:       "REPLACEMENT",
		AppendSystemPrompt: "APPENDED GUIDE",
	}, func(provider.ProviderEvent) {})
	if err == nil {
		t.Fatal("NewSession succeeded without a binary")
	}
	if left := promptFilesIn(t, dir); len(left) != 0 {
		t.Fatalf("failed launch left prompt files behind: %v", left)
	}
}

// privatePromptDir points TMPDIR, where the prompt files are written, at a
// directory only the calling test writes, so prompt files from other tests
// and other test processes never count against it. t.Setenv keeps the test
// out of the parallel phase while TMPDIR is changed.
func privatePromptDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

func promptFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "ao-claude-system-prompt-*.txt"))
	if err != nil {
		t.Fatalf("glob prompt files: %v", err)
	}
	return matches
}
