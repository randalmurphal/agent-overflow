package providerpath

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/testutil/reporoot"
)

// TestMain guards this package's tests as it guards every other's, so a
// process they leave fails them too.
func TestMain(m *testing.M) { os.Exit(Run(m)) }

// spawnPackages hold the exec sites that run a provider CLI: provider
// (DetectProvider's version probe, Spawn), the Claude, Claude TUI and Codex
// session and probe code, and textgen's one-shot runs. Every other spawn goes
// through one of them, so a test binary that links none cannot start a
// provider, and one that links any can, through a direct caller added later.
var spawnPackages = []string{
	"agent-overflow/internal/provider",
	"agent-overflow/internal/provider/acp",
	"agent-overflow/internal/provider/agy",
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
	reporoot.ObserveGoSources(t)
	// -e keeps listing past a package that does not load, such as the root
	// package before frontend/dist is built, and reports why per package.
	cmd := exec.Command("go", "list", "-e", "-test", "-json=ImportPath,Deps,Error", "./...")
	cmd.Dir = reporoot.Dir(t)
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
		// A dependency that imports the package under test is rebuilt for
		// the test binary and listed as "<path> [<pkg>.test]"; it links the
		// same exec sites as the plain one.
		deps := make([]string, len(pkg.Deps))
		for i, dep := range pkg.Deps {
			deps[i], _, _ = strings.Cut(dep, " [")
		}
		if slices.ContainsFunc(spawnPackages, func(p string) bool { return slices.Contains(deps, p) }) &&
			!slices.Contains(deps, selfPackage) {
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

// probeInstalledCodex stands in for production code that resolves the bare
// provider name and asks it for its version.
func probeInstalledCodex(dir string) error {
	cmd := exec.Command("codex", "--version")
	cmd.Dir = dir
	_, err := cmd.Output()
	return err
}

// startBackgroundProbe stands in for a service that probes from a goroutine
// of its own, which the goroutine's creation site names.
func startBackgroundProbe(dir string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- probeInstalledCodex(dir) }()
	return done
}

func TestPoisonReportNamesTheCallerFromItsStack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the poison is a shell script")
	}
	t.Setenv("PATH", os.Getenv("PATH"))
	t.Setenv("SHELL", os.Getenv("SHELL"))
	check, err := Install()
	if err != nil {
		t.Fatal(err)
	}
	// A directory whose name carries no test name, so only the stacks can
	// name the caller.
	dir, err := os.MkdirTemp("", "providerpath-caller-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove probe directory: %v", err)
		}
	}()
	if err := <-startBackgroundProbe(dir); err == nil {
		t.Fatal("poisoned codex exited 0")
	}
	err = check()
	if err == nil {
		t.Fatal("check reported no attempts")
	}
	report := err.Error()
	for _, want := range []string{
		"the record names no test; see the stacks",
		"codex --version",
		"tests running: TestPoisonReportNamesTheCallerFromItsStack",
		"goroutines waiting on a child process, one of which ran it (1):",
		"providerpath.probeInstalledCodex(",
		"created by agent-overflow/internal/testutil/providerpath.startBackgroundProbe in goroutine",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report does not contain %q:\n%s", want, report)
		}
	}
	// The creating goroutine is this test's, still running.
	if !regexp.MustCompile(`(?m)^    goroutine \d+ runs TestPoisonReportNamesTheCallerFromItsStack$`).MatchString(report) {
		t.Fatalf("report does not name the test that created the waiting goroutine:\n%s", report)
	}
}

func TestPoisonReportSaysWhenAChildProcessRanIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the poison is a shell script")
	}
	t.Setenv("PATH", os.Getenv("PATH"))
	t.Setenv("SHELL", os.Getenv("SHELL"))
	check, err := Install()
	if err != nil {
		t.Fatal(err)
	}
	// The trailing command keeps the shell from exec-ing codex in place.
	if err := exec.Command("/bin/sh", "-c", "codex --version; exit $?").Run(); err == nil {
		t.Fatal("shell running poisoned codex exited 0")
	}
	err = check()
	if err == nil {
		t.Fatal("check reported no attempts")
	}
	want := fmt.Sprintf("is not this test process (pid %d): a child process of a test ran it", os.Getpid())
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("report does not contain %q:\n%v", want, err)
	}
}

func TestPoisonStopsHoldingItsCallerWhenTheRunEnds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the poison is a shell script")
	}
	// No watcher acks this poison; removing its directory, as check does,
	// must release it rather than leave it waiting out its bound.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ackDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	poison := filepath.Join(dir, "codex")
	mockexec.WriteFile(t, poison, []byte(poisonScript(dir)), 0o755)
	cmd := exec.Command(poison, "--version")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		t.Fatalf("poison exited before its ack or the end of the run: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatalf("poison kept waiting after its directory was removed (kill: %v)", cmd.Process.Kill())
	}
}
