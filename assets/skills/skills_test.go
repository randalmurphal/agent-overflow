package skills

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"agent-overflow/internal/skillsinstall"
)

// What ships is exactly what the repository tracks: nothing a script's
// tooling left in the checkout, and nothing tracked left out.
func TestShippedSkillsAreTheTrackedTree(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z", "--", "pstack/skills").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var tracked []string
	for _, name := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		tracked = append(tracked, strings.TrimPrefix(name, "pstack/skills/"))
	}
	sort.Strings(tracked)
	tree, err := skillsinstall.LoadTree(PStackSkills(), PStackUpstream())
	if err != nil {
		t.Fatalf("LoadTree: %v", err)
	}
	if got, want := strings.Join(tree.Files(), "\n"), strings.Join(tracked, "\n"); got != want {
		t.Fatalf("shipped files differ from the tracked tree.\nshipped:\n%s\ntracked:\n%s", got, want)
	}
	if tree.Version == "" || len(tree.Commit) != 40 {
		t.Fatalf("pin = %q %q", tree.Version, tree.Commit)
	}
}

// An embedded file has no mode, so an install marks a file executable when
// it starts with `#!`. Every tracked executable must, or it would install
// without its bit.
func TestEveryTrackedExecutableStartsWithAShebang(t *testing.T) {
	err := filepath.WalkDir("pstack/skills", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Mode().Perm()&0o111 == 0 {
			return err
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(string(raw), "#!") {
			t.Errorf("%s is executable but has no #! line, so it would install without its mode", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
