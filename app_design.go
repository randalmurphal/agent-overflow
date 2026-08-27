package main

import (
	"context"
	"fmt"
	"log"
	"path/filepath"

	"agent-overflow/internal/design"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/screenshot"
	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"
)

// designCapturerFunc adapts a closure to the design.Capturer
// interface. The Reactor calls Capture(ctx, threadID); the closure
// builds the loopback URL for that thread and delegates to the
// shared screenshot.Manager.
type designCapturerFunc func(ctx context.Context, threadID string) ([]byte, error)

func (f designCapturerFunc) Capture(ctx context.Context, threadID string) ([]byte, error) {
	return f(ctx, threadID)
}

// newDesignCapturer wires the design.Reactor's screenshot path to
// the headless Chromium manager. The capturer reads the transport
// server's listen address at call time (rather than at boot) so a
// LAN rebind is picked up automatically — chromedp opens a fresh
// connection per capture either way.
func (a *App) newDesignCapturer() design.Capturer {
	return designCapturerFunc(func(ctx context.Context, threadID string) ([]byte, error) {
		if a.design.screenshots == nil {
			return nil, fmt.Errorf("design: screenshot manager not initialised")
		}
		srv := a.transportServer.Load()
		if srv == nil {
			return nil, fmt.Errorf("design: transport server not ready for capture")
		}
		addr := srv.Addr()
		if addr == "" {
			return nil, fmt.Errorf("design: transport server has no listen address")
		}
		// /design/ requests are gated by loopbackHostGuard +
		// designLoopbackOnly; the headless browser is also on
		// loopback so the request is allowed without a token.
		url := fmt.Sprintf("http://%s/design/%s/main/", addr, threadID)
		return a.design.screenshots.Capture(ctx, screenshot.CaptureOptions{
			URL:            url,
			ViewportWidth:  screenshot.DefaultTileWidth,
			ViewportHeight: screenshot.DefaultTileHeight,
		})
	})
}

type designSessionConfig struct {
	Prompt          string
	MCPServers      map[string]any
	MergeMCPServers bool
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
	if a.design.workdir == nil {
		return designSessionConfig{}, fmt.Errorf("design workdir manager unavailable")
	}
	if err := a.design.workdir.EnsureThread(thread.ID); err != nil {
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
	// Fire the chrome-headless-shell install + chromedp boot in the
	// background so the agent's first read_screenshot doesn't pay the
	// download cost on the hot path. ensureStarted is idempotent —
	// repeat activations are no-ops once the manager is up.
	a.primeDesignScreenshotManager()
	return servers, nil
}

// primeDesignScreenshotManager fires the screenshot manager's lazy
// install + boot in a goroutine so the user-visible "first capture"
// doesn't block on the ~150 MB chrome-headless-shell download. Failure
// here is non-fatal: the agent's first read_screenshot would just
// retry the install on the hot path, same as without the prime.
//
// The goroutine survives App.Shutdown intentionally — interrupting a
// mid-stream zip extract leaves a half-extracted version dir that the
// next install detects and rebuilds from scratch. Letting the prime
// finish doesn't slow shutdown (Shutdown closes the manager via Step
// 7 which cancels browserCtx, the goroutine sees a started+then-closed
// manager and exits without further work).
func (a *App) primeDesignScreenshotManager() {
	if a.design.screenshots == nil {
		return
	}
	go func() {
		if err := a.design.screenshots.Prime(context.Background()); err != nil {
			log.Printf("screenshot: prime: %v", err)
		}
	}()
}

// designMCPConfigForThread returns the MCP server config block for the
// thread's provider. Both providers use the same Codex HTTP MCP server
// (Codex consumes it inline, Claude consumes it via --mcp-config).
func (a *App) designMCPConfigForThread(thread store.Thread) (map[string]any, error) {
	if a.design.mcp == nil {
		return nil, fmt.Errorf("design MCP server unavailable")
	}
	return a.design.mcp.RegisterThread(thread.ID)
}

func (a *App) teardownDesignThread(threadID string) {
	// This is the shared per-provider-session feature teardown despite its
	// historical name: every stop path already funnels through it, including
	// non-design threads, so browser capabilities cannot outlive a provider.
	a.teardownBrowserThread(threadID)
	a.stopDesignWatcher(threadID)
	if a.design.reactor != nil {
		a.design.reactor.TeardownThread(threadID)
	}
	if a.design.mcp != nil {
		a.design.mcp.UnregisterThread(threadID)
	}
}

func (a *App) startDesignWatcher(threadID string) error {
	if a.design.workdir == nil {
		return fmt.Errorf("design workdir manager unavailable")
	}
	mainPath, err := a.design.workdir.MainPath(threadID)
	if err != nil {
		return err
	}
	threadDir := filepath.Dir(mainPath)
	a.design.watchersMu.Lock()
	defer a.design.watchersMu.Unlock()
	if existing, ok := a.design.watchers[threadID]; ok {
		// Idempotent: a redundant call (e.g. session restart) reuses
		// the live watcher instead of leaking a duplicate goroutine.
		_ = existing
		return nil
	}
	w := design.NewWatcher(threadID, threadDir, a.handleDesignWatcherEvent, design.WatcherOptions{})
	a.design.watchers[threadID] = w
	return nil
}

func (a *App) stopDesignWatcher(threadID string) {
	a.design.watchersMu.Lock()
	w, ok := a.design.watchers[threadID]
	if ok {
		delete(a.design.watchers, threadID)
	}
	a.design.watchersMu.Unlock()
	if ok {
		w.Stop()
	}
}

// handleDesignWatcherEvent is called on the watcher's run goroutine.
// It records iframe-ready activity in the diagnostic buffer's
// settle-window tracker and emits a thread-aware reload event so the
// frontend cache-busts the iframe / refreshes panels.
func (a *App) handleDesignWatcherEvent(ev design.WatchEvent) {
	if a.design.diagnostics != nil {
		// MarkActivity tells the diagnostic buffer the iframe is
		// likely about to load + report; Drain on get_design_diagnostics
		// will then block briefly to avoid stale-empty results.
		a.design.diagnostics.MarkActivity(ev.ThreadID)
	}
	switch ev.Subject {
	case design.WatchSubjectMain:
		a.emit(eventchan.DesignReloadMain, map[string]any{
			"threadId": ev.ThreadID,
		})
	case design.WatchSubjectOptions:
		a.emit(eventchan.DesignOptionsUpdate, map[string]any{
			"threadId": ev.ThreadID,
			"setId":    ev.SetID,
		})
	}
}

// ListDesignOptions returns the option ids inside `options/{setId}/`
// for a design thread, sorted lexically. Used by the frontend's
// options panel to hydrate the iframe grid when the watcher fires
// design:options-update — the agent writes the option directories,
// the watcher reports the set, and this binding enumerates the
// options inside it.
func (a *App) ListDesignOptions(threadID, setID string) ([]string, error) {
	if a.design.workdir == nil {
		return nil, fmt.Errorf("design workdir manager unavailable")
	}
	return a.design.workdir.ListOptions(threadID, setID)
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
	if a.design.workdir == nil {
		return nil, fmt.Errorf("design workdir manager unavailable")
	}
	setID, optionIDs, err := a.design.workdir.LatestUnpickedOptionSet(threadID)
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
	if a.design.workdir == nil {
		return fmt.Errorf("design workdir manager unavailable")
	}
	return a.design.workdir.MarkOptionSetPicked(threadID, setID)
}

// designWorkDirOverride returns the per-thread workdir to use as the
// provider subprocess's CWD for design threads, or "" when the
// session should keep the thread's WorkspacePath. The bundled system
// prompt teaches the agent that `main/` and `options/` are sibling
// directories in its CWD; without this override the agent's
// Read/Edit/Write would resolve against the project repo instead,
// the file watcher would never fire on its writes, and `pwd` would
// name the wrong place.
//
// Extracted from startSessionNow so the override is unit-testable
// without booting a full session.
func (a *App) designWorkDirOverride(t store.Thread) (string, error) {
	if t.Mode != "design" || a.design.workdir == nil {
		return "", nil
	}
	return a.design.workdir.ThreadDir(t.ID)
}

// EnsureDesignWorkdir materialises the per-thread {main,options}/
// layout (and seeds a placeholder index.html) for a design thread. The
// frontend's DesignPreviewPanel calls this when the iframe is about
// to mount so the file server has something to serve before the
// agent's first edit. Idempotent — safe to call on every mount.
func (a *App) EnsureDesignWorkdir(threadID string) error {
	if a.design.workdir == nil {
		return fmt.Errorf("design workdir manager unavailable")
	}
	if err := a.design.workdir.EnsureThread(threadID); err != nil {
		return fmt.Errorf("ensure workdir for thread %q (base=%q): %w",
			threadID, a.design.workdir.BaseDir(), err)
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
	if a.design.workdir == nil {
		return DesignWorkdirInfo{}, fmt.Errorf("design workdir manager unavailable")
	}
	mainPath, err := a.design.workdir.MainPath(threadID)
	if err != nil {
		return DesignWorkdirInfo{}, err
	}
	files, err := a.design.workdir.ListMainFiles(threadID)
	if err != nil {
		return DesignWorkdirInfo{}, err
	}
	return DesignWorkdirInfo{
		MainPath: mainPath,
		Files:    files,
	}, nil
}

// IngestDiagnosticBatch is the wire-bound entry point for the
// frontend's postMessage forwarder. The iframe-injected capture script
// posts diagnostics to the parent window; the frontend buffers and
// calls this binding to feed them into the per-thread ring.
func (a *App) IngestDiagnosticBatch(batch design.DiagnosticBatch) error {
	if a.design.diagnostics == nil {
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
		batch.Diagnostics[i].Message = stringsx.Clip(batch.Diagnostics[i].Message, maxDiagnosticFieldChars)
		batch.Diagnostics[i].Source = stringsx.Clip(batch.Diagnostics[i].Source, maxDiagnosticFieldChars)
		batch.Diagnostics[i].URL = stringsx.Clip(batch.Diagnostics[i].URL, maxDiagnosticFieldChars)
		batch.Diagnostics[i].Stack = stringsx.Clip(batch.Diagnostics[i].Stack, maxDiagnosticStackChars)
	}
	a.design.diagnostics.AppendBatch(batch.ThreadID, batch.Diagnostics)
	return nil
}

const (
	maxDiagnosticBatchEntries = 256
	maxDiagnosticFieldChars   = 8 * 1024
	maxDiagnosticStackChars   = 32 * 1024
)
