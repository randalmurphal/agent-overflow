package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/claudetui"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"
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
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	return a.startSession(threadID)
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
func (a *App) buildSessionOptions(t store.Thread) (provider.SessionOptions, designSessionConfig, error) {
	designCfg, err := a.designSessionConfig(t)
	if err != nil {
		return provider.SessionOptions{}, designSessionConfig{}, err
	}
	systemPrompt := stringsx.JoinNonEmpty("\n\n", designCfg.Prompt, a.threadSystemPrompt(t.ID))

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

	sessionToken := uuid.NewString()
	onEvent := a.sessionEventHandler(threadID, sessionToken, t.Provider)

	opts, designCfg, err := a.buildSessionOptions(t)
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
	if t.Provider == string(provider.Claude) && opts.Resume != "" && !opts.ForkSession {
		opts.ResumeAt = resolveClaudeResumeAt(opts.Resume, opts.WorkDir, claudeResumeAt)
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
	// designServers is non-nil only for design threads. For Codex
	// chat/plan threads we inject the per-thread enabled set via
	// config.mcp_servers so each session is isolated from global config
	// changes. Claude chat/plan sessions use native discovery +
	// post-init mcp_set_servers reconcile instead (see
	// reconcileClaudeMCPOnInit in app_mcp_claude.go).
	designCfg.MCPServers = designServers
	if designServers == nil && t.Provider == string(provider.Codex) {
		if servers, err := a.buildCodexMCPServersForThread(t); err != nil {
			log.Printf("start session: build codex mcp for thread %s: %v", threadID, err)
		} else if servers != nil {
			designCfg.MCPServers = servers
		}
	}
	if designServers == nil {
		chatServer, enabled, err := a.workflowChatMCPServerConfig(t)
		if err != nil {
			return fmt.Errorf("start session: %w", err)
		}
		if enabled {
			if designCfg.MCPServers == nil {
				designCfg.MCPServers = map[string]any{}
			}
			designCfg.MCPServers[workflowChatMCPServerName] = chatServer
			designCfg.WorkflowChatMCP = true
			designCfg.MergeMCPServers = t.Provider == string(provider.Claude)
		}
	}

	// Flip any persisted `is_background=running` rows for a Codex thread
	// to errored/lost BEFORE spawning the new subprocess. Those rows
	// point at PTYs / spawned child threads owned by a prior subprocess
	// that is guaranteed dead (we just stopped the existing session, if
	// any, and startup is now running against either a fresh process or
	// an on-reopen cold state). Must land before spawnProviderSession so
	// no replay `item/started` can race with the flip — the store is the
	// only source of truth for this reconcile, the probe happens
	// downstream in reconcileCodexAfterStart once the live session
	// exists. See app_codex_reconcile.go for the rationale + warm-
	// reconnect fallback. Claude threads are not flipped here (their
	// `stop_task` primitive and natural completion handle the same
	// concern on a different rail).
	if t.Provider == string(provider.Codex) {
		a.flipCodexGhostBackgroundRowsOnStart(threadID)
	}

	newSess, err := a.spawnProviderSession(threadID, sessionToken, t, opts, designCfg, onEvent)
	if err != nil {
		a.teardownDesignThread(threadID)
		a.emitProviderStatusOnSessionStartError(t.Provider)
		return fmt.Errorf("start session: %w", err)
	}

	a.sessionManager().put(threadID, newSess)

	// Register the freshly-spawned process group with the orphan reaper so
	// it's torn down if the app dies before the session closes cleanly
	// (macOS only; no-op elsewhere). Paired with the release in
	// closeProviderSession.
	if ps := newSess.providerSession(); ps != nil {
		a.watchSessionProcess(ps.PID(), sessionToken)
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
func resolveClaudeResumeAt(sessionRef, workDir, explicit string) string {
	if explicit != "" {
		onBranch, err := claude.ResumeAtOnActiveBranch(sessionRef, workDir, explicit)
		switch {
		case err != nil:
			log.Printf("start session: validate Claude resume-at %s against session %s: %v — falling back to file scan", explicit, sessionRef, err)
		case onBranch:
			return explicit
		default:
			log.Printf("start session: Claude resume-at %s is off the active branch of session %s — rejecting, falling back to file scan", explicit, sessionRef)
		}
	}
	state, err := claude.ScanSessionLeaf(sessionRef, workDir)
	if err != nil {
		log.Printf("start session: scan Claude session leaf %s: %v", sessionRef, err)
		return ""
	}
	return state.CanonicalLeafUUID
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

	// Thread-scoped design state must be torn down alongside the session
	// so a restart doesn't leak the prior turn's MCP registration.
	a.teardownDesignThread(threadID)
	return a.closeProviderSession(threadID, existing)
}

// mergeProviderEnv folds providerExtraEnv into a session config's Env
// map, allocating only when there is something to merge. Extra env wins
// on key conflicts — it exists precisely to override for this boot mode.
func (a *App) mergeProviderEnv(env map[string]string) map[string]string {
	if len(a.providerExtraEnv) == 0 {
		return env
	}
	merged := make(map[string]string, len(env)+len(a.providerExtraEnv))
	for k, v := range env {
		merged[k] = v
	}
	for k, v := range a.providerExtraEnv {
		merged[k] = v
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

	switch t.Provider {
	case string(provider.Claude):
		cfg := claude.ConfigFromOptions(opts)
		if len(workflowSchema) > 0 {
			cfg.OutputSchema = string(workflowSchema)
		}
		cfg.Binary = a.providerBinaryPath(t.Provider)
		cfg.Env = a.mergeProviderEnv(cfg.Env)
		cfg.EventLogger = a.logger
		cfg.MCPServers = designCfg.MCPServers
		cfg.MergeMCPServers = designCfg.MergeMCPServers
		sess, err := claude.NewSession(context.Background(), threadID, cfg, onEvent)
		if err != nil {
			return session{}, err
		}
		return session{
			provider:        string(provider.Claude),
			token:           sessionToken,
			claude:          sess,
			launchOpts:      opts,
			workflowChatMCP: designCfg.WorkflowChatMCP,
			liveness:        liveness,
		}, nil

	case string(provider.Codex):
		cfg := codex.ConfigFromOptions(opts)
		cfg.Binary = a.providerBinaryPath(t.Provider)
		cfg.Env = a.mergeProviderEnv(cfg.Env)
		cfg.EventLogger = a.logger
		cfg.MCPServers = designCfg.MCPServers
		sess, err := codex.NewSession(context.Background(), threadID, cfg, onEvent)
		if err != nil {
			return session{}, err
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
		return session{
			provider:        string(provider.Codex),
			token:           sessionToken,
			codex:           sess,
			launchOpts:      opts,
			workflowChatMCP: designCfg.WorkflowChatMCP,
			liveness:        liveness,
		}, nil

	case string(provider.ClaudeTUI):
		cfg := claudetui.ConfigFromOptions(opts)
		// The interactive provider drives the same `claude` binary as the
		// headless one; there is no separate TUI binary setting.
		cfg.Binary = a.providerBinaryPath(string(provider.Claude))
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
			liveness:   liveness,
		}, nil

	default:
		return session{}, fmt.Errorf("unknown provider: %s", t.Provider)
	}
}

func (a *App) workflowSchemaForSession(thread store.Thread) (json.RawMessage, error) {
	if thread.Mode != "workflow" {
		return nil, nil
	}
	if a.workflowRunner == nil {
		return nil, fmt.Errorf("start workflow session: runner unavailable")
	}
	schema, registered := a.workflowRunner.sessionSchemaForThread(thread.ID)
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
	return a.sendMessage(threadID, content, attachmentIDs)
}

// SendMessageWithOptions applies send-time composer settings and dispatches the
// user turn. RuntimeMode is staged in the composer and persisted here, under
// the same per-thread action lock as provider session start/send.
func (a *App) SendMessageWithOptions(threadID string, content string, opts SendMessageOptions) (store.Thread, error) {
	if a.shuttingDown.Load() {
		return store.Thread{}, ErrShuttingDown
	}
	if _, err := a.sendMessageWithOptions(threadID, content, sendMessageOptions{
		AttachmentIDs:                opts.AttachmentIDs,
		RuntimeMode:                  opts.RuntimeMode,
		SourceProposedPlan:           opts.SourceProposedPlan,
		RevisionSourceProposedPlan:   opts.RevisionSourceProposedPlan,
		RevisionSourceCommentIDs:     opts.RevisionSourceCommentIDs,
		RevisionSourceDiffReview:     opts.RevisionSourceDiffReview,
		RevisionSourceDiffCommentIDs: opts.RevisionSourceDiffCommentIDs,
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
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return nil
	}

	providerSess := sess.providerSession()
	if providerSess == nil {
		return fmt.Errorf("session has no provider")
	}
	if err := providerSess.Interrupt(context.Background()); err != nil {
		return err
	}
	if a.triage != nil {
		if _, err := a.triage.MarkUserInterrupt(threadID); err != nil {
			log.Printf("app: interrupt turn: mark user interrupt: %v", err)
		}
		a.eagerPersistFlushSendsOnInterrupt(threadID, sess)
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
func (a *App) eagerPersistFlushSendsOnInterrupt(threadID string, sess session) {
	a.triage.PromoteQuietFlushSends(threadID)

	persisted := a.triage.EagerPersistDeferredFlushSends(threadID)
	if len(persisted) == 0 {
		return
	}

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("app: eager persist flush: load thread %s: %v", threadID, err)
		return
	}

	for _, p := range persisted {
		a.captureMessageCheckpoint(thread, store.Item{
			ID:        p.UserItemID,
			ThreadID:  threadID,
			TurnIndex: p.TurnIndex,
		})
	}

	if sess.codex == nil {
		return
	}

	a.codexResendAfterInterrupt(threadID, sess, persisted, thread)
}

// codexResendAfterInterrupt clears stranded pending sends and re-sends
// eagerly-persisted flush items as a fresh Codex turn. Codex discards
// steered pending_input on turn/interrupt (codex-rs abort_all_tasks →
// input_queue.clear_pending), so the client must re-submit — mirroring
// the Codex TUI's submit_pending_steers_after_interrupt flow.
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

	merged := strings.Join(contents, "\n\n")
	sendOpts := provider.SendOptions{
		InteractionMode: provider.NormalizeInteractionMode(thread.Mode),
	}

	turnIndex, err := a.nextSendTurnIndex(threadID)
	if err != nil {
		log.Printf("app: codex interrupt re-send: resolve turn index for %s: %v", threadID, err)
		return
	}

	// Register a non-deferred pending send for the first item so the
	// echo stamps provider_item_id via attachProviderItemIDToUserRow.
	// Items 2+ are merged into the same turn and share the first
	// item's provider correlation. The row was already persisted at its
	// interrupt position by EagerPersistDeferredFlushSends — anchor the
	// fresh entry so the echo's :flush: bump doesn't move it again.
	a.triage.RegisterPendingSend(threadID, persisted[0].UserItemID, turnIndex)
	a.triage.MarkPendingSendAnchoredAtInterrupt(threadID, persisted[0].UserItemID)
	sess.liveness.bumpActivity(time.Now())

	if sendErr := sess.codex.Send(context.Background(), merged, sendOpts); sendErr != nil {
		log.Printf("app: codex interrupt re-send for thread %s: %v", threadID, sendErr)
		a.triage.ClearPendingSendForFailure(threadID, persisted[0].UserItemID)
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
	a.teardownDesignThread(threadID)
	if a.triage != nil {
		// Restore before cleanup: the sweep's clearFlushQueueLocked
		// discards queued items, and they only exist in router memory —
		// without the restore the user's queued text would vanish.
		if a.triage.ThreadEpoch(threadID) == epoch {
			a.restoreUnconfirmedQueueOnSessionDeath(threadID)
		}
		if !a.triage.CleanupThreadIfEpoch(threadID, epoch) {
			log.Printf("app: skipped triage cleanup for thread %s — a replacement session reactivated it mid-teardown", threadID)
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
	if a.triage != nil {
		a.triage.CleanupThread(threadID)
	}
	return a.closeProviderSession(threadID, sess)
}
