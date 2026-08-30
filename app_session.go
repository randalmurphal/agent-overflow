package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/provider"
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
// bindings. Start/stop plumbing that coordinates multiple in-flight
// start attempts lives in app_session_start.go; thin reconnect/switch
// wrappers live in app_session_bindings.go. Everything in this file
// runs the actual spawn and shutdown of a provider subprocess for a
// given thread.

// codexReopenReconcileTimeout bounds the on-reopen probe that asks
// the Codex app-server whether the thread needs a `thread/resume`
// after a session start. Runs in a background goroutine; the user
// has already seen the session come up.
const codexReopenReconcileTimeout = 30 * time.Second
const codexAccountReadTimeout = 8 * time.Second

// StartSession is the Wails-bound entry point for "bring this thread's
// provider subprocess up." The sendMessage path also calls
// startSessionNow via runSessionStart when a thread has no active
// session yet.
//
// Takes the per-thread action lock: an unserialized start racing a
// revert can read the pre-revert SessionRef, clear the stopped-thread
// gate mid-revert (MarkThreadActive), and register a session bound to
// the old provider thread after the revert repointed the row. Internal
// callers that already hold the lock (sends, deferred config restarts)
// go through a.startSession / runSessionStart directly.
func (a *App) StartSession(threadID string) error {
	return a.startSessionTakingLock(context.Background(), threadID)
}

// startSessionTakingLock is StartSession's body with a caller-supplied context.
// The bound method cannot take one — Wails generates the TS wrapper from its
// signature — so internal callers that DO have a cancellable context (the
// workflow runner's start path) come through here instead.
//
// ctx bounds the two waits that have performed no side effects yet: the
// per-thread action lock and the join on somebody else's in-flight start.
// Once the spawn itself begins it runs to completion.
func (a *App) startSessionTakingLock(ctx context.Context, threadID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	unlock, err := a.threadLocks().LockCtx(ctx, threadID)
	if err != nil {
		return fmt.Errorf("start session for thread %s: %w", threadID, err)
	}
	defer unlock()
	return a.startSession(ctx, threadID)
}

// startSessionNow builds the provider-specific launch config, stops
// any prior session on the thread, and spawns a fresh one. Callers go
// through StartSession / runSessionStart so concurrent start attempts
// share a single spawn instead of racing.
func (a *App) startSessionNow(threadID string) error {
	return a.startSessionNowWithClaudeResumeAt(threadID, "")
}

// buildSessionOptions composes the provider-agnostic SessionOptions bundle
// (plus the design-mode config it derives from) for a thread row. Shared by
// the spawn path and the live config reconciler (app_session_config.go) so
// both see the exact same view of "what a session for this row looks like".
//
// Design-mode plumbing (extra system prompt + Codex MCP servers) is
// caller-owned: the provider package intentionally doesn't know about
// design or discussion. We compose the final system prompt here, then
// hand a provider-agnostic SessionOptions bundle to each provider's
// ConfigFromOptions translator.
//
// What this function does NOT do is stamp the settings-owned axes
// (SystemPrompt when no feature owns it, DisabledTools). Those are read
// from Settings rather than projected from the thread row, so the two
// callers need opposite treatment — the spawn path applies today's values
// (applySettingsOwnedAxes), the reconcile path resolves against the live
// session's (reconcileSettingsOwnedAxes: tool lists pin, a headless-Claude
// prompt override converges live only when its STORED text actually
// changed). Both live in app_session_prompt_override.go. Leaving them out
// here is what keeps the reconciler from composing an override — up to two
// git subprocesses under the per-thread config lock — on every model,
// effort, and runtime-mode change.
func (a *App) buildSessionOptions(t store.Thread) (provider.SessionOptions, designSessionConfig, error) {
	designCfg, err := a.designSessionConfig(t)
	if err != nil {
		return provider.SessionOptions{}, designSessionConfig{}, err
	}
	systemPrompt := a.featureOwnedSystemPrompt(t, designCfg)

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
		systemPrompt,
		forkSession,
	)

	// The fast-mode service tier is a live-catalog fact, so it can come from
	// neither the thread row (which persists only the bool) nor the provider
	// package (which has no catalog access). Stamping it here — the one
	// SessionOptions construction site, shared by the spawn path and the live
	// reconciler — is what keeps a config diff from flapping between a resolved
	// and an unresolved tier for the same thread.
	opts.FastModeTierID = a.fastModeTierIDForModel(t.Provider, t.Model)

	if dir, err := a.designWorkDirOverride(t); err != nil {
		return provider.SessionOptions{}, designSessionConfig{}, fmt.Errorf("resolve design workdir: %w", err)
	} else if dir != "" {
		opts.WorkDir = dir
	}

	return opts, designCfg, nil
}

func (a *App) startSessionNowWithClaudeResumeAt(threadID, claudeResumeAt string) error {
	t, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	sanitized := a.sanitizeThreadModelSettings(t)
	if !chatmodel.SameModelFields(t, sanitized) {
		if err := a.store.UpdateThread(sanitized); err != nil {
			return fmt.Errorf("start session: persist live model settings: %w", err)
		}
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

	opts, designCfg, err := a.buildSessionOptions(t)
	if err != nil {
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
	if err := a.stopExistingSessionLocked(threadID); err != nil {
		return fmt.Errorf("start session: %w", err)
	}

	// Resolve the resume cursor only after the prior session is fully
	// stopped: Close blocks on the read loop, and the CLI can append
	// final transcript rows right up to exit. Scanning before the stop
	// could validate against (or pick a leaf from) a file the dying
	// process is still extending.
	//
	// A fork start resolves against the SOURCE session instead
	// (opts.Resume IS the source ref while PendingForkRef is pending),
	// and only when the fork stored a pin: a pinned lazy fork must cut
	// where its timeline was cloned, so an unresolvable pin FAILS the
	// start rather than falling back to the source's current tail —
	// spawning unpinned there silently snapshots whatever the source
	// has streamed since the fork (the 2026-08-22 skew incident). An
	// unpinned fork (idle source at fork time) keeps the CLI's own
	// tail-cut semantics.
	if t.Provider == string(provider.Claude) && opts.Resume != "" {
		projectsDir, err := a.claudeProjectsDir()
		if err != nil {
			return fmt.Errorf("start session: %w", err)
		}
		if opts.ForkSession {
			if t.PendingForkResumeAt != "" {
				cursor, err := resolveClaudeForkResumeAt(projectsDir, opts.Resume, opts.WorkDir, t.PendingForkResumeAt)
				if err != nil {
					return fmt.Errorf("start session: resolve fork resume cursor: %w", err)
				}
				opts.ResumeAt = cursor
			}
		} else {
			opts.ResumeAt = resolveClaudeResumeAt(projectsDir, opts.Resume, opts.WorkDir, claudeResumeAt)
		}
	}

	// Re-admit the thread's events in triage BEFORE spawning. A prior
	// StopSession left the stopped-thread marker set, and the
	// replacement session can emit before any wire proof-of-life lands:
	// Codex emits EventInit synchronously inside NewSession, and a
	// Claude process that dies during startup (unusable
	// --resume-session-at cursor) emits its only diagnostics pre-init.
	// Clearing here is safe — stopExistingSessionLocked has fully
	// drained the prior session's read loop (Close blocks on it), so no
	// stale frame can slip through. This is the ONLY place the marker
	// is cleared; see triage.MarkThreadActive.
	if a.triage != nil {
		a.triage.MarkThreadActive(threadID)
	}

	// Activate watcher + MCP AFTER stopExistingSessionLocked so the
	// teardown of any prior session for the same thread doesn't stop
	// resources we just allocated. teardownDesignThread on the failure
	// paths below cleans up anything activate created.
	designServers, err := a.activateDesignSession(t)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}

	// The one side effect a rendered system-prompt override carries. Gated
	// on the resolution above (which already knows whether the override won
	// the prompt) and never fatal to the spawn.
	a.ensureClaudeMemoryDir(t, opts.WorkDir, promptOverride)
	// designServers is non-nil only for design threads. Chat/plan
	// sessions on both providers use native MCP discovery from the
	// provider's own config (Claude's per-workspace disabledMcpServers,
	// Codex's global enabled flags) — AO injects nothing, so a config
	// toggle made anywhere is simply what the next session start gets.
	designCfg.MCPServers = designServers
	// The `ao` credential is minted here, before the spawn, because the
	// process env is what carries it. It only becomes usable when the session
	// is registered (sessionManager.put), so a failed spawn leaves nothing
	// behind. See app_ao_session.go.
	credential, err := a.mintAOCredential(t)
	if err != nil {
		a.teardownDesignThread(threadID)
		return fmt.Errorf("start session: %w", err)
	}

	// Retire runtime state owned by the prior Codex app-server before a new
	// subprocess can replay or emit lifecycle events. Child ownership stays
	// persisted for resume, while old live turns and PTYs leave the tray.
	if t.Provider == string(provider.Codex) {
		if err := a.retireCodexBackgroundRuntime(threadID); err != nil {
			a.teardownDesignThread(threadID)
			return fmt.Errorf("start session: %w", err)
		}
	}

	newSess, err := a.spawnProviderSession(threadID, sessionToken, t, opts, designCfg, credential, onEvent)
	if err != nil {
		a.teardownDesignThread(threadID)
		a.emitProviderStatusOnSessionStartError(t.Provider)
		return fmt.Errorf("start session: %w", err)
	}

	a.sessionManager().put(threadID, newSess)
	a.emitProviderSessionAccount(threadID)

	// Register the freshly-spawned process group with the orphan reaper so
	// it's torn down if the app dies before the session closes cleanly
	// (macOS only; no-op elsewhere). Paired with the release in
	// closeProviderSession.
	if ps := newSess.providerSession(); ps != nil {
		a.watchSessionProcess(ps.PID(), sessionToken)
	}

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
	if newSess.codex != nil && opts.Resume != "" {
		go a.reconcileCodexAfterStart(threadID)
	}

	return nil
}

// resolveClaudeResumeAt picks the --resume-session-at cursor for a
// Claude session resume. An explicit cursor (today only the live-tracker
// leaf passed by the context-repair restart, app_claude_context.go) is
// validated against the session file's active parentUuid branch first:
// the tracker is wire-derived and can disagree with the file — the CLI
// appends deferred system/api_error rows with stale parents at the NEXT
// user send, moving the active branch out from under any cursor chosen
// earlier (invariant 28). Claude hard-fails resume on an off-branch
// uuid pre-init, so an unvalidated cursor would brick the restart.
// Off-branch or unverifiable cursors are rejected loudly and the
// branch-aware file scan decides instead; scan failure resumes with no
// cursor at all (claude's own default-leaf semantics).
func resolveClaudeResumeAt(projectsDir, sessionRef, workDir, explicit string) string {
	if explicit != "" {
		onBranch, err := claude.ResumeAtOnActiveBranch(projectsDir, sessionRef, workDir, explicit)
		switch {
		case err != nil:
			log.Printf("start session: validate Claude resume-at %s against session %s: %v — falling back to file scan", explicit, sessionRef, err)
		case onBranch:
			return explicit
		default:
			log.Printf("start session: Claude resume-at %s is off the active branch of session %s — rejecting, falling back to file scan", explicit, sessionRef)
		}
	}
	state, err := claude.ScanSessionLeaf(projectsDir, sessionRef, workDir)
	if err != nil {
		log.Printf("start session: scan Claude session leaf %s: %v", sessionRef, err)
		return ""
	}
	return state.CanonicalLeafUUID
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

// reconcileCodexAfterStart runs the on-reopen reconcile once the Codex
// session is in a.sessions. Called from startSessionNow (not a Wails
// binding) so the caller-visible startup latency isn't paying for a
// probe RPC. Runs in a goroutine — errors are logged because the
// reconcile is best-effort; the session is already up either way.
//
// Split out of startSessionNow so the goroutine body is trivial and
// testable: TestStartSessionTriggersCodexReconcile installs a probe stub
// and asserts this runs exactly when opts.Resume != "".
func (a *App) reconcileCodexAfterStart(threadID string) {
	ctx, cancel := context.WithTimeout(a.lifeCtx(), codexReopenReconcileTimeout)
	defer cancel()
	result, err := a.ReconcileCodexOnReopen(ctx, threadID)
	if err != nil {
		log.Printf("app: reconcile codex on reopen for %s: %v", threadID, err)
		return
	}
	if !result.NeedsResume {
		return
	}

	// notLoaded probe: the session is up but the app-server has dropped
	// the thread from memory (eviction, server restart). Call
	// thread/resume to rehydrate before any future turn would try to
	// send on a stale thread id. Best-effort — if the session has gone
	// away in the meantime Resume will error and we log it; no new work
	// was in flight anyway.
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.codex == nil {
		return
	}
	if err := sess.codex.Resume(ctx); err != nil {
		log.Printf("app: reconcile codex resume for %s: %v", threadID, err)
	}
}

// stopExistingSessionLocked tears down any prior session for the thread
// before we start a replacement. Separated from startSessionNow so the
// caller reads top-down as "stop, compute options, spawn, register".
func (a *App) stopExistingSessionLocked(threadID string) error {
	existing, ok := a.sessionManager().take(threadID)

	if !ok {
		return nil
	}
	a.emitProviderSessionDisconnected(threadID, existing.provider)

	// Thread-scoped design state must be torn down alongside the session
	// so a restart doesn't leak the prior turn's MCP registration.
	a.teardownDesignThread(threadID)
	err := a.closeProviderSession(threadID, existing)
	// The replacement session streams fresh items; the old stream's
	// scanner states are dead weight (their final ticks will never
	// arrive) and a reused item ID would inherit a stale watermark.
	// unregisterSession can't purge for this path — the token was
	// already taken above, so its callback no-ops. Unlike
	// teardownAndCloseSession there is no CleanupThread here (the
	// replacement path deliberately keeps triage live), so the stream
	// persist buffers must be drained explicitly first: Close drained
	// the read loop, but a buffer armed by its final deltas would
	// otherwise fire its 250ms flush AFTER the purge and re-register
	// the state it just removed.
	if a.triage != nil {
		if flushErr := a.triage.FlushThread(threadID); flushErr != nil {
			log.Printf("app: flush stream buffers before seeder purge for %s: %v", threadID, flushErr)
		}
	}
	a.highlightSeeder.purgeThread(threadID)
	return err
}

// sessionProcessEnv is the ONE place a provider subprocess's environment is
// assembled, so anything the app injects reaches Claude and Codex identically:
// the user's custom environment for that provider (settings, applied at spawn),
// the boot-mode overrides (providerExtraEnv — the harness's mock-control
// credentials), the session's CLI credential (AO_ENDPOINT / AO_TOKEN /
// AO_THREAD_ID, plus AO_RUN_ID / AO_PHASE_ID for a workflow phase), and the
// PATH entry that makes `agent-overflow` resolve to this executable (D30).
//
// Precedence is provider config < user custom env < boot-mode overrides < AO
// credential. The user's environment outranks the provider config because it
// is a deliberate override of AO's defaults; it sits below the last two because
// those are contracts, not preferences — the AO names are how the CLI finds its
// own app, and the harness's mock control channel must not be reconfigurable
// from user settings. In practice the layers cannot collide: every name AO
// injects is reserved at the settings boundary (settings.providerenv.go), so a
// user value that would have been shadowed here is refused on save instead of
// vanishing at spawn. The contract itself — which variable is set for which
// kind of session, and what the CLI does when one is missing — lives in
// internal/aocli/AGENTS.md, next to the code that reads it.
//
// PATH is the one entry that merges rather than wins: whatever a provider
// config or boot-mode override put there is kept, with the CLI's directory in
// front. A session that can reach the command but lost its own PATH would be a
// worse trade than the other way round.
func (a *App) sessionProcessEnv(
	providerName string,
	env map[string]string,
	credential aoSessionCredential,
) map[string]string {
	custom := a.providerCustomEnv(providerName)
	if len(custom) == 0 && len(a.providerExtraEnv) == 0 && len(credential.env) == 0 && a.cliBinDir == "" {
		return env
	}
	merged := make(
		map[string]string,
		len(env)+len(custom)+len(a.providerExtraEnv)+len(credential.env)+1,
	)
	for k, v := range env {
		merged[k] = v
	}
	for k, v := range custom {
		merged[k] = v
	}
	for k, v := range a.providerExtraEnv {
		merged[k] = v
	}
	for k, v := range credential.env {
		merged[k] = v
	}
	if a.cliBinDir != "" {
		prependCLIBinDir(merged, a.cliBinDir)
	}
	return merged
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
	designCfg designSessionConfig,
	credential aoSessionCredential,
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

	switch t.Provider {
	case string(provider.Claude):
		cfg := claude.ConfigFromOptions(opts)
		if len(workflowSchema) > 0 {
			cfg.OutputSchema = string(workflowSchema)
		}
		cfg.Binary = a.providerBinaryPath(t.Provider)
		cfg.Env = a.sessionProcessEnv(t.Provider, cfg.Env, credential)
		cfg.EventLogger = a.logger
		cfg.MCPServers = designCfg.MCPServers
		cfg.MergeMCPServers = designCfg.MergeMCPServers
		// Injected, never resolved inside the provider package: a session
		// that read the transcript home from $HOME would hand its readers
		// (and every writer downstream of them) the developer's real
		// ~/.claude on any boot whose provider home is pinned elsewhere.
		if projectsDir, dirErr := a.claudeProjectsDir(); dirErr != nil {
			log.Printf("start session: resolve claude projects dir: %v", dirErr)
		} else {
			cfg.ProjectsDir = projectsDir
		}
		applyClaudeSessionAxes(&cfg, a.settings.Get().ClaudeSessionAxesForProvider(t.Provider))
		// The peer-visible name rides the Config rather than the options
		// bundle on purpose: it is live-changeable through `/rename`, so
		// letting the reconciler see it would queue a restart every time a
		// thread title lands. See app_claude_peer_name.go.
		cfg.PeerSessionName = opts.ClaudePeerSessionName
		sess, err := claude.NewSession(context.Background(), threadID, cfg, onEvent)
		if err != nil {
			return session{}, err
		}
		return session{
			provider:             string(provider.Claude),
			token:                sessionToken,
			credentialGeneration: accountSelection.Generation,
			credentialAccountID:  accountSelection.AccountID,
			credentialAccount:    accountSelection.Account,
			claude:               sess,
			launchOpts:           opts,
			aoToken:              credential.token,
			aoScope:              credential.scope,
			aoEnv:                credential.env,
			liveness:             liveness,
		}, nil

	case string(provider.Codex):
		cfg := codex.ConfigFromOptions(opts)
		cfg.Binary = a.providerBinaryPath(t.Provider)
		cfg.Env = a.sessionProcessEnv(t.Provider, cfg.Env, credential)
		cfg.EventLogger = a.logger
		cfg.MCPServers = designCfg.MCPServers
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
		sess, err := codex.NewSession(context.Background(), threadID, cfg, onEvent)
		if err != nil {
			return session{}, err
		}
		if accountSelection.AccountID != "" {
			accountCtx, cancelAccountRead := context.WithTimeout(
				a.lifeCtx(),
				codexAccountReadTimeout,
			)
			accountInfo, accountErr := sess.AccountInfo(accountCtx)
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
				if accountErr == nil && a.providerAccounts != nil {
					if account, ok := a.providerAccounts.Get(
						string(provider.Codex),
						accountSelection.AccountID,
						time.Now(),
					); ok {
						accountSelection.Account = providerAccountInfo(account)
					}
				}
			}
			if accountErr != nil {
				closeErr := sess.Close()
				return session{}, errors.Join(accountErr, closeErr)
			}
		}
		// Wire the OAuth-completion observer before any RPC can fly so
		// the first `mcpServer/oauthLogin/completed` notification has a
		// handler ready. Claude has no equivalent wire signal — its
		// completion is internal to the CLI's loopback listener and the
		// popup's "Refresh" path covers that side.
		sess.SetMCPOAuthCompletedHandler(func(serverName string, success bool, errMsg string) {
			a.handleCodexMCPOAuthCompleted(threadID, serverName, success, errMsg)
		})
		// `mcpServer/startupStatus/updated` carries per-server live
		// state during/after thread/start. Feed it into the mcpstatus
		// cache so the popup reflects the running provider's view
		// without an ephemeral refetch.
		sess.SetMCPStartupUpdateHandler(func(u codex.MCPStartupUpdate) {
			a.handleCodexMCPStartupUpdate(u)
		})
		// `skills/changed` is the app-server's own signal that its watched
		// skill files moved. Drop the skills cache so the next composer
		// render re-reads instead of offering a list that no longer
		// matches disk.
		sess.SetSkillsChangedHandler(func() {
			a.handleCodexSkillsChanged()
		})
		return session{
			provider:             string(provider.Codex),
			token:                sessionToken,
			credentialGeneration: accountSelection.Generation,
			credentialAccountID:  accountSelection.AccountID,
			credentialAccount:    accountSelection.Account,
			codex:                sess,
			launchOpts:           opts,
			aoToken:              credential.token,
			aoScope:              credential.scope,
			aoEnv:                credential.env,
			liveness:             liveness,
		}, nil

	case string(provider.ClaudeTUI):
		cfg := claudetui.ConfigFromOptions(opts)
		// The interactive provider drives the same `claude` binary as the
		// headless one; there is no separate TUI binary setting.
		cfg.Binary = a.providerBinaryPath(string(provider.Claude))
		// claudetui's Config carries a whole environment rather than an
		// override map, so the shared rule is applied here instead of inside
		// the provider package. Skipping this branch would leave one provider
		// silently without the boot-mode overrides and the ao credential.
		//
		// The TUI shares Claude's custom environment (one binary, one backend),
		// with one variable it cannot take through the process env:
		// ANTHROPIC_BASE_URL names the per-session loopback gateway for the
		// child, and claudetui's buildEnv strips any inherited value. Handing it
		// to the gateway's upstream instead is what keeps the interactive
		// provider on the same backend as the headless one — dropping it here
		// would silently run take-control sessions against api.anthropic.com
		// while every other Claude surface used the user's endpoint.
		cfg.Env = provider.BuildEnvironment(
			a.sessionProcessEnv(string(provider.ClaudeTUI), nil, credential),
		)
		cfg.Upstream = a.claudetuiUpstream()
		cfg.EventLogger = a.logger
		sess, err := claudetui.NewSession(context.Background(), threadID, cfg, onEvent)
		if err != nil {
			return session{}, err
		}
		return session{
			provider:   string(provider.ClaudeTUI),
			token:      sessionToken,
			claudetui:  sess,
			launchOpts: opts,
			aoToken:    credential.token,
			aoScope:    credential.scope,
			aoEnv:      credential.env,
			liveness:   liveness,
		}, nil

	default:
		return session{}, fmt.Errorf("unknown provider: %s", t.Provider)
	}
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
	if a.workflowRunner == nil {
		return nil, fmt.Errorf("start workflow session: runner unavailable")
	}
	schema, registered := a.workflowRunner.SessionSchemaForThread(thread.ID)
	if !registered {
		return nil, fmt.Errorf("start workflow session %s: phase output schema is not registered", thread.ID)
	}
	return schema, nil
}

// SendMessageOptions carries send-time composer settings. AttachmentIDs is the
// current attachment payload; RuntimeMode is an optional draft override applied
// immediately before the provider turn starts.
type SendMessageOptions struct {
	AttachmentIDs                []string            `json:"attachmentIds"`
	RuntimeMode                  string              `json:"runtimeMode,omitempty"`
	SourceProposedPlan           *SourceProposedPlan `json:"sourceProposedPlan,omitempty"`
	RevisionSourceProposedPlan   *SourceProposedPlan `json:"revisionSourceProposedPlan,omitempty"`
	RevisionSourceCommentIDs     []string            `json:"revisionSourceCommentIds,omitempty"`
	RevisionSourceDiffReview     *SourceDiffReview   `json:"revisionSourceDiffReview,omitempty"`
	RevisionSourceDiffCommentIDs []string            `json:"revisionSourceDiffCommentIds,omitempty"`
}

// SendMessage is the Wails-bound compatibility entry point for user-typed
// content. The options-aware path below owns newer composer controls.
func (a *App) SendMessage(threadID string, content string, attachmentIDs []string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	_, err := a.sendMessageWithOptions(context.Background(), threadID, content, sendMessageOptions{
		AttachmentIDs: attachmentIDs,
		// Wire entry: this text was typed into a composer (D31). The internal
		// a.sendMessage helper deliberately does NOT set this — its other
		// caller (discussion drive) sends app-composed prompts.
		ExpandComposerCommands: true,
	})
	return err
}

// SendMessageWithOptions applies send-time composer settings and dispatches the
// user turn. RuntimeMode is staged in the composer and persisted here, under
// the same per-thread action lock as provider session start/send.
func (a *App) SendMessageWithOptions(threadID string, content string, opts SendMessageOptions) (store.Thread, error) {
	if a.shuttingDown.Load() {
		return store.Thread{}, ErrShuttingDown
	}
	if _, err := a.sendMessageWithOptions(context.Background(), threadID, content, sendMessageOptions{
		AttachmentIDs:                opts.AttachmentIDs,
		RuntimeMode:                  opts.RuntimeMode,
		SourceProposedPlan:           opts.SourceProposedPlan,
		RevisionSourceProposedPlan:   opts.RevisionSourceProposedPlan,
		RevisionSourceCommentIDs:     opts.RevisionSourceCommentIDs,
		RevisionSourceDiffReview:     opts.RevisionSourceDiffReview,
		RevisionSourceDiffCommentIDs: opts.RevisionSourceDiffCommentIDs,
		// Wire entry: this text was typed into a composer (D31).
		ExpandComposerCommands: true,
	}); err != nil {
		return store.Thread{}, err
	}
	return a.store.GetThread(threadID)
}

// InterruptTurn fires a provider-level interrupt on the thread's active
// session. Returns nil when no session is active — the subprocess is
// already gone, there is nothing to interrupt, and surfacing an error
// banner here would just paper over the more useful "Reconnect" path.
// Other failure modes (the provider's Interrupt call failed, the CLI
// never acked the control_request) DO surface as errors so a wedged
// Claude Code CLI is visible to the user.
//
// Spec: on user interrupt, any streaming items on the current turn are
// flipped to errored with a " — stopped" suffix, and a system `error`
// row with Summary "Stopped by user" is appended. This happens AFTER
// the provider interrupt signal is sent so the UI gets a consistent
// "signal sent, now here's the record" ordering. If the triage
// bookkeeping fails we log — the provider interrupt already fired, so
// the session state is correct even if the timeline marker is missing.
func (a *App) InterruptTurn(threadID string) error {
	return a.interruptTurnCtx(context.Background(), threadID)
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
func (a *App) interruptTurnCtx(ctx context.Context, threadID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	// Capture before waiting on the app-level thread lock. A Codex revert holds
	// that lock across its provider and SQLite halves; an interrupt that entered
	// before or during the cut must not wake afterward and hit the reloaded
	// thread. The provider fence also covers direct Session callers that do not
	// know about this lock.
	observedSessionToken := ""
	observedSession := false
	var fencedCodex *codex.Session
	var interruptFence codex.InterruptFence
	if current, ok := a.sessionManager().get(threadID); ok {
		observedSession = true
		observedSessionToken = current.token
		if current.codex != nil {
			fencedCodex = current.codex
			interruptFence = current.codex.CaptureInterruptFence()
		}
	}
	// The whole interrupt — session capture, pre-ack Mark, ack wait,
	// and the post-ack bookkeeping/promote/eager block — runs under the
	// thread action lock: the same lock the session-start funnel holds
	// across MarkThreadActive + spawn, the death drain holds across its
	// restore, and the dispatch worker holds per batch. Without it a
	// replacement could reactivate the thread between the post-ack
	// epoch checks and their store writes, and the Codex resend could
	// go through a captured session already swapped out (round-12,
	// CT12-2/CT12-3/C12-2). runPlainInterruptLocked holds the same lock
	// via its caller — including across the ack wait, the established
	// precedent that the provider read loop never blocks on this lock.
	// The in-triage epoch fences stay as defense for an interrupt that
	// acquires this lock only after a replacement completed.
	unlock, err := a.threadLocks().LockCtx(ctx, threadID)
	if err != nil {
		return fmt.Errorf("interrupt turn %s: thread action lock: %w", threadID, err)
	}
	defer unlock()
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return nil
	}
	if observedSession && sess.token != observedSessionToken {
		// The work visible when the interrupt entered is gone. A replacement
		// session may already be processing a later send, so the stale intent
		// must not be retargeted onto it.
		return nil
	}

	providerSess := sess.providerSession()
	if providerSess == nil {
		return fmt.Errorf("session has no provider")
	}
	// Sampled BEFORE the interrupt ack: awaiting it keeps the read loop
	// processing wire events, so the cut turn can settle in the gap and
	// a later sample would miss it (round-5, R5-4). The sample is also
	// PUBLISHED onto the unconsumed pending flush entries pre-ack: the
	// CLI's mid-loop queue drain can echo one back during the ack wait,
	// and a post-ack stamp would let that echo settle the cut turn
	// "end_turn" (round-6, R6-4; broadened past still-deferred entries
	// in round-7, R7-5). If the interrupt request itself fails the
	// previous stamps are RESTORED — not wiped: an entry eager-persisted
	// by an earlier interrupt still carries that interrupt's valid
	// stamp.
	interruptedTurn := -1
	var stampToken triage.FlushStampToken
	if a.triage != nil {
		interruptedTurn = a.triage.OpenTurnIndex(threadID)
		stampToken = a.triage.MarkFlushSendsInterrupted(threadID, interruptedTurn)
	}
	var interruptErr error
	interruptSent := true
	if sess.codex != nil && sess.codex == fencedCodex {
		interruptSent, interruptErr = sess.codex.InterruptIfCurrent(context.Background(), interruptFence)
	} else {
		interruptErr = providerSess.Interrupt(context.Background())
	}
	if interruptErr != nil {
		if a.triage != nil {
			a.triage.RestoreFlushSendsInterrupted(threadID, stampToken)
		}
		return interruptErr
	}
	if !interruptSent {
		// The intent entered before a same-session revert and therefore names
		// the history that operation already discarded. Do not synthesize a
		// second user interrupt in triage for the reloaded thread.
		if a.triage != nil {
			a.triage.RestoreFlushSendsInterrupted(threadID, stampToken)
		}
		return nil
	}
	if a.triage != nil {
		// The pre-ack sampled turn, not a fresh resolution: a queued echo
		// consumed during the ack wait may have opened its own turn, and
		// the stopped bookkeeping belongs on the turn the user cut
		// (round-11, C11-1).
		if _, err := a.triage.MarkUserInterrupt(threadID, interruptedTurn, stampToken); err != nil {
			log.Printf("app: interrupt turn: mark user interrupt: %v", err)
		}
		a.eagerPersistFlushSendsOnInterrupt(threadID, sess, interruptedTurn, stampToken)
	}
	return nil
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
// For Codex, deferred sends are also re-sent as a fresh turn because
// Codex discards steered pending_input on turn/interrupt.
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

	persisted := a.triage.EagerPersistDeferredFlushSends(threadID, interruptedTurn, stampToken)
	if len(persisted) == 0 {
		return
	}

	if sess.codex == nil {
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
	contents := make([]string, len(persisted))
	for i, p := range persisted {
		ids[i] = p.UserItemID
		contents[i] = p.Content
	}
	a.triage.ClearPendingSendsByItemIDs(threadID, ids)

	// The original dispatch delivered image attachments alongside the
	// steer; this resend is the content's only remaining delivery, so
	// it must carry them too (round-14, CT14-2). IDs come from the
	// persisted rows' meta — the same source the requeue payload uses.
	var attachmentIDs []string
	for _, p := range persisted {
		ids, err := attachmentIDsFromUserMeta(p.Meta)
		if err != nil {
			log.Printf("app: codex interrupt re-send: decode attachments for %s/%s: %v", threadID, p.UserItemID, err)
			continue
		}
		attachmentIDs = append(attachmentIDs, ids...)
	}
	providerAttachments, _, err := a.resolveSendMessageAttachments(threadID, attachmentIDs)
	if err != nil {
		log.Printf("app: codex interrupt re-send: resolve attachments for %s: %v", threadID, err)
		a.restoreEagerPersistedFlushesToDraft(threadID, persisted)
		return
	}

	merged := strings.Join(contents, "\n\n")
	// The merged re-send is correlated to the FIRST item's row, which is
	// the row the pending entry below is registered for. Items 2+ share
	// that correlation, exactly as they share the turn.
	resendClientID, resendExpect := providerSendIdentity(sess, persisted[0].UserItemID, "")
	sendOpts := provider.SendOptions{
		InteractionMode:     provider.NormalizeInteractionMode(thread.Mode),
		Attachments:         providerAttachments,
		ClientUserMessageID: resendClientID,
	}

	turnIndex, err := a.nextSendTurnIndex(threadID)
	if err != nil {
		log.Printf("app: codex interrupt re-send: resolve turn index for %s: %v", threadID, err)
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
	sess.liveness.bumpActivity(time.Now())

	if sendErr := sess.codex.Send(context.Background(), merged, sendOpts); sendErr != nil {
		if codex.IsAmbiguousTurnStartTimeout(sendErr) {
			// The turn/start was written; the turn — and its echo — may
			// already be running. Requeueing would re-send content Codex
			// may have consumed (round-14, D14-2). Keep the pending entry:
			// a late echo settles it normally, and if the turn truly never
			// started the session-death drain recovers the rows.
			log.Printf("app: codex interrupt re-send for thread %s timed out after write; leaving pending confirmation for provider echo", threadID)
			return
		}
		log.Printf("app: codex interrupt re-send for thread %s: %v", threadID, sendErr)
		a.triage.ClearPendingSendForFailure(threadID, persisted[0].UserItemID)
		// Codex discarded the queued input at turn/interrupt and this
		// resend was its only delivery — the rows would sit in the
		// timeline looking sent. Delete them and hand the content back
		// to the composer, the same recovery the Codex TUI applies to
		// unconsumed input.
		a.restoreEagerPersistedFlushesToDraft(threadID, persisted)
	}
}

// StopSession tears down the thread's provider session. Idempotent: a
// thread with no active session still runs the design teardown +
// triage cleanup so stale per-thread state doesn't leak.
func (a *App) StopSession(threadID string) error {
	sess, _ := a.sessionManager().take(threadID)
	// teardownAndCloseSession tolerates the zero-value session — when
	// no entry was registered, closeProviderSession returns nil and
	// only the design + triage state is reset.
	return a.teardownAndCloseSession(threadID, sess)
}

// teardownDeadPreInitSession reaps a session that reported a hard error
// result before ever reaching init: it cannot serve sends (the resume
// cursor it was launched with is unusable) and would otherwise linger
// alive forever — Claude does not exit after a failed
// --resume-session-at validation. The orphan error item persisted by
// triage is the user-facing surface; this teardown stops the useless
// process, hands any queued sends back to the composer draft, and
// sweeps the failed send's pending state so the next send lazy-starts
// fresh.
//
// Runs on its own goroutine (see sessionEventHandler) because the
// caller is the provider read loop and Close blocks on read-loop exit.
// Two guards keep it from harming a user retry that races in:
//
//   - The token-guarded unregister means a retry that already replaced
//     this session in the registry makes the teardown a no-op.
//   - The triage epoch, captured BEFORE the unregister, covers the
//     longer window after a successful unregister: a retry's start path
//     calls MarkThreadActive (bumping the epoch) and then spawns for
//     potentially seconds before re-registering, during which a
//     registry check proves nothing. The queue restore and the triage
//     cleanup both run only while the epoch is unchanged, so a live
//     replacement is never re-stopped and never has its queue drained.
//     (The epoch probe before the restore and the restore itself are
//     adjacent calls, not one atomic section — but a reactivation
//     squeezing between them would have to finish MarkThreadActive
//     within microseconds of passing the probe AND have new queue
//     state to lose, which requires a full spawn that takes orders of
//     magnitude longer. CleanupThreadIfEpoch's sweep re-checks under
//     the router lock, so the destructive path is strictly guarded.)
//
// Not routed through teardownAndCloseSession: that helper's
// unconditional CleanupThread is exactly what must not run here once a
// replacement has claimed the thread.
func (a *App) teardownDeadPreInitSession(threadID, sessionToken string) {
	var epoch uint64
	if a.triage != nil {
		epoch = a.triage.ThreadEpoch(threadID)
	}
	sess, ok := a.sessionManager().unregister(threadID, sessionToken)
	if !ok {
		return
	}
	a.emitProviderSessionDisconnected(threadID, sess.provider)
	a.teardownDesignThread(threadID)
	if a.triage != nil {
		// Restore before cleanup: the sweep's clearFlushQueueLocked
		// discards queued items, and they only exist in router memory —
		// without the restore the user's queued text would vanish.
		var requeued []triage.UnconfirmedFlushItem
		if a.triage.ThreadEpoch(threadID) == epoch {
			requeued = a.restoreUnconfirmedQueueOnSessionDeath(threadID)
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
				a.requeueUnconfirmedFlushItems(threadID, requeued)
				a.emitQueueStateChanged(threadID)
			}
		}
	}
	if err := a.closeProviderSession(threadID, sess); err != nil {
		log.Printf("app: teardown dead pre-init session for thread %s: %v", threadID, err)
	}
}

// teardownAndCloseSession runs the per-thread design + triage cleanup
// and closes the provider subprocess. Shared by StopSession (user
// action) and idleCloseSession (reaper) so the close sequence stays in
// one place — future per-thread cleanup steps land once and all paths
// inherit it. teardownDeadPreInitSession mirrors the sequence but owns
// its own copy: its CleanupThread must be epoch-guarded against a
// racing replacement start, and it restores the flush queue first.
//
// Callers must remove the session from a.sessions BEFORE invoking so
// two concurrent closers can't both call Close on the same provider
// session. A zero-value sess argument is intentionally tolerated:
// closeProviderSession returns nil for it, which lets StopSession
// reuse this helper to scrub design/triage state on a thread that
// never had a session registered.
//
// Not shared with unregisterSession (readLoop disconnect): that path
// runs after the provider subprocess has already exited, so calling
// closeProviderSession would be redundant, and it deliberately leaves
// triage state alone so the final wire frames have somewhere to land.
func (a *App) teardownAndCloseSession(threadID string, sess session) error {
	a.teardownDesignThread(threadID)
	a.releaseWorkflowUsageAttentionForThread(threadID)
	if a.triage != nil {
		a.triage.CleanupThread(threadID)
	}
	err := a.closeProviderSession(threadID, sess)
	if err == nil && sess.provider == string(provider.Codex) {
		err = a.retireCodexBackgroundRuntime(threadID)
	}
	if sess.provider != "" {
		a.emitProviderSessionDisconnected(threadID, sess.provider)
	}
	// After the provider process is closed no flush tick can re-register
	// seeder state; a thread killed mid-stream would otherwise strand
	// its entries (no final tick ever arrives to clear them).
	a.highlightSeeder.purgeThread(threadID)
	return err
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
