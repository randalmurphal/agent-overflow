package aocli

import (
	"strings"
	"testing"
)

// runCLI executes one command and returns its exit code plus both streams.
func runCLI(args []string, lookupEnv func(string) (string, bool)) (int, string, string) {
	var stdout, stderr strings.Builder
	code := RunWithEnv(args, lookupEnv, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func noEnv(string) (string, bool) { return "", false }

func TestSessionFromEnvRejectsAHalfInjectedEnvironment(t *testing.T) {
	only := func(name, value string) func(string) (string, bool) {
		return func(lookup string) (string, bool) {
			if lookup == name {
				return value, true
			}
			return "", false
		}
	}
	if _, err := SessionFromEnv(only(EnvEndpoint, "http://127.0.0.1:1")); err == nil ||
		!strings.Contains(err.Error(), EnvToken) {
		t.Fatalf("endpoint without token: %v", err)
	}
	if _, err := SessionFromEnv(only(EnvToken, "tok")); err == nil ||
		!strings.Contains(err.Error(), EnvEndpoint) {
		t.Fatalf("token without endpoint: %v", err)
	}
	session, err := SessionFromEnv(func(name string) (string, bool) {
		return map[string]string{EnvEndpoint: " http://127.0.0.1:1 ", EnvToken: "tok", EnvThreadID: "t-1"}[name], true
	})
	if err != nil {
		t.Fatalf("SessionFromEnv: %v", err)
	}
	if session.Endpoint != "http://127.0.0.1:1" || session.ThreadID != "t-1" {
		t.Fatalf("session = %#v", session)
	}
}

// Inside a session too, a verb this build does not have is unknown rather
// than reaching a backend. The endpoint is never dialed.
func TestUnknownCommandsExitTwo(t *testing.T) {
	session := func(name string) (string, bool) {
		value, ok := map[string]string{EnvEndpoint: "http://127.0.0.1:1", EnvToken: "test-token", EnvThreadID: "thread-1"}[name]
		return value, ok
	}
	for _, args := range [][]string{
		{"nonsense"},
		{"run", "start", "flow"},
		{"workflow", "list"},
		{"memory", "list"},
	} {
		code, _, stderr := runCLI(args, session)
		if code != exitError {
			t.Fatalf("%v exit = %d, want %d", args, code, exitError)
		}
		if !strings.Contains(stderr, "unknown") {
			t.Fatalf("%v stderr = %q, want an unknown-command refusal", args, stderr)
		}
	}
}

func TestHelpExitsZeroAndPrintsUsage(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"thread", "--help"}} {
		code, stdout, stderr := runCLI(args, noEnv)
		if code != exitOK {
			t.Fatalf("%v exit = %d (%s)", args, code, stderr)
		}
		if !strings.Contains(stdout, "Usage:") {
			t.Fatalf("%v printed no usage: %q", args, stdout)
		}
	}
}
