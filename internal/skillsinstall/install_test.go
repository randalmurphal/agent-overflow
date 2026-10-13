package skillsinstall

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

const testUpstream = "vendored at commit `3a604672c46cd8187d2b19980eae0a34f9f91138`. Mirrors x (version 1.2.3)."

func testTree(t *testing.T, files map[string]string) Tree {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	tree, err := LoadTree(fsys, []byte(testUpstream))
	if err != nil {
		t.Fatalf("LoadTree: %v", err)
	}
	return tree
}

var v1 = map[string]string{
	"alpha/SKILL.md":         "alpha v1",
	"alpha/scripts/run":      "#!/bin/sh\necho alpha\n",
	"beta/SKILL.md":          "beta v1",
	"beta/references/doc.md": "beta doc",
}

// listHome returns every file under home/skills with its mode, the manifest
// left out, as "path mode".
func listHome(t *testing.T, home string) []string {
	t.Helper()
	root := filepath.Join(home, "skills")
	var out []string
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() == ManifestName || entry.Name() == lockName {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, name)
		out = append(out, filepath.ToSlash(rel)+" "+info.Mode().Perm().String())
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func readHome(t *testing.T, home, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "skills", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestLoadTreeReadsThePinAndSkipsNodeModules(t *testing.T) {
	tree := testTree(t, map[string]string{
		"alpha/SKILL.md":                      "a",
		"alpha/scripts/node_modules/dep/x.js": "dependency",
		"README.md":                           "top-level files are not skills",
	})
	if tree.Version != "1.2.3" || tree.Commit != "3a604672c46cd8187d2b19980eae0a34f9f91138" {
		t.Errorf("pin = %q %q", tree.Version, tree.Commit)
	}
	if got := strings.Join(tree.Files(), ","); got != "alpha/SKILL.md" {
		t.Errorf("files = %s", got)
	}
	if _, err := LoadTree(fstest.MapFS{"a/SKILL.md": {}}, []byte("no pin")); err == nil {
		t.Error("a tree with no pin loaded")
	}
}

func TestDigestFollowsContent(t *testing.T) {
	base := testTree(t, v1).Digest
	if again := testTree(t, v1).Digest; again != base {
		t.Fatal("digest is not stable")
	}
	edited := map[string]string{}
	for k, v := range v1 {
		edited[k] = v
	}
	edited["beta/SKILL.md"] = "beta v2"
	if testTree(t, edited).Digest == base {
		t.Error("a content edit kept the digest")
	}
	delete(edited, "beta/SKILL.md")
	edited["beta/README.md"] = "beta v2"
	if testTree(t, edited).Digest == testTree(t, map[string]string{"alpha/SKILL.md": "alpha v1", "alpha/scripts/run": "#!/bin/sh\necho alpha\n", "beta/references/doc.md": "beta doc", "beta/SKILL.md": "beta v2"}).Digest {
		t.Error("a rename kept the digest")
	}
}

func TestInstallCopiesFlattenedWithModesAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	tree := testTree(t, v1)
	if status, err := GetStatus(home, tree); err != nil || status.Installed {
		t.Fatalf("fresh home status = %+v, %v", status, err)
	}
	if err := Install(home, tree); err != nil {
		t.Fatalf("Install: %v", err)
	}
	want := []string{
		"alpha/SKILL.md -rw-r--r--",
		"alpha/scripts/run -rwxr-xr-x",
		"beta/SKILL.md -rw-r--r--",
		"beta/references/doc.md -rw-r--r--",
	}
	if got := listHome(t, home); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("installed:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	info, err := os.Lstat(filepath.Join(home, "skills", "alpha"))
	if err != nil || !info.IsDir() {
		t.Fatalf("alpha is not a real directory: %v %v", info, err)
	}
	status, err := GetStatus(home, tree)
	if err != nil || !status.Installed || !status.Complete || !status.Current || status.Version != "1.2.3" {
		t.Fatalf("status = %+v, %v", status, err)
	}
	manifest := filepath.Join(home, "skills", ManifestName)
	before, _ := os.Stat(manifest)
	if err := Install(home, tree); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	after, _ := os.Stat(manifest)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("installing a current tree again rewrote the manifest")
	}
}

func TestInstallRefusesSkillsItDidNotWrite(t *testing.T) {
	home := t.TempDir()
	own := filepath.Join(home, "skills", "beta")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "SKILL.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Install(home, testTree(t, v1))
	var conflict *ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(err.Error(), own) {
		t.Fatalf("Install = %v, want a conflict naming %s", err, own)
	}
	if got := listHome(t, home); strings.Join(got, ",") != "beta/SKILL.md -rw-r--r--" {
		t.Fatalf("a refused install wrote: %v", got)
	}
	if _, err := os.Stat(filepath.Join(home, "skills", ManifestName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused install wrote a manifest: %v", err)
	}
}

func TestUpdateReplacesWhatTheManifestNames(t *testing.T) {
	home := t.TempDir()
	if err := Update(home, testTree(t, v1)); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Update of an empty home = %v", err)
	}
	if err := Install(home, testTree(t, v1)); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(home, "skills", "beta", "my-notes.md")
	if err := os.WriteFile(userFile, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	v2 := map[string]string{
		"alpha/SKILL.md":    "alpha v2",
		"alpha/scripts/run": "#!/bin/sh\necho alpha 2\n",
		"gamma/SKILL.md":    "gamma",
	}
	tree2 := testTree(t, v2)
	if status, _ := GetStatus(home, tree2); status.Current {
		t.Fatal("a changed tree reads as current")
	}
	if err := Update(home, tree2); err != nil {
		t.Fatalf("Update: %v", err)
	}
	want := []string{
		"alpha/SKILL.md -rw-r--r--",
		"alpha/scripts/run -rwxr-xr-x",
		"beta/my-notes.md -rw-r--r--",
		"gamma/SKILL.md -rw-r--r--",
	}
	if got := listHome(t, home); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("after update:\n%s", strings.Join(got, "\n"))
	}
	if got := readHome(t, home, "alpha/SKILL.md"); got != "alpha v2" {
		t.Errorf("alpha = %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, "skills", "beta", "references")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an emptied directory stayed: %v", err)
	}
	if status, _ := GetStatus(home, tree2); !status.Current {
		t.Error("updated home is not current")
	}
}

func TestRemoveDeletesOnlyWhatTheManifestNames(t *testing.T) {
	home := t.TempDir()
	if err := Remove(home); err != nil {
		t.Fatalf("Remove of an empty home: %v", err)
	}
	other := filepath.Join(home, "skills", "users-own", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Install(home, testTree(t, v1)); err != nil {
		t.Fatal(err)
	}
	added := filepath.Join(home, "skills", "alpha", "scripts", "local.sh")
	if err := os.WriteFile(added, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Remove(home); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	want := []string{"alpha/scripts/local.sh -rw-r--r--", "users-own/SKILL.md -rw-r--r--"}
	if got := listHome(t, home); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("after remove:\n%s", strings.Join(got, "\n"))
	}
	if _, err := os.Stat(filepath.Join(home, "skills", "beta")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("beta stayed: %v", err)
	}
	if status, err := GetStatus(home, testTree(t, v1)); err != nil || status.Installed {
		t.Fatalf("status after remove = %+v, %v", status, err)
	}
	if err := Remove(home); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}

// failingFS fails the second open of one file: LoadTree's read succeeds and
// the install's copy fails part way.
type failingFS struct {
	fstest.MapFS
	name  string
	mu    sync.Mutex
	opens int
}

func (f *failingFS) Open(name string) (fs.File, error) {
	if name == f.name {
		f.mu.Lock()
		f.opens++
		opens := f.opens
		f.mu.Unlock()
		if opens > 1 {
			return nil, errors.New("read failed")
		}
	}
	return f.MapFS.Open(name)
}

func TestInstallThatFailsPartWayIsVisibleAndRemovable(t *testing.T) {
	home := t.TempDir()
	fsys := &failingFS{MapFS: fstest.MapFS{}, name: "beta/SKILL.md"}
	for name, body := range v1 {
		fsys.MapFS[name] = &fstest.MapFile{Data: []byte(body)}
	}
	tree, err := LoadTree(fsys, []byte(testUpstream))
	if err != nil {
		t.Fatal(err)
	}
	err = Install(home, tree)
	if err == nil || !strings.Contains(err.Error(), "beta/SKILL.md") {
		t.Fatalf("Install = %v, want the failing file named", err)
	}
	status, err := GetStatus(home, tree)
	if err != nil || !status.Installed || status.Complete || status.Current {
		t.Fatalf("status after a failed install = %+v, %v", status, err)
	}
	if len(listHome(t, home)) == 0 {
		t.Fatal("the failure came before anything was written; the test proves nothing")
	}
	// The next install finishes it.
	if err := Install(home, testTree(t, v1)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if status, _ := GetStatus(home, testTree(t, v1)); !status.Complete || !status.Current {
		t.Fatalf("status after retry = %+v", status)
	}
	if err := Remove(home); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := listHome(t, home); len(got) != 0 {
		t.Fatalf("left behind: %v", got)
	}
}

// A file Remove cannot delete is reported and stays named, so a later Remove
// finishes the job.
func TestRemoveThatFailsKeepsNamingWhatIsLeft(t *testing.T) {
	home := t.TempDir()
	if err := Install(home, testTree(t, v1)); err != nil {
		t.Fatal(err)
	}
	// A non-empty directory where a file was cannot be removed by os.Remove.
	blocker := filepath.Join(home, "skills", "beta", "SKILL.md")
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(blocker, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := Remove(home)
	if err == nil || !strings.Contains(err.Error(), "SKILL.md") {
		t.Fatalf("Remove = %v, want the failure", err)
	}
	status, _ := GetStatus(home, testTree(t, v1))
	if !status.Installed || status.Complete {
		t.Fatalf("status after a failed remove = %+v", status)
	}
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatal(err)
	}
	if err := Remove(home); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "skills", ManifestName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("manifest stayed: %v", err)
	}
}
