package sessionfork

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrSessionFileNotFound is returned when the JSONL for sessionID can't
// be located on disk (neither in the workspace's project dir nor in any
// other project dir under ~/.claude/projects/).
var ErrSessionFileNotFound = errors.New("sessionfork: session file not found")

// LocateSessionFile resolves the on-disk path of a Claude session JSONL.
//
// Claude stores sessions at ~/.claude/projects/<slug>/<sessionID>.jsonl
// where slug is derived from the workspace's CANONICAL absolute path
// (symlinks resolved): replace each path separator with '-' and prepend
// a leading '-'. On macOS, /tmp resolves to /private/tmp so the slug is
// `-private-tmp-<...>` not `-tmp-<...>`.
//
// If the file isn't where we expect, fall back to scanning every project
// dir under ~/.claude/projects/ — sessions can migrate when a workspace
// is moved.
func LocateSessionFile(sessionID, workspacePath string) (string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", fmt.Errorf("sessionfork: empty sessionID")
	}
	// Defensive: reject sessionIDs that could break out of the projects dir via
	// path components or a NUL byte before the id ever reaches filepath.Join.
	// SessionRef originates from the provider's wire system_init.session_id (a
	// real process boundary, not a Go-minted UUID), and this helper is exposed
	// at the package boundary, so a malformed id must be caught here. Cheaper to
	// validate once than to audit every call site.
	if strings.ContainsAny(sessionID, "/\\\x00") || strings.Contains(sessionID, "..") {
		return "", fmt.Errorf("sessionfork: sessionID contains path separator, NUL, or traversal: %q", sessionID)
	}

	pdir, err := defaultProjectsDir()
	if err != nil {
		return "", err
	}

	// Primary lookup: compute the slug for the workspace's canonical path.
	if workspacePath != "" {
		canonical, err := filepath.EvalSymlinks(workspacePath)
		if err == nil {
			abs, err := filepath.Abs(canonical)
			if err == nil {
				slug := projectSlug(abs)
				candidate := filepath.Join(pdir, slug, sessionID+".jsonl")
				if fileExists(candidate) {
					return candidate, nil
				}
			}
		}
	}

	// Fallback: scan all project dirs.
	entries, err := os.ReadDir(pdir)
	if err != nil {
		// No projects dir at all = no session files. Treat as a normal
		// not-found rather than a hard error, so callers can fall back
		// gracefully (e.g. revert truncates the timeline even when
		// Claude state is gone from disk).
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrSessionFileNotFound, sessionID)
		}
		return "", fmt.Errorf("sessionfork: read projects dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(pdir, e.Name(), sessionID+".jsonl")
		if fileExists(candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("%w: %s", ErrSessionFileNotFound, sessionID)
}

// defaultProjectsDir returns ~/.claude/projects, resolving the user's home
// dir from $HOME (or os.UserHomeDir as fallback). It is the LIVE-thread
// answer; a caller writing beside an existing transcript derives the projects
// dir from that file instead (see WorkspaceProjectDir).
func defaultProjectsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("sessionfork: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// MaxSanitizedSlugLen mirrors MAX_SANITIZED_LENGTH in Claude's
// sessionStoragePortable.ts. At or below it the slug is the sanitized path
// verbatim; above it the CLI truncates to this length and appends a
// `Bun.hash(name)` suffix — a hash we cannot reproduce in Go — so an
// over-length path's exact project dir is unknowable from here.
const MaxSanitizedSlugLen = 200

// sanitizeProjectComponent encodes a string into a Claude project-dir name the
// way sessionStoragePortable.ts `sanitizePath` does: every non-alphanumeric
// rune becomes '-'. Claude's JS `String.replace(/[^a-zA-Z0-9]/g, '-')` runs
// over UTF-16 code units; for BMP paths — effectively all real workspace paths
// — rune iteration is equivalent (astral-plane chars would yield one '-' here
// vs two in JS, an irrelevant edge for filesystem paths).
func sanitizeProjectComponent(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// projectSlug encodes a canonical absolute path the way Claude does
// (sanitizeProjectComponent). Used for LocateSessionFile's primary-lookup
// candidate, so the over-length case returns a best-effort truncated prefix
// WITHOUT the Bun.hash suffix: it simply won't match the real dir, and the
// caller falls through to the full project-dir scan. Earlier this replaced
// only path separators, which silently missed for any path containing '.',
// '_', ':' (i.e. nearly all of them) and made the scan the de-facto path.
func projectSlug(absPath string) string {
	slug := sanitizeProjectComponent(absPath)
	if len(slug) > MaxSanitizedSlugLen {
		slug = slug[:MaxSanitizedSlugLen]
	}
	// Absolute paths begin with a separator, so the sanitized form already
	// leads with '-'. Keep the guard for the degenerate relative-path caller.
	if !strings.HasPrefix(slug, "-") {
		slug = "-" + slug
	}
	return slug
}

// exactWorkspaceSlug resolves workspacePath to its canonical absolute form and
// returns the EXACT Claude project-dir slug. ok is false when the sanitized
// slug exceeds MaxSanitizedSlugLen — there the CLI appends a Bun.hash suffix we
// can't reproduce, so callers that must land in a precise directory (session
// relocation) treat !ok as "unresolvable" rather than guess and misplace.
func exactWorkspaceSlug(workspacePath string) (slug string, ok bool, err error) {
	if strings.TrimSpace(workspacePath) == "" {
		return "", false, fmt.Errorf("sessionfork: empty workspace path")
	}
	// Match Claude's realpath-based canonicalization. The destination is the
	// reattach target and must exist, so a resolve failure is a real error,
	// not a soft miss.
	canonical, err := filepath.EvalSymlinks(workspacePath)
	if err != nil {
		return "", false, fmt.Errorf("sessionfork: canonicalize %s: %w", workspacePath, err)
	}
	abs, err := filepath.Abs(canonical)
	if err != nil {
		return "", false, fmt.Errorf("sessionfork: abs %s: %w", canonical, err)
	}
	s := sanitizeProjectComponent(abs)
	if len(s) > MaxSanitizedSlugLen {
		return "", false, nil
	}
	return s, true, nil
}

// WorkspaceProjectDir returns the EXACT project directory a session run with
// cwd == workspacePath is stored under — `<projectsDir>/<slug>` — without
// requiring the directory to exist.
//
// It is what a caller that must WRITE a transcript into the right slug uses:
// Claude resolves `--resume` against the slug of the current cwd, so a file
// written under any other slug is invisible to the resume that needs it (see
// RelocateSession's header). ok is false when the sanitized slug exceeds
// MaxSanitizedSlugLen, where the CLI appends a `Bun.hash` suffix Go cannot
// reproduce — there is no dir to name, and callers degrade rather than guess.
// A hard error means the workspace could not be canonicalized (it is gone).
//
// projectsDir is a PARAMETER rather than `~/.claude/projects`: the app can be
// running against an injected Claude home (the credential-home override, the
// harness's `AO_HARNESS_KEEP_HOME`), where `$HOME` and the home a transcript
// was read from are two different directories. A caller cutting a file beside
// an existing session derives it from that session's own location, and then
// the write can only ever land in the home it was read from.
func WorkspaceProjectDir(projectsDir, workspacePath string) (dir string, ok bool, err error) {
	slug, ok, err := exactWorkspaceSlug(workspacePath)
	if err != nil || !ok {
		return "", ok, err
	}
	return filepath.Join(projectsDir, slug), true, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Size() > 0
}
