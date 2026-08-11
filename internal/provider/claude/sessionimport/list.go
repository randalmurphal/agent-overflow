package sessionimport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/importir"
)

// Options configures a listing pass.
type Options struct {
	// ProjectsDir is the Claude projects root (`<claudeHome>/projects`).
	// REQUIRED and always injected: this package must never resolve a
	// home directory itself, because "which Claude home" is an app-level
	// decision (credential-home override, WSL relocation) that a library
	// guess would silently disagree with.
	ProjectsDir string
	// Concurrency bounds the head/tail read fan-out. <= 0 means
	// runtime.NumCPU().
	Concurrency int
}

// ErrProjectsDirRequired is returned when Options.ProjectsDir is empty.
var ErrProjectsDirRequired = errors.New("sessionimport: Options.ProjectsDir is required")

// SessionInfo is one importable Claude session, derived entirely from a
// stat plus a head/tail read.
type SessionInfo struct {
	SessionID string
	Path      string
	// ProjectPath is the session's recorded cwd. Always non-empty: a
	// session with no workspace has nowhere to be imported to, so List
	// skips it rather than hand the caller a row it cannot act on.
	ProjectPath string
	GitBranch   string
	Title       string

	CreatedAt      int64 // epoch ms, from the first row's ISO timestamp
	LastActivityAt int64 // epoch ms, file mtime
	SizeBytes      int64

	SubagentCount int
	// ForkedFromSessionID names the session this one was forked from, when
	// the transcript carries fork provenance. The scan orchestrator uses it
	// to keep a fork's ancestor out of the candidate list.
	ForkedFromSessionID string
	// Entrypoint is the transcript's own `entrypoint` marker, verbatim:
	// which client ran the session. Every row carries it, and the values
	// observed on a real home are `cli`, `sdk-cli` and `agent-overflow` (AO
	// pins `CLAUDE_CODE_ENTRYPOINT` on every spawn). Empty when the head
	// window holds none. Interpreting it is the orchestrator's job — this
	// package reports what the file says.
	Entrypoint string
}

// Warning codes emitted by List. Grouped so a caller can dedupe or count
// without matching prose.
const (
	WarnUnreadableProjectDir = "unreadable-project-dir"
	WarnUnreadableSession    = "unreadable-session"
	WarnMissingWorkspace     = "missing-workspace"
)

// List enumerates every importable session under opts.ProjectsDir.
//
// It deliberately does NOT parse transcripts: a title, cwd, branch and
// timestamps all come out of the first and last 64 KB of each file. The
// only JSON decode is the single candidate line the first-prompt fallback
// needs, and only when the cheaper title sources came up empty.
//
// Sessions that are subagent sidechain files, or that carry no title of
// any kind (metadata-only stubs), are skipped silently — they are not
// user-visible conversations. Files that cannot be read produce a warning
// and are skipped; a bad file never fails the listing. A projects
// directory that cannot be read at all IS an error: that is the caller's
// provider-level "Claude home unavailable" signal.
func List(ctx context.Context, opts Options) ([]SessionInfo, []importir.Warning, error) {
	if strings.TrimSpace(opts.ProjectsDir) == "" {
		return nil, nil, ErrProjectsDirRequired
	}
	entries, err := os.ReadDir(opts.ProjectsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("read claude projects dir: %w", err)
	}

	var (
		candidates []candidate
		warnings   []importir.Warning
	)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, warnings, err
		}
		if !entry.IsDir() {
			continue
		}
		projectDir := filepath.Join(opts.ProjectsDir, entry.Name())
		found, warn := listProjectCandidates(projectDir)
		if warn != nil {
			warnings = append(warnings, *warn)
			continue
		}
		candidates = append(candidates, found...)
	}

	sessions, readWarnings := readCandidates(ctx, candidates, opts.Concurrency)
	warnings = append(warnings, readWarnings...)
	if err := ctx.Err(); err != nil {
		return nil, warnings, err
	}
	return dedupeAndSort(sessions), warnings, nil
}

type candidate struct {
	sessionID  string
	path       string
	projectDir string
}

func listProjectCandidates(projectDir string) ([]candidate, *importir.Warning) {
	names, err := os.ReadDir(projectDir)
	if err != nil {
		return nil, &importir.Warning{
			Code:    WarnUnreadableProjectDir,
			Message: fmt.Sprintf("Could not read %s, so its sessions were skipped.", filepath.Base(projectDir)),
		}
	}
	out := make([]candidate, 0, len(names))
	for _, name := range names {
		if name.IsDir() {
			continue
		}
		base := name.Name()
		if !strings.HasSuffix(base, ".jsonl") {
			continue
		}
		sessionID := strings.TrimSuffix(base, ".jsonl")
		if !validSessionUUID(sessionID) {
			continue
		}
		out = append(out, candidate{
			sessionID:  sessionID,
			path:       filepath.Join(projectDir, base),
			projectDir: projectDir,
		})
	}
	return out, nil
}

// readCandidates fans the head/tail reads out over a bounded worker pool.
func readCandidates(ctx context.Context, candidates []candidate, concurrency int) ([]SessionInfo, []importir.Warning) {
	if len(candidates) == 0 {
		return nil, nil
	}
	if concurrency <= 0 {
		concurrency = runtime.NumCPU()
	}
	if concurrency > len(candidates) {
		concurrency = len(candidates)
	}

	var (
		mu       sync.Mutex
		sessions []SessionInfo
		warnings []importir.Warning
		wg       sync.WaitGroup
	)
	work := make(chan candidate)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, liteBufSize)
			for c := range work {
				info, warn, ok := readCandidate(c, buf)
				mu.Lock()
				if warn != nil {
					warnings = append(warnings, *warn)
				}
				if ok {
					sessions = append(sessions, info)
				}
				mu.Unlock()
			}
		}()
	}

	for _, c := range candidates {
		if ctx.Err() != nil {
			break
		}
		work <- c
	}
	close(work)
	wg.Wait()
	return sessions, warnings
}

func readCandidate(c candidate, buf []byte) (SessionInfo, *importir.Warning, bool) {
	lite, err := readLite(c.path, buf)
	if err != nil {
		if errors.Is(err, errEmptySession) {
			// A zero-byte transcript is a crashed start, not a session.
			return SessionInfo{}, nil, false
		}
		return SessionInfo{}, &importir.Warning{
			Code:    WarnUnreadableSession,
			Message: fmt.Sprintf("Could not read session %s: %v", c.sessionID, err),
		}, false
	}
	info, ok := parseSessionInfo(c, lite)
	if !ok {
		return SessionInfo{}, nil, false
	}
	if info.ProjectPath == "" {
		// No cwd anywhere in the head window. Every transcript row carries
		// one, so this is a damaged file — and without a workspace there is
		// no project to attach an imported thread to. Say so instead of
		// returning a row whose import would have to invent a path.
		return SessionInfo{}, &importir.Warning{
			Code:    WarnMissingWorkspace,
			Message: fmt.Sprintf("Session %s records no workspace directory and cannot be imported.", c.sessionID),
		}, false
	}
	info.SubagentCount = countSubagentTranscripts(filepath.Join(c.projectDir, c.sessionID, subagentsSubdir))
	return info, nil, true
}

// parseSessionInfo derives the listing row from a lite read. Returns
// ok=false for the two categories that are not importable conversations:
// a subagent sidechain file, and a metadata-only file with no title from
// any source.
func parseSessionInfo(c candidate, lite liteFile) (SessionInfo, bool) {
	firstLine := lite.Head
	if idx := bytes.IndexByte(firstLine, '\n'); idx >= 0 {
		firstLine = firstLine[:idx]
	}
	if containsAny(firstLine, `"isSidechain":true`, `"isSidechain": true`) {
		return SessionInfo{}, false
	}

	// Title precedence, ported from listSessionsImpl.ts: a user rename
	// beats an AI title beats the most recent prompt beats a legacy
	// summary beats the first prompt in the file. Tail before head at
	// every step — these records are appended, so the tail copy is the
	// current one.
	title := firstNonEmpty(
		extractLastJSONStringField(lite.Tail, "customTitle"),
		extractLastJSONStringField(lite.Head, "customTitle"),
		extractLastJSONStringField(lite.Tail, "aiTitle"),
		extractLastJSONStringField(lite.Head, "aiTitle"),
		extractLastJSONStringField(lite.Tail, "lastPrompt"),
		extractLastJSONStringField(lite.Tail, "summary"),
		extractFirstPromptFromHead(lite.Head),
	)
	if title == "" {
		return SessionInfo{}, false
	}

	info := SessionInfo{
		SessionID:      c.sessionID,
		Path:           c.path,
		ProjectPath:    extractJSONStringField(lite.Head, "cwd"),
		GitBranch:      firstNonEmpty(extractLastJSONStringField(lite.Tail, "gitBranch"), extractJSONStringField(lite.Head, "gitBranch")),
		Title:          title,
		LastActivityAt: lite.ModTimeMillis,
		SizeBytes:      lite.Size,

		ForkedFromSessionID: extractForkedFromSessionID(lite.Head),
		// FIRST occurrence, like `cwd` beside it: the marker names the client
		// that STARTED the session, and a transcript resumed by another
		// client appends rows carrying that client's own value. Same head
		// buffer, so this costs no additional read.
		Entrypoint: extractJSONStringField(lite.Head, "entrypoint"),
	}
	info.CreatedAt = parseISOMillis(extractJSONStringField(lite.Head, "timestamp"))
	if info.CreatedAt == 0 {
		// The first row's ISO timestamp is the honest creation time and
		// survives a copy; mtime is the only thing left when it is
		// missing (older writers, truncated first line).
		info.CreatedAt = lite.ModTimeMillis
	}
	return info, true
}

// extractForkedFromSessionID reads `forkedFrom.sessionId` out of the head
// window. The scan is SCOPED to the object that follows the `forkedFrom`
// key — an unscoped `sessionId` lookup would return the fork's OWN id,
// which every transcript row carries.
func extractForkedFromSessionID(head []byte) string {
	idx := bytes.Index(head, []byte(`"forkedFrom"`))
	if idx < 0 {
		return ""
	}
	rest := head[idx:]
	// Bound the scan to the record the key belongs to; a later row's
	// sessionId must not leak in when this row's object is truncated.
	if end := bytes.IndexByte(rest, '\n'); end >= 0 {
		rest = rest[:end]
	}
	return extractJSONStringField(rest, "sessionId")
}

// subagentsSubdir is the per-session directory Claude writes one
// `agent-<agentId>.jsonl` transcript into per Task/Agent invocation.
const subagentsSubdir = "subagents"

// countSubagentTranscripts counts the subagent transcripts of a session.
// It counts `agent-*.jsonl` specifically rather than every directory
// entry: each agent also gets an `agent-<id>.meta.json` sidecar, so a
// bare entry count reports double.
func countSubagentTranscripts(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if name := e.Name(); strings.HasPrefix(name, agentFilePrefix) && strings.HasSuffix(name, ".jsonl") {
			count++
		}
	}
	return count
}

// parseISOMillis parses Claude's ISO-8601 timestamps into epoch ms.
// Returns 0 when the value is absent or unparseable — callers substitute
// their own fallback rather than inheriting a zero time.
func parseISOMillis(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// dedupeAndSort collapses same-id sessions (the same conversation can
// exist under two project slugs after a workspace move — the newest copy
// is the live one) and orders newest first, breaking mtime ties on the
// session id so the listing is stable across calls.
func dedupeAndSort(sessions []SessionInfo) []SessionInfo {
	byID := make(map[string]SessionInfo, len(sessions))
	for _, s := range sessions {
		existing, ok := byID[s.SessionID]
		// Reads are concurrent, so arrival order is not stable — the
		// tie-break on path is what keeps two same-mtime copies from
		// resolving differently on consecutive calls.
		if ok && (existing.LastActivityAt > s.LastActivityAt ||
			(existing.LastActivityAt == s.LastActivityAt && existing.Path <= s.Path)) {
			continue
		}
		byID[s.SessionID] = s
	}
	out := make([]SessionInfo, 0, len(byID))
	for _, s := range byID {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastActivityAt != out[j].LastActivityAt {
			return out[i].LastActivityAt > out[j].LastActivityAt
		}
		return out[i].SessionID > out[j].SessionID
	})
	return out
}
