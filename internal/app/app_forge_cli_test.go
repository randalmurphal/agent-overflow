package app

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
)

// installForgeTraps puts gh and glab scripts first on PATH that record they
// ran. Returns the marker directory.
func installForgeTraps(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("trap CLIs are shell scripts")
	}
	bin := t.TempDir()
	markers := t.TempDir()
	for _, name := range []string{"gh", "glab"} {
		script := "#!/bin/sh\ntouch '" + filepath.Join(markers, name) + "'\necho '{\"title\":\"real cli\"}'\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return markers
}

func trapsThatRan(t *testing.T, markers string) []string {
	t.Helper()
	entries, err := os.ReadDir(markers)
	if err != nil {
		t.Fatal(err)
	}
	var ran []string
	for _, entry := range entries {
		ran = append(ran, entry.Name())
	}
	return ran
}

// writeFakeForge writes a stand-in for ao-mockforge that records the CLI
// it stands in for and the control env it was given, then answers
// `pr view` and the GitLab MR endpoint with a title.
func writeFakeForge(t *testing.T) (path, record string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "ao-mockforge")
	record = filepath.Join(dir, "record")
	script := "#!/bin/sh\n" +
		"printf '%s %s\\n' \"$AO_FORGE_CLI\" \"$AO_HARNESS_CONTROL\" >> '" + record + "'\n" +
		"echo '{\"title\":\"from fake\",\"iid\":7}'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, record
}

// The seam an isolated boot relies on: ConfigureIsolation plus the control
// env is all an App needs for every git.Core it builds to run the fake as
// gh and glab, with the harness's control credentials, and never the CLI on
// PATH.
func TestIsolatedAppRunsTheFakeForgeCLI(t *testing.T) {
	markers := installForgeTraps(t)
	fake, record := writeFakeForge(t)
	app := &App{}
	ConfigureIsolation(app, IsolationConfig{ForgeCLI: fake})
	SetProviderExtraEnv(app, map[string]string{"AO_HARNESS_CONTROL": "http://127.0.0.1:1/control"})

	core := app.gitCore()
	for _, forge := range []string{"github", "gitlab"} {
		meta, err := core.ForgeByID(forge).ViewPR("", "acme/widgets", 7)
		if err != nil {
			t.Fatalf("%s ViewPR: %v", forge, err)
		}
		if meta.Title != "from fake" {
			t.Fatalf("%s ViewPR title = %q, want the fake's answer", forge, meta.Title)
		}
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	want := "gh http://127.0.0.1:1/control\nglab http://127.0.0.1:1/control\n"
	if string(got) != want {
		t.Fatalf("fake saw %q, want %q", got, want)
	}
	if ran := trapsThatRan(t, markers); len(ran) != 0 {
		t.Fatalf("the real %v on PATH ran under an isolated App", ran)
	}
}

func TestIsolatedAppWithoutFakeRefusesForgeCLIs(t *testing.T) {
	markers := installForgeTraps(t)
	app := &App{}
	ConfigureIsolation(app, IsolationConfig{})

	for _, forge := range []string{"github", "gitlab"} {
		_, err := app.gitCore().ForgeByID(forge).ViewPR("", "acme/widgets", 7)
		if _, ok := errors.AsType[*gitops.ForgeCLIUnavailableError](err); !ok {
			t.Fatalf("%s ViewPR error = %v, want ForgeCLIUnavailableError", forge, err)
		}
	}
	if ran := trapsThatRan(t, markers); len(ran) != 0 {
		t.Fatalf("the real %v on PATH ran under an isolated App", ran)
	}

	// The control: the same PATH reaches the trap from an ordinary App,
	// so the assertions above are not passing because it was unreachable.
	if _, err := (&App{}).gitCore().ForgeByID("github").ViewPR("", "acme/widgets", 7); err != nil {
		t.Fatalf("desktop ViewPR through the trap: %v", err)
	}
	if ran := trapsThatRan(t, markers); strings.Join(ran, ",") != "gh" {
		t.Fatalf("desktop App ran %v, want the gh on PATH", ran)
	}
}

// Every git.Core the app package builds must come from newGitCore, or an
// isolated boot would hold a Core that resolves gh and glab on PATH.
func TestAppBuildsGitCoresOnlyThroughNewGitCore(t *testing.T) {
	paths, err := filepath.Glob("internal/app/*.go")
	if err != nil {
		t.Fatal(err)
	}
	direct := regexp.MustCompile(`gitops\.NewCore\(`)
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		count := len(direct.FindAllIndex(source, -1))
		if filepath.Base(path) == "app_git.go" {
			if count != 2 {
				t.Errorf("app_git.go calls gitops.NewCore %d times; only newGitCore's two branches may", count)
			}
			continue
		}
		if count != 0 {
			t.Errorf("%s builds a git.Core directly; use a.newGitCore()", path)
		}
	}
}
