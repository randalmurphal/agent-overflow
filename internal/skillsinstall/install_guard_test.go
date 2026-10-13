package skillsinstall

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

// helperEnv makes the test binary install one tree into a home and exit, so
// a test can race installs from separate processes.
const helperEnv = "AO_TEST_SKILLS_INSTALL"

func TestMain(m *testing.M) {
	if spec := os.Getenv(helperEnv); spec != "" {
		home, variant, _ := strings.Cut(spec, "|")
		n, _ := strconv.Atoi(variant)
		tree, err := LoadTree(variantTree(n), []byte(testUpstream))
		if err == nil {
			err = Install(home, tree)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// variantTree is a tree of many files whose skills overlap with the other
// variants' and differ from them, so racing installs touch the same names.
func variantTree(n int) fstest.MapFS {
	fsys := fstest.MapFS{}
	skills := []string{"shared", fmt.Sprintf("only-%d", n)}
	for _, skill := range skills {
		for i := range 150 {
			fsys[fmt.Sprintf("%s/f%03d-%d.md", skill, i, n%2)] = &fstest.MapFile{Data: []byte(fmt.Sprintf("variant %d file %d", n, i))}
		}
	}
	return fsys
}

func writeRawManifest(t *testing.T, home string, m manifest) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "skills", ManifestName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A manifest naming anything outside its own skill directories is refused as
// unreadable: Remove and Update delete what it names.
func TestManifestNamingOutsideItsSkillsIsRefused(t *testing.T) {
	cases := map[string]manifest{
		"parent file":         {Skills: []string{"alpha"}, Files: []string{"../settings.json"}},
		"parent inside skill": {Skills: []string{"alpha"}, Files: []string{"alpha/../../settings.json"}},
		"absolute file":       {Skills: []string{"alpha"}, Files: []string{"/settings.json"}},
		"file of no skill":    {Skills: []string{"alpha"}, Files: []string{"beta/SKILL.md"}},
		"top-level file":      {Skills: []string{"alpha"}, Files: []string{"settings.json"}},
		"parent skill":        {Skills: []string{".."}, Files: []string{"../settings.json"}},
		"nested skill":        {Skills: []string{"a/b"}, Files: []string{"a/b/c"}},
		"dot skill":           {Skills: []string{"."}, Files: []string{"./x"}},
		"hidden skill":        {Skills: []string{".agent-overflow-pstack.json"}},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			victim := filepath.Join(home, "settings.json")
			if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			bad.Complete = true
			writeRawManifest(t, home, bad)

			if err := Remove(home); err == nil || !strings.Contains(err.Error(), "skills manifest") {
				t.Fatalf("Remove = %v, want the manifest refused", err)
			}
			if err := Update(home, testTree(t, v1)); err == nil || !strings.Contains(err.Error(), "skills manifest") {
				t.Fatalf("Update = %v, want the manifest refused", err)
			}
			if _, err := GetStatus(home, testTree(t, v1)); err == nil {
				t.Fatal("GetStatus read a manifest that names outside its skills")
			}
			if raw, err := os.ReadFile(victim); err != nil || string(raw) != "keep" {
				t.Fatalf("a path outside the skills was touched: %q %v", raw, err)
			}
		})
	}
}

// A file the user added inside a skill directory AO owns refuses an Install
// or Update whose tree now ships that file, and nothing is written.
func TestInstallRefusesAUserFileInsideAnOwnedSkill(t *testing.T) {
	home := t.TempDir()
	if err := Install(home, testTree(t, v1)); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(home, "skills", "alpha", "new.md")
	if err := os.WriteFile(userFile, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	next := map[string]string{"alpha/new.md": "shipped"}
	for k, v := range v1 {
		next[k] = v + " v2"
	}
	before := listHome(t, home)
	manifestBefore, _ := os.ReadFile(filepath.Join(home, "skills", ManifestName))
	for name, run := range map[string]func(string, Tree) error{"Install": Install, "Update": Update} {
		err := run(home, testTree(t, next))
		var conflict *ConflictError
		if !errors.As(err, &conflict) || !strings.Contains(err.Error(), userFile) {
			t.Fatalf("%s = %v, want a conflict naming %s", name, err, userFile)
		}
		if got := listHome(t, home); strings.Join(got, ",") != strings.Join(before, ",") {
			t.Fatalf("%s wrote files: %v", name, got)
		}
		if raw, _ := os.ReadFile(userFile); string(raw) != "mine" {
			t.Fatalf("%s overwrote the user's file", name)
		}
		if raw, _ := os.ReadFile(filepath.Join(home, "skills", "alpha", "SKILL.md")); string(raw) != "alpha v1" {
			t.Fatalf("%s rewrote an owned file before refusing", name)
		}
		if after, _ := os.ReadFile(filepath.Join(home, "skills", ManifestName)); string(after) != string(manifestBefore) {
			t.Fatalf("%s rewrote the manifest", name)
		}
	}
}

// Installs racing from two processes and two goroutines on one home leave a
// home whose manifest names every skill file on it.
func TestRacingInstallsLeaveEveryFileNamed(t *testing.T) {
	for round := range 4 {
		home := t.TempDir()
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for n := 1; n <= 2; n++ {
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%s|%d", helperEnv, home, n))
			wg.Go(func() {
				if out, err := cmd.CombinedOutput(); err != nil {
					errs <- fmt.Errorf("process %d: %v: %s", n, err, out)
				}
			})
		}
		for n := 3; n <= 4; n++ {
			wg.Go(func() {
				tree, err := LoadTree(variantTree(n), []byte(testUpstream))
				if err == nil {
					err = Install(home, tree)
				}
				if err != nil {
					errs <- fmt.Errorf("goroutine %d: %w", n, err)
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("round %d: %v", round, err)
		}
		m, err := readManifest(home)
		if err != nil || m == nil || !m.Complete {
			t.Fatalf("round %d: manifest = %+v, %v", round, m, err)
		}
		named := setOf(m.Files)
		root := filepath.Join(home, "skills")
		err = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || entry.Name() == ManifestName || entry.Name() == lockName {
				return err
			}
			rel, _ := filepath.Rel(root, name)
			if !named[filepath.ToSlash(rel)] {
				t.Errorf("round %d: %s is on disk and not in the manifest", round, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
