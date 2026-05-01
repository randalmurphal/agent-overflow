package editor

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// SpawnOptions describes one open-in-editor invocation.
//
// Line and Column are 1-indexed when set. Zero is the documented "no
// cursor placement" sentinel — callers that want column 1 should pass
// Column == 1 explicitly. The convention matches t3-code's
// open-in-editor binding so the wire shape stays predictable.
//
// WorkspacePath, when non-empty, is the absolute base directory used
// to resolve a relative Path. The frontend passes the active thread's
// workspace so click sites that hand us a repo-relative path (diff
// cards, tool result paths) round-trip correctly. Resolution must
// stay below WorkspacePath — see ResolvePath for the traversal-escape
// guard.
type SpawnOptions struct {
	Editor        *Editor
	Path          string
	Line          int
	Column        int
	WorkspacePath string
}

// ResolvePath enforces the path-shape contract Open requires and
// returns the absolute path to spawn the editor against.
//
// Rules:
//   - Absolute path → must already be canonical (filepath.Clean is a
//     no-op). Returned unchanged. WorkspacePath is ignored.
//   - Relative path → WorkspacePath must be supplied, absolute, and
//     canonical. The result is filepath.Join(workspacePath, path).
//     The joined result must remain a sub-path of WorkspacePath; a
//     `..`-traversal that escapes the workspace is rejected.
//
// Why the escape guard is the load-bearing check: the LAN-bind threat
// model lets a token-holder over the network call OpenInEditor. Without
// the sub-path check, path = "../../../../etc/passwd" + workspace =
// "/home/user/repo" Joins to "/etc/passwd" cleanly and the canonical
// check passes. filepath.Rel surfaces the escape unambiguously.
func ResolvePath(path, workspacePath string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("editor: open: path is required")
	}
	if filepath.IsAbs(path) {
		if filepath.Clean(path) != path {
			return "", fmt.Errorf("editor: open: path must be canonical (no traversal), got %q", path)
		}
		return path, nil
	}
	if workspacePath == "" {
		return "", fmt.Errorf("editor: open: relative path %q requires workspacePath", path)
	}
	if !filepath.IsAbs(workspacePath) {
		return "", fmt.Errorf("editor: open: workspacePath must be absolute, got %q", workspacePath)
	}
	if filepath.Clean(workspacePath) != workspacePath {
		return "", fmt.Errorf("editor: open: workspacePath must be canonical, got %q", workspacePath)
	}
	joined := filepath.Join(workspacePath, path)
	rel, err := filepath.Rel(workspacePath, joined)
	if err != nil {
		return "", fmt.Errorf("editor: open: resolve %q against %q: %w", path, workspacePath, err)
	}
	// rel == ".." or starts with "..<sep>" means joined sits outside
	// workspacePath. rel == "." means joined equals workspacePath
	// itself, which is fine (opening the workspace dir). Anything else
	// is a normal sub-path.
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("editor: open: path %q escapes workspace %q", path, workspacePath)
	}
	return joined, nil
}

// lookPath is the indirection seam exec.LookPath flows through. Tests
// substitute a fake; production leaves it on exec.LookPath. The Open
// path uses it for a TOCTOU re-check between detect and spawn so an
// editor uninstalled in that window surfaces a clear error rather than
// a generic "command not found" exec error.
var lookPath = exec.LookPath

// startCmd is the indirection seam Cmd.Start flows through. Tests
// override this to record the invocation without spawning a real
// process; production leaves it on (*exec.Cmd).Start. The wrapper
// signature takes a *exec.Cmd so tests can inspect Path / Args /
// SysProcAttr exactly as production assembled them.
var startCmd = func(cmd *exec.Cmd) error {
	return cmd.Start()
}

// Open spawns the chosen editor with the supplied path and cursor
// placement. The child runs in its own process group (POSIX) /
// hidden-window (Windows) so it survives the parent process exiting
// — open-in-editor is fire-and-forget by design.
//
// Errors:
//   - opts.Editor nil or unavailable → ErrNoEditor.
//   - the binary disappeared between detect and spawn → an error that
//     wraps ErrCommandNotFound and includes the user-facing editor name.
//   - spawn itself failed → an error that wraps the underlying exec
//     error.
//
// stdout / stderr are routed to /dev/null. Editors that bridge their
// own logs (VS Code's `--verbose`, etc.) handle their own streams; we
// don't want a chatty editor blocking the goroutine that called Open.
func Open(ctx context.Context, opts SpawnOptions) error {
	if opts.Editor == nil || !opts.Editor.Available || opts.Editor.ResolvedPath == "" {
		return ErrNoEditor
	}
	// LAN-bind safety: a token-holder over the network can call
	// OpenInEditor on the host. ResolvePath enforces the floor — the
	// final path must be absolute, canonical, and (when joined from a
	// relative input) must not escape WorkspacePath. The trust boundary
	// above this is the token holder; anything stricter (allow-list of
	// workspace roots) belongs in app-level authz.
	resolvedPath, err := ResolvePath(opts.Path, opts.WorkspacePath)
	if err != nil {
		return err
	}
	opts.Path = resolvedPath

	resolved, err := resolveSpawnBinary(opts.Editor)
	if err != nil {
		return err
	}

	args := buildArgs(opts)
	cmd := exec.CommandContext(ctx, resolved, args...)
	devNull, err := openDevNull()
	if err != nil {
		return fmt.Errorf("editor: open dev null: %w", err)
	}
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	applyDetachAttrs(cmd)

	if err := startCmd(cmd); err != nil {
		_ = devNull.Close()
		return fmt.Errorf("editor: launch %s: %w", opts.Editor.Name, err)
	}
	// Hand the dev-null FD off to the child via Stdout/Stderr; we
	// can drop our own copy now. The child keeps the fd it was dup'd
	// into via os/exec, so closing here doesn't break the editor.
	_ = devNull.Close()
	return nil
}

// resolveSpawnBinary re-checks that the editor binary is still present
// at the path detection found. The TOCTOU window between DetectEditors
// and Open is short, but uninstalls do happen — when they do we want a
// clean "Editor X not found" toast rather than the kernel's exec
// failure bubbling up unrooted.
//
// For absolute paths we exec.LookPath the path itself: LookPath is
// happy with absolute inputs and validates execute permission, which
// catches both deletion and chmod-x.
func resolveSpawnBinary(e *Editor) (string, error) {
	if _, err := lookPath(e.ResolvedPath); err == nil {
		return e.ResolvedPath, nil
	}
	if e.Command != "" && e.Command != e.ResolvedPath {
		if path, err := lookPath(e.Command); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("editor: %s not found in PATH or known locations: %w", e.Name, ErrCommandNotFound)
}

// buildArgs translates a SpawnOptions into the editor-specific argv.
// Each branch matches the editor's documented command-line shape. We
// intentionally pre-build the args slice (rather than appending into
// a shared one) so each launch style stays auditable.
func buildArgs(opts SpawnOptions) []string {
	switch opts.Editor.LaunchStyle {
	case LaunchStyleGoto:
		// `code --goto path:line:col` — column defaults to 1 when a
		// line is set (column 0 is not a valid VS Code position).
		// When no line is supplied we drop --goto and pass the path
		// directly so the editor opens without moving the cursor.
		if opts.Line <= 0 {
			return []string{opts.Path}
		}
		col := opts.Column
		if col <= 0 {
			col = 1
		}
		target := opts.Path + ":" + strconv.Itoa(opts.Line) + ":" + strconv.Itoa(col)
		return []string{"--goto", target}
	case LaunchStyleLineColumn:
		// `subl --line N --column N path`. Each flag is independent —
		// pass only the ones the caller supplied so we don't force a
		// "column 1" jump for callers that only know the line.
		args := make([]string, 0, 5)
		if opts.Line > 0 {
			args = append(args, "--line", strconv.Itoa(opts.Line))
		}
		if opts.Column > 0 {
			args = append(args, "--column", strconv.Itoa(opts.Column))
		}
		args = append(args, opts.Path)
		return args
	default:
		return []string{opts.Path}
	}
}
