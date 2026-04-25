package editor

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
)

// SpawnOptions describes one open-in-editor invocation.
//
// Line and Column are 1-indexed when set. Zero is the documented "no
// cursor placement" sentinel — callers that want column 1 should pass
// Column == 1 explicitly. The convention matches t3-code's
// open-in-editor binding so the wire shape stays predictable.
type SpawnOptions struct {
	Editor *Editor
	Path   string
	Line   int
	Column int
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
	if opts.Path == "" {
		return fmt.Errorf("editor: open: empty path")
	}
	// LAN-bind safety: a token-holder over the network can call
	// OpenInEditor on the host. Without validation, anyone holding the
	// token could ask the host's editor to open /etc/passwd or a
	// traversal path that escapes the workspace. Enforce the floor:
	// the path must be absolute AND already canonical (filepath.Clean
	// returns the same value). The trust boundary above this is the
	// token holder — anything stricter (allow-list of workspace roots)
	// belongs in app-level authz and is tracked separately.
	if !filepath.IsAbs(opts.Path) {
		return fmt.Errorf("editor: open: path must be absolute, got %q", opts.Path)
	}
	if filepath.Clean(opts.Path) != opts.Path {
		return fmt.Errorf("editor: open: path must be canonical (no traversal), got %q", opts.Path)
	}

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
