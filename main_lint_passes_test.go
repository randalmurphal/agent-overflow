package main

import (
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
)

// lintTarget is one system the exhaustive linter runs as, with the build tag
// sets of its passes.
type lintTarget struct {
	goos   string
	cgo    bool
	passes [][]string
	// wholeTree is false for a target whose passes name only some packages.
	wholeTree bool
}

// The exhaustive linter sees only the files a pass compiles, so a file whose
// build constraint no pass satisfies is never checked. `make
// go-lint-whole-tree` runs few passes on the strength of one fact about the
// tree: no file needs one tag on and another off. This test holds that fact.
// It fails for a file that some combination of build tags compiles on a
// linted system and that no pass compiles. The Windows passes name only the
// launcher's packages, so they count only for a file that neither Linux nor
// macOS can compile; which packages they name is the recipe's business.
func TestExhaustiveLintPassesReachEveryFile(t *testing.T) {
	targets := exhaustiveLintTargets(t)
	var wholeTree []lintTarget
	for _, target := range targets {
		if target.wholeTree {
			wholeTree = append(wholeTree, target)
		}
	}
	root := reporoot.Dir(t)
	var unreached []string
	walkErr := reporoot.Walk(t, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != root && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		expr := fileConstraint(t, path)
		if expr == nil {
			return nil
		}
		reach := targets
		if compiledBySomeBuild(expr, wholeTree) {
			reach = wholeTree
		}
		if compiledBySomeBuild(expr, reach) && !compiledBySomePass(expr, reach) {
			rel, _ := filepath.Rel(root, path)
			unreached = append(unreached, filepath.ToSlash(rel)+": "+expr.String())
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk the tree: %v", walkErr)
	}
	if len(unreached) > 0 {
		t.Fatalf("no exhaustive lint pass compiles these files; add a pass to the "+
			"go-lint-whole-tree recipe whose build tags satisfy them:\n%s",
			strings.Join(unreached, "\n"))
	}
}

// exhaustiveLintTargets reads the passes from where they are run: this
// host's and the Windows launcher's from the go-lint-whole-tree recipe, and
// the macOS job's from the CI workflow.
func exhaustiveLintTargets(t *testing.T) []lintTarget {
	t.Helper()
	recipe := makefileRecipe(t, "go-lint-whole-tree")

	var host [][]string
	hostPass := regexp.MustCompile(`(?m)^\t\$\(GO_LINT_WHOLE_TREE\) --enable-only exhaustive(,unused)?(?: --build-tags (\S+))? \./\.\.\.$`)
	for _, match := range hostPass.FindAllStringSubmatch(recipe, -1) {
		tags := splitTags(match[2])
		// unused reads one file set: the untagged pass, and only it.
		if (match[1] != "") != (len(tags) == 0) {
			t.Fatalf("go-lint-whole-tree runs unused on the untagged pass only; the pass with tags %v has unused=%v", tags, match[1] != "")
		}
		host = append(host, tags)
	}
	if !slices.ContainsFunc(host, func(tags []string) bool { return len(tags) == 0 }) {
		t.Fatalf("the go-lint-whole-tree recipe has no exhaustive pass without build tags:\n%s", recipe)
	}

	windowsLoop := regexp.MustCompile(`for tags in ([^;]+); do`).FindStringSubmatch(recipe)
	if windowsLoop == nil || !strings.Contains(recipe, "GOOS=windows CGO_ENABLED=0 $(GO_LINT_WHOLE_TREE) --enable-only exhaustive") {
		t.Fatalf("the go-lint-whole-tree recipe has no Windows launcher passes:\n%s", recipe)
	}
	var windows [][]string
	for _, tags := range strings.Fields(windowsLoop[1]) {
		windows = append(windows, splitTags(strings.Trim(tags, `"`)))
	}

	workflow, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("read the CI workflow: %v", err)
	}
	macOSJob, _, _ := strings.Cut(afterFirst(string(workflow), "\n  build-macos:\n"), "\n  go-lint:\n")
	if !strings.Contains(macOSJob, `run: make go-lint GO_LINT_FLAGS="--enable-only exhaustive"`) {
		t.Fatal("the CI workflow's macOS job no longer runs the exhaustive linter over the untagged file set")
	}

	return []lintTarget{
		{goos: "linux", cgo: true, passes: host, wholeTree: true},
		{goos: "darwin", cgo: true, passes: [][]string{nil}, wholeTree: true},
		{goos: "windows", cgo: false, passes: windows},
	}
}

func splitTags(list string) []string {
	if list == "" {
		return nil
	}
	return strings.Split(list, ",")
}

func afterFirst(text, marker string) string {
	_, rest, _ := strings.Cut(text, marker)
	return rest
}

// fileConstraint returns the file's //go:build expression joined with what
// its name implies (name_windows.go), or nil when it has neither.
func fileConstraint(t *testing.T, path string) constraint.Expr {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var expr constraint.Expr
	for _, group := range file.Comments {
		if group.Pos() > file.Package {
			break
		}
		for _, comment := range group.List {
			if !constraint.IsGoBuild(comment.Text) {
				continue
			}
			parsed, err := constraint.Parse(comment.Text)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			expr = parsed
		}
	}
	stem := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".go"), "_test")
	parts := strings.Split(stem, "_")
	for _, part := range parts[1:] {
		if !slices.Contains(lintedSystems, part) {
			continue
		}
		named := &constraint.TagExpr{Tag: part}
		if expr == nil {
			expr = named
		} else {
			expr = &constraint.AndExpr{X: expr, Y: named}
		}
	}
	return expr
}

var lintedSystems = []string{"linux", "darwin", "windows"}

// toolchainTags are set by the toolchain or the machine, never by -tags, so
// no pass chooses them: either value counts for a build and for a pass.
var toolchainTags = []string{"amd64", "arm64", "race"}

// compiledBySomeBuild reports whether some set of -tags compiles the file on
// a linted system.
func compiledBySomeBuild(expr constraint.Expr, targets []lintTarget) bool {
	chosen := buildTagsIn(expr)
	for _, target := range targets {
		for mask := 0; mask < 1<<len(chosen); mask++ {
			var tags []string
			for i, tag := range chosen {
				if mask&(1<<i) != 0 {
					tags = append(tags, tag)
				}
			}
			if compiles(expr, target, tags) {
				return true
			}
		}
	}
	return false
}

// compiledBySomePass reports whether a pass of a linted system compiles the
// file.
func compiledBySomePass(expr constraint.Expr, targets []lintTarget) bool {
	for _, target := range targets {
		for _, tags := range target.passes {
			if compiles(expr, target, tags) {
				return true
			}
		}
	}
	return false
}

// compiles evaluates the constraint for one system and one set of -tags,
// trying both values of every toolchain tag.
func compiles(expr constraint.Expr, target lintTarget, tags []string) bool {
	for mask := 0; mask < 1<<len(toolchainTags); mask++ {
		ok := expr.Eval(func(tag string) bool {
			if i := slices.Index(toolchainTags, tag); i >= 0 {
				return mask&(1<<i) != 0
			}
			switch tag {
			case target.goos:
				return true
			case "unix":
				return target.goos != "windows"
			case "cgo":
				return target.cgo
			}
			return slices.Contains(tags, tag)
		})
		if ok {
			return true
		}
	}
	return false
}

// buildTagsIn lists the tags of the expression that a -tags flag could set:
// every tag that is not a system, an architecture or the toolchain's own.
func buildTagsIn(expr constraint.Expr) []string {
	var tags []string
	var visit func(constraint.Expr)
	visit = func(node constraint.Expr) {
		switch node := node.(type) {
		case *constraint.TagExpr:
			fixed := node.Tag == "unix" || node.Tag == "cgo" ||
				slices.Contains(otherSystems, node.Tag) ||
				slices.Contains(lintedSystems, node.Tag) ||
				slices.Contains(toolchainTags, node.Tag)
			if !fixed && !slices.Contains(tags, node.Tag) {
				tags = append(tags, node.Tag)
			}
		case *constraint.NotExpr:
			visit(node.X)
		case *constraint.AndExpr:
			visit(node.X)
			visit(node.Y)
		case *constraint.OrExpr:
			visit(node.X)
			visit(node.Y)
		}
	}
	visit(expr)
	return tags
}

// otherSystems are GOOS values no pass lints as: false on every linted
// system, whatever -tags says.
var otherSystems = []string{
	"aix", "android", "dragonfly", "freebsd", "illumos", "ios", "js",
	"netbsd", "openbsd", "plan9", "solaris", "wasip1",
}
