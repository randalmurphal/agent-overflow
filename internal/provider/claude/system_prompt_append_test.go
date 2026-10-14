package claude

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

// The snapshot opt-out is gated on the binary's version: a build below
// 2.1.267 answers the flag with `error: unknown option` and exits, so an
// unknown or older version must omit it rather than fail the spawn.
func TestSystemPromptSnapshotArgsAreVersionGated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		want    bool
	}{
		{"", false},
		{"garbage", false},
		{"2.1.266", false},
		{"2.1.266 (Claude Code)", false},
		{"2.1.267", true},
		{"2.1.267 (Claude Code)", true},
		{"2.1.284", true},
		{"3.0.0", true},
	}
	for _, tc := range tests {
		args := SystemPromptSnapshotArgs(tc.version)
		got := slices.Equal(args, []string{"--system-prompt-snapshot", "off"})
		if got != tc.want {
			t.Errorf("SystemPromptSnapshotArgs(%q) = %v, want flag=%v", tc.version, args, tc.want)
		}
		if !got && args != nil {
			t.Errorf("SystemPromptSnapshotArgs(%q) = %v, want nil", tc.version, args)
		}
	}
}

// The append prompt travels by file like the replacement does, and the
// snapshot flag rides the same argv whenever the version admits it,
// replacement or not.
func TestBuildArgsPassesTheAppendSystemPromptFileAndSnapshotFlag(t *testing.T) {
	t.Parallel()
	appendPath, err := WriteSystemPromptFile(t.TempDir(), "APPENDED GUIDE")
	if err != nil {
		t.Fatalf("WriteSystemPromptFile: %v", err)
	}
	t.Cleanup(func() { RemoveSystemPromptFile(appendPath) })

	args := buildArgs(Config{AppendSystemPrompt: "APPENDED GUIDE", InstalledCLIVersion: "2.1.284 (Claude Code)"}, "", appendPath)
	if !hasArgPair(args, "--append-system-prompt-file", appendPath) {
		t.Errorf("argv lacks --append-system-prompt-file %s: %v", appendPath, args)
	}
	if slices.Contains(args, "APPENDED GUIDE") || slices.Contains(args, "--append-system-prompt") {
		t.Errorf("the append text reached argv: %v", args)
	}
	if slices.Contains(args, "--system-prompt-file") {
		t.Errorf("an append-only session carries --system-prompt-file: %v", args)
	}
	if !hasArgPair(args, "--system-prompt-snapshot", "off") {
		t.Errorf("argv lacks --system-prompt-snapshot off on 2.1.284: %v", args)
	}

	args = buildArgs(Config{InstalledCLIVersion: "2.1.266"}, "", "")
	if slices.Contains(args, "--append-system-prompt-file") || slices.Contains(args, "--system-prompt-snapshot") {
		t.Errorf("argv carries append or snapshot flags without a guide on an old build: %v", args)
	}
	args = buildArgs(Config{InstalledCLIVersion: "2.1.284"}, "", "")
	if !hasArgPair(args, "--system-prompt-snapshot", "off") {
		t.Errorf("snapshot opt-out must not depend on the append: %v", args)
	}
}

func hasArgPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// writeArgvRecordingClaude is a mock CLI that records its argv, one line per
// value, then behaves like a quiet stream-json process.
func writeArgvRecordingClaude(t *testing.T, argvPath string) string {
	t.Helper()
	script := "#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\" >> '" + argvPath + "'; done\ncat >/dev/null\n"
	return mockexec.Write(t, t.TempDir()+"/claude-argv", script)
}

// Both prompt files are written before the spawn, reach argv by path, and
// are removed by Close. A session with a replacement AND an append carries
// both files: the CLI composes them as replacement, blank line, append.
func TestNewSessionWritesAndRemovesBothPromptFiles(t *testing.T) {
	t.Parallel()
	argvPath := t.TempDir() + "/argv.txt"
	binary := writeArgvRecordingClaude(t, argvPath)

	dir := t.TempDir()
	s, err := NewSession(context.Background(), testThread, Config{
		Binary:             binary,
		SystemPrompt:       "REPLACEMENT",
		AppendSystemPrompt: "APPENDED GUIDE",
		SystemPromptDir:    dir,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if s.systemPromptPath == "" || s.appendSystemPromptPath == "" {
		t.Fatalf("prompt files = (%q, %q), want both written", s.systemPromptPath, s.appendSystemPromptPath)
	}
	if left := promptFilesIn(t, dir); len(left) != 2 {
		t.Fatalf("prompt files in SystemPromptDir = %v, want both", left)
	}
	for path, want := range map[string]string{s.systemPromptPath: "REPLACEMENT", s.appendSystemPromptPath: "APPENDED GUIDE"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(data) != want {
			t.Errorf("%s = %q, want %q", path, data, want)
		}
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, path := range []string{s.systemPromptPath, s.appendSystemPromptPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("os.Stat(%s) = %v after Close, want removed", path, err)
		}
	}
	raw, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	argv := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if !hasArgPair(argv, "--system-prompt-file", s.systemPromptPath) || !hasArgPair(argv, "--append-system-prompt-file", s.appendSystemPromptPath) {
		t.Errorf("spawned argv lacks the two prompt file flags: %v", argv)
	}
}

// A spawn that fails must not leave either prompt file behind.
func TestNewSessionRemovesBothPromptFilesWhenTheSpawnFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := NewSession(context.Background(), testThread, Config{
		Binary:             t.TempDir() + "/does-not-exist",
		SystemPrompt:       "REPLACEMENT",
		AppendSystemPrompt: "APPENDED GUIDE",
		SystemPromptDir:    dir,
	}, func(provider.ProviderEvent) {})
	if err == nil {
		t.Fatal("NewSession succeeded with a missing binary")
	}
	if left := promptFilesIn(t, dir); len(left) > 0 {
		t.Fatalf("failed spawn left prompt files behind: %v", left)
	}
}

// A process that dies without Close cannot remove its prompt files. They
// must then be in SystemPromptDir, which the app empties at boot, and never
// in the shared temp directory, where nothing would remove them.
func TestKilledSessionLeavesItsPromptFilesOnlyInTheSystemPromptDir(t *testing.T) {
	sharedTemp := t.TempDir()
	t.Setenv("TMPDIR", sharedTemp)
	dir := t.TempDir()
	binary := writeArgvRecordingClaude(t, t.TempDir()+"/argv.txt")
	s, err := NewSession(context.Background(), testThread, Config{
		Binary:             binary,
		SystemPrompt:       "REPLACEMENT",
		AppendSystemPrompt: "APPENDED GUIDE",
		SystemPromptDir:    dir,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	proc, err := os.FindProcess(s.PID())
	if err != nil {
		t.Fatalf("find session process: %v", err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill session process: %v", err)
	}
	select {
	case <-s.readDone:
	case <-time.After(10 * time.Second):
		t.Fatal("read loop did not end after the process was killed")
	}

	if left := promptFilesIn(t, sharedTemp); len(left) > 0 {
		t.Fatalf("prompt files in the shared temp directory: %v", left)
	}
	if left := promptFilesIn(t, dir); len(left) != 2 {
		t.Fatalf("prompt files in SystemPromptDir after the kill = %v, want both", left)
	}
}

func promptFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "ao-claude-system-prompt-*.txt"))
	if err != nil {
		t.Fatalf("glob prompt files: %v", err)
	}
	return matches
}

// The spawned process carries the snapshot opt-out exactly when the
// installed version is known to accept it. Without it a --resume reuses the
// transcript's recorded prompt, and a restart after a prompt file edit would
// not pick the edit up.
func TestNewSessionPassesTheSnapshotOptOutToTheProcess(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		version string
		want    bool
	}{{"", false}, {"2.1.266", false}, {"2.1.284 (Claude Code)", true}} {
		argvPath := t.TempDir() + "/argv.txt"
		s, err := NewSession(context.Background(), testThread, Config{
			Binary:              writeArgvRecordingClaude(t, argvPath),
			InstalledCLIVersion: tc.version,
		}, func(provider.ProviderEvent) {})
		if err != nil {
			t.Fatalf("NewSession(%q): %v", tc.version, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		raw, err := os.ReadFile(argvPath)
		if err != nil {
			t.Fatalf("read argv: %v", err)
		}
		argv := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		if got := hasArgPair(argv, "--system-prompt-snapshot", "off"); got != tc.want {
			t.Errorf("InstalledCLIVersion %q: argv has --system-prompt-snapshot off = %v, want %v: %v", tc.version, got, tc.want, argv)
		}
	}
}
