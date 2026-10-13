package aocli

import (
	"bytes"
	"strings"
	"testing"
)

// The app binary decides "CLI invocation or boot" from Commands(), so a verb
// Run dispatches but Commands omits would be unreachable as `agent-overflow
// <verb>`, and a verb Commands reports but Run rejects would swallow a boot.
// Both directions are pinned here because only the table can satisfy both.
func TestCommandsMatchesWhatRunDispatches(t *testing.T) {
	names := Commands()
	if len(names) == 0 {
		t.Fatal("Commands() is empty")
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			t.Fatalf("Commands() repeats %q", name)
		}
		seen[name] = true
		if !IsCommand(name) {
			t.Fatalf("IsCommand(%q) = false for a name Commands() reports", name)
		}
		// A dispatched command reaches its own handler: it may still refuse the
		// empty argument list, but never as "unknown command", which is the one
		// answer that would mean Run does not know the name at all.
		var stdout, stderr bytes.Buffer
		RunWithEnv([]string{name}, func(string) (string, bool) { return "", false }, &stdout, &stderr)
		if strings.Contains(stderr.String(), "unknown command") {
			t.Fatalf("Run rejected %q as unknown: %q", name, stderr.String())
		}
	}
	if IsCommand("frobnicate") {
		t.Fatal("IsCommand accepted a name no table row declares")
	}
	var stdout, stderr bytes.Buffer
	if code := RunWithEnv([]string{"frobnicate"}, func(string) (string, bool) { return "", false }, &stdout, &stderr); code != exitError {
		t.Fatalf("an unknown command exited %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("an unknown command did not say so: %q", stderr.String())
	}
}
