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
	realWorkspace, ok := canonicalWorkspace(workspacePath)
	if !ok {
		return nil
	}
	matches := pathPattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	if len(matches) > maxCandidates {
		matches = matches[:maxCandidates]
	}

	// Pass 1: filter candidates by boundary + heuristic, in match order.
	candidates := make([]pathCandidate, 0, len(matches))
	for _, m := range matches {
		if c, ok := candidateFromMatch(text, m, 0); ok {
			candidates = append(candidates, c)
		}
	}

	return validateCandidates(workspacePath, realWorkspace, candidates, stat)
}

// canonicalWorkspace runs the workspace preamble shared by the one-shot
// and streaming extractors. It rejects anything that isn't a usable
// workspace root — without an absolute canonical root we can't compute
// the boundary check (`filepath.Rel`) and can't safely join relatives,
// so the result would either over-accept or be silently misleading —
// and resolves the workspace's own symlinks once. The boundary check
// must compare symlink-resolved paths on both sides, or a workspace
// whose own path contains a symlink prefix (macOS /tmp → /private/tmp
// is the classic case) will produce false negatives against candidates
// that EvalSymlinks resolves through the same prefix. A missing or
// unreadable workspace drops everything because no further check can
// be trusted.
func canonicalWorkspace(workspacePath string) (string, bool) {
	if workspacePath == "" || !filepath.IsAbs(workspacePath) || filepath.Clean(workspacePath) != workspacePath {
		return "", false
	}
	realWorkspace, err := filepath.EvalSymlinks(workspacePath)
	if err != nil {
		return "", false
	}
	return realWorkspace, true
}

// pathCandidate is one boundary-and-heuristic-approved regex hit.
// start is the full-match start offset in the source text — the
// streaming scanner uses it to discard candidates from a rescanned
// tail region.
type pathCandidate struct {
	start int
	path  string
	line  int
	col   int
}

// candidateFromMatch applies the boundary + heuristic filter to one
// regex match. `m` holds submatch indices relative to text[base:]; the
// boundary byte is read from the full text so a region scan judges
// offset-zero matches by their true predecessor.
//
// Group indices: 0/1 = full, 2/3 = optional @, 4/5 = path body,
// 6/7 = line, 8/9 = col. The optional `@` is presentation only; the
// frontend re-detects it from surrounding text at wrap time.
func candidateFromMatch(text string, m []int, base int) (pathCandidate, bool) {
	pathStart, pathEnd := m[4], m[5]
	if pathStart < 0 {
		return pathCandidate{}, false
	}
	// Boundary rule: the char before the *full match* must be a
	// safe boundary OR input-start. If the match begins with `@`,
	// the boundary check applies to the char before the `@`.
	boundaryAt := base + m[0]
	if boundaryAt > 0 && !safeBoundary(text[boundaryAt-1]) {
		return pathCandidate{}, false
	}
	// strings.Clone severs the substring from the message body's
	// backing array. Without it, a single captured token would
	// keep the entire `text` alive in memory for as long as any
	// PathRef referencing it lived — multi-MB messages would pin
	// their full body in the item.Meta JSON-encoding pipeline.
	token := strings.Clone(text[base+pathStart : base+pathEnd])
	if !looksLikeFilePath(token) {
		return pathCandidate{}, false
	}
	line := 0
	col := 0
	if m[6] >= 0 {
		line = atoi(text[base+m[6] : base+m[7]])
	}
	if m[8] >= 0 {
		col = atoi(text[base+m[8] : base+m[9]])
	}
	return pathCandidate{start: boundaryAt, path: token, line: line, col: col}, true
}

// validateCandidates is passes 2+3 of the pipeline: one boundary check
// + stat per unique path (in source order — kernel dirent prefetch is
// more effective when sibling paths are stat'd together than when map
// iteration randomizes the order), then per-occurrence PathRef output
// in source-text order.
func validateCandidates(workspacePath, realWorkspace string, candidates []pathCandidate, stat statFunc) []PathRef {
	if len(candidates) == 0 {
		return nil
	}
	uniqueOrder := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		if _, dup := seen[c.path]; !dup {
			seen[c.path] = struct{}{}
			uniqueOrder = append(uniqueOrder, c.path)
		}
	}

	// Boundary check first (cheap, in-process) so a workspace-escape
	// never reaches stat.
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

// StreamScanner extracts path refs from an APPEND-ONLY text stream
// without rescanning the whole text on every tick. Regex extraction is
// incremental — each Update scans only the appended tail plus the
// trailing token run it may have extended — while validation is NOT:
// every Update re-stats the full known candidate list, so the returned
// refs are exactly what ExtractAndValidate would produce over the
// current text. That preserves the live-link behavior the streaming
// enrichment exists for: a path mentioned early and created later
// becomes clickable mid-stream, and a deleted file's link disappears.
//
// The incremental scan is exact, not approximate:
//
//   - A regex match never contains a byte outside the match charset
//     (see isPathMatchByte), so no match crosses a non-matchable byte.
//     Everything before the maximal matchable run containing the
//     previous scan boundary is final; the run itself is rescanned
//     because appended bytes may have extended its matches.
//   - maxCandidates keeps its global first-N-in-document-order
//     semantics: raw match starts (including boundary/heuristic
//     rejects, which the one-shot scan also counts against the cap)
//     are tracked so the stored prefix is always the first N raw
//     matches of the full text.
//
// Not safe for concurrent use; callers serialize Updates per stream.
type StreamScanner struct {
	workspacePath string
	stat          statFunc

	// scannedLen is the text length as of the last Update. Bytes before
	// the trailing matchable run at this offset are settled.
	scannedLen int
	// rawMatchStarts holds the full-match start offsets of every regex
	// hit counted toward maxCandidates, ascending. len == maxCandidates
	// means the cap is exhausted, exactly like the one-shot truncation.
	rawMatchStarts []int
	// candidates is the filtered occurrence list in document order.
	candidates []pathCandidate
}

// NewStreamScanner returns a scanner validating against workspacePath.
// The workspace preamble runs per Update (like ExtractAndValidate runs
// it per call), so a workspace that becomes unreadable mid-stream
// drops refs on the next tick.
func NewStreamScanner(workspacePath string) *StreamScanner {
	return newStreamScanner(workspacePath, defaultStat)
}

func newStreamScanner(workspacePath string, stat statFunc) *StreamScanner {
	return &StreamScanner{workspacePath: workspacePath, stat: stat}
}

// Update ingests the stream's CURRENT FULL text (not a delta) and
// returns the validated refs for it. The result matches
// ExtractAndValidate(workspacePath, text) exactly.
func (s *StreamScanner) Update(text string) []PathRef {
	if s == nil || text == "" {
		return nil
	}
	realWorkspace, ok := canonicalWorkspace(s.workspacePath)
	if !ok {
		return nil
	}
	s.extendScan(text)
	return validateCandidates(s.workspacePath, realWorkspace, s.candidates, s.stat)
}

func (s *StreamScanner) extendScan(text string) {
	if len(text) < s.scannedLen {
		// The stream shrank — not an append. Defensive full restart so a
		// caller contract violation degrades to the one-shot behavior
		// instead of emitting refs for text that no longer exists.
		s.scannedLen = 0
		s.rawMatchStarts = nil
		s.candidates = nil
	}
	if len(text) == s.scannedLen {
		return
	}
	// Walk back over the trailing run of match-charset bytes: matches in
	// it may extend as the stream appends, so it is re-derived each tick.
	scanStart := s.scannedLen
	for scanStart > 0 && isPathMatchByte(text[scanStart-1]) {
		scanStart--
	}
	for len(s.rawMatchStarts) > 0 && s.rawMatchStarts[len(s.rawMatchStarts)-1] >= scanStart {
		s.rawMatchStarts = s.rawMatchStarts[:len(s.rawMatchStarts)-1]
	}
	for len(s.candidates) > 0 && s.candidates[len(s.candidates)-1].start >= scanStart {
		s.candidates = s.candidates[:len(s.candidates)-1]
	}
	s.scannedLen = len(text)

	remaining := maxCandidates - len(s.rawMatchStarts)
	if remaining <= 0 {
		// Cap exhausted by matches before scanStart: the stored prefix is
		// the one-shot scan's truncated match list; later text is ignored
		// there too.
		return
	}
	// The limited FindAll mirrors the one-shot `matches[:maxCandidates]`
	// truncation: stored matches all start before scanStart, so stored +
	// region hits remain the first-N raw matches in document order. When
	// a previous tick hit the cap inside the trailing run, the trimming
	// above freed those slots and scanStart precedes the region the
	// limit left unscanned, so the rescan covers it.
	matches := pathPattern.FindAllStringSubmatchIndex(text[scanStart:], remaining)
	for _, m := range matches {
		s.rawMatchStarts = append(s.rawMatchStarts, scanStart+m[0])
		if c, ok := candidateFromMatch(text, m, scanStart); ok {
			s.candidates = append(s.candidates, c)
		}
	}
}

// isPathMatchByte reports whether b can appear INSIDE a pathPattern
// match: the pattern is built from literals `@ . / : - ~` and the
// ASCII-only \w / \d classes, so any byte outside this set is a hard
// match boundary. Keep in sync with pathPattern.
func isPathMatchByte(b byte) bool {
	switch {
	case b >= '0' && b <= '9', b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z':
		return true
	}
	switch b {
	case '_', '@', '.', '/', ':', '-', '~':
		return true
	}
	return false
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
