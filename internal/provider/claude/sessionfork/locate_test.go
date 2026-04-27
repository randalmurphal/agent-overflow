package sessionfork

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectSlug(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/Users/randy/repos/agent-overflow", "-Users-randy-repos-agent-overflow"},
		{"/private/tmp/foo", "-private-tmp-foo"},
		{"/", "-"},
	}
	for _, c := range cases {
		if got := projectSlug(c.in); got != c.want {
			t.Errorf("projectSlug(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestLocateSessionFile_PrimaryHit(t *testing.T) {
	// Build a fake ~/.claude/projects layout under TempDir, then point
	// HOME at it so projectsDir() resolves correctly.
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	abs, _ := filepath.Abs(canonical)
	slug := projectSlug(abs)

	projectDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	want := filepath.Join(projectDir, "abc123.jsonl")
	if err := os.WriteFile(want, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}

	got, err := LocateSessionFile("abc123", workspace)
	if err != nil {
		t.Fatalf("LocateSessionFile: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLocateSessionFile_FallbackScan(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Create a JSONL under a project dir that does NOT correspond to
	// the workspace path we'll pass — exercises the fallback scan.
	otherDir := filepath.Join(home, ".claude", "projects", "-some-other-project")
	if err := os.MkdirAll(otherDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	want := filepath.Join(otherDir, "stray-uuid.jsonl")
	if err := os.WriteFile(want, []byte("hi\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Pass a workspace dir that doesn't have a matching project slug —
	// should fall back and find the stray file.
	workspace := filepath.Join(home, "elsewhere")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	got, err := LocateSessionFile("stray-uuid", workspace)
	if err != nil {
		t.Fatalf("LocateSessionFile: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLocateSessionFile_NotFound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Create the projects dir so ReadDir doesn't fail.
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, err := LocateSessionFile("does-not-exist", "/any/workspace")
	if !errors.Is(err, ErrSessionFileNotFound) {
		t.Fatalf("err=%v, want ErrSessionFileNotFound", err)
	}
}

func TestLocateSessionFile_EmptySessionID(t *testing.T) {
	_, err := LocateSessionFile("", "/some/path")
	if err == nil {
		t.Fatal("expected error for empty sessionID")
	}
	if !strings.Contains(err.Error(), "sessionID") {
		t.Errorf("err=%v, want message about sessionID", err)
	}
}
