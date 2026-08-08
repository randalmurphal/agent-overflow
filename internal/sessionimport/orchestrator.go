package sessionimport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/importir"
	claudesessions "agent-overflow/internal/provider/claude/sessionimport"
	"agent-overflow/internal/provider/codex/rollout"
	"agent-overflow/internal/store"
)

// The two providers whose on-disk sessions can be imported. `claude-tui` is
// deliberately absent: it drives the Claude binary and has no session files
// of its own, which is also why migration v50's CHECK omits it.
const (
	ProviderClaude = "claude"
	ProviderCodex  = "codex"
)

// Deps is everything the orchestrator needs from outside itself.
//
// Both provider homes are INJECTED, never resolved here. Which Claude/Codex
// home is in play is an app-level decision (credential-home override, WSL
// relocation) and a library guess would silently list sessions the app
// cannot resume. See the root AGENTS.md §Permanent invariants.
type Deps struct {
	Store   *store.Store
	GitCore *gitops.Core
	// ClaudeProjectsDir is `<claudeHome>/projects`. Empty means "this host
	// has no Claude home", which Scan reports as an unavailable provider
	// rather than as an error.
	ClaudeProjectsDir string
	// CodexHome is the directory holding Codex's thread index (normally
	// `~/.codex`). Empty is the same "unavailable, not broken" state.
	CodexHome string
}

// Filter narrows a scan.
//
// Nothing in the app reaches it: `ImportScanRequest` carries no filter, the
// modal narrows its own rows client-side, and `scanImportableSessions` always
// passes the zero value. It stays because this package's own tests use it to
// scan one provider's fixture home at a time — a wire filter is a different
// question (whose answer would depend on which filter last ran) from a library
// caller asking for a subset.
type Filter struct {
	// Provider limits the scan to one provider. Empty scans both.
	Provider string
	// WorkspacePath limits rows to sessions recorded against exactly that
	// workspace. It is matched verbatim — a session's cwd is what it is,
	// and resolving git roots for it would cost one subprocess per row.
	WorkspacePath string
}

// ProviderStatus is one provider's availability for this scan. Scan returns
// one per provider it was asked to scan, healthy or not: "Codex has no
// sessions" and "Codex could not be read" look identical in a row list, and
// only the second one is a problem the user can act on.
type ProviderStatus struct {
	Provider string
	// Available is true when the provider's home was read successfully.
	Available bool
	// Error is user-facing prose, empty when Available.
	Error string
	// SkippedCount counts session files the reader could not use.
	SkippedCount int
}

// Row is one importable session as the scan sees it: the session's own
// metadata, plus what AO knows about the project it belongs to.
type Row struct {
	// ID is the opaque row key the import accepts back. Minted here so a
	// caller never has to compose one out of provider + session id.
	ID             string
	Provider       string
	SessionID      string
	Title          string
	ProjectPath    string
	ProjectID      string
	ProjectLabel   string
	GitBranch      string
	CreatedAt      int64
	LastActivityAt int64
	SizeBytes      int64
	// BranchCount is how many AO threads importing this row will create.
	// Codex is always 1. Claude is 0, meaning NOT DETERMINED: enumerating a
	// transcript's leaves needs a full read of the file, and a real home is
	// gigabytes across a thousand-plus transcripts — the list would take
	// minutes. The true count is known at import and reported per row then.
	BranchCount   int
	SubagentCount int
	SourcePath    string
	// KnownProject is true when AO already has a project row covering this
	// session's workspace, so importing it will not create one.
	KnownProject bool
	// Warnings are user-facing prose about THIS row, not the scan.
	Warnings []string
}

// ScanResult is one scan.
type ScanResult struct {
	Providers []ProviderStatus
	Rows      []Row
}

// RowKey mints the opaque key that names one importable session.
func RowKey(providerName, sessionID string) string {
	return providerName + ":" + sessionID
}

// Scan enumerates every provider session AO does not already have.
//
// Three subtractions make the result safe to import wholesale, which is what
// an "Import All" button needs:
//
//  1. Sessions AO already knows (`ListImportedSessionRefs` — a live
//     session_ref, an unresumed fork's pending ref, or an earlier import's
//     recorded source). This is what makes pressing Import All twice a
//     no-op instead of a duplicate.
//  2. Fork ancestors. A forked session's file contains its parent's history,
//     so importing both would import that history twice, as two threads.
//  3. Subagent / spawned-child sessions. Those are already excluded inside
//     each provider's lister, which is where the provider-specific test for
//     "is this a child" belongs.
//
// A provider that cannot be read is reported in Providers and does not fail
// the scan: a broken Codex home must not take Claude's sessions away.
func Scan(ctx context.Context, d Deps, filter Filter) (ScanResult, error) {
	if d.Store == nil {
		return ScanResult{}, fmt.Errorf("sessionimport: scan has no store")
	}
	if err := ctx.Err(); err != nil {
		return ScanResult{}, err
	}

	// A failure here is fatal on purpose: without the dedup set every
	// already-imported session would be offered again, and Import All would
	// duplicate the user's whole history.
	known, err := d.Store.ListImportedSessionRefs()
	if err != nil {
		return ScanResult{}, err
	}
	projects, err := newProjectIndex(d.Store)
	if err != nil {
		return ScanResult{}, err
	}

	result := ScanResult{}
	for _, providerName := range scanProviders(filter.Provider) {
		status := ProviderStatus{Provider: providerName}
		rows, warnings, err := scanProvider(ctx, d, providerName, known)
		switch {
		case err != nil:
			status.Error = err.Error()
		default:
			status.Available = true
			status.SkippedCount = countSkipped(providerName, warnings)
			result.Rows = append(result.Rows, rows...)
		}
		result.Providers = append(result.Providers, status)
	}
	if err := ctx.Err(); err != nil {
		return ScanResult{}, err
	}

	kept := result.Rows[:0]
	for _, row := range result.Rows {
		if filter.WorkspacePath != "" && row.ProjectPath != filter.WorkspacePath {
			continue
		}
		projects.decorate(&row)
		if _, err := os.Stat(row.ProjectPath); err != nil {
			row.Warnings = append(row.Warnings, fmt.Sprintf(
				"The workspace %s no longer exists. Importing still works; resuming the session will not.",
				row.ProjectPath))
		}
		kept = append(kept, row)
	}
	result.Rows = kept

	// Newest first, id as the tiebreak so two scans of an unchanged home
	// produce the same order.
	sort.SliceStable(result.Rows, func(i, j int) bool {
		if result.Rows[i].LastActivityAt != result.Rows[j].LastActivityAt {
			return result.Rows[i].LastActivityAt > result.Rows[j].LastActivityAt
		}
		return result.Rows[i].ID < result.Rows[j].ID
	})
	return result, nil
}

// scanProviders resolves the filter into the providers to read.
func scanProviders(filtered string) []string {
	switch strings.TrimSpace(filtered) {
	case ProviderClaude:
		return []string{ProviderClaude}
	case ProviderCodex:
		return []string{ProviderCodex}
	default:
		return []string{ProviderClaude, ProviderCodex}
	}
}

func scanProvider(
	ctx context.Context, d Deps, providerName string, known map[string]string,
) ([]Row, []importir.Warning, error) {
	switch providerName {
	case ProviderClaude:
		return scanClaude(ctx, d, known)
	case ProviderCodex:
		return scanCodex(ctx, d, known)
	default:
		return nil, nil, fmt.Errorf("sessionimport: unknown provider %q", providerName)
	}
}

func scanClaude(ctx context.Context, d Deps, known map[string]string) ([]Row, []importir.Warning, error) {
	home := strings.TrimSpace(d.ClaudeProjectsDir)
	if home == "" {
		return nil, nil, fmt.Errorf("no Claude home was found on this machine")
	}
	sessions, warnings, err := claudesessions.List(ctx, claudesessions.Options{ProjectsDir: home})
	if err != nil {
		return nil, nil, fmt.Errorf("Claude sessions could not be read: %w", err)
	}

	candidates := make([]claudesessions.SessionInfo, 0, len(sessions))
	for _, session := range sessions {
		if _, taken := known[session.SessionID]; taken {
			continue
		}
		candidates = append(candidates, session)
	}
	// Fork ancestry is read off the surviving candidates only: a fork whose
	// ancestor is already imported has nothing to say about that ancestor,
	// and the whole point of the exclusion is to avoid importing one
	// conversation twice in one pass.
	ancestors := make(map[string]struct{}, len(candidates))
	for _, session := range candidates {
		if parent := strings.TrimSpace(session.ForkedFromSessionID); parent != "" {
			ancestors[parent] = struct{}{}
		}
	}

	rows := make([]Row, 0, len(candidates))
	for _, session := range candidates {
		if _, isAncestor := ancestors[session.SessionID]; isAncestor {
			continue
		}
		rows = append(rows, Row{
			ID:             RowKey(ProviderClaude, session.SessionID),
			Provider:       ProviderClaude,
			SessionID:      session.SessionID,
			Title:          session.Title,
			ProjectPath:    session.ProjectPath,
			GitBranch:      session.GitBranch,
			CreatedAt:      session.CreatedAt,
			LastActivityAt: session.LastActivityAt,
			SizeBytes:      session.SizeBytes,
			BranchCount:    0, // Not determined at list time — see Row.BranchCount.
			SubagentCount:  session.SubagentCount,
			SourcePath:     session.Path,
		})
	}
	return rows, warnings, nil
}

func scanCodex(ctx context.Context, d Deps, known map[string]string) ([]Row, []importir.Warning, error) {
	home := strings.TrimSpace(d.CodexHome)
	if home == "" {
		return nil, nil, fmt.Errorf("no Codex home was found on this machine")
	}
	sessions, warnings, err := rollout.List(ctx, rollout.ListOptions{CodexHome: home})
	if err != nil {
		return nil, nil, fmt.Errorf("Codex sessions could not be read: %w", err)
	}

	candidates := make([]rollout.SessionInfo, 0, len(sessions))
	for _, session := range sessions {
		if _, taken := known[session.ThreadID]; taken {
			continue
		}
		candidates = append(candidates, session)
	}
	ancestors, err := codexForkAncestors(ctx, candidates)
	if err != nil {
		return nil, nil, err
	}

	rows := make([]Row, 0, len(candidates))
	for _, session := range candidates {
		if _, isAncestor := ancestors[session.ThreadID]; isAncestor {
			continue
		}
		rows = append(rows, Row{
			ID:             RowKey(ProviderCodex, session.ThreadID),
			Provider:       ProviderCodex,
			SessionID:      session.ThreadID,
			Title:          session.Title,
			ProjectPath:    session.Cwd,
			GitBranch:      session.GitBranch,
			CreatedAt:      session.CreatedAt,
			LastActivityAt: session.LastActivityAt,
			SizeBytes:      session.SizeBytes,
			BranchCount:    1, // A rollout is one linear conversation.
			SourcePath:     session.RolloutPath,
		})
	}
	return rows, warnings, nil
}

// codexForkAncestorReaders bounds the fork-provenance fan-out. Each worker
// does one bounded head read of one rollout; the work is IO-latency bound, so
// a handful of readers hides the per-file open/seek cost of a home holding
// hundreds of sessions without turning a browse into a disk storm.
const codexForkAncestorReaders = 8

// codexForkAncestors reads the fork provenance of every candidate.
//
// Codex records it in the rollout file, not the thread index, so this costs
// one BOUNDED head read per surviving candidate (ReadSessionMeta stops at the
// first matching session_meta). A file with no readable meta simply names no
// ancestor — provenance is an exclusion, and failing to read one can only
// leave a session listed.
func codexForkAncestors(ctx context.Context, candidates []rollout.SessionInfo) (map[string]struct{}, error) {
	ancestors := make(map[string]struct{}, len(candidates))
	if len(candidates) == 0 {
		return ancestors, nil
	}
	workers := min(codexForkAncestorReaders, len(candidates))

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		next atomic.Int64
	)
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(candidates) {
					return
				}
				// Cancellation is read straight off ctx rather than mirrored
				// into a flag: ctx.Err() is sticky, so the check after Wait
				// below sees the same answer a worker that bailed early did.
				if i%32 == 0 && ctx.Err() != nil {
					return
				}
				session := candidates[i]
				meta, err := rollout.ReadSessionMeta(session.RolloutPath, session.ThreadID)
				if err != nil {
					continue
				}
				mu.Lock()
				for _, parent := range []string{meta.ForkedFromID, meta.ParentThreadID} {
					if parent = strings.TrimSpace(parent); parent != "" {
						ancestors[parent] = struct{}{}
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		// A cancelled fan-out has an INCOMPLETE ancestor set, and an
		// incomplete one is worse than none: every unread parent would be
		// offered for import alongside its fork.
		return nil, err
	}
	return ancestors, nil
}

// skippedWarningCodes are the per-provider warning codes that mean "one
// session FILE was not usable", as opposed to "something inside a session was
// odd" or "a whole directory could not be read". Only the first kind belongs
// in a per-file count.
//
// claudesessions.WarnUnreadableProjectDir is deliberately absent: it is one
// warning per project DIRECTORY, and counting it would report "1 session
// skipped" for a directory that held a hundred (or none). The directory
// warning still reaches the caller on the warning slice, where it reads as
// what it is.
var skippedWarningCodes = map[string]map[string]bool{
	ProviderClaude: {
		claudesessions.WarnUnreadableSession: true,
		claudesessions.WarnMissingWorkspace:  true,
	},
	ProviderCodex: {
		rollout.WarnRolloutMissing: true,
		rollout.WarnRolloutOutside: true,
	},
}

func countSkipped(providerName string, warnings []importir.Warning) int {
	codes := skippedWarningCodes[providerName]
	count := 0
	for _, warning := range warnings {
		if codes[warning.Code] {
			count++
		}
	}
	return count
}

// projectIndex answers "does AO already have a project for this workspace"
// without running git.
//
// EnsureForWorkspace resolves a git repository root before it looks a
// project up, and that is one subprocess per row — unaffordable across a
// thousand sessions in a listing a user expects to be instant. So the scan
// answers the cheaper question: is there a project AT this path, or a
// project whose root CONTAINS it? Both are true whenever the git-root
// resolution would have found the same row, and the answer only drives a
// label; the import itself still goes through EnsureForWorkspace.
type projectIndex struct {
	byPath map[string]store.Project
	// roots is byPath's key set, longest first — the containment probe's
	// iteration order. The projects themselves are read back out of byPath;
	// a second path-keyed map would be the same map under another name.
	roots []string
}

func newProjectIndex(s *store.Store) (*projectIndex, error) {
	projects, err := s.ListProjects()
	if err != nil {
		return nil, err
	}
	index := &projectIndex{
		byPath: make(map[string]store.Project, len(projects)),
		roots:  make([]string, 0, len(projects)),
	}
	for _, project := range projects {
		path := filepath.Clean(project.Path)
		index.byPath[path] = project
		index.roots = append(index.roots, path)
	}
	// Longest first so a nested project wins over the repo containing it.
	sort.Slice(index.roots, func(i, j int) bool {
		return len(index.roots[i]) > len(index.roots[j])
	})
	return index, nil
}

func (p *projectIndex) decorate(row *Row) {
	workspace := filepath.Clean(row.ProjectPath)
	if project, ok := p.byPath[workspace]; ok {
		row.ProjectID = project.ID
		row.ProjectLabel = project.Name
		row.KnownProject = true
		return
	}
	for _, root := range p.roots {
		if workspace == root || strings.HasPrefix(workspace, root+string(filepath.Separator)) {
			project := p.byPath[root]
			row.ProjectID = project.ID
			row.ProjectLabel = project.Name
			row.KnownProject = true
			return
		}
	}
	// No project yet: the label is what the import will name the one it
	// creates, so the row reads the same before and after.
	row.ProjectLabel = filepath.Base(workspace)
}
