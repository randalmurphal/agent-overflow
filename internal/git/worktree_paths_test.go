package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeWorktreePathSegment(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"simple branch", "main", "main"},
		{"slashes become dashes", "feature/login", "feature-login"},
		{"backslashes become dashes", "feature\\login", "feature-login"},
		{"spaces become dashes", "feature login", "feature-login"},
		{"tabs become dashes", "feature\tlogin", "feature-login"},
		{"leading/trailing dots stripped", "...feature...", "feature"},
		{"leading/trailing dashes stripped", "---feature---", "feature"},
		{"only meta -> worktree", "//\\\\", "worktree"},
		{"empty -> worktree", "", "worktree"},
		{"whitespace-only -> worktree", "   ", "worktree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeWorktreePathSegment(tc.in); got != tc.want {
				t.Errorf("SanitizeWorktreePathSegment(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDefaultWorktreesBaseDir(t *testing.T) {
	got := DefaultWorktreesBaseDir("/home/u/repos/agent-overflow")
	want := filepath.Join("/home/u/repos", "agent-overflow-worktrees")
	if got != want {
		t.Errorf("DefaultWorktreesBaseDir = %q, want %q", got, want)
	}
}

func TestUniqueWorktreePathReturnsInputWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "feature-login")
	got, err := UniqueWorktreePath(target)
	if err != nil {
		t.Fatalf("UniqueWorktreePath: %v", err)
	}
	if got != target {
		t.Errorf("UniqueWorktreePath(%q) = %q, want %q", target, got, target)
	}
}

func TestUniqueWorktreePathSuffixesWhenPresent(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "feature")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	got, err := UniqueWorktreePath(base)
	if err != nil {
		t.Fatalf("UniqueWorktreePath: %v", err)
	}
	if !strings.HasPrefix(got, base+"-") {
		t.Errorf("UniqueWorktreePath returned %q, want suffix of %q", got, base)
	}
	if got == base {
		t.Errorf("UniqueWorktreePath did not bump suffix when %q existed", base)
	}
}
