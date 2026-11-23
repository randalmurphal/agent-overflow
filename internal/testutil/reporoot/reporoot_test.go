package reporoot

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestDirIsTheCheckoutEvenAfterTheTestChangesDirectory(t *testing.T) {
	want, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

	if got := Dir(t); got != want {
		t.Fatalf("Dir = %s, want %s", got, want)
	}
	self := Path(t, "internal/testutil/reporoot/reporoot.go")
	if _, err := os.Stat(self); err != nil {
		t.Fatalf("Path does not reach this package's source: %v", err)
	}
}

func TestFindStopsAtTheNearestGoMod(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "module")
	start := filepath.Join(inner, "internal", "pkg")
	if err := os.MkdirAll(start, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{outer, inner} {
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if got, err := find(start); err != nil || got != inner {
		t.Fatalf("find(%s) = %s, %v; want %s", start, got, err, inner)
	}
}

func TestFindFailsWithNoGoModAbove(t *testing.T) {
	volumeRoot := filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
	if dir, err := find(volumeRoot); err == nil {
		t.Fatalf("find(%s) = %s, want an error naming the missing go.mod", volumeRoot, dir)
	}
}

func TestWalkLeavesOutWhatGitignoreIgnores(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		".gitignore":                    "/bin/\nnode_modules/\n*.log\n",
		"go.mod":                        "module fixture\n",
		"main.go":                       "package main\n",
		"bin/tool":                      "built",
		"web/node_modules/dep/index.js": "dep",
		"web/app.ts":                    "app",
		"internal/pkg/new.go":           "package pkg\n",
		"internal/pkg/run.log":          "log",
	}
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitInit := exec.Command("git", "init", "-q", root)
	gitInit.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	ignored, err := listIgnored(root)
	if err != nil {
		t.Fatal(err)
	}

	var walked []string
	err = walk(root, ignored, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		walked = append(walked, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Nothing is committed: untracked files that no .gitignore names are
	// part of the tree.
	want := []string{".", ".gitignore", "go.mod", "internal", "internal/pkg", "internal/pkg/new.go", "main.go", "web", "web/app.ts"}
	if !slices.Equal(walked, want) {
		t.Fatalf("walked %q, want %q", walked, want)
	}
}
