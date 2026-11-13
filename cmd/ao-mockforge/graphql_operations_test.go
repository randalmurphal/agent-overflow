package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/harness/forgefake"
)

// Every GraphQL document the GitHub and GitLab adapters send must pass
// the forges' variable rules with the variables their callers build. The
// rig's engine validates each request (forgefake.GraphQLRequestProblems)
// and a rig test fails on one it refuses (recordRigInvocation); a run of
// the whole package then fails when an operation internal/git's source
// defines never reached a rig (checkEveryGraphQLOperationSent, from
// TestMain), so a new document needs a test here that sends it. Two
// documents under one operation name (PipelineJobs and its form without
// the cancel permissions) count as one.

// gitSourceDir is the package whose documents are checked.
const gitSourceDir = "../../internal/git"

// rigOperations are the GraphQL operations the rigs' engines received.
var rigOperations sync.Map

// recordRigInvocation records a GraphQL request's operation and fails t
// on a request the engine refused as invalid.
func recordRigInvocation(t testing.TB, inv forgefake.Invocation) {
	if inv.Operation == "" {
		return
	}
	rigOperations.Store(inv.Operation, struct{}{})
	if inv.Unhandled && strings.Contains(inv.Detail, "invalid http request") {
		t.Errorf("%s", inv.Detail)
	}
}

// graphQLOperationHeader is a document's start: every document opens
// with its kind and name in one string literal.
var graphQLOperationHeader = regexp.MustCompile(`^\s*(query|mutation|subscription)\s+([A-Za-z_][A-Za-z0-9_]*)\s*[({@]`)

// productionGraphQLOperations are the operation names dir's non-test
// source defines, read from its string literals.
func productionGraphQLOperations(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var ops []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if text, err := strconv.Unquote(lit.Value); err == nil {
				if m := graphQLOperationHeader.FindStringSubmatch(text); m != nil && !slices.Contains(ops, m[2]) {
					ops = append(ops, m[2])
				}
			}
			return true
		})
	}
	slices.Sort(ops)
	return ops, nil
}

// checkEveryGraphQLOperationSent names the operations internal/git
// defines that no rig received. It runs only when the whole package ran:
// a -run or -skip filter leaves tests out.
func checkEveryGraphQLOperationSent() error {
	for _, name := range []string{"test.run", "test.skip"} {
		if f := flag.Lookup(name); f != nil && f.Value.String() != "" {
			return nil
		}
	}
	ops, err := productionGraphQLOperations(gitSourceDir)
	if err != nil {
		return fmt.Errorf("read internal/git's GraphQL operations: %w", err)
	}
	var missing []string
	for _, op := range ops {
		if _, ok := rigOperations.Load(op); !ok {
			missing = append(missing, op)
		}
	}
	if len(ops) == 0 || len(missing) > 0 {
		return fmt.Errorf("no test here sends these GraphQL operations of internal/git through the app's code to the validating engine: %s (of %d found)", strings.Join(missing, ", "), len(ops))
	}
	return nil
}

func TestProductionGraphQLOperations(t *testing.T) {
	t.Parallel()
	ops, err := productionGraphQLOperations(gitSourceDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PRTick", "ReactionTarget", "AddReaction", "RemoveReaction", "MRNoteReactions", "PipelineJobs", "SuggestionFile", "PRManage"} {
		if !slices.Contains(ops, want) {
			t.Errorf("the source scan missed %s: %v", want, ops)
		}
	}
}
