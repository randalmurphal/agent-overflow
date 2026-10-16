package providerpath

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// spawnPackages hold the exec sites that run a provider CLI: provider
// (DetectProvider's version probe, Spawn), the Claude, Claude TUI and Codex
// session and probe code, and textgen's one-shot runs. Every other spawn goes
// through one of them, so a test binary that links none cannot start a
// provider, and one that links any can, through a direct caller added later.
var spawnPackages = []string{
	"agent-overflow/internal/provider",
	"agent-overflow/internal/provider/claude",
	"agent-overflow/internal/provider/claudetui",
	"agent-overflow/internal/provider/codex",
	"agent-overflow/internal/textgen",
}

const selfPackage = "agent-overflow/internal/testutil/providerpath"

// listedPackage is the part of `go list -json` the guard reads.
type listedPackage struct {
	ImportPath string
	Deps       []string
	Error      *struct{ Err string }
}

func TestEveryTestBinaryThatCanStartAProviderInstallsThePoison(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate source")
	}
	// -e keeps listing past a package that does not load, such as the root
	// package before frontend/dist is built, and reports why per package.
	cmd := exec.Command("go", "list", "-e", "-test", "-json=ImportPath,Deps,Error", "./...")
	cmd.Dir = filepath.Join(filepath.Dir(source), "..", "..", "..")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	var missing, unloadable []string
	decoder := json.NewDecoder(bytes.NewReader(out))
	for decoder.More() {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		if pkg.Error != nil {
			unloadable = append(unloadable, pkg.ImportPath+": "+pkg.Error.Err)
			continue
		}
		if !strings.HasSuffix(pkg.ImportPath, ".test") {
			continue
		}
		if slices.ContainsFunc(spawnPackages, func(p string) bool { return slices.Contains(pkg.Deps, p) }) &&
			!slices.Contains(pkg.Deps, selfPackage) {
			missing = append(missing, strings.TrimSuffix(pkg.ImportPath, ".test"))
		}
	}
	if len(unloadable) > 0 {
		t.Errorf("packages go list could not load, so the guard cannot check them:\n  %s",
			strings.Join(unloadable, "\n  "))
	}
	if len(missing) > 0 {
		t.Errorf("test packages that can start a provider without providerpath in TestMain:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

func TestPoisonRecordsAndNamesTheTest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the poison is a shell script")
	}
	// Install changes both for the process; t.Setenv restores them.
	t.Setenv("PATH", os.Getenv("PATH"))
	t.Setenv("SHELL", os.Getenv("SHELL"))
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
	// The shell sync probes `$SHELL -ilc`; the stand-in answers with this
	// process's PATH, poison first, whatever a login shell would add.
	probe, err := exec.Command(os.Getenv("SHELL"), "-ilc", `printf '%s' "$PATH"`).Output()
	if err != nil {
		t.Fatalf("stand-in shell -ilc: %v", err)
	}
	if string(probe) != os.Getenv("PATH") {
		t.Fatalf("stand-in shell reported PATH %q, want the process PATH %q", probe, os.Getenv("PATH"))
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
