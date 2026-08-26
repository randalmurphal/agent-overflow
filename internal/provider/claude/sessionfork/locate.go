package sessionfork

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
)

// ErrSessionFileNotFound is returned when the JSONL for sessionID can't
// be located on disk (neither in the workspace's project dir nor in any
// other project dir under ~/.claude/projects/).
var ErrSessionFileNotFound = errors.New("sessionfork: session file not found")

// ProjectsDirForHome returns `<home>/.claude/projects`.
//
// The home is INJECTED, never resolved here. Every path this package
// returns becomes a WRITE target one call later — WriteForkFile* writes
// beside the transcript LocateSessionFile found — so a $HOME read here
// would put a fork into the developer's real `~/.claude/projects` on any
// boot whose provider home is pinned elsewhere (AO_HARNESS_KEEP_HOME, a
// test fixture's temp home). The app layer's App.providerHome() is the
// one seam that decides which home this is.
func ProjectsDirForHome(home string) string {
	return filepath.Join(home, ".claude", "projects")
}

// LocateSessionFile resolves the on-disk path of a Claude session JSONL
// under projectsDir (see ProjectsDirForHome).
//
// Claude stores sessions at <projectsDir>/<slug>/<sessionID>.jsonl
// where slug is the workspace's CANONICAL absolute path (symlinks
// resolved) run through the CLI's own encoder — every non-alphanumeric
// UTF-16 code unit becomes '-', and a sanitized form past
// MaxSanitizedSlugLen is truncated and hash-suffixed
// (claudeProjectDirName). On macOS, /tmp resolves to /private/tmp so the
// slug is `-private-tmp-<...>` not `-tmp-<...>`.
//
// If the file isn't where we expect, fall back to scanning every project
// dir under projectsDir — sessions migrate when a workspace is
// moved, and a pre-2.1.224 CLI wrote long paths under the untruncated
// name the primary candidate no longer spells.
func LocateSessionFile(projectsDir, sessionID, workspacePath string) (string, error) {
	if strings.TrimSpace(projectsDir) == "" {
		return "", fmt.Errorf("sessionfork: empty projects dir")
	}
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

	pdir := projectsDir

	// Primary lookup: compute the slug for the workspace's canonical path.
	if workspacePath != "" {
		canonical, err := filepath.EvalSymlinks(workspacePath)
		if err == nil {
			abs, err := filepath.Abs(canonical)
			if err == nil {
				slug := claudeProjectDirName(abs)
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

// MaxSanitizedSlugLen mirrors MAX_SANITIZED_LENGTH (`kie` in the minified
// bundle) in Claude's project-dir encoder. At or below it the slug is the
// sanitized path verbatim; above it the CLI truncates the SANITIZED string to
// this length and appends `-<hash>`, where the hash runs over the ORIGINAL
// (unsanitized) path — see claudeProjectDirName.
//
// The long-path truncation landed in 2.1.224 (collision fix). Older CLIs wrote
// the full sanitized name with no cap, which is why LocateSessionFile keeps its
// project-dir scan: the exact candidate answers for 2.1.224+, the scan still
// finds a transcript an older binary filed under the untruncated name.
const MaxSanitizedSlugLen = 200

// A note on CLAUDE_CODE_PROJECT_DIR_NAME, added in 2.1.234: it is NOT usable by
// AO as a way to pin a known project dir. Two disqualifiers, both read out of
// the 2.1.237 bundle. It is honored only when CLAUDE_CONFIG_DIR is ALSO set
// (`(t.CLAUDE_CONFIG_DIR ? sPs(t.CLAUDE_CODE_PROJECT_DIR_NAME) : void 0) ?? W9(r)`),
// and setting CLAUDE_CONFIG_DIR relocates settings AND credentials — a
// non-starter under AO's shared-`~/.claude` credential model, where the CLI and
// AO deliberately read the same login. And it is a process-wide memoized
// constant, not a per-project value: the resolver that consults it
// (`xN(e) = nlu() ?? W9(e)`) ignores its own path argument whenever the override
// is present, so one AO process would file every workspace's transcripts into a
// single directory. Reproducing W9 in Go is the only correct option.

// sanitizeProjectComponent encodes a string into a Claude project-dir name the
// way the CLI's `sanitizePath` does: `e.replace(/[^a-zA-Z0-9]/g, "-")`.
//
// That regex runs over UTF-16 CODE UNITS, so an astral-plane rune (one Go rune,
// two UTF-16 units) becomes TWO dashes, not one. That is not cosmetic any more:
// the output length decides both the MaxSanitizedSlugLen comparison and where
// the truncation cuts, so a one-dash-per-rune encoding would compute a different
// slug for any over-length path containing an emoji.
func sanitizeProjectComponent(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteByte(byte(r))
		case r > 0xFFFF:
			// Surrogate pair on the JS side: two code units, two dashes.
			b.WriteString("--")
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// claudeProjectDirHash ports the CLI's `y__` / `Act` pair — a classic
// djb2-style `h = h*31 + unit` fold rendered in lowercase base 36:
//
//	function Act(e){let t=0;for(let r=0;r<e.length;r++)t=(t<<5)-t+e.charCodeAt(r)|0;return t}
//	function y__(e){return Math.abs(Act(e)).toString(36)}
//
// Three details are load-bearing:
//
//   - The fold runs over the ORIGINAL path, not the sanitized form. Sanitizing
//     first would collide exactly the paths the suffix exists to separate.
//   - `charCodeAt` yields UTF-16 code units, so an astral rune contributes its
//     two surrogates in order. This is `utf16.Encode([]rune(path))` folded in
//     place; the split is done inline to keep the hash allocation-free.
//   - JS `|0` is ToInt32, and every intermediate is exact in a float64 (magnitude
//     below 2^33), so wrapping int32 arithmetic in Go is bit-identical. `Math.abs`
//     of the int32 result is a DOUBLE, so `Math.abs(-2147483648)` is 2147483648
//     ("zik0zk"), not a wrapped negative — widen to int64 before negating.
func claudeProjectDirHash(path string) string {
	var h int32
	for _, r := range path {
		if r > 0xFFFF {
			hi, lo := utf16.EncodeRune(r)
			h = h<<5 - h + int32(hi)
			h = h<<5 - h + int32(lo)
			continue
		}
		h = h<<5 - h + int32(r)
	}
	v := int64(h)
	if v < 0 {
		v = -v
	}
	return strconv.FormatInt(v, 36)
}

// claudeProjectDirName ports the CLI's project-dir encoder verbatim
// (2.1.237 bundle, `W9`):
//
//	function W9(e){let t=z$o(e);if(t.length<=kie)return t;return `${t.slice(0,kie)}-${y__(e)}`}
//
// The `slice(0,200)` is a UTF-16 slice of the SANITIZED string; sanitized output
// is pure ASCII, so a byte slice is exactly equivalent here.
//
// Callers pass an already-canonical absolute path, matching the CLI, which
// resolves + realpaths before encoding. There is no leading-dash fixup: an
// absolute path's separator already sanitizes to one, and synthesizing it for a
// relative caller would produce a name the CLI never writes.
func claudeProjectDirName(path string) string {
	s := sanitizeProjectComponent(path)
	if len(s) <= MaxSanitizedSlugLen {
		return s
	}
	return s[:MaxSanitizedSlugLen] + "-" + claudeProjectDirHash(path)
}

// exactWorkspaceSlug resolves workspacePath to its canonical absolute form and
// returns the EXACT Claude project-dir slug. Every path resolves, over-length
// ones included — claudeProjectDirName reproduces the CLI's truncate-and-hash
// suffix — so the only failure left is a workspace that cannot be
// canonicalized, i.e. it is gone. That is a hard error, never a soft miss: this
// answer is what a WRITE lands on, and a guess would put the transcript
// somewhere `claude --resume` will never look.
func exactWorkspaceSlug(workspacePath string) (string, error) {
	if strings.TrimSpace(workspacePath) == "" {
		return "", fmt.Errorf("sessionfork: empty workspace path")
	}
	// Match Claude's realpath-based canonicalization. The destination is the
	// reattach target and must exist.
	canonical, err := filepath.EvalSymlinks(workspacePath)
	if err != nil {
		return "", fmt.Errorf("sessionfork: canonicalize %s: %w", workspacePath, err)
	}
	abs, err := filepath.Abs(canonical)
	if err != nil {
		return "", fmt.Errorf("sessionfork: abs %s: %w", canonical, err)
	}
	return claudeProjectDirName(abs), nil
}

// WorkspaceProjectDir returns the EXACT project directory a session run with
// cwd == workspacePath is stored under — `<projectsDir>/<slug>` — without
// requiring the directory to exist.
//
// It is what a caller that must WRITE a transcript into the right slug uses:
// Claude resolves `--resume` against the slug of the current cwd, so a file
// written under any other slug is invisible to the resume that needs it (see
// RelocateSession's header). Over-length paths are answered exactly too — the
// CLI's truncate-and-hash suffix is reproduced by claudeProjectDirName — so the
// only failure is a workspace that could not be canonicalized (it is gone),
// and that is an error, not a soft "unresolvable" signal.
//
// projectsDir is a PARAMETER rather than `~/.claude/projects`: the app can be
// running against an injected Claude home (the credential-home override, the
// harness's `AO_HARNESS_KEEP_HOME`), where `$HOME` and the home a transcript
// was read from are two different directories. A caller cutting a file beside
// an existing session derives it from that session's own location, and then
// the write can only ever land in the home it was read from.
func WorkspaceProjectDir(projectsDir, workspacePath string) (string, error) {
	slug, err := exactWorkspaceSlug(workspacePath)
	if err != nil {
		return "", err
	}
	return filepath.Join(projectsDir, slug), nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Size() > 0
}
