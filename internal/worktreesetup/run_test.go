package worktreesetup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunInjectsCheckoutPaths pins the env contract setup recipes are written
// against: both checkouts named, absolute, distinct, with the inherited
// environment intact underneath — and an inherited value of either name losing
// to the injected one, since the app can itself be launched from inside an
// agent session that exports AO_* names.
func TestRunInjectsCheckoutPaths(t *testing.T) {
	project := t.TempDir()
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, ".env"), []byte("TOKEN=main-checkout\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ProjectRootEnv, filepath.Join(t.TempDir(), "stale-project"))
	t.Setenv(WorktreePathEnv, filepath.Join(t.TempDir(), "stale-worktree"))
	if err := Run(context.Background(), project, worktree, Config{
		Run: [][]string{
			// Relative redirect: also proves the variables agree with the cwd.
			{"/bin/sh", "-c", `printf '%s\n%s\n%s\n' "$AO_PROJECT_ROOT" "$AO_WORKTREE_PATH" "$PATH" > reported.txt`},
			// The recipe the contract exists for.
			{"/bin/sh", "-c", `ln -s "$AO_PROJECT_ROOT/.env" "$AO_WORKTREE_PATH/.env"`},
		},
		Timeout: "30s",
	}); err != nil {
		t.Fatal(err)
	}
	reported, err := os.ReadFile(filepath.Join(worktree, "reported.txt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(reported), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("setup command reported %d lines: %q", len(lines), reported)
	}
	if !filepath.IsAbs(lines[0]) || lines[0] != project {
		t.Fatalf("%s = %q, want absolute %q", ProjectRootEnv, lines[0], project)
	}
	if !filepath.IsAbs(lines[1]) || lines[1] != worktree {
		t.Fatalf("%s = %q, want absolute %q", WorktreePathEnv, lines[1], worktree)
	}
	if lines[2] != os.Getenv("PATH") {
		t.Fatalf("inherited PATH = %q, want %q", lines[2], os.Getenv("PATH"))
	}
	target, err := os.Readlink(filepath.Join(worktree, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(project, ".env") {
		t.Fatalf("symlink target = %q, want %q", target, filepath.Join(project, ".env"))
	}
	linked, err := os.ReadFile(filepath.Join(worktree, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(linked) != "TOKEN=main-checkout\n" {
		t.Fatalf("linked .env = %q", linked)
	}
}

// TestEnvRelativeRootsResolveToTheCommandsTree covers the one input shape the
// absolutising exists for: a relative root has to land on the same tree exec
// resolves a relative command.Dir against.
func TestEnvRelativeRootsResolveToTheCommandsTree(t *testing.T) {
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	env, err := Env("some/project", "some/worktree")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		ProjectRootEnv + "=" + filepath.Join(workingDir, "some/project"),
		WorktreePathEnv + "=" + filepath.Join(workingDir, "some/worktree"),
	}
	if got := env[len(env)-2:]; got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("trailing setup env = %q, want %q", got, want)
	}
}

// The timeout kills the whole process group, so a command that backgrounded its
// real work cannot outlive it.
func TestRunTimeoutKillsTheProcessTree(t *testing.T) {
	project := t.TempDir()
	worktree := t.TempDir()
	started := time.Now()
	err := Run(context.Background(), project, worktree, Config{
		Run: [][]string{{"/bin/sh", "-c", "sleep 10 & wait"}}, Timeout: "20ms",
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("process-tree timeout took %s", elapsed)
	}
}

func TestRunReportsTheFailingCommandsOutputTail(t *testing.T) {
	project := t.TempDir()
	worktree := t.TempDir()
	err := Run(context.Background(), project, worktree, Config{
		Run: [][]string{{"/bin/sh", "-c", "echo diagnosis >&2; exit 3"}},
	})
	if err == nil || !strings.Contains(err.Error(), "diagnosis") || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("failure error = %v", err)
	}
}

// A later command never runs after an earlier one failed: the tree is already
// broken, and a recipe's steps are ordered because they depend on each other.
func TestRunStopsAtTheFirstFailure(t *testing.T) {
	project := t.TempDir()
	worktree := t.TempDir()
	err := Run(context.Background(), project, worktree, Config{
		Run: [][]string{
			{"/bin/sh", "-c", "exit 1"},
			{"/bin/sh", "-c", "printf ran > second.txt"},
		},
	})
	if err == nil {
		t.Fatal("failing recipe reported success")
	}
	if _, statErr := os.Stat(filepath.Join(worktree, "second.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("command after a failure ran: %v", statErr)
	}
}

func TestRunZeroConfigDoesNothing(t *testing.T) {
	if err := Run(context.Background(), t.TempDir(), t.TempDir(), Config{}); err != nil {
		t.Fatalf("empty setup = %v", err)
	}
}

func TestRunRefusesAnUnparseableTimeout(t *testing.T) {
	err := Run(context.Background(), t.TempDir(), t.TempDir(), Config{Timeout: "later"})
	if err == nil || !strings.Contains(err.Error(), "invalid worktree setup timeout") {
		t.Fatalf("bad timeout error = %v", err)
	}
}
