package app

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// localPathPatterns match what a capture carries from the machine that
// recorded it. A match whose name group is "user" is the placeholder the
// committed references use.
var localPathPatterns = []struct {
	re *regexp.Regexp
	// name is the submatch holding a user name, or 0 when any match leaks.
	name int
}{
	{regexp.MustCompile(`/(home|Users)/([^/\s"'\\]+)/`), 2},
	// Claude encodes a cwd into a directory name with dashes.
	{regexp.MustCompile(`-(home|Users)-([A-Za-z0-9_.]+)-`), 2},
	// Claude's per-user temp root on a Linux developer machine.
	{regexp.MustCompile(`claude-1000`), 0},
	// An asset-tag host name.
	{regexp.MustCompile(`\b[A-Z]{2,6}-[0-9]{5,}\b`), 0},
}

// scanLocalPaths reports every line under root that carries a local path
// or host name, as file:line: match.
func scanLocalPaths(root string) (found []string, files int, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			for _, p := range localPathPatterns {
				for _, m := range p.re.FindAllStringSubmatch(line, -1) {
					if p.name == 0 || m[p.name] != "user" {
						found = append(found, fmt.Sprintf("%s:%d: %s", path, i+1, m[0]))
					}
				}
			}
		}
		return nil
	})
	return found, files, err
}

// The committed provider references and fixtures carry placeholders, not
// the paths or host name of the machine that captured them.
func TestReferencesCarryNoLocalPaths(t *testing.T) {
	t.Parallel()
	found, files, err := scanLocalPaths(filepath.Join("docs", "references"))
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no reference files scanned")
	}
	if len(found) > 0 {
		t.Fatalf("replace with /home/user, /tmp/ao-fixture or a neutral host:\n%s", strings.Join(found, "\n"))
	}
}

func TestScanLocalPathsFindsEachLeak(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("clean.ndjson", `{"cwd":"/home/user/repo","dir":"-home-user-repo","mac":"/Users/user/repo","tmp":"/tmp/ao-fixture/-work-repo"}`+"\n")
	write("leaks.ndjson", strings.Join([]string{
		`{"cwd":"/home/someone/repo"}`,
		`{"dir":"/tmp/ao-fixture/-home-someone-repo"}`,
		`{"mac":"/Users/someone/repo"}`,
		`{"out":"/tmp/claude-1000/-work-repo/tasks/a.output"}`,
		`{"host":"ABC-12345"}`,
	}, "\n")+"\n")
	found, files, err := scanLocalPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if files != 2 {
		t.Fatalf("scanned %d files, want 2", files)
	}
	want := []string{
		"leaks.ndjson:1: /home/someone/",
		"leaks.ndjson:2: -home-someone-",
		"leaks.ndjson:3: /Users/someone/",
		"leaks.ndjson:4: claude-1000",
		"leaks.ndjson:5: ABC-12345",
	}
	if len(found) != len(want) {
		t.Fatalf("found %q, want %q", found, want)
	}
	for i, w := range want {
		if w = root + string(filepath.Separator) + w; found[i] != w {
			t.Errorf("finding %d = %q, want %q", i, found[i], w)
		}
	}
}
