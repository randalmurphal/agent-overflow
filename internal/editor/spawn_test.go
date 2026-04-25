package editor

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"testing"
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

func TestBuildArgs_LineColumn(t *testing.T) {
	opts := SpawnOptions{
		Editor: &Editor{LaunchStyle: LaunchStyleLineColumn},
		Path:   "/tmp/foo.go",
		Line:   12,
		Column: 3,
	}
	got := buildArgs(opts)
	want := []string{"--line", "12", "--column", "3", "/tmp/foo.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs line-column:\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildArgs_LineColumnLineOnly(t *testing.T) {
	opts := SpawnOptions{
		Editor: &Editor{LaunchStyle: LaunchStyleLineColumn},
		Path:   "/tmp/foo.go",
		Line:   12,
	}
	got := buildArgs(opts)
	want := []string{"--line", "12", "/tmp/foo.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs line-column line-only:\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildArgs_LineColumnNoCursor(t *testing.T) {
	opts := SpawnOptions{
		Editor: &Editor{LaunchStyle: LaunchStyleLineColumn},
		Path:   "/tmp/foo.go",
	}
	got := buildArgs(opts)
	want := []string{"/tmp/foo.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs line-column no cursor:\n got: %v\nwant: %v", got, want)
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

// TestOpen_RejectsRelativePath pins the LAN-bind safety floor: a remote
// token-holder asking the host's editor to open `./foo.ts` (which would
// resolve relative to the server's cwd) gets a clear error rather than
// a quiet open. The check fires before resolveSpawnBinary, so the test
// doesn't need to stub lookPath.
func TestOpen_RejectsRelativePath(t *testing.T) {
	ed := &Editor{
		ID: "code", Name: "VS Code",
		Available: true, ResolvedPath: "/usr/bin/code",
		LaunchStyle: LaunchStyleGoto,
	}
	err := Open(context.Background(), SpawnOptions{Editor: ed, Path: "./foo.ts"})
	if err == nil {
		t.Fatal("expected error for relative path")
	}
	if !contains(err.Error(), "absolute") {
		t.Fatalf("expected error to mention 'absolute', got %q", err.Error())
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
	if !contains(err.Error(), "canonical") {
		t.Fatalf("expected error to mention 'canonical', got %q", err.Error())
	}
}

// TestOpen_AcceptsCleanAbsolute confirms the happy path still flows
// through unchanged — a clean absolute path reaches startCmd and the
// editor's argv is built normally.
func TestOpen_AcceptsCleanAbsolute(t *testing.T) {
	originalLookPath := lookPath
	originalStart := startCmd
	t.Cleanup(func() {
		lookPath = originalLookPath
		startCmd = originalStart
	})

	lookPath = func(string) (string, error) { return "/usr/bin/code", nil }
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
	t.Cleanup(func() {
		lookPath = originalLookPath
		startCmd = originalStart
	})

	lookPath = func(name string) (string, error) {
		if name == "/usr/bin/code" {
			return "/usr/bin/code", nil
		}
		return "", exec.ErrNotFound
	}

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
	if !contains(err.Error(), want) {
		t.Fatalf("expected error text to include %q; got %q", want, err.Error())
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
