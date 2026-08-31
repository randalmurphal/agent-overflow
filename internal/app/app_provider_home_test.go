package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/provider/claude/sessionfork"
)

// testProviderProjectsDir is what a test passes where production passes
// App.claudeProjectsDir(). Every App fixture detaches HOME
// (kerneltest.DetachHome), so this resolves to the fixture's own empty temp
// home and never to the developer's real `~/.claude/projects`.
func testProviderProjectsDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve test home: %v", err)
	}
	return sessionfork.ProjectsDirForHome(home)
}

// TestProviderHomeSeamPinsEveryProviderPathUnderTheOverride is the BEHAVIOR
// half of the rule the source scan below enforces structurally.
//
// It stands in for a keep-home boot: `$HOME` is a real, populated directory
// that is NOT the harness's, exactly as `AO_HARNESS_KEEP_HOME=1` leaves it,
// while `credentialHomeOverride` holds `<dataRoot>/home`. Every provider path
// the backend builds must come out under the override — the credential home,
// the Claude projects tree a fork/relocate/leaf-scan reads and WRITES, the
// `~/.claude.json` the MCP adapter rewrites, and the `~/.codex/config.toml`
// its Codex twin rewrites. Before the seam, the last three resolved through
// `$HOME` and an isolated boot mutated the developer's real provider state.
func TestProviderHomeSeamPinsEveryProviderPathUnderTheOverride(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	t.Setenv("USERPROFILE", realHome)

	pinned := t.TempDir()
	app := &App{credentialHomeOverride: pinned}

	home, err := app.providerHome()
	if err != nil {
		t.Fatalf("providerHome: %v", err)
	}
	if home != pinned {
		t.Fatalf("providerHome = %q, want the override %q", home, pinned)
	}

	claudeJSON, err := app.claudeConfigJSONPath()
	if err != nil {
		t.Fatalf("claudeConfigJSONPath: %v", err)
	}
	projects, err := app.claudeProjectsDir()
	if err != nil {
		t.Fatalf("claudeProjectsDir: %v", err)
	}
	codexTOML, err := app.codexConfigTOMLPath()
	if err != nil {
		t.Fatalf("codexConfigTOMLPath: %v", err)
	}

	for _, tc := range []struct{ name, got string }{
		{"claude config json", claudeJSON},
		{"claude projects dir", projects},
		{"codex config toml", codexTOML},
	} {
		if !strings.HasPrefix(tc.got, pinned+string(os.PathSeparator)) {
			t.Errorf("%s = %q, want it under the pinned home %q", tc.name, tc.got, pinned)
		}
		if strings.HasPrefix(tc.got, realHome+string(os.PathSeparator)) {
			t.Errorf("%s = %q resolved through the real $HOME %q", tc.name, tc.got, realHome)
		}
	}
}

// TestProviderHomeFallsBackToTheOSHomeWithoutAnOverride pins the other
// direction: a normal desktop boot sets no override and must still resolve the
// user's own provider home. The seam is an INDIRECTION, not a redirect.
func TestProviderHomeFallsBackToTheOSHomeWithoutAnOverride(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	t.Setenv("USERPROFILE", realHome)

	want, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve os home: %v", err)
	}
	got, err := (&App{}).providerHome()
	if err != nil {
		t.Fatalf("providerHome: %v", err)
	}
	if got != want {
		t.Fatalf("providerHome = %q, want the OS home %q", got, want)
	}
}

// providerHomeSeamScanRoots are the trees whose non-test Go files may not
// resolve a provider home for themselves. They are the three places a
// `~/.claude` or `~/.codex` path is actually built: the App layer, the
// provider packages, and the two shared-config adapters that WRITE into a
// provider home.
var providerHomeSeamScanRoots = []string{
	".", // app_*.go only — see scanFilesUnder
	"internal/provider",
	"internal/claudeconfig",
	"internal/codexconfig",
}

// providerHomeSeamAllowlist names the files whose os.UserHomeDir() call was
// individually verified NOT to be a provider-home resolution. Each entry
// carries the reason, because an unexplained entry is how an allowlist stops
// being a review.
//
// Adding a file here is a claim that the call resolves something else
// entirely. If it resolves `<home>/.claude` or `<home>/.codex`, it belongs on
// App.providerHome() instead (app_provider_home.go).
var providerHomeSeamAllowlist = map[string]string{
	"app_startup.go": "the config-dir fallback (~/ as the data root when os.UserConfigDir fails), " +
		"not a provider home — the provider credential home two blocks below it goes through providerHome()",
	"app_threads.go": "the cwd a terminal thread opens in when it has no project, " +
		"a working directory rather than a provider path",
	"app_provider_account_env.go": "providerProbeWorkDir: the project-free cwd every account probe is " +
		"pinned to. A cwd, not a provider home — the probe's own home comes from its ProbeConfig",
	"app_provider_home.go": "the seam itself",
}

// TestAppLayerResolvesProviderHomesThroughOneSeam fails on a bare
// os.UserHomeDir() (or os.Getenv("HOME") / os.Getenv("USERPROFILE")) in the
// code that builds provider paths.
//
// The rule exists because AO_HARNESS_KEEP_HOME is documented as READ-ONLY
// WIDENING: an isolated boot keeps the real `$HOME` so provider CHILD
// processes see it, while the backend's own resolution stays pinned under
// `<dataRoot>/home`. Before this test, exactly two call sites honoured the
// pin and the rest read `$HOME` — which put the MCP config writer, the
// Claude memory-directory mkdir, the session-fork writer and an
// authenticated rate-limit probe on the developer's real provider home.
//
// A found call is not automatically a bug; it is automatically a REVIEW. Move
// it onto App.providerHome() (or inject the home, for a package under
// internal/), or add the file to providerHomeSeamAllowlist with the reason.
//
// The sibling enforcement is TestMockedBootModesShareOneIsolationHelper
// (main_soak_test.go), which pins the isolation itself; this one pins who is
// allowed to look past it.
func TestAppLayerResolvesProviderHomesThroughOneSeam(t *testing.T) {
	var offenders []string
	for _, root := range providerHomeSeamScanRoots {
		for _, path := range scanFilesUnder(t, root) {
			base := filepath.Base(path)
			if _, ok := providerHomeSeamAllowlist[base]; ok {
				continue
			}
			for _, call := range homeResolvingCalls(t, path) {
				offenders = append(offenders, path+": "+call)
			}
		}
	}
	if len(offenders) > 0 {
		t.Errorf(
			"these files resolve a home directory for themselves instead of going through the one "+
				"provider-home seam (App.providerHome / an injected home parameter):\n  %s\n\n"+
				"AO_HARNESS_KEEP_HOME leaves the real $HOME in place for provider CHILD processes; the "+
				"backend's own `~/.claude` / `~/.codex` resolution must stay pinned under the isolated "+
				"data root, or an isolated boot writes into the developer's real provider state. "+
				"Route it through App.providerHome(), inject the home (internal/AGENTS.md: \"Provider "+
				"homes are injected, never resolved here\"), or add the file to "+
				"providerHomeSeamAllowlist with the reason it is not a provider path.",
			strings.Join(offenders, "\n  "),
		)
	}
}

// scanFilesUnder lists the non-test .go files this rule covers. The repo root
// is narrowed to `app_*.go` on purpose: main*.go owns boot-time data-root and
// launcher-default paths that are legitimately home-relative and have nothing
// to do with provider state.
func scanFilesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	if root == "." {
		matches, err := filepath.Glob("app_*.go")
		if err != nil {
			t.Fatalf("glob app_*.go: %v", err)
		}
		for _, m := range matches {
			if !strings.HasSuffix(m, "_test.go") {
				out = append(out, m)
			}
		}
		return out
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// homeResolvingCalls returns a description of every home-resolving call in
// one file. Comments are deliberately NOT scanned — several of these files
// explain the seam in prose and must be able to name the function.
func homeResolvingCalls(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "os" {
			return true
		}
		switch sel.Sel.Name {
		case "UserHomeDir":
			found = append(found, "os.UserHomeDir() at line "+lineOf(fset, call.Pos()))
		case "Getenv":
			if len(call.Args) != 1 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			switch lit.Value {
			case `"HOME"`, `"USERPROFILE"`:
				found = append(found, "os.Getenv("+lit.Value+") at line "+lineOf(fset, call.Pos()))
			}
		}
		return true
	})
	return found
}

func lineOf(fset *token.FileSet, pos token.Pos) string {
	return strconv.Itoa(fset.Position(pos).Line)
}
