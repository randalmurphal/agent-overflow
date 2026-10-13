package skillsinstall

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// linkedHome installs v1 into a new home, then replaces the directory link
// (an owned skill, or a directory inside one) with a link to a directory
// holding a file under every name v1 puts below link, each reading
// "theirs". inside puts that directory in the skills directory, so the link
// does not leave the root. It returns the home and the directory.
func linkedHome(t *testing.T, link string, inside bool) (home, target string) {
	t.Helper()
	home = t.TempDir()
	if err := Install(home, testTree(t, v1)); err != nil {
		t.Fatal(err)
	}
	target = filepath.Join(t.TempDir(), "outside")
	if inside {
		target = filepath.Join(home, "skills", "elsewhere")
	}
	for name := range v1 {
		if rest, ok := strings.CutPrefix(name, link+"/"); ok {
			theirs := filepath.Join(target, filepath.FromSlash(rest))
			if err := os.MkdirAll(filepath.Dir(theirs), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(theirs, []byte("theirs"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	at := filepath.Join(home, "skills", filepath.FromSlash(link))
	if err := os.RemoveAll(at); err != nil {
		t.Fatal(err)
	}
	// A relative link: the root reads an absolute one as leaving it.
	rel, err := filepath.Rel(filepath.Dir(at), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, at); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	return home, target
}

// checkTheirs fails when target holds anything but the files linkedHome
// put there, each still reading "theirs".
func checkTheirs(t *testing.T, target string, link string) {
	t.Helper()
	var want []string
	for name := range v1 {
		if rest, ok := strings.CutPrefix(name, link+"/"); ok {
			want = append(want, rest)
		}
	}
	sort.Strings(want)
	var got []string
	err := filepath.WalkDir(target, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(target, name)
		got = append(got, filepath.ToSlash(rel))
		if raw, err := os.ReadFile(name); err != nil || string(raw) != "theirs" {
			t.Errorf("%s was written through the link: %q, %v", rel, raw, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the linked directory holds %v, want %v", got, want)
	}
}

var linkCases = []struct {
	name   string
	link   string
	inside bool
}{
	{"owned skill linked outside", "beta", false},
	{"directory in a skill linked outside", "alpha/scripts", false},
	{"owned skill linked inside the skills directory", "beta", true},
	{"directory in a skill linked inside the skills directory", "alpha/scripts", true},
}

// A link where a directory of an owned skill was refuses Install and Update
// as a conflict naming it, whether it leaves the skills directory or not:
// the manifest's names would no longer each mean one file.
func TestChangesRefuseALinkedDirectory(t *testing.T) {
	next := map[string]string{}
	for name, body := range v1 {
		next[name] = body + " v2"
	}
	for _, c := range linkCases {
		t.Run(c.name, func(t *testing.T) {
			home, target := linkedHome(t, c.link, c.inside)
			manifestBefore := readHome(t, home, ManifestName)
			for op, run := range map[string]func(string, Tree) error{"Install": Install, "Update": Update} {
				err := run(home, testTree(t, next))
				var conflict *ConflictError
				want := filepath.Join(home, "skills", filepath.FromSlash(c.link))
				if !errors.As(err, &conflict) || !strings.Contains(err.Error(), want) {
					t.Fatalf("%s = %v, want a conflict naming %s", op, err, want)
				}
				checkTheirs(t, target, c.link)
				if got := readHome(t, home, ManifestName); got != manifestBefore {
					t.Fatalf("%s rewrote the manifest", op)
				}
			}
		})
	}
}

// Remove deletes the link where a directory of the install was, never what
// it points to, and the rest of the install as usual.
func TestRemoveDeletesALinkNotThroughIt(t *testing.T) {
	for _, c := range linkCases {
		t.Run(c.name, func(t *testing.T) {
			home, target := linkedHome(t, c.link, c.inside)
			if err := Remove(home); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			checkTheirs(t, target, c.link)
			if _, err := os.Lstat(filepath.Join(home, "skills", filepath.FromSlash(c.link))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the link stayed: %v", err)
			}
			var want []string
			if c.inside {
				// The target is a directory the user made in skills/.
				for name := range v1 {
					if rest, ok := strings.CutPrefix(name, c.link+"/"); ok {
						want = append(want, "elsewhere/"+rest+" -rw-r--r--")
					}
				}
			}
			sort.Strings(want)
			if got := listHome(t, home); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("after remove: %v, want %v", got, want)
			}
		})
	}
}

// Remove prunes no directory through a link: an empty directory in the
// linked one, where the install had one, stays.
func TestRemovePrunesNothingThroughALink(t *testing.T) {
	for _, inside := range []bool{false, true} {
		home, target := linkedHome(t, "beta", inside)
		emptied := filepath.Join(target, "references")
		if err := os.Remove(filepath.Join(emptied, "doc.md")); err != nil {
			t.Fatal(err)
		}
		if err := Remove(home); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if info, err := os.Stat(emptied); err != nil || !info.IsDir() {
			t.Fatalf("inside=%v: the linked directory's %s was pruned: %v", inside, emptied, err)
		}
	}
}

// The root, not each caller's check, keeps writes and reads in the skills
// directory: a write or a manifest read through a link that leaves it fails.
func TestTheSkillsRootRefusesALinkOut(t *testing.T) {
	home, target := linkedHome(t, "beta", false)
	root, err := os.OpenRoot(filepath.Join(home, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	tree := testTree(t, map[string]string{"beta/SKILL.md": "ours"})
	if err := copyFile(root, tree.fsys, tree.files[0]); err == nil {
		t.Fatal("copyFile wrote through a link out of the skills directory")
	}
	if err := writeAtomic(root, "beta/references/doc.md", strings.NewReader("ours"), 0o644); err == nil {
		t.Fatal("writeAtomic wrote through a link out of the skills directory")
	}
	checkTheirs(t, target, "beta")

	manifestOut := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.Rename(filepath.Join(home, "skills", ManifestName), manifestOut); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(manifestOut, filepath.Join(home, "skills", ManifestName)); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifest(root); err == nil {
		t.Fatal("readManifest read through a link out of the skills directory")
	}
}
