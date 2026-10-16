package providerpath

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// spawnPackages run a provider CLI. A test binary that links one can start a
// provider, so its TestMain must install the poison.
var spawnPackages = []string{
	"agent-overflow/internal/provider/claude",
	"agent-overflow/internal/provider/claudetui",
	"agent-overflow/internal/provider/codex",
	"agent-overflow/internal/textgen",
}

const selfPackage = "agent-overflow/internal/testutil/providerpath"

func TestEveryTestBinaryThatCanStartAProviderInstallsThePoison(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate source")
	}
	cmd := exec.Command("go", "list", "-test", "-f", `{{.ImportPath}}|{{join .Deps ","}}`, "./...")
	cmd.Dir = filepath.Join(filepath.Dir(source), "..", "..", "..")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var missing []string
	for line := range strings.Lines(string(out)) {
		binary, deps, _ := strings.Cut(strings.TrimSpace(line), "|")
		if !strings.HasSuffix(binary, ".test") {
			continue
		}
		linked := strings.Split(deps, ",")
		if slices.ContainsFunc(spawnPackages, func(p string) bool { return slices.Contains(linked, p) }) &&
			!slices.Contains(linked, selfPackage) {
			missing = append(missing, strings.TrimSuffix(binary, ".test"))
		}
	}
	if len(missing) > 0 {
		t.Fatalf("test packages that can start a provider without providerpath in TestMain:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

func TestPoisonRecordsAndNamesTheTest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the poison is a shell script")
	}
	t.Setenv("PATH", os.Getenv("PATH"))
	check, err := Install()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range Names {
		path, err := exec.LookPath(name)
		if err != nil || !strings.Contains(path, "agent-overflow-provider-path-") {
			t.Fatalf("LookPath(%s) = %q, %v; want the poison first on PATH", name, path, err)
		}
		run := exec.Command(name, "--version")
		run.Dir = t.TempDir()
		if err := run.Run(); err == nil {
			t.Fatalf("poisoned %s exited 0", name)
		}
	}
	err = check()
	if err == nil {
		t.Fatal("check reported no attempts")
	}
	for _, want := range []string{"TestPoisonRecordsAndNamesTheTest", "claude --version", "codex --version"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("check error does not name %q:\n%v", want, err)
		}
	}
}
