package main

import (
	"encoding/base64"
	"fmt"
	"path/filepath"

	"agent-overflow/internal/design"
	"agent-overflow/internal/store"
)

type designSessionConfig struct {
	Prompt     string
	MCPServers map[string]any
}

// designSessionConfig is split into two phases so the watcher + MCP
// token are not allocated until after stopExistingSessionLocked has
// torn down any prior session for the same thread. Without that
// ordering, a stop on the old session would also stop the freshly
// allocated watcher, leaving the new session without one.
//
// Phase 1 (this function): EnsureThread (idempotent mkdir), system
// prompt load. Cheap, safe to do early so the caller can decide
// whether to even attempt the start.
func (a *App) designSessionConfig(thread store.Thread) (designSessionConfig, error) {
	if thread.Mode != "design" {
		return designSessionConfig{}, nil
	}
	if a.designWorkdir == nil {
		return designSessionConfig{}, fmt.Errorf("design workdir manager unavailable")
	}
	if err := a.designWorkdir.EnsureThread(thread.ID); err != nil {
		return designSessionConfig{}, err
	}
	return designSessionConfig{
		Prompt: design.LoadDesignSystemPrompt(a.configDir, thread.WorkspacePath),
	}, nil
}

// activateDesignSession is phase 2: run AFTER stopExistingSessionLocked
// so the watcher and MCP token belong to the new session, not a
// just-torn-down predecessor. Returns the MCP server map the provider
// startup needs.
func (a *App) activateDesignSession(thread store.Thread) (map[string]any, error) {
	if thread.Mode != "design" {
		return nil, nil
	}
	if err := a.startDesignWatcher(thread.ID); err != nil {
		return nil, err
	}
	servers, err := a.designMCPConfigForThread(thread)
	if err != nil {
		// Roll back the watcher if MCP registration fails so we don't
		// leak it to a session that never started.
		a.stopDesignWatcher(thread.ID)
		return nil, err
	}
	return servers, nil
}

// designMCPConfigForThread returns the MCP server config block for the
// thread's provider. Both providers use the same Codex HTTP MCP server
// (Codex consumes it inline, Claude consumes it via --mcp-config).
func (a *App) designMCPConfigForThread(thread store.Thread) (map[string]any, error) {
	if a.designMCP == nil {
		return nil, fmt.Errorf("design MCP server unavailable")
	}
	return a.designMCP.RegisterThread(thread.ID)
}

func (a *App) teardownDesignThread(threadID string) {
	a.stopDesignWatcher(threadID)
	if a.reactor != nil {
		a.reactor.TeardownThread(threadID)
	}
	if a.designMCP != nil {
		a.designMCP.UnregisterThread(threadID)
	}
}

func (a *App) startDesignWatcher(threadID string) error {
	if a.designWorkdir == nil {
		return fmt.Errorf("design workdir manager unavailable")
	}
	mainPath, err := a.designWorkdir.MainPath(threadID)
	if err != nil {
		return err
	}
	threadDir := filepath.Dir(mainPath)
	a.designWatchersMu.Lock()
	defer a.designWatchersMu.Unlock()
	if existing, ok := a.designWatchers[threadID]; ok {
		// Idempotent: a redundant call (e.g. session restart) reuses
		// the live watcher instead of leaking a duplicate goroutine.
		_ = existing
		return nil
	}
	w := design.NewWatcher(threadID, threadDir, a.handleDesignWatcherEvent, design.WatcherOptions{})
	a.designWatchers[threadID] = w
	return nil
}

func (a *App) stopDesignWatcher(threadID string) {
	a.designWatchersMu.Lock()
	w, ok := a.designWatchers[threadID]
	if ok {
		delete(a.designWatchers, threadID)
	}
	a.designWatchersMu.Unlock()
	if ok {
		w.Stop()
	}
}

// handleDesignWatcherEvent is called on the watcher's run goroutine.
// It records iframe-ready activity in the diagnostic buffer's
// settle-window tracker and emits a thread-aware reload event so the
// frontend cache-busts the iframe / refreshes panels.
func (a *App) handleDesignWatcherEvent(ev design.WatchEvent) {
	if a.designDiagnostics != nil {
		// MarkActivity tells the diagnostic buffer the iframe is
		// likely about to load + report; Drain on get_design_diagnostics
		// will then block briefly to avoid stale-empty results.
		a.designDiagnostics.MarkActivity(ev.ThreadID)
	}
	switch ev.Subject {
	case design.WatchSubjectMain:
		a.emit("design:reload-main", map[string]any{
			"threadId": ev.ThreadID,
		})
	case design.WatchSubjectOptions:
		a.emit("design:options-update", map[string]any{
			"threadId": ev.ThreadID,
			"setId":    ev.SetID,
		})
	case design.WatchSubjectSnapshots:
		a.emit("design:snapshots-update", map[string]any{
			"threadId": ev.ThreadID,
		})
	}
}

// ListDesignSnapshots returns persisted snapshot metadata for a thread,
// newest first.
func (a *App) ListDesignSnapshots(threadID string) ([]design.Snapshot, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store unavailable")
	}
	return a.store.ListDesignSnapshots(threadID)
}

// ListDesignOptions returns the option ids inside `options/{setId}/`
// for a design thread, sorted lexically. Used by the frontend's
// options panel to hydrate the iframe grid when the watcher fires
// design:options-update — the agent writes the option directories,
// the watcher reports the set, and this binding enumerates the
// options inside it.
func (a *App) ListDesignOptions(threadID, setID string) ([]string, error) {
	if a.designWorkdir == nil {
		return nil, fmt.Errorf("design workdir manager unavailable")
	}
	return a.designWorkdir.ListOptions(threadID, setID)
}

// DesignOptionSet is the wire shape for the "currently active
// options picker" projection. Returned by LatestDesignOptionSet for
// frontend hydration on pane mount / refresh.
type DesignOptionSet struct {
	SetID     string   `json:"setId"`
	OptionIDs []string `json:"optionIds"`
}

// LatestDesignOptionSet returns the most recent option set under the
// thread's `options/` dir that has at least one option containing
// `index.html` and no `.picked` marker — i.e. the picker state the
// frontend should show on a freshly mounted design pane. Returns nil
// when no such set exists (the user has either picked every set or
// the agent has not generated any).
//
// This is the load-bearing piece of the persistence story: refresh
// or app restart re-derives picker state from the per-thread workdir
// rather than from in-memory svelte state. The on-disk layout is
// already durable, so no SQLite row needs to mirror the picker — the
// `.picked` marker file is the dismissal record.
func (a *App) LatestDesignOptionSet(threadID string) (*DesignOptionSet, error) {
	if a.designWorkdir == nil {
		return nil, fmt.Errorf("design workdir manager unavailable")
	}
	setID, optionIDs, err := a.designWorkdir.LatestUnpickedOptionSet(threadID)
	if err != nil {
		return nil, err
	}
	if setID == "" {
		return nil, nil
	}
	return &DesignOptionSet{SetID: setID, OptionIDs: optionIDs}, nil
}

// DismissDesignOptionSet writes the `.picked` marker file into
// `options/{setId}/` so a refresh / restart does not re-hydrate the
// picker for a set the user has already resolved. The option
// directories themselves stay on disk so the agent can read the
// picked option's files via absolute path when applying the chosen
// direction to main/.
//
// Called from the frontend's DesignOptionsPanel after SendMessage
// for the structured `option_chosen` payload returns successfully.
func (a *App) DismissDesignOptionSet(threadID, setID string) error {
	if a.designWorkdir == nil {
		return fmt.Errorf("design workdir manager unavailable")
	}
	return a.designWorkdir.MarkOptionSetPicked(threadID, setID)
}

// designWorkDirOverride returns the per-thread workdir to use as the
// provider subprocess's CWD for design threads, or "" when the
// session should keep the thread's WorkspacePath. The bundled system
// prompt teaches the agent that `main/`, `options/`, and `snapshots/`
// are sibling directories in its CWD; without this override the
// agent's Read/Edit/Write would resolve against the project repo
// instead, the file watcher would never fire on its writes, and
// `pwd` would name the wrong place.
//
// Extracted from startSessionNow so the override is unit-testable
// without booting a full session.
func (a *App) designWorkDirOverride(t store.Thread) (string, error) {
	if t.Mode != "design" || a.designWorkdir == nil {
		return "", nil
	}
	return a.designWorkdir.ThreadDir(t.ID)
}

// EnsureDesignWorkdir materialises the per-thread {main,options,
// snapshots}/ layout (and seeds a placeholder index.html) for a design
// thread. The frontend's DesignPreviewPanel calls this when the iframe
// is about to mount so the file server has something to serve before
// the agent's first edit. Idempotent — safe to call on every mount.
func (a *App) EnsureDesignWorkdir(threadID string) error {
	if a.designWorkdir == nil {
		return fmt.Errorf("design workdir manager unavailable")
	}
	if err := a.designWorkdir.EnsureThread(threadID); err != nil {
		return fmt.Errorf("ensure workdir for thread %q (base=%q): %w",
			threadID, a.designWorkdir.BaseDir(), err)
	}
	return nil
}

// DesignWorkdirInfo describes the on-disk state of a design thread's
// main/ directory: the absolute path and a flat manifest of the
// regular files directly inside. Used by the "Send to thread" flow
// in the design preview panel to seed a brand-new chat thread's
// draft with a reference back to the in-progress design.
type DesignWorkdirInfo struct {
	MainPath string   `json:"mainPath"`
	Files    []string `json:"files"`
}

// GetDesignWorkdirInfo returns the absolute path to the thread's main/
// directory and the names of regular files directly inside it. The
// workdir is normally already ensured by EnsureDesignWorkdir on iframe
// mount; this binding tolerates a missing main/ directory by returning
// an empty file manifest (ListMainFiles' contract) so a caller racing
// the iframe-mount effect doesn't see a hard error.
//
// Listed in LocalOnlyMethods because the absolute path is filesystem
// state we don't want a remote peer enumerating. Verb prefix mirrors
// sibling getter bindings (GetSettings, GetThreadRuntimeMode) and
// keeps the method name distinct from the DesignWorkdirInfo struct.
func (a *App) GetDesignWorkdirInfo(threadID string) (DesignWorkdirInfo, error) {
	if a.designWorkdir == nil {
		return DesignWorkdirInfo{}, fmt.Errorf("design workdir manager unavailable")
	}
	mainPath, err := a.designWorkdir.MainPath(threadID)
	if err != nil {
		return DesignWorkdirInfo{}, err
	}
	files, err := a.designWorkdir.ListMainFiles(threadID)
	if err != nil {
		return DesignWorkdirInfo{}, err
	}
	return DesignWorkdirInfo{
		MainPath: mainPath,
		Files:    files,
	}, nil
}

// CaptureSnapshot freezes the current main/ directory as a labeled
// snapshot. Auto-on-turn-start callers pass an empty label and Auto=true.
func (a *App) CaptureSnapshot(threadID, label string) (design.Snapshot, error) {
	if a.designWorkdir == nil {
		return design.Snapshot{}, fmt.Errorf("design workdir manager unavailable")
	}
	return a.designWorkdir.Snapshot(threadID, design.SnapshotSpec{
		Label: label,
		Auto:  false,
	})
}

// BranchFromSnapshot restores main/ from a snapshot's stored copy.
// The snapshot row stays in the tree as a sibling.
func (a *App) BranchFromSnapshot(threadID, snapshotID string) error {
	if a.designWorkdir == nil {
		return fmt.Errorf("design workdir manager unavailable")
	}
	return a.designWorkdir.RestoreFromSnapshot(threadID, snapshotID)
}

// IngestDiagnosticBatch is the wire-bound entry point for the
// frontend's postMessage forwarder. The iframe-injected capture script
// posts diagnostics to the parent window; the frontend buffers and
// calls this binding to feed them into the per-thread ring.
func (a *App) IngestDiagnosticBatch(batch design.DiagnosticBatch) error {
	if a.designDiagnostics == nil {
		return fmt.Errorf("design diagnostic buffer unavailable")
	}
	if len(batch.Diagnostics) > maxDiagnosticBatchEntries {
		return fmt.Errorf("design: diagnostic batch too large (%d > %d)", len(batch.Diagnostics), maxDiagnosticBatchEntries)
	}
	// Truncate per-string lengths so a runaway iframe (or malicious
	// local peer reaching the binding via the wire) can't bloat the
	// per-thread ring with megabyte stack traces. Bounds match the
	// frontend's isBoundedString defaults plus headroom for real
	// stack traces.
	for i := range batch.Diagnostics {
		batch.Diagnostics[i].Message = clipString(batch.Diagnostics[i].Message, maxDiagnosticFieldChars)
		batch.Diagnostics[i].Source = clipString(batch.Diagnostics[i].Source, maxDiagnosticFieldChars)
		batch.Diagnostics[i].URL = clipString(batch.Diagnostics[i].URL, maxDiagnosticFieldChars)
		batch.Diagnostics[i].Stack = clipString(batch.Diagnostics[i].Stack, maxDiagnosticStackChars)
	}
	a.designDiagnostics.AppendBatch(batch.ThreadID, batch.Diagnostics)
	return nil
}

const (
	maxDiagnosticBatchEntries = 256
	maxDiagnosticFieldChars   = 8 * 1024
	maxDiagnosticStackChars   = 32 * 1024
)

func clipString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// IngestScreenshot completes a pending read_screenshot tool call. The
// frontend captures the live iframe in response to a
// design:capture-request event and posts the PNG bytes back here.
func (a *App) IngestScreenshot(result design.ScreenshotResult) error {
	if a.designScreenshots == nil {
		return fmt.Errorf("design screenshot broker unavailable")
	}
	if result.PNGBase64 == "" {
		return a.designScreenshots.Fail(result.RequestID, "empty png payload")
	}
	png, err := base64.StdEncoding.DecodeString(result.PNGBase64)
	if err != nil {
		return fmt.Errorf("decode screenshot png: %w", err)
	}
	return a.designScreenshots.Resolve(result.RequestID, png)
}

// FailScreenshot marks a pending capture as failed. Used when the
// frontend's html-to-image conversion errors out.
func (a *App) FailScreenshot(requestID, reason string) error {
	if a.designScreenshots == nil {
		return fmt.Errorf("design screenshot broker unavailable")
	}
	return a.designScreenshots.Fail(requestID, reason)
}
