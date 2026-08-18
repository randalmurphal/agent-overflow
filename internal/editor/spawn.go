package editor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/appimage"
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
// cards, tool result paths) round-trip correctly. See ResolvePath for
// the openability rule the resolved target must satisfy.
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
//   - A leading `~/` (or a bare `~`) expands to the backend process's
//     home directory before any other rule runs, so agent-written
//     links like `~/.claude/notes.md` resolve the way a shell would.
//     The expansion must stay under home — `~/../…` is refused rather
//     than silently arriving at the canonicality check pre-cleaned.
//   - Absolute path + WorkspacePath supplied → must be canonical, then
//     the openability rule (resolveAgainstWorkspace): an existing
//     regular file opens from anywhere; a target that exists but is
//     not a regular file is refused everywhere; a target that does not
//     exist yet opens only inside the workspace (the new-file flow).
//   - Absolute path + no WorkspacePath → must be canonical, returned
//     unchanged, no stat. This is the deliberate project-open trust
//     path — the header "Open workspace" affordances hand us a project
//     root to folder-open. It is safe to leave untyped because the
//     rendered-markdown pipeline never produces a workspace-less link
//     (pathLinkExtension.ts refuses every href shape without a
//     workspace), and the OpenInEditor binding itself is classified
//     LocalOnly in internal/transport — a remote token-holder cannot
//     call it at all.
//   - Relative path → WorkspacePath must be supplied, absolute, and
//     canonical. The result is filepath.Join(workspacePath, path),
//     then the same openability rule as the absolute branch.
//
// Threat model, in caller-visible terms: the inputs that reach this
// function from rendered markdown are model-authored or third-party
// (PR bodies, review comments) with NO render-time validation — the
// user's click plus this gate are the entire control. The rule keeps a
// click at "show the local user an existing file in their own editor",
// plus in-workspace new-file creation. Directory opens are refused
// even inside the workspace: VS Code folder-opens execute workspace
// config (`.vscode/` tasks), and model output can author both the
// link and the tasks file. UNC (`\\`) input is refused before any
// stat — on Windows the stat itself performs SMB authentication
// against the named host.
func ResolvePath(path, workspacePath string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("editor: open: path is required")
	}
	// UNC shapes are refused up front, on every platform: on Windows
	// even a stat of `\\host\share` performs SMB authentication against
	// the named host (the NetNTLM leak vector), and no click surface
	// legitimately produces a backslash-backslash path on POSIX either
	// (there it would be a bizarre relative filename, not a share).
	if strings.HasPrefix(path, `\\`) {
		return "", fmt.Errorf("editor: open: network share paths are not supported, got %q", path)
	}
	path, err := expandHome(path)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(path) {
		if filepath.Clean(path) != path {
			return "", fmt.Errorf("editor: open: path must be canonical (no traversal), got %q", path)
		}
		if workspacePath == "" {
			return path, nil
		}
		if err := validateWorkspacePath(workspacePath); err != nil {
			return "", err
		}
		return resolveAgainstWorkspace(path, workspacePath)
	}
	if workspacePath == "" {
		return "", fmt.Errorf("editor: open: relative path %q requires workspacePath", path)
	}
	if err := validateWorkspacePath(workspacePath); err != nil {
		return "", err
	}
	return resolveAgainstWorkspace(filepath.Join(workspacePath, path), workspacePath)
}

func validateWorkspacePath(workspacePath string) error {
	// Same UNC refusal as the path input: a `\\host\share` workspace
	// would reach EvalSymlinks below, and on Windows that stat performs
	// SMB authentication against the named host.
	if strings.HasPrefix(workspacePath, `\\`) {
		return fmt.Errorf("editor: open: network share workspace paths are not supported, got %q", workspacePath)
	}
	if !filepath.IsAbs(workspacePath) {
		return fmt.Errorf("editor: open: workspacePath must be absolute, got %q", workspacePath)
	}
	if filepath.Clean(workspacePath) != workspacePath {
		return fmt.Errorf("editor: open: workspacePath must be canonical, got %q", workspacePath)
	}
	return nil
}

// userHomeDir is the indirection seam home expansion flows through so
// tests can pin a fixture home; production leaves it on os.UserHomeDir.
var userHomeDir = os.UserHomeDir

// expandHome rewrites a leading `~/` (or a bare `~`) to the current
// user's home directory. `~user/...` forms are NOT supported — they
// would need passwd lookups and no click surface produces them; they
// fall through unchanged and are treated as workspace-relative names
// downstream (real files with `~`-leading names exist, e.g. Office's
// `~$doc.docx` lock files, so refusing outright would break those).
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("editor: open: expand %q: %w", path, err)
	}
	if path == "~" {
		return home, nil
	}
	joined := filepath.Join(home, path[2:])
	// Join cleans, so `~/../etc/passwd` would otherwise hop out of home
	// and arrive at the canonicality check already clean. The tilde form
	// must stay under home; anything else has an honest absolute
	// spelling that passes the same gates. Guard consistency, not a
	// privilege boundary.
	if rel, err := filepath.Rel(home, joined); err != nil ||
		rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("editor: open: %q escapes the home directory", path)
	}
	return joined, nil
}

// resolveAgainstWorkspace applies the openability rule to a resolved
// absolute target (see the ResolvePath doc for the threat model):
//   - An existing REGULAR FILE is openable anywhere, inside or outside
//     the workspace. Showing a file in an editor executes nothing.
//   - A target that exists but is not a regular file (directories,
//     sockets, devices) is refused everywhere — including inside the
//     workspace, where model output can author both a `[src](src)`
//     link and the `.vscode/tasks.json` a folder-open would execute.
//     The project-open affordances that legitimately folder-open pass
//     no WorkspacePath and take the absolute pass-through instead.
//   - A target that does not exist opens only INSIDE the workspace
//     (the new-file flow); outside it the affordance stays "show me
//     this file", never "scaffold files anywhere on the host".
//
// The error strings stay diagnostic (missing vs directory) on purpose:
// they surface as user-facing toasts on a dead link, and the only
// caller that could abuse them as a filesystem oracle is a loopback
// peer that already holds BrowseDirectory.
func resolveAgainstWorkspace(target, workspacePath string) (string, error) {
	info, statErr := os.Stat(target)
	if statErr == nil {
		if info.Mode().IsRegular() {
			return target, nil
		}
		return "", fmt.Errorf("editor: open: %q is not a regular file; links open files only (a folder open can execute workspace config)", target)
	}
	if insideWorkspace(target, workspacePath) {
		// New-file flow: a not-yet-existing target inside the workspace
		// is handed to the editor to create. Stat failures other than
		// not-exist (permission, I/O) land here too — the editor
		// surfaces its own error if the path truly can't be opened.
		return target, nil
	}
	return "", fmt.Errorf("editor: open: %q is outside the workspace and does not exist: %v", target, statErr)
}

// insideWorkspace reports whether target resolves under workspacePath.
// Only consulted for targets that do NOT exist (existing targets are
// decided by the regular-file rule alone), so symlink resolution walks
// up to the nearest existing ancestor: a not-yet-created file under
// `ws/link/…` where `link` is a symlink out of the workspace must not
// count as inside — that would re-open the out-of-tree scaffolding
// door through a workspace-internal symlink. Rendered-markdown hrefs
// have no upstream validator, so this check is the only symlink gate
// on that path. Failures degrade to "outside", which the caller turns
// into a refusal — never into scaffolding.
func insideWorkspace(target, workspacePath string) bool {
	// Both sides go through the same ancestor-resolving walk: resolving
	// only the target would misclassify a workspace that itself sits
	// under a symlinked root (macOS /var → /private/var) as "outside"
	// its own contents.
	rel, err := filepath.Rel(
		resolveThroughExistingAncestor(workspacePath),
		resolveThroughExistingAncestor(target),
	)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveThroughExistingAncestor EvalSymlinks the deepest existing
// ancestor of target and lexically re-joins the not-yet-existing
// remainder. For a target that exists it is exactly EvalSymlinks; for
// one that doesn't, the remainder can't contain `..` segments because
// ResolvePath already required the canonical form.
func resolveThroughExistingAncestor(target string) string {
	remainder := ""
	dir := target
	for {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return filepath.Join(resolved, remainder)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Walked to the root without an existing ancestor; fall
			// back to the lexical form.
			return filepath.Join(dir, remainder)
		}
		remainder = filepath.Join(filepath.Base(dir), remainder)
		dir = parent
	}
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
	// ResolvePath enforces the click-surface floor: the final path must
	// be absolute and canonical, and must satisfy the openability rule
	// (existing regular file anywhere, new files only inside the
	// workspace, folder opens never). The inputs that reach here from
	// rendered markdown are model- or third-party-authored with no
	// render-time validation; the OpenInEditor binding itself is
	// LocalOnly, so a remote token-holder can't call it at all.
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
	// The editor inherits our environment, minus the AppImage launch
	// artifacts: an editor started from an AppImage build would otherwise
	// load its libraries out of our squashfs mount and lose them the moment
	// Agent Overflow exits, and every terminal it opens would inherit a PATH
	// pointing into that mount. nil on every other launch shape, which keeps
	// exec.Cmd on its own inherit path.
	cmd.Env = appimage.ScrubInherited()
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
	case LaunchStylePathLineColumn:
		// `subl path:line:col` / `zed path:line:col` — the position
		// rides on the path itself; neither editor has --line/--column
		// flags (see the constant's doc for the upstream references).
		// Append only the pieces the caller supplied: `:line` alone is
		// valid for both, and a column without a line is meaningless.
		target := opts.Path
		if opts.Line > 0 {
			target += ":" + strconv.Itoa(opts.Line)
			if opts.Column > 0 {
				target += ":" + strconv.Itoa(opts.Column)
			}
		}
		return []string{target}
	default:
		return []string{opts.Path}
	}
}
