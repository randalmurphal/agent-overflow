//go:build noremote

package aocli

import (
	"bytes"
	"strings"
	"testing"

	"agent-overflow/internal/buildvariant"
)

// A build without remote access refuses the commands that exist only for
// it, before any of them reads the environment or reaches a backend, and
// its help does not offer them.
func TestNoremoteRefusesRemoteCommands(t *testing.T) {
	noEnv := func(string) (string, bool) { return "", false }
	for _, args := range [][]string{
		{"pair", "--lan"},
		{"remote", "list"},
		{"service", "install"},
	} {
		var stdout, stderr bytes.Buffer
		code := RunWithEnv(args, noEnv, &stdout, &stderr)
		if code != exitError {
			t.Errorf("%v exited %d, want %d", args, code, exitError)
		}
		if !strings.Contains(stderr.String(), buildvariant.ErrRemoteAccessUnavailable.Error()) {
			t.Errorf("%v stderr = %q, want the remote-access refusal", args, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("%v wrote to stdout: %q", args, stdout.String())
		}
	}
	for _, offered := range []string{"  serve ", "service install", "pair --lan", "  remote "} {
		if strings.Contains(rootUsage, offered) {
			t.Errorf("help offers %q in a build without remote access:\n%s", offered, rootUsage)
		}
	}
	if !strings.Contains(rootUsage, "workflow new") {
		t.Errorf("help lost the offline commands:\n%s", rootUsage)
	}
}
