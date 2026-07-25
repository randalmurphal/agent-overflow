package editor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBuildArgs_DirectPath(t *testing.T) {
	opts := SpawnOptions{
		Editor: &Editor{LaunchStyle: LaunchStyleDirectPath},
		Path:   "/tmp/foo.go",
		Line:   10,
		Column: 20,
	}
	got := buildArgs(opts)
	want := []string{"/tmp/foo.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs direct-path:\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildArgs_GotoWithLineAndColumn(t *testing.T) {
	opts := SpawnOptions{
		Editor: &Editor{LaunchStyle: LaunchStyleGoto},
		Path:   "/tmp/foo.go",
		Line:   42,
		Column: 7,
	}
	got := buildArgs(opts)
	want := []string{"--goto", "/tmp/foo.go:42:7"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs goto line+col:\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildArgs_GotoWithLineDefaultsColumnToOne(t *testing.T) {
	opts := SpawnOptions{
		Editor: &Editor{LaunchStyle: LaunchStyleGoto},
		Path:   "/tmp/foo.go",
		Line:   42,
	}
	got := buildArgs(opts)
	want := []string{"--goto", "/tmp/foo.go:42:1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs goto line-only:\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildArgs_GotoWithoutLineDropsFlag(t *testing.T) {
	opts := SpawnOptions{
		Editor: &Editor{LaunchStyle: LaunchStyleGoto},
		Path:   "/tmp/foo.go",
	}
	got := buildArgs(opts)
	want := []string{"/tmp/foo.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs goto no-line:\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildArgs_PathLineColumn(t *testing.T) {
	opts := SpawnOptions{
		Editor: &Editor{LaunchStyle: LaunchStylePathLineColumn},
		Path:   "/tmp/foo.go",
		Line:   12,
		Column: 3,
	}
	got := buildArgs(opts)
	// Sublime and Zed take the position as a `:line:column` suffix on
	// the path — they have no --line/--column flags, so passing flags
	// here opened bogus paths (or errored) instead of placing the
	// cursor. See LaunchStylePathLineColumn's doc for the upstream
	// references.
	want := []string{"/tmp/foo.go:12:3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs path-line-column:\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildArgs_PathLineColumnLineOnly(t *testing.T) {
	opts := SpawnOptions{
		Editor: &Editor{LaunchStyle: LaunchStylePathLineColumn},
		Path:   "/tmp/foo.go",
		Line:   12,
	}
	got := buildArgs(opts)
	want := []string{"/tmp/foo.go:12"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs path-line-column line-only:\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildArgs_PathLineColumnColumnWithoutLine(t *testing.T) {
	opts := SpawnOptions{
		Editor: &Editor{LaunchStyle: LaunchStylePathLineColumn},
		Path:   "/tmp/foo.go",
		Column: 3,
	}
	got := buildArgs(opts)
	// A column with no line is meaningless (`path::3` is not a valid
	// suffix for either editor) — drop the position entirely.
	want := []string{"/tmp/foo.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs path-line-column column-only:\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildArgs_PathLineColumnNoCursor(t *testing.T) {
	opts := SpawnOptions{
		Editor: &Editor{LaunchStyle: LaunchStylePathLineColumn},
		Path:   "/tmp/foo.go",
	}
	got := buildArgs(opts)
	want := []string{"/tmp/foo.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs path-line-column no cursor:\n got: %v\nwant: %v", got, want)
	}
}

// Pins each catalog editor to the argv its real CLI accepts. The style
// enum tests above only prove buildArgs emits each style's shape — this
// table is what catches an editor pointed at the WRONG style (the bug
// that shipped Sublime/Zed with --line/--column flags neither supports).
func TestBuildArgs_PerCatalogEditor(t *testing.T) {
	want := map[string][]string{
		// VS Code family: `--goto path:line:col`
		// (code.visualstudio.com/docs/editor/command-line).
		"code":          {"--goto", "/tmp/foo.go:12:3"},
		"code-insiders": {"--goto", "/tmp/foo.go:12:3"},
		"cursor":        {"--goto", "/tmp/foo.go:12:3"},
		"windsurf":      {"--goto", "/tmp/foo.go:12:3"},
		"codium":        {"--goto", "/tmp/foo.go:12:3"},
		// Positional suffix form, no flags: Sublime
		// (sublimetext.com/docs/command_line.html) and Zed
		// (zed crates/cli "Use `path:line:column` syntax").
		"subl": {"/tmp/foo.go:12:3"},
		"zed":  {"/tmp/foo.go:12:3"},
	}
	for _, ed := range EditorCatalog() {
		expected, ok := want[ed.ID]
		if !ok {
			t.Errorf("catalog editor %q has no expected argv in this table — add it with a reference to its CLI docs", ed.ID)
			continue
		}
		got := buildArgs(SpawnOptions{Editor: &ed, Path: "/tmp/foo.go", Line: 12, Column: 3})
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("buildArgs for %s:\n got: %v\nwant: %v", ed.ID, got, expected)
		}
	}
}

func TestOpen_RejectsNilEditor(t *testing.T) {
	if err := Open(context.Background(), SpawnOptions{Path: "/tmp/x"}); !errors.Is(err, ErrNoEditor) {
		t.Fatalf("expected ErrNoEditor; got %v", err)
	}
}

func TestOpen_RejectsUnavailableEditor(t *testing.T) {
	ed := &Editor{ID: "code", Available: false}
	if err := Open(context.Background(), SpawnOptions{Editor: ed, Path: "/tmp/x"}); !errors.Is(err, ErrNoEditor) {
		t.Fatalf("expected ErrNoEditor for unavailable editor; got %v", err)
	}
}

func TestOpen_RejectsEmptyPath(t *testing.T) {
	ed := &Editor{ID: "code", Available: true, ResolvedPath: "/usr/bin/code"}
	if err := Open(context.Background(), SpawnOptions{Editor: ed}); err == nil {
		t.Fatal("expected error for empty path")
	}
}

// TestOpen_RejectsRelativePathWithoutWorkspace pins the LAN-bind safety
// floor: a remote token-holder asking the host's editor to open
// `./foo.ts` without a workspace anchor gets a clear error rather than
// a quiet open against the server's cwd. The check fires before
// resolveSpawnBinary, so the test doesn't need to stub lookPath.
func TestOpen_RejectsRelativePathWithoutWorkspace(t *testing.T) {
	ed := &Editor{
		ID: "code", Name: "VS Code",
		Available: true, ResolvedPath: "/usr/bin/code",
		LaunchStyle: LaunchStyleGoto,
	}
	err := Open(context.Background(), SpawnOptions{Editor: ed, Path: "./foo.ts"})
	if err == nil {
		t.Fatal("expected error for relative path without workspace")
	}
	if !strings.Contains(err.Error(), "workspacePath") {
		t.Fatalf("expected error to mention 'workspacePath', got %q", err.Error())
	}
}

// TestOpen_ResolvesRelativeAgainstWorkspace covers the diff-card path:
// the click site emits a repo-relative path (e.g. "internal/uikeys/keys.go")
// and the active thread's workspacePath. The backend joins them and
// spawns the editor against the absolute result. Without this, every
// editor-link in a Diff card silently failed.
func TestOpen_ResolvesRelativeAgainstWorkspace(t *testing.T) {
	originalLookPath := lookPath
	originalStart := startCmd
	originalObserve := observeFastExit
	t.Cleanup(func() {
		lookPath = originalLookPath
		startCmd = originalStart
		observeFastExit = originalObserve
	})

	lookPath = func(string) (string, error) { return "/usr/bin/code", nil }
	var captured *exec.Cmd
	startCmd = func(cmd *exec.Cmd) error {
		captured = cmd
		return nil
	}
	observeFastExit = func(*exec.Cmd, string) error { return nil }

	ed := &Editor{
		ID: "code", Name: "VS Code", Command: "code",
		Available: true, ResolvedPath: "/usr/bin/code",
		LaunchStyle: LaunchStyleGoto,
	}
	err := Open(context.Background(), SpawnOptions{
		Editor:        ed,
		Path:          "internal/uikeys/keys.go",
		Line:          12,
		WorkspacePath: "/home/user/repo",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if captured == nil {
		t.Fatal("startCmd not invoked")
	}
	wantArg := "/home/user/repo/internal/uikeys/keys.go:12:1"
	found := false
	for _, a := range captured.Args {
		if a == wantArg {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("editor argv missing resolved path %q; got %v", wantArg, captured.Args)
	}
}

// TestOpen_RejectsWorkspaceEscape pins the traversal-escape guard: a
// caller that supplies a workspacePath plus a `..`-laden relative path
// must not be able to navigate outside the workspace. Without this,
// "../../../etc/passwd" + "/home/user/repo" would Join to "/etc/passwd"
// cleanly and pass the canonical check.
func TestOpen_RejectsWorkspaceEscape(t *testing.T) {
	ed := &Editor{
		ID: "code", Name: "VS Code",
		Available: true, ResolvedPath: "/usr/bin/code",
		LaunchStyle: LaunchStyleGoto,
	}
	err := Open(context.Background(), SpawnOptions{
		Editor:        ed,
		Path:          "../../../etc/passwd",
		WorkspacePath: "/home/user/repo",
	})
	if err == nil {
		t.Fatal("expected error for path that escapes workspace")
	}
	if !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("expected error to mention 'escapes workspace', got %q", err.Error())
	}
}

// TestDefaultObserveFastExit_NonZeroExitWithinWindow exercises the
// real (non-stubbed) watcher against a fast-failing binary. `false`
// exits 1 inside fastExitWindow → defaultObserveFastExit must surface
// the exit code as an error so the frontend can toast it.
func TestDefaultObserveFastExit_NonZeroExitWithinWindow(t *testing.T) {
	cmd := exec.Command("false")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start false: %v", err)
	}
	err := defaultObserveFastExit(cmd, "VS Code")
	if err == nil {
		t.Fatal("expected error from non-zero fast exit")
	}
	if !strings.Contains(err.Error(), "exited with code 1") {
		t.Fatalf("expected exit code in error; got %q", err.Error())
	}
}

// TestDefaultObserveFastExit_ZeroExitWithinWindow covers the VS Code
// CLI handoff case: the binary returns 0 immediately after sending the
// open IPC to a running window. Treat as success — no error, no
// noise.
func TestDefaultObserveFastExit_ZeroExitWithinWindow(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start true: %v", err)
	}
	if err := defaultObserveFastExit(cmd, "VS Code"); err != nil {
		t.Fatalf("zero-exit must return nil; got %v", err)
	}
}

// TestDefaultObserveFastExit_StillRunningAtWindow covers the
// long-lived editor case: the window expires before the child has
// exited. We treat that as a successful launch and let the goroutine
// continue waiting for the eventual exit (the buffered channel keeps
// the goroutine non-leaky).
func TestDefaultObserveFastExit_StillRunningAtWindow(t *testing.T) {
	// sleep 5s — way past the 750ms window.
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start sleep: %v", err)
	}
	t.Cleanup(func() {
		// Kill only — no cmd.Wait(). The watcher's abandoned goroutine is
		// still blocked in its own cmd.Wait() (that is the production
		// contract: it reaps the long-lived child), and exec.Cmd.Wait must
		// not be called twice concurrently — the race detector flags the
		// internal ProcessState writes. Kill is documented safe alongside
		// a concurrent Wait; the goroutine reaps the killed child.
		_ = cmd.Process.Kill()
	})
	start := time.Now()
	err := defaultObserveFastExit(cmd, "VS Code")
	if err != nil {
		t.Fatalf("long-lived child must produce nil; got %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < fastExitWindow {
		t.Fatalf("watcher returned in %v, expected at least %v", elapsed, fastExitWindow)
	}
	// Generous upper bound — the watcher should return promptly after
	// the window closes, not wait for the process to finish.
	if elapsed > fastExitWindow+250*time.Millisecond {
		t.Fatalf("watcher took %v, expected close to %v", elapsed, fastExitWindow)
	}
}

// TestOpen_FastExitFailureSurfacesEditorName pins the defense-in-depth
// layer: when startCmd succeeds but the spawned editor exits non-zero
// inside fastExitWindow, Open returns an error rather than silently
// returning nil. Catches the broken-shim case where the Microsoft
// `code` shim prints "Cannot find module .../cli.js" and exits 1
// before showing a window — the shim suppresses its own stderr, so
// without this layer the click looks like a no-op.
func TestOpen_FastExitFailureSurfacesEditorName(t *testing.T) {
	originalLookPath := lookPath
	originalStart := startCmd
	originalObserve := observeFastExit
	t.Cleanup(func() {
		lookPath = originalLookPath
		startCmd = originalStart
		observeFastExit = originalObserve
	})
	lookPath = func(string) (string, error) { return "/usr/bin/code", nil }
	startCmd = func(*exec.Cmd) error { return nil }
	observeFastExit = func(_ *exec.Cmd, name string) error {
		return fmt.Errorf("editor: %s exited with code 1 before opening (likely a broken install — try running it from a terminal)", name)
	}

	ed := &Editor{
		ID: "code", Name: "VS Code", Command: "code",
		Available: true, ResolvedPath: "/usr/bin/code",
		LaunchStyle: LaunchStyleGoto,
	}
	err := Open(context.Background(), SpawnOptions{Editor: ed, Path: "/tmp/x"})
	if err == nil {
		t.Fatal("expected fast-exit failure to surface as an error")
	}
	if !strings.Contains(err.Error(), "VS Code") {
		t.Fatalf("error should name the editor; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "exited with code 1") {
		t.Fatalf("error should report exit code; got %q", err.Error())
	}
}

// TestOpen_RejectsRelativeWorkspace confirms the workspacePath itself
// must be absolute. A relative workspacePath would just shift the
// cwd-relative attack vector up one level.
func TestOpen_RejectsRelativeWorkspace(t *testing.T) {
	ed := &Editor{
		ID: "code", Name: "VS Code",
		Available: true, ResolvedPath: "/usr/bin/code",
		LaunchStyle: LaunchStyleGoto,
	}
	err := Open(context.Background(), SpawnOptions{
		Editor:        ed,
		Path:          "foo.go",
		WorkspacePath: "relative/workspace",
	})
	if err == nil {
		t.Fatal("expected error for relative workspacePath")
	}
	if !strings.Contains(err.Error(), "workspacePath must be absolute") {
		t.Fatalf("expected workspacePath error, got %q", err.Error())
	}
}

// TestOpen_RejectsTraversalPath pins the canonicalisation guard. An
// absolute path that contains traversal segments (`/foo/../etc/passwd`)
// canonicalises to a different string; the check refuses it before the
// editor sees it. Defence in depth alongside whatever workspace
// validation the caller might layer on top.
func TestOpen_RejectsTraversalPath(t *testing.T) {
	ed := &Editor{
		ID: "code", Name: "VS Code",
		Available: true, ResolvedPath: "/usr/bin/code",
		LaunchStyle: LaunchStyleGoto,
	}
	err := Open(context.Background(), SpawnOptions{Editor: ed, Path: "/foo/../etc/passwd"})
	if err == nil {
		t.Fatal("expected error for traversal path")
	}
	if !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("expected error to mention 'canonical', got %q", err.Error())
	}
}

// TestResolvePath_AbsoluteWithWorkspaceRejectsOutsidePath pins the
// defense-in-depth gap closed for absolute paths: a network token
// holder calling OpenInEditor with path="/etc/passwd" alongside ANY
// workspacePath must not get an editor pointed at /etc/passwd. The
// pre-fix branch returned the canonical absolute path unchanged.
func TestResolvePath_AbsoluteWithWorkspaceRejectsOutsidePath(t *testing.T) {
	ws := t.TempDir()
	_, err := ResolvePath("/etc/passwd", ws)
	if err == nil {
		t.Fatalf("expected error for absolute path outside workspace, got nil")
	}
	if !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("expected error to mention 'escapes workspace', got %q", err.Error())
	}
}

// TestResolvePath_AbsoluteWithWorkspaceAcceptsInsidePath asserts the
// new containment rule allows absolute paths that ARE inside the
// supplied workspace — that's the legitimate case the validator
// emits when the agent writes an absolute path inside the repo.
func TestResolvePath_AbsoluteWithWorkspaceAcceptsInsidePath(t *testing.T) {
	ws := t.TempDir()
	inside := filepath.Join(ws, "src", "foo.ts")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(inside, nil, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := ResolvePath(inside, ws)
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	if got != inside {
		t.Fatalf("expected %q, got %q", inside, got)
	}
}

// TestResolvePath_AbsoluteWithoutWorkspaceStillTrusted preserves the
// project-open code path: callers that hand us a project root with
// no workspace context (e.g. the sidebar's "Open in editor" menu)
// still get the canonical absolute back unchanged.
func TestResolvePath_AbsoluteWithoutWorkspaceStillTrusted(t *testing.T) {
	got, err := ResolvePath("/tmp/project-root", "")
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	if got != "/tmp/project-root" {
		t.Fatalf("expected pass-through, got %q", got)
	}
}

// TestResolvePath_RejectsSymlinkEscape covers the defense-in-depth
// symlink check at the click-time editor boundary. The validator
// already runs this check at extraction time; the click-time mirror
// catches future bypasses where a path reaches OpenInEditor without
// having gone through pathlinks.ExtractAndValidate.
func TestResolvePath_RejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatalf("mkdir ws: %v", err)
	}
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, nil, 0o644); err != nil {
		t.Fatalf("seed outside: %v", err)
	}
	link := filepath.Join(ws, "evil.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported in this test env: %v", err)
	}
	_, err := ResolvePath("evil.md", ws)
	if err == nil {
		t.Fatalf("expected symlink-escape rejection, got nil")
	}
	if !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("expected 'escapes workspace' error, got %q", err.Error())
	}
}

// TestResolvePath_NewFileFlowStillWorks asserts that the fallback to
// the lexical check kicks in when the target file does not exist
// yet — opening a new file via path is a legitimate workflow that
// the symlink resolution must not break.
func TestResolvePath_NewFileFlowStillWorks(t *testing.T) {
	ws := t.TempDir()
	got, err := ResolvePath("new/file.md", ws)
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	want := filepath.Join(ws, "new", "file.md")
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

// TestOpen_AcceptsCleanAbsolute confirms the happy path still flows
// through unchanged — a clean absolute path reaches startCmd and the
// editor's argv is built normally.
func TestOpen_AcceptsCleanAbsolute(t *testing.T) {
	originalLookPath := lookPath
	originalStart := startCmd
	originalObserve := observeFastExit
	t.Cleanup(func() {
		lookPath = originalLookPath
		startCmd = originalStart
		observeFastExit = originalObserve
	})

	lookPath = func(string) (string, error) { return "/usr/bin/code", nil }
	var captured *exec.Cmd
	startCmd = func(cmd *exec.Cmd) error {
		captured = cmd
		return nil
	}
	observeFastExit = func(*exec.Cmd, string) error { return nil }

	ed := &Editor{
		ID: "code", Name: "VS Code", Command: "code",
		Available: true, ResolvedPath: "/usr/bin/code",
		LaunchStyle: LaunchStyleGoto,
	}
	if err := Open(context.Background(), SpawnOptions{
		Editor: ed,
		Path:   "/tmp/foo.go",
	}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if captured == nil {
		t.Fatal("startCmd not invoked on clean absolute path")
	}
}

func TestOpen_TOCTOUFailureWrapsErrCommandNotFound(t *testing.T) {
	originalLookPath := lookPath
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { lookPath = originalLookPath })

	ed := &Editor{
		ID: "code", Name: "VS Code", Command: "code",
		Available: true, ResolvedPath: "/usr/bin/code",
		LaunchStyle: LaunchStyleGoto,
	}
	err := Open(context.Background(), SpawnOptions{Editor: ed, Path: "/tmp/x"})
	if !errors.Is(err, ErrCommandNotFound) {
		t.Fatalf("expected ErrCommandNotFound; got %v", err)
	}
}

func TestOpen_AssemblesArgsAndStartsCommand(t *testing.T) {
	originalLookPath := lookPath
	originalStart := startCmd
	originalObserve := observeFastExit
	t.Cleanup(func() {
		lookPath = originalLookPath
		startCmd = originalStart
		observeFastExit = originalObserve
	})

	lookPath = func(name string) (string, error) {
		if name == "/usr/bin/code" {
			return "/usr/bin/code", nil
		}
		return "", exec.ErrNotFound
	}
	observeFastExit = func(*exec.Cmd, string) error { return nil }

	var captured *exec.Cmd
	startCmd = func(cmd *exec.Cmd) error {
		captured = cmd
		return nil
	}

	ed := &Editor{
		ID: "code", Name: "VS Code", Command: "code",
		Available: true, ResolvedPath: "/usr/bin/code",
		LaunchStyle: LaunchStyleGoto,
	}
	if err := Open(context.Background(), SpawnOptions{
		Editor: ed,
		Path:   "/tmp/foo.go",
		Line:   10,
		Column: 5,
	}); err != nil {
		t.Fatalf("Open: %v", err)
	}

	if captured == nil {
		t.Fatal("startCmd not invoked")
	}
	if captured.Path != "/usr/bin/code" {
		t.Fatalf("captured.Path = %q, want /usr/bin/code", captured.Path)
	}
	wantArgs := []string{"/usr/bin/code", "--goto", "/tmp/foo.go:10:5"}
	if !reflect.DeepEqual(captured.Args, wantArgs) {
		t.Fatalf("captured.Args:\n got: %v\nwant: %v", captured.Args, wantArgs)
	}
	if captured.SysProcAttr == nil {
		t.Fatal("expected SysProcAttr set so editor outlives parent")
	}
}

func TestOpen_StartCommandFailureSurfacesEditorName(t *testing.T) {
	originalLookPath := lookPath
	originalStart := startCmd
	t.Cleanup(func() {
		lookPath = originalLookPath
		startCmd = originalStart
	})

	lookPath = func(string) (string, error) { return "/usr/bin/code", nil }
	startCmd = func(*exec.Cmd) error { return errors.New("boom") }

	ed := &Editor{
		ID: "code", Name: "VS Code",
		Available: true, ResolvedPath: "/usr/bin/code",
		LaunchStyle: LaunchStyleGoto,
	}
	err := Open(context.Background(), SpawnOptions{Editor: ed, Path: "/tmp/x"})
	if err == nil {
		t.Fatal("expected error from start failure")
	}
	if errors.Is(err, ErrNoEditor) || errors.Is(err, ErrCommandNotFound) {
		t.Fatalf("start failure should not be classified as no-editor/not-found; got %v", err)
	}
	// Error text must include the human-readable editor name so the
	// frontend toast can render "Failed to launch VS Code: boom".
	want := "VS Code"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("expected error text to include %q; got %q", want, err.Error())
	}
}
