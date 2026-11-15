package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Git reads the index's copy of an attribute file the working tree does
// not have, so that entry's state is part of the context.
func TestTrackedChurnKeysTheIndexCopyOfAnAttributeFile(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("a.txt", "one\ntwo\nthree\n")
	r.write(".gitattributes", "a.txt -diff\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()

	r.write("a.txt", "one\nTWO\nthree\n")
	if err := os.Remove(filepath.Join(r.dir, ".gitattributes")); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name string
		run  func()
		want trackedChurnTotals
	}{
		{"the working tree copy deleted", func() {}, trackedChurnTotals{deletions: 1, files: 2}},
		{"the index copy removed", func() { r.git("rm", "-q", "--cached", ".gitattributes") }, trackedChurnTotals{insertions: 1, deletions: 2, files: 2}},
	}
	for _, step := range steps {
		step.run()
		if whole := r.whole(r.dir); whole != step.want {
			t.Fatalf("whole-tree diff after %q = %+v, want %+v: the step must change the diff", step.name, whole, step.want)
		}
		if got := r.scan(core, r.dir); got != step.want {
			t.Fatalf("scan after %q = %+v, want %+v", step.name, got, step.want)
		}
	}
}

// What a diff reads besides the two contents is part of every key: when
// it changes, what was stored under the old one is not replayed.
func TestTrackedChurnForgetsItsMemoWhenTheDiffContextChanges(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("dir/a.txt", "one\ntwo\nthree\n")
	r.write(".gitignore", "dir/.gitattributes\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()
	outside := t.TempDir()
	writeFile := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r.write("dir/a.txt", "one\nTWO\nthree\n")
	text, binary := trackedChurnTotals{insertions: 1, deletions: 1, files: 1}, trackedChurnTotals{files: 1}
	if got := r.scan(core, r.dir); got != text {
		t.Fatalf("first scan = %+v, want %+v", got, text)
	}
	if memo := r.memo(core); len(memo.files) != 1 {
		t.Fatalf("memo holds %d files, want 1", len(memo.files))
	}

	// Each step changes how dir/a.txt diffs and nothing about the file.
	infoAttributes := filepath.Join(r.dir, ".git", "info", "attributes")
	globalAttributes := filepath.Join(outside, "global-attributes")
	included := filepath.Join(r.dir, ".git", "included.cfg")
	steps := []struct {
		name string
		run  func()
		want trackedChurnTotals
	}{
		{"an untracked attribute file", func() { r.write(".gitattributes", "a.txt -diff\n") }, binary},
		{"the attribute file removed", func() {
			if err := os.Remove(filepath.Join(r.dir, ".gitattributes")); err != nil {
				t.Fatal(err)
			}
		}, text},
		{"an attribute file git ignores", func() { r.write("dir/.gitattributes", "a.txt -diff\n") }, binary},
		{"the ignored attribute file emptied", func() { r.write("dir/.gitattributes", "") }, text},
		{"the repository's info/attributes", func() { writeFile(infoAttributes, "a.txt -diff\n") }, binary},
		{"info/attributes emptied", func() { writeFile(infoAttributes, "") }, text},
		{"a config naming an attribute file", func() {
			writeFile(globalAttributes, "a.txt -diff\n")
			r.git("config", "core.attributesFile", globalAttributes)
		}, binary},
		{"that attribute file rewritten", func() { writeFile(globalAttributes, "a.txt diff\n") }, text},
		{"that attribute file rewritten again", func() { writeFile(globalAttributes, "a.txt -diff\n") }, binary},
		{"the config changed back", func() { r.git("config", "--unset", "core.attributesFile") }, text},
		{"a config file the config includes", func() {
			writeFile(included, "[core]\n\tattributesFile = "+globalAttributes+"\n")
			r.git("config", "include.path", "included.cfg")
		}, binary},
		{"the included config file rewritten", func() { writeFile(included, "[core]\n\tattributesFile = /nonexistent\n") }, text},
	}
	for _, step := range steps {
		step.run()
		if whole := r.whole(r.dir); whole != step.want {
			t.Fatalf("whole-tree diff after %q = %+v, want %+v: the step must change the diff", step.name, whole, step.want)
		}
		if got := r.scan(core, r.dir); got != step.want {
			t.Fatalf("scan after %q = %+v, want %+v", step.name, got, step.want)
		}
	}
}

// The effective config is part of the context: one that changes how a
// file's content is read changes its counts.
func TestTrackedChurnForgetsItsMemoWhenTheConfigChanges(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("a.txt", "one\ntwo\nthree\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()

	r.write("a.txt", "one\r\nTWO\r\nthree\r\n")
	steps := []struct {
		name string
		run  func()
		want trackedChurnTotals
	}{
		{"line endings read as written", func() { r.git("config", "core.autocrlf", "false") }, trackedChurnTotals{insertions: 3, deletions: 3, files: 1}},
		{"line endings converted", func() { r.git("config", "core.autocrlf", "true") }, trackedChurnTotals{insertions: 1, deletions: 1, files: 1}},
		{"line endings read as written again", func() { r.git("config", "core.autocrlf", "false") }, trackedChurnTotals{insertions: 3, deletions: 3, files: 1}},
	}
	for _, step := range steps {
		step.run()
		if whole := r.whole(r.dir); whole != step.want {
			t.Fatalf("whole-tree diff under %q = %+v, want %+v: the step must change the diff", step.name, whole, step.want)
		}
		if got := r.scan(core, r.dir); got != step.want {
			t.Fatalf("scan under %q = %+v, want %+v", step.name, got, step.want)
		}
	}
}

// The attribute files above a path key only that path: a file starting or
// stopping to change in another directory, with or without its own
// attribute file, leaves every other stored result as it was.
func TestTrackedChurnKeepsItsMemoWhenAnotherDirectoryChanges(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("a.txt", "one\n")
	r.write("deep/er/b.txt", "one\n")
	r.write("attributed/.gitattributes", "*.txt text\n")
	r.write("attributed/c.txt", "one\n")
	r.write("attributed/zz/.gitattributes", "*.txt text\n")
	r.write("attributed/zz/d.txt", "one\n")
	r.write("other/.gitattributes", "*.txt text\n")
	r.write("other/e.txt", "one\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()

	r.write("a.txt", "two\n")
	r.write("attributed/zz/d.txt", "two\n")
	r.scan(core, r.dir)
	r.plant(core, "a.txt", 100)
	r.plant(core, "attributed/zz/d.txt", 1000)
	steps := []struct {
		name string
		run  func()
		want trackedChurnTotals
	}{
		{"a file in a directory with no attribute file starts changing", func() { r.write("deep/er/b.txt", "two\n") },
			trackedChurnTotals{insertions: 1101, deletions: 3, files: 3}},
		{"a file in a directory with its own attribute file starts changing", func() { r.write("other/e.txt", "two\n") },
			trackedChurnTotals{insertions: 1102, deletions: 4, files: 4}},
		{"a file under the attribute file above a planted one starts changing", func() { r.write("attributed/c.txt", "two\n") },
			trackedChurnTotals{insertions: 1103, deletions: 5, files: 5}},
		{"the last changed file in an attributed directory is restored", func() { r.git("checkout", "-q", "--", "other/e.txt") },
			trackedChurnTotals{insertions: 1102, deletions: 4, files: 4}},
		{"a file is committed", func() { r.git("commit", "-q", "-m", "c", "attributed/c.txt") },
			trackedChurnTotals{insertions: 1101, deletions: 3, files: 3}},
	}
	for _, step := range steps {
		step.run()
		if got := r.scan(core, r.dir); got != step.want {
			t.Fatalf("scan after %s = %+v, want the replayed %+v", step.name, got, step.want)
		}
	}

	// An attribute file rekeys the paths under it, and only them.
	r.write("attributed/.gitattributes", "*.txt -diff\n")
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 102, deletions: 3, files: 4}); got != want {
		t.Fatalf("scan after the attribute file above d.txt changed = %+v, want %+v", got, want)
	}
}

// The attribute file of the git installation is part of the context.
func TestTrackedChurnKeysTheSystemAttributeFile(t *testing.T) {
	hooks := installHookedGit(t)
	r := newChurnRepo(t)
	r.write("a.txt", "one\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	r.write("a.txt", "two\n")
	core := r.core()
	system := filepath.Join(t.TempDir(), "gitattributes")
	hooks.set("before-var", fmt.Sprintf("if [ \"$2\" = GIT_ATTR_SYSTEM ]; then echo %q; exit 0; fi\n", system))

	want := trackedChurnTotals{insertions: 1, deletions: 1, files: 1}
	if got := r.scan(core, r.dir); got != want {
		t.Fatalf("first scan = %+v, want %+v", got, want)
	}
	r.plant(core, "a.txt", 100)
	if got, replayed := r.scan(core, r.dir), (trackedChurnTotals{insertions: 100, deletions: 1, files: 1}); got != replayed {
		t.Fatalf("scan with nothing changed = %+v, want the replayed %+v", got, replayed)
	}
	if err := os.WriteFile(system, []byte("* text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := r.scan(core, r.dir); got != want {
		t.Fatalf("scan after the system attribute file appeared = %+v, want %+v", got, want)
	}
}

// A git that cannot name its attribute files keys nothing and is asked
// once. Each later scan runs what a scan without a memo runs: status and
// one whole-tree diff, pinned to the commit HEAD names.
func TestTrackedChurnKeysNothingWhenGitCannotNameItsAttributeFiles(t *testing.T) {
	hooks := installHookedGit(t)
	r := newChurnRepo(t)
	r.write("a.txt", "one\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	r.write("a.txt", "two\n")
	head := strings.TrimSpace(gitStdout(t, r.dir, "rev-parse", "HEAD"))
	core := r.core()
	r.clock.settle()

	hooks.set("before-var", "echo 'usage: git var (-l | <variable>)' >&2; exit 129\n")
	want := trackedChurnTotals{insertions: 1, deletions: 1, files: 1}
	if got := r.scanNow(core, r.dir); got != want {
		t.Fatalf("first scan = %+v, want %+v", got, want)
	}
	if asked := strings.Count(readFile(t, hooks.logPath), "var GIT_ATTR_"); asked != 1 {
		t.Fatalf("git var ran %d times, want 1", asked)
	}
	if memo := r.memo(core); len(memo.files) != 0 {
		t.Fatalf("the first scan keyed a file under an unknown context: %+v", memo.files)
	}
	for round := 0; round < 2; round++ {
		if err := os.Truncate(hooks.logPath, 0); err != nil {
			t.Fatal(err)
		}
		if got := r.scanNow(core, r.dir); got != want {
			t.Fatalf("later scan %d = %+v, want %+v", round, got, want)
		}
		if memo := r.memo(core); len(memo.files) != 0 {
			t.Fatalf("later scan %d keyed a file under an unknown context: %+v", round, memo.files)
		}
		ran := strings.Split(strings.TrimSpace(readFile(t, hooks.logPath)), "\n")
		if len(ran) != 3 {
			t.Fatalf("later scan %d ran %q, want the HEAD lookup, status and one diff", round, ran)
		}
		if argvs := diffArgvs(t, hooks); len(argvs) != 1 || !strings.HasSuffix(argvs[0], " "+head+" --") {
			t.Fatalf("later scan %d ran diffs %q, want one of the whole tree against %s", round, argvs, head)
		}
	}
}

// Git reads the attribute files outside the working tree through a link,
// so the file a link names is what the context keys.
func TestTrackedChurnKeysALinkedAttributeFileByItsTarget(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"info/attributes", "core.attributesFile"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			r := newChurnRepo(t)
			r.write("a.txt", "one\ntwo\nthree\n")
			r.git("add", "-A")
			r.git("commit", "-q", "-m", "base")
			r.write("a.txt", "one\nTWO\nthree\n")
			target := filepath.Join(t.TempDir(), "attributes")
			if err := os.WriteFile(target, []byte("a.txt -diff\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(r.dir, ".git", "info", "attributes")
			if source == "core.attributesFile" {
				link = filepath.Join(t.TempDir(), "link")
				r.git("config", "core.attributesFile", link)
			}
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			core := r.core()

			steps := []struct {
				content string
				want    trackedChurnTotals
			}{
				{"a.txt -diff\n", trackedChurnTotals{files: 1}},
				{"a.txt diff\n", trackedChurnTotals{insertions: 1, deletions: 1, files: 1}},
				{"a.txt -diff\n", trackedChurnTotals{files: 1}},
			}
			for _, step := range steps {
				if err := os.WriteFile(target, []byte(step.content), 0o644); err != nil {
					t.Fatal(err)
				}
				if whole := r.whole(r.dir); whole != step.want {
					t.Fatalf("whole-tree diff with %q = %+v, want %+v: the link must be followed", step.content, whole, step.want)
				}
				if got := r.scan(core, r.dir); got != step.want {
					t.Fatalf("scan with %q = %+v, want %+v", step.content, got, step.want)
				}
			}
		})
	}
}

// Attributes read from a tree change when the ref naming it moves, which
// no file or config this scan keys shows: nothing is keyed under them.
func TestTrackedChurnKeysNothingUnderAttributesFromATree(t *testing.T) {
	for _, source := range []string{"attr.tree", "GIT_ATTR_SOURCE"} {
		t.Run(source, func(t *testing.T) {
			r := newChurnRepo(t)
			r.write("a.txt", "one\ntwo\nthree\n")
			r.git("add", "-A")
			r.git("commit", "-q", "-m", "base")
			r.git("checkout", "-q", "-b", "attrs")
			r.write(".gitattributes", "a.txt -diff\n")
			r.git("add", "-A")
			r.git("commit", "-q", "-m", "binary")
			r.write(".gitattributes", "a.txt diff\n")
			r.git("commit", "-q", "-am", "text")
			r.git("checkout", "-q", "main")
			r.git("branch", "source", "attrs~1")
			if source == "attr.tree" {
				r.git("config", "attr.tree", "refs/heads/source")
			} else {
				t.Setenv("GIT_ATTR_SOURCE", "refs/heads/source")
			}
			r.write("a.txt", "one\nTWO\nthree\n")
			core := r.core()

			steps := []struct {
				ref  string
				want trackedChurnTotals
			}{
				{"attrs~1", trackedChurnTotals{files: 1}},
				{"attrs", trackedChurnTotals{insertions: 1, deletions: 1, files: 1}},
			}
			for _, step := range steps {
				r.git("update-ref", "refs/heads/source", step.ref)
				if whole := r.whole(r.dir); whole != step.want {
					t.Fatalf("whole-tree diff with the tree of %s = %+v, want %+v: the tree must supply the attributes", step.ref, whole, step.want)
				}
				for round := 0; round < 2; round++ {
					if got := r.scan(core, r.dir); got != step.want {
						t.Fatalf("scan %d with the tree of %s = %+v, want %+v", round, step.ref, got, step.want)
					}
					if memo := r.memo(core); len(memo.files) != 0 || memo.warm {
						t.Fatalf("scan %d keyed results under attributes from a tree: %+v", round, memo)
					}
				}
			}
		})
	}
}
