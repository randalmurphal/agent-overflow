package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/highlight"
	"agent-overflow/internal/highlightapp"
	"agent-overflow/internal/testutil"
)

func TestHighlightWireProjectionsPreserveEveryField(t *testing.T) {
	lines := []highlight.EncodedLine{{Runs: []uint16{2, 3}}}
	result := wireHighlightResult(highlightapp.Result{Lang: "go", Lines: lines, Truncated: true, Incomplete: true, Primed: true})
	if result.Lang != "go" || len(result.Lines) != 1 || !result.Truncated || !result.Incomplete || !result.Primed {
		t.Fatalf("result = %+v", result)
	}
	seed := wireHighlightSeed(highlightapp.SeedEvent{ThreadID: "t", ItemID: "i", Lang: "go", ContentKey: "key", LineHashes: []uint32{7}, Lines: lines, Final: true})
	if seed.ThreadID != "t" || seed.ItemID != "i" || seed.Lang != "go" || seed.ContentKey != "key" || len(seed.LineHashes) != 1 || len(seed.Lines) != 1 || !seed.Final {
		t.Fatalf("seed = %+v", seed)
	}
	patches := wirePatchSpanSeeds([]highlightapp.PatchSpanSeed{{Path: "x.go", ContentKey: "patch-key", Lines: lines, Primed: true}})
	if len(patches) != 1 || patches[0].Path != "x.go" || patches[0].ContentKey != "patch-key" || len(patches[0].Lines) != 1 || !patches[0].Primed {
		t.Fatalf("patches = %+v", patches)
	}
}

const testDocstringPatch = `diff --git a/route.py b/route.py
--- a/route.py
+++ b/route.py
@@ -2,3 +2,4 @@
     """Docstring prose already open.
-    Old line.
+    New line with for and while keywords.
+    Another prose line.
     """
`

func TestHighlightCode(t *testing.T) {
	app := &App{}
	res, err := app.HighlightCode(HighlightCodeRequest{Lang: "python", Source: "def f():\n    return 1\n"})
	if err != nil {
		t.Fatalf("HighlightCode() error = %v", err)
	}
	if res.Lang != "python" {
		t.Errorf("Lang = %q, want python", res.Lang)
	}
	if len(res.Lines) != 3 {
		t.Fatalf("len(Lines) = %d, want 3", len(res.Lines))
	}
	if res.Lines[0].Runs == nil {
		t.Error("def line has no styled runs")
	}

	// Unknown language is a plain result, NOT an error — the frontend
	// caches empty-success. Absent lines render plain, so the result
	// carries no per-line allocation at all.
	plain, err := app.HighlightCode(HighlightCodeRequest{Lang: "brainfuck", Source: "+++\n"})
	if err != nil {
		t.Fatalf("unknown lang must not error, got %v", err)
	}
	if plain.Lang != "plaintext" || len(plain.Lines) != 0 {
		t.Errorf("unknown lang result = {%q, %d lines}, want plaintext with no lines", plain.Lang, len(plain.Lines))
	}
}

func TestHighlightCodeOversizedRequest(t *testing.T) {
	app := &App{}
	res, err := app.HighlightCode(HighlightCodeRequest{
		Lang:   "python",
		Source: strings.Repeat("\n", highlight.MaxRequestBytes+1),
	})
	if err != nil {
		t.Fatalf("oversized source must degrade, not error: %v", err)
	}
	if !res.Truncated || len(res.Lines) != 0 {
		t.Errorf("oversized source = {truncated %v, %d lines}, want truncated with no lines", res.Truncated, len(res.Lines))
	}
}

func TestHighlightPatchOversizedRequest(t *testing.T) {
	app := &App{}
	res, err := app.HighlightPatch(HighlightPatchRequest{
		Path:  "big.py",
		Patch: strings.Repeat("\n", highlight.MaxRequestBytes+1),
	})
	if err != nil {
		t.Fatalf("oversized patch must degrade, not error: %v", err)
	}
	if !res.Truncated || len(res.Lines) != 0 {
		t.Errorf("oversized patch = {truncated %v, %d lines}, want truncated with no lines", res.Truncated, len(res.Lines))
	}
}

func TestHighlightPatch(t *testing.T) {
	app := &App{}
	res, err := app.HighlightPatch(HighlightPatchRequest{Path: "route.py", Patch: testDocstringPatch})
	if err != nil {
		t.Fatalf("HighlightPatch() error = %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(testDocstringPatch, "\n"), "\n")
	if len(res.Lines) != len(lines) {
		t.Fatalf("len(Lines) = %d, want %d (patch-aligned)", len(res.Lines), len(lines))
	}
	// Meta lines plain.
	for i := 0; i < 4; i++ {
		if res.Lines[i].Runs != nil {
			t.Errorf("meta line %d has runs %v, want plain", i, res.Lines[i].Runs)
		}
	}
	// Unknown extension degrades plain without error.
	plain, err := app.HighlightPatch(HighlightPatchRequest{Path: "notes.unknownext", Patch: testDocstringPatch})
	if err != nil {
		t.Fatalf("unknown extension must not error, got %v", err)
	}
	if plain.Lang != "plaintext" {
		t.Errorf("Lang = %q, want plaintext", plain.Lang)
	}
}

func TestHighlightPatchWithContextWorkspaceScope(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	// A REGISTERED worktree, not the project root: the ref addresses a
	// checkout, and a worktree is one of the two things it may name.
	workspace := filepath.Join(t.TempDir(), "wt")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "feature/highlight", workspace)
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, workspace, true) })
	ref := WorkspaceRef{ProjectID: project.ID, WorkspacePath: workspace}

	fileContent := "def handler(request):\n" +
		"    \"\"\"Docstring prose already open.\n" +
		"    New line with for and while keywords.\n" +
		"    Another prose line.\n" +
		"    \"\"\"\n"
	if err := os.WriteFile(filepath.Join(workspace, "route.py"), []byte(fileContent), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// Membership resolves from git's on-disk layout, so no git binary is
	// needed to answer it.
	t.Setenv("PATH", "")
	res, err := app.HighlightPatchWithContext(ref, HighlightPatchContextRequest{
		Scope: "workspace",
		Path:  "route.py",
		Patch: testDocstringPatch,
	})
	if err != nil {
		t.Fatalf("HighlightPatchWithContext() error = %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(testDocstringPatch, "\n"), "\n")
	if len(res.Lines) != len(lines) {
		t.Fatalf("len(Lines) = %d, want %d", len(res.Lines), len(lines))
	}
	// The hunk starts mid-docstring: only the primed path can know the
	// added line is string content, not code with keywords.
	addedLine := res.Lines[6]
	body := lines[6][1:]
	classes := make(map[uint16]bool)
	pos := 0
	for i := 0; i+1 < len(addedLine.Runs); i += 2 {
		classes[addedLine.Runs[i+1]] = true
		pos += int(addedLine.Runs[i])
	}
	if addedLine.Runs == nil {
		t.Fatal("primed added line has no runs")
	}
	if pos != len(body) {
		t.Fatalf("runs cover %d bytes, body has %d", pos, len(body))
	}
	if classes[1] { // ClassKeyword
		t.Error("primed mid-docstring line has keyword runs — priming did not take effect")
	}
}

func TestHighlightPatchWithContextFallsBackWithoutContent(t *testing.T) {
	// A resolvable checkout with no such file: content resolution fails and
	// the RPC degrades to the unprimed result instead of erroring.
	app := newTestAppWithStore(t)
	ref := testWorkspaceRef(t, app, t.TempDir())
	res, err := app.HighlightPatchWithContext(ref, HighlightPatchContextRequest{
		Scope: "workspace",
		Path:  "route.py",
		Patch: testDocstringPatch,
	})
	if err != nil {
		t.Fatalf("fallback must not error, got %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(testDocstringPatch, "\n"), "\n")
	if len(res.Lines) != len(lines) {
		t.Fatalf("len(Lines) = %d, want %d", len(res.Lines), len(lines))
	}
}

// Priming is best-effort; naming the checkout is not. A ref that resolves to
// nothing — or to a directory that is not a checkout of the project — is a
// caller error and comes back as one, never as silently unprimed spans.
func TestHighlightPatchWithContextRefusesUnresolvableRefs(t *testing.T) {
	app := newTestAppWithStore(t)
	project := testWorkspaceRef(t, app, testutil.InitGitRepo(t))

	for name, ref := range map[string]WorkspaceRef{
		"zero ref":        {},
		"unknown project": {ProjectID: "nope"},
		"non-checkout directory": {
			ProjectID: project.ProjectID, WorkspacePath: t.TempDir(),
		},
	} {
		if _, err := app.HighlightPatchWithContext(ref, HighlightPatchContextRequest{
			Scope: "workspace", Path: "route.py", Patch: testDocstringPatch,
		}); err == nil {
			t.Fatalf("%s: HighlightPatchWithContext accepted an unresolvable ref", name)
		}
	}
}

// The two entry points are each other's tripwire: neither serves the other's
// subject, so a caller cannot reach a thread's history through the checkout
// RPC or a checkout through the thread RPC.
func TestHighlightPatchWithContextScopesAreExclusive(t *testing.T) {
	app := newTestAppWithStore(t)
	ref := testWorkspaceRef(t, app, t.TempDir())
	thread := testThread("thread-highlight-scope-split")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if _, err := app.HighlightPatchWithContext(ref, HighlightPatchContextRequest{
		Scope: "edits", Path: "route.py", Patch: testDocstringPatch,
	}); err == nil {
		t.Fatal("HighlightPatchWithContext accepted the edits scope")
	}
	if _, err := app.HighlightEditPatchWithContext(thread.ID, HighlightPatchContextRequest{
		Scope: "workspace", Path: "route.py", Patch: testDocstringPatch,
	}); err == nil {
		t.Fatal("HighlightEditPatchWithContext accepted a live scope")
	}
}

func TestHighlightClassNamesShape(t *testing.T) {
	app := &App{}
	names := app.HighlightClassNames()
	if len(names) == 0 || names[0] != "none" {
		t.Fatalf("class names = %v, want index 0 = none", names)
	}
}

func TestHighlightGuardsShutdown(t *testing.T) {
	app := &App{}
	app.shuttingDown.Store(true)
	if _, err := app.HighlightCode(HighlightCodeRequest{Lang: "go", Source: "x"}); err == nil {
		t.Error("HighlightCode should refuse during shutdown")
	}
	if _, err := app.HighlightPatch(HighlightPatchRequest{Path: "a.go", Patch: ""}); err == nil {
		t.Error("HighlightPatch should refuse during shutdown")
	}
	if _, err := app.HighlightPatchWithContext(WorkspaceRef{}, HighlightPatchContextRequest{}); err == nil {
		t.Error("HighlightPatchWithContext should refuse during shutdown")
	}
	if _, err := app.HighlightEditPatchWithContext("t", HighlightPatchContextRequest{}); err == nil {
		t.Error("HighlightEditPatchWithContext should refuse during shutdown")
	}
}

func TestHighlightEditPatchWithContextEditsScope(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()
	thread := testThread("thread-highlight-edits")
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	// The post-edit file state: matches testDocstringPatch's new side.
	fileContent := "def handler(request):\n" +
		"    \"\"\"Docstring prose already open.\n" +
		"    New line with for and while keywords.\n" +
		"    Another prose line.\n" +
		"    \"\"\"\n"
	if err := os.WriteFile(filepath.Join(workspace, "route.py"), []byte(fileContent), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	res, err := app.HighlightEditPatchWithContext(thread.ID, HighlightPatchContextRequest{
		Scope: "edits",
		Path:  "route.py",
		Patch: testDocstringPatch,
	})
	if err != nil {
		t.Fatalf("HighlightEditPatchWithContext(edits) error = %v", err)
	}
	if !res.Primed {
		t.Fatal("matching workspace must produce a primed result")
	}

	// Drift the docstring region: verification fails and the result
	// degrades to unprimed spans (never wrong colors), still no error.
	if err := os.WriteFile(filepath.Join(workspace, "route.py"), []byte("completely different\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(drift) error = %v", err)
	}
	drifted, err := app.HighlightEditPatchWithContext(thread.ID, HighlightPatchContextRequest{
		Scope: "edits",
		Path:  "route.py",
		Patch: testDocstringPatch,
	})
	if err != nil {
		t.Fatalf("HighlightEditPatchWithContext(drifted) error = %v", err)
	}
	if drifted.Primed {
		t.Fatal("drifted workspace must not produce a primed result")
	}
}
