package acmecert

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The hook's whole contract is its argv, so it is asserted against a real
// process rather than a stub: the four appended arguments, in order,
// after whatever the user stored.
//
// /bin/sh, like internal/worktreesetup's run tests, because that is where
// the backend actually executes — native macOS or Linux, or the Linux
// backend under WSL.
func TestTheHookIsInvokedWithTheDocumentedArguments(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	argv := []string{"/bin/sh", "-c", `printf '%s\n' "$@" >> "` + log + `"`, "dns-hook"}

	if err := runHook(context.Background(), argv, time.Minute, hookSet, "_acme-challenge.backend.example", "the-value"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := runHook(context.Background(), argv, time.Minute, hookClear, "_acme-challenge.backend.example", "the-value"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	recorded, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read what the hook recorded: %v", err)
	}
	want := strings.Join([]string{
		"set", "_acme-challenge.backend.example", "the-value",
		"clear", "_acme-challenge.backend.example", "the-value",
		"",
	}, "\n")
	if string(recorded) != want {
		t.Fatalf("the hook received:\n%s\nwant:\n%s", recorded, want)
	}
}

// A hook that failed silently would look like the certificate authority
// refusing to validate, so the failure carries both what the process did
// and what it said.
func TestAFailingHookReportsItsOutput(t *testing.T) {
	err := runHook(context.Background(),
		[]string{"/bin/sh", "-c", "echo 'zone example is read-only' >&2; exit 3", "dns-hook"},
		time.Minute, hookSet, "_acme-challenge.backend.example", "the-value")
	requireNames(t, err, "zone example is read-only")
	requireNames(t, err, "exit status 3")
}

// The bound is real: a hook that never returns must not hold the renewal
// loop, and the timeout kills its whole process group.
func TestAHookThatNeverReturnsIsBounded(t *testing.T) {
	started := time.Now()
	err := runHook(context.Background(),
		[]string{"/bin/sh", "-c", "sleep 30", "dns-hook"},
		150*time.Millisecond, hookSet, "_acme-challenge.backend.example", "the-value")
	requireNames(t, err, "timed out")
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the hook was not bounded: it ran for %s", elapsed)
	}
}

// An empty argv is a configuration problem, and it is reported as one
// rather than as an exec failure nobody can act on.
func TestAnEmptyHookIsRefused(t *testing.T) {
	requireNames(t,
		runHook(context.Background(), nil, time.Minute, hookSet, "name", "value"),
		"no command to run")
}
