package git

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// forgeFakeHelperEnv turns this test binary into a fake forge CLI: it
// reports what it was started with as JSON on stdout and exits.
const forgeFakeHelperEnv = "AO_GIT_TEST_FORGE_FAKE"

type forgeFakeReport struct {
	Argv0 string   `json:"argv0"`
	Args  []string `json:"args"`
	Stdin string   `json:"stdin"`
	Cwd   string   `json:"cwd"`
	Env   string   `json:"env"`
	CLI   string   `json:"cli"`
}

func TestMain(m *testing.M) {
	if os.Getenv(forgeFakeHelperEnv) == "1" {
		stdin, _ := io.ReadAll(os.Stdin)
		cwd, _ := os.Getwd()
		_ = json.NewEncoder(os.Stdout).Encode(forgeFakeReport{
			Argv0: os.Args[0],
			Args:  os.Args[1:],
			Stdin: string(stdin),
			Cwd:   cwd,
			Env:   os.Getenv("AO_GIT_TEST_FORGE_EXTRA"),
			CLI:   os.Getenv(ForgeCLINameEnv),
		})
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// installTrapCLIs puts executables named gh and glab first on PATH. Each
// one records that it ran, so a test can prove the real CLIs were never
// reached. Returns the marker directory.
func installTrapCLIs(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("trap CLIs are shell scripts")
	}
	bin := t.TempDir()
	markers := t.TempDir()
	for _, name := range forgeCLINames {
		script := "#!/bin/sh\ntouch '" + filepath.Join(markers, name) + "'\necho trapped\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return markers
}

func assertNoTrapRan(t *testing.T, markers string) {
	t.Helper()
	entries, err := os.ReadDir(markers)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("the %s on PATH ran under an isolated Core", entry.Name())
	}
}

func TestTrapCLIsAreReachableWithoutIsolation(t *testing.T) {
	// The control for the tests below: the same PATH, an ordinary Core,
	// and the trap does run. Without this the isolation assertions could
	// pass because the trap was never reachable at all.
	markers := installTrapCLIs(t)
	result, err := NewCore().runBinary("gh", "", "api", "user")
	if err != nil {
		t.Fatalf("runBinary: %v", err)
	}
	if strings.TrimSpace(result.stdout) != "trapped" {
		t.Fatalf("stdout = %q, want the trap's answer", result.stdout)
	}
	if _, err := os.Stat(filepath.Join(markers, "gh")); err != nil {
		t.Fatalf("trap marker missing: %v", err)
	}
}

func TestIsolatedCoreWithoutFakeRefusesForgeCLIs(t *testing.T) {
	markers := installTrapCLIs(t)
	core := NewCore(WithIsolatedForgeCLIs("", nil))

	for _, forge := range []string{"github", "gitlab"} {
		_, err := core.ForgeByID(forge).ViewPR("", "acme/widgets", 7)
		var unavailable *ForgeCLIUnavailableError
		if !errors.As(err, &unavailable) {
			t.Fatalf("%s ViewPR error = %v, want ForgeCLIUnavailableError", forge, err)
		}
		if !strings.Contains(err.Error(), "isolated boot") {
			t.Errorf("error %q does not say why", err)
		}
	}
	if _, _, err := core.FetchAttachment("", PRReference{Forge: "github", Namespace: "acme", Repo: "widgets", Number: 7},
		"https://github.com/user-attachments/assets/0f1e2d3c", 1<<20); err == nil {
		t.Fatal("attachment download succeeded with no fake configured")
	}
	if _, err := core.runBinary("curl", "", "https://example.invalid"); err == nil {
		t.Fatal("an isolated Core ran a binary that is neither git nor a forge CLI")
	}
	assertNoTrapRan(t, markers)
}

func TestIsolatedCoreRunsTheFakeAsTheForgeCLI(t *testing.T) {
	markers := installTrapCLIs(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	core := NewCore(WithIsolatedForgeCLIs(self, []string{
		forgeFakeHelperEnv + "=1",
		"AO_GIT_TEST_FORGE_EXTRA=control",
	}))

	for _, binary := range forgeCLINames {
		result, err := core.runBinaryInput(binary, cwd, `{"event":"APPROVE"}`, "api", "repos/acme/widgets/pulls/7/reviews", "-X", "POST", "--input", "-")
		if err != nil {
			t.Fatalf("%s: %v", binary, err)
		}
		var report forgeFakeReport
		if err := json.Unmarshal([]byte(result.stdout), &report); err != nil {
			t.Fatalf("%s: decode fake report %q: %v", binary, result.stdout, err)
		}
		if report.Argv0 != binary || report.CLI != binary {
			t.Errorf("argv[0] = %q, %s = %q, want both %q", report.Argv0, ForgeCLINameEnv, report.CLI, binary)
		}
		wantArgs := []string{"api", "repos/acme/widgets/pulls/7/reviews", "-X", "POST", "--input", "-"}
		if strings.Join(report.Args, "\x00") != strings.Join(wantArgs, "\x00") {
			t.Errorf("args = %q, want %q", report.Args, wantArgs)
		}
		if report.Stdin != `{"event":"APPROVE"}` {
			t.Errorf("stdin = %q", report.Stdin)
		}
		if resolved, _ := filepath.EvalSymlinks(cwd); report.Cwd != cwd && report.Cwd != resolved {
			t.Errorf("cwd = %q, want %q", report.Cwd, cwd)
		}
		if report.Env != "control" {
			t.Errorf("fake env = %q, want the configured control env", report.Env)
		}
	}

	// Git is not a forge CLI: it keeps resolving on PATH.
	result, err := core.run(cwd, "--version")
	if err != nil || !strings.HasPrefix(result.stdout, "git version") {
		t.Fatalf("git under an isolated Core: %q, %v", result.stdout, err)
	}
	assertNoTrapRan(t, markers)
}

func TestForgeCLIEnvReachesOnlyTheFake(t *testing.T) {
	core := NewCore(WithIsolatedForgeCLIs("/fake/ao-mockforge", []string{"AO_GIT_TEST_ONLY_FAKE=1"}))
	target, err := core.forgeCLIs.resolve("git")
	if err != nil {
		t.Fatal(err)
	}
	if target.path != "git" || len(target.env) != 0 {
		t.Fatalf("git resolves to %+v; it must stay on PATH without the fake's env", target)
	}
}
