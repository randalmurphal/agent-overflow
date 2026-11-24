package treecheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
)

// goFileCheck is a check over every Go file the build compiles. visit sees
// each file once, as reporoot.GoFiles lists them, with the file set its
// positions are in; report runs once every file has been seen.
type goFileCheck struct {
	name   string
	visit  func(fset *token.FileSet, path string, file *ast.File)
	report func(t *testing.T)
}

// TestEveryGoFile lists and parses the Go tree once and hands each file to
// every check, which reports as a subtest of its own. A check that reads
// the whole tree joins this list rather than walking it again.
func TestEveryGoFile(t *testing.T) {
	t.Parallel()
	checks := []goFileCheck{providerQueueCheck(), wirePayloadCheck(), emitSiteCheck(), windowBoundaryCheck()}
	fset := token.NewFileSet()
	for _, path := range reporoot.GoFiles(t) {
		file, err := reporoot.ParseGoFile(t, fset, path, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, check := range checks {
			check.visit(fset, path, file)
		}
	}
	for _, check := range checks {
		t.Run(check.name, check.report)
	}
}
