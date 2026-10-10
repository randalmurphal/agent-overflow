package app

import (
	"agent-overflow/internal/testutil/mockexec"
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
		script := "#!/bin/sh\ntouch '" + filepath.Join(markers, name) + "'\necho 'https://forge.example/real-cli/1'\n"
		mockexec.Write(t, filepath.Join(bin, name), script)
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
// it stands in for and the control env it was given, then answers every
// call with the same line.
func writeFakeForge(t *testing.T) (path, record string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "ao-mockforge")
	record = filepath.Join(dir, "record")
	script := "#!/bin/sh\n" +
		"printf '%s %s\\n' \"$AO_FORGE_CLI\" \"$AO_HARNESS_CONTROL\" >> '" + record + "'\n" +
		"echo 'https://forge.example/from-fake/7'\n"
	mockexec.Write(t, path, script)
	return path, record
}

// runForgeCLI makes the one call that runs the forge's CLI, `gh pr create`
// or `glab mr create`; every other forge call goes through the forge API
// transport.
func runForgeCLI(t *testing.T, core *gitops.Core, forge string) (string, error) {
	t.Helper()
	return core.ForgeByID(forge).CreatePR(t.Context(), t.TempDir(), "title", "", "", false)
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
		out, err := runForgeCLI(t, core, forge)
		if err != nil {
			t.Fatalf("%s forge CLI call: %v", forge, err)
		}
		if !strings.Contains(out, "from-fake") {
			t.Fatalf("%s forge CLI call = %q, want the fake's answer", forge, out)
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
		_, err := runForgeCLI(t, app.gitCore(), forge)
		if _, ok := errors.AsType[*gitops.ForgeCLIUnavailableError](err); !ok {
			t.Fatalf("%s forge CLI call error = %v, want ForgeCLIUnavailableError", forge, err)
		}
	}
	if ran := trapsThatRan(t, markers); len(ran) != 0 {
		t.Fatalf("the real %v on PATH ran under an isolated App", ran)
	}

	// The control: the same PATH reaches the trap from an ordinary App,
	// so the assertions above are not passing because it was unreachable.
	if _, err := runForgeCLI(t, (&App{}).gitCore(), "gitlab"); err != nil {
		t.Fatalf("desktop glab mr create through the trap: %v", err)
	}
	if ran := trapsThatRan(t, markers); strings.Join(ran, ",") != "glab" {
		t.Fatalf("desktop App ran %v, want the glab on PATH", ran)
	}
}

// Every git.Core the app package builds must come from buildGitCore, or an
// isolated boot would hold a Core that resolves gh and glab on PATH.
func TestAppBuildsGitCoresOnlyThroughNewGitCore(t *testing.T) {
	t.Parallel()
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
			if count != 1 {
				t.Errorf("app_git.go calls gitops.NewCore %d times; only buildGitCore may", count)
			}
			continue
		}
		if count != 0 {
			t.Errorf("%s builds a git.Core directly; use a.buildGitCore()", path)
		}
	}
}
