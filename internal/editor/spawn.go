package editor

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// fastExitWindow caps how long Open waits for the spawned editor to
// exit before deciding the launch was successful. A graphical editor
// either runs indefinitely (we time out → success) or returns
// immediately after handing the open request off to a running instance
// (we observe exit code 0 → success). Anything that exits non-zero
// inside this window is a launch failure (e.g. broken Microsoft VS
// Code shim that fails on Cannot-find-module before showing a window).
//
// 750ms balances two concerns: long enough that VS Code's WSL Remote
// handshake on a slow first launch usually completes before we time
// out (so we observe the real exit code), short enough that a
// successful click-to-open doesn't feel laggy. The Microsoft shim's
// failure case typically exits in 100-300ms, comfortably inside this
// window.
const fastExitWindow = 750 * time.Millisecond

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
//   - Absolute path + WorkspacePath supplied → must be canonical AND
//     remain a sub-path of WorkspacePath after symlink resolution.
//     This mirrors the relative-path branch and closes the gap an
//     attacker would otherwise exploit by passing
//     path = "/etc/passwd" alongside any workspace.
//   - Absolute path + no WorkspacePath → must be canonical, returned
//     unchanged. Used by project-open affordances that hand us a
//     project root directly; the trust boundary above this is the
//     OpenInEditor binding's authz.
//   - Relative path → WorkspacePath must be supplied, absolute, and
//     canonical. The result is filepath.Join(workspacePath, path).
//     The joined result must remain a sub-path of WorkspacePath
//     after symlink resolution; a `..`-traversal that escapes (or a
//     workspace-internal symlink whose target sits outside) is
//     rejected.
//
// Why the escape guard is the load-bearing check: the LAN-bind threat
// model lets a token-holder over the network call OpenInEditor. Without
// the sub-path check, path = "../../../../etc/passwd" + workspace =
// "/home/user/repo" Joins to "/etc/passwd" cleanly and the canonical
// check passes. filepath.Rel surfaces the escape unambiguously.
//
// Symlink resolution is best-effort — when the joined path does not
// exist yet (the user is opening a new file), EvalSymlinks fails and
// we fall back to the lexical Rel check on the unresolved form. The
// validator (internal/pathlinks) catches symlink-escape candidates
// before they ever reach this code, so this is defense-in-depth.
func ResolvePath(path, workspacePath string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("editor: open: path is required")
	}
	if filepath.IsAbs(path) {
		if filepath.Clean(path) != path {
			return "", fmt.Errorf("editor: open: path must be canonical (no traversal), got %q", path)
		}
		if workspacePath == "" {
			return path, nil
		}
		if !filepath.IsAbs(workspacePath) {
			return "", fmt.Errorf("editor: open: workspacePath must be absolute, got %q", workspacePath)
		}
		if filepath.Clean(workspacePath) != workspacePath {
			return "", fmt.Errorf("editor: open: workspacePath must be canonical, got %q", workspacePath)
		}
		if err := ensureInsideWorkspace(path, workspacePath); err != nil {
			return "", err
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
	if err := ensureInsideWorkspace(joined, workspacePath); err != nil {
		return "", err
	}
	return joined, nil
}

// ensureInsideWorkspace returns nil if `target` resolves to a path
// inside `workspacePath` after symlink resolution, an error otherwise.
//
// When either side cannot be EvalSymlinks'd (most often because the
// target file does not exist yet), the check falls back to the lexical
// Rel comparison so the new-file flow keeps working. The validator
// (internal/pathlinks) already runs the symlink check at extraction
// time for agent-supplied paths, so this layer is the LAN-bind
// safety floor for direct OpenInEditor callers.
func ensureInsideWorkspace(target, workspacePath string) error {
	realTarget, errTarget := filepath.EvalSymlinks(target)
	realWorkspace, errWs := filepath.EvalSymlinks(workspacePath)
	useReal := errTarget == nil && errWs == nil
	cmpTarget := target
	cmpWorkspace := workspacePath
	if useReal {
		cmpTarget = realTarget
		cmpWorkspace = realWorkspace
	}
	rel, err := filepath.Rel(cmpWorkspace, cmpTarget)
	if err != nil {
		return fmt.Errorf("editor: open: resolve %q against %q: %w", target, workspacePath, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("editor: open: path %q escapes workspace %q", target, workspacePath)
	}
	return nil
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

// observeFastExit is the indirection seam the post-spawn watcher
// flows through. Tests that fake startCmd to skip the real fork+exec
// also need a fake watcher: cmd.Wait on a never-started cmd returns
// immediately with code -1, which would trip the broken-shim error
// path. Production sets this to defaultObserveFastExit, which spawns
// a goroutine that observes the real child's exit.
var observeFastExit = defaultObserveFastExit

// Open spawns the chosen editor with the supplied path and cursor
// placement. The child runs in its own session (POSIX) /
// hidden-window (Windows) so it survives the parent process exiting
// — open-in-editor is fire-and-forget by design.
//
// Errors:
//   - opts.Editor nil or unavailable → ErrNoEditor.
//   - the binary disappeared between detect and spawn → an error that
//     wraps ErrCommandNotFound and includes the user-facing editor name.
//   - spawn itself failed (exec returned an error) → an error that
//     wraps the underlying exec error.
//   - the editor exited non-zero within fastExitWindow → an error that
//     surfaces the editor name + exit code. Catches the broken-shim
//     case where the spawn appears to succeed (Start returns nil) but
//     the child immediately fails before opening anything. Defense in
//     depth alongside detection-time validateWindowsCodeShim.
//
// A long-lived editor that's still running after fastExitWindow is
// treated as success; the goroutine continues waiting for the eventual
// exit so the child reaps cleanly.
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

	return observeFastExit(cmd, opts.Editor.Name)
}

// defaultObserveFastExit watches the spawned child for fastExitWindow
// and surfaces a non-zero exit code as an error. Beyond the window the
// editor is presumed running; the wait goroutine keeps running so the
// child reaps cleanly when it eventually exits.
//
// Buffered channel + abandoned goroutine pattern: the goroutine always
// writes its result and returns; if no one reads (the timeout branch
// won), the buffered slot holds the result and the goroutine GCs at
// process scope. No leak — long-lived editors keep one goroutine
// blocked on Wait, which is what we'd need anyway to reap the child.
func defaultObserveFastExit(cmd *exec.Cmd, editorName string) error {
	type waitResult struct {
		err  error
		code int
	}
	done := make(chan waitResult, 1)
	go func() {
		err := cmd.Wait()
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		done <- waitResult{err: err, code: code}
	}()
	select {
	case res := <-done:
		if res.code == 0 {
			// Common for VS Code-family CLIs: hand off to the running
			// instance, exit 0 immediately. Treat as success.
			return nil
		}
		return fmt.Errorf("editor: %s exited with code %d before opening (likely a broken install — try running it from a terminal)", editorName, res.code)
	case <-time.After(fastExitWindow):
		// Editor still running — assume success.
		return nil
	}
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
