package gitenv

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// lastValue is the value exec.Cmd hands the child for key: the last entry
// wins.
func lastValue(env []string, key string) (string, bool) {
	value, found := "", false
	for _, entry := range env {
		if name, v, ok := strings.Cut(entry, "="); ok && name == key {
			value, found = v, true
		}
	}
	return value, found
}

func TestNonInteractiveOverridesTheInheritedPromptConfiguration(t *testing.T) {
	env := NonInteractive([]string{
		"GIT_TERMINAL_PROMPT=1",
		"GIT_ASKPASS=/usr/lib/ssh/user-askpass",
		"SSH_ASKPASS=/usr/lib/ssh/user-askpass",
		"SSH_ASKPASS_REQUIRE=force",
		"GCM_INTERACTIVE=auto",
		"WSLENV=FOO/p:TERM",
	})
	for key, want := range map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "",
		"SSH_ASKPASS":         "",
		"SSH_ASKPASS_REQUIRE": "never",
		"GCM_INTERACTIVE":     "never",
		"WSLENV":              "FOO/p:TERM:GIT_TERMINAL_PROMPT:SSH_ASKPASS_REQUIRE:GCM_INTERACTIVE",
	} {
		if got, ok := lastValue(env, key); !ok || got != want {
			t.Errorf("%s = %q (set %v), want %q", key, got, ok, want)
		}
	}
}

func TestNonInteractiveCreatesWSLENV(t *testing.T) {
	env := NonInteractive([]string{"PATH=/usr/bin"})
	if got, _ := lastValue(env, "WSLENV"); got != "GIT_TERMINAL_PROMPT:SSH_ASKPASS_REQUIRE:GCM_INTERACTIVE" {
		t.Fatalf("WSLENV = %q", got)
	}
}

func TestNonInteractiveTwiceListsEachNameOnce(t *testing.T) {
	once := NonInteractive([]string{"WSLENV=TERM"})
	twice := NonInteractive(once)
	a, _ := lastValue(once, "WSLENV")
	b, _ := lastValue(twice, "WSLENV")
	if a != b {
		t.Fatalf("WSLENV grew on a second pass: %q then %q", a, b)
	}
}

func TestNonInteractiveLeavesTheCallersSliceAlone(t *testing.T) {
	backing := make([]string, 1, 16)
	backing[0] = "PATH=/usr/bin"
	NonInteractive(backing)
	if spare := backing[:2][1]; spare != "" {
		t.Fatalf("wrote %q into the caller's spare capacity", spare)
	}
}

// gitSpawnExempt are the directories whose git children are not the app's
// own: the harness and the test fixtures build and read local repositories.
var gitSpawnExempt = []string{"internal/harness/", "internal/testutil/"}

// TestEveryGitSpawnIsNonInteractive is a tripwire, because nothing in the
// type system stops a new exec.Command("git", ...): every package that
// names git in an os/exec call must build that child's environment here.
// internal/git resolves its binary through a policy rather than a literal,
// so its runner is held by its own tests instead.
func TestEveryGitSpawnIsNonInteractive(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information")
	}
	root := filepath.Join(filepath.Dir(self), "..", "..")

	spawns := map[string][]string{} // package dir -> files spawning git
	imports := map[string]bool{}    // package dir -> imports gitenv
	fset := token.NewFileSet()
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if slices.ContainsFunc(gitSpawnExempt, func(prefix string) bool { return strings.HasPrefix(rel, prefix) }) {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			dir := filepath.ToSlash(filepath.Dir(rel))
			execName := ""
			for _, spec := range file.Imports {
				switch importPath, _ := strconv.Unquote(spec.Path.Value); importPath {
				case "agent-overflow/internal/gitenv":
					imports[dir] = true
				case "os/exec":
					execName = "exec"
					if spec.Name != nil {
						execName = spec.Name.Name
					}
				}
			}
			if execName == "" {
				return nil
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext") {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != execName {
					return true
				}
				for _, arg := range call.Args {
					if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING && lit.Value == `"git"` {
						spawns[dir] = append(spawns[dir], rel)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// A scan that matches nothing would read as coverage.
	for _, known := range []string{"internal/gitdiff", "internal/workspacefiles"} {
		if len(spawns[known]) == 0 {
			t.Errorf("the scan no longer sees %s spawn git; fix the matcher", known)
		}
	}
	for dir, files := range spawns {
		if !imports[dir] {
			t.Errorf("%s starts git in %v without gitenv.NonInteractive: a remote that needs credentials would open a sign-in prompt", dir, files)
		}
	}
}
