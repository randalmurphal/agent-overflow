package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"agent-overflow/internal/aocli"
	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/acp"
	"agent-overflow/internal/provider/agy"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/claudetui"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"

	"github.com/google/uuid"
)

// --- Session bindings and spawn helpers ---
//
// These methods own the provider-session lifecycle seen by Wails
// bindings. Session runtime coordination lives in internal/sessionruntime;
// thin wire façades live in app_session_bindings.go. Everything in this file
// runs the actual spawn and shutdown of a provider subprocess for a
// given thread.

const codexAccountReadTimeout = 8 * time.Second

// startSessionTakingLock brings the thread's provider subprocess up for a
// caller that does not hold the per-thread action lock: the workflow
// runner's start and the harness's HarnessStartSession. Sends start a
// session lazily through runRegisteredSessionStart instead.
//
// It takes the action lock because an unserialized start racing a revert
// can read the pre-revert SessionRef, clear the stopped-thread gate
// mid-revert (MarkThreadActive), and register a session bound to the old
// provider thread after the revert repointed the row. Callers that already
// hold the lock (sends, deferred config restarts) go through a.startSession
// / runRegisteredSessionStart directly.
//
// ctx bounds the two waits that have performed no side effects yet: the
// per-thread action lock and the join on somebody else's in-flight start.
// Once the spawn itself begins it runs to completion.
func (a *App) startSessionTakingLock(ctx context.Context, threadID string) error {
	_, err := a.startRegisteredSessionTakingLock(ctx, threadID)
	return err
}

func (a *App) startRegisteredSessionTakingLock(ctx context.Context, threadID string) (session, error) {
	if a.shuttingDown.Load() {
		return session{}, ErrShuttingDown
	}
	unlock, err := a.threadLocks().LockCtx(ctx, threadID)
	if err != nil {
		return session{}, fmt.Errorf("start session for thread %s: %w", threadID, err)
	}
	defer unlock()
	return a.startRegisteredSession(ctx, threadID)
}

// ensureProvenSession leaves the thread with a session whose provider proved
// it holds the conversation the thread continues, or with none. Under the
// thread action lock it proves the session registered for the thread, or,
// when none is, calls beforeStart, starts one and proves the session that
// start registered. It reports whether it started the session. A
// beforeStart error is returned as it is and nothing is started.
//
// The proof is the provider's report of how that process's start ended
// (sessionruntime.Entry.AwaitStart); nothing is read from the provider's
// transcript. The lock is held until the proof returns, so no send, steer,
// revert's resend or queued flush, each of which takes the lock, reaches the
// process first: Claude's content check is valid only before the first
// input. The commands AO writes on its own (live config, peer rename) do not
// take the lock, and the provider reports one that lands first as content
// unproven. A claude-tui session reports no start outcome and is accepted
// while it lives. ctx bounds the wait for the lock and the proof; the workflow
// runner's start deadline is what bounds them there
// (internal/workflowhost/start_watchdog.go).
//
// A session it cannot prove is torn down as a dead pre-init session, under
// the same lock, so it leaves no session registered and its queued messages
// go back to the composer before a flush can send them; a close that fails
// is returned instead of the proof's outcome, since a process may outlive it.
func (a *App) ensureProvenSession(ctx context.Context, threadID string, beforeStart func() error) (bool, error) {
	if a.shuttingDown.Load() {
		return false, ErrShuttingDown
	}
	unlock, err := a.threadLocks().LockCtx(ctx, threadID)
	if err != nil {
		return false, fmt.Errorf("prove the session of thread %s: %w", threadID, err)
	}
	defer unlock()
	proving, registered := a.sessionManager().get(threadID)
	if registered && proving.ClaudeTUI != nil {
		return false, nil
	}
	if !registered {
		if err := beforeStart(); err != nil {
			return false, err
		}
		proving, err = a.startRegisteredSession(ctx, threadID)
		if err != nil {
			return false, err
		}
	}
	outcome := proving.AwaitStart(ctx)
	if outcome == nil {
		return !registered, nil
	}
	if err := a.teardownDeadPreInitSessionLocked(threadID, proving.Token); err != nil {
		return false, fmt.Errorf("start of thread %s failed (%v), and %w", threadID, outcome, err)
	}
	return false, outcome
}

// buildSessionOptions composes the provider-agnostic SessionOptions bundle
// for a thread row. Shared by
// the spawn path and the live config reconciler (app_session_config.go) so
// both see the exact same view of "what a session for this row looks like".
//
// What this function does NOT do is stamp the settings-owned axes
// (SystemPrompt, DisabledTools). Those are read
// from Settings rather than projected from the thread row, so the two
// callers need opposite treatment — the spawn path applies today's values
// (applySettingsOwnedAxes), the reconcile path resolves against the live
// session's (reconcileSettingsOwnedAxes: the tool lists and the
// settings-level prompt pin, since a prompt change applies only at the next
// session start). Both live in app_session_prompt_override.go. Leaving them
// out here is what keeps the reconciler from composing an override (up to
// two git subprocesses under the per-thread config lock) on every model,
// effort, and runtime-mode change.
func (a *App) buildSessionOptions(t store.Thread) (provider.SessionOptions, error) {
	// Pending-fork intent is a one-shot. SessionOptionsFromThread reads
	// either PendingForkRef or SessionRef into opts.Resume based on this
	// flag. We consume it here (bool in scope) rather than mutating the
	// Thread row; any persistence changes belong to the caller that set
	// PendingForkRef originally.
	forkSession := t.PendingForkRef != ""

	settings := a.settings.Get()
	standardDefault, extendedDefault := settings.AutoCompactPercents(t.Provider)
	opts := provider.SessionOptionsFromThread(
		t,
		provider.AutoCompactDefaults{
			StandardPercent: standardDefault,
			ExtendedPercent: extendedDefault,
		},
		forkSession,
	)

	// The fast-mode service tier is a live-catalog fact, so it can come from
	// neither the thread row (which persists only the bool) nor the provider
	// package (which has no catalog access). Stamping it here — the one
	// SessionOptions construction site, shared by the spawn path and the live
	// reconciler — is what keeps a config diff from flapping between a resolved
	// and an unresolved tier for the same thread.
	opts.FastModeTierID = a.fastModeTierIDForModel(t.Provider, t.Model)

	return opts, nil
}

// startSessionNow builds the provider-specific launch config, stops
// any prior session on the thread, and spawns a fresh one. Callers go
// through StartSession / runSessionStart so concurrent start attempts
// share a single spawn instead of racing.
func (a *App) startSessionNow(threadID string) error {
	endAdmission, admitErr := a.workAdmission.begin(a.lifeCtx())
	if admitErr != nil {
		return admitErr
	}
	defer endAdmission()

	if err := a.store.CheckThreadTransferAccess(threadID); err != nil {
		return err
	}
	t, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	if err := a.store.CheckThreadExecutionAccess(t); err != nil {
		return err
	}
	sanitized := a.sanitizeThreadModelSettings(t)
	if !chatmodel.SameModelFields(t, sanitized) {
		if err := a.store.UpdateThreadModelSettings(threadID, t.Provider, sanitized.ModelSettings()); err != nil {
			return fmt.Errorf("start session: persist live model settings: %w", err)
		}
		a.announceThread(triage.ThreadActionFull, threadID)
		t = sanitized
	}
	// An imported Claude branch that is not its transcript's active branch
	// has no resume reference until it needs one. This is where it gets one,
	// by cutting the branch out of the source transcript into a session file
	// of its own. No-op for every other thread. See
	// app_session_import_branch.go.
	t = a.materializeImportedClaudeBranch(t)

	sessionToken := uuid.NewString()
	onEvent := a.sessionEventHandler(threadID, sessionToken, t.Provider)

	opts, err := a.buildSessionOptions(t)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	// Before anything is torn down, so a refused start leaves the existing
	// session running (app_isolated_workspace.go).
	if err := a.requireIsolatedWorkspace(opts.WorkDir); err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	// Also before the transcript is settled under the workspace, which would
	// otherwise report its own failure for a directory that is gone.
	if err := requireWorkspaceDir(opts.WorkDir); err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	// Spawn-only, and the one place the settings-level override is decided
	// for this session. The resolution travels to ensureClaudeMemoryDir
	// below so the directory we create and the prompt we rendered come from
	// the same settings snapshot — a save landing mid-spawn cannot make them
	// disagree.
	promptOverride, err := a.applySettingsOwnedAxes(t, &opts)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	// The thread's sub-thread place fixes, for this session's life, whether
	// it has thread_spawn and its provider's own subagents.
	place, err := a.subThreadPlaceOf(threadID)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	withoutNativeSubagents(&opts, t.Provider, place)
	// Before anything is torn down: an assistant whose transcript the
	// provider cleaned up continues in a new thread instead.
	if err := a.requireAssistantSession(t, opts); err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	if err := a.stopExistingSessionLocked(threadID); err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	// The transcript must sit under the workspace's project slug before
	// `--resume` runs from that cwd. The CLI relocates it itself when it
	// moves (EnterWorktree / ExitWorktree, app_worktree_follow.go); this is
	// the safety net for a row and a file that disagree, and it runs here
	// because the prior process is fully stopped and nothing else writes
	// the file.
	if !opts.ForkSession {
		a.settleClaudeTranscriptForWorkspace(t)
	}

	// An ordinary Claude resume passes no cursor: the CLI picks its own
	// default leaf and applies its own resume filters, so it never
	// rejects the start over a leaf AO chose.
	//
	// A pinned fork start resolves its stored pin against the SOURCE
	// session (opts.Resume is the source ref while PendingForkRef is
	// pending). A pinned lazy fork must cut where its timeline was cut,
	// so an unresolvable pin fails the start rather than falling back to
	// the source's current tail, which would hand the fork whatever the
	// source streamed since the fork. An unpinned fork (idle source at
	// fork time) keeps the CLI's own tail cut.
	if t.Provider == string(provider.Claude) && opts.ForkSession && t.PendingForkResumeAt != "" {
		projectsDir, err := a.claudeProjectsDir()
		if err != nil {
			return fmt.Errorf("start session: %w", err)
		}
		cursor, err := resolveClaudeForkResumeAt(projectsDir, opts.Resume, opts.WorkDir, t.PendingForkResumeAt)
		if err != nil {
			return fmt.Errorf("start session: resolve fork resume cursor: %w", err)
		}
		opts.ResumeAt = cursor
	}

	// Commit the thread to this session in triage BEFORE spawning. A
	// prior StopSession left the stopped-thread marker set, and the
	// replacement session can emit before any wire proof-of-life lands:
	// Codex emits EventInit synchronously inside NewSession, and a
	// Claude process that dies during startup (a fork start's unusable
	// --resume-session-at cursor) emits its only diagnostics pre-init.
	// From here triage admits only this session's frames, so a prior
	// session's frames that stopExistingSessionLocked's drain left
	// queued when the worker stalled are refused rather than applied to
	// this one. This is the ONLY place the marker is cleared; see
	// triage.MarkThreadActive.
	if a.triage != nil {
		a.triage.MarkThreadActive(threadID, sessionToken)
	}

	registeredTools := false
	defer func() {
		if !registeredTools {
			a.revokeSessionMCP(threadID, sessionToken)
			// The commit above retired the session this start replaced,
			// so that session's disconnect leaves the thread's event
			// queue alone, and this start's own session never ran to
			// disconnect. Mark the queue here, or it stays registered
			// until a later session ends.
			a.providerEvents.retireWhenIdle(threadID)
		}
	}()
	browserServers, err := a.browserMCPConfigForThread(t, sessionToken)
	if err != nil {
		return fmt.Errorf("start session: register browser tools: %w", err)
	}

	remoteRevision := a.remoteMCP.revision.Load()
	remoteServers, err := a.remoteMCPConfigForThread(t, sessionToken)
	if err != nil {
		return fmt.Errorf("start session: register remote tools: %w", err)
	}
	threadServers, err := a.threadMCPConfigForThread(t, sessionToken, place)
	if err != nil {
		return fmt.Errorf("start session: register thread tools: %w", err)
	}
	workflowServers, err := a.workflowMCPConfigForThread(t, sessionToken)
	if err != nil {
		return fmt.Errorf("start session: register workflow tools: %w", err)
	}
	if browserServers == nil {
		browserServers = map[string]any{}
	}
	for name, config := range remoteServers {
		browserServers[name] = remoteMCPServerConfig(t.Provider, config)
	}
	for name, config := range threadServers {
		browserServers[name] = threadMCPServerConfig(t.Provider, config)
	}
	for name, config := range workflowServers {
		browserServers[name] = threadMCPServerConfig(t.Provider, config)
	}
	if len(browserServers) == 0 {
		browserServers = nil
	}

	// The one side effect a rendered system-prompt override carries. Gated
	// on the resolution above (which already knows whether the override won
	// the prompt) and never fatal to the spawn.
	a.ensureClaudeMemoryDir(t, opts.WorkDir, promptOverride)

	// Retire runtime state owned by the prior Codex app-server before a new
	// subprocess can replay or emit lifecycle events. Child ownership stays
	// persisted for resume, while old live turns and PTYs leave the tray.
	if t.Provider == string(provider.Codex) {
		if err := a.retireCodexBackgroundRuntime(threadID); err != nil {
			return fmt.Errorf("start session: %w", err)
		}
	}

	agent, err := a.readStandingAgentSession(t)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	// A read-only session refuses every write, so its memory comes
	// without the write rules; claude-tui leaves approvals to the
	// terminal, so its mode never refuses one.
	readOnly := opts.RuntimeMode == provider.RuntimeReadOnly && provider.CapabilitiesForProvider(t.Provider).EnforcesRuntimeMode
	mem, err := a.readSessionMemory(t, agent.Agent, opts.Resume != "", readOnly)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	newSess, err := a.spawnProviderSession(threadID, sessionToken, t, opts, browserServers, agent, mem, place, onEvent)
	if err != nil {
		if (codex.IsThreadNotFound(err) || acp.IsSessionNotFound(err)) && agent.Block != "" {
			// An assistant's provider session the runtime no longer has.
			err = errors.Join(errAssistantSessionGone, err)
		}
		a.emitProviderStatusOnSessionStartError(t.Provider)
		// The start failed for its own reason; the memory that could not be
		// read is still news the user needs before the next attempt.
		a.reportSessionMemory(threadID, mem)
		return fmt.Errorf("start session: %w", err)
	}

	if mem.Hook != nil {
		newSess.MemoryPlan = mem.Plan
	}
	a.sessionManager().put(threadID, newSess)
	registeredTools = true
	// Codex reports its thread in an EventInit emitted before NewSession
	// returns, and callers act on the started session as soon as this
	// returns (a send, a fork reading the session ref). Handle what the
	// provider emitted while starting before handing the session over.
	a.drainProviderEvents(threadID, "start session")
	if a.remoteMCP.revision.Load() != remoteRevision {
		a.signalRemotePeers()
	}
	a.emitProviderSessionAccount(threadID)
	a.reportSessionMemory(threadID, mem)
	// The fresh session launched with today's prompt, which clears any
	// prompt change flag; a save that landed while it spawned flags it.
	a.reconcileStalePromptSession(threadID)

	// A session-death restore can requeue messages instead of restoring
	// them to the draft (failed stale-row cleanup, R11-1). Every other
	// drain trigger needs a live session (RegisterQueueItem) or wire
	// traffic (the boundary drains), so an idle replacement would strand
	// them indefinitely — flush now that the session can accept sends
	// (round-12, D12-2). The dispatch worker serializes behind this
	// function's thread action lock, so the flush lands after startup
	// completes.
	if a.triage != nil && a.triage.HasQueuedFlushItems(threadID) {
		a.triage.FlushQueuedItems(threadID)
	}

	// Best-effort Codex reconcile for the on-reopen case. If the prior
	// app-server forgot the thread across a restart we want to flip any
	// still-`running && is_background` rows to errored/lost (systemError
	// probe) or rehydrate the thread (notLoaded probe) before the user
	// sends another turn. This only applies to Codex sessions that were
	// resumed — a brand-new thread has nothing to reconcile.
	//
	// Runs asynchronously so a slow `thread/read` can't block session
	// startup; errors are logged because reconciliation is never
	// user-perceivable. The pattern mirrors probeStartupProviderStatuses
	// in ServiceStartup.
	if newSess.Codex != nil && opts.Resume != "" {
		go a.reconcileCodexAfterStart(threadID, newSess)
	}

	return nil
}

// claudeForkPinRetries / claudeForkPinBackoff bound the wait for a fork
// pin — a leaf observed on the source session's stdout at fork time —
// to reach the source transcript file. The CLI emits the stdout frame
// before appending the row, so a fork whose first send lands inside
// that window scans a file that does not hold the pin yet. The gap has
// never been measured; the budget is a guess with a bounded failure
// mode: if the real gap is longer, the cost is 500ms spent here and a
// fall back to the deepest on-disk cursor, which cuts slightly EARLIER
// than the pin — the honest interrupt shape, never later.
const (
	claudeForkPinRetries = 10
	claudeForkPinBackoff = 50 * time.Millisecond
)

// resolveClaudeForkResumeAt turns a pinned fork's stored cut
// (threads.pending_fork_resume_at) into the --resume-session-at cursor
// its `--fork-session` spawn passes. The pin is repaired against the
// CLI's resume deserialization filters over the SOURCE file — the CLI
// filters BEFORE the cursor lookup, so a pin sitting on a row the
// filters drop (a dangling tool_use from a tool that was mid-flight at
// fork time) would hard-fail resume pre-init; the repair substitutes
// the deepest surviving row at or before the pin's file position, never
// after it. A pin not on disk yet is the stdout-to-disk append gap and
// is waited out (bounded); exhaustion falls back to the deepest
// surviving on-disk cursor. Only two shapes fail the start: a real I/O
// fault reading the source, and a source that holds no resumable row at
// all — both would otherwise spawn a fork whose cut is a lie.
func resolveClaudeForkResumeAt(projectsDir, sourceRef, workDir, pin string) (string, error) {
	var last claude.ForkResumeCursor
	for attempt := range claudeForkPinRetries {
		if attempt > 0 {
			time.Sleep(claudeForkPinBackoff)
		}
		res, err := claude.ResolveForkResumeCursor(projectsDir, sourceRef, workDir, pin)
		if err != nil {
			return "", fmt.Errorf("resolve fork cut %s in source session %s: %w", pin, sourceRef, err)
		}
		if res.PinOnDisk {
			if res.Cursor == "" {
				return "", fmt.Errorf("fork source session %s holds no resumable row at or before the fork cut %s", sourceRef, pin)
			}
			if res.Cursor != pin {
				log.Printf("start session: fork cut %s does not survive claude's resume filters in %s — pinning at %s instead", pin, sourceRef, res.Cursor)
			} else if attempt > 0 {
				log.Printf("start session: fork cut %s reached %s after %d retries", pin, sourceRef, attempt)
			}
			return res.Cursor, nil
		}
		last = res
	}
	if last.Cursor == "" {
		return "", fmt.Errorf("fork cut %s never reached source session %s and its transcript holds no resumable row", pin, sourceRef)
	}
	log.Printf("start session: fork cut %s never reached source session %s — pinning at the on-disk cursor %s instead", pin, sourceRef, last.Cursor)
	return last.Cursor, nil
}

// stopExistingSessionLocked tears down any prior session for the thread
// before we start a replacement. Separated from startSessionNow so the
// caller reads top-down as "stop, compute options, spawn, register".
func (a *App) stopExistingSessionLocked(threadID string) error {
	existing, ok := a.sessionManager().take(threadID)

	if !ok {
		// A session that died may have discarded sends whose death restore
		// gave way to this replacement.
		a.restoreUndeliveredSendsLocked(threadID)
		return nil
	}
	a.emitProviderSessionDisconnected(threadID, existing.Provider)

	err := a.closeProviderSession(threadID, existing)
	if a.triage != nil {
		if _, settleErr := a.triage.SettleBackgroundLaunchesForSessionEnd(threadID); settleErr != nil {
			log.Printf("app: settle background launches on replaced session for %s: %v", threadID, settleErr)
		}
		a.triage.EndReplacedSessionTurn(threadID)
	}
	// The close processed the old CLI's final frames, including the sends
	// it discarded unrun. No session-end restore runs on this path.
	a.restoreUndeliveredSendsLocked(threadID)
	// The replacement session streams fresh items; the old stream's
	// scanner states are dead weight (their final ticks will never
	// arrive) and a reused item ID would inherit a stale watermark.
	// unregisterSession can't purge for this path: the token was
	// already taken above, so its callback only drops the dead
	// process's schedule and skips the registry half that purges. Unlike
	// teardownAndCloseSession there is no CleanupThread here (the
	// replacement path keeps triage live apart from the closed turn), so the stream
	// persist buffers must be drained explicitly first: the close drained
	// the read loop and its events, but a buffer armed by its final deltas would
	// otherwise fire its 250ms flush AFTER the purge and re-register
	// the state it just removed.
	if a.triage != nil {
		if flushErr := a.triage.FlushThread(threadID); flushErr != nil {
			log.Printf("app: flush stream buffers before seeder purge for %s: %v", threadID, flushErr)
		}
	}
	a.highlightService().PurgeThread(threadID)
	return err
}

// sessionProcessEnv is the ONE place a provider subprocess's environment is
// assembled, so anything the app injects reaches every provider identically:
// the user's custom environment for that provider (settings, applied at spawn),
// the boot-mode overrides (providerExtraEnv — the harness's mock-control
// credentials), the static in-session marker (AO_IN_SESSION=1 so the binary
// refuses to boot a second app instance from inside a session), and the
// PATH entry that makes `agent-overflow` resolve to this executable (D30).
//
// The boot-mode overrides and the marker are pins, over the user's custom
// environment: they are contracts, not preferences. Every AO_* name is
// reserved at the settings boundary, so a user value that would have been
// shadowed is refused on save instead of vanishing at spawn. The CLI's
// directory goes in front of the inherited PATH.
func (a *App) sessionProcessEnv(providerName string) provider.EnvInput {
	pins := provider.AOPins(a.providerExtraEnv)
	if pins == nil {
		pins = make(map[provider.ReservedEnv]string, 1)
	}
	pins[provider.AOEnv(aocli.EnvInSession)] = "1"
	return provider.EnvInput{
		Custom:      a.providerCustomEnv(providerName),
		Pins:        pins,
		PathPrepend: a.cliBinDir,
	}
}

// spawnProviderSession builds the provider-specific Config via
// ConfigFromOptions + per-provider ancillary wiring (binary path, MCP
// servers for Codex, event logger) and calls the provider's NewSession
// constructor. Returns a populated session wrapper ready to register in
// a.sessions.
func (a *App) spawnProviderSession(
	threadID, sessionToken string,
	t store.Thread,
	opts provider.SessionOptions,
	mcpServers map[string]any,
	agent standingAgentSession,
	mem sessionMemory,
	place subThreadPlace,
	onEvent func(provider.ProviderEvent),
) (session, error) {
	// liveness is initialized once and attached to whichever provider
	// branch fires. Keeping the construction out of the switch arms
	// guarantees a future third provider can't forget the field and
	// silently end up immune to the idle reaper.
	liveness := newSessionLiveness(time.Now())
	workflowSchema, err := a.workflowSchemaForSession(t)
	if err != nil {
		return session{}, err
	}
	accountSelection := a.captureProviderAccountSelection(t.Provider)
	// What the session appends to its system prompt (app_agent_guide.go) is
	// spawn-only on every provider: it rides the Config, not the options
	// bundle, so the reconciler never queues a restart for it and a change
	// lands at the next start or resume.
	prompts, err := a.sessionPromptsFor(t, mcpServers, agent.Block, place)
	if err != nil {
		return session{}, err
	}
	appSettings := a.currentSettings()

	switch t.Provider {
	case string(provider.Claude):
		cfg := claude.ConfigFromOptions(opts)
		if len(workflowSchema) > 0 {
			cfg.OutputSchema = string(workflowSchema)
		}
		cfg.Binary = a.providerBinaryPath(t.Provider)
		cfg.AppendSystemPrompt = prompts.claudeAppend
		cfg.SessionStartHook = mem.Hook
		cfg.SessionStartForget = mem.Forget
		cfg.NativeMemoryOff = mem.OwnMemoryOff
		cfg.InstalledCLIVersion = a.installedProviderVersion(t.Provider)
		cfg.Env = a.sessionProcessEnv(t.Provider)
		if mcpServers[remoteMCPName] != nil {
			cfg.Env = withRemoteMCPClaudeEnv(cfg.Env)
		}
		// Admitting the thread tools without a prompt: under `dontAsk`
		// this is what lets the call through at all, and under the
		// prompting modes it is what skips the prompt. One argv value,
		// wildcarded over the server so a new tool needs no second edit.
		if mcpServers[threadMCPName] != nil {
			cfg.AllowedTools = append(cfg.AllowedTools, ThreadToolsAllowedTool)
		}
		if mcpServers[workflowMCPName] != nil {
			cfg.AllowedTools = append(cfg.AllowedTools, WorkflowToolsAllowedTool)
		}
		cfg.EventLogger = a.logger
		cfg.Reaper = a.groupReaper("claude session")
		cfg.MCPServers = mcpServers
		cfg.AdditionalDirs = a.claudeAdditionalDirs()
		cfg.AccessDirs = mem.Dirs
		applyClaudeSessionAxes(&cfg, appSettings.ClaudeSessionAxesForProvider(t.Provider))
		cfg.Retention = a.claudeRetention(cfg.WorkDir)
		// The peer-visible name rides the Config rather than the options
		// bundle on purpose: it is live-changeable through `/rename`, so
		// letting the reconciler see it would queue a restart every time a
		// thread title lands. See app_claude_peer_name.go.
		cfg.PeerSessionName = opts.ClaudePeerSessionName
		cfg.ModelAliases = a.providerDiscoveryService().ClaudeModelAliases()
		cfg.AgentLaunchByTask = a.claudeAgentLaunchLookup(threadID)
		cfg.SystemPromptDir, err = a.claudePromptFileDir()
		if err != nil {
			return session{}, err
		}
		sess, err := claude.NewSession(context.Background(), threadID, cfg, onEvent)
		if err != nil {
			return session{}, err
		}
		return session{
			Provider:             string(provider.Claude),
			Token:                sessionToken,
			CredentialGeneration: accountSelection.Generation,
			CredentialAccountID:  accountSelection.AccountID,
			CredentialAccount:    accountSelection.Account,
			Claude:               sess,
			LaunchOptions:        opts,
			Liveness:             liveness,
		}, nil

	case string(provider.Codex):
		cfg := codex.ConfigFromOptions(opts)
		cfg.Binary = a.providerBinaryPath(t.Provider)
		cfg.Env = a.sessionProcessEnv(t.Provider)
		cfg.EventLogger = a.logger
		cfg.Reaper = a.groupReaper("codex session")
		cfg.MCPServers = mcpServers
		// Codex never shows a server's instructions to the model, so the
		// app guide and the on servers' guides ride developer instructions
		// instead. Each guide follows the same shape its tool list does and
		// comes from the same source, so the two cannot drift; a browser or
		// thread-tools entry is registered whether or not its switch is on
		// (so the tool list can be turned on live), while its guide is only
		// sent when the tools are on, and a live switch reaches the text at
		// the next start or resume: a resumed thread whose text changed
		// gets it injected (codexDeveloperInstructionsDelivery).
		cfg.DeveloperInstructions = prompts.codexDeveloper
		cfg.SessionStartHook = mem.Hook
		cfg.SessionStartForget = mem.Forget
		cfg.WritableRoots = mem.Dirs
		cfg.NativeMemoryOff = mem.OwnMemoryOff
		cfg.DisableBundledBrowserPlugin = a.agentGuideServersOn(t.ID, mcpServers)[appbrowser.ServerName]
		cfg.DeveloperInstructionsDelivery, err = a.codexDeveloperInstructionsDelivery(threadID, cfg.ResumeThreadID)
		if err != nil {
			return session{}, err
		}
		// Ownership of a row sitting in the PROVIDER's queue is a store
		// question — the id grammar is deterministic and therefore not a
		// credential — so the codex package asks the app layer. AO writes no
		// rows there any more, so this only ever claims a legacy one.
		cfg.OwnsQueuedClientID = func(clientID string) bool {
			return a.ownsLegacyProviderQueuedClientID(threadID, clientID)
		}
		// A message an OLD build handed to the provider's queue outlives the
		// app-server that took it: it sits in codex's SQLite and the idle hook
		// dispatches it on THIS connection, as a turn nobody asked for. Retire
		// those rows — delete them from the queue and hand the text back to the
		// composer — before that can happen.
		//
		// BeforeResume, not after NewSession: `thread/resume` is what LOADS the
		// thread, and a loaded thread's idle hook is what dispatches the queue.
		// A sunset that runs after the dispatch is no sunset at all.
		cfg.BeforeResume = func(resumed *codex.Session) {
			a.sunsetLegacyProviderQueueRows(threadID, resumed)
		}
		// A RESUMED thread cannot opt into raw events, so the mailbox record
		// that closes a spawn card is invisible on its wire. Load the compact
		// unresolved-spawn projection once: it rebuilds provider-side child
		// ownership without loading transcript history, and its non-emptiness
		// also answers whether the rollout recovery tail is needed.
		if cfg.ResumeThreadID != "" {
			cfg.ResumeCollabLaunches, err = a.codexResumeCollabLaunches(threadID)
			if err != nil {
				return session{}, err
			}
			cfg.ResumeHasUnresolvedSubagents = len(cfg.ResumeCollabLaunches) > 0
		}
		conn, releaseConn, poolKey, err := a.acquireCodexConn(
			a.lifeCtx(),
			threadID,
			cfg.Binary,
			cfg.WorkDir,
			cfg.Env,
			&accountSelection,
		)
		if err != nil {
			return session{}, err
		}
		sess, err := conn.OpenPooled(a.lifeCtx(), threadID, cfg, onEvent, releaseConn)
		if err != nil && errors.Is(err, codex.ErrThreadUnloadFailed) {
			// The shared app-server could not unload the prior loaded thread;
			// dial a dedicated process and verify the account identity BEFORE
			// opening/resuming the thread so changed overrides take effect
			// cleanly and no turn can start under a mismatched account.
			log.Printf("codex: shared conn unload failed for %s (%v); spawning dedicated app-server", threadID, err)
			poolKey = ""
			conn = nil
			dedicatedConn, dialErr := codex.Dial(a.lifeCtx(), codex.ConnConfig{
				Binary:      cfg.Binary,
				WorkDir:     cfg.WorkDir,
				Env:         cfg.Env,
				EventLogger: cfg.EventLogger,
				Reaper:      cfg.Reaper,
				ThreadID:    threadID,
			})
			if dialErr != nil {
				return session{}, dialErr
			}
			if accountSelection.AccountID != "" {
				accountCtx, cancelAccountRead := context.WithTimeout(a.lifeCtx(), codexAccountReadTimeout)
				accountInfo, accountErr := dedicatedConn.AccountInfo(accountCtx)
				cancelAccountRead()
				if accountErr == nil {
					var observedAccountID string
					var updatedAccount *provideraccounts.Account
					observedAccountID, updatedAccount, accountErr = a.accountIDForObservedIdentity(
						string(provider.Codex),
						accountSelection.AccountID,
						accountInfo,
					)
					if accountErr == nil && updatedAccount != nil {
						a.emitProviderAccountIfCurrent(
							string(provider.Codex),
							*updatedAccount,
							providerAccountInfo(*updatedAccount),
						)
					}
					if accountErr == nil && observedAccountID != accountSelection.AccountID {
						accountErr = fmt.Errorf(
							"codex app-server authenticated as account %q instead of selected account %q",
							observedAccountID,
							accountSelection.AccountID,
						)
					}
				}
				if accountErr != nil {
					return session{}, errors.Join(accountErr, dedicatedConn.Close())
				}
			}
			dedicatedConn.SetSkillsChangedHandler(func() {
				a.handleCodexSkillsChanged()
			})
			dedicatedConn.SetMCPStartupUpdateHandler(func(u codex.MCPStartupUpdate) {
				a.handleCodexMCPStartupUpdate(u)
			})
			dedicatedConn.SetMCPOAuthCompletedHandler(func(_ string, serverName string, success bool, errMsg string) {
				a.handleCodexMCPOAuthCompleted("", serverName, success, errMsg)
			})
			sess, err = dedicatedConn.OpenOwned(a.lifeCtx(), threadID, cfg, onEvent)
			if err != nil {
				return session{}, errors.Join(err, dedicatedConn.Close())
			}
		}
		if err != nil {
			return session{}, err
		}
		// Wire the per-session OAuth-completion observer; connection-wide
		// skills/changed and mcpServer/startupStatus/updated handlers are
		// installed once on the Conn during Dial.
		sess.SetMCPOAuthCompletedHandler(func(serverName string, success bool, errMsg string) {
			a.handleCodexMCPOAuthCompleted(threadID, serverName, success, errMsg)
		})
		return session{
			Provider:             string(provider.Codex),
			Token:                sessionToken,
			CredentialGeneration: accountSelection.Generation,
			CredentialAccountID:  accountSelection.AccountID,
			CredentialAccount:    accountSelection.Account,
			Codex:                sess,
			Conn:                 conn,
			PoolKey:              poolKey,
			LaunchOptions:        opts,
			Liveness:             liveness,
		}, nil

	case string(provider.ClaudeTUI):
		cfg, err := a.claudeTUISessionConfig(threadID, opts, prompts.claudeAppend, mcpServers, mem)
		if err != nil {
			return session{}, err
		}
		sess, err := claudetui.NewSession(context.Background(), threadID, cfg, onEvent)
		if err != nil {
			return session{}, err
		}
		return session{
			Provider:      string(provider.ClaudeTUI),
			Token:         sessionToken,
			ClaudeTUI:     sess,
			LaunchOptions: opts,
			Liveness:      liveness,
		}, nil

	case string(provider.Agy):
		binary := a.providerBinaryPath(t.Provider)
		env := a.sessionProcessEnv(t.Provider)
		conn, releaseConn, poolKey, err := a.acquireAgyConn(
			a.lifeCtx(),
			threadID,
			binary,
			opts.WorkDir,
			env,
			&accountSelection,
		)
		if err != nil {
			return session{}, err
		}
		sessCfg := acp.SessionConfig{
			WorkDir:               opts.WorkDir,
			ResumeSessionID:       opts.Resume,
			Model:                 opts.Model,
			ReasoningEffort:       string(opts.ReasoningEffort),
			InteractionMode:       opts.Mode,
			RuntimeMode:           opts.RuntimeMode,
			SystemPrompt:          opts.SystemPrompt,
			AppendSystemPrompt:    joinPromptParts(prompts.claudeAppend, memoryPromptText(mem)),
			AdditionalDirectories: mem.Dirs,
			DisabledTools:         opts.DisabledTools,
			MCPServers:            agy.TranslateMCPServers(mcpServers),
			Flavor:                agy.NewFlavor(a.agyAppDataDir()),
		}
		sess, err := conn.OpenPooled(a.lifeCtx(), threadID, sessCfg, onEvent, releaseConn)
		if err != nil {
			return session{}, err
		}
		return session{
			Provider:             string(provider.Agy),
			Token:                sessionToken,
			CredentialGeneration: accountSelection.Generation,
			CredentialAccountID:  accountSelection.AccountID,
			CredentialAccount:    accountSelection.Account,
			ACP:                  sess,
			Session:              sess,
			Conn:                 conn,
			PoolKey:              poolKey,
			LaunchOptions:        opts,
			Liveness:             liveness,
		}, nil

	default:
		return session{}, fmt.Errorf("unknown Provider: %s", t.Provider)
	}
}

// claudeTUISessionConfig is the launch configuration of a thread's
// interactive Claude session.
func (a *App) claudeTUISessionConfig(
	threadID string,
	opts provider.SessionOptions,
	appGuide string,
	mcpServers map[string]any,
	mem sessionMemory,
) (claudetui.Config, error) {
	cfg := claudetui.ConfigFromOptions(opts)
	// The interactive provider drives the same `claude` binary as the
	// headless one; there is no separate TUI binary setting.
	cfg.Binary = a.providerBinaryPath(string(provider.Claude))
	cfg.AppendSystemPrompt = appGuide
	cfg.SessionStartHook = mem.Hook
	cfg.SessionStartForget = mem.Forget
	cfg.NativeMemoryOff = mem.OwnMemoryOff
	cfg.InstalledCLIVersion = a.installedProviderVersion(string(provider.Claude))
	cfg.ModelAliases = a.providerDiscoveryService().ClaudeModelAliases()
	cfg.AgentLaunchByTask = a.claudeAgentLaunchLookup(threadID)
	// claudetui builds its child environment itself (buildEnv) rather than
	// through Spawn, from the same sessionProcessEnv input every provider
	// gets. Skipping this branch would leave one provider silently without
	// the boot-mode values and the ao credential.
	//
	// The TUI shares Claude's custom environment (one binary, one backend),
	// with one variable it cannot take through the process env:
	// ANTHROPIC_BASE_URL names the per-session loopback gateway for the
	// child, and claudetui's buildEnv strips any inherited value. Handing it
	// to the gateway's upstream instead is what keeps the interactive
	// provider on the same backend as the headless one — dropping it here
	// would silently run take-control sessions against api.anthropic.com
	// while every other Claude surface used the user's endpoint.
	//
	// The command timeout ceiling is a default, as on the headless spawn:
	// custom env over the setting over an inherited value.
	cfg.Env = claude.WithCommandTimeoutEnv(
		a.sessionProcessEnv(string(provider.ClaudeTUI)),
		opts.ClaudeMaxCommandTimeoutHours,
	)
	cfg.AdditionalDirs = a.claudeAdditionalDirs()
	cfg.AccessDirs = mem.Dirs
	cfg.MCPServers, cfg.AllowedTools = claudeTUIMCPServers(mcpServers)
	cfg.Retention = a.claudeRetention(cfg.WorkDir)
	cfg.Upstream = a.claudetuiUpstream()
	cfg.EventLogger = a.logger
	cfg.Reaper = a.groupReaper("claude terminal session")
	var err error
	cfg.SystemPromptDir, err = a.claudePromptFileDir()
	if err != nil {
		return claudetui.Config{}, err
	}
	return cfg, nil
}

func (a *App) codexResumeCollabLaunches(threadID string) ([]codex.ResumeCollabLaunch, error) {
	if a.store == nil {
		return nil, nil
	}
	items, err := a.store.ListIncompleteCodexSubagentOwnerships(threadID)
	if err != nil {
		return nil, fmt.Errorf("load Codex resume child ownership for thread %s: %w", threadID, err)
	}
	launches := make([]codex.ResumeCollabLaunch, 0, len(items))
	for _, item := range items {
		launches = append(launches, codex.ResumeCollabLaunch{
			ItemID: item.ItemID,
			Meta:   json.RawMessage(item.Meta),
		})
	}
	return launches, nil
}

func (a *App) workflowSchemaForSession(thread store.Thread) (json.RawMessage, error) {
	if thread.Mode != "workflow" {
		return nil, nil
	}
	if !a.workflowApplication().HasRunner() {
		return nil, fmt.Errorf("start workflow session: runner unavailable")
	}
	schema, registered := a.workflowApplication().SessionSchemaForThread(thread.ID)
	if !registered {
		return nil, fmt.Errorf("start workflow session %s: phase output schema is not registered", thread.ID)
	}
	return schema, nil
}

// SendMessageOptions carries send-time composer settings. AttachmentIDs is the
// current attachment payload; RuntimeMode is an optional draft override applied
// immediately before the provider turn starts.
type SendMessageOptions struct {
	// ConsumeDraft is captured before sending. Omission preserves legacy clearing.
	ConsumeDraft                 *DraftSnapshot      `json:"consumeDraft,omitempty"`
	AttachmentIDs                []string            `json:"attachmentIds"`
	RuntimeMode                  string              `json:"runtimeMode,omitempty"`
	SourceProposedPlan           *SourceProposedPlan `json:"sourceProposedPlan,omitempty"`
	RevisionSourceProposedPlan   *SourceProposedPlan `json:"revisionSourceProposedPlan,omitempty"`
	RevisionSourceCommentIDs     []string            `json:"revisionSourceCommentIds,omitempty"`
	RevisionSourceDiffReview     *SourceDiffReview   `json:"revisionSourceDiffReview,omitempty"`
	RevisionSourceDiffCommentIDs []string            `json:"revisionSourceDiffCommentIds,omitempty"`
	// SendID is the client-minted idempotency id of ONE composer send,
	// carried identically by SendMessageWithOptions and RegisterQueueItem
	// (frontend/src/lib/utils/sendOptions.ts mints it in the one place both
	// paths build their options).
	//
	// It exists because a socket that dies after the frame reached the
	// backend is indistinguishable, on the client, from one that died
	// before: the person presses Send again and starts the turn twice. With
	// an id, the second arrival finds the first one's record — the
	// dispatched row's meta, or the durable queue row — and returns that
	// outcome instead of sending anything.
	//
	// EMPTY IS LEGAL and disables the check for that call: every
	// app-internal injector leaves it unset, as does any client bundle
	// older than the field.
	SendID string `json:"sendId,omitempty"`
	// ReconcileBySendID declares that the caller finds its message's row by
	// SendID, rather than predicting user:<turn>. Windows older than v0.0.15
	// send SendID without it, so its presence alone cannot say this; a
	// client send without it is refused (sendClientMessage).
	ReconcileBySendID bool `json:"reconcileBySendId,omitempty"`
}

// pageSendOptions are the options of the page's default send under sendID
// (frontend/src/lib/utils/sendOptions.ts buildSendOptions): rows found by
// that id, and an empty draft snapshot, which consumes no composer draft.
// frontend/src/test/fixtures/pageDefaultSend.json is the reference they are
// checked against.
func pageSendOptions(sendID string) SendMessageOptions {
	return SendMessageOptions{
		AttachmentIDs:     []string{},
		ConsumeDraft:      &DraftSnapshot{AttachmentIDs: []string{}, TerminalChips: []TerminalChip{}},
		SendID:            sendID,
		ReconcileBySendID: true,
	}
}

// interruptTurnCtx is InterruptTurn with a bounded entry: ctx limits ONLY the
// thread-action-lock acquisition, which is the one wait here that can be
// abandoned with no side effects (`LockCtx`'s contract). The workflow runner's
// takeover stop uses that bound so a wedged holder of this lock — a session
// restart mid-start, the incident shape — refuses the takeover instead of
// freezing the engine's command loop under an uncancellable acquisition.
// Everything past the acquisition, the provider interrupt ack included, runs
// exactly as an unbounded call would: provider IO in flight is never wired to
// a cancellation, because "cancelled" there is indistinguishable from
// "delivered".
//
// Its callers, the workflow runner and an agent's cancel, pass a stop with
// no consent, so live background agents never refuse it.
func (a *App) interruptTurnCtx(ctx context.Context, threadID string, stop turnStop) error {
	_, err := a.interruptTurnAtIndex(ctx, threadID, anyOpenTurn, stop)
	return err
}

// anyOpenTurn is the turn fence of an interrupt that means whatever the
// thread is running: a person's own stop, and a workflow takeover.
const anyOpenTurn = -1

// interruptTurnAtIndex is interruptTurnCtx fenced on the turn the caller
// means to stop. A non-negative expectTurnIndex interrupts only while THAT
// turn is the open one, which is what keeps an agent request's cancel from
// taking a later turn away from whoever started it; anyOpenTurn interrupts
// whatever is running. It reports whether the interrupt was sent, so a caller
// that must say what it stopped does not have to guess.
//
// stop names who is stopping the turn, recorded for the requests it
// answers. The user's Stop carries its consent: the call returns a
// backgroundKillRefusal while the interrupt would kill a live background
// agent the consent does not cover. Any other stop is never refused. The
// check runs before the parked-call cancels below and again under the thread
// action lock, where an agent launched after the first check is still seen
// before the interrupt.
func (a *App) interruptTurnAtIndex(ctx context.Context, threadID string, expectTurnIndex int, stop turnStop) (bool, error) {
	if !stop.valid() {
		return false, fmt.Errorf("interrupt turn %s: no stop cause", threadID)
	}
	consent := stop.consent
	if a.shuttingDown.Load() {
		return false, ErrShuttingDown
	}
	if current, ok := a.sessionManager().get(threadID); ok {
		if err := a.refuseBackgroundKill(threadID, current, consent); err != nil {
			return false, err
		}
	}
	// A tool call parked on a remote command returns at once as backgrounded;
	// the command itself keeps running and the tray still owns stopping it.
	// A tool call parked on another thread's request returns the same way:
	// the request keeps running and the answer arrives as a message. An
	// interrupt of whatever is running ends them before it waits for the
	// lock; a fenced one only once it knows the turn is the one it names,
	// because the calls belong to whatever turn the thread is running.
	if expectTurnIndex == anyOpenTurn {
		a.cancelParkedTurnCalls(threadID)
	}
	// Capture before waiting on the app-level thread lock. A Codex revert holds
	// that lock across its provider and SQLite halves; an interrupt that entered
	// before or during the cut must not wake afterward and hit the reloaded
	// thread. The provider fence also covers direct Session callers that do not
	// know about this lock.
	observed := a.observeInterruptTarget(threadID)
	// The whole interrupt — session capture, pre-ack Mark, ack wait,
	// and the post-ack bookkeeping/promote/eager block — runs under the
	// thread action lock: the same lock the session-start funnel holds
	// across MarkThreadActive + spawn, the death drain holds across its
	// restore, and the dispatch worker holds per batch. Without it a
	// replacement could reactivate the thread between the post-ack
	// epoch checks and their store writes, and the Codex resend could
	// go through a captured session already swapped out (round-12,
	// CT12-2/CT12-3/C12-2). Holding it across the ack wait is safe because
	// the provider event worker never blocks on this lock.
	// The in-triage epoch fences stay as defense for an interrupt that
	// acquires this lock only after a replacement completed.
	unlock, err := a.threadLocks().LockCtx(ctx, threadID)
	if err != nil {
		return false, fmt.Errorf("interrupt turn %s: thread action lock: %w", threadID, err)
	}
	defer unlock()
	return a.interruptTurnHoldingLock(threadID, expectTurnIndex, stop, observed)
}

// cancelParkedTurnCalls ends the tool calls a thread's turn has parked on a
// remote command or on another thread's request.
func (a *App) cancelParkedTurnCalls(threadID string) {
	a.cancelRemoteWaits(threadID)
	a.cancelThreadRequestWaits(threadID)
}

// interruptTarget is the session an interrupt saw before it took the thread
// action lock, which fences it against a revert or a replacement that ran
// while it waited.
type interruptTarget struct {
	seen  bool
	token string
	codex *codex.Session
	fence codex.InterruptFence
}

func (a *App) observeInterruptTarget(threadID string) interruptTarget {
	current, ok := a.sessionManager().get(threadID)
	if !ok {
		return interruptTarget{}
	}
	observed := interruptTarget{seen: true, token: current.Token}
	if current.Codex != nil {
		observed.codex = current.Codex
		observed.fence = current.Codex.CaptureInterruptFence()
	}
	return observed
}

// interruptTurnHoldingLock is interruptTurnAtIndex past its lock: the
// caller holds the thread action lock, and observed is the session it saw
// before taking it, or under it. An interrupt of whatever is running has
// ended the turn's parked calls already.
func (a *App) interruptTurnHoldingLock(threadID string, expectTurnIndex int, stop turnStop, observed interruptTarget) (bool, error) {
	if !stop.valid() {
		return false, fmt.Errorf("interrupt turn %s: no stop cause", threadID)
	}
	consent := stop.consent
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return false, nil
	}
	if observed.seen && sess.Token != observed.token {
		// The work visible when the interrupt entered is gone. A replacement
		// session may already be processing a later send, so the stale intent
		// must not be retargeted onto it.
		return false, nil
	}
	fencedCodex, interruptFence := observed.codex, observed.fence

	providerSess := sess.ProviderSession()
	if providerSess == nil {
		return false, fmt.Errorf("session has no provider")
	}
	if err := a.refuseBackgroundKill(threadID, sess, consent); err != nil {
		return false, err
	}
	// Sampled BEFORE the interrupt ack: the event worker keeps handling
	// wire events while it is awaited, so the cut turn can settle in the gap and
	// a later sample would miss it (round-5, R5-4). The sample is also
	// PUBLISHED onto the unconsumed pending flush entries pre-ack: the
	// CLI's mid-loop queue drain can echo one back during the ack wait,
	// and a post-ack stamp would let that echo settle the cut turn
	// "end_turn" (round-6, R6-4; broadened past still-deferred entries
	// in round-7, R7-5). If the interrupt request itself fails the
	// previous stamps are RESTORED — not wiped: an entry eager-persisted
	// by an earlier interrupt still carries that interrupt's valid
	// stamp.
	pendingTUISendTurn := a.claudeTUIPendingSendTurn(threadID, sess)
	sample := triage.InterruptSample{Turn: anyOpenTurn}
	if a.triage != nil {
		sample = a.triage.SampleInterrupt(threadID)
	}
	// interruptedTurn is the open turn. cutTurn is the turn this stop
	// cuts: the open one, or the turn of a send the provider holds whose
	// turn has not opened yet.
	interruptedTurn, cutTurn := sample.Turn, sample.CutTurn()
	if expectTurnIndex != anyOpenTurn && cutTurn != anyOpenTurn && cutTurn != expectTurnIndex {
		// The thread has moved on to a turn this caller does not own, open
		// or about to open. Stopping it would take work away from whoever
		// started it.
		return false, nil
	}
	var stampToken triage.FlushStampToken
	if a.triage != nil {
		stampToken = a.triage.MarkFlushSendsInterrupted(threadID, interruptedTurn)
	}
	discardsMessage := a.claudeTUIStopDiscardsMessage(threadID, sess, interruptedTurn, pendingTUISendTurn)
	a.recordTurnStop(threadID, a.stoppedTurnOf(threadID, sess.Token, cutTurn, interruptedTurn), stop)
	if expectTurnIndex != anyOpenTurn {
		a.cancelParkedTurnCalls(threadID)
	}
	var interruptErr error
	interruptSent := true
	if sess.Codex != nil && sess.Codex == fencedCodex {
		interruptSent, interruptErr = sess.Codex.InterruptIfCurrent(context.Background(), interruptFence)
	} else {
		interruptErr = providerSess.Interrupt(context.Background())
	}
	if interruptErr != nil {
		a.dropTurnStop(threadID, cutTurn)
		if a.triage != nil {
			a.triage.RestoreFlushSendsInterrupted(threadID, stampToken)
		}
		return false, interruptErr
	}
	if !interruptSent {
		a.dropTurnStop(threadID, cutTurn)
		// The intent entered before a same-session revert and therefore names
		// the history that operation already discarded. Do not synthesize a
		// second user interrupt in triage for the reloaded thread.
		if a.triage != nil {
			a.triage.RestoreFlushSendsInterrupted(threadID, stampToken)
		}
		return false, nil
	}
	if a.triage != nil {
		// The pre-ack sample, not a fresh resolution: a queued echo
		// consumed during the ack wait may have opened its own turn, and
		// the stopped bookkeeping belongs on the turn the user cut
		// (round-11, C11-1).
		if _, err := a.triage.MarkInterrupt(threadID, sample, stampToken, stop.label); err != nil {
			// The provider stopped; the thread's record of the stop did
			// not land, so its rows can still read as running.
			log.Printf("app: interrupt turn: mark interrupt: %v", err)
			a.emitErrorToThread(threadID, fmt.Sprintf("The turn stopped, but recording the stop failed: %v", err))
		}
		if sess.ACP != nil {
			// Drain queued provider events (confirmations + EventTurnComplete)
			// emitted during sess.ACP.Interrupt before clearing cancelled
			// unconfirmed sends or eagerly persisting/resending steered items.
			if drainErr := a.providerEvents.drain(threadID); drainErr != nil {
				log.Printf("app: interrupt turn: drain provider events for %s: %v", threadID, drainErr)
			}
			a.triage.ClearCancelledACPDirectSends(threadID, stampToken)
		}
		a.eagerPersistFlushSendsOnInterrupt(threadID, sess, interruptedTurn, stampToken)
	}
	if discardsMessage {
		a.noticeClaudeTUIDiscardedMessage(threadID)
	}
	return true, nil
}

// eagerPersistFlushSendsOnInterrupt makes queued user messages
// visible in the chat timeline immediately on interrupt. Two paths:
//
//  1. Deferred sends (DeferredItem != nil): persisted via
//     EagerPersistDeferredFlushSends, which writes the row and emits.
//  2. Quietly-persisted sends (DeferredItem == nil, :flush: ID):
//     already in the store from dispatch-time PersistItemQuiet.
//     PromoteQuietFlushSends emits provider:item_event so they
//     transition from Zone 2 to the timeline.
//
// For Codex, deferred sends steered into the running turn are also re-sent
// as a fresh turn because Codex discards steered pending_input on
// turn/interrupt. A send that went out as the input of a turn it starts
// (EagerPersistedFlush.TurnInput) is that turn's own input and is not
// re-sent.
//
// interruptedTurn is the caller's pre-ack OpenTurnIndex sample (-1
// when no turn was open) — see EagerPersistDeferredFlushSends for why
// it cannot be sampled here (round-5, R5-4). stampToken is the pre-ack
// mark's token, fencing the eager stamp against newer concurrent
// interrupts and session replacement (round-9, R9-5/R9-6).
func (a *App) eagerPersistFlushSendsOnInterrupt(threadID string, sess session, interruptedTurn int, stampToken triage.FlushStampToken) {
	// Message-anchor recording for both paths happens INSIDE the router
	// calls, under the thread's flush anchor lock, via the confirmed
	// hook. This is the rows' baseline anchor (rollback stays offered if
	// the session dies before the echo); the echo-time hook later stamps
	// the provider ids at the consumption boundary (round-7, R7-1). The
	// baseline must commit before the mutex releases or an echo in the
	// gap stamps ids onto an anchor that doesn't exist yet (round-4
	// review, CT4-1).
	a.triage.PromoteQuietFlushSends(threadID, stampToken)

	if sess.Codex == nil && sess.ACP == nil {
		a.triage.EagerPersistDeferredFlushSends(threadID, interruptedTurn, stampToken)
		return
	}
	var persisted []triage.EagerPersistedFlush
	var clearTurnInputIDs []string
	for _, flush := range a.triage.EagerPersistDeferredFlushSends(threadID, interruptedTurn, stampToken) {
		if !flush.TurnInput {
			persisted = append(persisted, flush)
		} else if sess.ACP != nil {
			// On ACP, session/cancel cancels the idle-queued prompt itself
			// without sending a confirmation echo; clear its now-orphaned
			// pending entry after eager persistence places the row in timeline.
			clearTurnInputIDs = append(clearTurnInputIDs, flush.UserItemID)
		}
	}
	if len(clearTurnInputIDs) > 0 {
		a.triage.ClearPendingSendsByItemIDs(threadID, clearTurnInputIDs)
	}
	if len(persisted) == 0 {
		return
	}

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("app: eager persist flush: load thread %s: %v", threadID, err)
		// The resend cannot run, and Codex discarded the queued input at
		// turn/interrupt — the entries' echoes will never come. Clear
		// them (a lingering FIFO entry would mis-pair the next send's
		// echo) and restore the messages to the composer draft, the
		// provider-native recovery for input Codex never consumed.
		ids := make([]string, len(persisted))
		for i, p := range persisted {
			ids[i] = p.UserItemID
		}
		a.triage.ClearPendingSendsByItemIDs(threadID, ids)
		a.restoreEagerPersistedFlushesToDraft(threadID, persisted)
		return
	}

	a.codexResendAfterInterrupt(threadID, sess, persisted, thread)
}

// codexResendAfterInterrupt clears stranded pending sends and re-sends
// eagerly-persisted flush items as a fresh Codex turn. Codex discards
// steered pending_input on turn/interrupt (codex-rs abort_all_tasks →
// input_queue.clear_pending), so the client must re-submit — mirroring
// the Codex TUI's submit_pending_steers_after_interrupt flow, merged
// into one message like the TUI's merge_user_messages resubmit.
//
// Failure posture also mirrors the TUI: input the model never consumed
// is restored to the composer draft
// (restoreEagerPersistedFlushesToDraft), never left in the timeline
// looking sent. Only the delivery-ambiguous turn/start timeout keeps
// its pending entry — the turn may be running and a restore would
// double-send.
func (a *App) codexResendAfterInterrupt(
	threadID string,
	sess session,
	persisted []triage.EagerPersistedFlush,
	thread store.Thread,
) {
	ids := make([]string, len(persisted))
	for i, p := range persisted {
		ids[i] = p.UserItemID
	}
	a.triage.ClearPendingSendsByItemIDs(threadID, ids)

	// Each row goes back on the wire as its dispatch first sent it: marked
	// with its sender and with its composer command expanded, neither of
	// which the stored row holds.
	contents := make([]string, len(persisted))
	for i, p := range persisted {
		content, err := a.rowWireText(threadID, p.Content, p.Meta)
		if err != nil {
			log.Printf("app: codex interrupt re-send: rebuild %s/%s: %v", threadID, p.UserItemID, err)
			a.restoreEagerPersistedFlushesToDraft(threadID, persisted)
			return
		}
		contents[i] = content
	}

	// The original dispatch delivered the turn's attachments alongside the
	// steer; this resend is the content's only remaining delivery, so it
	// must carry them too (round-14, CT14-2) — images in the provider
	// slice, files as prompt lines. IDs come from the persisted rows'
	// meta — the same source the requeue payload uses.
	var attachmentIDs []string
	for _, p := range persisted {
		ids, err := attachmentIDsFromUserMeta(p.Meta)
		if err != nil {
			log.Printf("app: codex interrupt re-send: decode attachments for %s/%s: %v", threadID, p.UserItemID, err)
			continue
		}
		attachmentIDs = append(attachmentIDs, ids...)
	}
	attachments, err := a.resolveSendMessageAttachments(threadID, attachmentIDs)
	if err != nil {
		log.Printf("app: codex interrupt re-send: resolve attachments for %s: %v", threadID, err)
		a.restoreEagerPersistedFlushesToDraft(threadID, persisted)
		return
	}

	// This resend is the merged content's only remaining delivery, so it
	// carries the file lines too — the original dispatch put them on its
	// providerContent, which Codex discarded with the rest of the pending
	// input at turn/interrupt.
	merged := attachments.promptText(contents, "\n\n")
	// The merged re-send is correlated to the FIRST item's row, which is
	// the row the pending entry below is registered for. Items 2+ share
	// that correlation, exactly as they share the turn.
	resendUUID := ""
	if sess.ACP != nil {
		resendUUID = uuid.NewString()
	}
	resendClientID, resendExpect := providerSendIdentity(sess, persisted[0].UserItemID, persisted[0].Meta, resendUUID)
	sendOpts := provider.SendOptions{
		InteractionMode:     provider.NormalizeInteractionMode(thread.Mode),
		Attachments:         attachments.images,
		UserMessageUUID:     resendUUID,
		ClientUserMessageID: resendClientID,
	}

	turnIndex, err := a.nextSendTurnIndex(threadID)
	if err != nil {
		log.Printf("app: interrupt re-send: resolve turn index for %s: %v", threadID, err)
		a.restoreEagerPersistedFlushesToDraft(threadID, persisted)
		return
	}

	// Register a non-deferred pending send for the first item so the
	// echo stamps provider_item_id via attachProviderItemIDToUserRow.
	// Items 2+ are merged into the same turn and share the first
	// item's provider correlation. The row was already persisted at its
	// interrupt position by EagerPersistDeferredFlushSends — anchor the
	// fresh entry so the echo's :flush: bump doesn't move it again.
	a.triage.RegisterPendingFlushResendWithExpectation(
		threadID, persisted[0].UserItemID, turnIndex, resendExpect)
	a.triage.MarkPendingSendAnchoredAtInterrupt(threadID, persisted[0].UserItemID)
	sess.Liveness.BumpActivity(time.Now())

	var sendErr error
	if sess.Codex != nil {
		sendErr = sess.Codex.Send(context.Background(), merged, sendOpts)
	} else if sess.ACP != nil {
		sendErr = sess.ACP.Send(context.Background(), merged, sendOpts)
	}
	if sendErr != nil {
		if sess.Codex != nil && codex.IsAmbiguousTurnStartTimeout(sendErr) {
			// The turn/start was written; the turn — and its echo — may
			// already be running. Requeueing would re-send content Codex
			// may have consumed (round-14, D14-2). Keep the pending entry:
			// a late echo settles it normally, and if the turn truly never
			// started the session-death drain recovers the rows.
			log.Printf("app: interrupt re-send for thread %s timed out after write; leaving pending confirmation for provider echo", threadID)
			return
		}
		log.Printf("app: interrupt re-send for thread %s: %v", threadID, sendErr)
		a.triage.ClearPendingSendForFailure(threadID, persisted[0].UserItemID)
		// The provider discarded the queued input on interrupt and this
		// resend was its only delivery — the rows would sit in the
		// timeline looking sent. Delete them and hand the content back
		// to the composer.
		a.restoreEagerPersistedFlushesToDraft(threadID, persisted)
	}
}

// teardownDeadPreInitSession reaps a session whose start failed before
// it initialized: the provider reported a startup failure
// (provider.IsStartupFailure; for example a fork start's resume cursor is
// unusable, or the conversation to resume is gone), or a caller that
// waited on the start could not prove it (ensureProvenSession). It
// cannot serve sends and could otherwise linger alive forever: Claude
// does not exit after a failed --resume-session-at validation. The orphan
// error item persisted by triage is the user-facing surface; this
// teardown stops the useless process, hands any queued sends back to the
// composer draft, and sweeps the failed send's pending state so the next
// send lazy-starts fresh. It returns the close's failure.
//
// The teardown runs whole under the thread action lock: lock, unregister
// by token, restore the queue, close. Whoever holds the lock owns it. The
// event handler's reaper runs it on its own goroutine (see
// sessionEventHandler), since its caller is the thread's event worker and
// the close drains that worker's queue; it waits for the lock, so a caller
// already tearing the session down under the lock (ensureProvenSession)
// does all of it and returns the close's result, and the reaper then finds
// the token gone and does nothing. Holding the lock across the close cannot
// deadlock: the event worker never waits on it.
//
// Two guards keep it from harming a start that replaces the session:
//
//   - The token-guarded unregister makes the teardown a no-op once a start
//     replaced this session in the registry. A start that holds the action
//     lock does that, or has not begun, before the teardown gets the lock.
//   - The triage epoch, captured before the unregister, covers a start that
//     does not take the action lock (a workflow takeover's restart): its
//     MarkThreadActive bumps the epoch before it spawns, and the queue
//     restore and the triage cleanup run only while the epoch is unchanged.
//     CleanupThreadIfEpoch rechecks under the router lock.
//
// Not routed through teardownAndCloseSession: that helper's
// unconditional CleanupThread is exactly what must not run here once a
// replacement has claimed the thread.
func (a *App) teardownDeadPreInitSession(threadID, sessionToken string) error {
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	return a.teardownDeadPreInitSessionLocked(threadID, sessionToken)
}

// teardownDeadPreInitSessionLocked is teardownDeadPreInitSession for a
// caller that holds the thread action lock.
func (a *App) teardownDeadPreInitSessionLocked(threadID, sessionToken string) error {
	var epoch uint64
	if a.triage != nil {
		epoch = a.triage.ThreadEpoch(threadID)
	}
	sess, ok := a.sessionManager().unregister(threadID, sessionToken)
	if !ok {
		return nil
	}
	a.emitProviderSessionDisconnected(threadID, sess.Provider)
	if a.triage != nil {
		// Restore before cleanup: the sweep drops the thread's triage
		// state, queued items included, and they only exist in router memory —
		// without the restore the user's queued text would vanish.
		var requeued []triage.UnconfirmedFlushItem
		if a.triage.ThreadEpoch(threadID) == epoch {
			requeued = a.restoreUnconfirmedQueueLocked(threadID)
		}
		if !a.triage.CleanupThreadIfEpoch(threadID, epoch) {
			log.Printf("app: skipped triage cleanup for thread %s — a replacement session reactivated it mid-teardown", threadID)
		} else {
			// Successful draft restores already settled injected workflow wakes;
			// failed restores requeued the same still-pending settlement below.
			// Releasing all usage claims here would detach those survivors from
			// their durable storm-suppression record.
			if len(requeued) > 0 {
				// The restore REQUEUED these (failed stale-row cleanup or
				// failed draft restore) and the cleanup just wiped the queue
				// they re-entered — re-register them so the next start's
				// funnel flush still finds them (round-13, D13-1). When the
				// cleanup was skipped, the queue was never wiped and they
				// are still registered.
				a.requeueUnconfirmedFlushItems(threadID, requeued, queueRestoredReasonSessionDied)
				a.emitQueueStateChanged(threadID)
			}
		}
	}
	var err error
	if a.closeProviderSessionFn != nil {
		err = a.closeProviderSessionFn(threadID)
	} else {
		err = a.closeProviderSession(threadID, sess)
	}
	if err != nil {
		return fmt.Errorf("teardown dead pre-init session for thread %s: %w", threadID, err)
	}
	return nil
}

// teardownAndCloseSession runs the per-thread triage cleanup
// and closes the provider subprocess, dropping the thread's queue. Shared by
// StopSession (user action) and stopArchivedThreadSession (archive releasing
// the thread's host resources) so the close sequence stays in one place —
// future per-thread cleanup steps land once and all paths inherit it.
// teardownDeadPreInitSession mirrors the sequence but owns its own copy: its
// CleanupThread must be epoch-guarded against a racing replacement start,
// and it restores the flush queue first.
//
// Callers must remove the session from a.sessions BEFORE invoking so
// two concurrent closers can't both call Close on the same provider
// session. A zero-value sess argument is intentionally tolerated:
// closeProviderSession returns nil for it, which lets StopSession
// reuse this helper to scrub triage state on a thread that
// never had a session registered.
//
// Not shared with unregisterSession (readLoop disconnect): that path
// runs after the provider subprocess has already exited (a Claude TUI
// session has also released its own listeners, feed loop, parser and
// prompt files by then), so calling closeProviderSession would be
// redundant, and it deliberately leaves
// triage state alone so the final wire frames have somewhere to land.
func (a *App) teardownAndCloseSession(threadID string, sess session) error {
	return a.teardownSession(threadID, sess, false, dropQueue)
}

// teardownAndCloseSessionHoldingMutations is teardownAndCloseSession for a
// caller that holds the thread's mutation lock (the idle reaper, which
// tears down only a session with nothing queued).
func (a *App) teardownAndCloseSessionHoldingMutations(threadID string, sess session) error {
	return a.teardownSession(threadID, sess, true, dropQueue)
}

// teardownAndCloseSessionRestoringQueue is the teardown of a stop nobody
// asked for: a removed worktree, a reconnect. The thread's queued and
// unconfirmed messages go back to the composer, as they do when a session
// dies. The caller holds the thread action lock and not its mutation lock.
func (a *App) teardownAndCloseSessionRestoringQueue(threadID string, sess session) error {
	return a.teardownSession(threadID, sess, false, restoreQueue)
}

// teardownQueue is what a session teardown does with the messages queued
// behind the session and the ones it sent and never saw confirmed.
type teardownQueue uint8

const (
	// dropQueue throws them away, durable rows included: the person's
	// own Stop of the session, and archive.
	dropQueue teardownQueue = iota
	// restoreQueue hands them back to the composer.
	restoreQueue
)

// errBackgroundNotRetired marks a teardown whose provider closed but whose
// Codex background calls could not be marked ended. They stopped with the
// process; their rows still show as running until the thread's next session
// start or the next app start retires them.
var errBackgroundNotRetired = errors.New("background calls still marked running")

func (a *App) teardownSession(threadID string, sess session, mutationsHeld bool, queue teardownQueue) error {
	a.workflowApplication().ReleaseUsageAttentionForThread(threadID)
	// Events the provider emitted before the stop were read and must be
	// persisted; CleanupThread below marks the thread stopped, after which
	// triage drops them.
	a.drainProviderEvents(threadID, "stop session")
	var held []ownedMessage
	var requests teardownRequests
	var restore flushRestore
	if a.triage != nil {
		held, requests, restore = a.settleQueueForTeardown(threadID, mutationsHeld, queue)
	}
	// The answers and remote completions the drop held go back to their
	// requests and watches once the session is closed, so a completion the
	// watcher queues again starts a session only after this one is gone.
	// Other threads' requests go back on the queue the cleanup emptied; a
	// caller holding the mutation lock a queue write takes (the idle
	// reaper) has them queued once it lets go. A restore's app messages
	// are settled the same way, after the mutation lock is released.
	defer a.finishFlushRestore(threadID, restore)
	defer a.giveBackToOwners(threadID, held)
	if mutationsHeld && !requests.empty() {
		defer a.goThreadRequestSettling(func() { a.giveBackTeardownRequests(threadID, requests) })
	} else {
		defer a.giveBackTeardownRequests(threadID, requests)
	}
	var err error
	if a.closeProviderSessionFn != nil {
		err = a.closeProviderSessionFn(threadID)
	} else {
		err = a.closeProviderSession(threadID, sess)
	}
	if err == nil && sess.Provider == string(provider.Codex) {
		if retireErr := a.retireCodexBackgroundRuntime(threadID); retireErr != nil {
			err = fmt.Errorf("%w: %w", errBackgroundNotRetired, retireErr)
		}
	}
	if a.triage != nil {
		// The provider process is gone, so its backgrounded shells are
		// too — including subagent-owned NESTED launches, which the
		// top-level lifecycle gates never settle (invariant 24). Write
		// their session_died completion siblings now instead of leaving
		// tray zombies until the next app boot. Self-filtering: the
		// query only matches Claude-provider threads, and callers hold
		// the thread action lock, so a replacement session cannot have
		// started new launches yet.
		if _, err := a.triage.SettleBackgroundLaunchesForSessionEnd(threadID); err != nil {
			log.Printf("app: settle background launches on session close for thread %s: %v", threadID, err)
			a.emitErrorToThread(threadID, triage.BackgroundSettleFailureSummary(err))
		}
		// Its prompts went with the process, and so do their copies routed
		// to the thread that spawned it.
		a.settleEndedWorkerPrompts(threadID)
	}
	if sess.Provider != "" {
		a.emitProviderSessionDisconnected(threadID, sess.Provider)
	}
	// After the provider process is closed no flush tick can re-register
	// seeder state; a thread killed mid-stream would otherwise strand
	// its entries (no final tick ever arrives to clear them).
	a.highlightService().PurgeThread(threadID)
	return err
}

// settleQueueForTeardown empties the thread's queue for a teardown, durable
// rows and memory, and then runs the triage cleanup. A drop discards the
// messages: a row that outlived the queue it belonged to would put the
// message back in the composer at the next boot, long after the user's Stop
// threw it away. A restore returns them to the composer first, and keeps
// queued, durably, any message whose restore failed.
//
// Either way the triage cleanup finds no send still pending: a session's end
// is where a message it sent and never confirmed stops being pending, so its
// row leaves the timeline here with its text, to the composer or with the
// dropped queue (itemmeta.SetPendingFlush). The sweep would otherwise forget
// the send and leave its row marked as history.
//
// A drop returns the answers and remote completions it held for their
// owners, and the other threads' requests it took back before the agent
// took them in (returnRequestsAtTeardown). Among the messages the provider
// was handed and has not confirmed, an answer, a reminder or a completion is
// held too: its row went at the provider write, and nothing else would tell
// its owner it was not taken in.
//
// It runs under the thread's mutation lock, which every queue write takes
// (registerQueueItem), so a message queued during a teardown is either in
// the queue the teardown empties, and settled with it, or queued once the
// memory cleanup is done, durable and in memory for the next session. A
// drop takes nothing else; a restore needs the thread action lock its
// caller holds. A restore returns what it owes the app's messages for the
// caller to settle once the lock is released (finishFlushRestore): settling
// requeues other threads' requests, which is a queue write.
func (a *App) settleQueueForTeardown(threadID string, mutationsHeld bool, queue teardownQueue) ([]ownedMessage, teardownRequests, flushRestore) {
	if !mutationsHeld {
		defer a.threadApplication().LockMutations(threadID)()
	}
	var held []ownedMessage
	var requests teardownRequests
	var restore flushRestore
	switch queue {
	case restoreQueue:
		// The restore settles what the app wrote as a session end does
		// (agentPartsNotTakenIn): answers and completions back to their
		// owners, other threads' requests back to their queue.
		restore = a.restoreUnconfirmedQueueOwing(threadID)
	case dropQueue:
		fallthrough
	default:
		requests = a.returnRequestsAtTeardown(threadID)
		var dispatched []triage.QueuedFlushItem
		held, dispatched = a.dropFlushQueueLocked(threadID)
		// Read before dropUnconfirmedSends drains the sends they name.
		held = append(held, a.unconfirmedOwnedMessages(threadID, held)...)
		a.dropUnconfirmedSends(threadID, dispatched, requests)
	}
	a.triage.CleanupThread(threadID)
	if len(restore.requeued) > 0 {
		// The cleanup wiped the queue these re-entered: they are queued
		// again under the same hold of the mutation lock as the restore
		// read their rows, and finishFlushRestore announces the queue.
		var failures requeueOutcome
		restore.requeued, failures = a.requeueUnconfirmedFlushItemsLocked(threadID, restore.requeued, queueRestoredReasonSessionDied)
		restore.requeueFailed = restore.requeueFailed.join(failures)
	}
	return held, requests, restore
}

// unconfirmedOwnedMessages names the answers, reminders and completions
// among the sends the provider was handed and has not confirmed, each once
// and none that held already names. The send of a joined message names
// them all; the person's text joined with them stays the teardown's.
func (a *App) unconfirmedOwnedMessages(threadID string, held []ownedMessage) []ownedMessage {
	var owned []ownedMessage
	named := func(id string) bool {
		return slices.ContainsFunc(held, func(m ownedMessage) bool { return m.sendID == id }) ||
			slices.ContainsFunc(owned, func(m ownedMessage) bool { return m.sendID == id })
	}
	for _, item := range a.triage.UnconfirmedFlushSends(threadID) {
		for _, id := range unconfirmedMembersOf(item).ids {
			if ownedSendID(id) && !named(id) {
				owned = append(owned, ownedMessage{sendID: id, unconfirmed: true})
			}
		}
	}
	return owned
}

// applyClaudeSessionAxes stamps the Claude-only `--settings` block axes
// (output style, subagent fan-out caps, tool memory limit) onto a freshly
// built Config.
//
// The cross-session inbox policy is deliberately NOT here even though it
// rides the same `--settings` block: turning the inbox on or off changes
// whether a running session is reachable by other sessions at all, which
// is worth a deferred restart rather than "next session, whenever that
// is". It therefore travels on `provider.SessionOptions` instead — see
// `applySettingsOwnedAxes` and `claude.ConfigFromOptions`.
//
// They live HERE — in the spawn wiring, beside the binary path and the
// process environment — rather than on `provider.SessionOptions`, and
// that placement is the feature's "next sessions only" contract. The
// config reconciler diffs `ConfigFromOptions(prev)` against
// `ConfigFromOptions(next)`; neither carries these fields, so saving one
// of these settings can never queue a restart on a running session, and
// there is nothing for `reconcileSettingsOwnedAxes` to pin. A restart that
// happens for some other reason rebuilds from scratch and picks the new
// values up, which is a new session and intended — the same shape the
// prompt-override / disabled-tool axes use
// (docs/specs/prompt-tool-overrides.md).
//
// One settings snapshot feeds all three, so a save landing mid-spawn
// cannot produce a session whose axes came from two versions of the file.
func applyClaudeSessionAxes(cfg *claude.Config, axes settings.ClaudeSessionAxes) {
	cfg.OutputStyle = axes.OutputStyle
	cfg.MaxSubagentSpawnDepth = axes.SubagentLimits.MaxSpawnDepth
	cfg.MaxConcurrentSubagents = axes.SubagentLimits.MaxConcurrent
	cfg.ToolMemoryLimit = axes.ToolMemoryLimit
}

// claudeAgentLaunchLookup answers the Claude parser's question about an
// agent launch an earlier process ran: the thread's history record of it
// (claude.Config.AgentLaunchByTask).
func (a *App) claudeAgentLaunchLookup(threadID string) func(taskID string) (claude.AgentLaunchRecord, bool, error) {
	return func(taskID string) (claude.AgentLaunchRecord, bool, error) {
		launch, found, err := a.triage.AgentLaunchByTask(threadID, taskID)
		if err != nil || !found {
			return claude.AgentLaunchRecord{}, found, err
		}
		return claude.AgentLaunchRecord{ToolUseID: launch.ID, Effort: launch.NamedEffort, Model: launch.Model}, true, nil
	}
}
