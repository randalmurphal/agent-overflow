package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/sessionimport"
	"agent-overflow/internal/slicesx"
)

// --- Session import: provider sessions on disk → AO threads ---
//
// The whole feature is a file read and a SQLite write. Nothing here spawns a
// process, and nothing here generates a title through a provider CLI: an
// import replays history that already happened, and paying for a model turn
// per imported thread would turn "import my sessions" into a billed
// operation.
//
// Every method here carries //ao:scope threads:operate: they read the user's
// provider homes and hand back file paths and prompt text from them, and they
// write threads. A session granted that scope may run an import from off-host,
// which is what the grant means.

// ImportScanRequest is how a caller asks for the listing.
//
// There is deliberately no provider or workspace filter on the wire. The
// modal narrows its own rows client-side — it already holds them all, and a
// server-side filter would make the answer depend on which filter last ran
// while the row set behind it is identical. `sessionimport.Filter` still
// exists for the orchestrator's own tests; nothing reaches it from here.
type ImportScanRequest struct {
	// ForceRefresh bypasses the TTL cache. The modal's Refresh button.
	ForceRefresh bool `json:"forceRefresh,omitempty"`
}

// ImportScanResult is one listing of importable sessions.
type ImportScanResult struct {
	// Providers carries one entry per scanned provider, healthy or not:
	// "Codex has no sessions" and "Codex could not be read" look identical
	// in a row list, and only the second is something the user can act on.
	Providers []ImportProviderStatus `json:"providers"`
	Rows      []ImportableSession    `json:"rows"`
	// ScannedAt is when the disk was read, in epoch ms — which is older than
	// the response whenever the cache served it.
	ScannedAt int64 `json:"scannedAt"`
}

// ImportProviderStatus is one provider's availability for a scan.
type ImportProviderStatus struct {
	Provider  string `json:"provider"`
	Available bool   `json:"available"`
	// Error is user-facing prose, empty when Available.
	Error string `json:"error"`
	// SkippedCount counts session files the reader could not use.
	SkippedCount int `json:"skippedCount"`
}

// ImportableSession is one provider session AO does not already have.
type ImportableSession struct {
	// ID is the opaque row key ImportSessions accepts back. Minted by the
	// backend; the frontend must never compose one.
	ID           string `json:"id"`
	Provider     string `json:"provider"`
	SessionID    string `json:"sessionId"`
	Title        string `json:"title"`
	ProjectPath  string `json:"projectPath"`
	ProjectID    string `json:"projectId"`
	ProjectLabel string `json:"projectLabel"`
	GitBranch    string `json:"gitBranch,omitempty"`
	// CreatedAt / LastActivityAt are epoch ms, like every AO wire timestamp.
	CreatedAt      int64  `json:"createdAt"`
	LastActivityAt int64  `json:"lastActivityAt"`
	SizeBytes      int64  `json:"sizeBytes"`
	SubagentCount  int    `json:"subagentCount"`
	SourcePath     string `json:"sourcePath"`
	KnownProject   bool   `json:"knownProject"`
	// Origin is the provider's own origin marker, verbatim — Claude's
	// `entrypoint` (`cli`, `sdk-cli`, `agent-overflow`) or Codex's
	// `originator` (`codex_cli`, `agent_overflow`) — and "" when the session
	// file carries none. Display only.
	Origin string `json:"origin"`
	// RanInAgentOverflow is true when Origin is THIS app's marker for the
	// row's provider. The backend owns the comparison so the two spellings
	// never reach the frontend. These rows are still listed: the modal hides
	// them behind a toggle, which needs both the row and the flag.
	RanInAgentOverflow bool `json:"ranInAgentOverflow"`
	// ImportedFrom is set when Codex's own external-import ledger says this
	// Codex session is a conversation Codex imported from ANOTHER coding
	// agent. Absent on Claude rows and on ordinary Codex sessions.
	ImportedFrom *ImportOrigin `json:"importedFrom,omitempty"`
	// Warnings are about THIS row, not the scan.
	Warnings []string `json:"warnings,omitempty"`
}

// ImportOrigin says where a session came from before the provider that now
// holds it. Display only — nothing in the import path branches on it.
type ImportOrigin struct {
	// Agent is the source coding agent: `claude-code`, `cursor`, or "" when
	// the ledger's path shape matches neither. The frontend labels the known
	// ones and shows a generic badge otherwise; it must not compose its own
	// list, since the backend derives this.
	Agent string `json:"agent"`
	// SourcePath is the file the other agent's session lived in. It may no
	// longer exist.
	SourcePath string `json:"sourcePath"`
	// SourceID is the source agent's own session id, when its layout encodes
	// one. Empty otherwise.
	SourceID string `json:"sourceId"`
	// ImportedAt is epoch ms, like every AO wire timestamp.
	ImportedAt int64 `json:"importedAt"`
	// DuplicateOfThreadID names an AO thread that already holds this same
	// conversation, imported from the source agent directly. The row is still
	// offered — both provider sessions exist and both resume — so this is a
	// label, not a filter.
	DuplicateOfThreadID string `json:"duplicateOfThreadId,omitempty"`
}

// ImportUpdateStatus is what a check of one imported thread found.
type ImportUpdateStatus struct {
	ThreadID string `json:"threadId"`
	// Status is one of sessionimport's Update* constants: up-to-date,
	// updates-available, diverged-local, source-missing, source-diverged,
	// not-imported.
	Status   string `json:"status"`
	NewItems int    `json:"newItems"`
	NewTurns int    `json:"newTurns"`
	// RestoresModelProfile is true when apply will restore the model settings
	// recorded in the provider session, with or without new history rows.
	RestoresModelProfile bool `json:"restoresModelProfile"`
	// Detail is user-facing prose.
	Detail string `json:"detail,omitempty"`
}

// ImportUpdateResult is what a refresh actually wrote.
type ImportUpdateResult struct {
	AppliedItems         int  `json:"appliedItems"`
	AppliedTurns         int  `json:"appliedTurns"`
	RestoredModelProfile bool `json:"restoredModelProfile"`
}

const (
	sessionImportStatusImported = sessionimport.ImportStatusImported
	sessionImportStatusFailed   = sessionimport.ImportStatusFailed
	sessionImportStatusSkipped  = sessionimport.ImportStatusSkipped
)

// ImportSessionsRequest names the rows to import by their opaque scan ids.
type ImportSessionsRequest struct {
	IDs []string `json:"ids"`
}

// ImportRunHandle is what ImportSessions returns immediately; everything else
// arrives on eventchan.SessionImportProgress.
type ImportRunHandle struct {
	ImportID string `json:"importId"`
	Total    int    `json:"total"`
}

// SessionImportProgressEvent is one frame of an import run.
//
// One channel carries both the per-session frames and the terminal one, so a
// listener that sees Done has seen everything: a second "finished" channel
// could arrive before the last per-session frame and leave the UI stuck.
type SessionImportProgressEvent struct {
	ImportID string `json:"importId"`
	// Completed counts frames reported so far, 0..Total, monotonic. On a
	// cancelled run the terminal frame stops short of Total.
	Completed int `json:"completed"`
	Total     int `json:"total"`
	// ID is the row this frame reports on; absent on the terminal frame.
	ID string `json:"id,omitempty"`
	// Status is imported | failed | skipped; absent on the terminal frame.
	Status string `json:"status,omitempty"`
	// ThreadIDs names the thread the session created. It is a slice because
	// the UI applies row additions in one shape; the importer enforces at most
	// one id per provider session.
	ThreadIDs []string `json:"threadIds,omitempty"`
	// Error is user-facing prose.
	Error string `json:"error,omitempty"`
	// Done is true exactly once, on the final frame.
	Done bool `json:"done,omitempty"`
}

// ListImportableSessions scans the provider homes for sessions AO does not
// already have.
//
// Cached for sessionimport.ScanTTL; the request's ForceRefresh bypasses it. A
// provider whose home cannot be read is reported in Providers and does not
// fail the call — a broken Codex home must not take Claude's sessions away.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) ListImportableSessions(req ImportScanRequest) (ImportScanResult, error) {
	scan, err := a.sessionImporter().List(req.ForceRefresh)
	if err != nil {
		return ImportScanResult{}, err
	}
	return wireImportScanResult(scan), nil
}

// CheckThreadImportUpdates reports whether the provider session behind an
// imported thread has new history or model settings that can be restored.
//
// Read-only: it builds the rows a refresh WOULD write (the writer is
// store-pure) so the counts it reports are exact rather than estimated, and
// so a tail that cannot be converted is refused here rather than half-applied
// by ImportThreadUpdates.
//
// It takes the SAME thread lock the apply does. PlanUpdate reads the thread's
// cursor, its turn ids, and its last item position and then decides whether
// the thread diverged; a send landing between those reads would make the
// answer describe a thread that no longer exists — reporting "3 new items" for
// a thread that is, by the time the user clicks, diverged-local.
//
//ao:scope threads:operate
func (a *App) CheckThreadImportUpdates(threadID string) (ImportUpdateStatus, error) {
	update, err := a.sessionImporter().CheckThreadUpdates(threadID)
	if err != nil {
		return ImportUpdateStatus{}, err
	}
	return ImportUpdateStatus{
		ThreadID:             update.ThreadID,
		Status:               update.Status,
		NewItems:             update.NewItems,
		NewTurns:             update.NewTurns,
		RestoresModelProfile: update.RestoresModelProfile,
		Detail:               update.Detail,
	}, nil
}

// ImportThreadUpdates applies newer source history and/or restores recorded
// model settings. A history append advances the source cursor; a profile-only
// repair leaves it exactly where it was.
//
// It re-plans rather than trusting a status the caller checked earlier: the
// file and the thread can both have moved since, and a stale plan would
// append rows against indices the thread has since allocated.
//
//ao:scope threads:operate
func (a *App) ImportThreadUpdates(threadID string) (ImportUpdateResult, error) {
	result, err := a.sessionImporter().ApplyThreadUpdates(threadID)
	if err != nil {
		return ImportUpdateResult{}, err
	}
	return ImportUpdateResult{
		AppliedItems:         result.Items,
		AppliedTurns:         result.Turns,
		RestoredModelProfile: result.RestoredModelProfile,
	}, nil
}

// ImportSessions starts an import run and returns immediately. Progress
// arrives on eventchan.SessionImportProgress, one frame per session plus a
// terminal frame.
//
// Duplicate ids in one request collapse: the scan's dedup happens BEFORE a
// run, so importing one row twice in the same run would create the threads
// twice.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) ImportSessions(req ImportSessionsRequest) (ImportRunHandle, error) {
	handle, err := a.sessionImporter().Start(req.IDs)
	if err != nil {
		return ImportRunHandle{}, err
	}
	return ImportRunHandle{ImportID: handle.ImportID, Total: handle.Total}, nil
}

// CancelSessionImport stops the named run. The run still emits its terminal
// frame, with Completed short of Total — a listener never has to infer the
// end from silence.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) CancelSessionImport(importID string) error {
	return a.sessionImporter().Cancel(importID)
}

func wireSessionImportProgress(frame sessionimport.ProgressEvent) SessionImportProgressEvent {
	return SessionImportProgressEvent{
		ImportID: frame.ImportID, Completed: frame.Completed, Total: frame.Total,
		ID: frame.ID, Status: frame.Status, ThreadIDs: frame.ThreadIDs,
		Error: frame.Error, Done: frame.Done,
	}
}

func (a *App) stopSessionImports() {
	a.sessionImporter().Stop()
}

// sessionImportDeps resolves the provider homes ONCE and hands them down.
//
// Which home is in play is an app-level decision — the credential-home
// override is what the harness and the tests point at — so nothing under
// internal/ resolves it. CLAUDE_CONFIG_DIR and CODEX_HOME are deliberately
// ignored: AO clears CODEX_HOME on every spawn and pins its own Claude home,
// so honouring them here would list sessions this app could never resume.
//
// A home directory that does not exist is passed as empty, which the scan
// reports as "this host has no Claude/Codex home" rather than as a read
// failure — the difference between an absent provider and a broken one.
//
// Inside a test binary the os.UserHomeDir() fallthrough is refused outright,
// the way resolveTextGenerationExecutor refuses a real CLI: a fixture that
// left the override empty would list — and IMPORT — the developer's real
// ~/.claude and ~/.codex sessions into its throwaway store, and the walk
// alone reads gigabytes of private transcripts. A fixture that wants import
// behavior points credentialHomeOverride at a temp home (importHome.attach).
// testing.Testing() is false in every production process, so ordinary runs
// pay nothing.
func (a *App) sessionImportDeps() (sessionimport.Deps, error) {
	if a.store == nil {
		return sessionimport.Deps{}, fmt.Errorf("session import: no store")
	}
	home := a.credentialHomeOverride
	if home == "" {
		if testing.Testing() {
			return sessionimport.Deps{}, errors.New(
				"tests must not scan the real provider homes; set app.credentialHomeOverride to a temp home")
		}
		resolved, err := a.providerHome()
		if err != nil {
			return sessionimport.Deps{}, fmt.Errorf("session import: %w", err)
		}
		home = resolved
	}
	return sessionimport.Deps{
		Store:             a.store,
		ClaudeProjectsDir: providerHomeIfPresent(filepath.Join(home, ".claude", "projects")),
		CodexHome:         providerHomeIfPresent(filepath.Join(home, ".codex")),
	}, nil
}

// providerHomeIfPresent returns path when it is a directory, else "" — which
// is how sessionimport.Deps spells "this host has no such provider home".
func providerHomeIfPresent(path string) string {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return ""
	}
	return path
}

// sessionImporter lazy-builds the coordinator so a bare App test fixture only
// needs to wire the dependencies its call path reaches.
func (a *App) sessionImporter() *sessionimport.Manager {
	a.sessionImport.once.Do(func() {
		if a.sessionImport.manager == nil {
			a.sessionImport.manager = a.newSessionImportManager(sessionimport.ManagerConfig{})
		}
	})
	return a.sessionImport.manager
}

func (a *App) newSessionImportManager(config sessionimport.ManagerConfig) *sessionimport.Manager {
	config.Context = a.lifeCtx
	config.ResolveDeps = a.sessionImportDeps
	config.ValidateStart = func() error {
		if a.store == nil {
			return fmt.Errorf("import sessions: no store")
		}
		return nil
	}
	config.LockThread = a.threadLocks().Lock
	config.ShutdownError = ErrShuttingDown
	config.EmitProgress = func(frame sessionimport.ProgressEvent) {
		a.emit(eventchan.SessionImportProgress, wireSessionImportProgress(frame))
	}
	return sessionimport.NewManager(config)
}

func wireImportScanResult(scan sessionimport.CachedScan) ImportScanResult {
	providers := make([]ImportProviderStatus, 0, len(scan.Result.Providers))
	for _, status := range scan.Result.Providers {
		providers = append(providers, ImportProviderStatus{
			Provider:     status.Provider,
			Available:    status.Available,
			Error:        status.Error,
			SkippedCount: status.SkippedCount,
		})
	}
	rows := make([]ImportableSession, 0, len(scan.Result.Rows))
	for _, row := range scan.Result.Rows {
		rows = append(rows, ImportableSession{
			ID:                 row.ID,
			Provider:           row.Provider,
			SessionID:          row.SessionID,
			Title:              row.Title,
			ProjectPath:        row.ProjectPath,
			ProjectID:          row.ProjectID,
			ProjectLabel:       row.ProjectLabel,
			GitBranch:          row.GitBranch,
			CreatedAt:          row.CreatedAt,
			LastActivityAt:     row.LastActivityAt,
			SizeBytes:          row.SizeBytes,
			SubagentCount:      row.SubagentCount,
			SourcePath:         row.SourcePath,
			KnownProject:       row.KnownProject,
			Origin:             row.Origin,
			RanInAgentOverflow: row.RanInAgentOverflow,
			ImportedFrom:       wireImportOrigin(row.ImportedFrom),
			// Safe to alias: Get returns a deep copy of the cached scan.
			Warnings: row.Warnings,
		})
	}
	return ImportScanResult{
		Providers: slicesx.OrEmpty(providers),
		Rows:      slicesx.OrEmpty(rows),
		ScannedAt: scan.ScannedAt,
	}
}

// wireImportOrigin copies the scan's origin onto the wire shape. A fresh
// struct rather than an aliased pointer: the cached scan hands out a deep
// copy, and a shared pointer would quietly reintroduce the aliasing the copy
// exists to prevent.
func wireImportOrigin(origin *sessionimport.ExternalImportOrigin) *ImportOrigin {
	if origin == nil {
		return nil
	}
	return &ImportOrigin{
		Agent:               origin.Agent,
		SourcePath:          origin.SourcePath,
		SourceID:            origin.SourceSessionID,
		ImportedAt:          origin.ImportedAt,
		DuplicateOfThreadID: origin.DuplicateOfThreadID,
	}
}
