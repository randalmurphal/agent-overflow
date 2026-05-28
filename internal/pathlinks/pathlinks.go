// Package pathlinks extracts file-path references from agent prose and
// validates them against a workspace filesystem.
//
// The chat surface auto-linkifies path-shaped tokens in assistant output
// (e.g. `src/lib/foo.ts:42`) so the user can click to open them in their
// editor. Pattern matching alone — what the frontend used to do —
// produced false positives for any `prefix/word.word` shape, including
// bogus tokens like `something/else.` or `foo/bar.nonsense`. This
// package centralizes detection and validation so the frontend can
// trust an allowlist of confirmed file paths rather than re-deriving
// candidates from prose.
//
// Pipeline: regex extracts candidates → heuristic filter rejects
// obvious non-paths (URLs, scoped npm packages, emails, version
// strings, trailing-dot tokens) → workspace-boundary check rejects
// `..`-escape and out-of-workspace absolute paths → unique paths are
// stat'd once → per-occurrence PathRef entries are returned for
// everything that validated.
//
// The workspace-boundary check is the load-bearing safety floor.
// Agent prose is untrusted input; a hostile message containing
// `../../etc/passwd` or `/etc/shadow` would otherwise turn `os.Stat`
// into a filesystem existence oracle (and ultimately a click-to-open
// gadget against host-sensitive paths). The check mirrors
// `internal/editor.ResolvePath` so validation and the eventual
// open-in-editor call agree.
//
// One pass per assistant message at content_block_stop. Sub-5ms for
// a 200-path/50KB pathological case; ~100µs for a typical 5-path
// message. A `maxCandidates` cap bounds the worst case so a hostile
// blob can't fan thousands of `os.Stat` syscalls at the settle hot
// path.
package pathlinks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// MetaKey is the JSON object key under which a validated PathRef
// allowlist lives on persisted item / channel-message meta. The
// frontend's getPathRefsFromMeta reads the same key — keep them in
// sync.
const MetaKey = "pathRefs"

// MarshalRefsJSON returns `{"pathRefs":[...]}` for the given refs.
// Centralised so triage and discussion can't drift on the wire shape
// — both call this, never re-emit the struct literal inline.
func MarshalRefsJSON(refs []PathRef) (string, error) {
	out, err := json.Marshal(struct {
		PathRefs []PathRef `json:"pathRefs"`
	}{PathRefs: refs})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// PathRef is one validated path occurrence. Multiple occurrences of
// the same Path produce multiple PathRef entries so the frontend can
// wrap every instance — but stat'ing happens once per unique Path.
type PathRef struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
	Col  int    `json:"col,omitempty"`
}

// Path body: at least one path segment with `/` separators. Allow
// letters, digits, `_`, `-`, `.`, `~`. The optional leading `@` is
// matched here so emails are still rejected — the post-filter on the
// character before the match enforces that `@` only counts when
// preceded by a safe boundary (whitespace, bracket, quote, etc.).
//
// RE2 doesn't support lookbehind, so the safe-boundary rule moves to a
// post-match check.
//
//	group 1 — optional leading `@` (presentation prefix)
//	group 2 — the path body (what we validate and emit)
//	group 3 — optional :line
//	group 4 — optional :col
var pathPattern = regexp.MustCompile(`(@)?((?:\.{0,2}/|/)?[\w.\-~]+(?:/[\w.\-~]+)+)(?::(\d+)(?::(\d+))?)?`)

// safeBoundary reports whether b can legitimately precede a path
// token in prose. Matches the frontend lookbehind set (whitespace +
// opening brackets + comma + semicolon + quotes + angle brackets +
// equals). Characters that would suggest the token is part of a URL,
// scoped npm package, or email (`:`, `/`, `@`, alphanumerics) are
// deliberately absent.
func safeBoundary(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '(', '[', '{', ',', ';', '\'', '"', '`', '<', '>', '=':
		return true
	}
	return false
}

// looksLikeFilePath rejects shapes that pass the regex body but
// aren't realistic file paths. The frontend has the same rejection
// list (see `frontend/src/lib/utils/pathLinkify.ts`).
func looksLikeFilePath(token string) bool {
	// `://` and `..//` would otherwise sneak through. The regex's path
	// segments don't allow consecutive slashes inside, but a token
	// captured at a URL tail could still contain one if the post-filter
	// fails to fire — defensive check.
	if strings.Contains(token, "//") {
		return false
	}
	lastSlash := strings.LastIndex(token, "/")
	final := token
	if lastSlash >= 0 {
		final = token[lastSlash+1:]
	}
	// Final segment must include a `.` to look like a file. This is
	// what distinguishes `src/lib/foo.ts` from `src/lib`.
	if !strings.Contains(final, ".") {
		return false
	}
	// Reject trailing-dot tokens like `something/else.` — common in
	// prose ("see something/else.") and never a real filename.
	if strings.HasSuffix(final, ".") {
		return false
	}
	// Reject pure version strings (`1.2.3`) that occasionally appear
	// after a slash in changelogs.
	if isVersionString(final) {
		return false
	}
	return true
}

func isVersionString(s string) bool {
	if s == "" {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 4 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
		}
	}
	return true
}

// statFunc is the existence check used by ExtractAndValidate. In
// production it's a thin wrapper over os.Stat; tests inject a counting
// or deterministic variant through the unexported `extractAndValidate`
// seam without needing a real filesystem.
type statFunc func(path string) bool

func defaultStat(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// maxCandidates caps the number of regex hits we'll consider per
// message. A hostile prose blob with thousands of path-shaped tokens
// would otherwise fan into thousands of os.Stat syscalls on the
// settle hot path. 256 is well above any realistic agent message
// (the spike's 200-path corpus was already pathological) while small
// enough to bound worst-case time even on cold WSL2 / NFS mounts.
const maxCandidates = 256

// ExtractAndValidate scans text for path-shaped tokens, filters by
// the heuristic, rejects anything that escapes workspacePath, then
// stats each surviving unique path. Returns one PathRef per validated
// occurrence in source-text order.
//
// `workspacePath` must be absolute and canonical. An empty or
// non-canonical workspacePath drops every candidate — relative paths
// have nothing to join against and absolute paths can't be checked
// for workspace containment.
//
// Absolute candidates are accepted only when they sit inside
// workspacePath. This is deliberate: the linkifier exists to surface
// files an agent is reasoning about in the user's project, not to
// probe arbitrary host filesystems. Click-time
// `internal/editor.ResolvePath` enforces the same rule; validation
// agreeing with it is what closes the existence-oracle vector
// (`/etc/shadow`, etc. would otherwise stat at validation time even
// though click would reject).
func ExtractAndValidate(workspacePath, text string) []PathRef {
	return extractAndValidate(workspacePath, text, defaultStat)
}

func extractAndValidate(workspacePath, text string, stat statFunc) []PathRef {
	if text == "" {
		return nil
	}
	// Reject anything that isn't a usable workspace root. Without an
	// absolute canonical root we can't compute the boundary check
	// (`filepath.Rel`) and can't safely join relatives, so the result
	// would either over-accept or be silently misleading.
	if workspacePath == "" || !filepath.IsAbs(workspacePath) || filepath.Clean(workspacePath) != workspacePath {
		return nil
	}
	// Resolve the workspace's own symlinks once. The boundary check
	// must compare symlink-resolved paths on both sides, or a workspace
	// whose own path contains a symlink prefix (macOS /tmp →
	// /private/tmp is the classic case) will produce false negatives
	// against candidates that EvalSymlinks resolves through the same
	// prefix. A missing/unreadable workspace drops everything because
	// no further check can be trusted.
	realWorkspace, err := filepath.EvalSymlinks(workspacePath)
	if err != nil {
		return nil
	}
	matches := pathPattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	if len(matches) > maxCandidates {
		matches = matches[:maxCandidates]
	}

	// Pass 1: filter candidates by boundary + heuristic. Build the
	// unique-path set in source order (slice + presence map) so the
	// stat phase hits the filesystem in document order — kernel
	// dirent prefetch is more effective when sibling paths are
	// stat'd together than when map iteration randomizes the order.
	type candidate struct {
		path string
		line int
		col  int
	}
	candidates := make([]candidate, 0, len(matches))
	uniqueOrder := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		// Group indices: 0/1 = full, 2/3 = optional @, 4/5 = path body,
		// 6/7 = line, 8/9 = col. The optional `@` is presentation only;
		// the frontend re-detects it from surrounding text at wrap time.
		pathStart, pathEnd := m[4], m[5]
		if pathStart < 0 {
			continue
		}
		// Boundary rule: the char before the *full match* must be a
		// safe boundary OR input-start. If the match begins with `@`,
		// the boundary check applies to the char before the `@`.
		boundaryAt := m[0]
		if boundaryAt > 0 && !safeBoundary(text[boundaryAt-1]) {
			continue
		}
		// strings.Clone severs the substring from the message body's
		// backing array. Without it, a single captured token would
		// keep the entire `text` alive in memory for as long as any
		// PathRef referencing it lived — multi-MB messages would pin
		// their full body in the item.Meta JSON-encoding pipeline.
		token := strings.Clone(text[pathStart:pathEnd])
		if !looksLikeFilePath(token) {
			continue
		}
		line := 0
		col := 0
		if m[6] >= 0 {
			line = atoi(text[m[6]:m[7]])
		}
		if m[8] >= 0 {
			col = atoi(text[m[8]:m[9]])
		}
		candidates = append(candidates, candidate{path: token, line: line, col: col})
		if _, dup := seen[token]; !dup {
			seen[token] = struct{}{}
			uniqueOrder = append(uniqueOrder, token)
		}
	}

	// Pass 2: validate each unique path. Boundary check first
	// (cheap, in-process) so a workspace-escape never reaches stat.
	// One stat per unique path, in source order.
	validated := make(map[string]struct{}, len(uniqueOrder))
	for _, token := range uniqueOrder {
		resolved, ok := resolveInsideWorkspace(workspacePath, realWorkspace, token)
		if !ok {
			continue
		}
		if stat(resolved) {
			validated[token] = struct{}{}
		}
	}

	// Pass 3: produce per-occurrence PathRef entries for validated
	// paths. Order follows source-text positions because Pass 1
	// walked matches in order.
	out := make([]PathRef, 0, len(candidates))
	for _, c := range candidates {
		if _, ok := validated[c.path]; !ok {
			continue
		}
		ref := PathRef{Path: c.path}
		if c.line > 0 {
			ref.Line = c.line
		}
		if c.col > 0 {
			ref.Col = c.col
		}
		out = append(out, ref)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveInsideWorkspace returns the absolute path to stat for
// `token` (relative paths joined to workspacePath, absolutes
// cleaned) and reports whether the result is inside workspacePath
// AFTER following any symlinks in the candidate path.
//
// `realWorkspace` is the symlink-resolved form of workspacePath,
// computed once by the caller — see extractAndValidate.
//
// The symlink resolution closes the workspace-internal-symlink
// escape: a file `workspace/notes` that is a symlink to
// `/etc/passwd` passes the lexical Rel check on its raw path but
// resolves outside the workspace once EvalSymlinks runs. Without
// this step, `os.Stat` would happily follow the symlink and emit a
// PathRef for a path the user never authored. EvalSymlinks also
// fails on broken/missing paths, which is the right behavior — a
// path that doesn't exist on disk has no business validating.
//
// workspacePath is guaranteed to be absolute + canonical here —
// extractAndValidate's preamble rejects anything else.
func resolveInsideWorkspace(workspacePath, realWorkspace, token string) (string, bool) {
	var resolved string
	if filepath.IsAbs(token) {
		resolved = filepath.Clean(token)
	} else {
		resolved = filepath.Join(workspacePath, token)
	}
	realResolved, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(realWorkspace, realResolved)
	if err != nil {
		return "", false
	}
	// rel == "." means the path equals workspacePath (a directory
	// reference, not a file); rel beginning with ".." (alone or
	// followed by the OS separator) means it escapes upward.
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return resolved, true
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
