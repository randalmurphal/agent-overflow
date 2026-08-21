package sessionimport

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
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

// originRuneCap bounds Row.Origin. The marker is provider-file text this
// package neither writes nor validates, it is cached and pushed over the wire
// once per row, and the frontend renders it into a DOM title — so a session
// file carrying a megabyte under `entrypoint` must not become a megabyte in
// every scan. 64 runes is far past every real marker (`agent-overflow`,
// `sdk-cli`, `codex_cli`, `forge_desktop`), and the cut is deliberately
// ellipsis-free: this is an identifier, not prose.
const originRuneCap = 64

// Deps is everything the orchestrator needs from outside itself.
//
// Both provider homes are INJECTED, never resolved here. Which Claude/Codex
// home is in play is an app-level decision (credential-home override, WSL
// relocation) and a library guess would silently list sessions the app
// cannot resume. See the root AGENTS.md §Permanent invariants.
type Deps struct {
	Store *store.Store
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
	// WorkspacePath limits rows to sessions whose recorded cwd is exactly
	// this path, matched verbatim. It asks about the cwd, not about the
	// project the cwd resolves to: a repository's own root and each of its
	// worktrees are different answers here, which is the distinction a caller
	// naming one workspace is making.
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
	ID        string
	Provider  string
	SessionID string
	// ParentSessionID is the provider session this session was explicitly
	// forked from. It is provenance, not a reason to suppress either row:
	// both ids are independently resumable conversations.
	ParentSessionID string
	Title           string
	ProjectPath     string
	ProjectID       string
	ProjectLabel    string
	GitBranch       string
	CreatedAt       int64
	LastActivityAt  int64
	SizeBytes       int64
	SubagentCount   int
	SourcePath      string
	// KnownProject is true when AO already has a project row covering this
	// session's workspace, so importing it will not create one.
	KnownProject bool
	// Origin is the provider's own origin marker for this session: Claude's
	// `entrypoint` (`agent-overflow`, `cli`, `sdk-cli`, …) or Codex's
	// `originator` (`agent_overflow`, `codex_cli`, …). Empty when the file
	// carries none, and bounded to originRuneCap — the value is the provider
	// file's text, not ours.
	Origin string
	// RanInAgentOverflow is true when Origin is THIS app's marker for the
	// row's provider (`provider.ClaudeEntrypointOrigin` /
	// `provider.CodexClientOrigin`). It is decided here so the two wire
	// spellings never reach a caller, and it is decided BEFORE the cap, so no
	// length bound can move it. Rows are never filtered on it — the listing
	// offers them and the frontend hides them behind a toggle.
	RanInAgentOverflow bool
	// Warnings are user-facing prose about THIS row, not the scan.
	Warnings []string
	// IndexedProfile is a provider-index fallback carried only for import.
	// Codex's rollout profile remains authoritative when present.
	IndexedProfile importir.ModelProfile
	// ImportedFrom is set when Codex's own external-import ledger
	// (`<codexHome>/external_agent_session_imports.json`) says THIS Codex
	// thread is a conversation Codex imported from another coding agent.
	// Always nil for Claude rows: AO reads Codex's ledger, and Claude Code
	// keeps no equivalent.
	//
	// It is the only place that provenance survives — the imported rollout is
	// an ordinary Codex rollout whose originator says `codex_cli` — so
	// without it the picker offers a Claude Code conversation as a Codex
	// session with nothing to say where it came from.
	ImportedFrom *ExternalImportOrigin
}

// ExternalImportOrigin is where a Codex session came from before it was a
// Codex session.
type ExternalImportOrigin struct {
	// Agent is the source coding agent (`claude-code`, `cursor`), or "" when
	// the ledger's path shape matches neither. Derived by the rollout reader
	// from the source path, because the ledger records no agent field.
	Agent string
	// SourcePath is the file Codex read, as the ledger recorded it. It may
	// name a file that no longer exists.
	SourcePath string
	// SourceSessionID is the SOURCE agent's own session id, when its layout
	// encodes one (Claude Code: the `.jsonl` stem). Empty otherwise.
	SourceSessionID string
	// ImportedAt is when Codex recorded the import, epoch ms.
	ImportedAt int64
	// DuplicateOfThreadID names the AO thread that already holds this same
	// conversation, imported from the SOURCE agent directly rather than
	// through Codex. Empty when AO does not have it.
	//
	// The row is still offered. Two provider sessions genuinely exist and
	// both are independently resumable — the Codex copy resumes in Codex —
	// so suppressing it would take away a real choice. Naming the duplicate
	// lets the picker say so instead.
	DuplicateOfThreadID string
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
// Two subtractions make the result safe to import wholesale, which is what
// an "Import All" button needs:
//
//  1. Sessions AO already knows (`ListImportedSessionRefs` — a live
//     session_ref, an unresumed fork's pending ref, or an earlier import's
//     recorded source). This is what makes pressing Import All twice a
//     no-op instead of a duplicate.
//  2. Subagent / spawned-child sessions. Those are already excluded inside
//     each provider's lister, which is where the provider-specific test for
//     "is this a child" belongs.
//
// Explicit provider forks are NOT subtracted. A fork is a new provider
// session with its own resume id and a shared historical prefix; suppressing
// its ancestor loses an independently continuable conversation. Row carries
// the provider parent id so import can reproduce that lineage in AO.
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
	projects, err := newProjectIndex(ctx, d.Store)
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
	for i, row := range result.Rows {
		// The first row naming a given cwd pays for its resolution — a stat
		// and a repository walk, either of which can be a stale network path.
		// Same sampling as the session-meta fan-out; ctx.Err() is sticky, so
		// the sampled check and a later one agree.
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return ScanResult{}, err
			}
		}
		if filter.WorkspacePath != "" && row.ProjectPath != filter.WorkspacePath {
			continue
		}
		projects.stamp(&row)
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
	ctx context.Context, d Deps, providerName string, known map[store.ProviderSessionRef]string,
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

func scanClaude(ctx context.Context, d Deps, known map[store.ProviderSessionRef]string) ([]Row, []importir.Warning, error) {
	home := strings.TrimSpace(d.ClaudeProjectsDir)
	if home == "" {
		return nil, nil, fmt.Errorf("no Claude home was found on this machine")
	}
	sessions, warnings, err := claudesessions.List(ctx, claudesessions.Options{ProjectsDir: home})
	if err != nil {
		return nil, nil, fmt.Errorf("Claude sessions could not be read: %w", err)
	}

	rows := make([]Row, 0, len(sessions))
	for _, session := range sessions {
		key := store.ProviderSessionRef{Provider: ProviderClaude, SessionID: session.SessionID}
		if _, taken := known[key]; taken {
			continue
		}
		origin := session.Entrypoint
		rows = append(rows, Row{
			ID:                 RowKey(ProviderClaude, session.SessionID),
			Provider:           ProviderClaude,
			SessionID:          session.SessionID,
			ParentSessionID:    strings.TrimSpace(session.ForkedFromSessionID),
			Title:              session.Title,
			ProjectPath:        session.ProjectPath,
			GitBranch:          session.GitBranch,
			CreatedAt:          session.CreatedAt,
			LastActivityAt:     session.LastActivityAt,
			SizeBytes:          session.SizeBytes,
			SubagentCount:      session.SubagentCount,
			SourcePath:         session.Path,
			Origin:             capOrigin(origin),
			RanInAgentOverflow: origin == provider.ClaudeEntrypointOrigin,
		})
	}
	return rows, warnings, nil
}

func scanCodex(ctx context.Context, d Deps, known map[store.ProviderSessionRef]string) ([]Row, []importir.Warning, error) {
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
		key := store.ProviderSessionRef{Provider: ProviderCodex, SessionID: session.ThreadID}
		if _, taken := known[key]; taken {
			continue
		}
		candidates = append(candidates, session)
	}
	metas, err := codexSessionMetas(ctx, candidates)
	if err != nil {
		return nil, nil, err
	}
	// One bounded read of one small JSON file for the whole scan, not one per
	// row. A home that never imported from another agent has no file and no
	// warning; a broken one costs the origin labels and nothing else.
	imports, ledgerWarnings := rollout.ReadExternalImportLedger(home)
	warnings = append(warnings, ledgerWarnings...)
	rows := make([]Row, 0, len(candidates))
	for _, session := range candidates {
		meta := metas[session.ThreadID]
		origin := strings.TrimSpace(meta.Originator)
		rows = append(rows, Row{
			ID:                 RowKey(ProviderCodex, session.ThreadID),
			Provider:           ProviderCodex,
			SessionID:          session.ThreadID,
			ParentSessionID:    strings.TrimSpace(meta.ForkedFromID),
			Title:              session.Title,
			ProjectPath:        session.Cwd,
			GitBranch:          session.GitBranch,
			CreatedAt:          session.CreatedAt,
			LastActivityAt:     session.LastActivityAt,
			SizeBytes:          session.SizeBytes,
			SourcePath:         session.RolloutPath,
			Origin:             capOrigin(origin),
			RanInAgentOverflow: origin == provider.CodexClientOrigin,
			IndexedProfile: importir.ModelProfile{
				Model:           session.Model,
				ReasoningEffort: session.ReasoningEffort,
			},
			ImportedFrom: externalImportOrigin(imports, known, session.ThreadID),
		})
	}
	return rows, warnings, nil
}

// Clone returns an independent copy. Every field is a value, so a fresh
// pointer to a struct copy is a full deep copy — which is what the scan
// cache's "the result is a deep copy" contract requires of a pointer field.
func (o *ExternalImportOrigin) Clone() *ExternalImportOrigin {
	if o == nil {
		return nil
	}
	copied := *o
	return &copied
}

// externalImportOrigin resolves one Codex thread's external-import provenance,
// including whether AO already holds the SAME conversation imported straight
// from the source agent.
//
// The duplicate check reuses the scan's existing `known` set — the same map
// that already subtracts sessions AO has — so it costs no extra read. It asks
// about the SOURCE session id under the SOURCE provider, which is a different
// question from the Codex-side dedup above: that one asks whether AO has this
// Codex thread, this one asks whether AO has the conversation inside it.
func externalImportOrigin(
	imports map[string]rollout.ExternalImportRecord,
	known map[store.ProviderSessionRef]string,
	threadID string,
) *ExternalImportOrigin {
	record, imported := imports[threadID]
	if !imported {
		return nil
	}
	origin := &ExternalImportOrigin{
		Agent:           record.Agent,
		SourcePath:      record.SourcePath,
		SourceSessionID: record.SourceSessionID,
		ImportedAt:      record.ImportedAt,
	}
	if record.Agent == rollout.ExternalImportAgentClaude && record.SourceSessionID != "" {
		origin.DuplicateOfThreadID = known[store.ProviderSessionRef{
			Provider:  ProviderClaude,
			SessionID: record.SourceSessionID,
		}]
	}
	return origin
}

// codexSessionMetaReaders bounds the session-meta fan-out. Each worker does
// one bounded head read of one rollout; the work is IO-latency bound, so a
// handful of readers hides the per-file open/seek cost of a home holding
// hundreds of sessions without turning a browse into a disk storm.
const codexSessionMetaReaders = 8

// codexSessionMetas reads the accepted `session_meta` of every candidate,
// keyed by thread id.
//
// It is ONE bounded head read per surviving candidate (ReadSessionMeta stops
// at the first matching session_meta), and it is the only read of these files
// the scan does — both things it answers ride on it: fork provenance (which
// Codex records in the rollout, not the thread index) and the originator that
// says whether AO itself ran the session. A file with no readable meta
// contributes neither, which is the honest degrade for both: the session is
// still offered but its unresolved lineage stays absent, and an unknown
// originator reads as "not ours". ParentThreadID is deliberately not
// used as fork provenance: Codex uses it for spawned/subagent relationships,
// while ForkedFromID identifies an explicit user fork.
func codexSessionMetas(
	ctx context.Context, candidates []rollout.SessionInfo,
) (map[string]rollout.SessionMeta, error) {
	metas := make(map[string]rollout.SessionMeta, len(candidates))
	if len(candidates) == 0 {
		return metas, nil
	}
	workers := min(codexSessionMetaReaders, len(candidates))

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
				metas[session.ThreadID] = meta
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		// A cancelled fan-out has incomplete origin and lineage metadata; do
		// not publish a partially decorated scan as if it were complete.
		return nil, err
	}
	return metas, nil
}

// capOrigin bounds a provider-authored origin marker at extraction, which is
// the only place it is guaranteed to happen once: past here the value is
// cached, wired to the frontend per row, and rendered. Callers derive
// RanInAgentOverflow from the UNCAPPED value first, so the bound can never
// change the answer.
func capOrigin(origin string) string {
	runes := []rune(origin)
	if len(runes) <= originRuneCap {
		return origin
	}
	return string(runes[:originRuneCap])
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
