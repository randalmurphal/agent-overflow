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
	"time"
)

// helperEnv makes the test binary run one change on a home and exit, so a
// test can race changes from separate processes. Its value is
// "home|variant|op", op one of install, update or remove.
const helperEnv = "AO_TEST_SKILLS_INSTALL"

func TestMain(m *testing.M) {
	if spec := os.Getenv(helperEnv); spec != "" {
		parts := strings.SplitN(spec, "|", 3)
		n, _ := strconv.Atoi(parts[1])
		if err := runChange(parts[2], parts[0], n); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runChange runs one racing change racingRepeats times, so the racers'
// changes overlap. An Update that finds nothing installed lost its race to
// a Remove, which is not a failure.
func runChange(op, home string, variant int) error {
	tree, err := LoadTree(variantTree(variant), []byte(testUpstream))
	if err != nil {
		return err
	}
	for range racingRepeats {
		switch op {
		case "install":
			err = Install(home, tree)
		case "update":
			if err = Update(home, tree); errors.Is(err, ErrNotInstalled) {
				err = nil
			}
		case "remove":
			err = Remove(home)
		default:
			err = fmt.Errorf("unknown op %q", op)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

const racingRepeats = 4

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

// Install, Update and Remove racing from separate processes and from
// goroutines on one home leave it consistent after every round: the
// manifest, when there is one, is complete, names every skill file on disk
// and names nothing that is missing; with no manifest there is no skill file.
func TestRacingChangesLeaveEveryFileNamed(t *testing.T) {
	changes := []struct {
		op      string
		variant int
		process bool
	}{
		{"install", 1, true}, {"update", 2, true}, {"remove", 0, true},
		{"install", 3, false}, {"update", 4, false}, {"remove", 0, false},
	}
	for round := range 6 {
		home := t.TempDir()
		if err := runChange("install", home, 0); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, len(changes))
		for i, change := range changes {
			// Rotate the start order so each change leads some rounds.
			change = changes[(i+round)%len(changes)]
			if change.process {
				cmd := exec.Command(os.Args[0], "-test.run=^$")
				cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%s|%d|%s", helperEnv, home, change.variant, change.op))
				wg.Go(func() {
					if out, err := cmd.CombinedOutput(); err != nil {
						errs <- fmt.Errorf("process %s %d: %v: %s", change.op, change.variant, err, out)
					}
				})
				continue
			}
			wg.Go(func() {
				if err := runChange(change.op, home, change.variant); err != nil {
					errs <- fmt.Errorf("goroutine %s %d: %w", change.op, change.variant, err)
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("round %d: %v", round, err)
		}
		checkEveryFileNamed(t, home, round)
	}
}

func checkEveryFileNamed(t *testing.T, home string, round int) {
	t.Helper()
	root, err := os.OpenRoot(filepath.Join(home, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	m, err := readManifest(root)
	if err != nil || (m != nil && !m.Complete) {
		t.Fatalf("round %d: manifest = %+v, %v", round, m, err)
	}
	var named map[string]bool
	if m != nil {
		named = setOf(m.Files)
	}
	onDisk := map[string]bool{}
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || name == ManifestName {
			return err
		}
		onDisk[name] = true
		if !named[name] {
			t.Errorf("round %d: %s is on disk and not in the manifest", round, name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name := range named {
		if !onDisk[name] {
			t.Errorf("round %d: the manifest names %s and it is missing", round, name)
		}
	}
}

// Install, Update and Remove each wait for a change holding the home's
// lock, touching nothing until it is released.
func TestChangesWaitForTheLock(t *testing.T) {
	next := map[string]string{}
	for name, body := range v1 {
		next[name] = body + " v2"
	}
	for op, change := range map[string]func(home string) error{
		"Install": func(home string) error { return Install(home, testTree(t, next)) },
		"Update":  func(home string) error { return Update(home, testTree(t, next)) },
		"Remove":  Remove,
	} {
		t.Run(op, func(t *testing.T) {
			home := t.TempDir()
			if err := Install(home, testTree(t, v1)); err != nil {
				t.Fatal(err)
			}
			before := listHome(t, home)
			unlock, _, err := lockHome(home, false, false)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- change(home) }()
			select {
			case err := <-done:
				t.Fatalf("%s returned while the lock was held: %v", op, err)
			case <-time.After(200 * time.Millisecond):
			}
			if got := listHome(t, home); strings.Join(got, ",") != strings.Join(before, ",") || readHome(t, home, "alpha/SKILL.md") != "alpha v1" {
				t.Fatalf("%s changed the home while the lock was held: %v", op, got)
			}
			if err := unlock(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatalf("%s after the lock was released: %v", op, err)
			}
		})
	}
}

// A status read while a change holds the lock waits for it and reports
// what the change finished, never the unfinished install part way.
func TestGetStatusWaitsForAChangeInProgress(t *testing.T) {
	home := t.TempDir()
	tree := testTree(t, v1)
	if err := Install(home, tree); err != nil {
		t.Fatal(err)
	}
	unlock, _, err := lockHome(home, false, false)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Join(home, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	m, err := readManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	m.Complete = false
	if err := writeManifest(root, *m); err != nil {
		t.Fatal(err)
	}
	type result struct {
		status Status
		err    error
	}
	read := make(chan result, 1)
	go func() {
		status, err := GetStatus(home, tree)
		read <- result{status, err}
	}()
	select {
	case got := <-read:
		t.Fatalf("GetStatus returned during the change: %+v, %v", got.status, got.err)
	case <-time.After(200 * time.Millisecond):
	}
	m.Complete = true
	if err := writeManifest(root, *m); err != nil {
		t.Fatal(err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	got := <-read
	if got.err != nil || !got.status.Complete || !got.status.Current {
		t.Fatalf("GetStatus after the change = %+v, %v", got.status, got.err)
	}
}
